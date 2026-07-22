-- 0060_source_chunk_failures.up.sql
--
-- Surface per-source failure state so operators can see, in the UI,
-- which sources had chunk extraction failures and re-trigger
-- reprocessing for just those failed chunks (instead of re-running
-- the whole source, which would duplicate the facts from successful
-- chunks since CreateFact has no ON CONFLICT).
--
-- Three columns added to okt_repository.sources:
--
--   chunk_failures INT NOT NULL DEFAULT 0
--     Count of chunks that failed extraction after in-job retries
--     exhausted. 0 means the source decomposed cleanly. Updated by
--     the source_decomposition worker at the end of its run; cleared
--     by the reprocess admin endpoint when a follow-up run succeeds.
--
--   chunk_errors JSONB
--     Array of {index, type, error, attempts} objects, one per
--     failed chunk, for diagnostic display in the UI tooltip. Bounded
--     by the number of chunks (typically <= 20). NULL when
--     chunk_failures = 0.
--
--   last_chunk_failure_at TIMESTAMPTZ
--     When the most recent chunk failure was recorded. Used for
--     sorting/filtering in the UI ("show sources with recent
--     failures"). NULL when chunk_failures = 0.
--
--   concept_skip_count INT NOT NULL DEFAULT 0
--     Denormalized count of facts linked to this source that have a
--     fact_concept_skips row with attempts < max_concept_attempts
--     (i.e. facts that are still retryable). Maintained by
--     extract_concepts.recordSkip (increment) and the admin
--     reextract endpoint (decrement on clear). Denormalized so the
--     sources list query doesn't need a JOIN to fact_concept_skips.
--     0 means either every fact has a concept link or every skip has
--     exhausted its retry budget.
--
-- The source_decomposition worker no longer marks the source
-- `processed` (status='processed', processed_at=now()) when any
-- chunks still fail after in-job retries. The source stays in
-- `fetched` state with chunk_failures > 0, surfaced in the UI, and
-- the operator triggers reprocessing via
-- POST /api/v1/admin/repos/{repoID}/sources/{sourceID}/reprocess,
-- which enqueues a SourceDecompositionArgs carrying only the failed
-- chunk indices (RetryChunkIndices) so successful chunks are not
-- re-run and no duplicate facts are created.

ALTER TABLE okt_repository.sources
    ADD COLUMN IF NOT EXISTS chunk_failures INT NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS chunk_errors JSONB,
    ADD COLUMN IF NOT EXISTS last_chunk_failure_at TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS concept_skip_count INT NOT NULL DEFAULT 0;

-- Partial index so the UI can cheaply list "sources with failures"
-- without scanning the whole repo's source list.
CREATE INDEX IF NOT EXISTS idx_sources_chunk_failures
    ON okt_repository.sources (repository_id, last_chunk_failure_at DESC)
    WHERE chunk_failures > 0;

CREATE INDEX IF NOT EXISTS idx_sources_concept_skip_count
    ON okt_repository.sources (repository_id, concept_skip_count)
    WHERE concept_skip_count > 0;