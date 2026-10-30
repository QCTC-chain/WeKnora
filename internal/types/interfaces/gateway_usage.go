package interfaces

import (
	"context"
	"time"

	"github.com/Tencent/WeKnora/internal/types"
)

// GatewayUsageService meters model-gateway turns and answers the quota
// probes and daily rollups built on them (M2-b). Metering failures must
// never fail a turn — callers log-and-continue.
type GatewayUsageService interface {
	// Record appends one metering row. CreatedAt is filled when zero.
	Record(ctx context.Context, entry *types.GatewayUsage) error
	// QuotaUsed sums (total_tokens, requests) the key consumed since the
	// start of the given UTC day. Day boundaries are UTC to match the
	// documented "daily budget" semantics.
	QuotaUsed(ctx context.Context, keyID uint64, day time.Time) (tokens int64, requests int64, err error)
	// TenantQuotaUsed sums total tokens for the whole tenant since the
	// start of the given UTC day (tenant-level total gate).
	TenantQuotaUsed(ctx context.Context, tenantID uint64, day time.Time) (tokens int64, err error)
	// DailyUsage returns per-(UTC day, agent) rollups for the tenant in
	// [from, to). Rows are ordered by day then agent.
	DailyUsage(ctx context.Context, tenantID uint64, from, to time.Time) ([]types.GatewayUsageDay, error)
}
