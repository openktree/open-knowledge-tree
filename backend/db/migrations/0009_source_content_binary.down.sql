-- 0009_source_content_binary.down.sql
-- Reverse: BYTEA → TEXT. The bytea→text cast is permissive and
-- does not validate encoding, so no data prep is needed.

ALTER TABLE okt_repository.sources
    ALTER COLUMN content TYPE TEXT USING content::text;
