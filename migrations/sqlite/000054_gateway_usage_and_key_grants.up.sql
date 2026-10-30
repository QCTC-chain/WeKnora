-- Migration: 000054_gateway_usage_and_key_grants (SQLite)
-- Per-key gateway grants + usage metering; see
-- migrations/versioned/000137_gateway_usage_and_key_grants.up.sql.
ALTER TABLE tenant_api_keys ADD COLUMN agent_grants TEXT NOT NULL DEFAULT '[]';
ALTER TABLE tenant_api_keys ADD COLUMN daily_token_budget INTEGER;
ALTER TABLE tenant_api_keys ADD COLUMN daily_request_budget INTEGER;
CREATE TABLE IF NOT EXISTS gateway_usage (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    tenant_id INTEGER NOT NULL,
    key_id INTEGER NOT NULL DEFAULT 0,
    agent_id VARCHAR(36) NOT NULL,
    session_id VARCHAR(36) NOT NULL DEFAULT '',
    model VARCHAR(255) NOT NULL DEFAULT '',
    stream BOOLEAN NOT NULL DEFAULT FALSE,
    prompt_tokens INTEGER NOT NULL DEFAULT 0,
    completion_tokens INTEGER NOT NULL DEFAULT 0,
    total_tokens INTEGER NOT NULL DEFAULT 0,
    duration_ms INTEGER NOT NULL DEFAULT 0,
    end_reason VARCHAR(32) NOT NULL DEFAULT '',
    status_code INTEGER NOT NULL DEFAULT 200,
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE INDEX IF NOT EXISTS idx_gateway_usage_tenant_created ON gateway_usage (tenant_id, created_at);
CREATE INDEX IF NOT EXISTS idx_gateway_usage_key_created ON gateway_usage (key_id, created_at);
CREATE INDEX IF NOT EXISTS idx_gateway_usage_agent_created ON gateway_usage (agent_id, created_at);
