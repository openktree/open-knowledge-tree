DROP TABLE IF EXISTS okt_repository.fact_sources;
DROP TABLE IF EXISTS okt_repository.facts;

ALTER TABLE okt_repository.sources DROP COLUMN IF EXISTS processed_at;

ALTER TABLE okt_repository.sources
    DROP CONSTRAINT IF EXISTS sources_status_check;

ALTER TABLE okt_repository.sources
    ADD CONSTRAINT sources_status_check
        CHECK (status IN ('pending', 'fetching', 'fetched', 'failed'));