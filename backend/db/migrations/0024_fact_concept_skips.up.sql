-- 0024_fact_concept_skips.up.sql
--
-- Permanent "skip" markers for facts that the extract_concepts
-- worker could not process (e.g. the concept-extraction LLM
-- returned a transient error such as an OpenRouter timeout). The
-- worker has no periodic re-driver, so without an explicit skip
-- row every subsequent dedup → extract_concepts chain would
-- re-attempt the same failing fact on every pass and burn LLM
-- quota forever. Instead, the worker records a skip row and
-- moves on; an operator must delete the row (or re-enqueue a
-- fresh extract_concepts job after fixing the upstream issue) to
-- retry.
--
-- No TTL column: skips are permanent by design. See
-- ListStableFactsForConceptExtraction for the NOT EXISTS filter
-- that excludes skipped facts from the extraction candidate set.

CREATE TABLE IF NOT EXISTS okt_repository.fact_concept_skips (
    fact_id     UUID PRIMARY KEY REFERENCES okt_repository.facts(id) ON DELETE CASCADE,
    last_error  TEXT NOT NULL,
    skipped_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_fact_concept_skips_skipped_at
    ON okt_repository.fact_concept_skips (skipped_at);