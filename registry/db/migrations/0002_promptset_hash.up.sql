-- 0002_promptset_hash.up.sql
--
-- Tags every registry decomposition with the promptset hash that
-- produced it, so the OKT pull worker can filter remote
-- decompositions to those whose promptset_hash is in the pulling
-- repo's accepted set. Without this, two OKT instances running
-- different promptsets would cross-contaminate each other's graphs
-- via the shared registry, and the resulting knowledge graph would
-- not be able to carry a coherent posture (the user's stated
-- concern: "if we mix them at the registry level all the graphs
-- would end up corrupted").
--
-- The column is NULLable with no DEFAULT: NULL means "this
-- decomposition predates the promptset feature" and is interpreted
-- by the OKT pull worker as the built-in hash. The backfill is done
-- at OKT app boot, not by this migration, so the literal built-in
-- hash (which changes whenever a prompt is edited) is never baked
-- into registry SQL.
--
-- The column is added to decompositions (the per-(source, model)
-- row) rather than to fact_hashes because the promptset is a
-- property of the decomposition as a whole: every fact in a
-- decomposition shares the same promptset hash. The pull worker
-- reads decompositions.promptset_hash and skips the whole package
-- when the hash is not in the pulling repo's accepted set.

ALTER TABLE decompositions
    ADD COLUMN promptset_hash TEXT;

CREATE INDEX IF NOT EXISTS idx_decompositions_promptset ON decompositions(promptset_hash);