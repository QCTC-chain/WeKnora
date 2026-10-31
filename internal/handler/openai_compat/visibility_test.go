package openai_compat

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// fakeShareRepo serves ListSharedAgentsForTenant for scope=org tests; every
// other method panics through the embedded nil interface.
type fakeShareRepo struct {
	interfaces.AgentShareRepository
	shares []*types.AgentShare
	err    error
}

func (f *fakeShareRepo) ListSharedAgentsForTenant(context.Context, uint64) ([]*types.AgentShare, error) {
	return f.shares, f.err
}

func orgShare(agentID string, sourceTenant uint64, agent *types.CustomAgent) *types.AgentShare {
	return &types.AgentShare{AgentID: agentID, SourceTenantID: sourceTenant, Agent: agent}
}

// serveModelsWithUser mounts ListModels with tenant+user context.
func serveModelsWithUser(t *testing.T, svc *fakeAgentService, shares interfaces.AgentShareRepository, userID string) *httptest.ResponseRecorder {
	t.Helper()
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.GET("/v1/models", func(c *gin.Context) {
		ctx := context.WithValue(c.Request.Context(), types.TenantIDContextKey, uint64(42))
		if userID != "" {
			ctx = context.WithValue(ctx, types.UserIDContextKey, userID)
		}
		c.Request = c.Request.WithContext(ctx)
		c.Next()
	}, NewGatewayHandler(svc, nil, nil, nil, shares, nil, nil, nil, nil).ListModels, nil)
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

func modelIDs(t *testing.T, rec *httptest.ResponseRecorder) []string {
	t.Helper()
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var body ModelListResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	ids := make([]string, 0, len(body.Data))
	for _, m := range body.Data {
		ids = append(ids, m.ID)
	}
	return ids
}

func gatewayAgent(id, name string, scope string, createdBy string) *types.CustomAgent {
	agent := publishedAgent(id, name, true)
	agent.GatewayScope = scope
	agent.CreatedBy = createdBy
	agent.GatewayVersion = 2
	return agent
}

func TestGatewayVisibilityTenantScopeDefault(t *testing.T) {
	// Legacy rows (empty scope) behave as tenant: whole workspace sees them.
	svc := &fakeAgentService{agents: []*types.CustomAgent{
		gatewayAgent("agt-1", "Doc QA", "", "u1"),
	}}
	rec := serveModelsWithUser(t, svc, nil, "someone-else")
	ids := modelIDs(t, rec)
	require.Equal(t, []string{"agt-1"}, ids)
}

func TestGatewayVisibilityPrivateScope(t *testing.T) {
	svc := &fakeAgentService{agents: []*types.CustomAgent{
		gatewayAgent("agt-1", "Doc QA", types.GatewayScopePrivate, "u1"),
	}}
	// Creator sees it.
	require.Equal(t, []string{"agt-1"}, modelIDs(t, serveModelsWithUser(t, svc, nil, "u1")))
	// Another workspace user does not.
	require.Empty(t, modelIDs(t, serveModelsWithUser(t, svc, nil, "u2")))
	// No user in context (service-key caller) — private is creator-only, so hidden.
	require.Empty(t, modelIDs(t, serveModelsWithUser(t, svc, nil, "")))
}

func TestGatewayVisibilityOrgScopeOwnTenant(t *testing.T) {
	svc := &fakeAgentService{agents: []*types.CustomAgent{
		gatewayAgent("agt-1", "Doc QA", types.GatewayScopeOrg, "u1"),
	}}
	// Creator always sees their own org-scoped agent.
	require.Equal(t, []string{"agt-1"}, modelIDs(t, serveModelsWithUser(t, svc, nil, "u1")))
	// Non-creator without an org share: hidden.
	require.Empty(t, modelIDs(t, serveModelsWithUser(t, svc, nil, "u2")))
	// Non-creator with the agent org-shared to an org this tenant belongs to.
	shares := &fakeShareRepo{shares: []*types.AgentShare{orgShare("agt-1", 42, nil)}}
	require.Equal(t, []string{"agt-1"}, modelIDs(t, serveModelsWithUser(t, svc, shares, "u2")))
}

func TestGatewayVisibilityOrgScopeCrossTenant(t *testing.T) {
	// Tenant 7's org-scoped agent, shared into an org tenant 42 belongs to.
	foreign := gatewayAgent("agt-7", "Shared QA", types.GatewayScopeOrg, "u7")
	foreign.TenantID = 7
	svc := &fakeAgentService{agents: []*types.CustomAgent{foreign}}
	shares := &fakeShareRepo{shares: []*types.AgentShare{orgShare("agt-7", 7, foreign)}}
	require.Equal(t, []string{"agt-7"}, modelIDs(t, serveModelsWithUser(t, svc, shares, "u2")))

	// Without the share row, tenant 42 sees nothing.
	require.Empty(t, modelIDs(t, serveModelsWithUser(t, svc, nil, "u2")))
}

func TestGatewayVisibilityOrgCrossTenantTenantScopeNotLeaked(t *testing.T) {
	// Defense in depth: a share row pointing at a foreign agent must not
	// surface it unless the agent itself is org-scoped and published.
	foreign := gatewayAgent("agt-7", "Shared QA", types.GatewayScopeTenant, "u7")
	foreign.TenantID = 7
	svc := &fakeAgentService{agents: []*types.CustomAgent{foreign}}
	shares := &fakeShareRepo{shares: []*types.AgentShare{orgShare("agt-7", 7, foreign)}}
	require.Empty(t, modelIDs(t, serveModelsWithUser(t, svc, shares, "u2")))
}

func TestGatewayVisibilitySunsetExpired(t *testing.T) {
	past := time.Now().Add(-time.Hour)
	future := time.Now().Add(24 * time.Hour)
	expired := gatewayAgent("agt-1", "Doc QA", types.GatewayScopeTenant, "u1")
	expired.GatewaySunset = &past
	live := gatewayAgent("agt-2", "Wiki Bot", types.GatewayScopeTenant, "u1")
	live.GatewaySunset = &future
	svc := &fakeAgentService{agents: []*types.CustomAgent{expired, live}}

	ids := modelIDs(t, serveModelsWithUser(t, svc, nil, "u1"))
	require.Equal(t, []string{"agt-2"}, ids, "sunset-expired agent must resolve as unpublished")

	// Deprecated-but-callable agent carries meta hints.
	var body ModelListResponse
	require.NoError(t, json.Unmarshal(serveModelsWithUser(t, svc, nil, "u1").Body.Bytes(), &body))
	require.Len(t, body.Data, 1)
	require.True(t, body.Data[0].Meta.Deprecated, "future sunset = deprecated transition")
	require.NotEmpty(t, body.Data[0].Meta.SunsetAt)
	require.EqualValues(t, 2, body.Data[0].Meta.Version)
	require.Equal(t, types.GatewayScopeTenant, body.Data[0].Meta.Scope)
}

func TestResolveAgentSnapshotConfig(t *testing.T) {
	agent := gatewayAgent("agt-1", "Doc QA", types.GatewayScopeTenant, "u1")
	agent.Config = types.CustomAgentConfig{AgentMode: types.AgentModeSmartReasoning, SystemPrompt: "live"}
	agent.GatewaySnapshot = &types.CustomAgentConfig{AgentMode: types.AgentModeQuickAnswer, SystemPrompt: "frozen"}
	svc := &fakeAgentService{agents: []*types.CustomAgent{agent}}
	h := NewGatewayHandler(svc, nil, nil, nil, nil, nil, nil, nil, nil)
	ctx := context.WithValue(context.Background(), types.TenantIDContextKey, uint64(42))

	got, ok := h.resolveAgent(ctx, 42, "Doc QA")
	require.True(t, ok)
	require.NotSame(t, agent, got, "resolve must hand out a copy, not the cached row")
	require.Equal(t, types.AgentModeQuickAnswer, got.Config.AgentMode, "gateway must execute the publish snapshot")
	require.Equal(t, "frozen", got.Config.SystemPrompt)
	// The stored row is untouched.
	require.Equal(t, types.AgentModeSmartReasoning, agent.Config.AgentMode)

	// Name resolution is case-insensitive and trim-tolerant.
	got, ok = h.resolveAgent(ctx, 42, "  doc qa ")
	require.True(t, ok)
	require.Equal(t, "agt-1", got.ID)
}

func TestResolveAgentSunsetExpiredGone(t *testing.T) {
	past := time.Now().Add(-time.Hour)
	agent := gatewayAgent("agt-1", "Doc QA", types.GatewayScopeTenant, "u1")
	agent.GatewaySunset = &past
	svc := &fakeAgentService{agents: []*types.CustomAgent{agent}}
	h := NewGatewayHandler(svc, nil, nil, nil, nil, nil, nil, nil, nil)
	ctx := context.WithValue(context.Background(), types.TenantIDContextKey, uint64(42))

	_, ok := h.resolveAgent(ctx, 42, "agt-1")
	require.False(t, ok, "sunset-expired agent must answer 404, like unpublished")
}

func TestResolveAgentPrivateScopeOtherUser404(t *testing.T) {
	agent := gatewayAgent("agt-1", "Doc QA", types.GatewayScopePrivate, "u1")
	svc := &fakeAgentService{agents: []*types.CustomAgent{agent}}
	h := NewGatewayHandler(svc, nil, nil, nil, nil, nil, nil, nil, nil)
	ctx := context.WithValue(
		context.WithValue(context.Background(), types.TenantIDContextKey, uint64(42)),
		types.UserIDContextKey, "u2")

	_, ok := h.resolveAgent(ctx, 42, "agt-1")
	require.False(t, ok)
}

func TestCompletionsDeprecationHeader(t *testing.T) {
	future := time.Now().Add(24 * time.Hour).UTC().Truncate(time.Second)
	agent := gatewayAgent("agt-1", "Doc QA", types.GatewayScopeTenant, "u1")
	agent.GatewaySunset = &future
	fakes := completionsFakes{
		agents:   &fakeAgentService{agents: []*types.CustomAgent{agent}},
		sessions: &fakeGatewaySessions{script: answerScript},
		messages: &fakeGatewayMessages{},
	}
	rec := serveCompletions(t, fakes, simpleCompletionsBody(), "u1")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Equal(t, "sunset="+future.Format(time.RFC3339),
		rec.Header().Get("X-WeKnora-Deprecation"))
}
