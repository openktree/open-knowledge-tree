-- 0023_concepts.down.sql
-- Reverse the concepts Phase 1 schema. Tables are dropped in
-- reverse dependency order so CASCADE from parent tables clears
-- the children. Idempotent: each DROP IF EXISTS is a no-op when
-- the table is already gone.

DROP TABLE IF EXISTS okt_repository.fact_concepts;
DROP TABLE IF EXISTS okt_repository.concept_aliases;
DROP TABLE IF EXISTS okt_repository.concepts;