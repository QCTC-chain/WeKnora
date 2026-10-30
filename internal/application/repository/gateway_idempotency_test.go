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

func newIdempotencyRepo(t *testing.T) (*gatewayIdempotencyRepository, context.Context) {
	t.Helper()
	db, err := gorm.Open(sqlite.Open("file:"+t.Name()+"?mode=memory&cache=shared"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&types.GatewayIdempotency{}))
	return &gatewayIdempotencyRepository{db: db}, context.Background()
}

func TestGatewayIdempotencyClaimCompleteReplay(t *testing.T) {
	repo, ctx := newIdempotencyRepo(t)

	// First caller claims.
	outcome, claim, err := repo.Claim(ctx, 42, 7, "retry-1", "fp-aaa")
	require.NoError(t, err)
	require.Equal(t, types.GatewayIdempotencyClaimed, outcome)
	require.NotZero(t, claim.ID)

	// Concurrent duplicate → in_flight, no execution rights.
	outcome, existing, err := repo.Claim(ctx, 42, 7, "retry-1", "fp-aaa")
	require.NoError(t, err)
	require.Equal(t, types.GatewayIdempotencyInFlight, outcome)
	require.Equal(t, claim.ID, existing.ID)

	// Complete → replay carries the stored response.
	require.NoError(t, repo.Complete(ctx, claim.ID, 200, "application/json", []byte(`{"ok":true}`), true))
	outcome, stored, err := repo.Claim(ctx, 42, 7, "retry-1", "fp-aaa")
	require.NoError(t, err)
	require.Equal(t, types.GatewayIdempotencyReplay, outcome)
	require.Equal(t, 200, stored.StatusCode)
	require.Equal(t, []byte(`{"ok":true}`), stored.ResponseBody)

	// Same key, different body → mismatch (never replayed).
	outcome, _, err = repo.Claim(ctx, 42, 7, "retry-1", "fp-bbb")
	require.NoError(t, err)
	require.Equal(t, types.GatewayIdempotencyMismatch, outcome)

	// Claims are per-key: another key claims the same client key cleanly.
	outcome, other, err := repo.Claim(ctx, 42, 8, "retry-1", "fp-aaa")
	require.NoError(t, err)
	require.Equal(t, types.GatewayIdempotencyClaimed, outcome)
	require.NotEqual(t, claim.ID, other.ID)
}

func TestGatewayIdempotencyStaleInFlightTakeover(t *testing.T) {
	repo, ctx := newIdempotencyRepo(t)
	outcome, claim, err := repo.Claim(ctx, 42, 7, "retry-2", "fp-1")
	require.NoError(t, err)
	require.Equal(t, types.GatewayIdempotencyClaimed, outcome)

	// Simulate a crashed writer: age the in-flight row past the takeover window.
	stale := time.Now().Add(-(GatewayIdempotencyTakeover + time.Minute))
	require.NoError(t, repo.db.Model(&types.GatewayIdempotency{}).
		Where("id = ?", claim.ID).
		Updates(map[string]interface{}{"updated_at": stale}).Error)

	// The retry takes the claim over and may execute.
	outcome, _, err = repo.Claim(ctx, 42, 7, "retry-2", "fp-1")
	require.NoError(t, err)
	require.Equal(t, types.GatewayIdempotencyClaimed, outcome, "stale in-flight claims are reassigned")

	// A fresh in-flight claim is NOT taken over.
	outcome, _, err = repo.Claim(ctx, 42, 7, "retry-3", "fp-2")
	require.NoError(t, err)
	require.Equal(t, types.GatewayIdempotencyClaimed, outcome)
	outcome, _, err = repo.Claim(ctx, 42, 7, "retry-3", "fp-2")
	require.NoError(t, err)
	require.Equal(t, types.GatewayIdempotencyInFlight, outcome)
}

func TestGatewayIdempotencyOversizedDropAndPrune(t *testing.T) {
	repo, ctx := newIdempotencyRepo(t)
	_, claim, err := repo.Claim(ctx, 42, 7, "retry-4", "fp-big")
	require.NoError(t, err)

	// Unreplayable (oversized) completion deletes the claim: retry re-executes.
	require.NoError(t, repo.Complete(ctx, claim.ID, 0, "", nil, false))
	outcome, reclamed, err := repo.Claim(ctx, 42, 7, "retry-4", "fp-big")
	require.NoError(t, err)
	require.Equal(t, types.GatewayIdempotencyClaimed, outcome, "dropped claims are re-claimable")

	// PruneExpired removes rows past the TTL.
	require.NoError(t, repo.Complete(ctx, reclamed.ID, 200, "application/json", []byte("x"), true))
	old := time.Now().Add(-(GatewayIdempotencyTTL + time.Minute))
	require.NoError(t, repo.db.Model(&types.GatewayIdempotency{}).
		Where("id = ?", reclamed.ID).
		Updates(map[string]interface{}{"created_at": old, "updated_at": old}).Error)
	removed, err := repo.PruneExpired(ctx, time.Now())
	require.NoError(t, err)
	require.EqualValues(t, 1, removed)
}

func TestGatewayIdempotencyRelease(t *testing.T) {
	repo, ctx := newIdempotencyRepo(t)
	_, claim, err := repo.Claim(ctx, 42, 7, "retry-5", "fp-5")
	require.NoError(t, err)
	require.NoError(t, repo.Release(ctx, claim.ID))
	outcome, _, err := repo.Claim(ctx, 42, 7, "retry-5", "fp-5")
	require.NoError(t, err)
	require.Equal(t, types.GatewayIdempotencyClaimed, outcome, "released claims are immediately re-claimable")
}
