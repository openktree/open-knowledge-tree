-- Adds attribution columns to ai_usage so the dashboard can
-- break consumption down per repository, per source, and per
-- operation kind. The new columns are nullable so the migration
-- is back-compatible with existing rows and with the write path
-- until every call site threads the new fields through. The
-- `operation` column distinguishes chat / embedding / fact_extraction
-- so the dashboard can split consumption by operation without
-- inferring it from completion_tokens == 0.
ALTER TABLE okt_system.ai_usage
    ADD COLUMN IF NOT EXISTS repository_id UUID,
    ADD COLUMN IF NOT EXISTS source_id      UUID,
    ADD COLUMN IF NOT EXISTS operation      TEXT NOT NULL DEFAULT 'chat';

-- Index the new filter columns so the dashboard's WHERE clauses
-- stay index-backed as the table grows.
CREATE INDEX IF NOT EXISTS idx_ai_usage_repository_id ON okt_system.ai_usage(repository_id);
CREATE INDEX IF NOT EXISTS idx_ai_usage_source_id      ON okt_system.ai_usage(source_id);
CREATE INDEX IF NOT EXISTS idx_ai_usage_operation      ON okt_system.ai_usage(operation);