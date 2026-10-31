-- Migration: 000136_agent_publish_governance (down)
ALTER TABLE custom_agents DROP COLUMN IF EXISTS deprecation_sunset;
ALTER TABLE custom_agents DROP COLUMN IF EXISTS published_config;
ALTER TABLE custom_agents DROP COLUMN IF EXISTS published_version;
ALTER TABLE custom_agents DROP COLUMN IF EXISTS published_scope;
