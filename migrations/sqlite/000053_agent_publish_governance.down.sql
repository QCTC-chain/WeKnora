-- Migration: 000053_agent_publish_governance (SQLite, down)
ALTER TABLE custom_agents DROP COLUMN deprecation_sunset;
ALTER TABLE custom_agents DROP COLUMN published_config;
ALTER TABLE custom_agents DROP COLUMN published_version;
ALTER TABLE custom_agents DROP COLUMN published_scope;
