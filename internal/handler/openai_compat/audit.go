package openai_compat

import (
	"context"
	"encoding/json"
	"time"

	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/types"
)

// gatewayAuditPayloadMaxRunes bounds the opt-in query retention in audit
// details: audit_payload is a sampling/debug switch, not a transcript.
const gatewayAuditPayloadMaxRunes = 2000

// gatewayTurnAudit is the facts of one executed gateway turn. Raw request
// bodies are NOT persisted unless gateway.audit_payload=true (docs §6) —
// external clients routinely send code context in messages.
type gatewayTurnAudit struct {
	TenantID uint64
	// UserID is the API-key principal bound to the temporary session (may
	// be empty for platform-key callers without a bound user).
	UserID string
	// KeyID / KeyName identify the credential; the raw key is never
	// available here and never logged — the id is the stable fingerprint.
	KeyID   uint64
	KeyName string
	AgentID string
	// Model is the caller's raw model string, echoed for mismatch diagnosis.
	Model  string
	Stream bool
	Query  string
	// EndReason: completed | error | timeout | disconnect |
	// disconnect_run_completed.
	EndReason string
	// Duration is the handler-side wall time of the turn.
	Duration time.Duration
	Usage    *Usage
	// Warnings reports renderer/turn adjustments for the audit trail.
	Warnings []string
}

// auditGatewayTurn writes one gateway.completion audit row. Audit failures
// are logged but never fail the completion: the answer already left, and a
// broken audit sink must not turn a successful turn into a 500.
func (h *GatewayHandler) auditGatewayTurn(ctx context.Context, turn gatewayTurnAudit) {
	if h.audit == nil {
		return
	}
	details := types.JSONMap{
		"key_id":        turn.KeyID,
		"key_name":      turn.KeyName,
		"agent_id":      turn.AgentID,
		"model":         turn.Model,
		"stream":        turn.Stream,
		"end_reason":    turn.EndReason,
		"duration_ms":   turn.Duration.Milliseconds(),
		"gateway_proto": GatewayVersion,
	}
	if turn.Usage != nil {
		details["prompt_tokens"] = turn.Usage.PromptTokens
		details["completion_tokens"] = turn.Usage.CompletionTokens
		details["total_tokens"] = turn.Usage.TotalTokens
	}
	if len(turn.Warnings) > 0 {
		details["warnings"] = turn.Warnings
	}
	if h.gateway != nil && h.gateway.AuditPayload && turn.Query != "" {
		details["query"] = truncateRunes(turn.Query, gatewayAuditPayloadMaxRunes)
	}

	outcome := types.AuditOutcomeSuccess
	switch turn.EndReason {
	case "error":
		outcome = types.AuditOutcomeFailed
	case "timeout", "disconnect":
		outcome = types.AuditOutcomeCanceled
	}

	detailsRaw, _ := json.Marshal(details)
	entry := &types.AuditLog{
		TenantID:      turn.TenantID,
		ActorUserID:   turn.UserID,
		Action:        types.AuditActionGatewayCompletion,
		ScopeType:     "agent",
		ScopeID:       turn.AgentID,
		TargetType:    "gateway_model",
		TargetID:      turn.Model,
		RequestPath:   "/v1/chat/completions",
		RequestMethod: "POST",
		Outcome:       outcome,
		Details:       types.JSON(detailsRaw),
	}
	if err := h.audit.Log(ctx, entry); err != nil {
		logger.Warnf(ctx, "[gateway] audit log write failed: model=%s err=%v", turn.Model, err)
	}
}
