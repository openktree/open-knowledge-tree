-- 0006_sources_doi.down.sql

ALTER TABLE okt_repository.sources
    DROP COLUMN IF EXISTS doi;
