-- 0058_concepts_search_tsv.down.sql
--
-- Drops the GIN expression indexes added by 0058.

DROP INDEX IF EXISTS okt_repository.idx_concept_aliases_search_tsv;
DROP INDEX IF EXISTS okt_repository.idx_concepts_search_tsv;