-- 0018_sources_fetch_attempts.down.sql
--
-- Reverses 0018_sources_fetch_attempts.up.sql. Drops the
-- fetch_attempts column from okt_repository.sources. Safe
-- to run multiple times (DROP COLUMN IF EXISTS).

ALTER TABLE okt_repository.sources
    DROP COLUMN IF EXISTS fetch_attempts;