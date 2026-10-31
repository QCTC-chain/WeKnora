package openai_compat

// M2-4 idempotency: retried requests carrying `Idempotency-Key` replay the
// original response instead of re-executing a half-applied turn. The claim
// is (API key, Idempotency-Key); the fingerprint is sha256 over the raw
// request body — a claim reused with a different body is rejected.
//
// Capture bounds: responses over gatewayIdempotencyMaxBody bytes delete the
// claim, so an oversized turn re-executes on retry rather than replaying a
// truncated body. Requests without the header behave exactly as before.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strings"

	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/gin-gonic/gin"
)

const (
	// IdempotencyKeyHeader is the client-facing claim key (design §4).
	IdempotencyKeyHeader = "Idempotency-Key"
	// gatewayIdempotencyMaxBody caps replayable response capture at 1MB:
	// gateway answers are text turns; anything larger is a retrieval dump
	// that the client should stream instead of retry blindly.
	gatewayIdempotencyMaxBody = 1 << 20
	// gatewayIdempotencyKeyMaxRunes bounds the client-supplied key length.
	gatewayIdempotencyKeyMaxRunes = 128
)

// capturingResponseWriter tees the response bytes (bounded) so a completed
// turn can be stored for replay. WriteHeader passthrough keeps SSE flush
// semantics; the embedded gin.ResponseWriter serves Flush/CloseNotify/etc.
type capturingResponseWriter struct {
	gin.ResponseWriter
	buf      bytes.Buffer
	status   int
	overflow bool
}

func (w *capturingResponseWriter) capture(b []byte) {
	if w.overflow {
		return
	}
	if w.buf.Len()+len(b) > gatewayIdempotencyMaxBody {
		w.overflow = true
		w.buf.Reset()
		return
	}
	w.buf.Write(b)
}

func (w *capturingResponseWriter) Write(b []byte) (int, error) {
	w.capture(b)
	return w.ResponseWriter.Write(b)
}

func (w *capturingResponseWriter) WriteString(s string) (int, error) {
	w.capture([]byte(s))
	return w.ResponseWriter.WriteString(s)
}

func (w *capturingResponseWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

// claimIdempotency applies the M2-4 protocol and returns (claim, replay,
// handled). handled=true means the request was answered here (replay or
// 409) and the caller must return immediately.
func (h *GatewayHandler) claimIdempotency(
	ctx context.Context, c *gin.Context, tenantID uint64, body []byte,
) (claim *types.GatewayIdempotency, replay bool, handled bool) {
	if h.idempotency == nil {
		return nil, false, false // store not wired: header ignored (degradation)
	}
	idemKey := strings.TrimSpace(c.GetHeader(IdempotencyKeyHeader))
	if idemKey == "" {
		return nil, false, false // no header: behaviour unchanged
	}
	if len([]rune(idemKey)) > gatewayIdempotencyKeyMaxRunes {
		writeError(c, http.StatusBadRequest, "invalid_idempotency_key",
			"Idempotency-Key must be at most 128 characters")
		return nil, false, true
	}
	keyID := gatewayKeyID(ctx)
	if keyID == 0 {
		// Tenant-level callers without a key row (tests, direct auth): scope
		// the claim by tenant so rows stay unique per caller.
		keyID = tenantID
	}
	digest := sha256.Sum256(body)
	fingerprint := hex.EncodeToString(digest[:])

	outcome, existing, err := h.idempotency.Claim(ctx, tenantID, keyID, idemKey, fingerprint)
	if err != nil {
		// Ledger failure must not block the turn; proceed unclaimed.
		logger.Warnf(ctx, "[gateway] idempotency claim failed (continuing unclaimed): %v", err)
		return nil, false, false
	}
	switch outcome {
	case types.GatewayIdempotencyClaimed:
		return existing, false, false
	case types.GatewayIdempotencyReplay:
		contentType := existing.ContentType
		if contentType == "" {
			contentType = "application/json; charset=utf-8"
		}
		c.Data(existing.StatusCode, contentType, existing.ResponseBody)
		c.Header("X-WeKnora-Idempotent-Replay", "true")
		return existing, true, true
	case types.GatewayIdempotencyInFlight:
		c.Header("Retry-After", "2")
		writeError(c, http.StatusConflict, "request_in_progress",
			"An identical request with this Idempotency-Key is still running; retry after it finishes")
		return nil, false, true
	default: // mismatch
		writeError(c, http.StatusConflict, "idempotency_key_mismatch",
			"This Idempotency-Key was already used with a different request body; use a new key for the new request")
		return nil, false, true
	}
}

// finishIdempotency stores (or drops) the captured response after the turn.
// Never fails the request: the answer already left the wire.
func (h *GatewayHandler) finishIdempotency(
	ctx context.Context, claim *types.GatewayIdempotency, capture *capturingResponseWriter,
) {
	if claim == nil || capture == nil {
		return
	}
	if capture.overflow {
		// Oversized: drop the claim so a retry re-executes.
		if err := h.idempotency.Complete(ctx, claim.ID, 0, "", nil, false); err != nil {
			logger.Warnf(ctx, "[gateway] idempotency drop failed (claim id=%d): %v", claim.ID, err)
		}
		return
	}
	status := capture.status
	if status == 0 {
		status = http.StatusOK
	}
	contentType := capture.Header().Get("Content-Type")
	err := h.idempotency.Complete(ctx, claim.ID, status, contentType, capture.buf.Bytes(), true)
	if err != nil {
		logger.Warnf(ctx, "[gateway] idempotency complete failed (claim id=%d): %v", claim.ID, err)
	}
}

// releaseIdempotency drops the claim when the turn failed before any bytes
// were written (session/DI failures), so an immediate retry re-executes.
func (h *GatewayHandler) releaseIdempotency(ctx context.Context, claim *types.GatewayIdempotency) {
	if claim == nil || h.idempotency == nil {
		return
	}
	if err := h.idempotency.Release(ctx, claim.ID); err != nil {
		logger.Warnf(ctx, "[gateway] idempotency release failed (claim id=%d): %v", claim.ID, err)
	}
}
