DROP INDEX IF EXISTS idx_gateway_idempotency_tenant;
DROP INDEX IF EXISTS idx_gateway_idempotency_created_at;
DROP INDEX IF EXISTS uq_gateway_idempotency_claim;
DROP TABLE IF EXISTS gateway_idempotency;
