-- 0048_decomposition_promptset_tag.up.sql
--
-- Tags every decomposition artifact with the promptset hash that
-- produced it, so downstream queries (synthesis, summarization,
-- registry pull/push, search-by-concept) can filter to a single
-- philosophy and decompositions from different promptsets never mix.
--
-- The columns are NULLable with no DEFAULT: NULL means "this row
-- predates the promptset feature" and is interpreted by app code as
-- the built-in hash (promptset.DefaultHash). This avoids baking a
-- literal hash into the migration (the built-in hash is computed at
-- app init from the current prompt constants, so it changes whenever
-- a prompt is edited — a literal here would silently go stale). The
-- backfill is done at app boot by the promptset resolver, not by SQL.
--
-- Indexes on promptset_hash support the downstream filter joins
-- (WHERE promptset_hash = $active) that enforce isolation. The
-- fact_concepts junction is tagged too because a concept derived
-- under promptset A must not be linked to a fact derived under
-- promptset B by a query that joins on the junction.
--
-- All tables live in okt_repository (per-repo data). ALTER TABLE is
-- unqualified to match the 0013 / 0023 pattern (sqlc's parser
-- resolves via the runtime search_path).

ALTER TABLE okt_repository.facts
    ADD COLUMN IF NOT EXISTS promptset_hash TEXT;

ALTER TABLE okt_repository.concepts
    ADD COLUMN IF NOT EXISTS promptset_hash TEXT;

ALTER TABLE okt_repository.fact_concepts
    ADD COLUMN IF NOT EXISTS promptset_hash TEXT;

ALTER TABLE okt_repository.fact_references
    ADD COLUMN IF NOT EXISTS promptset_hash TEXT;

CREATE INDEX IF NOT EXISTS idx_facts_promptset       ON okt_repository.facts(promptset_hash);
CREATE INDEX IF NOT EXISTS idx_concepts_promptset    ON okt_repository.concepts(promptset_hash);
CREATE INDEX IF NOT EXISTS idx_fact_concepts_promptset ON okt_repository.fact_concepts(promptset_hash);
CREATE INDEX IF NOT EXISTS idx_fact_references_promptset ON okt_repository.fact_references(promptset_hash);