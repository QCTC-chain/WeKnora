package types

import "time"

// Gateway idempotency (M2-4): one row per (key, Idempotency-Key) claim.
// Rows are the crash-safe record that lets a retried request replay the
// original response instead of re-executing a half-applied turn.
type GatewayIdempotency struct {
	ID int64 `json:"id"`
	// TenantID scopes the row to the caller's workspace.
	TenantID uint64 `json:"tenant_id"`
	// KeyID is the API key that claimed the idempotency key; claims are
	// per-key so two keys never collide on the same client-generated value.
	// uniqueIndex mirrors migration uq_gateway_idempotency_claim so
	// AutoMigrate test databases enforce the same constraint.
	KeyID uint64 `json:"key_id" gorm:"uniqueIndex:uq_gateway_idempotency_claim"`
	// IdempotencyKey is the client-supplied Idempotency-Key header value.
	IdempotencyKey string `json:"idempotency_key" gorm:"type:varchar(128);uniqueIndex:uq_gateway_idempotency_claim"`
	// Fingerprint is sha256 over the raw request body; a claim reused with a
	// different body is rejected instead of replayed.
	Fingerprint string `json:"fingerprint" gorm:"type:varchar(64)"`
	// Status is in-flight while the first request executes, done afterwards.
	Status string `json:"status" gorm:"type:varchar(16)"`
	// StatusCode/ContentType/ResponseBody replay the completed response.
	// ResponseBody is capped (gateway idempotency capture limit); oversized
	// responses delete the claim so a retry re-executes.
	StatusCode   int    `json:"status_code"`
	ContentType  string `json:"content_type" gorm:"type:varchar(128)"`
	ResponseBody []byte `json:"response_body"`
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// TableName keeps gorm's default pluralization explicit.
func (GatewayIdempotency) TableName() string { return "gateway_idempotency" }

// GatewayIdempotencyOutcome is the result of claiming an idempotency key.
type GatewayIdempotencyOutcome string

// Gateway idempotency row states (gateway_idempotency.status).
const (
	GatewayIdempotencyStatusInFlight = "in_flight"
	GatewayIdempotencyStatusDone     = "done"
)

// Claim outcomes.
const (
	// GatewayIdempotencyClaimed: this request owns the execution.
	GatewayIdempotencyClaimed GatewayIdempotencyOutcome = "claimed"
	// GatewayIdempotencyReplay: an identical completed request exists; replay.
	GatewayIdempotencyReplay GatewayIdempotencyOutcome = "replay"
	// GatewayIdempotencyInFlight: the same claim is still executing.
	GatewayIdempotencyInFlight GatewayIdempotencyOutcome = "in_flight"
	// GatewayIdempotencyMismatch: same key, different fingerprint.
	GatewayIdempotencyMismatch GatewayIdempotencyOutcome = "mismatch"
)
