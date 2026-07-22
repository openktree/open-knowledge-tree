-- 0059_fact_concept_skips_retry_budget.up.sql
--
-- Convert fact_concept_skips from a permanent marker into a retry-
-- budget soft-skip. Previously any transient LLM failure (OpenRouter
-- timeout, rate-limit wait, network blip, malformed JSON) wrote a
-- permanent skip row, and the candidate-selection query excluded that
-- fact forever. ~86% of the unlinked-facts bug (139,756 of 161,496)
-- came from this: a single 120s streaming-decode timeout permanently
-- severed 10 facts from their concepts.
--
-- New behavior:
--   - attempts counts consecutive permanent failures for this fact.
--     The candidate-selection query excludes the fact only when
--     attempts >= cfg.task.concept_extraction.max_attempts (default 3).
--     Transient errors do NOT call RecordFactConceptSkip at all — the
--     fact stays in the candidate set and is retried on the next pass.
--   - last_attempt_at tracks when the most recent skip happened, for
--     diagnostics and the admin UI.
--
-- Existing rows (the 121,312 already in the table) get attempts=1,
-- last_attempt_at=skipped_at. They are eligible for re-extraction via
-- the admin endpoint (POST /api/v1/admin/repos/{id}/concepts/reextract)
-- which clears rows with attempts < max_attempts. An operator can
-- also DELETE them directly.
--
-- See ListStableFactsForConceptExtraction for the NOT EXISTS filter
-- that now reads attempts >= @max_concept_attempts.

ALTER TABLE okt_repository.fact_concept_skips
    ADD COLUMN IF NOT EXISTS attempts INT NOT NULL DEFAULT 1,
    ADD COLUMN IF NOT EXISTS last_attempt_at TIMESTAMPTZ NOT NULL DEFAULT now();

-- Backfill last_attempt_at from skipped_at for existing rows so the
-- diagnostics column is populated immediately.
UPDATE okt_repository.fact_concept_skips
   SET last_attempt_at = skipped_at
 WHERE last_attempt_at = now() AND skipped_at <> now();

CREATE INDEX IF NOT EXISTS idx_fact_concept_skips_attempts
    ON okt_repository.fact_concept_skips (attempts);