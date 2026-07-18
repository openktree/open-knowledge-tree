ALTER TABLE repositories
    DROP COLUMN IF EXISTS active_promptset_hash;

ALTER TABLE repositories
    DROP COLUMN IF EXISTS accepted_promptset_hashes;