-- 0004_sources.down.sql
DROP INDEX IF EXISTS okt_repository.idx_sources_status;
DROP INDEX IF EXISTS okt_repository.idx_sources_repository_id;
DROP TABLE IF EXISTS sources;
