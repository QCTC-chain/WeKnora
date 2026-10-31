DROP INDEX IF EXISTS idx_custom_agents_published;
ALTER TABLE custom_agents DROP COLUMN published_at;
ALTER TABLE custom_agents DROP COLUMN published;
