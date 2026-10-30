package openai_compat

// M2-6 observability tests: the hand-rolled Prometheus registry renders the
// exposition format, turn telemetry lands in the metric families, the
// audit_payload opt-in retains the sampled event journal, and translation
// drops/budget/model-not-found counters fire on their paths.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Tencent/WeKnora/internal/config"
	"github.com/Tencent/WeKnora/internal/event"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// metricLines parses exposition text into "metric{labels}" → value.
func metricLines(t *testing.T, exposition string) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, line := range strings.Split(exposition, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.SplitN(line, " ", 2)
		if len(parts) != 2 {
			continue
		}
		out[parts[0]] = parts[1]
	}
	return out
}

func gatewayMetricsBody(t *testing.T, h *GatewayHandler) map[string]string {
	t.Helper()
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.GET("/v1/metrics", h.GatewayMetrics)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/metrics", nil))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), "# TYPE gateway_requests_total counter")
	return metricLines(t, rec.Body.String())
}

// metricsTurnRouter mounts the completions handler with tenant + scope
// injection, shared by the metrics tests.
func metricsTurnRouter(
	t *testing.T, fakes completionsFakes, usage interfaces.GatewayUsageService,
	scope *types.TenantAPIKeyScope, gatewayCfg *config.GatewayConfig, audit *fakeGatewayAudit,
) (*gin.Engine, *GatewayHandler) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	cfg := &config.Config{}
	if gatewayCfg != nil {
		cfg.Gateway = gatewayCfg
	}
	var auditSink interfaces.AuditLogService
	if audit != nil {
		auditSink = audit
	}
	handler := NewGatewayHandler(fakes.agents, fakes.sessions, fakes.messages, auditSink, nil, usage, nil, cfg)
	router := gin.New()
	// Mounted like production so gateway_requests_total counts the turns.
	router.Use(GatewayMetricsMiddleware)
	router.POST("/v1/chat/completions", func(c *gin.Context) {
		ctx := context.WithValue(c.Request.Context(), types.TenantIDContextKey, uint64(42))
		if scope != nil {
			ctx = context.WithValue(ctx, types.TenantAPIKeyScopeContextKey, *scope)
		}
		c.Request = c.Request.WithContext(ctx)
		c.Next()
	}, handler.ChatCompletions)
	return router, handler
}

func postJSON(router *gin.Engine, path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

func TestGatewayMetricsExpositionAfterTurns(t *testing.T) {
	resetGatewayMetrics()
	fakes := streamFakes(answerScript)
	fakes.agents = &fakeAgentService{agents: []*types.CustomAgent{
		gatewayAgent("agt-1", "Doc QA", types.GatewayScopeTenant, "u1"),
	}}
	router, handler := metricsTurnRouter(t, fakes, &fakeGatewayUsage{}, keyScope(nil, nil, nil), nil, nil)

	// One streaming turn (TTFB lands) + one aggregate turn.
	require.Equal(t, http.StatusOK, postJSON(router, "/v1/chat/completions", streamBody("agt-1")).Code)
	require.Equal(t, http.StatusOK, postJSON(router, "/v1/chat/completions", simpleCompletionsBody()).Code)

	metrics := gatewayMetricsBody(t, handler)
	require.Equal(t, "2", metrics[`gateway_requests_total{protocol="openai",outcome="ok"}`])
	// Two turns (stream + aggregate), each prompt=120 completion=30.
	require.Equal(t, "240", metrics[`gateway_tokens_total{direction="prompt"}`])
	require.Equal(t, "60", metrics[`gateway_tokens_total{direction="completion"}`])
	// TTFB: only the streaming turn observes a first-content latency.
	require.NotEmpty(t, metrics[`gateway_ttfb_seconds_bucket{protocol="openai",agent="agt-1",le="+Inf"}`])
	require.Equal(t, "2", metrics[`gateway_turn_duration_seconds_count{protocol="openai"}`])
	// Amplification recorded for both turns (usage + visible answer).
	require.NotEmpty(t, metrics[`gateway_token_amplification_ratio_bucket{protocol="openai",le="+Inf"}`])
}

func TestGatewayMetricsBudgetAndNotFound(t *testing.T) {
	resetGatewayMetrics()
	fakes := streamFakes(answerScript)
	fakes.agents = &fakeAgentService{agents: []*types.CustomAgent{
		gatewayAgent("agt-1", "Doc QA", types.GatewayScopeTenant, "u1"),
	}}
	usage := &fakeGatewayUsage{requests: 5}
	router, handler := metricsTurnRouter(t, fakes, usage, keyScope(nil, nil, int64Ptr(5)), nil, nil)

	rec := postJSON(router, "/v1/chat/completions", simpleCompletionsBody())
	require.Equal(t, http.StatusTooManyRequests, rec.Code, rec.Body.String())

	// Unknown model: the 404 path (resolution happens before budgets).
	rec = postJSON(router, "/v1/chat/completions",
		`{"model":"nope","messages":[{"role":"user","content":"hi"}]}`)
	require.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())

	metrics := gatewayMetricsBody(t, handler)
	require.Equal(t, "1", metrics[`gateway_budget_exceeded_total{limit="key_requests"}`])
	require.Equal(t, "1", metrics[`gateway_model_not_found_total{agent="nope"}`])
	require.Equal(t, "1", metrics[`gateway_requests_total{protocol="openai",outcome="rate_limited"}`])
	require.Equal(t, "1", metrics[`gateway_requests_total{protocol="openai",outcome="not_found"}`])
}

