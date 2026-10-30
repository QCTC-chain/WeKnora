-- Migration: 000137_gateway_usage_and_key_grants (down)
DROP INDEX IF EXISTS idx_gateway_usage_agent_created;
DROP INDEX IF EXISTS idx_gateway_usage_key_created;
DROP INDEX IF EXISTS idx_gateway_usage_tenant_created;
DROP TABLE IF EXISTS gateway_usage;
ALTER TABLE tenant_api_keys DROP COLUMN IF EXISTS agent_grants;
ALTER TABLE tenant_api_keys DROP COLUMN IF EXISTS daily_token_budget;
ALTER TABLE tenant_api_keys DROP COLUMN IF EXISTS daily_request_budget;
