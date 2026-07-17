-- 0005_sources_content.down.sql

ALTER TABLE okt_repository.sources
    DROP COLUMN IF EXISTS error,
    DROP COLUMN IF EXISTS fetched_at,
    DROP COLUMN IF EXISTS content;
