package openai_compat

import (
	"context"
	"encoding/json"
	"net/http"
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
	// SessionID is the temporary gateway session the turn ran in (usage
	// rows link to it; audit details omit it as the session is transient).
	SessionID string
	// Model is the caller's raw model string, echoed for mismatch diagnosis.
	Model string
	// Stream / Query / Warnings feed the audit details; Query retention is
	// gated by the audit_payload flag.
	Stream bool
	Query  string
	// Telemetry carries M2-6 observability (rounds + sampled event log);
	// only the rounds count lands unconditionally, the journal requires
	// the audit_payload flag.
	Telemetry *turnTelemetry
	// EndReason: completed | error | timeout | disconnect |
	// disconnect_run_completed.
	EndReason string
	// StatusCode is the HTTP status the gateway answers with (200 for a
	// successful turn, 500 for an error, etc.).
	StatusCode int
	// Duration is the handler-side wall time of the turn.
	Duration time.Duration
	Usage    *Usage
	// Warnings reports renderer/turn adjustments for the audit trail.
	Warnings []string
}

// auditGatewayTurn writes one gateway.completion audit row and the M2-b
// gateway_usage metering row. Audit/metering failures are logged but never
// fail the completion: the answer already left, and a broken sink must not
// turn a successful turn into a 500.
func (h *GatewayHandler) auditGatewayTurn(ctx context.Context, turn gatewayTurnAudit) {
	h.recordGatewayTurnUsage(ctx, turn.TenantID, turn, turn.StatusCode)
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
	if turn.SessionID != "" {
		details["session_id"] = turn.SessionID
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
		// M2-6 §6: sampled post-render event journal rides the same opt-in
		// flag, giving support a request-local event timeline.
		if turn.Telemetry != nil && len(turn.Telemetry.Events) > 0 {
			details["event_log"] = turn.Telemetry.Events
			if turn.Telemetry.Drops > 0 {
				details["event_drops"] = turn.Telemetry.Drops
			}
		}
	}
	if turn.Telemetry != nil && turn.Telemetry.Rounds > 0 {
		details["internal_rounds"] = turn.Telemetry.Rounds
	}

	outcome := types.AuditOutcomeSuccess
	switch turn.EndReason {
	case "error":
		outcome = types.AuditOutcomeFailed
	case "timeout", "disconnect":
		outcome = types.AuditOutcomeCanceled
	}
	if turn.StatusCode == 0 {
		turn.StatusCode = http.StatusOK
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
