package openai_compat

// M2-4 idempotency handler tests: claim → execute → replay byte-identical;
// in-flight 409; fingerprint mismatch 409; no-header requests unaffected.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// fakeIdempotencyStore is an in-memory claim ledger mirroring the repository
// state machine closely enough for handler-level tests.
type fakeIdempotencyStore struct {
	interfaces.GatewayIdempotencyStore
	rows map[string]*types.GatewayIdempotency // keyed by "key:key"
	next int64
}

func newFakeIdempotencyStore() *fakeIdempotencyStore {
	return &fakeIdempotencyStore{rows: map[string]*types.GatewayIdempotency{}}
}

func (f *fakeIdempotencyStore) Claim(
	_ context.Context, tenantID, keyID uint64, idempotencyKey, fingerprint string,
) (types.GatewayIdempotencyOutcome, *types.GatewayIdempotency, error) {
	claimKey := idemRowKey(keyID, idempotencyKey)
	if row, ok := f.rows[claimKey]; ok {
		if row.Fingerprint != fingerprint {
			return types.GatewayIdempotencyMismatch, row, nil
		}
		if row.Status == types.GatewayIdempotencyStatusDone {
			return types.GatewayIdempotencyReplay, row, nil
		}
		return types.GatewayIdempotencyInFlight, row, nil
	}
	f.next++
	row := &types.GatewayIdempotency{
		ID: f.next, TenantID: tenantID, KeyID: keyID,
		IdempotencyKey: idempotencyKey, Fingerprint: fingerprint,
		Status: types.GatewayIdempotencyStatusInFlight,
	}
	f.rows[claimKey] = row
	return types.GatewayIdempotencyClaimed, row, nil
}

func (f *fakeIdempotencyStore) Complete(
	_ context.Context, claimID int64, statusCode int, contentType string, body []byte, replayable bool,
) error {
	for _, row := range f.rows {
		if row.ID != claimID {
			continue
		}
		if !replayable {
			delete(f.rows, idemRowKey(row.KeyID, row.IdempotencyKey))
			return nil
		}
		row.Status = types.GatewayIdempotencyStatusDone
		row.StatusCode = statusCode
		row.ContentType = contentType
		row.ResponseBody = body
	}
	return nil
}

func (f *fakeIdempotencyStore) Release(_ context.Context, claimID int64) error {
	for k, row := range f.rows {
		if row.ID == claimID {
			delete(f.rows, k)
		}
	}
	return nil
}

func (f *fakeIdempotencyStore) PruneExpired(context.Context, time.Time) (int64, error) {
	return 0, nil
}

func idemRowKey(keyID uint64, idemKey string) string {
	return strconv.FormatUint(keyID, 10) + ":" + idemKey
}

// serveIdempotent posts with an optional Idempotency-Key header.
func serveIdempotent(
	t *testing.T, store interfaces.GatewayIdempotencyStore, idemKey, body string,
) *httptest.ResponseRecorder {
	t.Helper()
	gin.SetMode(gin.TestMode)
	router := gin.New()
	handler := NewGatewayHandler(
		&fakeAgentService{agents: []*types.CustomAgent{
			gatewayAgent("agt-1", "Doc QA", types.GatewayScopeTenant, "u1"),
		}},
		&fakeGatewaySessions{script: answerScript}, &fakeGatewayMessages{}, nil, nil, nil, store, nil)
	router.POST("/v1/chat/completions", func(c *gin.Context) {
		ctx := c.Request.Context()
		ctx = context.WithValue(ctx, types.UserIDContextKey, "user-1")
		ctx = context.WithValue(ctx, types.TenantIDContextKey, uint64(42))
		c.Request = c.Request.WithContext(ctx)
		c.Next()
	}, handler.ChatCompletions)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if idemKey != "" {
		req.Header.Set(IdempotencyKeyHeader, idemKey)
	}
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

const idemBody = `{"model":"Doc QA","messages":[{"role":"user","content":"hi"}]}`

func TestGatewayIdempotentReplay(t *testing.T) {
	store := newFakeIdempotencyStore()

	first := serveIdempotent(t, store, "retry-1", idemBody)
	require.Equal(t, http.StatusOK, first.Code, first.Body.String())

	// Same key + same body → byte-identical replay, no second execution.
	replay := serveIdempotent(t, store, "retry-1", idemBody)
	require.Equal(t, http.StatusOK, replay.Code, replay.Body.String())
	require.Equal(t, first.Body.String(), replay.Body.String())
	require.Equal(t, "true", replay.Header().Get("X-WeKnora-Idempotent-Replay"))
}

func TestGatewayIdempotentMismatch409(t *testing.T) {
	store := newFakeIdempotencyStore()
	first := serveIdempotent(t, store, "retry-2", idemBody)
	require.Equal(t, http.StatusOK, first.Code)

	other := strings.Replace(idemBody, "hi", "different question", 1)
	replay := serveIdempotent(t, store, "retry-2", other)
	require.Equal(t, http.StatusConflict, replay.Code, replay.Body.String())
	require.Contains(t, replay.Body.String(), "idempotency_key_mismatch")
}

func TestGatewayIdempotentInFlight409(t *testing.T) {
	store := newFakeIdempotencyStore()
	// Pre-claim without completing → next identical request 409s.
	outcome, _, err := store.Claim(context.Background(), 42, 42, "retry-3", idemFingerprint(t, idemBody))
	require.NoError(t, err)
	require.Equal(t, types.GatewayIdempotencyClaimed, outcome)

	rec := serveIdempotent(t, store, "retry-3", idemBody)
	require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), "request_in_progress")
	require.Equal(t, "2", rec.Header().Get("Retry-After"))
}

func TestGatewayIdempotentNoHeaderUnaffected(t *testing.T) {
	store := newFakeIdempotencyStore()
	for i := 0; i < 2; i++ {
		rec := serveIdempotent(t, store, "", idemBody)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	}
	require.Empty(t, store.rows, "no header → no claims stored")
}

// idemFingerprint mirrors the handler's fingerprint (sha256 of raw body) so
// pre-claims line up; the test recomputes it independently.
func idemFingerprint(t *testing.T, body string) string {
	t.Helper()
	digest := sha256.Sum256([]byte(body))
	return hex.EncodeToString(digest[:])
}
