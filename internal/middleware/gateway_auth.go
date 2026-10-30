package middleware

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	"github.com/gin-gonic/gin"
)

// GatewayAuth authenticates OpenAI-compatible gateway requests (/v1) with a
// WeKnora tenant API key and attaches the same auth session the global
// middleware would for an X-API-Key caller.
//
// It exists because external OpenAI clients standardly send the key as
// `Authorization: Bearer <api-key>`, while the global Auth middleware only
// tries API keys under the X-API-Key header (bearer is JWT-first there and a
// non-JWT bearer is rejected). The gateway is registered BEFORE the global
// Auth middleware (like the MCP endpoint surface), so it carries its own
// self-contained auth that:
//
//   - accepts the key from `Authorization: Bearer` or `X-API-Key`;
//   - never accepts JWT sessions — this is a machine surface;
//   - requires the `chat` capability (or tenant full-access);
//   - resolves tenant + principal through the same helpers as the global
//     middleware so downstream services cannot tell the difference.
//
// All rejections use the OpenAI error envelope so clients surface them the
// way they surface upstream provider errors.
func GatewayAuth(
	tenantService interfaces.TenantService,
	userService interfaces.UserService,
	apiKeyService interfaces.TenantAPIKeyService,
) gin.HandlerFunc {
	return func(c *gin.Context) {
		apiKey := gatewayAPIKey(c)
		if apiKey == "" {
			writeGatewayAuthError(c, http.StatusUnauthorized, "invalid_api_key",
				"Missing API key: send it as 'Authorization: Bearer <key>' or 'X-API-Key: <key>'")
			return
		}
		ctx := c.Request.Context()
		key, err := apiKeyService.AuthenticateAPIKey(ctx, apiKey)
		if err != nil || key == nil {
			writeGatewayAuthError(c, http.StatusUnauthorized, "invalid_api_key",
				"Incorrect API key provided")
			return
		}
		if !gatewayKeyAllowed(key) {
			writeGatewayAuthError(c, http.StatusForbidden, "insufficient_capability",
				"API key is not allowed to call agents: missing 'chat' capability")
			return
		}

		var tenantID uint64
		if key.IsPlatform() {
			// Platform keys must pick a workspace explicitly; the gateway has
			// no tenant-optional routes.
			header := strings.TrimSpace(c.GetHeader("X-Tenant-ID"))
			if header == "" {
				writeGatewayAuthError(c, http.StatusUnauthorized, "tenant_required",
					"Platform API keys must send X-Tenant-ID")
				return
			}
			tenantID, err = strconv.ParseUint(header, 10, 64)
			if err != nil || tenantID == 0 {
				writeGatewayAuthError(c, http.StatusBadRequest, "invalid_tenant",
					"Invalid X-Tenant-ID header")
				return
			}
		} else {
			tenantID = key.TenantIDValue()
			if tenantID == 0 {
				writeGatewayAuthError(c, http.StatusUnauthorized, "invalid_api_key",
					"API key has no workspace scope")
				return
			}
			// A workspace key must never cross tenants, mirroring the global
			// middleware's X-Tenant-ID guard.
			if header := strings.TrimSpace(c.GetHeader("X-Tenant-ID")); header != "" {
				if requested, parseErr := strconv.ParseUint(header, 10, 64); parseErr != nil ||
					requested != tenantID {
					writeGatewayAuthError(c, http.StatusForbidden, "workspace_mismatch",
						"Forbidden: workspace API key cannot switch workspaces")
					return
				}
			}
		}

		if tenant, lookupErr := tenantService.GetTenantByID(ctx, tenantID); lookupErr == nil &&
			tenant != nil {
			status := strings.ToLower(tenant.Status)
			if status == "inactive" || status == "disabled" {
				writeGatewayAuthError(c, http.StatusForbidden, "workspace_inactive",
					"Workspace is inactive")
				return
			}
		}

		// Same session attach as the global X-API-Key path: identical context
		// keys, principal resolution, and API-key scope, so handlers and
		// services downstream stay unaware of which middleware ran.
		attachAPIKeyAuthContext(c, tenantService, userService, tenantID, key)
		if c.IsAborted() {
			return
		}
		c.Next()
	}
}

// gatewayAPIKey extracts the key from Authorization: Bearer first, then
// X-API-Key. A bearer that is not an API key simply fails authentication
// downstream — the gateway never falls back to JWT.
func gatewayAPIKey(c *gin.Context) string {
	if token, ok := bearerToken(c); ok && strings.TrimSpace(token) != "" {
		return strings.TrimSpace(token)
	}
	return strings.TrimSpace(c.GetHeader("X-API-Key"))
}

// gatewayKeyAllowed reports whether the key may call the gateway: tenant
// full-access, or a scoped key carrying the chat capability (the same one
// that gates /agent-chat today).
func gatewayKeyAllowed(key *types.TenantAPIKey) bool {
	if key == nil {
		return false
	}
	if key.FullAccess && !key.IsPlatform() {
		return true
	}
	// Platform keys and scoped tenant keys both need the chat capability
	// explicitly; full-access alone does not open the gateway.
	for _, capability := range key.Capabilities {
		if types.APIKeyCapability(capability) == types.APIKeyCapabilityChat {
			return true
		}
	}
	return false
}

// writeGatewayAuthError answers with the OpenAI error envelope. WWW-Authenticate
// signals the accepted scheme on 401s so plain HTTP clients get a hint.
func writeGatewayAuthError(c *gin.Context, status int, code, message string) {
	if status == http.StatusUnauthorized {
		c.Header("WWW-Authenticate", `Bearer realm="weknora-gateway"`)
	}
	c.AbortWithStatusJSON(status, gin.H{
		"error": gin.H{
			"message": message,
			"type":    gatewayErrorType(status),
			"code":    code,
		},
	})
}

// gatewayErrorType maps HTTP status to the OpenAI error.type vocabulary.
func gatewayErrorType(status int) string {
	switch status {
	case http.StatusUnauthorized:
		return "authentication_error"
	case http.StatusForbidden:
		return "permission_error"
	case http.StatusNotFound:
		return "invalid_request_error"
	case http.StatusTooManyRequests:
		return "rate_limit_error"
	case http.StatusBadRequest:
		return "invalid_request_error"
	default:
		return "server_error"
	}
}
