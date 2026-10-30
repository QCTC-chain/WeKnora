-- Migration: 000136_agent_publish_governance
-- M2-a publish governance for the model gateway (docs/design/agent-model-gateway-m2-m3.md §1):
--   published_scope   private | org | tenant  — who may call the published agent
--   published_version monotonic counter bumped on every publish; /v1/models meta.version
--   published_config  config snapshot frozen at publish time; edits after publish do
--                     not change the gateway face until the next publish
--   deprecation_sunset set while the agent is in the "deprecated" transition state:
--                     still callable, responses carry X-WeKnora-Deprecation; once the
--                     instant passes the agent resolves as unpublished (lazy check).
DO $$ BEGIN RAISE NOTICE '[Migration 000136] Adding custom_agents publish governance columns'; END $$;

ALTER TABLE custom_agents ADD COLUMN IF NOT EXISTS published_scope VARCHAR(16) NOT NULL DEFAULT 'tenant';
ALTER TABLE custom_agents ADD COLUMN IF NOT EXISTS published_version BIGINT NOT NULL DEFAULT 0;
ALTER TABLE custom_agents ADD COLUMN IF NOT EXISTS published_config JSON;
ALTER TABLE custom_agents ADD COLUMN IF NOT EXISTS deprecation_sunset TIMESTAMP WITH TIME ZONE;

-- /v1/models filters (tenant_id, published) as before; the scope widens only
-- through the org-share join, so no extra index is needed for M2-a sizes.

COMMENT ON COLUMN custom_agents.published_scope IS 'Gateway visibility: private (creator only), org (plus tenants sharing an org where the agent is published), tenant (whole workspace; default)';
COMMENT ON COLUMN custom_agents.published_version IS 'Publish snapshot version; bumped each publish, exposed as /v1/models meta.version';
COMMENT ON COLUMN custom_agents.published_config IS 'Config frozen at publish time; the gateway executes this snapshot, not the live row';
COMMENT ON COLUMN custom_agents.deprecation_sunset IS 'Deprecated transition: callable until this instant, then resolves as unpublished';
