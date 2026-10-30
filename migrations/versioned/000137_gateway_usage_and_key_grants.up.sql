-- Migration: 000137_gateway_usage_and_key_grants
-- M2-b per-key authorization + usage metering for the model gateway
-- (docs/design/agent-model-gateway-m2-m3.md §2):
--   tenant_api_keys.agent_grants          empty = all published agents callable;
--                                         non-empty = whitelist of agent UUIDs
--   tenant_api_keys.daily_*_budget        per-key daily quotas; NULL = unlimited
--   gateway_usage                         one row per executed gateway turn,
--                                         the base fact for daily rollups,
--                                         quotas and cost visibility

DO $$ BEGIN RAISE NOTICE '[Migration 000137] Adding gateway key grants and usage metering'; END $$;

ALTER TABLE tenant_api_keys ADD COLUMN IF NOT EXISTS agent_grants JSON NOT NULL DEFAULT '[]';
ALTER TABLE tenant_api_keys ADD COLUMN IF NOT EXISTS daily_token_budget BIGINT;
ALTER TABLE tenant_api_keys ADD COLUMN IF NOT EXISTS daily_request_budget BIGINT;

CREATE TABLE IF NOT EXISTS gateway_usage (
    id BIGSERIAL PRIMARY KEY,
    tenant_id BIGINT NOT NULL,
    key_id BIGINT NOT NULL DEFAULT 0,
    agent_id VARCHAR(36) NOT NULL,
    session_id VARCHAR(36) NOT NULL DEFAULT '',
    model VARCHAR(255) NOT NULL DEFAULT '',
    stream BOOLEAN NOT NULL DEFAULT FALSE,
    prompt_tokens BIGINT NOT NULL DEFAULT 0,
    completion_tokens BIGINT NOT NULL DEFAULT 0,
    total_tokens BIGINT NOT NULL DEFAULT 0,
    duration_ms BIGINT NOT NULL DEFAULT 0,
    end_reason VARCHAR(32) NOT NULL DEFAULT '',
    status_code INT NOT NULL DEFAULT 200,
    created_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_gateway_usage_tenant_created
    ON gateway_usage (tenant_id, created_at);
CREATE INDEX IF NOT EXISTS idx_gateway_usage_key_created
    ON gateway_usage (key_id, created_at);
CREATE INDEX IF NOT EXISTS idx_gateway_usage_agent_created
    ON gateway_usage (agent_id, created_at);

COMMENT ON COLUMN tenant_api_keys.agent_grants IS 'Gateway agent whitelist (agent UUIDs); empty = all published agents of reachable scopes';
COMMENT ON COLUMN tenant_api_keys.daily_token_budget IS 'Gateway daily total-token budget for this key; NULL = unlimited';
COMMENT ON COLUMN tenant_api_keys.daily_request_budget IS 'Gateway daily request budget for this key; NULL = unlimited';
COMMENT ON TABLE gateway_usage IS 'One row per executed model-gateway turn (M2-b metering)';
