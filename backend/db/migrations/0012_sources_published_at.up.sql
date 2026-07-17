-- 0012_sources_published_at.up.sql
--
-- Add a `published_at` DATE column to okt_repository.sources
-- to record the publication date of the underlying resource
-- when it is known. Nullable: most homepage / dataset /
-- non-article sources will not have a meaningful date, and
-- even article sources occasionally lack one (the OpenAlex
-- record has no publication_date, trafilatura / htmldate
-- gave up on the page, the search result we clicked on
-- didn't carry a date, etc.).
--
-- Why DATE and not TIMESTAMPTZ:
--
--   * OpenAlex `publication_date` is ISO 8601 day-precision
--     ("2018-02-13").
--   * Trafilatura's `Metadata.Date` is a `time.Time` populated
--     by htmldate, which also works at day precision.
--   * The "show me sources from 2023" / "sort by publication
--     date" use cases the future fact table and search index
--     will exercise are all day-resolution.
--   * DATE is smaller on disk, indexes smaller, and casts to
--     TIMESTAMPTZ cheaply when the rare sub-day-precision
--     value ever shows up.
--
-- Where the value comes from (in priority order, see
-- internal/taskmanager/tasks/retrieve_source.go):
--
--   1. The `published_at` field on RetrieveSourceArgs, when
--      the search-result click-through path set it
--      (OpenAlex, future providers that ship a date).
--   2. Otherwise, `parsed.PublishedAt` from the
--      content_parsing step (trafilatura / htmldate).
--   3. Otherwise, NULL. We do not overwrite a value the
--      caller already set with a parsed date — earliest
--      known date wins, which matches how DOI is
--      backfilled on the same row.
--
-- This column is intentionally NOT populated on every
-- re-parse. A re-parse (e.g. after a content fix) keeps
-- the existing date; only the parsed_* fields are
-- refreshed. That keeps the value stable for the UI
-- ("Published 2021-04-23") across reparses that don't
-- change the underlying publication.
ALTER TABLE okt_repository.sources
    ADD COLUMN IF NOT EXISTS published_at DATE;

-- A date column on a per-repo table that the future fact
-- table and the search index will both filter on. The
-- composite (repository_id, published_at) covers the
-- common "list this repo's sources from 2024 sorted by
-- date" query; a single-column index on published_at
-- covers cross-repo statistics ("how many sources per
-- month across the system"). Both are cheap to maintain
-- and the planner picks the right one for the WHERE
-- clause.
CREATE INDEX IF NOT EXISTS idx_sources_published_at
    ON okt_repository.sources (published_at)
    WHERE published_at IS NOT NULL;

CREATE INDEX IF NOT EXISTS idx_sources_repo_published_at
    ON okt_repository.sources (repository_id, published_at DESC)
    WHERE published_at IS NOT NULL;
