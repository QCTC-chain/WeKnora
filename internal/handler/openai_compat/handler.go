package openai_compat

import (
	"context"
	"strings"
	"time"

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
	// shares resolves org-level agent shares for published_scope=org
	// visibility (M2-a). Nil in tests: org-scoped agents stay visible to
	// their own tenant (creator-only outside it) — a documented degradation,
	// never a wider surface.
	shares interfaces.AgentShareRepository
	// usage meters gateway turns and answers quota probes (M2-b). Nil in
	// tests: no metering, no budget gate (documented degradation).
	usage interfaces.GatewayUsageService
	// turnLocks serializes turns per continued session (M2-3): concurrent
	// sends on one thread answer 409 session_busy.
	turnLocks *sessionTurnLocks
	// idempotency stores (key, Idempotency-Key) claims for response replay
	// (M2-4). Nil in tests: header ignored (documented degradation).
	idempotency interfaces.GatewayIdempotencyStore
	// gateway carries the gateway config section (timeouts, streaming
	// modes, concurrency, audit payload); nil falls back to the defaults.
	gateway *config.GatewayConfig
	// concurrency enforces gateway.max_concurrency_per_key (docs §9);
	// process-local by design (docs/concurrency.go).
	concurrency *perKeyConcurrency
}

// NewGatewayHandler wires the gateway handler. agents/sessions/messages are
// the same services the workspace UI uses — the gateway deliberately sees
// agents with tenant isolation and zero extra authority. shares backs the
// M2-a org scope; nil keeps org-scoped agents tenant-local. cfg carries the
// gateway section; nil falls back to the defaults.
func NewGatewayHandler(
	agents interfaces.CustomAgentService,
	sessions interfaces.SessionService,
	messages interfaces.MessageService,
	audit interfaces.AuditLogService,
	shares interfaces.AgentShareRepository,
	usage interfaces.GatewayUsageService,
	idempotency interfaces.GatewayIdempotencyStore,
	cfg *config.Config,
) *GatewayHandler {
	h := &GatewayHandler{agents: agents, sessions: sessions, messages: messages, audit: audit, shares: shares, usage: usage, idempotency: idempotency, concurrency: newPerKeyConcurrency(), turnLocks: newSessionTurnLocks()}
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
	agents, err := h.visibleAgents(ctx, tenantID)
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
	data = filterGrantedModels(ctx, data)
	c.JSON(200, ModelListResponse{Object: "list", Data: data})
}

// modelEntry renders one published agent as an OpenAI model entry. M2-a adds
// the publish-snapshot version and the deprecation transition hints.
func modelEntry(agent *types.CustomAgent) ModelEntry {
	meta := &ModelMeta{
		AgentName:   agent.Name,
		Description: trimRunes(agent.Description, 200),
		AgentMode:   string(agent.GatewayEffectiveConfig().AgentMode),
		IsBuiltin:   agent.IsBuiltin,
		Version:     agent.GatewayVersion,
		Scope:       agent.EffectiveGatewayScope(),
	}
	if agent.GatewayDeprecated(time.Now()) {
		meta.Deprecated = true
		meta.SunsetAt = agent.GatewaySunset.UTC().Format(time.RFC3339)
	}
	return ModelEntry{
		ID:      agent.ID,
		Object:  "model",
		Created: unixTime(agent.PublishedAt),
		OwnedBy: "weknora",
		Meta:    meta,
	}
}

// resolveAgent maps a completions `model` string to a gateway-visible agent.
// UUID wins first; the trimmed, case-insensitive agent name is the alias.
// Visibility follows M2-a published_scope (private/org/tenant) plus the
// deprecation sunset; unpublished, sunset-expired, out-of-scope and ambiguous
// names all answer 404 model_not_found — never 403, so the gateway does not
// leak agent existence across workspaces.
//
// The returned agent carries the publish snapshot config (M2-a): edits made
// after the last publish do not change what the gateway executes. Covered by
// unit tests: the alias rules are part of the published contract.
func (h *GatewayHandler) resolveAgent(ctx context.Context, tenantID uint64, model string) (*types.CustomAgent, bool) {
	model = strings.TrimSpace(model)
	if model == "" {
		return nil, false
	}
	agents, err := h.visibleAgents(ctx, tenantID)
	if err != nil {
		logger.ErrorWithFields(ctx, err, map[string]interface{}{"tenant_id": tenantID})
		return nil, false
	}
	// Pass 1: exact ID (published + in-scope only).
	for _, agent := range agents {
		if agent != nil && agent.Published && agent.ID == model {
			return withGatewaySnapshot(agent), true
		}
	}
	// Pass 2: name alias among visible agents.
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
	if found == nil {
		return nil, false
	}
	return withGatewaySnapshot(found), true
}

