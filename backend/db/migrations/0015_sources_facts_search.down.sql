-- 0014_sources_facts_search.down.sql
--
-- Drops the generated tsvector columns and their GIN indexes.
-- Dropping the column cascades to the index, but we drop the
-- index explicitly first so a partial-failure re-run is clean.

DROP INDEX IF EXISTS okt_repository.idx_sources_search_tsv;
DROP INDEX IF EXISTS okt_repository.idx_facts_search_tsv;

ALTER TABLE okt_repository.sources
    DROP COLUMN IF EXISTS search_tsv;

ALTER TABLE okt_repository.facts
    DROP COLUMN IF EXISTS search_tsv;