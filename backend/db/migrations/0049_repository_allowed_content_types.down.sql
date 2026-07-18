-- 0049_repository_allowed_content_types.down.sql
--
-- Drops the per-repository allowed content types column added in
-- 0049. Re-enables all content types for every repo (the default
-- behavior before the gate shipped).

ALTER TABLE repositories
    DROP COLUMN IF EXISTS allowed_content_types;