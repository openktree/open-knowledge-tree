-- 0059_fact_concept_skips_retry_budget.down.sql
DROP INDEX IF EXISTS okt_repository.idx_fact_concept_skips_attempts;
ALTER TABLE okt_repository.fact_concept_skips
    DROP COLUMN IF EXISTS last_attempt_at,
    DROP COLUMN IF EXISTS attempts;