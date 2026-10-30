package types

import "time"

// GatewayUsage is the metering fact of one executed model-gateway turn
// (M2-b). One row per request that reached the turn pipeline — success,
// client error or upstream failure — so quotas and reports see the real
// request volume. Raw queries are never stored here; the audit log keeps
// the opt-in payload sampling.
type GatewayUsage struct {
	ID        uint64 `json:"id" gorm:"primaryKey;autoIncrement"`
	TenantID  uint64 `json:"tenant_id" gorm:"not null;index:idx_gateway_usage_tenant_created,priority:1"`
	KeyID     uint64 `json:"key_id" gorm:"not null;default:0;index:idx_gateway_usage_key_created,priority:1"`
	AgentID   string `json:"agent_id" gorm:"type:varchar(36);not null;index:idx_gateway_usage_agent_created,priority:1"`
	SessionID string `json:"session_id" gorm:"type:varchar(36);not null;default:''"`
	Model     string `json:"model" gorm:"type:varchar(255);not null;default:''"`
	Stream    bool   `json:"stream" gorm:"not null;default:false"`

	PromptTokens     int64 `json:"prompt_tokens" gorm:"not null;default:0"`
	CompletionTokens int64 `json:"completion_tokens" gorm:"not null;default:0"`
	TotalTokens      int64 `json:"total_tokens" gorm:"not null;default:0"`
	DurationMS       int64 `json:"duration_ms" gorm:"not null;default:0"`

	// EndReason mirrors the audit vocabulary:
	// completed | error | timeout | disconnect | disconnect_run_completed |
	// budget_exceeded | rejected.
	EndReason  string `json:"end_reason" gorm:"type:varchar(32);not null;default:''"`
	StatusCode int    `json:"status_code" gorm:"not null;default:200"`
	CreatedAt  time.Time `json:"created_at"`
}

// TableName pins the GORM table.
func (GatewayUsage) TableName() string { return "gateway_usage" }

// GatewayUsageDay is one daily rollup cell: per (day, agent) aggregates
// computed from gateway_usage for cost visibility and export.
type GatewayUsageDay struct {
	// Day is the UTC date in YYYY-MM-DD form.
	Day             string `json:"day"`
	AgentID         string `json:"agent_id"`
	Requests        int64  `json:"requests"`
	PromptTokens    int64  `json:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens"`
	TotalTokens     int64  `json:"total_tokens"`
	AvgDurationMS   int64  `json:"avg_duration_ms"`
}
