DROP INDEX IF EXISTS okt_system.idx_ai_usage_operation;
DROP INDEX IF EXISTS okt_system.idx_ai_usage_source_id;
DROP INDEX IF EXISTS okt_system.idx_ai_usage_repository_id;
ALTER TABLE okt_system.ai_usage
    DROP COLUMN IF EXISTS operation,
    DROP COLUMN IF EXISTS repository_id,
    DROP COLUMN IF EXISTS source_id;