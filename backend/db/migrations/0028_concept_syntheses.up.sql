-- 0028_concept_syntheses.up.sql
--
-- Concept syntheses are the single authoritative "definition" the
-- system produces for a canonical-name group, folding together ALL
-- summary slices across every concept_id sharing lower(canonical_name)
-- in a repository. Unlike concept_summaries (one incremental slice
-- per BatchSize facts, many per concept_id), a concept_synthesis is
-- ONE upsertable row per (repository_id, lower(canonical_name)),
-- regenerated whenever any of the group's concept_ids gets a new or
-- updated summary slice.
--
-- The synthesize_concept worker (chained from summarize_concepts)
-- loads the group's slices, optionally picks up to N image facts via
-- a separate LLM call, and runs one synthesis LLM call. The returned
-- markdown is stored verbatim; it may embed text citations
-- [text](<fact_id>) and image citations ![alt](<fact_id>) that the
-- frontend rewrites into clickable links / renderable image URLs.
--
-- embedded_image_ids is the set of image fact_ids the synthesis
-- embeds (extracted from the markdown by the worker) so the read
-- path can eager-load those facts' image_url without parsing the
-- markdown server-side on every GET.
--
-- No per-row lock column: the worker uses a group-keyed Postgres
-- advisory lock (pg_try_advisory_lock(hashtext(repo_id||':'
-- ||lower(canonical_name)))) held across the LLM call and released in
-- a defer. Advisory locks are per-connection in pgx; a crashed
-- worker's connection returns to the pool and the lock clears
-- automatically, so no staleness window is needed.

CREATE TABLE IF NOT EXISTS okt_repository.concept_syntheses (
    id                  UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    repository_id       UUID NOT NULL REFERENCES okt_system.repositories(id) ON DELETE CASCADE,
    canonical_name      TEXT NOT NULL,
    content             TEXT NOT NULL,
    covered_summary_ids UUID[] NOT NULL DEFAULT '{}',
    covered_concept_ids UUID[] NOT NULL DEFAULT '{}',
    embedded_image_ids  UUID[] NOT NULL DEFAULT '{}',
    model               TEXT,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX IF NOT EXISTS uq_concept_syntheses_repo_name
    ON okt_repository.concept_syntheses (repository_id, lower(canonical_name));
CREATE INDEX IF NOT EXISTS idx_concept_syntheses_repo
    ON okt_repository.concept_syntheses (repository_id);