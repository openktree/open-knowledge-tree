-- summaries.sql — concept summarization queries.
--
-- A concept_summary is an incremental LLM-produced synthesis of the
-- facts linked to a (concept, context) pair. The summarization task
-- (tasks.SummarizeConcepts) fans out from extract_concepts in
-- parallel with embed_concepts. See migration 0026 for the schema
-- and the slicing/locking model.
--
-- Slicing model: the first slice (sequence_num 1) is an "open
-- accumulator" — regenerated as new facts trickle in, until it
-- reaches BatchSize covered facts and freezes (is_complete = TRUE).
-- Once a complete slice exists, the worker switches to batch-only
-- mode: it only emits complete BatchSize slices, never an open
-- remainder, and skips the pass entirely when fewer than BatchSize
-- new facts have arrived. This caps LLM cost by spending one call
-- per fact while a concept is still small, then one call per full
-- batch once it has crystallized.

-- name: ListTouchedConceptsForSummary :many
-- Source-scoped: distinct concept_ids linked (via fact_concepts →
-- fact_sources) to at least one fact from THIS source. Used by the
-- summarize_concepts worker to know which concepts the current
-- source's extract_concepts pass touched. DISTINCT because the
-- join expands (a concept links to many facts, a fact to many
-- sources). The worker passes the resulting list as the explicit
-- ConceptIDs chunk of one SummarizeConceptsArgs job per chunk.
SELECT DISTINCT c.id, c.canonical_name, c.context, c.repository_id
FROM okt_repository.concepts c
JOIN okt_repository.fact_concepts fc ON fc.concept_id = c.id
JOIN okt_repository.fact_sources fs ON fs.fact_id = fc.fact_id
WHERE c.repository_id = @repository_id
  AND fs.source_id = @source_id
ORDER BY c.canonical_name;

-- name: ListTouchedConceptsForSummaryByRepo :many
-- Repo-wide variant: distinct concept_ids for a repository. Used
-- when SummarizeConceptsArgs.SourceID is empty (manual re-enqueue
-- / periodic catch-up). Same JOIN shape as the source-scoped
-- variant.
SELECT DISTINCT c.id, c.canonical_name, c.context, c.repository_id
FROM okt_repository.concepts c
JOIN okt_repository.fact_concepts fc ON fc.concept_id = c.id
JOIN okt_repository.fact_sources fs ON fs.fact_id = fc.fact_id
JOIN okt_repository.sources s ON fs.source_id = s.id
WHERE c.repository_id = @repository_id
ORDER BY c.canonical_name;

-- name: ListUncoveredFactIDsForSummary :many
-- Fact_ids linked to a concept that are NOT in any of its existing
-- summaries' covered_fact_ids. These are the new facts the worker
-- needs to fold into the open slice (or seed a new one). Ordered
-- by fact_concepts.first_seen_at so the slicing is deterministic
-- (the oldest facts land in the oldest slice).
SELECT fc.fact_id
FROM okt_repository.fact_concepts fc
WHERE fc.concept_id = $1
  AND NOT EXISTS (
      SELECT 1
      FROM okt_repository.concept_summaries cs
      WHERE cs.concept_id = fc.concept_id
        AND fc.fact_id = ANY(cs.covered_fact_ids)
  )
ORDER BY fc.first_seen_at;

-- name: ListAllFactIDsForConcept :many
-- Every fact_id linked to a concept, ordered by first_seen_at.
-- Used to reconstruct the open slice's covered set together with
-- the newly arrived facts when regenerating an open summary.
SELECT fc.fact_id
FROM okt_repository.fact_concepts fc
WHERE fc.concept_id = $1
ORDER BY fc.first_seen_at;

-- name: GetFactsForSummary :many
-- Bulk fetch of fact bodies by id list. Used by the worker to
-- build FactInput rows for the LLM prompt without an N+1 query.
-- ANY() on an empty array matches nothing, which is correct.
SELECT id, text FROM okt_repository.facts
WHERE id = ANY($1::uuid[]);

-- name: ListFactSourcesByFactIDs :many
-- Batched source attribution: one row per (fact, source) for the
-- given fact_id set. Joined with sources.url + parsed_title so the
-- worker can build the "(BBC; J. Smith)" parenthetical for the
-- prompt. Ordered by fact_id, first_seen_at so the Go grouping is
-- deterministic. ANY() on an empty array matches nothing.
SELECT fs.fact_id,
       fs.source_id,
       fs.first_seen_at,
       s.url,
       s.parsed_title
FROM okt_repository.fact_sources fs
JOIN okt_repository.sources s ON s.id = fs.source_id
WHERE fs.fact_id = ANY($1::uuid[])
ORDER BY fs.fact_id, fs.first_seen_at;

