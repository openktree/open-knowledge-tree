-- 0026_concept_summaries.down.sql
ALTER TABLE okt_repository.concepts DROP COLUMN IF EXISTS summarizing_at;
DROP TABLE IF EXISTS okt_repository.concept_summaries;