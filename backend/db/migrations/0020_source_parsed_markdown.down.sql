-- 0020_source_parsed_markdown.down.sql
--
-- Drop the parsed_markdown column added in 0020. Idempotent so
-- re-running on a database that was never migrated succeeds.

ALTER TABLE okt_repository.sources
    DROP COLUMN IF EXISTS parsed_markdown;