func TestGatewayMetricsAnthropicRequests(t *testing.T) {
	resetGatewayMetrics()
	fakes := streamFakes(answerScript)
	fakes.agents = &fakeAgentService{agents: []*types.CustomAgent{
		gatewayAgent("agt-1", "Doc QA", types.GatewayScopeTenant, "u1"),
	}}
	gin.SetMode(gin.TestMode)
	handler := NewGatewayHandler(fakes.agents, fakes.sessions, fakes.messages, nil, nil, nil, nil, nil)
	router := gin.New()
	router.Use(GatewayMetricsMiddleware)
	router.POST("/v1/messages", func(c *gin.Context) {
		ctx := context.WithValue(c.Request.Context(), types.TenantIDContextKey, uint64(42))
		ctx = context.WithValue(ctx, types.TenantAPIKeyScopeContextKey, *keyScope(nil, nil, nil))
		c.Request = c.Request.WithContext(ctx)
		c.Next()
	}, handler.AnthropicMessages)

	require.Equal(t, http.StatusOK, postJSON(router, "/v1/messages", simpleMessagesBody(false)).Code)
	metrics := gatewayMetricsBody(t, handler)
	require.Equal(t, "1", metrics[`gateway_requests_total{protocol="anthropic",outcome="ok"}`])
	require.Equal(t, "120", metrics[`gateway_tokens_total{direction="prompt"}`])
}

func TestGatewayTranslationDropsMetric(t *testing.T) {
	resetGatewayMetrics()
	// thought_mode=drop + progress_mode=drop: the script's thought and
	// progress events are dropped and counted.
	cfg := &config.GatewayConfig{ThoughtMode: "drop", ProgressMode: "drop"}
	script := func(bus *event.EventBus, _ *types.QARequest) error {
		emit := func(typ event.EventType, data interface{}) error {
			return bus.Emit(context.Background(), event.Event{Type: typ, Data: data})
		}
		if err := emit(event.EventAgentThought, event.AgentThoughtData{Content: "思考"}); err != nil {
			return err
		}
		if err := emit(event.EventAgentProgress, event.AgentProgressData{Content: "检索中"}); err != nil {
			return err
		}
		if err := emit(event.EventAgentFinalAnswer, event.AgentFinalAnswerData{Content: "答案", Done: true}); err != nil {
			return err
		}
		return emit(event.EventAgentComplete, event.AgentCompleteData{FinalAnswer: "答案"})
	}
	fakes := streamFakes(script)
	fakes.agents = &fakeAgentService{agents: []*types.CustomAgent{
		gatewayAgent("agt-1", "Doc QA", types.GatewayScopeTenant, "u1"),
	}}
	router, handler := metricsTurnRouter(t, fakes, nil, keyScope(nil, nil, nil), cfg, nil)

	rec := postJSON(router, "/v1/chat/completions", streamBody("agt-1"))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.NotContains(t, rec.Body.String(), "思考")

	metrics := gatewayMetricsBody(t, handler)
	require.Equal(t, "1", metrics[`gateway_translation_dropped_total{kind="thought_dropped"}`])
	require.Equal(t, "1", metrics[`gateway_translation_dropped_total{kind="progress_dropped"}`])
}

