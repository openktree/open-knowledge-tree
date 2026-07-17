-- 0016_facts_image.up.sql
--
-- Adds image_url + fact_kind to okt_repository.facts so image facts
-- (facts extracted from a source's images by the multimodal image
-- extractor) can carry the canonical image URL separately from the
-- fact text. The frontend renders <img> directly from image_url; the
-- text column still holds the fact-oriented description produced by
-- the model, which is what gets embedded and deduped against.
--
-- fact_kind defaults to 'text' so every existing row is backfilled
-- without a write-path migration. The CHECK constraint keeps the
-- column to the two known kinds; a future kind (e.g. 'audio') would
-- extend the CHECK.
--
-- Idempotent per the migration rules in AGENTS.md: ADD COLUMN IF NOT
-- EXISTS. The same file runs against every database declared in
-- cfg.Databases.

ALTER TABLE okt_repository.facts
    ADD COLUMN IF NOT EXISTS image_url TEXT,
    ADD COLUMN IF NOT EXISTS fact_kind TEXT NOT NULL DEFAULT 'text'
        CHECK (fact_kind IN ('text', 'image'));