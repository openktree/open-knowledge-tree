-- 0016_facts_image.down.sql
ALTER TABLE okt_repository.facts
    DROP COLUMN IF EXISTS image_url,
    DROP COLUMN IF EXISTS fact_kind;