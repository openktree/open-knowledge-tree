-- 0050_repository_contributor.down.sql
--
-- Reverse of 0050_repository_contributor.up.sql: drop the
-- contributor identity columns. Repositories revert to the
-- pre-migration behavior (no contributor field on push, which the
-- registry treats as anonymous).

ALTER TABLE repositories
    DROP COLUMN IF EXISTS contributor_display_name,
    DROP COLUMN IF EXISTS contributor_anonymous;