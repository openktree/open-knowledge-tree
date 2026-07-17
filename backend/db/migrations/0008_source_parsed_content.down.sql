-- 0008_source_parsed_content.down.sql
--
-- Reverse the additive changes from the corresponding .up.sql.
-- DROP TABLE first because the source table has a FK into
-- the source_images table; the FK is declared with ON DELETE
-- CASCADE so dropping the parent (sources) would cascade and
-- also drop the child, but we drop explicitly so the
-- statement order in the down-migration matches the
-- up-migration's reverse order.
DROP TABLE IF EXISTS okt_repository.source_images;

ALTER TABLE okt_repository.sources
    DROP COLUMN IF EXISTS parse_status,
    DROP COLUMN IF EXISTS parsed_at,
    DROP COLUMN IF EXISTS parsed_language,
    DROP COLUMN IF EXISTS parsed_sitename,
    DROP COLUMN IF EXISTS parsed_author,
    DROP COLUMN IF EXISTS parsed_html,
    DROP COLUMN IF EXISTS parsed_text,
    DROP COLUMN IF EXISTS parsed_title;
