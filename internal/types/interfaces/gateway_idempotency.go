package interfaces

import (
	"context"
	"time"

	"github.com/Tencent/WeKnora/internal/types"
)

// GatewayIdempotencyStore is the M2-4 claim ledger for retried gateway
// requests (per-(key, Idempotency-Key), TTL-bounded response replay).
type GatewayIdempotencyStore interface {
	// Claim registers or classifies a (keyID, idempotencyKey) claim.
	// Outcomes: claimed (caller executes), replay (row carries a completed
	// identical response), in_flight (same claim executing elsewhere),
	// mismatch (same key, different fingerprint).
	Claim(
		ctx context.Context, tenantID, keyID uint64, idempotencyKey, fingerprint string,
	) (types.GatewayIdempotencyOutcome, *types.GatewayIdempotency, error)
	// Complete stores the finished response; replayable=false drops the
	// claim (oversized capture) so a retry re-executes.
	Complete(ctx context.Context, claimID int64, statusCode int, contentType string, body []byte, replayable bool) error
	// Release drops an in-flight claim after an early failure.
	Release(ctx context.Context, claimID int64) error
	// PruneExpired removes claims past their TTL.
	PruneExpired(ctx context.Context, now time.Time) (int64, error)
}
