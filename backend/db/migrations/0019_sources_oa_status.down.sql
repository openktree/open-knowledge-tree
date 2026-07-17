-- 0019_sources_oa_status.down.sql
ALTER TABLE okt_repository.sources
    DROP COLUMN IF EXISTS oa_status;