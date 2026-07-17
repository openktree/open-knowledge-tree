-- 0022_fact_references.down.sql

DROP TABLE IF EXISTS okt_repository.fact_references;

ALTER TABLE okt_repository.sources
    DROP COLUMN IF EXISTS sentence_offsets;