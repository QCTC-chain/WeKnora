-- Migration: 000053_agent_publish_governance (SQLite)
-- Model-gateway publish governance; see migrations/versioned/000136_agent_publish_governance.up.sql.
ALTER TABLE custom_agents ADD COLUMN published_scope VARCHAR(16) NOT NULL DEFAULT 'tenant';
ALTER TABLE custom_agents ADD COLUMN published_version INTEGER NOT NULL DEFAULT 0;
ALTER TABLE custom_agents ADD COLUMN published_config TEXT;
ALTER TABLE custom_agents ADD COLUMN deprecation_sunset DATETIME;
