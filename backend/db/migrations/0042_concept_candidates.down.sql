-- 0042_concept_candidates.down.sql
--
-- Reverses 0042_concept_candidates.up.sql. Drops the candidate
-- tables and the aliases_refined_at column. Note: dropping
-- concept_candidates with resolved_concept_id SET NULL does not
-- cascade to concepts; the concepts created by refine_concepts stay.

ALTER TABLE okt_repository.concepts
    DROP COLUMN IF EXISTS aliases_refined_at;

DROP TABLE IF EXISTS okt_repository.fact_candidates;
DROP TABLE IF EXISTS okt_repository.concept_candidates;