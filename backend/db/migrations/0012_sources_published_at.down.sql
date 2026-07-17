-- 0012_sources_published_at.down.sql
--
-- Reverse 0012_sources_published_at.up.sql. DROP INDEX IF
-- EXISTS keeps the down migration idempotent so re-running
-- it against a partially-applied schema is a no-op rather
-- than a failure.
DROP INDEX IF EXISTS okt_repository.idx_sources_repo_published_at;
DROP INDEX IF EXISTS okt_repository.idx_sources_published_at;

ALTER TABLE okt_repository.sources
    DROP COLUMN IF EXISTS published_at;
