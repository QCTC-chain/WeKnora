package repository

import (
	"context"
	"time"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	"gorm.io/gorm"
)

// gatewayUsageRepository persists one row per executed gateway turn and
// computes the quota probes / daily rollups on top (M2-b). Append-only,
// like the audit log.
type gatewayUsageRepository struct {
	db *gorm.DB
}

// NewGatewayUsageRepository constructs the production gateway usage
// repository backed by the shared GORM connection.
func NewGatewayUsageRepository(db *gorm.DB) interfaces.GatewayUsageService {
	return &gatewayUsageRepository{db: db}
}

// Record inserts one metering row.
func (r *gatewayUsageRepository) Record(ctx context.Context, entry *types.GatewayUsage) error {
	if entry.CreatedAt.IsZero() {
		entry.CreatedAt = time.Now()
	}
	return r.db.WithContext(ctx).Create(entry).Error
}

// dayStartUTC truncates to the UTC midnight of t.
func dayStartUTC(t time.Time) time.Time {
	u := t.UTC()
	return time.Date(u.Year(), u.Month(), u.Day(), 0, 0, 0, 0, time.UTC)
}

// QuotaUsed sums the key's total tokens and request count since the start
// of day's UTC day. A missing table (fresh deploys before migration) or a
// transient error surfaces to the caller, which must fail open.
func (r *gatewayUsageRepository) QuotaUsed(
	ctx context.Context, keyID uint64, day time.Time,
) (int64, int64, error) {
	start := dayStartUTC(day)
	var row struct {
		Tokens   int64
		Requests int64
	}
	err := r.db.WithContext(ctx).
		Model(&types.GatewayUsage{}).
		Select("COALESCE(SUM(total_tokens),0) AS tokens, COUNT(*) AS requests").
		Where("key_id = ? AND created_at >= ?", keyID, start).
		Scan(&row).Error
	return row.Tokens, row.Requests, err
}

// TenantQuotaUsed sums the tenant's total tokens since the start of day's
// UTC day (tenant-level total gate).
func (r *gatewayUsageRepository) TenantQuotaUsed(
	ctx context.Context, tenantID uint64, day time.Time,
) (int64, error) {
	start := dayStartUTC(day)
	var total int64
	err := r.db.WithContext(ctx).
		Model(&types.GatewayUsage{}).
		Select("COALESCE(SUM(total_tokens),0)").
		Where("tenant_id = ? AND created_at >= ?", tenantID, start).
		Scan(&total).Error
	return total, err
}

// DailyUsage aggregates per (UTC day, agent) for the tenant window
// [from, to), ordered by day then agent. Key-level splits are available
// through the same table; the management UI starts with agent dimension
// because amplification (internal vs visible tokens) is agent-shaped.
func (r *gatewayUsageRepository) DailyUsage(
	ctx context.Context, tenantID uint64, from, to time.Time,
) ([]types.GatewayUsageDay, error) {
	// The day column is spelled per dialect: postgres TO_CHAR vs sqlite
	// strftime (same pattern as chunk_image_assets.go).
	dayExpr := "TO_CHAR(created_at, 'YYYY-MM-DD')"
	if r.db.Dialector.Name() == "sqlite" {
		dayExpr = "strftime('%Y-%m-%d', created_at)"
	}
	var rows []types.GatewayUsageDay
	err := r.db.WithContext(ctx).
		Model(&types.GatewayUsage{}).
		Select(
			dayExpr+" AS day, "+
				"agent_id AS agent_id, "+
				"COUNT(*) AS requests, "+
				"COALESCE(SUM(prompt_tokens),0) AS prompt_tokens, "+
				"COALESCE(SUM(completion_tokens),0) AS completion_tokens, "+
				"COALESCE(SUM(total_tokens),0) AS total_tokens, "+
				"COALESCE(AVG(duration_ms),0) AS avg_duration_ms",
		).
		Where("tenant_id = ? AND created_at >= ? AND created_at < ?", tenantID, from.UTC(), to.UTC()).
		Group("day, agent_id").
		Order("day ASC, agent_id ASC").
		Scan(&rows).Error
	return rows, err
}
