package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Tencent/WeKnora/internal/application/repository"
	"github.com/Tencent/WeKnora/internal/application/service"
	"github.com/Tencent/WeKnora/internal/middleware"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// publishRepo serves one agent to the publish flow and records writes.
type publishRepo struct {
	interfaces.CustomAgentRepository
	agent  *types.CustomAgent
	list   []*types.CustomAgent
	err    error
	writes int
}

func (r *publishRepo) GetAgentByID(context.Context, string, uint64) (*types.CustomAgent, error) {
	return r.agent, r.err
}

func (r *publishRepo) ListAgentsByTenantID(context.Context, uint64) ([]*types.CustomAgent, error) {
	return r.list, nil
}

func (r *publishRepo) UpdateAgent(_ context.Context, a *types.CustomAgent) error {
	r.writes++
	r.agent = a
	return r.err
}

func patchPublished(t *testing.T, repo *publishRepo, body string) *httptest.ResponseRecorder {
	t.Helper()
	gin.SetMode(gin.TestMode)
	svc := service.NewCustomAgentService(repo, nil, nil, nil, nil, nil, nil, nil)
	h := &CustomAgentHandler{service: svc}
	router := gin.New()
	router.Use(func(c *gin.Context) {
		// Stand in for the auth middleware's tenant context.
		c.Request = c.Request.WithContext(
			context.WithValue(c.Request.Context(), types.TenantIDContextKey, uint64(1)))
		c.Next()
	})
	router.Use(middleware.ErrorHandler())
	router.PATCH("/agents/:id", h.PatchAgent)
	req := httptest.NewRequest(http.MethodPatch, "/agents/custom-1", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

func TestPatchAgentPublishOnly(t *testing.T) {
	repo := &publishRepo{agent: &types.CustomAgent{
		ID: "custom-1", Name: "Doc QA", TenantID: 1,
		Config: types.CustomAgentConfig{AgentMode: types.AgentModeQuickAnswer},
	}}
	rec := patchPublished(t, repo, `{"published": true}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var body struct {
		Data types.CustomAgent `json:"data"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.True(t, body.Data.Published)
	require.NotNil(t, body.Data.PublishedAt)
	require.Equal(t, 1, repo.writes, "publish-only PATCH must write exactly once")
	// Publish must not disturb the agent config.
	require.Equal(t, types.AgentModeQuickAnswer, body.Data.Config.AgentMode)

	// Unpublish clears both fields (fresh target: omitted fields in the
	// response JSON must not leak values from the previous unmarshal).
	rec = patchPublished(t, repo, `{"published": false}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var unpublishBody struct {
		Data types.CustomAgent `json:"data"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &unpublishBody))
	require.False(t, unpublishBody.Data.Published)
	require.Nil(t, unpublishBody.Data.PublishedAt)
}

func TestPatchAgentPublishNameConflict409(t *testing.T) {
	repo := &publishRepo{
		agent: &types.CustomAgent{ID: "custom-1", Name: "Doc QA", TenantID: 1},
		list: []*types.CustomAgent{
			{ID: "custom-2", Name: "doc qa", TenantID: 1, Published: true},
		},
	}
	rec := patchPublished(t, repo, `{"published": true}`)
	require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
	require.False(t, repo.agent.Published, "conflicted publish must not flip the flag")
}

func TestPatchAgentPublishBuiltin403(t *testing.T) {
	repo := &publishRepo{agent: &types.CustomAgent{
		ID: "builtin-quick-answer", Name: "快速问答", TenantID: 1, IsBuiltin: true,
	}}
	rec := patchPublished(t, repo, `{"published": true}`)
	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
}

func TestPatchAgentPublishNotFound404(t *testing.T) {
	repo := &publishRepo{err: repository.ErrCustomAgentNotFound}
	rec := patchPublished(t, repo, `{"published": true}`)
	require.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())
}

func TestPatchAgentPublishCombinedWithConfig(t *testing.T) {
	// published + description goes through the normal config-update path; the
	// publish state set first must survive the full update.
	repo := &publishRepo{agent: &types.CustomAgent{
		ID: "custom-1", Name: "Doc QA", TenantID: 1,
		Description: "old",
		Config:      types.CustomAgentConfig{AgentMode: types.AgentModeQuickAnswer},
	}}
	rec := patchPublished(t, repo, `{"published": true, "description": "new"}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var body struct {
		Data types.CustomAgent `json:"data"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.Equal(t, "new", body.Data.Description)
	require.True(t, body.Data.Published)
	require.NotNil(t, body.Data.PublishedAt)
}

func TestPatchAgentPublishUnnamedAgent400(t *testing.T) {
	repo := &publishRepo{agent: &types.CustomAgent{ID: "custom-1", Name: "  ", TenantID: 1}}
	rec := patchPublished(t, repo, `{"published": true}`)
	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
}

// --- M2-a publish governance: scope / sunset / audit ---

// recordingAudit captures audit rows written by the publish PATCH.
type recordingAudit struct {
	interfaces.AuditLogService
	rows []*types.AuditLog
}

func (a *recordingAudit) Log(_ context.Context, entry *types.AuditLog) error {
	a.rows = append(a.rows, entry)
	return nil
}

func patchPublishedWithAudit(t *testing.T, repo *publishRepo, audit *recordingAudit, body string) *httptest.ResponseRecorder {
	t.Helper()
	gin.SetMode(gin.TestMode)
	svc := service.NewCustomAgentService(repo, nil, nil, nil, nil, nil, nil, nil)
	h := &CustomAgentHandler{service: svc, audit: audit}
	router := gin.New()
	router.Use(func(c *gin.Context) {
		c.Request = c.Request.WithContext(context.WithValue(
			c.Request.Context(), types.TenantIDContextKey, uint64(1)))
		c.Request = c.Request.WithContext(context.WithValue(
			c.Request.Context(), types.UserIDContextKey, "user-1"))
		c.Next()
	})
	router.Use(middleware.ErrorHandler())
	router.PATCH("/agents/:id", h.PatchAgent)
	req := httptest.NewRequest(http.MethodPatch, "/agents/custom-1", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

func governanceBody(t *testing.T, rec *httptest.ResponseRecorder) types.CustomAgent {
	t.Helper()
	var body struct {
		Data types.CustomAgent `json:"data"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	return body.Data
}

func TestPatchAgentPublishGovernanceFields(t *testing.T) {
	future := time.Now().Add(72 * time.Hour).UTC().Truncate(time.Second)
	repo := &publishRepo{agent: &types.CustomAgent{ID: "custom-1", Name: "Doc QA", TenantID: 1}}
	rec := patchPublishedWithAudit(t, repo, &recordingAudit{}, `{
		"published": true,
		"published_scope": "private",
		"deprecation_sunset": "`+future.Format(time.RFC3339)+`"
	}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	got := governanceBody(t, rec)
	require.True(t, got.Published)
	require.Equal(t, types.GatewayScopePrivate, got.GatewayScope)
	require.EqualValues(t, 1, got.GatewayVersion)
	require.NotNil(t, got.GatewaySnapshot, "publish must freeze a snapshot")
	require.NotNil(t, got.GatewaySunset)
}

func TestPatchAgentPublishGovernanceInvalidScope400(t *testing.T) {
	repo := &publishRepo{agent: &types.CustomAgent{ID: "custom-1", Name: "Doc QA", TenantID: 1}}
	rec := patchPublishedWithAudit(t, repo, &recordingAudit{}, `{"published": true, "published_scope": "world"}`)
	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
}

func TestPatchAgentPublishGovernanceInvalidSunset400(t *testing.T) {
	repo := &publishRepo{agent: &types.CustomAgent{ID: "custom-1", Name: "Doc QA", TenantID: 1}}
	rec := patchPublishedWithAudit(t, repo, &recordingAudit{},
		`{"published": true, "deprecation_sunset": "2020-01-01T00:00:00Z"}`)
	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
}

func TestPatchAgentPublishGovernanceSunsetNullClears(t *testing.T) {
	future := time.Now().Add(72 * time.Hour)
	repo := &publishRepo{agent: &types.CustomAgent{
		ID: "custom-1", Name: "Doc QA", TenantID: 1, Published: true,
		GatewayScope: types.GatewayScopeTenant, GatewayVersion: 3, GatewaySunset: &future,
	}}
	rec := patchPublishedWithAudit(t, repo, &recordingAudit{}, `{"deprecation_sunset": null}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	got := governanceBody(t, rec)
	require.Nil(t, got.GatewaySunset, "deprecation_sunset:null must clear the sunset")
	require.EqualValues(t, 3, got.GatewayVersion, "sunset clear must not bump the version")
}

func TestPatchAgentPublishAuditActions(t *testing.T) {
	future := time.Now().Add(72 * time.Hour)
	audit := &recordingAudit{}

	// Publish → gateway.publish
	repo := &publishRepo{agent: &types.CustomAgent{ID: "custom-1", Name: "Doc QA", TenantID: 1}}
	rec := patchPublishedWithAudit(t, repo, audit, `{"published": true}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Len(t, audit.rows, 1)
	require.Equal(t, types.AuditActionGatewayPublish, audit.rows[0].Action)
	require.Equal(t, "user-1", audit.rows[0].ActorUserID)

	// Scope-only PATCH on the published agent → gateway.governance
	rec = patchPublishedWithAudit(t, repo, audit, `{"published_scope": "org"}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Len(t, audit.rows, 2)
	require.Equal(t, types.AuditActionGatewayGovernance, audit.rows[1].Action)

	// Unpublish → gateway.unpublish, version retained
	rec = patchPublishedWithAudit(t, repo, audit, `{"published": false}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Len(t, audit.rows, 3)
	require.Equal(t, types.AuditActionGatewayUnpublish, audit.rows[2].Action)
	require.EqualValues(t, 1, repo.agent.GatewayVersion)
	_ = future
}

func TestPatchAgentPublishToolPolicyGovernance(t *testing.T) {
	repo := &publishRepo{agent: &types.CustomAgent{ID: "custom-1", Name: "Doc QA", TenantID: 1}}
	// Publish + policy in one call; the policy freezes into the snapshot.
	rec := patchPublishedWithAudit(t, repo, &recordingAudit{},
		`{"published": true, "gateway_tool_policy": "client_only"}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	got := governanceBody(t, rec)
	require.True(t, got.Published)
	require.Equal(t, types.GatewayToolPolicyClientOnly, got.Config.GatewayToolPolicy)
	require.NotNil(t, got.GatewaySnapshot)
	require.Equal(t, types.GatewayToolPolicyClientOnly, got.GatewaySnapshot.GatewayToolPolicy)

	// A policy-only PATCH skips the config path and answers directly.
	repo2 := &publishRepo{agent: &types.CustomAgent{
		ID: "custom-1", Name: "Doc QA", TenantID: 1, Published: true, GatewayVersion: 2,
	}}
	rec = patchPublished(t, repo2, `{"gateway_tool_policy": "none"}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	got = governanceBody(t, rec)
	require.Equal(t, types.GatewayToolPolicyNone, got.Config.GatewayToolPolicy)
	require.EqualValues(t, 2, got.GatewayVersion, "policy flip must not release a version")

	// Invalid values are rejected on the governance path.
	repo3 := &publishRepo{agent: &types.CustomAgent{ID: "custom-1", Name: "Doc QA", TenantID: 1}}
	rec = patchPublished(t, repo3, `{"gateway_tool_policy": "sometimes"}`)
	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
}