-- name: ExistsCompleteSummary :one
-- Whether the concept has at least one frozen (is_complete = TRUE)
-- summary slice. The worker uses this to decide whether to run the
-- incremental open-accumulator path (only while no complete slice
-- exists) or the batch-only path (once the first slice is frozen).
SELECT EXISTS (
    SELECT 1
    FROM okt_repository.concept_summaries
    WHERE concept_id = $1 AND is_complete = TRUE
) AS exists;

-- name: GetOpenSummary :one
-- The single open (is_complete = FALSE) summary for a concept, or
-- no rows when none exists. The unique partial index
-- uq_concept_summaries_concept_open guarantees at most one open
-- summary per concept_id, so this is a scalar lookup.
SELECT * FROM okt_repository.concept_summaries
WHERE concept_id = $1 AND is_complete = FALSE;

-- name: GetMaxSummarySequenceNum :one
-- The highest sequence_num among a concept's summaries, or 0 when
-- the concept has no summaries yet. Used by the worker to pick the
-- next sequence_num when creating a new slice. The cast to int
-- pins the sqlc return type to int32 (without it COALESCE on
-- MAX(int) returns an interface{} in sqlc's Go emitter).
SELECT COALESCE(MAX(sequence_num), 0)::int AS sequence_num
FROM okt_repository.concept_summaries
WHERE concept_id = $1;

-- name: ClaimConceptForSummary :one
-- Atomically acquire the per-concept summarization lock. Returns
-- the concept id on success; returns no rows when another worker
-- already holds a fresh lock (summarizing_at IS NOT NULL AND
-- summarizing_at >= now() - @staleness::interval). A stale lock
-- (summarizing_at older than the staleness window) is reclaimable
-- so a crashed worker doesn't wedge the concept forever. The
-- staleness is passed as a Postgres interval string (e.g. "2h").
UPDATE okt_repository.concepts
SET summarizing_at = now()
WHERE id = @id
  AND (summarizing_at IS NULL OR summarizing_at < now() - @staleness::interval)
RETURNING id;

-- name: ReleaseConceptSummaryLock :exec
-- Release the per-concept summarization lock. Called by the worker
-- in a defer so a panic mid-loop still releases. The lock is
-- best-effort: a query error is logged and swallowed so a failing
-- release doesn't poison the result.
UPDATE okt_repository.concepts
SET summarizing_at = NULL
WHERE id = $1;

-- name: CreateSummary :one
-- Insert a new summary row. The caller passes the explicit
-- sequence_num, is_complete, covered_fact_ids, content, fact_count,
-- and the denormalized context/repository_id. Used for both the
-- "freeze a slice" path (is_complete = TRUE) and the "seed a new
-- open slice" path (is_complete = FALSE).
INSERT INTO okt_repository.concept_summaries (
    concept_id, repository_id, context, sequence_num,
    is_complete, fact_count, content, covered_fact_ids, model
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
RETURNING *;

-- name: UpdateSummary :one
-- Regenerate an existing summary row. Used by the worker to update
-- the open slice as new facts arrive (covered_fact_ids grows,
-- fact_count grows, content is regenerated) and to freeze it
-- (is_complete flips to TRUE) when the covered set reaches
-- BatchSize. updated_at is bumped automatically by the trigger-free
-- DEFAULT now() on the column? No — we set it explicitly so the
-- row carries a meaningful "last regenerated" timestamp even though
-- the column has a DEFAULT; the explicit SET wins.
UPDATE okt_repository.concept_summaries
SET content = $2,
    covered_fact_ids = $3,
    fact_count = $4,
    is_complete = $5,
    model = $6,
    updated_at = now()
WHERE id = $1
RETURNING *;

-- name: ListSummariesByConcept :many
-- Every summary for a concept, ordered by sequence_num so the
-- oldest slice is first. Used by the HTTP read endpoint and by the
-- worker to reconstruct the open slice's covered set.
SELECT * FROM okt_repository.concept_summaries
WHERE concept_id = $1
ORDER BY sequence_num;

-- name: CountSummariesByConcept :one
SELECT COUNT(*) FROM okt_repository.concept_summaries WHERE concept_id = $1;

-- name: CountFactsForConceptSummary :one
-- Total fact count for a concept (over fact_concepts), used by the
-- summarize_concepts worker to select a fact-summary curriculum
-- tier (SummarizationConfig.TierFor). The fact_concepts PRIMARY KEY
-- index on concept_id makes this a fast index-only scan. Distinct
-- from CountFactsByConcept (concepts.sql) which joins facts and
-- accepts a search predicate; this one is the bare count the worker
-- needs to pick a batch_size/max_tokens tier.
SELECT COUNT(*) FROM okt_repository.fact_concepts fc
WHERE fc.concept_id = $1;