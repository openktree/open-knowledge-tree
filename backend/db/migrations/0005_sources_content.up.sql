-- 0005_sources_content.up.sql
--
-- Extend the per-repository `sources` table with the columns
-- the RetrieveSource worker needs to persist a row when a
-- fetch source task runs. The columns are intentionally
-- nullable/optional: an enqueued job that hasn't been picked
-- up yet (or one that ran but the body is too large to keep
-- inline) just leaves them NULL.
--
--   content:     the response body the fetch strategy
--                returned, stored as TEXT. Large or binary
--                payloads are truncated to a head-prefix so
--                the row stays reasonable in size; a future
--                migration can move heavy content into object
--                storage and reduce this to a pointer.
--   fetched_at:  wall-clock time the worker finished the
--                fetch (success or failure). NULL while the
--                job is queued/running.
--   error:       short string capturing the worker's error
--                message when the fetch failed. NULL on
--                success. Kept as TEXT for grep-ability.
--
-- The existing status CHECK constraint already includes
-- 'fetching' (set up in 0004_sources.up.sql) so no schema
-- change is needed there. This migration is purely
-- additive: ALTER TABLE ... ADD COLUMN IF NOT EXISTS is
-- idempotent so the migration runner is happy on a database
-- that already saw it.

ALTER TABLE okt_repository.sources
    ADD COLUMN IF NOT EXISTS content    TEXT,
    ADD COLUMN IF NOT EXISTS fetched_at TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS error      TEXT;
