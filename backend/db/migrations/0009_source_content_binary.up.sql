-- 0009_source_content_binary.up.sql
--
-- Switch okt_repository.sources.content from TEXT to BYTEA so the
-- worker can persist non-UTF-8 fetched bodies (PDF, JPEG, ZIP).
--
-- The column is cleared first because PostgreSQL's text→bytea cast
-- runs the server's encoding validator and raises SQLSTATE 22P02 on
-- any value that isn't valid UTF-8. With the column empty the cast
-- has nothing to reject.
--
-- Historical head-prefix copies the worker had already stored are
-- lost; the application code will repopulate them on the next fetch.

UPDATE okt_repository.sources
    SET content = NULL;

ALTER TABLE okt_repository.sources
    ALTER COLUMN content TYPE BYTEA USING content::bytea;
