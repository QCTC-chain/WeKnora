-- Migration: 000135_agent_published
-- Adds the model-gateway publish flag to custom_agents. An agent only shows
-- up as a selectable "model" on the OpenAI-compatible gateway (GET /v1/models)
-- after an explicit publish; existing rows stay unpublished.
DO $$ BEGIN RAISE NOTICE '[Migration 000135] Adding custom_agents.published / published_at'; END $$;

ALTER TABLE custom_agents ADD COLUMN IF NOT EXISTS published BOOLEAN NOT NULL DEFAULT false;
ALTER TABLE custom_agents ADD COLUMN IF NOT EXISTS published_at TIMESTAMP WITH TIME ZONE;

-- /v1/models resolves a caller's tenant and lists published agents; the index
-- keeps that list cheap without narrowing tenant isolation (tenant_id stays
-- the leading filter in queries).
CREATE INDEX IF NOT EXISTS idx_custom_agents_published ON custom_agents(tenant_id, published);

COMMENT ON COLUMN custom_agents.published IS 'Callable as a model via the OpenAI-compatible gateway /v1';
COMMENT ON COLUMN custom_agents.published_at IS 'Last publish instant; NULL while unpublished';
