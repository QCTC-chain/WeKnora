package middleware

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	"github.com/gin-gonic/gin"
)

// gatewayFakeUserService satisfies the slice of UserService the gateway auth
// path touches; every other method panics via the embedded nil interface.
type gatewayFakeUserService struct {
	interfaces.UserService
	user *types.User
}

func (f *gatewayFakeUserService) GetUserByTenantID(context.Context, uint64) (*types.User, error) {
	return f.user, nil
}

// gatewayFakeAPIKeyService resolves one canned key.
type gatewayFakeAPIKeyService struct {
	interfaces.TenantAPIKeyService
	key *types.TenantAPIKey
	err error
}

func (f *gatewayFakeAPIKeyService) AuthenticateAPIKey(context.Context, string) (*types.TenantAPIKey, error) {
	return f.key, f.err
}

func gatewayKey(capabilities ...types.APIKeyCapability) *types.TenantAPIKey {
	caps := make(types.StringArray, 0, len(capabilities))
	for _, c := range capabilities {
		caps = append(caps, string(c))
	}
	tenantID := uint64(42)
	return &types.TenantAPIKey{
		ID: 7, TenantID: &tenantID, ScopeType: types.APIKeyScopeTenant, Capabilities: caps,
	}
}

func platformGatewayKey(capabilities ...types.APIKeyCapability) *types.TenantAPIKey {
	key := gatewayKey(capabilities...)
	key.ScopeType = types.APIKeyScopePlatform
	return key
}

// serveGateway runs GatewayAuth followed by a marker handler that records the
// authenticated tenant ID, then returns the recorder.
func serveGateway(
	t *testing.T, key *types.TenantAPIKey, keyErr error, tenant *types.Tenant,
	headers map[string]string,
) (*httptest.ResponseRecorder, uint64) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.GET("/v1/models",
		GatewayAuth(
			&fakeTenantService{tenant: tenant},
			&gatewayFakeUserService{user: &types.User{ID: "u1", IsActive: true}},
			&gatewayFakeAPIKeyService{key: key, err: keyErr},
		),
		func(c *gin.Context) {
			tenantID, _ := types.TenantIDFromContext(c.Request.Context())
			c.JSON(http.StatusOK, gin.H{"tenant_id": tenantID})
		},
	)
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec, 0
}

