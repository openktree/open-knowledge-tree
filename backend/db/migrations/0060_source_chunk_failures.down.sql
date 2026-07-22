-- 0060_source_chunk_failures.down.sql
DROP INDEX IF EXISTS okt_repository.idx_sources_concept_skip_count;
DROP INDEX IF EXISTS okt_repository.idx_sources_chunk_failures;
ALTER TABLE okt_repository.sources
    DROP COLUMN IF EXISTS concept_skip_count,
    DROP COLUMN IF EXISTS last_chunk_failure_at,
    DROP COLUMN IF EXISTS chunk_errors,
    DROP COLUMN IF EXISTS chunk_failures;