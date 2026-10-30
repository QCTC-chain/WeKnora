package repository

// M2-4 gateway idempotency store. Claims are per-(key, Idempotency-Key) rows
// with a two-state lifecycle (in_flight → done). Crash safety: an in-flight
// claim older than the takeover window is reassigned to a retrying caller.

import (
	"context"
	"errors"
	"time"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// Idempotency claim windows.
const (
	// GatewayIdempotencyTTL bounds how long a completed response stays
	// replayable (design §4: 默认 24h).
	GatewayIdempotencyTTL = 24 * time.Hour
	// GatewayIdempotencyTakeover bounds how long an in-flight claim may hold
	// the key before a retry may take it over (crashed writer detection).
	GatewayIdempotencyTakeover = 10 * time.Minute
)

type gatewayIdempotencyRepository struct {
	db *gorm.DB
}

// NewGatewayIdempotencyRepository wires the M2-4 idempotency store.
func NewGatewayIdempotencyRepository(db *gorm.DB) interfaces.GatewayIdempotencyStore {
	return &gatewayIdempotencyRepository{db: db}
}

// Claim tries to register (keyID, idempotencyKey) for this request's
// fingerprint. Outcome semantics:
//   - claimed: caller executes the turn;
//   - replay: an identical request completed within the TTL — row carries it;
//   - in_flight: the same claim is executing elsewhere right now;
//   - mismatch: the key was claimed with a different fingerprint.
func (r *gatewayIdempotencyRepository) Claim(
	ctx context.Context, tenantID, keyID uint64, idempotencyKey, fingerprint string,
) (types.GatewayIdempotencyOutcome, *types.GatewayIdempotency, error) {
	now := time.Now()
	claim := &types.GatewayIdempotency{
		TenantID: tenantID, KeyID: keyID,
		IdempotencyKey: idempotencyKey, Fingerprint: fingerprint,
		Status: types.GatewayIdempotencyStatusInFlight, CreatedAt: now, UpdatedAt: now,
	}
	res := r.db.WithContext(ctx).Clauses(clause.OnConflict{DoNothing: true}).Create(claim)
	if res.Error != nil {
		return "", nil, res.Error
	}
	if res.RowsAffected == 1 {
		return types.GatewayIdempotencyClaimed, claim, nil
	}

	// Lost the insert race: classify the existing row.
	existing, err := r.get(ctx, keyID, idempotencyKey)
	if err != nil {
		return "", nil, err
	}
	if existing == nil {
		// Vanished between insert and select (prune race): retry once.
		return r.Claim(ctx, tenantID, keyID, idempotencyKey, fingerprint)
	}
	if existing.Fingerprint != fingerprint {
		return types.GatewayIdempotencyMismatch, existing, nil
	}
	switch {
	case existing.Status == types.GatewayIdempotencyStatusDone && now.Sub(existing.CreatedAt) <= GatewayIdempotencyTTL:
		return types.GatewayIdempotencyReplay, existing, nil
	case existing.Status == types.GatewayIdempotencyStatusInFlight && now.Sub(existing.UpdatedAt) <= GatewayIdempotencyTakeover:
		return types.GatewayIdempotencyStatusInFlight, existing, nil
	}

	// Stale claim (done past TTL, or an in-flight writer that died): take it
	// over; the conditional UPDATE keeps two concurrent takeovers safe.
	takeover := r.db.WithContext(ctx).Model(&types.GatewayIdempotency{}).
		Where("key_id = ? AND idempotency_key = ? AND (status = ? OR updated_at < ?)",
			keyID, idempotencyKey, types.GatewayIdempotencyStatusDone, now.Add(-GatewayIdempotencyTakeover)).
		Updates(map[string]interface{}{
			"fingerprint": fingerprint,
			"status":      types.GatewayIdempotencyStatusInFlight,
			"updated_at":  now,
		})
	if takeover.Error != nil {
		return "", nil, takeover.Error
	}
	if takeover.RowsAffected == 1 {
		return types.GatewayIdempotencyClaimed, existing, nil
	}
	// Another caller took it over between select and update.
	return types.GatewayIdempotencyStatusInFlight, existing, nil
}

// Complete stores the finished response and flips the claim to done.
// With replayable=false (oversized capture) the claim is deleted so a retry
// re-executes instead of replaying a truncated body.
func (r *gatewayIdempotencyRepository) Complete(
	ctx context.Context, claimID int64, statusCode int, contentType string, body []byte, replayable bool,
) error {
	if !replayable {
		return r.db.WithContext(ctx).
			Where("id = ?", claimID).
			Delete(&types.GatewayIdempotency{}).Error
	}
	return r.db.WithContext(ctx).Model(&types.GatewayIdempotency{}).
		Where("id = ?", claimID).
		Updates(map[string]interface{}{
			"status":        types.GatewayIdempotencyStatusDone,
			"status_code":   statusCode,
			"content_type":  contentType,
			"response_body": body,
			"updated_at":    time.Now(),
		}).Error
}

// Release drops an in-flight claim after an early failure, so an immediate
// client retry re-executes instead of waiting out the takeover window.
func (r *gatewayIdempotencyRepository) Release(ctx context.Context, claimID int64) error {
	return r.db.WithContext(ctx).
		Where("id = ? AND status = ?", claimID, types.GatewayIdempotencyStatusInFlight).
		Delete(&types.GatewayIdempotency{}).Error
}

// PruneExpired removes finished claims past the TTL and dead in-flight rows.
// Called by the usage maintenance loop; returns the number of rows removed.
func (r *gatewayIdempotencyRepository) PruneExpired(ctx context.Context, now time.Time) (int64, error) {
	res := r.db.WithContext(ctx).
		Where("created_at < ?", now.Add(-GatewayIdempotencyTTL)).
		Delete(&types.GatewayIdempotency{})
	return res.RowsAffected, res.Error
}

func (r *gatewayIdempotencyRepository) get(ctx context.Context, keyID uint64, idempotencyKey string) (*types.GatewayIdempotency, error) {
	var row types.GatewayIdempotency
	err := r.db.WithContext(ctx).
		Where("key_id = ? AND idempotency_key = ?", keyID, idempotencyKey).
		First(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &row, nil
}