// gatewayAuthTenantID extracts the authenticated tenant from the marker
// handler's JSON body.
func gatewayAuthTenantID(t *testing.T, rec *httptest.ResponseRecorder) uint64 {
	t.Helper()
	var body struct {
		TenantID uint64 `json:"tenant_id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("marker body: %v (%s)", err, rec.Body.String())
	}
	return body.TenantID
}

// gatewayAuthErrorCode extracts error.code from an OpenAI envelope body.
func gatewayAuthErrorCode(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var body struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("error envelope: %v (%s)", err, rec.Body.String())
	}
	return body.Error.Code
}

func TestGatewayAuthMissingKeyRejected(t *testing.T) {
	rec, _ := serveGateway(t, gatewayKey(), nil, &types.Tenant{ID: 42, Status: "active"}, nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 (%s)", rec.Code, rec.Body.String())
	}
	if got := gatewayAuthErrorCode(t, rec); got != "invalid_api_key" {
		t.Fatalf("code = %q, want invalid_api_key", got)
	}
	if rec.Header().Get("WWW-Authenticate") == "" {
		t.Fatal("401 must carry WWW-Authenticate")
	}
}

func TestGatewayAuthInvalidKeyRejected(t *testing.T) {
	rec, _ := serveGateway(t, nil, context.Canceled, &types.Tenant{ID: 42, Status: "active"},
		map[string]string{"Authorization": "Bearer wk-wrong"})
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	if got := gatewayAuthErrorCode(t, rec); got != "invalid_api_key" {
		t.Fatalf("code = %q, want invalid_api_key", got)
	}
}

func TestGatewayAuthBearerAPIKeyAuthenticates(t *testing.T) {
	rec, _ := serveGateway(t, gatewayKey(types.APIKeyCapabilityChat), nil,
		&types.Tenant{ID: 42, Status: "active"},
		map[string]string{"Authorization": "Bearer wk-secret"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	if got := gatewayAuthTenantID(t, rec); got != 42 {
		t.Fatalf("tenant = %d, want 42", got)
	}
}

func TestGatewayAuthXAPIKeyHeaderAuthenticates(t *testing.T) {
	rec, _ := serveGateway(t, gatewayKey(types.APIKeyCapabilityChat), nil,
		&types.Tenant{ID: 42, Status: "active"},
		map[string]string{"X-API-Key": "wk-secret"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
}

// TestGatewayAuthLowercaseXAPIKeyAuthenticates proves the Anthropic header
// spelling (x-api-key) resolves through Go header canonicalization — the
// /v1/messages shell relies on it.
func TestGatewayAuthLowercaseXAPIKeyAuthenticates(t *testing.T) {
	rec, _ := serveGateway(t, gatewayKey(types.APIKeyCapabilityChat), nil,
		&types.Tenant{ID: 42, Status: "active"},
		map[string]string{"x-api-key": "wk-secret"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
}

func TestGatewayAuthFullAccessKeyAllowed(t *testing.T) {
	key := gatewayKey()
	key.FullAccess = true
	rec, _ := serveGateway(t, key, nil, &types.Tenant{ID: 42, Status: "active"},
		map[string]string{"Authorization": "Bearer wk-secret"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
}

func TestGatewayAuthCapabilityRejected(t *testing.T) {
	rec, _ := serveGateway(t, gatewayKey(types.APIKeyCapabilityRetrieve), nil,
		&types.Tenant{ID: 42, Status: "active"},
		map[string]string{"Authorization": "Bearer wk-secret"})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (%s)", rec.Code, rec.Body.String())
	}
	if got := gatewayAuthErrorCode(t, rec); got != "insufficient_capability" {
		t.Fatalf("code = %q, want insufficient_capability", got)
	}
}

func TestGatewayAuthPlatformKeyRequiresTenantHeader(t *testing.T) {
	rec, _ := serveGateway(t, platformGatewayKey(types.APIKeyCapabilityChat), nil,
		&types.Tenant{ID: 42, Status: "active"},
		map[string]string{"Authorization": "Bearer wk-platform"})
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 (%s)", rec.Code, rec.Body.String())
	}
	if got := gatewayAuthErrorCode(t, rec); got != "tenant_required" {
		t.Fatalf("code = %q, want tenant_required", got)
	}
}

func TestGatewayAuthPlatformKeyWithTenantHeader(t *testing.T) {
	rec, _ := serveGateway(t, platformGatewayKey(types.APIKeyCapabilityChat), nil,
		&types.Tenant{ID: 42, Status: "active"},
		map[string]string{
			"Authorization": "Bearer wk-platform",
			"X-Tenant-ID":   "42",
		})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	if got := gatewayAuthTenantID(t, rec); got != 42 {
		t.Fatalf("tenant = %d, want 42", got)
	}
}

func TestGatewayAuthWorkspaceKeyCannotSwitchTenant(t *testing.T) {
	rec, _ := serveGateway(t, gatewayKey(types.APIKeyCapabilityChat), nil,
		&types.Tenant{ID: 42, Status: "active"},
		map[string]string{
			"Authorization": "Bearer wk-secret",
			"X-Tenant-ID":   "43",
		})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (%s)", rec.Code, rec.Body.String())
	}
	if got := gatewayAuthErrorCode(t, rec); got != "workspace_mismatch" {
		t.Fatalf("code = %q, want workspace_mismatch", got)
	}
}

func TestGatewayAuthInactiveTenantRejected(t *testing.T) {
	rec, _ := serveGateway(t, gatewayKey(types.APIKeyCapabilityChat), nil,
		&types.Tenant{ID: 42, Status: "inactive"},
		map[string]string{"Authorization": "Bearer wk-secret"})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (%s)", rec.Code, rec.Body.String())
	}
	if got := gatewayAuthErrorCode(t, rec); got != "workspace_inactive" {
		t.Fatalf("code = %q, want workspace_inactive", got)
	}
}

func TestGatewayKeyAllowedMatrix(t *testing.T) {
	cases := []struct {
		name string
		key  *types.TenantAPIKey
		want bool
	}{
		{"nil", nil, false},
		{"tenant chat", gatewayKey(types.APIKeyCapabilityChat), true},
		{"tenant retrieve only", gatewayKey(types.APIKeyCapabilityRetrieve), false},
		{"tenant full access", func() *types.TenantAPIKey {
			k := gatewayKey()
			k.FullAccess = true
			return k
		}(), true},
		{"platform chat", platformGatewayKey(types.APIKeyCapabilityChat), true},
		{"platform full access no chat", func() *types.TenantAPIKey {
			k := platformGatewayKey()
			k.FullAccess = true
			return k
		}(), false},
	}
	for _, tc := range cases {
		if got := gatewayKeyAllowed(tc.key); got != tc.want {
			t.Fatalf("%s: gatewayKeyAllowed = %v, want %v", tc.name, got, tc.want)
		}
	}
}
