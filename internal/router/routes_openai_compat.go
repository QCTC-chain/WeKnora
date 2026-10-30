package router

import (
	"github.com/Tencent/WeKnora/internal/handler/openai_compat"
	"github.com/Tencent/WeKnora/internal/middleware"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	"github.com/gin-gonic/gin"
)

// GatewayV1BasePath is the mount point of the OpenAI-compatible gateway.
// Keep it a bare top-level path: OpenAI clients join <base_url>/models and
// <base_url>/chat/completions themselves.
const GatewayV1BasePath = "/v1"

// RegisterOpenAICompatRoutes mounts the OpenAI-compatible gateway surface:
//
//	GET /v1/models             — published agents as models
//	POST /v1/chat/completions  — (G2 milestone) agents as chat models
//
// It authenticates with WeKnora tenant API keys sent as
// `Authorization: Bearer <key>` (the OpenAI client convention) or X-API-Key,
// and therefore must be registered BEFORE the global Auth middleware — the
// same pattern as the MCP endpoint surface (RegisterMCPServerRoutes).
func RegisterOpenAICompatRoutes(
	r *gin.Engine,
	gateway *openai_compat.GatewayHandler,
	tenantService interfaces.TenantService,
	userService interfaces.UserService,
	apiKeyService interfaces.TenantAPIKeyService,
) {
	if gateway == nil {
		return
	}
	auth := middleware.GatewayAuth(tenantService, userService, apiKeyService)
	grp := r.Group(GatewayV1BasePath, auth)
	grp.GET("/models", gateway.ListModels)
}
