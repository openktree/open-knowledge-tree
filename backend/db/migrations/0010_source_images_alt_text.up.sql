ALTER TABLE IF EXISTS okt_repository.source_images
    ADD COLUMN IF NOT EXISTS alt_text TEXT;
