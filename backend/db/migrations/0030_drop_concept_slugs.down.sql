-- 0030_drop_concept_slugs.down.sql
--
-- Reverses 0030_drop_concept_slugs.up.sql. Restores the strict
-- canonical_name charset, the slug column + trigger + function, and
-- the slug-keyed concept_relations matview. Idempotent.
--
-- WARNING: any canonical_name rows that violate the strict charset
-- (accented/punctuated names inserted while 0030 was applied) will be
-- left in place but their slugs will be derived from the strict
-- slugify rule (accents collapse to '-', punctuation collapses to
-- '-'), so their slugs may collide with other rows. Rolling back is
-- only safe if you first delete or fix any non-strict canonical names.

-- 1. Re-add the slug column (NOT NULL DEFAULT '' until the trigger
--    populates it for every row).
ALTER TABLE okt_repository.concepts
    ADD COLUMN IF NOT EXISTS slug TEXT NOT NULL DEFAULT '';

-- 2. Restore the slug trigger function (strict slugify: lowercase,
--    strip apostrophes, collapse non-alnum to '-', trim '-').
CREATE OR REPLACE FUNCTION okt_repository.concepts_slug()
RETURNS TRIGGER AS $$
BEGIN
    NEW.slug := btrim(
        regexp_replace(
            regexp_replace(
                lower(NEW.canonical_name),
                '''', '', 'g'
            ),
            '[^a-z0-9]+', '-', 'g'
        ),
        '-'
    );
    RETURN NEW;
END;
$$ LANGUAGE plpgsql IMMUTABLE;

DROP TRIGGER IF EXISTS trg_concepts_slug ON okt_repository.concepts;
CREATE TRIGGER trg_concepts_slug
    BEFORE INSERT OR UPDATE OF canonical_name ON okt_repository.concepts
    FOR EACH ROW
    EXECUTE FUNCTION okt_repository.concepts_slug();

-- 3. Backfill slugs for existing rows by touching canonical_name so
--    the BEFORE UPDATE trigger fires. A no-op UPDATE on every row.
UPDATE okt_repository.concepts SET canonical_name = canonical_name WHERE true;

-- 4. Restore the strict canonical_name charset.
ALTER TABLE okt_repository.concepts
    DROP CONSTRAINT IF EXISTS chk_concepts_canonical_name_charset;
ALTER TABLE okt_repository.concepts
    ADD CONSTRAINT chk_concepts_canonical_name_charset
    CHECK (canonical_name ~ '^[A-Za-z0-9 ''-]+$'
           AND canonical_name ~ '[A-Za-z0-9]');

-- 5. Restore the slug indexes.
CREATE INDEX IF NOT EXISTS idx_concepts_repo_slug
    ON okt_repository.concepts (repository_id, slug);
CREATE INDEX IF NOT EXISTS idx_concepts_repo_name_lower
    ON okt_repository.concepts (repository_id, lower(canonical_name));

-- 6. Rebuild the concept_relations matview keyed on slug.
DROP MATERIALIZED VIEW IF EXISTS okt_repository.concept_relations;

CREATE MATERIALIZED VIEW okt_repository.concept_relations AS
SELECT
    c1.repository_id,
    c1.slug    AS slug_a,
    c2.slug    AS slug_b,
    COUNT(DISTINCT fc1.fact_id) AS shared_fact_count
FROM okt_repository.fact_concepts fc1
JOIN okt_repository.concepts       c1 ON c1.id = fc1.concept_id
JOIN okt_repository.fact_concepts  fc2 ON fc2.fact_id = fc1.fact_id
JOIN okt_repository.concepts       c2 ON c2.id = fc2.concept_id
WHERE c1.repository_id = c2.repository_id
  AND c1.slug < c2.slug
GROUP BY c1.repository_id, c1.slug, c2.slug
WITH DATA;

CREATE UNIQUE INDEX uq_concept_relations_repo_pair
    ON okt_repository.concept_relations (repository_id, slug_a, slug_b);

CREATE INDEX idx_concept_relations_repo_a_count
    ON okt_repository.concept_relations (repository_id, slug_a, shared_fact_count DESC);
CREATE INDEX idx_concept_relations_repo_b_count
    ON okt_repository.concept_relations (repository_id, slug_b, shared_fact_count DESC);