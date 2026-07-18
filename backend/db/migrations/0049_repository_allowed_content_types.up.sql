-- 0049_repository_allowed_content_types.up.sql
--
-- Per-repository allowed content types gate. Restricts what kinds of
-- sources a repo accepts so an admin can configure a repo to be
-- strictly scientific (DOI only) or upload-only, etc.
--
-- NULL (the default) = allow all content types (backward compatible
-- for existing repos). A non-NULL array restricts to the listed
-- kinds: "document" (uploaded files), "url" (web URLs), "doi" (DOIs).
-- The three values are the only accepted members; the CHECK enforces
-- it. An empty array is rejected by the handler (use NULL to reset
-- to allow-all).
--
-- Enforcement is at the three ingestion points: CreateSource (url/doi),
-- UploadSource (document), and EnqueueRetrieveSource (url/doi via
-- fetch.ClassifyURL). A 403 is returned when the classified type is
-- not in the repo's allow-list.
--
-- Note: unqualified ALTER TABLE to match 0035/0036/0037/0038/0040/0044.

ALTER TABLE repositories
    ADD COLUMN IF NOT EXISTS allowed_content_types TEXT[]
    CHECK (allowed_content_types <@ ARRAY['document','url','doi']::text[]);