-- 0030_drop_concept_slugs.up.sql
--
-- Drop the concept slug subsystem and re-key concept grouping onto
-- lower(canonical_name). Migration 0025 introduced a `slug` column
-- derived from canonical_name via a BEFORE INSERT/UPDATE trigger and
-- restricted canonical_name to a strict ASCII alphabet
-- (letters/digits/space/hyphen/apostrophe) via
-- chk_concepts_canonical_name_charset. Slug was injective over that
-- alphabet, so it was safe to use as a grouping key for the by-slug
-- detail endpoint and the concept_relations matview.
--
-- In practice the LLM concept/alias providers emit names that violate
-- the strict charset (accented letters like "Ana Obregón", punctuation
-- like "2,6-Diaminopurine", "J.P. Morgan"). A violated CHECK poisons the
-- write transaction, no fact_concepts/fact_concept_skips row is written,
-- and ListStableFactsForConceptExtraction re-returns the same facts on
-- the next batch — the extract_concepts worker spins forever on the same
-- facts, burning LLM calls until the 4h JobTimeout (root cause of the
-- stuck extract_concepts queue).
--
-- Fix: drop the slug column, trigger, function, and strict charset
-- entirely. Loosen the CHECK to "at least one ASCII letter or digit"
-- (rejects empty/whitespace/punctuation-only names but accepts any
-- real-world name). Re-key the concept_relations matview onto
-- lower(canonical_name) so relations still group by the concept's
-- identity, not a lossy rendering of it. The by-slug endpoint is
-- removed; concepts are addressed by their UUID (the existing
-- GetConcept handler resolves UUID -> canonical_name -> group via
-- ListConceptsByRepoName, which already groups by lower(canonical_name)).
--
-- Idempotent: every statement uses IF EXISTS / IF NOT EXISTS.

-- 1. Loosen the canonical_name charset. Drop the strict alphabet
--    whitelist; keep only the "at least one ASCII alnum" guarantee so
--    empty/whitespace/punctuation-only names are still rejected (the
--    slug trigger used to enforce non-empty via its own logic; now the
--    CHECK carries that responsibility alone).
ALTER TABLE okt_repository.concepts
    DROP CONSTRAINT IF EXISTS chk_concepts_canonical_name_charset;
ALTER TABLE okt_repository.concepts
    ADD CONSTRAINT chk_concepts_canonical_name_charset
    CHECK (canonical_name ~ '[A-Za-z0-9]');

-- 2. Drop the old slug-keyed concept_relations matview FIRST, before
--    dropping the slug column it depends on (Postgres refuses to
--    drop a column a matview references). The view is rebuilt in
--    step 4 keyed on lower(canonical_name).
DROP MATERIALIZED VIEW IF EXISTS okt_repository.concept_relations;

-- 3. Drop the slug subsystem: index, trigger, function, column.
DROP INDEX IF EXISTS okt_repository.idx_concepts_repo_slug;
DROP TRIGGER IF EXISTS trg_concepts_slug ON okt_repository.concepts;
DROP FUNCTION IF EXISTS okt_repository.concepts_slug();
ALTER TABLE okt_repository.concepts
    DROP COLUMN IF EXISTS slug;

-- 4. Rebuild the concept_relations materialized view keyed on
--    lower(canonical_name) instead of slug. Pairs are stored as
--    ordered (lower(name_a) < lower(name_b)) with self-pairs excluded,
--    so each unordered pair appears once per repo. The unique index
--    on (repository_id, name_a, name_b) enables REFRESH CONCURRENTLY.
--    WITH DATA populates the view from the existing fact_concepts rows
--    so relations are available immediately after migration.
CREATE MATERIALIZED VIEW okt_repository.concept_relations AS
SELECT
    c1.repository_id,
    lower(c1.canonical_name) AS name_a,
    lower(c2.canonical_name) AS name_b,
    COUNT(DISTINCT fc1.fact_id) AS shared_fact_count
FROM okt_repository.fact_concepts fc1
JOIN okt_repository.concepts       c1 ON c1.id = fc1.concept_id
JOIN okt_repository.fact_concepts  fc2 ON fc2.fact_id = fc1.fact_id
JOIN okt_repository.concepts       c2 ON c2.id = fc2.concept_id
WHERE c1.repository_id = c2.repository_id
  AND lower(c1.canonical_name) < lower(c2.canonical_name)
GROUP BY c1.repository_id, lower(c1.canonical_name), lower(c2.canonical_name)
WITH DATA;

CREATE UNIQUE INDEX uq_concept_relations_repo_pair
    ON okt_repository.concept_relations (repository_id, name_a, name_b);

CREATE INDEX idx_concept_relations_repo_a_count
    ON okt_repository.concept_relations (repository_id, name_a, shared_fact_count DESC);
CREATE INDEX idx_concept_relations_repo_b_count
    ON okt_repository.concept_relations (repository_id, name_b, shared_fact_count DESC);