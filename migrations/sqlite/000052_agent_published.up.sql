-- Migration: 000052_agent_published (SQLite)
-- Model-gateway publish flag; see migrations/versioned/000135_agent_published.up.sql.
ALTER TABLE custom_agents ADD COLUMN published BOOLEAN NOT NULL DEFAULT 0;
ALTER TABLE custom_agents ADD COLUMN published_at DATETIME;
CREATE INDEX idx_custom_agents_published ON custom_agents(tenant_id, published);
