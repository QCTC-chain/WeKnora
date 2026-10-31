-- M2-4 gateway idempotency: (key, Idempotency-Key) claims that let retried
-- requests replay the original response instead of re-executing a turn.
CREATE TABLE IF NOT EXISTS gateway_idempotency (
    id              BIGSERIAL PRIMARY KEY,
    tenant_id       BIGINT       NOT NULL,
    key_id          BIGINT       NOT NULL,
    idempotency_key VARCHAR(128) NOT NULL,
    fingerprint     VARCHAR(64)  NOT NULL,
    status          VARCHAR(16)  NOT NULL DEFAULT 'in_flight',
    status_code     INT          NOT NULL DEFAULT 0,
    content_type    VARCHAR(128) NOT NULL DEFAULT '',
    response_body   BYTEA,
    created_at      TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    updated_at      TIMESTAMPTZ  NOT NULL DEFAULT NOW()
);

-- The claim lookup is always (key_id, idempotency_key).
CREATE UNIQUE INDEX IF NOT EXISTS uq_gateway_idempotency_claim
    ON gateway_idempotency (key_id, idempotency_key);
-- Cleanup sweep index (TTL prune).
CREATE INDEX IF NOT EXISTS idx_gateway_idempotency_created_at
    ON gateway_idempotency (created_at);
CREATE INDEX IF NOT EXISTS idx_gateway_idempotency_tenant
    ON gateway_idempotency (tenant_id);
