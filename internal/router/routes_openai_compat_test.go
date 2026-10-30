package router

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Tencent/WeKnora/internal/config"
	"github.com/Tencent/WeKnora/internal/handler/openai_compat"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// gatewayRouterFakes provides the three service interfaces GatewayAuth needs.
type gatewayRouterFakes struct {
	interfaces.TenantService
	interfaces.UserService
	interfaces.TenantAPIKeyService
	tenant *types.Tenant
	user   *types.User
	key    *types.TenantAPIKey
}

func (f *gatewayRouterFakes) GetTenantByID(context.Context, uint64) (*types.Tenant, error) {
	return f.tenant, nil
}

func (f *gatewayRouterFakes) GetUserByTenantID(context.Context, uint64) (*types.User, error) {
	return f.user, nil
}

func (f *gatewayRouterFakes) AuthenticateAPIKey(context.Context, string) (*types.TenantAPIKey, error) {
	return f.key, nil
}

type gatewayRouterAgents struct {
	interfaces.CustomAgentService
	agents []*types.CustomAgent
}

func (f *gatewayRouterAgents) ListAgents(context.Context) ([]*types.CustomAgent, error) {
	return f.agents, nil
}

// newGatewayTestRouter mounts the gateway on a bare engine — the same shape
// NewRouter uses (gateway registered before global Auth).
func newGatewayTestRouter(t *testing.T, key *types.TenantAPIKey) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	fakes := &gatewayRouterFakes{
		tenant: &types.Tenant{ID: 42, Status: "active"},
		user:   &types.User{ID: "u1", IsActive: true},
		key:    key,
	}
	agents := &gatewayRouterAgents{agents: []*types.CustomAgent{{
		ID: "agt-1", Name: "Doc QA", TenantID: 42, Published: true,
	}}}
	r := gin.New()
	RegisterOpenAICompatRoutes(r, openai_compat.NewGatewayHandler(agents, nil, nil, nil, nil), fakes, fakes, fakes)
	return r
}

func gatewayChatKey() *types.TenantAPIKey {
	tenantID := uint64(42)
	return &types.TenantAPIKey{
		ID: 7, TenantID: &tenantID, ScopeType: types.APIKeyScopeTenant,
		Capabilities: types.StringArray{string(types.APIKeyCapabilityChat)},
	}
}

func TestGatewayModelsRouteServedWithBearerAPIKey(t *testing.T) {
	r := newGatewayTestRouter(t, gatewayChatKey())
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer wk-secret")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var body openai_compat.ModelListResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.Len(t, body.Data, 1)
	require.Equal(t, "agt-1", body.Data[0].ID)
	require.Equal(t, "weknora", body.Data[0].OwnedBy)
}

func TestGatewayModelsRouteRejectsMissingKey(t *testing.T) {
	r := newGatewayTestRouter(t, gatewayChatKey())
	rec := httptest.NewRecorder()
	r.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	require.Equal(t, http.StatusUnauthorized, rec.Code)
}

func TestGatewayModelsRouteRejectsWrongMethod(t *testing.T) {
	r := newGatewayTestRouter(t, gatewayChatKey())
	req := httptest.NewRequest(http.MethodPost, "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer wk-secret")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	// gin answers 404 for unregistered method+path by default (HandleMethodNotAllowed off).
	require.NotEqual(t, http.StatusOK, rec.Code)
}

func TestGatewayRouteMountsExactlyAtV1(t *testing.T) {
	// The gateway must not shadow /api/v1/*: only the bare /v1 prefix is ours.
	r := newGatewayTestRouter(t, gatewayChatKey())
	for _, path := range []string{"/api/v1/models", "/v1", "/v1/", "/v1/models/extra"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Authorization", "Bearer wk-secret")
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		require.NotEqual(t, http.StatusOK, rec.Code, "path %s must not be served by the gateway", path)
	}
}

func TestGatewayChatCompletionsRouteMounted(t *testing.T) {
	// The POST route must exist behind the same GatewayAuth (401 without a
	// key — not gin's 404 for an unregistered route).
	r := newGatewayTestRouter(t, gatewayChatKey())
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	require.Equal(t, http.StatusUnauthorized, rec.Code)

	authed := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{}`))
	authed.Header.Set("Authorization", "Bearer wk-secret")
	authed.Header.Set("Content-Type", "application/json")
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, authed)
	// nil SessionService would panic on a real run; a 400 from body
	// validation proves the route is mounted and reached the handler.
	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
}

func TestGatewayConfigDisabledByDefault(t *testing.T) {
	require.False(t, (*config.GatewayConfig)(nil).IsEnabled())
	require.False(t, (&config.GatewayConfig{}).IsEnabled())
	require.True(t, (&config.GatewayConfig{Enabled: true}).IsEnabled())
}
