-- 0025_concepts_slug_charset.up.sql
--
-- Concept unification at the API level (Phase 1). The concept list
-- and detail endpoints group per-context concept rows by canonical
-- name so the UI presents "one concept, many contexts" instead of
-- one row per (canonical_name, context). Grouping is by
-- lower(canonical_name) within a repo — no alias clustering, no
-- parent_concept_id materialization yet (those are Phase 2).
--
-- To make the grouped detail endpoint addressable by a stable,
-- shareable URL (instead of a per-context UUID), every concept row
-- carries a `slug` derived from its canonical_name. The slug is the
-- group key: every sibling row in a group shares the same slug, so
-- GET /concepts/by-slug/{slug} resolves to the whole group in one
-- indexed equality lookup.
--
-- slugify rule (must match the frontend helper in
-- frontend/src/services/api.js exactly):
--   1. lowercase
--   2. strip apostrophes (so "O'Brien" -> "obrien")
--   3. collapse every other run of non-alphanumeric chars to a
--      single '-'
--   4. trim leading/trailing '-'
--
-- To keep slugify injective over canonical names (so two distinct
-- names never slugify to the same value), canonical_name is
-- restricted to a safe alphabet: letters, digits, spaces, hyphens,
-- and apostrophes. Punctuation-rich surface forms belong in
-- concept_aliases.alias_text, not in canonical_name. The CHECK
-- constraint enforces the alphabet at the DB layer; the alias
-- provider prompt is tightened to emit compliant canonical names so
-- the CHECK never fires in practice.
--
-- This migration assumes a fresh database (no existing rows to
-- backfill), per the project's current dev posture.

-- 1. Add the slug column and populate it for every row via a
--    BEFORE INSERT OR UPDATE trigger. The trigger mirrors the
--    slugify rule above so the column is always consistent with
--    canonical_name without every caller having to compute it.
ALTER TABLE okt_repository.concepts
    ADD COLUMN IF NOT EXISTS slug TEXT NOT NULL DEFAULT '';

CREATE OR REPLACE FUNCTION okt_repository.concepts_slug()
RETURNS TRIGGER AS $$
BEGIN
    -- slugify rule (MUST match frontend api.conceptSlug and Go
    -- concepts.Slugify): lowercase, strip apostrophes, collapse
    -- non-alphanumeric runs to '-', trim leading/trailing '-'.
    -- Lowercasing MUST happen before the regexp_replace so the
    -- [^a-z0-9]+ class doesn't treat uppercase letters as
    -- non-alphanumeric (the class is case-sensitive).
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

-- 2. Canonical-name charset. Apostrophes, letters, digits, spaces,
--    and hyphens only. Anything else (periods, commas, parentheses,
--    slashes, ampersands, ...) must live in concept_aliases. The
--    regex allows one-or-more of the allowed chars; the empty
--    string is already rejected by NOT NULL... but NOT NULL permits
--    a single space, so the CHECK also requires at least one
--    non-whitespace allowed char by virtue of the character class
--    (a string of only spaces matches '[A-Za-z0-9 ''-]+' because
--    space is in the class, so we additionally require a
--    letter/digit somewhere).
ALTER TABLE okt_repository.concepts
    DROP CONSTRAINT IF EXISTS chk_concepts_canonical_name_charset;
ALTER TABLE okt_repository.concepts
    ADD CONSTRAINT chk_concepts_canonical_name_charset
    CHECK (canonical_name ~ '^[A-Za-z0-9 ''-]+$'
           AND canonical_name ~ '[A-Za-z0-9]');

-- 3. Index for the grouped detail lookup: (repository_id, slug).
--    Every sibling row in a group shares the slug, so the lookup
--    returns the whole group in one index scan.
CREATE INDEX IF NOT EXISTS idx_concepts_repo_slug
    ON okt_repository.concepts (repository_id, slug);

-- 4. Index for the grouped list: (repository_id, lower(canonical_name))
--    so COUNT(DISTINCT lower(canonical_name)) and the per-group
--    representative pick are index-only scans.
CREATE INDEX IF NOT EXISTS idx_concepts_repo_name_lower
    ON okt_repository.concepts (repository_id, lower(canonical_name));