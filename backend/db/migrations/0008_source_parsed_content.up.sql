-- 0008_source_parsed_content.up.sql
--
-- Persist the content_parsing.ParsedDoc result on the source
-- row, and split image references out into a child table.
--
-- Design notes (the "one source of truth" rule):
--
--   * The 1:1 fields (title, text, html, author, sitename,
--     language, parsed_at, parse_status) live on the source
--     row. The source row is the one the API, the UI, the
--     future fact table, and the future search index all
--     point at. A second table for a 1:1 split buys nothing
--     and forces an extra JOIN on every read of the most
--     frequently queried table in the system.
--
--   * The 1:many fields (images) go into a child table.
--     Reasons:
--       - A PDF can have dozens of page renders; even an
--         HTML article can have 20+ inline images. A column
--         would make the source row wide and force every
--         read of the source to also deserialize the image
--         list.
--       - The future fact table (a source has many facts;
--         some facts are grounded in a specific image) needs
--         a foreign-key target on individual images. That
--         target must be a row, not a JSONB array index.
--       - The future CDN / object-store migration will add
--         local_path, bytes, cdn_url, mirrored_at columns
--         on individual image rows. A child table absorbs
--         those schema additions; a JSONB blob would not.
--       - The discriminator (kind = 'inline' | 'page') is
--         one column. Splitting into two tables would only
--         pay off if the two kinds diverged in shape, which
--         they don't.
--
--   * JSONB was rejected for the image list for the reasons
--     above (no FK target, no per-image indexes, every read
--     deserializes the list) and because Postgres has
--     excellent support for the alternative: a small
--     dedicated table with two covering indexes.
--
--   * The constraints below encode the data shape so the
--     application code can stop defensively checking. They
--     are cheap (CHECK constraints run only on write) and
--     protect against the worker, future ETL jobs, and
--     hand-rolled admin queries from writing rows that
--     violate the invariant the UI relies on.

-- 1:1 parsed fields on the source row.
ALTER TABLE okt_repository.sources
    ADD COLUMN IF NOT EXISTS parsed_title    TEXT,
    ADD COLUMN IF NOT EXISTS parsed_text     TEXT,
    ADD COLUMN IF NOT EXISTS parsed_html     TEXT,
    ADD COLUMN IF NOT EXISTS parsed_author   TEXT,
    ADD COLUMN IF NOT EXISTS parsed_sitename TEXT,
    ADD COLUMN IF NOT EXISTS parsed_language TEXT,
    ADD COLUMN IF NOT EXISTS parsed_at       TIMESTAMPTZ,
    -- parse_status is explicit and nullable so the UI can
    -- distinguish a never-parsed row (NULL) from a parsed
    -- but empty row ('ok' with empty text) from a row whose
    -- parser errored ('failed'). 'unsupported' is set when
    -- the source type has no parser wired in.
    ADD COLUMN IF NOT EXISTS parse_status    TEXT
        CHECK (parse_status IS NULL OR parse_status IN ('ok', 'unsupported', 'failed'));

-- 1:many images. ON DELETE CASCADE matches the source
-- row's relationship with its repository (also CASCADE)
-- so a source deletion cleans up the image table in one
-- statement.
CREATE TABLE IF NOT EXISTS okt_repository.source_images (
    id           UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    source_id    UUID NOT NULL REFERENCES okt_repository.sources(id) ON DELETE CASCADE,
    -- 'inline' for HTML <img>, 'page' for PDF page renders.
    kind         TEXT NOT NULL,
    -- Page number 1..N for PDF page renders; NULL for inline.
    page_number  INT,
    -- 0-indexed position within the document. Stable across
    -- re-parses when the parser preserves order, which both
    -- trafilatura (DOM order) and go-fitz (page order) do.
    position     INT NOT NULL DEFAULT 0,
    -- Absolute URL when known. Required for 'inline' (HTML
    -- <img src>), NULL for 'page' (rendered locally by the
    -- PDF parser — no remote source exists).
    url          TEXT,
    -- Image dimensions. Filled for 'page' renders (we know
    -- the DPI and the page geometry). Left NULL for 'inline'
    -- until a future job downloads the asset and probes the
    -- file header.
    width        INT,
    height       INT,
    -- File size in bytes. Filled for 'page' (we just
    -- rendered it). NULL for 'inline' until downloaded.
    bytes        INT,
    -- Path under the configured storage root (e.g.
    -- 'var/source_pages/<source-id>/page-1.png') or, in the
    -- future, an object-store key. NULL until the hosting
    -- job runs.
    local_path   TEXT,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),

    -- Data shape constraints. Each one encodes a rule the
    -- UI and the future fact table rely on.
    CONSTRAINT source_images_kind_chk
        CHECK (kind IN ('inline', 'page')),
    CONSTRAINT source_images_page_inline_chk
        CHECK (
            (kind = 'page'   AND page_number IS NOT NULL AND page_number > 0) OR
            (kind = 'inline' AND page_number IS NULL)
        ),
    CONSTRAINT source_images_url_chk
        CHECK (
            (kind = 'inline' AND url IS NOT NULL AND length(url) > 0) OR
            (kind = 'page'   AND url IS NULL)
        )
);

-- Covering index for the hot read path: "give me all media
-- for this source, ordered for display." Composite order
-- matches the UI's expected render order (page renders
-- first by page number, inline images by DOM position).
CREATE INDEX IF NOT EXISTS idx_source_images_source_order
    ON okt_repository.source_images (source_id, kind, page_number NULLS LAST, position);

-- Lookup by URL for the future dedup job: a source that
-- has been re-fetched from the same URL on a different
-- repository will have multiple rows; this index is the
-- join target for "find all sources that reference this
-- image URL" (used by the future cross-repo dedup pass).
CREATE INDEX IF NOT EXISTS idx_source_images_url
    ON okt_repository.source_images (url)
    WHERE url IS NOT NULL;
