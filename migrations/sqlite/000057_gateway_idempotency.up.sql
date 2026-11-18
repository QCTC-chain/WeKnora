-- M2-4 gateway idempotency (SQLite baseline).
CREATE TABLE IF NOT EXISTS gateway_idempotency (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    tenant_id       INTEGER      NOT NULL,
    key_id          INTEGER      NOT NULL,
    idempotency_key TEXT         NOT NULL,
    fingerprint     TEXT         NOT NULL,
    status          TEXT         NOT NULL DEFAULT 'in_flight',
    status_code     INTEGER      NOT NULL DEFAULT 0,
    content_type    TEXT         NOT NULL DEFAULT '',
    response_body   BLOB,
    created_at      DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at      DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE UNIQUE INDEX IF NOT EXISTS uq_gateway_idempotency_claim
    ON gateway_idempotency (key_id, idempotency_key);
CREATE INDEX IF NOT EXISTS idx_gateway_idempotency_created_at
    ON gateway_idempotency (created_at);
CREATE INDEX IF NOT EXISTS idx_gateway_idempotency_tenant
    ON gateway_idempotency (tenant_id);
