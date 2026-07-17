-- 0025_concepts_slug_charset.down.sql
--
-- Reverses 0025_concepts_slug_charset.up.sql. Drops the slug
-- column, the trigger/function, the CHECK constraint, and the
-- supporting indexes.

DROP INDEX IF EXISTS okt_repository.idx_concepts_repo_name_lower;
DROP INDEX IF EXISTS okt_repository.idx_concepts_repo_slug;
ALTER TABLE okt_repository.concepts
    DROP CONSTRAINT IF EXISTS chk_concepts_canonical_name_charset;
DROP TRIGGER IF EXISTS trg_concepts_slug ON okt_repository.concepts;
DROP FUNCTION IF EXISTS okt_repository.concepts_slug();
ALTER TABLE okt_repository.concepts
    DROP COLUMN IF EXISTS slug;