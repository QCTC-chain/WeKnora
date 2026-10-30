package repository

import (
	"context"
	"testing"
	"time"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func newGatewayUsageRepo(t *testing.T) (*gatewayUsageRepository, context.Context) {
	t.Helper()
	db, err := gorm.Open(sqlite.Open("file:"+t.Name()+"?mode=memory&cache=shared"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&types.GatewayUsage{}))
	return &gatewayUsageRepository{db: db}, context.Background()
}

func usageRow(tenantID, keyID uint64, agentID string, total int64, at time.Time) *types.GatewayUsage {
	return &types.GatewayUsage{
		TenantID: tenantID, KeyID: keyID, AgentID: agentID,
		Model: "Doc QA", TotalTokens: total,
		PromptTokens: total / 2, CompletionTokens: total - total/2,
		DurationMS: 120, EndReason: "completed", StatusCode: 200,
		CreatedAt: at,
	}
}

func TestGatewayUsageQuotaSumsTodayOnly(t *testing.T) {
	repo, ctx := newGatewayUsageRepo(t)
	now := time.Now()
	yesterday := now.Add(-24 * time.Hour)

	require.NoError(t, repo.Record(ctx, usageRow(1, 7, "agt-1", 100, yesterday)))
	tokens, requests, err := repo.QuotaUsed(ctx, 7, now)
	require.NoError(t, err)
	require.Zero(t, tokens)
	require.Zero(t, requests, "yesterday's usage must not count into today's budget")

	require.NoError(t, repo.Record(ctx, usageRow(1, 7, "agt-1", 300, now.Add(-time.Minute))))
	require.NoError(t, repo.Record(ctx, usageRow(1, 8, "agt-1", 999, now.Add(-time.Minute))))
	tokens, requests, err = repo.QuotaUsed(ctx, 7, now)
	require.NoError(t, err)
	require.EqualValues(t, 300, tokens, "only key 7's usage counts")
	require.EqualValues(t, 1, requests)
}

func TestGatewayUsageTenantQuota(t *testing.T) {
	repo, ctx := newGatewayUsageRepo(t)
	now := time.Now()
	require.NoError(t, repo.Record(ctx, usageRow(1, 7, "agt-1", 100, now)))
	require.NoError(t, repo.Record(ctx, usageRow(1, 8, "agt-2", 250, now)))
	require.NoError(t, repo.Record(ctx, usageRow(2, 9, "agt-1", 5000, now)))
	total, err := repo.TenantQuotaUsed(ctx, 1, now)
	require.NoError(t, err)
	require.EqualValues(t, 350, total)
}

func TestGatewayUsageDailyRollup(t *testing.T) {
	repo, ctx := newGatewayUsageRepo(t)
	today := time.Now().UTC().Truncate(24 * time.Hour).Add(12 * time.Hour)
	yesterday := today.Add(-24 * time.Hour)
	require.NoError(t, repo.Record(ctx, usageRow(1, 7, "agt-1", 100, yesterday)))
	require.NoError(t, repo.Record(ctx, usageRow(1, 7, "agt-1", 200, today)))
	require.NoError(t, repo.Record(ctx, usageRow(1, 8, "agt-2", 50, today)))

	rows, err := repo.DailyUsage(ctx, 1, today.Add(-48*time.Hour), today.Add(24*time.Hour))
	require.NoError(t, err)
	require.Len(t, rows, 3, "one row per (day, agent): yesterday/agt-1, today/agt-1, today/agt-2")

	byKey := map[string]types.GatewayUsageDay{}
	for _, r := range rows {
		byKey[r.Day+"/"+r.AgentID] = r
	}
	require.EqualValues(t, 100, byKey[yesterday.UTC().Format("2006-01-02")+"/agt-1"].TotalTokens)
	require.EqualValues(t, 200, byKey[today.UTC().Format("2006-01-02")+"/agt-1"].TotalTokens)
	require.EqualValues(t, 50, byKey[today.UTC().Format("2006-01-02")+"/agt-2"].TotalTokens)
	require.EqualValues(t, 1, byKey[today.UTC().Format("2006-01-02")+"/agt-2"].Requests)
}

func TestGatewayUsageRecordFillsCreatedAt(t *testing.T) {
	repo, ctx := newGatewayUsageRepo(t)
	entry := usageRow(1, 7, "agt-1", 10, time.Time{})
	require.NoError(t, repo.Record(ctx, entry))
	require.False(t, entry.CreatedAt.IsZero(), "Record fills CreatedAt when zero")
}
