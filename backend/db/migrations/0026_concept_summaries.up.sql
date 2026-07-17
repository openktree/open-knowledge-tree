-- 0026_concept_summaries.up.sql
--
-- Concept summaries are incremental LLM-produced syntheses of the
-- facts linked to a (concept, context) pair. The summarization task
-- (tasks.SummarizeConcepts) fans out from extract_concepts in
-- parallel with embed_concepts.
--
-- Summaries are partitioned into sequential "slices" per concept_id,
-- each covering BatchSize facts (configurable, default 20). The
-- oldest slice covering the fewest facts stays "open" (is_complete =
-- FALSE) and is regenerated as new facts arrive, until it reaches
-- BatchSize facts and freezes (is_complete = TRUE). The next batch
-- of facts then seeds a new open slice. A fact appears in exactly
-- one summary per (concept, context) pair (tracked by
-- covered_fact_ids), so the open slice's covered set plus the new
-- facts forms the regeneration input.
--
-- The unique partial index uq_concept_summaries_concept_open
-- guarantees at most ONE open (incremental) summary per concept_id;
-- the worker relies on this invariant (GetOpenSummary is a scalar
-- lookup).
--
-- concepts.summarizing_at is the per-concept lock the worker uses
-- to ensure two concurrent summarize_concepts jobs touching the
-- same concept don't both regenerate it. It is held across the
-- (out-of-tx) LLM call and released on completion; a staleness
-- window reclaims a lock whose owner crashed.

CREATE TABLE IF NOT EXISTS okt_repository.concept_summaries (
    id               UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    concept_id       UUID NOT NULL REFERENCES okt_repository.concepts(id) ON DELETE CASCADE,
    repository_id    UUID NOT NULL REFERENCES okt_system.repositories(id) ON DELETE CASCADE,
    context          TEXT NOT NULL,                  -- copied from concepts.context for query convenience
    sequence_num     INT  NOT NULL,                  -- 1-based slice index per concept_id
    is_complete      BOOLEAN NOT NULL DEFAULT FALSE, -- FALSE = still accumulating (open); TRUE = frozen
    fact_count       INT  NOT NULL DEFAULT 0,
    content          TEXT NOT NULL,                  -- markdown with [text](<fact_id>) citations
    covered_fact_ids UUID[] NOT NULL DEFAULT '{}',   -- exactly one summary per fact per pair
    model            TEXT,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_concept_summaries_concept
    ON okt_repository.concept_summaries (concept_id, sequence_num);
CREATE UNIQUE INDEX IF NOT EXISTS uq_concept_summaries_concept_open
    ON okt_repository.concept_summaries (concept_id)
    WHERE is_complete = FALSE;
CREATE INDEX IF NOT EXISTS idx_concept_summaries_repo
    ON okt_repository.concept_summaries (repository_id);

-- Per-concept summarization lock. Held across the LLM call by the
-- worker; released on completion. A stale lock (older than the
-- configured staleness, default 2h) is reclaimable by the next
-- worker run so a crashed job doesn't wedge the concept forever.
ALTER TABLE okt_repository.concepts
    ADD COLUMN IF NOT EXISTS summarizing_at TIMESTAMPTZ;