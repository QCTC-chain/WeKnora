package openai_compat

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Tencent/WeKnora/internal/event"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// fakeGatewayUsage captures metering rows and answers quota probes from an
// in-memory accumulator (M2-b).
type fakeGatewayUsage struct {
	interfaces.GatewayUsageService
	rows     []*types.GatewayUsage
	tokens   int64 // pre-existing today usage for quota probes
	requests int64
}

func (f *fakeGatewayUsage) Record(_ context.Context, entry *types.GatewayUsage) error {
	f.rows = append(f.rows, entry)
	return nil
}

func (f *fakeGatewayUsage) QuotaUsed(context.Context, uint64, time.Time) (int64, int64, error) {
	return f.tokens, f.requests, nil
}

func (f *fakeGatewayUsage) TenantQuotaUsed(context.Context, uint64, time.Time) (int64, error) {
	return f.tokens, nil
}

// serveWithGrants mounts handlers with the given API-key scope context (what
// GatewayAuth attaches), plus usage service.
func serveWithScope(
	t *testing.T, svc *fakeAgentService, usage interfaces.GatewayUsageService,
	scope *types.TenantAPIKeyScope, method, path, body string,
) *httptest.ResponseRecorder {
	t.Helper()
	gin.SetMode(gin.TestMode)
	router := gin.New()
	handler := NewGatewayHandler(svc, nil, nil, nil, nil, usage, nil, nil, nil)
	inject := func(c *gin.Context) {
		ctx := context.WithValue(c.Request.Context(), types.TenantIDContextKey, uint64(42))
		if scope != nil {
			ctx = context.WithValue(ctx, types.TenantAPIKeyScopeContextKey, *scope)
		}
		c.Request = c.Request.WithContext(ctx)
		c.Next()
	}
	switch {
	case path == "/v1/models":
		router.GET(path, inject, handler.ListModels)
	default:
		router.POST(path, inject, handler.ChatCompletions)
	}
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

func keyScope(grants []string, tokenBudget, requestBudget *int64) *types.TenantAPIKeyScope {
	return &types.TenantAPIKeyScope{
		KeyID:              7,
		Name:               "integration",
		ScopeType:          types.APIKeyScopeTenant,
		Capabilities:       types.StringArray{"chat"},
		AgentGrants:        grants,
		DailyTokenBudget:   tokenBudget,
		DailyRequestBudget: requestBudget,
	}
}

func int64Ptr(v int64) *int64 { return &v }

func TestGatewayModelsGrantsFilter(t *testing.T) {
	svc := &fakeAgentService{agents: []*types.CustomAgent{
		gatewayAgent("agt-1", "Doc QA", types.GatewayScopeTenant, "u1"),
		gatewayAgent("agt-2", "Wiki Bot", types.GatewayScopeTenant, "u1"),
	}}
	// No grants: everything visible.
	rec := serveWithScope(t, svc, nil, keyScope(nil, nil, nil), http.MethodGet, "/v1/models", "")
	var body ModelListResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.Len(t, body.Data, 2)

	// Grants=[agt-2]: only agt-2.
	rec = serveWithScope(t, svc, nil, keyScope([]string{"agt-2"}, nil, nil), http.MethodGet, "/v1/models", "")
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.Len(t, body.Data, 1)
	require.Equal(t, "agt-2", body.Data[0].ID)
}

func TestGatewayCompletionsGrantDenied404(t *testing.T) {
	svc := &fakeAgentService{agents: []*types.CustomAgent{
		gatewayAgent("agt-1", "Doc QA", types.GatewayScopeTenant, "u1"),
	}}
	rec := serveWithScope(t, svc, nil, keyScope([]string{"agt-other"}, nil, nil),
		http.MethodPost, "/v1/chat/completions", simpleCompletionsBody())
	require.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), "not granted")

	// Empty grants = unrestricted.
	rec = serveCompletionsWithScope(t, svc, nil, keyScope(nil, nil, nil),
		simpleCompletionsBody(), answerScript)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
}

func TestGatewayBudgetExceeded429(t *testing.T) {
	svc := &fakeAgentService{agents: []*types.CustomAgent{
		gatewayAgent("agt-1", "Doc QA", types.GatewayScopeTenant, "u1"),
	}}
	// Request budget reached: next request answers 429 budget_exceeded.
	usage := &fakeGatewayUsage{requests: 5}
	rec := serveWithScope(t, svc, usage, keyScope(nil, nil, int64Ptr(5)),
		http.MethodPost, "/v1/chat/completions", simpleCompletionsBody())
	require.Equal(t, http.StatusTooManyRequests, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), "budget_exceeded")
	require.Len(t, usage.rows, 0, "rejected request must not create a usage row")

	// Token budget reached.
	usage2 := &fakeGatewayUsage{tokens: 1000}
	rec = serveWithScope(t, svc, usage2, keyScope(nil, int64Ptr(1000), nil),
		http.MethodPost, "/v1/chat/completions", simpleCompletionsBody())
	require.Equal(t, http.StatusTooManyRequests, rec.Code, rec.Body.String())

	// Under budget: passes and records one usage row.
	usage3 := &fakeGatewayUsage{tokens: 10, requests: 1}
	rec = serveCompletionsWithScope(t, svc, usage3, keyScope(nil, int64Ptr(100000), int64Ptr(100)),
		simpleCompletionsBody(), answerScript)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Len(t, usage3.rows, 1)
	require.Equal(t, "agt-1", usage3.rows[0].AgentID)
	require.EqualValues(t, 7, usage3.rows[0].KeyID)
	require.Equal(t, "completed", usage3.rows[0].EndReason)
	require.EqualValues(t, 150, usage3.rows[0].TotalTokens, "usage row carries the turn's total tokens")
}

// simpleCompletionsBodyWithScript is serveWithScope's session-backed variant
// for POST bodies: it runs the full completions flow with a scripted session.
func serveCompletionsWithScope(
	t *testing.T, svc *fakeAgentService, usage interfaces.GatewayUsageService,
	scope *types.TenantAPIKeyScope, body string, script func(*event.EventBus, *types.QARequest) error,
) *httptest.ResponseRecorder {
	t.Helper()
	gin.SetMode(gin.TestMode)
	router := gin.New()
	handler := NewGatewayHandler(svc, &fakeGatewaySessions{script: script}, &fakeGatewayMessages{}, nil, nil, usage, nil, nil, nil)
	router.POST("/v1/chat/completions", func(c *gin.Context) {
		ctx := context.WithValue(c.Request.Context(), types.TenantIDContextKey, uint64(42))
		if scope != nil {
			ctx = context.WithValue(ctx, types.TenantAPIKeyScopeContextKey, *scope)
		}
		c.Request = c.Request.WithContext(ctx)
		c.Next()
	}, handler.ChatCompletions)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

func TestGatewayUsageErrorTurnRecorded(t *testing.T) {
	svc := &fakeAgentService{agents: []*types.CustomAgent{
		gatewayAgent("agt-1", "Doc QA", types.GatewayScopeTenant, "u1"),
	}}
	usage := &fakeGatewayUsage{}
	rec := serveCompletionsWithScope(t, svc, usage, keyScope(nil, nil, nil),
		simpleCompletionsBody(),
		func(bus *event.EventBus, _ *types.QARequest) error { return errors.New("boom") })
	require.Equal(t, http.StatusInternalServerError, rec.Code, rec.Body.String())
	require.Len(t, usage.rows, 1, "failed turns are metered too")
	require.Equal(t, "error", usage.rows[0].EndReason)
	require.Equal(t, 500, usage.rows[0].StatusCode)
}
