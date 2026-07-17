-- 0044_repository_sync_levels.down.sql

ALTER TABLE repositories
    DROP COLUMN IF EXISTS registry_push_level;

ALTER TABLE repositories
    DROP COLUMN IF EXISTS registry_pull_level;