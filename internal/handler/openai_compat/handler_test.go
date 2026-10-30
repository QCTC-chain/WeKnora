package openai_compat

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Tencent/WeKnora/internal/config"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeAgentService satisfies the slice of CustomAgentService the gateway
// touches; everything else panics through the embedded nil interface.
type fakeAgentService struct {
	interfaces.CustomAgentService
	agents []*types.CustomAgent
	err    error
}

func (f *fakeAgentService) ListAgents(context.Context) ([]*types.CustomAgent, error) {
	return f.agents, f.err
}

func publishedAgent(id, name string, published bool) *types.CustomAgent {
	now := time.Date(2025, 1, 2, 3, 4, 5, 0, time.UTC)
	return &types.CustomAgent{
		ID:      id,
		Name:    name,
		TenantID: 42,
		Published: published,
		PublishedAt: &now,
		Config: types.CustomAgentConfig{
			AgentMode:    types.AgentModeQuickAnswer,
			SystemPrompt: "Answer briefly.",
		},
	}
}

// testGatewayCfg, when set, is handed to NewGatewayHandler by newTestHandler.
// Streaming/concurrency tests swap it to exercise config knobs; the gateway
// tests run sequentially, so plain assignment is a safe swap protocol.
var testGatewayCfg *config.Config

// newTestHandler builds a GatewayHandler with optional gateway-side fakes;
// nil sessions/messages keep list/resolve tests free of QA plumbing.
func newTestHandler(svc *fakeAgentService, sessions interfaces.SessionService, messages interfaces.MessageService) *GatewayHandler {
	return NewGatewayHandler(svc, sessions, messages, nil, testGatewayCfg)
}

// serveModels mounts ListModels behind nothing (auth is the middleware's
// job) but injects the tenant context GatewayAuth would provide.
func serveModels(t *testing.T, svc *fakeAgentService) *httptest.ResponseRecorder {
	t.Helper()
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.GET("/v1/models", func(c *gin.Context) {
		c.Request = c.Request.WithContext(
			context.WithValue(c.Request.Context(), types.TenantIDContextKey, uint64(42)))
		c.Next()
	}, newTestHandler(svc, nil, nil).ListModels)
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

func TestListModelsOnlyPublished(t *testing.T) {
	svc := &fakeAgentService{agents: []*types.CustomAgent{
		publishedAgent("agt-1", "Doc QA", true),
		publishedAgent("agt-2", "Hidden", false),
		publishedAgent("agt-3", "Wiki Bot", true),
	}}
	rec := serveModels(t, svc)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var body ModelListResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.Equal(t, "list", body.Object)
	require.Len(t, body.Data, 2)
	assert.Equal(t, "agt-1", body.Data[0].ID)
	assert.Equal(t, "agt-3", body.Data[1].ID)
	assert.Equal(t, "model", body.Data[0].Object)
	assert.Equal(t, "weknora", body.Data[0].OwnedBy)
	assert.Equal(t, "Doc QA", body.Data[0].Meta.AgentName)
	assert.Equal(t, string(types.AgentModeQuickAnswer), body.Data[0].Meta.AgentMode)
	assert.Equal(t, time.Date(2025, 1, 2, 3, 4, 5, 0, time.UTC).Unix(), body.Data[0].Created)
}

func TestListModelsEmptyIsArrayNotNull(t *testing.T) {
	rec := serveModels(t, &fakeAgentService{agents: nil})
	require.Equal(t, http.StatusOK, rec.Code)
	assert.JSONEq(t, `{"object":"list","data":[]}`, rec.Body.String())
}

func TestListModelsServiceError(t *testing.T) {
	rec := serveModels(t, &fakeAgentService{err: context.DeadlineExceeded})
	require.Equal(t, http.StatusInternalServerError, rec.Code)
	var body ErrorEnvelope
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Equal(t, "internal_error", body.Error.Code)
	assert.Equal(t, "server_error", body.Error.Type)
}

func TestResolveAgent(t *testing.T) {
	svc := &fakeAgentService{agents: []*types.CustomAgent{
		publishedAgent("agt-1", "Doc QA", true),
		publishedAgent("agt-2", "Hidden", false),
		publishedAgent("agt-3", "Wiki Bot", true),
	}}
	h := newTestHandler(svc, nil, nil)

	// UUID match wins and requires published state.
	agent, ok := h.resolveAgent(context.Background(), 42, "agt-1")
	require.True(t, ok)
	assert.Equal(t, "agt-1", agent.ID)

	// Unpublished UUID never resolves even when present in the list.
	_, ok = h.resolveAgent(context.Background(), 42, "agt-2")
	assert.False(t, ok, "unpublished agent must not resolve")

	// Name alias is trimmed + case-insensitive.
	agent, ok = h.resolveAgent(context.Background(), 42, " DOC qa ")
	require.True(t, ok)
	assert.Equal(t, "agt-1", agent.ID)

	// Unknown model fails.
	_, ok = h.resolveAgent(context.Background(), 42, "nope")
	assert.False(t, ok)

	// Empty / whitespace model fails.
	_, ok = h.resolveAgent(context.Background(), 42, "  ")
	assert.False(t, ok)

	// Two published agents sharing a name are ambiguous → fail closed.
	ambiguous := &fakeAgentService{agents: []*types.CustomAgent{
		publishedAgent("agt-1", "Doc QA", true),
		publishedAgent("agt-9", "doc qa", true),
	}}
	_, ok = newTestHandler(ambiguous, nil, nil).resolveAgent(context.Background(), 42, "doc qa")
	assert.False(t, ok, "duplicate published names must not resolve")
}

func TestTrimRunes(t *testing.T) {
	assert.Equal(t, "abc", trimRunes("  abc  ", 10))
	assert.Equal(t, "ab…", trimRunes("abcdef", 2))
	// Multi-byte runes are not split.
	assert.Equal(t, "你你…", trimRunes("你你你", 2))
}
