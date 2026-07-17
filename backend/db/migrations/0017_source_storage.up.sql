-- 0017_source_storage.up.sql
--
-- Adds storage-backend columns to okt_repository.source_images and
-- okt_repository.sources so the retrieve_source worker can record
-- where it persisted each image (and, for PDF sources, the full
-- source body). The columns are populated by the new file-storage
-- module (`internal/providers/storage`); they stay NULL until the
-- hosting job runs, so existing rows are unaffected.
--
-- `storage_key` is the opaque key passed to the storage backend
-- (e.g. "repositories/{repoID}/sources/{sourceID}/images/{imageID}.png").
-- `content_type` is the MIME sniffed at fetch time, surfaced back on
-- the serving endpoint so the DB is the source of truth (no
-- re-sniffing on the hot path).
-- `local_path` already existed on source_images (migration 0008);
-- for sources it is added here as a human-readable view of
-- `storage_key` (mirrors the source_images shape for consistency).
-- `mirrored_at` / `stored_at` record when the storage write
-- completed; NULL means "not yet mirrored" (the frontend falls back
-- to the external URL, or shows a placeholder for page renders).
--
-- Idempotent per AGENTS.md: ADD COLUMN IF NOT EXISTS, CREATE INDEX
-- IF NOT EXISTS. The same file runs against every database declared
-- in cfg.Databases.

ALTER TABLE okt_repository.source_images
    ADD COLUMN IF NOT EXISTS storage_key   TEXT,
    ADD COLUMN IF NOT EXISTS content_type  TEXT,
    ADD COLUMN IF NOT EXISTS mirrored_at   TIMESTAMPTZ;

ALTER TABLE okt_repository.sources
    ADD COLUMN IF NOT EXISTS storage_key   TEXT,
    ADD COLUMN IF NOT EXISTS content_type  TEXT,
    ADD COLUMN IF NOT EXISTS local_path    TEXT,
    ADD COLUMN IF NOT EXISTS stored_at     TIMESTAMPTZ;

-- Speeds up the serving endpoint's lookup-by-key path (future CDN /
-- sync workers also enumerate by key).
CREATE INDEX IF NOT EXISTS idx_source_images_storage_key
    ON okt_repository.source_images (storage_key)
    WHERE storage_key IS NOT NULL;

CREATE INDEX IF NOT EXISTS idx_sources_storage_key
    ON okt_repository.sources (storage_key)
    WHERE storage_key IS NOT NULL;