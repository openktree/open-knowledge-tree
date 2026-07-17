-- 0042_concept_candidates.up.sql
--
-- Concept candidates: a routing cache + work queue for concept
-- refinement. extract_concepts creates a candidate for each
-- (concept_text, context) pair that does not text-search-match an
-- existing concept. refine_concepts resolves each candidate: it
-- routes to an existing concept (via canonical name / alias match,
-- pre- or post-LLM), merges into it, or creates a new concept. Once
-- resolved, the candidate row stays as a cache entry so a future
-- extraction of the same (concept_text, context) routes directly
-- without an LLM call.
--
-- fact_candidates is the junction for facts linked to unresolved
-- candidates. On resolution, rows are moved to fact_concepts and the
-- junction rows are deleted (the candidate row stays as cache).
--
-- aliases_refined_at on concepts tracks when a concept's alias set
-- was last refined by the LLM. NULL = never refined (candidate just
-- promoted). Used by the pruning gate: re-prune only when >= X new
-- aliases have accumulated since this timestamp.

CREATE TABLE IF NOT EXISTS okt_repository.concept_candidates (
    id                  UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    repository_id       UUID NOT NULL REFERENCES okt_system.repositories(id) ON DELETE CASCADE,
    concept_text        TEXT NOT NULL,
    context             TEXT NOT NULL,
    seed_aliases        TEXT[] NOT NULL DEFAULT '{}',
    resolved_concept_id UUID REFERENCES okt_repository.concepts(id) ON DELETE SET NULL,
    resolved_at         TIMESTAMPTZ,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX IF NOT EXISTS uq_concept_candidates_text_context
    ON okt_repository.concept_candidates (repository_id, lower(concept_text), lower(context));
CREATE INDEX IF NOT EXISTS idx_concept_candidates_repo
    ON okt_repository.concept_candidates (repository_id);
CREATE INDEX IF NOT EXISTS idx_concept_candidates_unresolved
    ON okt_repository.concept_candidates (repository_id)
    WHERE resolved_concept_id IS NULL;

CREATE TABLE IF NOT EXISTS okt_repository.fact_candidates (
    fact_id       UUID NOT NULL REFERENCES okt_repository.facts(id) ON DELETE CASCADE,
    candidate_id  UUID NOT NULL REFERENCES okt_repository.concept_candidates(id) ON DELETE CASCADE,
    first_seen_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (fact_id, candidate_id)
);
CREATE INDEX IF NOT EXISTS idx_fact_candidates_candidate
    ON okt_repository.fact_candidates (candidate_id);

ALTER TABLE okt_repository.concepts
    ADD COLUMN IF NOT EXISTS aliases_refined_at TIMESTAMPTZ;