func TestGatewayAuditEventJournal(t *testing.T) {
	audit := &fakeGatewayAudit{}
	cfg := &config.GatewayConfig{AuditPayload: true}
	script := func(bus *event.EventBus, _ *types.QARequest) error {
		emit := func(typ event.EventType, data interface{}) error {
			return bus.Emit(context.Background(), event.Event{Type: typ, Data: data})
		}
		if err := emit(event.EventAgentFinalAnswer, event.AgentFinalAnswerData{Content: "答案", Done: true}); err != nil {
			return err
		}
		return emit(event.EventAgentComplete, event.AgentCompleteData{
			FinalAnswer: "答案", TotalSteps: 4,
		})
	}
	fakes := streamFakes(script)
	fakes.agents = &fakeAgentService{agents: []*types.CustomAgent{
		gatewayAgent("agt-1", "Doc QA", types.GatewayScopeTenant, "u1"),
	}}
	router, _ := metricsTurnRouter(t, fakes, nil, keyScope(nil, nil, nil), cfg, audit)

	require.Equal(t, http.StatusOK, postJSON(router, "/v1/chat/completions", streamBody("agt-1")).Code)

	audit.mu.Lock()
	defer audit.mu.Unlock()
	require.Len(t, audit.rows, 1)
	var details map[string]any
	require.NoError(t, json.Unmarshal(audit.rows[0].Details, &details))
	require.Contains(t, details, "query", "audit_payload retains the rendered query")
	require.Contains(t, details, "event_log", "audit_payload retains the sampled event journal")
	require.Contains(t, details, "internal_rounds")
	// Journal entries are the sampled event flow, oldest first.
	log, ok := details["event_log"].([]any)
	require.True(t, ok, "event_log is an array")
	require.NotEmpty(t, log)
	first := log[0].(map[string]any)
	require.Contains(t, first, "t_ms")
	require.Contains(t, first, "event")
}

func TestGatewayMetricsExposureFormat(t *testing.T) {
	resetGatewayMetrics()
	gatewayMetrics.observeRequest(protoOpenAI, http.StatusOK)
	gatewayMetrics.observeTTFB(protoOpenAI, "agt-1", 42*1e6)
	gatewayMetrics.observeAmplification(protoOpenAI, 100, strings.Repeat("x", 25))
	render := renderGatewayMetrics(gatewayMetrics)
	for _, want := range []string{
		"# HELP gateway_requests_total ",
		"# TYPE gateway_requests_total counter",
		`gateway_requests_total{protocol="openai",outcome="ok"} 1`,
		"# TYPE gateway_ttfb_seconds histogram",
		`gateway_ttfb_seconds_bucket{protocol="openai",agent="agt-1",le="0.1"} 1`,
		`gateway_ttfb_seconds_bucket{protocol="openai",agent="agt-1",le="+Inf"} 1`,
		"# TYPE gateway_token_amplification_ratio histogram",
		`gateway_token_amplification_ratio_sum{protocol="openai"} 16.666666666`,
	} {
		require.Contains(t, render, want, "exposition line missing: %s", want)
	}
}

// compile-time guard: the audit fake stays a sink-capable double.
var _ interfaces.AuditLogService = (*fakeGatewayAudit)(nil)
