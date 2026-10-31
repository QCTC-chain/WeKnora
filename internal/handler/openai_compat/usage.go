package openai_compat

import (
	"context"
	"net/http"
	"time"

	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/gin-gonic/gin"
)

// M2-b per-key grants + metering + quotas (docs/design/agent-model-gateway-m2-m3.md §2).
//
// Grants: an API key's AgentGrants whitelist (empty = all published,
// non-empty = only listed agents) narrows /v1/models and completions.
// Metering: one gateway_usage row per executed turn; failures log and
// continue. Quotas: key-level daily budgets (from the key row) and the
// tenant-level total gate (config) are probed at request entry and answer
// 429 budget_exceeded; actual consumption is recorded at turn end.

// keyGrants returns the caller key's agent whitelist (nil = unrestricted).
func keyGrants(ctx context.Context) types.StringArray {
	if scope, ok := types.TenantAPIKeyScopeFromContext(ctx); ok {
		return scope.AgentGrants
	}
	return nil
}

// agentGranted reports whether the resolved agent is callable by the key's
// grant whitelist. Unrestricted keys grant everything; restricted keys only
// their listed agents. Platform-scoped keys follow the same rule (their
// scope carries the grants too).
func agentGranted(ctx context.Context, agentID string) bool {
	grants := keyGrants(ctx)
	if len(grants) == 0 {
		return true
	}
	for _, id := range grants {
		if id == agentID {
			return true
		}
	}
	return false
}

// filterGranted narrows a model list to the key's grants.
func filterGranted(ctx context.Context, agents []*types.CustomAgent) []*types.CustomAgent {
	grants := keyGrants(ctx)
	if len(grants) == 0 {
		return agents
	}
	allowed := make(map[string]struct{}, len(grants))
	for _, id := range grants {
		allowed[id] = struct{}{}
	}
	out := agents[:0:0]
	for _, agent := range agents {
		if _, ok := allowed[agent.ID]; ok {
			out = append(out, agent)
		}
	}
	return out
}

// filterGrantedModels narrows a rendered model list to the key's grants.
func filterGrantedModels(ctx context.Context, entries []ModelEntry) []ModelEntry {
	grants := keyGrants(ctx)
	if len(grants) == 0 {
		return entries
	}
	allowed := make(map[string]struct{}, len(grants))
	for _, id := range grants {
		allowed[id] = struct{}{}
	}
	out := entries[:0:0]
	for _, entry := range entries {
		if _, ok := allowed[entry.ID]; ok {
			out = append(out, entry)
		}
	}
	return out
}

// quotaDecision is the outcome of the request-entry budget probe.
type quotaDecision struct {
	ok     bool
	reason string // human-readable detail for the 429 body
	// limit names the exceeded scope for metrics: key_requests | key_tokens | tenant_tokens.
	limit string
}

// checkGatewayBudget probes the entry-time budgets: the key's daily token
// and request budgets plus the tenant-level total gate. A probe failure
// (e.g. usage table missing) fails OPEN: metering must not take the
// gateway down. Docs §2: budgets are enforced at entry (estimate) and
// actualized at turn end.
func (h *GatewayHandler) checkGatewayBudget(ctx context.Context, tenantID uint64) quotaDecision {
	if h.usage == nil {
		return quotaDecision{ok: true}
	}
	scope, ok := types.TenantAPIKeyScopeFromContext(ctx)
	keyID := uint64(0)
	if ok {
		keyID = scope.KeyID
	}
	now := time.Now()
	if ok && keyID > 0 {
		if scope.DailyRequestBudget != nil && *scope.DailyRequestBudget > 0 {
			_, requests, err := h.usage.QuotaUsed(ctx, keyID, now)
			if err != nil {
				logger.Warnf(ctx, "[gateway] quota probe failed; failing open: key=%d err=%v", keyID, err)
				return quotaDecision{ok: true}
			}
			if requests >= *scope.DailyRequestBudget {
				return quotaDecision{ok: false, reason: "daily request budget exceeded for this API key", limit: "key_requests"}
			}
		}
		if scope.DailyTokenBudget != nil && *scope.DailyTokenBudget > 0 {
			tokens, _, err := h.usage.QuotaUsed(ctx, keyID, now)
			if err != nil {
				logger.Warnf(ctx, "[gateway] quota probe failed; failing open: key=%d err=%v", keyID, err)
				return quotaDecision{ok: true}
			}
			if tokens >= *scope.DailyTokenBudget {
				return quotaDecision{ok: false, reason: "daily token budget exceeded for this API key", limit: "key_tokens"}
			}
		}
	}
	if budget := h.gateway.EffectiveTenantDailyTokenBudget(); budget > 0 {
		tokens, err := h.usage.TenantQuotaUsed(ctx, tenantID, now)
		if err != nil {
			logger.Warnf(ctx, "[gateway] tenant quota probe failed; failing open: tenant=%d err=%v", tenantID, err)
			return quotaDecision{ok: true}
		}
		if tokens >= budget {
			return quotaDecision{ok: false, reason: "workspace daily gateway token budget exceeded", limit: "tenant_tokens"}
		}
	}
	return quotaDecision{ok: true}
}

// writeBudgetExceeded answers the documented 429 budget_exceeded shape.
func writeBudgetExceeded(c *gin.Context, reason string) {
	writeError(c, http.StatusTooManyRequests, "budget_exceeded", reason+" ("+budgetResetHint()+")")
}

// budgetResetHint tells clients when the daily counters reset (UTC).
func budgetResetHint() string {
	now := time.Now().UTC()
	reset := now.Truncate(24 * time.Hour).Add(24 * time.Hour)
	return "resets at " + reset.Format("2006-01-02T15:04:05Z")
}

// recordGatewayTurnUsage appends one gateway_usage row for the executed
// turn. Failures log and never fail the response — the answer has already
// been produced; quota drift of one row is acceptable, an outage is not.
func (h *GatewayHandler) recordGatewayTurnUsage(
	ctx context.Context, tenantID uint64, turn gatewayTurnAudit, statusCode int,
) {
	if h.usage == nil {
		return
	}
	usage := &types.GatewayUsage{
		TenantID:   tenantID,
		KeyID:      turn.KeyID,
		AgentID:    turn.AgentID,
		SessionID:  turn.SessionID,
		Model:      turn.Model,
		Stream:     turn.Stream,
		DurationMS: turn.Duration.Milliseconds(),
		EndReason:  turn.EndReason,
		StatusCode: statusCode,
		CreatedAt:  time.Now(),
	}
	if turn.Usage != nil {
		usage.PromptTokens = int64(turn.Usage.PromptTokens)
		usage.CompletionTokens = int64(turn.Usage.CompletionTokens)
		usage.TotalTokens = int64(turn.Usage.TotalTokens)
	}
	if err := h.usage.Record(ctx, usage); err != nil {
		logger.Warnf(ctx, "[gateway] usage record failed: agent=%s key=%d err=%v",
			turn.AgentID, turn.KeyID, err)
	}
}