// withGatewaySnapshot returns a shallow copy of the agent whose Config is
// the publish snapshot when one exists (M2-a). The caller's copy is what the
// QA engine mutates (EnsureDefaults), so it must never write through to the
// cached service row.
func withGatewaySnapshot(agent *types.CustomAgent) *types.CustomAgent {
	if agent.GatewaySnapshot == nil {
		// No governance snapshot (M1-era publish): the row config is the
		// effective config; keep pointer identity for zero-diff behavior.
		return agent
	}
	copy_ := *agent
	snapshot := agent.GatewayEffectiveConfig()
	copy_.Config = snapshot
	return &copy_
}

// visibleAgents lists the agents the caller may reach on the gateway: own
// workspace published agents filtered by M2-a published_scope and the
// deprecation sunset, plus agents published with scope=org into an
// organization the caller's tenant belongs to (the existing agent_shares
// system; a share only counts while both sides remain org members).
func (h *GatewayHandler) visibleAgents(ctx context.Context, tenantID uint64) ([]*types.CustomAgent, error) {
	own, err := h.agents.ListAgents(ctx)
	if err != nil {
		return nil, err
	}
	userID, _ := types.UserIDFromContext(ctx)
	var sharedIDs map[string]struct{}
	if h.shares != nil {
		shares, shareErr := h.shares.ListSharedAgentsForTenant(ctx, tenantID)
		if shareErr != nil {
			logger.Warnf(ctx, "[gateway] org share lookup failed; org-scoped agents degrade to tenant visibility: tenant=%d err=%v",
				tenantID, shareErr)
		} else {
			sharedIDs = make(map[string]struct{}, len(shares))
			for _, share := range shares {
				if share == nil || share.AgentID == "" {
					continue
				}
				sharedIDs[share.AgentID] = struct{}{}
			}
		}
	}
	now := time.Now()
	visible := make([]*types.CustomAgent, 0, len(own))
	for _, agent := range own {
		if agent == nil || !agent.Published || agent.GatewaySunsetExpired(now) {
			continue
		}
		if h.agentInScope(agent, tenantID, userID, sharedIDs) {
			visible = append(visible, agent)
		}
	}
	if h.shares != nil && sharedIDs != nil {
		// Foreign org-shared agents: shares resolve through the same join, so
		// every share for this tenant whose agent is a published org-scoped
		// agent extends the reachable set across tenants.
		shares, shareErr := h.shares.ListSharedAgentsForTenant(ctx, tenantID)
		if shareErr == nil {
			seen := make(map[string]struct{}, len(visible))
			for _, a := range visible {
				seen[a.ID] = struct{}{}
			}
			for _, share := range shares {
				if share == nil || share.Agent == nil {
					continue
				}
				agent := share.Agent
				if _, dup := seen[agent.ID]; agent.TenantID == tenantID || dup {
					continue
				}
				if !agent.Published || agent.GatewaySunsetExpired(now) {
					continue
				}
				if agent.EffectiveGatewayScope() == types.GatewayScopeOrg {
					visible = append(visible, agent)
					seen[agent.ID] = struct{}{}
				}
			}
		}
	}
	return visible, nil
}

// agentInScope reports whether one own-workspace agent is callable by the
// caller identified by (tenantID, userID). sharedIDs holds the agent ids the
// caller's tenant reaches through org shares; nil (share lookup unavailable
// or returned nothing) degrades org scope to creator-only.
func (h *GatewayHandler) agentInScope(
	agent *types.CustomAgent, tenantID uint64, userID string, sharedIDs map[string]struct{},
) bool {
	if agent.TenantID != tenantID {
		return false
	}
	isCreator := userID != "" && userID == agent.CreatedBy
	switch agent.EffectiveGatewayScope() {
	case types.GatewayScopePrivate:
		return isCreator
	case types.GatewayScopeOrg:
		if isCreator {
			return true
		}
		_, ok := sharedIDs[agent.ID]
		return ok
	default: // tenant (also the legacy empty value)
		return true
	}
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
