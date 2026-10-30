package openai_compat

import (
	"context"
	"strings"

	"github.com/Tencent/WeKnora/internal/config"
	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	"github.com/gin-gonic/gin"
)

// GatewayHandler serves the OpenAI-compatible gateway surface (/v1). It is
// mounted BEFORE the global Auth middleware; every route carries
// middleware.GatewayAuth, so by the time a handler runs the request has a
// tenant-scoped API-key session attached.
type GatewayHandler struct {
	agents   interfaces.CustomAgentService
	sessions interfaces.SessionService
	messages interfaces.MessageService
	// audit receives one row per executed gateway turn (docs §6); nil in
	// tests degrades audit to a log line, never to a failed request.
	audit interfaces.AuditLogService
	// gateway carries the gateway config section (timeouts, streaming
	// modes, concurrency, audit payload); nil falls back to the defaults.
	gateway *config.GatewayConfig
	// concurrency enforces gateway.max_concurrency_per_key (docs §9);
	// process-local by design (docs/concurrency.go).
	concurrency *perKeyConcurrency
}

// NewGatewayHandler wires the gateway handler. agents/sessions/messages are
// the same services the workspace UI uses — the gateway deliberately sees
// agents with tenant isolation and zero extra authority. cfg carries the
// gateway section; nil falls back to the defaults.
func NewGatewayHandler(
	agents interfaces.CustomAgentService,
	sessions interfaces.SessionService,
	messages interfaces.MessageService,
	audit interfaces.AuditLogService,
	cfg *config.Config,
) *GatewayHandler {
	h := &GatewayHandler{agents: agents, sessions: sessions, messages: messages, audit: audit, concurrency: newPerKeyConcurrency()}
	if cfg != nil {
		h.gateway = cfg.Gateway
	}
	return h
}

// ListModels godoc
// @Summary      网关模型列表
// @Description  OpenAI 兼容端点:列出当前空间已发布(可通过网关调用)的智能体。
// @Description  每个智能体呈现为一个 model,id 为智能体 UUID;meta.agent_name
// @Description  同时可作为 chat/completions 的 model 别名。
// @Description  鉴权使用 WeKnora API key(Authorization: Bearer 或 X-API-Key,需要 chat 能力位)。
// @Tags         模型网关
// @Produce      json
// @Success      200  {object}  openai_compat.ModelListResponse  "模型列表"
// @Failure      401  {object}  openai_compat.ErrorEnvelope      "缺少或无效 API key"
// @Failure      403  {object}  openai_compat.ErrorEnvelope      "API key 缺少 chat 能力位"
// @Security     ApiKeyAuth
// @Router       /v1/models [get]
func (h *GatewayHandler) ListModels(c *gin.Context) {
	ctx := c.Request.Context()
	tenantID, ok := types.TenantIDFromContext(ctx)
	if !ok || tenantID == 0 {
		// Unreachable behind GatewayAuth; defensive only.
		logger.Error(ctx, "Gateway ListModels called without tenant context")
		writeError(c, 500, "internal_error", "Workspace context missing")
		return
	}
	agents, err := h.agents.ListAgents(ctx)
	if err != nil {
		logger.ErrorWithFields(ctx, err, map[string]interface{}{"tenant_id": tenantID})
		writeError(c, 500, "internal_error", "Failed to list agents")
		return
	}
	data := make([]ModelEntry, 0, len(agents))
	for _, agent := range agents {
		if agent == nil || !agent.Published {
			continue
		}
		data = append(data, modelEntry(agent))
	}
	c.JSON(200, ModelListResponse{Object: "list", Data: data})
}

// modelEntry renders one published agent as an OpenAI model entry.
func modelEntry(agent *types.CustomAgent) ModelEntry {
	entry := ModelEntry{
		ID:      agent.ID,
		Object:  "model",
		Created: unixTime(agent.PublishedAt),
		OwnedBy: "weknora",
		Meta: &ModelMeta{
			AgentName:   agent.Name,
			Description: trimRunes(agent.Description, 200),
			AgentMode:   string(agent.Config.AgentMode),
			IsBuiltin:   agent.IsBuiltin,
		},
	}
	return entry
}

// resolveAgent maps a completions `model` string to a published agent of the
// caller's workspace. UUID wins first; the trimmed, case-insensitive agent
// name is the alias. Unpublished, foreign-workspace and ambiguous names all
// answer 404 model_not_found — never 403, so the gateway does not leak agent
// existence across workspaces.
//
// Exported for the chat/completions milestone (G2); covered by unit tests now
// because the alias rules are part of the published contract.
func (h *GatewayHandler) resolveAgent(ctx context.Context, tenantID uint64, model string) (*types.CustomAgent, bool) {
	model = strings.TrimSpace(model)
	if model == "" {
		return nil, false
	}
	agents, err := h.agents.ListAgents(ctx)
	if err != nil {
		logger.ErrorWithFields(ctx, err, map[string]interface{}{"tenant_id": tenantID})
		return nil, false
	}
	// Pass 1: exact ID (published only).
	for _, agent := range agents {
		if agent != nil && agent.Published && agent.ID == model {
			return agent, true
		}
	}
	// Pass 2: name alias among published agents.
	var found *types.CustomAgent
	for _, agent := range agents {
		if agent == nil || !agent.Published {
			continue
		}
		if strings.EqualFold(strings.TrimSpace(agent.Name), model) {
			if found != nil {
				// Ambiguous (publish-time uniqueness should prevent this;
				// legacy data may not). Fail closed with 404 upstream.
				return nil, false
			}
			found = agent
		}
	}
	return found, found != nil
}

// trimRunes bounds a display string to max runes without splitting a rune.
func trimRunes(s string, max int) string {
	s = strings.TrimSpace(s)
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	return string(runes[:max]) + "…"
}
