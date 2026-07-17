-- 0017_source_storage.down.sql
-- Reverses 0017_source_storage.up.sql. Idempotent.

DROP INDEX IF EXISTS okt_repository.idx_sources_storage_key;
DROP INDEX IF EXISTS okt_repository.idx_source_images_storage_key;

ALTER TABLE okt_repository.sources
    DROP COLUMN IF EXISTS stored_at,
    DROP COLUMN IF EXISTS local_path,
    DROP COLUMN IF EXISTS content_type,
    DROP COLUMN IF EXISTS storage_key;

ALTER TABLE okt_repository.source_images
    DROP COLUMN IF EXISTS mirrored_at,
    DROP COLUMN IF EXISTS content_type,
    DROP COLUMN IF EXISTS storage_key;