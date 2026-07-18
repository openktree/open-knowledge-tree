DROP INDEX IF EXISTS idx_decompositions_promptset;

ALTER TABLE decompositions
    DROP COLUMN IF EXISTS promptset_hash;