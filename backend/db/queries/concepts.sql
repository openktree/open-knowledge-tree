-- concepts.sql — concept extraction Phase 1 queries.
--
-- A concept is a repo-scoped, canonical-named entity assigned a
-- context (an L3 ontology class label). Concepts are produced by
-- the extract_concepts worker over a repo's stable facts. The
-- fact_concepts junction is the end goal: querying a concept returns
-- every fact linked to it.

-- name: CreateConcept :one
-- ON CONFLICT DO NOTHING so a racing extract_concepts pass that
-- inserts the same (repository_id, canonical_name, context) twice
-- doesn't fail; the caller re-queries by the unique key when it
-- needs the id of the survivor. The conflict target matches the
-- uq_concepts_repo_name_context unique index on
-- (repository_id, lower(canonical_name), lower(context)).
-- promptset_hash tags the concept with the philosophy that produced
-- it so downstream queries (synthesis, registry pull) can filter to
-- a single promptset and decompositions from different promptsets do
-- not mix.
INSERT INTO okt_repository.concepts (repository_id, canonical_name, context, description, promptset_hash)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (repository_id, lower(canonical_name), lower(context)) DO NOTHING
RETURNING *;

-- name: GetConceptByID :one
SELECT * FROM okt_repository.concepts WHERE id = $1;

-- name: GetConceptCandidateByID :one
SELECT * FROM okt_repository.concept_candidates WHERE id = $1;

-- name: FindConceptByAlias :one
-- Text-search match scoped by (repository_id, context): matches
-- either a concept's canonical_name OR any of its aliases (both
-- case-insensitive). The context is matched case-insensitively too
-- so a context drift ("Politician" vs "politician") still resolves
-- to the same concept. LIMIT 1 so the first match wins; the caller
-- links the fact to it.
--
-- NOTE: callers that need to disambiguate an alias shared by
-- multiple concepts (e.g. "N" on both Nitrogen and Neutron) should
-- use FindConceptsByAlias (:many) and apply an embedding-distance
-- tie-break per fact. This :one query returns an arbitrary first
-- row on a shared alias and is only safe when the caller does not
-- care about ambiguity (canonical-name-only routing, or a context
-- where aliases are known to be unique).
SELECT c.*
FROM okt_repository.concepts c
WHERE c.repository_id = @repository_id
  AND lower(c.context) = lower(@context)
  AND (
      lower(c.canonical_name) = lower(@name)
      OR EXISTS (
          SELECT 1 FROM okt_repository.concept_aliases a
          WHERE a.concept_id = c.id AND lower(a.alias_text) = lower(@name)
      )
  )
LIMIT 1;

-- name: FindConceptsByAlias :many
-- Same predicate as FindConceptByAlias but returns ALL matches
-- (no LIMIT 1), ordered deterministically by created_at, id so the
-- fallback when no embedding tie-break is possible is stable. The
-- refine_concepts worker uses this to detect alias ambiguity: when
-- more than one concept shares an alias, it routes each of the
-- candidate's facts individually to the concept whose Qdrant vector
-- is cosine-closest to that fact's vector (see
-- internal/concepts.ResolveAliasMatchForFact). 0 or 1 match -> no
-- ambiguity, behave as the legacy single-target merge.
SELECT c.*
FROM okt_repository.concepts c
WHERE c.repository_id = @repository_id
  AND lower(c.context) = lower(@context)
  AND (
      lower(c.canonical_name) = lower(@name)
      OR EXISTS (
          SELECT 1 FROM okt_repository.concept_aliases a
          WHERE a.concept_id = c.id AND lower(a.alias_text) = lower(@name)
      )
  )
ORDER BY c.created_at, c.id;

-- name: ListFactIDsByCandidate :many
-- The fact_ids linked to an unresolved concept_candidate via
-- fact_candidates. Used by refine_concepts' per-fact routing branch
-- to route each fact individually to its cosine-winning concept when
-- an alias matches multiple concepts.
SELECT fact_id
FROM okt_repository.fact_candidates
WHERE candidate_id = @candidate_id
ORDER BY first_seen_at;

-- name: DeleteCandidate :exec
-- Delete a concept_candidate row (and its fact_candidates links via
-- cascade). Used by refine_concepts after per-fact routing when all
-- of a candidate's facts have been routed to real concepts and none
-- were deferred (a deferred fact stays on the candidate, so the
-- candidate is kept unresolved for the next pass). Distinct from
-- ResolveCandidate, which marks the candidate as a resolved cache
-- entry (resolved_concept_id) for single-target merges.
DELETE FROM okt_repository.concept_candidates
WHERE id = @id;

-- name: AddConceptAlias :one
-- Idempotent: re-adding the same alias (e.g. a seed alias from a
-- second fact that extracts the same concept) doesn't fail. The
-- caller uses :one so a conflict is surfaced as pgx.ErrNoRows,
-- which the worker treats as "already present, skip". The conflict
-- target matches the uq_concept_aliases_concept_text unique index
-- on (concept_id, lower(alias_text)).
INSERT INTO okt_repository.concept_aliases (concept_id, alias_text)
VALUES ($1, $2)
ON CONFLICT (concept_id, lower(alias_text)) DO NOTHING
RETURNING *;

-- name: AddFactConcept :one
-- Idempotent junction link. ON CONFLICT DO NOTHING so a re-extract
-- pass that re-links the same (fact, concept) pair is a no-op.
-- promptset_hash tags the link with the philosophy that produced
-- the fact+concept pair so the junction stays queryable by
-- philosophy (a fact and a concept derived under different
-- promptsets must not be joined by a query that filters by hash).
INSERT INTO okt_repository.fact_concepts (fact_id, concept_id, promptset_hash)
VALUES ($1, $2, $3)
ON CONFLICT (fact_id, concept_id) DO NOTHING
RETURNING *;

-- name: ListStableFactsForConceptExtraction :many
-- Stable facts not yet linked to any concept and not exhausted
-- their soft-skip retry budget. Batched by LIMIT so the worker
-- can process a repo in chunks without loading every fact into
-- memory at once. Ordered by created_at so the batch order is
-- stable across runs (a re-enqueue that races a previous pass
-- picks up where the first pass left off).
--
-- A fact is excluded from the candidate set when EITHER:
--   - it already has a fact_concepts row (linked), OR
--   - it has a fact_concept_skips row with attempts >=
--     @max_concept_attempts (permanently given up after N
--     consecutive permanent failures; an operator can clear the
--     skip via POST /api/v1/admin/repos/{id}/concepts/reextract),
--   - it has a fact_candidates row (pending refinement, when
--     refinement is enabled; refine_concepts will route it).
-- Transient LLM failures do NOT write a skip row at all, so the
-- fact stays in the candidate set and is retried on the next
-- pass (the in-job end-of-queue reprocess handles the common
-- case; the admin endpoint handles quiet-repo recovery).
--
-- DISTINCT is critical: a fact may have many fact_sources rows
-- (one per source it was extracted from), and the JOIN shape
-- (facts JOIN fact_sources JOIN sources) returns the same fact
-- once per source. That duplication caused the worker to call
-- the LLM N times for one fact and then hit the fact_concepts
-- unique index on the second insert, surfacing pgx.ErrNoRows as
-- a fatal error. DISTINCT collapses the duplicates; the JOIN
-- remains because facts does not carry repository_id (see
-- 0013_facts.up.sql), so the repo filter must go via sources.
SELECT DISTINCT f.id, f.text, f.created_at
FROM okt_repository.facts f
JOIN okt_repository.fact_sources fs ON fs.fact_id = f.id
JOIN okt_repository.sources s ON fs.source_id = s.id
WHERE s.repository_id = @repository_id
  AND f.status = 'stable'
  AND NOT EXISTS (SELECT 1 FROM okt_repository.fact_concepts fc WHERE fc.fact_id = f.id)
  AND NOT EXISTS (SELECT 1 FROM okt_repository.fact_concept_skips sk
                  WHERE sk.fact_id = f.id AND sk.attempts >= @max_concept_attempts)
  AND NOT EXISTS (SELECT 1 FROM okt_repository.fact_candidates fca WHERE fca.fact_id = f.id)
ORDER BY f.created_at
LIMIT @batch_size;

-- name: ListStableFactsForConceptExtractionBySource :many
-- Source-scoped variant of ListStableFactsForConceptExtraction.
-- Same filters (stable, not yet linked to a concept, not exhausted
-- the soft-skip budget) but additionally constrained to facts
-- linked to THIS source via fact_sources. Used by the source-
-- scoped extract_concepts pass so each source's job only scans
-- its own facts rather than re-scanning the whole repo every time
-- any source completes processing. The fact_sources join is
-- already present for the repo filter (facts has no
-- repository_id), so the source filter rides the same join;
-- DISTINCT collapses any per-source duplication (a fact may have
-- multiple fact_sources rows for the same source across chunks).
SELECT DISTINCT f.id, f.text, f.created_at
FROM okt_repository.facts f
JOIN okt_repository.fact_sources fs ON fs.fact_id = f.id
JOIN okt_repository.sources s ON fs.source_id = s.id
WHERE s.repository_id = @repository_id
  AND fs.source_id = @source_id
  AND f.status = 'stable'
  AND NOT EXISTS (SELECT 1 FROM okt_repository.fact_concepts fc WHERE fc.fact_id = f.id)
  AND NOT EXISTS (SELECT 1 FROM okt_repository.fact_concept_skips sk
                  WHERE sk.fact_id = f.id AND sk.attempts >= @max_concept_attempts)
  AND NOT EXISTS (SELECT 1 FROM okt_repository.fact_candidates fca WHERE fca.fact_id = f.id)
ORDER BY f.created_at
LIMIT @batch_size;

-- name: RecordFactConceptSkip :one
-- Soft-skip with retry budget. Increments `attempts` on each
-- consecutive permanent failure (parse error, empty result, etc.)
-- and refreshes last_error + last_attempt_at. The candidate-
-- selection query excludes the fact only when attempts >=
-- @max_concept_attempts (default 3), so the fact stays retryable
-- until the budget is exhausted. Transient errors (timeout,
-- rate-limit wait, network blip, 5xx, 429) do NOT call this —
-- the fact stays in the candidate set and is retried on the next
-- pass. ON CONFLICT DO UPDATE so re-skipping the same fact
-- increments the counter instead of clobbering it.
INSERT INTO okt_repository.fact_concept_skips (fact_id, last_error, attempts, last_attempt_at)
VALUES ($1, $2, 1, now())
ON CONFLICT (fact_id) DO UPDATE
SET last_error = EXCLUDED.last_error,
    attempts = fact_concept_skips.attempts + 1,
    last_attempt_at = now(),
    skipped_at = now()
RETURNING *;

-- name: GetFactConceptSkip :one
SELECT * FROM okt_repository.fact_concept_skips WHERE fact_id = $1;

-- name: ListFactConceptSkipsByRepo :many
-- For the admin UI / diagnostics: which facts have been skipped
-- for a given repo. facts does not carry repository_id, so we
-- scope via fact_sources/sources (same shape as
-- ListStableFactsForConceptExtraction). DISTINCT in case a fact
-- has multiple source rows.
SELECT DISTINCT sk.fact_id, sk.last_error, sk.skipped_at, sk.attempts, sk.last_attempt_at
FROM okt_repository.fact_concept_skips sk
JOIN okt_repository.facts f ON f.id = sk.fact_id
JOIN okt_repository.fact_sources fs ON fs.fact_id = f.id
JOIN okt_repository.sources s ON fs.source_id = s.id
WHERE s.repository_id = $1
ORDER BY sk.skipped_at DESC;

-- name: ClearRetryableFactConceptSkipsByRepo :execrows
-- Clears soft-skip rows that are still within the retry budget
-- (attempts < @max_concept_attempts) for one repo. Used by the
-- admin endpoint POST /api/v1/admin/repos/{id}/concepts/reextract
-- so the next extract_concepts pass re-attempts those facts.
-- Skips with attempts >= @max_concept_attempts are kept (the
-- budget is exhausted; an operator must DELETE them directly if
-- they want to retry a permanently-skipped fact). The fact_id set
-- is scoped via fact_sources/sources because facts has no
-- repository_id.
DELETE FROM okt_repository.fact_concept_skips sk
WHERE sk.attempts < @max_concept_attempts
  AND sk.fact_id IN (
    SELECT f.id
    FROM okt_repository.facts f
    JOIN okt_repository.fact_sources fs ON fs.fact_id = f.id
    JOIN okt_repository.sources s ON fs.source_id = s.id
    WHERE s.repository_id = @repository_id
  );

-- name: ClearUnresolvedFactCandidatesByRepo :execrows
-- Clears fact_candidates rows that point at unresolved
-- concept_candidates (resolved_concept_id IS NULL) for one repo.
-- Used by the admin reextract endpoint to un-strand Mode 5 facts
-- (stuck on a candidate that refine_concepts never routed). After
-- the clear, extract_concepts re-visits the fact
-- (ListStableFactsForConceptExtraction's NOT EXISTS fact_candidates
-- no longer excludes it), re-emits a candidate, and refine_concepts
-- gets a fresh chance to route it.
DELETE FROM okt_repository.fact_candidates fca
WHERE fca.candidate_id IN (
    SELECT c.id FROM okt_repository.concept_candidates c
    WHERE c.repository_id = @repository_id AND c.resolved_concept_id IS NULL
  )
  AND fca.fact_id IN (
    SELECT f.id
    FROM okt_repository.facts f
    JOIN okt_repository.fact_sources fs ON fs.fact_id = f.id
    JOIN okt_repository.sources s ON fs.source_id = s.id
    WHERE s.repository_id = @repository_id
  );

-- name: CountUnlinkedStableFactsByRepo :one
-- Counts stable facts that have no fact_concepts row and no
-- exhausted skip row (attempts < @max_concept_attempts). These
-- are the facts the next extract_concepts pass will pick up —
-- the "in scope" count for the admin reextract preview endpoint.
SELECT COUNT(*)::bigint FROM okt_repository.facts f
JOIN okt_repository.fact_sources fs ON fs.fact_id = f.id
JOIN okt_repository.sources s ON fs.source_id = s.id
WHERE s.repository_id = @repository_id
  AND f.status = 'stable'
  AND NOT EXISTS (SELECT 1 FROM okt_repository.fact_concepts fc WHERE fc.fact_id = f.id)
  AND NOT EXISTS (SELECT 1 FROM okt_repository.fact_concept_skips sk
                  WHERE sk.fact_id = f.id AND sk.attempts >= @max_concept_attempts);

-- name: ListSourcesWithUnlinkedFactsByRepo :many
-- Distinct source_ids in a repo that have at least one stable
-- unlinked fact (no fact_concepts, no exhausted skip). The admin
-- reextract endpoint enqueues one extract_concepts job per source
-- (matching the normal deduplicate_facts → extract_concepts chain)
-- so each source's facts are processed independently and the
-- queue can parallelize across sources.
SELECT DISTINCT s.id
FROM okt_repository.facts f
JOIN okt_repository.fact_sources fs ON fs.fact_id = f.id
JOIN okt_repository.sources s ON fs.source_id = s.id
WHERE s.repository_id = @repository_id
  AND f.status = 'stable'
  AND NOT EXISTS (SELECT 1 FROM okt_repository.fact_concepts fc WHERE fc.fact_id = f.id)
  AND NOT EXISTS (SELECT 1 FROM okt_repository.fact_concept_skips sk
                  WHERE sk.fact_id = f.id AND sk.attempts >= @max_concept_attempts);

-- name: CountRetryableSkipsByRepo :one
-- Counts fact_concept_skips rows with attempts < @max_concept_attempts
-- for the repo. These are the rows ClearRetryableFactConceptSkipsByRepo
-- will DELETE. Scopes via fact_sources/sources (facts has no repository_id).
SELECT COUNT(*)::bigint
FROM okt_repository.fact_concept_skips sk
JOIN okt_repository.facts f ON f.id = sk.fact_id
JOIN okt_repository.fact_sources fs ON fs.fact_id = f.id
JOIN okt_repository.sources s ON fs.source_id = s.id
WHERE s.repository_id = @repository_id
  AND sk.attempts < @max_concept_attempts;

-- name: CountUnresolvedCandidatesByRepo :one
-- Counts unresolved concept_candidates (resolved_concept_id IS NULL)
-- for the repo. These are the candidates whose fact_candidates rows
-- ClearUnresolvedFactCandidatesByRepo will DELETE.
SELECT COUNT(*)::bigint
FROM okt_repository.concept_candidates c
WHERE c.repository_id = @repository_id
  AND c.resolved_concept_id IS NULL;

-- name: ListConceptsByRepo :many
-- Paginated concept list with a computed fact_count so the UI can
-- sort by "most linked" and the user sees the most relevant
-- concepts first. The subquery is correlated but cheap: the
-- fact_concepts PK index makes the count an index-only scan.
--
-- NOTE: returns one row per (canonical_name, context) pair. The
-- HTTP list endpoint groups these into one entry per canonical
-- name via ListGroupedConceptsByRepo instead. This query is kept
-- for backward compatibility and direct store use; the handler no
-- longer calls it.
SELECT c.*,
       (SELECT COUNT(*) FROM okt_repository.fact_concepts fc WHERE fc.concept_id = c.id) AS fact_count
FROM okt_repository.concepts c
WHERE c.repository_id = $1
ORDER BY fact_count DESC, c.canonical_name ASC
LIMIT $2 OFFSET $3;

-- name: CountConceptsByRepo :one
SELECT COUNT(*) FROM okt_repository.concepts WHERE repository_id = $1;

-- name: ListGroupedConceptsByRepo :many
-- All per-context concept rows for a repo, with a computed
-- fact_count, intended to be grouped in Go by lower(canonical_name)
-- so the API can present "one concept, many contexts". Returns the
-- full set (no SQL-level pagination); the caller paginates the
-- resulting groups in Go. Optional @q runs a weighted full-text
-- search (websearch_to_tsquery, 'english' config) over the
-- concept's canonical_name (weight A) + description (weight D) and
-- over all of its concept_aliases.alias_text (weight A), via the
-- GIN expression indexes from migration 0058. An alias hit pulls in
-- the parent concept's whole canonical-name group.
--
-- search_rank is the ts_rank_cd of the best-matching tsv across the
-- concept's own weighted tsv and the MAX of its aliases' weighted
-- tsvs (0.0 when @q is empty or no rows match). It is consumed by
-- concepts.BuildGroups to order groups: search_rank DESC first (so
-- name/alias hits rank above description-only hits, since A > D),
-- then the pre-existing fact_count DESC, canonical_name ASC tie-
-- break. The group's search_rank is the MAX search_rank across its
-- contexts (a hit on any context ranks the whole group). Grouping
-- is by lower(canonical_name) in Go; the slug column was removed in
-- migration 0030.
SELECT c.id,
       c.repository_id,
       c.canonical_name,
       c.context,
       c.description,
       c.embedded_at,
       c.embedded_model,
       c.created_at,
       (SELECT COUNT(*) FROM okt_repository.fact_concepts fc WHERE fc.concept_id = c.id) AS fact_count,
       CASE WHEN @q::text = '' THEN 0.0::real
            ELSE COALESCE(
                ts_rank_cd(
                    setweight(to_tsvector('english', coalesce(c.canonical_name, '')), 'A')
                    || setweight(to_tsvector('english', coalesce(c.description, '')), 'D'),
                    websearch_to_tsquery('english', @q::text)
                ),
                0.0::real
            )
       END::real AS name_rank,
       (SELECT COALESCE(MAX(ts_rank_cd(
                          setweight(to_tsvector('english', coalesce(a.alias_text, '')), 'A'),
                          websearch_to_tsquery('english', @q::text)
                      )), 0.0::real)::real
          FROM okt_repository.concept_aliases a
         WHERE a.concept_id = c.id) AS alias_rank
FROM okt_repository.concepts c
WHERE c.repository_id = @repository_id
  AND (
      @q::text = ''
      OR (setweight(to_tsvector('english', coalesce(c.canonical_name, '')), 'A')
          || setweight(to_tsvector('english', coalesce(c.description, '')), 'D'))
          @@ websearch_to_tsquery('english', @q::text)
      OR EXISTS (
          SELECT 1 FROM okt_repository.concept_aliases a
          WHERE a.concept_id = c.id
            AND setweight(to_tsvector('english', coalesce(a.alias_text, '')), 'A')
                @@ websearch_to_tsquery('english', @q::text)
      )
  )
ORDER BY fact_count DESC, c.canonical_name ASC;

-- name: CountGroupedConceptsByRepo :one
-- Number of distinct canonical-name groups in a repo (one entry
-- per lower(canonical_name)). Optional @q runs the same weighted
-- full-text search as ListGroupedConceptsByRepo over canonical_name
-- (A) + description (D) and concept_aliases.alias_text (A).
SELECT COUNT(DISTINCT lower(c.canonical_name))
FROM okt_repository.concepts c
WHERE c.repository_id = @repository_id
  AND (
      @q::text = ''
      OR (setweight(to_tsvector('english', coalesce(c.canonical_name, '')), 'A')
          || setweight(to_tsvector('english', coalesce(c.description, '')), 'D'))
          @@ websearch_to_tsquery('english', @q::text)
      OR EXISTS (
          SELECT 1 FROM okt_repository.concept_aliases a
          WHERE a.concept_id = c.id
            AND setweight(to_tsvector('english', coalesce(a.alias_text, '')), 'A')
                @@ websearch_to_tsquery('english', @q::text)
      )
  );

-- name: GetConceptsByIDsForSearch :many
-- Fetches the ListGroupedConceptsByRepo-shaped row (with the
-- per-context fact_count) for an arbitrary set of concept ids in a
-- single round-trip. Used by the hybrid search path: the lexical
-- channel over-fetches its own rows, the semantic channel (Qdrant
-- concept collection) returns only ids, and this query fills in
-- the rows for any semantic-only ids the lexical batch didn't
-- already return. Name/alias ranks are zero (this query is not a
-- ranked search; the fused ranking is applied in Go via the
-- caller-supplied score map). No ordering is applied here — the
-- caller re-orders to match the fused ranking. Repository scoping
-- is enforced so a cross-repo id in the input set is silently
-- dropped.
SELECT c.id,
       c.repository_id,
       c.canonical_name,
       c.context,
       c.description,
       c.embedded_at,
       c.embedded_model,
       c.created_at,
       (SELECT COUNT(*) FROM okt_repository.fact_concepts fc WHERE fc.concept_id = c.id) AS fact_count,
       0.0::real AS name_rank,
       0.0::real AS alias_rank
FROM okt_repository.concepts c
WHERE c.id = ANY(@ids::uuid[])
  AND c.repository_id = @repository_id;

-- name: ListConceptsByRepoName :many
-- Group lookup by lower(canonical_name): the primary detail-endpoint
-- path. The handler resolves a concept UUID to its canonical_name
-- (via GetConceptByID), then calls this to fetch the whole group
-- (every per-context row sharing the lower(canonical_name)) in one
-- indexed equality lookup. Ordered by fact_count DESC so the first
-- row is the highest-fact_count context (the group representative).
SELECT c.id,
       c.repository_id,
       c.canonical_name,
       c.context,
       c.description,
       c.embedded_at,
       c.embedded_model,
       c.created_at,
       (SELECT COUNT(*) FROM okt_repository.fact_concepts fc WHERE fc.concept_id = c.id) AS fact_count
FROM okt_repository.concepts c
WHERE c.repository_id = $1 AND lower(c.canonical_name) = lower(sqlc.arg('canonical_name'))
ORDER BY fact_count DESC, c.context ASC;

-- name: ListConceptAliasesByConceptIDs :many
-- Aliases for a set of concept_ids in one query, returned
-- un-grouped (the caller groups by concept_id in Go). Used by the
-- detail endpoint to fetch every context's aliases in a single
-- round-trip instead of N per-context ListConceptAliasesByConcept
-- calls. Ordered by concept_id, alias_text so the Go grouping is
-- deterministic. ANY() on an empty array matches nothing, which is
-- correct (a group with no contexts returns no aliases).
SELECT ca.id,
       ca.concept_id,
       ca.alias_text,
       ca.created_at
FROM okt_repository.concept_aliases ca
WHERE ca.concept_id = ANY($1::uuid[])
ORDER BY ca.concept_id, ca.alias_text;

-- name: ListInvestigationConcepts :many
-- Investigation-scoped concept list. Concepts are reached via
-- fact_concepts → fact_sources → investigation_sources, so only
-- concepts derived from facts that came from the investigation's
-- own sources are returned (a brand-new investigation with no
-- sources returns nothing). fact_count is computed over ALL facts
-- linked to the concept (repo-wide), mirroring ListConceptsByRepo
-- so cross-confirmation still shows; the membership filter is only
-- on which concepts appear, not on the count. Paginated, ordered by
-- fact_count DESC then canonical_name ASC, mirroring the repo list.
--
-- NOTE: returns one row per (canonical_name, context) pair. The
-- HTTP investigation-concepts endpoint groups these by
-- lower(canonical_name) via ListGroupedInvestigationConcepts
-- instead. This query is kept for backward compatibility and direct
-- store use; the handler no longer calls it.
SELECT DISTINCT c.*,
        (SELECT COUNT(*) FROM okt_repository.fact_concepts fc WHERE fc.concept_id = c.id) AS fact_count
FROM okt_repository.concepts c
JOIN okt_repository.fact_concepts fcon ON fcon.concept_id = c.id
JOIN okt_repository.fact_sources fs ON fs.fact_id = fcon.fact_id
JOIN okt_repository.investigation_sources is_join ON is_join.source_id = fs.source_id
WHERE is_join.investigation_id = $1
ORDER BY fact_count DESC, c.canonical_name ASC
LIMIT $2 OFFSET $3;

-- name: CountInvestigationConcepts :one
-- Companion count for ListInvestigationConcepts. Counts DISTINCT
-- concepts reachable from the investigation's sources.
SELECT COUNT(DISTINCT c.id)
FROM okt_repository.concepts c
JOIN okt_repository.fact_concepts fcon ON fcon.concept_id = c.id
JOIN okt_repository.fact_sources fs ON fs.fact_id = fcon.fact_id
JOIN okt_repository.investigation_sources is_join ON is_join.source_id = fs.source_id
WHERE is_join.investigation_id = $1;

-- name: ListGroupedInvestigationConcepts :many
-- All per-context concept rows reachable from an investigation's
-- sources, with computed fact_count, intended to be grouped in Go
-- by lower(canonical_name). Returns the full set (no SQL-level
-- pagination); the caller paginates the resulting groups in Go.
-- Mirrors ListGroupedConceptsByRepo but scoped via
-- fact_concepts → fact_sources → investigation_sources. fact_count
-- is repo-wide (cross-confirmation still shows); the membership
-- filter is only on which concepts appear. Ordered by fact_count
-- DESC, canonical_name ASC so the in-Go group representative pick
-- is the highest-fact_count context.
SELECT DISTINCT c.id,
       c.repository_id,
       c.canonical_name,
       c.context,
       c.description,
       c.embedded_at,
       c.embedded_model,
       c.created_at,
       (SELECT COUNT(*) FROM okt_repository.fact_concepts fc WHERE fc.concept_id = c.id) AS fact_count
FROM okt_repository.concepts c
JOIN okt_repository.fact_concepts fcon ON fcon.concept_id = c.id
JOIN okt_repository.fact_sources fs ON fs.fact_id = fcon.fact_id
JOIN okt_repository.investigation_sources is_join ON is_join.source_id = fs.source_id
WHERE is_join.investigation_id = @investigation_id
ORDER BY fact_count DESC, c.canonical_name ASC;

-- name: CountGroupedInvestigationConcepts :one
-- Number of distinct canonical-name groups reachable from an
-- investigation's sources. Mirrors CountGroupedConceptsByRepo but
-- scoped via the investigation_sources join.
SELECT COUNT(DISTINCT lower(c.canonical_name))
FROM okt_repository.concepts c
JOIN okt_repository.fact_concepts fcon ON fcon.concept_id = c.id
JOIN okt_repository.fact_sources fs ON fs.fact_id = fcon.fact_id
JOIN okt_repository.investigation_sources is_join ON is_join.source_id = fs.source_id
WHERE is_join.investigation_id = @investigation_id;

-- name: ListFactsByConcept :many
-- The "query DNA → 200 facts" view. Paginated; the caller passes
-- the concept_id (resolved by the caller from the route param).
-- The optional search ($2) runs through websearch_to_tsquery against
-- facts.search_tsv (covers facts.text); empty string = no filter.
-- LIMIT $4 / OFFSET $5 apply on top of the ORDER BY.
--
-- The optional sort ($3) mirrors ListFactsByRepoWithSourceCount:
--   - 'created_at': newest facts first (the prior behavior when this
--     endpoint ordered by fc.first_seen_at — link-insertion order,
--     which was essentially ingest order, not fact recency; the new
--     'created_at' branch orders by the fact's own created_at, a
--     more meaningful "newest fact" signal).
--   - 'source_count' (default when $3 is empty or any other value):
--     most-confirmed facts first. A fact that survived dedup from N
--     independent sources is more load-bearing than a single-source
--     paraphrase — the same signal the repo-wide /facts endpoint
--     defaults to. This lifts multi-source answering facts (e.g. the
--     SBF "facing seven counts..." fact with source_count=2) out of
--     the middle of large concept fact pools where first_seen_at
--     ordering had buried them.
--
-- ts_rank(f.search_tsv, $2) is added as a tiebreaker after
-- source_count so that, among equally-confirmed facts, the ones
-- whose text most closely matches the query rank first. The rank
-- is 0 for all rows when $2 is empty, so the tiebreaker is a no-op
-- in the unfiltered case. fc.first_seen_at is the final
-- deterministic tiebreaker so pagination stays stable across pages.
--
-- The source_count is computed via a LEFT JOIN on a grouped
-- fact_sources subquery (mirrors ListFactsByRepoWithSourceCount).
SELECT f.*, fc.first_seen_at,
       COALESCE(fs_count.source_count, 0) AS source_count
FROM okt_repository.fact_concepts fc
JOIN okt_repository.facts f ON f.id = fc.fact_id
LEFT JOIN (
    SELECT fs2.fact_id, COUNT(*) AS source_count
    FROM okt_repository.fact_sources fs2
    JOIN okt_repository.sources s2 ON s2.id = fs2.source_id
    JOIN okt_repository.concepts c2 ON c2.id = $1
    WHERE s2.repository_id = c2.repository_id
    GROUP BY fs2.fact_id
) fs_count ON fs_count.fact_id = f.id
WHERE fc.concept_id = $1
  AND ($2::text = '' OR f.search_tsv @@ websearch_to_tsquery('english', $2))
ORDER BY
    -- created_at branch: newest facts first.
    CASE WHEN $3 = 'created_at' THEN f.created_at END DESC,
    -- source_count branch (default): most-confirmed facts first.
    CASE WHEN $3 != 'created_at' THEN COALESCE(fs_count.source_count, 0) END DESC,
    -- ts_rank tiebreaker (only when q is non-empty and not in
    -- created_at mode). 0 for all rows when q is empty, so it's a
    -- no-op in the unfiltered case.
    CASE WHEN $3 != 'created_at' AND $2::text != ''
         THEN ts_rank(f.search_tsv, websearch_to_tsquery('english', $2))
    END DESC,
    -- Final deterministic tiebreaker so pagination stays stable.
    fc.first_seen_at
LIMIT $4 OFFSET $5;

-- name: CountFactsByConcept :one
-- Companion count for ListFactsByConcept. Same concept_id and
-- search predicates, minus the ORDER BY / LIMIT / OFFSET, so the
-- API envelope can report `total` without a window function.
SELECT COUNT(*) FROM okt_repository.fact_concepts fc
JOIN okt_repository.facts f ON f.id = fc.fact_id
WHERE fc.concept_id = $1
  AND ($2::text = '' OR f.search_tsv @@ websearch_to_tsquery('english', $2));

-- name: ListSourcesByConcept :many
-- Unique sources backing the facts linked to a single concept
-- context (per-context, scoped by concept_id), with the number of
-- distinct facts each source supports (fact_count) as a provenance
-- signal. Ordered by fact_count DESC (most load-bearing source
-- first), then first_seen_at DESC (most recent addition first) with
-- the source id as the final deterministic tiebreaker so pagination
-- stays stable across pages. Used by the REST endpoint
-- GET /concepts/{conceptID}/sources and the UI Sources section.
SELECT s.id, s.url, s.doi, s.parsed_title, s.parsed_sitename,
       s.parsed_author, s.published_at,
       COUNT(DISTINCT fs.fact_id) AS fact_count,
       MIN(fs.first_seen_at)     AS src_first_seen_at
FROM okt_repository.fact_concepts fc
JOIN okt_repository.fact_sources fs ON fs.fact_id = fc.fact_id
JOIN okt_repository.sources s      ON s.id = fs.source_id
WHERE fc.concept_id = $1
GROUP BY s.id, s.url, s.doi, s.parsed_title, s.parsed_sitename,
         s.parsed_author, s.published_at
ORDER BY fact_count DESC, src_first_seen_at DESC, s.id
LIMIT $2 OFFSET $3;

-- name: CountSourcesByConcept :one
-- Companion count for ListSourcesByConcept. Same concept_id filter,
-- minus the ORDER BY / LIMIT / OFFSET, so the API envelope can report
-- `total` without a window function.
SELECT COUNT(DISTINCT s.id)
FROM okt_repository.fact_concepts fc
JOIN okt_repository.fact_sources fs ON fs.fact_id = fc.fact_id
JOIN okt_repository.sources s      ON s.id = fs.source_id
WHERE fc.concept_id = $1;

-- name: ListSourcesByConceptIDs :many
-- Group-level variant of ListSourcesByConcept: accepts an array of
-- concept_ids (the whole group sharing a canonical name, optionally
-- narrowed by a context filter) and aggregates unique sources across
-- ALL of them, summing the fact_count across every matching context.
-- Used by the MCP getConceptSources tool so an agent can query the
-- overall provenance for a concept (UUID or canonical name) in one
-- call. Same select list, ordering, and pagination as the per-context
-- query.
SELECT s.id, s.url, s.doi, s.parsed_title, s.parsed_sitename,
       s.parsed_author, s.published_at,
       COUNT(DISTINCT fs.fact_id) AS fact_count,
       MIN(fs.first_seen_at)     AS src_first_seen_at
FROM okt_repository.fact_concepts fc
JOIN okt_repository.fact_sources fs ON fs.fact_id = fc.fact_id
JOIN okt_repository.sources s      ON s.id = fs.source_id
WHERE fc.concept_id = ANY($1::uuid[])
GROUP BY s.id, s.url, s.doi, s.parsed_title, s.parsed_sitename,
         s.parsed_author, s.published_at
ORDER BY fact_count DESC, src_first_seen_at DESC, s.id
LIMIT $2 OFFSET $3;

-- name: CountSourcesByConceptIDs :one
-- Companion count for ListSourcesByConceptIDs. Same concept-id array
-- filter, minus the ORDER BY / LIMIT / OFFSET.
SELECT COUNT(DISTINCT s.id)
FROM okt_repository.fact_concepts fc
JOIN okt_repository.fact_sources fs ON fs.fact_id = fc.fact_id
JOIN okt_repository.sources s      ON s.id = fs.source_id
WHERE fc.concept_id = ANY($1::uuid[]);

-- name: ListConceptsByFact :many
-- The inverse view: which concepts a fact links to. Used by the
-- fact detail page to show the concept tags attached to a fact.
SELECT c.*
FROM okt_repository.fact_concepts fc
JOIN okt_repository.concepts c ON c.id = fc.concept_id
WHERE fc.fact_id = $1
ORDER BY c.canonical_name;

-- name: ListConceptAliasesByConcept :many
SELECT * FROM okt_repository.concept_aliases WHERE concept_id = $1 ORDER BY alias_text;

-- name: ListNewConceptsForEmbedding :many
-- Concepts with embedded_at IS NULL, scoped to a repository. The
-- embed_concepts worker bulk-embeds these (canonical_name + " " +
-- context) into the okt_concepts Qdrant collection.
SELECT id, canonical_name, context FROM okt_repository.concepts
WHERE repository_id = $1 AND embedded_at IS NULL
ORDER BY created_at;

-- name: ListNewConceptsForEmbeddingBySource :many
-- Source-scoped variant of ListNewConceptsForEmbedding. Concepts
-- have no direct source link; they reach sources via
-- fact_concepts → fact_sources. This selects unembedded concepts
-- that are linked to at least one fact linked to THIS source.
-- DISTINCT is required because a concept may be linked to multiple
-- facts, and a fact to multiple sources, so the join expands. A
-- concept already embedded by another source's earlier pass has
-- embedded_at set and is excluded by the IS NULL filter, so the
-- two passes are idempotent.
--
-- created_at is selected (not just used in ORDER BY) because
-- Postgres requires ORDER BY expressions to appear in the select
-- list of a DISTINCT query (SQLSTATE 42P10 otherwise). The repo-wide
-- ListNewConceptsForEmbedding query does the same.
SELECT DISTINCT c.id, c.canonical_name, c.context, c.created_at
FROM okt_repository.concepts c
JOIN okt_repository.fact_concepts fc ON fc.concept_id = c.id
JOIN okt_repository.fact_sources fs ON fs.fact_id = fc.fact_id
WHERE c.repository_id = $1
  AND fs.source_id = $2
  AND c.embedded_at IS NULL
ORDER BY c.created_at;

-- name: MarkConceptEmbedded :one
UPDATE okt_repository.concepts
SET embedded_at = now(),
    embedded_model = $2
WHERE id = $1
RETURNING *;

-- name: ListConceptRelationsByConceptName :many
-- Paginated list of concepts related to the concept group identified
-- by its lower(canonical_name), ordered by relation strength
-- (shared_fact_count DESC) then by the related name ASC for stable
-- pagination. A "relation" is the set of facts linked to both concept
-- groups; shared_fact_count is the distinct count of those shared
-- facts (deduped per fact, not per source — a fact confirmed by N
-- sources counts once).
--
-- Served from the okt_repository.concept_relations materialized view,
-- which stores ordered pairs (lower(name_a) < lower(name_b)) with
-- self-pairs excluded. A query for a concept's name therefore matches
-- either column and projects the OTHER name as `other_name`. The view
-- is refreshed concurrently by the refresh_concept_relations task
-- (enqueued at the end of each extract_concepts batch and on a
-- periodic schedule), so reads are a single index range scan immune
-- to parallel load.
--
-- canonical_name is joined from concepts (the related name groups one
-- or more per-context rows; MAX(canonical_name) picks a stable
-- representative — casing may vary across sibling rows, e.g. "Trump"
-- vs "trump", so we pick the lexicographic max deterministically
-- rather than relying on every sibling sharing the exact casing).
-- MIN(c.id) picks a representative concept_id for the related group
-- so the UI can build a detail-page link without a second lookup.
-- A repo with no relations for the name returns no rows.
SELECT other_name,
       MAX(c.canonical_name) AS canonical_name,
       MIN(c.id::text) AS concept_id,
       shared_fact_count
FROM (
    SELECT cr.name_b AS other_name, cr.shared_fact_count AS shared_fact_count
    FROM okt_repository.concept_relations cr
    WHERE cr.repository_id = $1 AND cr.name_a = lower($2)
    UNION ALL
    SELECT cr.name_a AS other_name, cr.shared_fact_count AS shared_fact_count
    FROM okt_repository.concept_relations cr
    WHERE cr.repository_id = $1 AND cr.name_b = lower($2)
) r
JOIN okt_repository.concepts c
  ON c.repository_id = $1 AND lower(c.canonical_name) = r.other_name
GROUP BY other_name, shared_fact_count
ORDER BY shared_fact_count DESC, other_name ASC
LIMIT $3 OFFSET $4;

-- name: CountConceptRelationsByConceptName :one
-- Companion count for ListConceptRelationsByConceptName: the number of
-- distinct related names for the given concept group, regardless of
-- which column of the matview the pair sits in. Pure index count on
-- the matview (no join to concepts), so it's cheap even for a concept
-- with thousands of relations.
SELECT COUNT(*) FROM (
    SELECT cr.name_b AS other_name
    FROM okt_repository.concept_relations cr
    WHERE cr.repository_id = $1 AND cr.name_a = lower($2)
    UNION
    SELECT cr.name_a AS other_name
    FROM okt_repository.concept_relations cr
    WHERE cr.repository_id = $1 AND cr.name_b = lower($2)
) r;

-- name: ListConceptRelationDetailsByConceptName :many
-- Relation details for a specific pair of concept names (A, B): one
-- row per CONTEXT of A, with shared_fact_count aggregated across all
-- of B's contexts. This is the per-context breakdown the user wanted
-- for drilling into how two concepts relate (e.g. Trump-as-Politician
-- shares N facts with X, aggregated across every context of X).
--
-- Unlike the list/count queries (which read the matview), this is a
-- LIVE query against fact_concepts. Rationale: the details endpoint
-- is on-demand for a specific pair (low volume) and benefits from
-- freshness (a just-extracted shared fact shows up immediately without
-- waiting for the matview refresh). The cost is bounded by A's fact
-- count × concepts-per-fact and the pair filter keeps the working set
-- small. fact_ids is the array of shared fact_ids (one row per shared
-- fact) so the UI can offer a "view shared facts" drill-down; it's
-- gathered via array_agg and capped by A's context fact count, which
-- is naturally bounded.
SELECT ca.context,
       COUNT(DISTINCT fc1.fact_id) AS shared_fact_count,
       ARRAY_AGG(DISTINCT fc1.fact_id) AS fact_ids
FROM okt_repository.fact_concepts fc1
JOIN okt_repository.concepts ca ON ca.id = fc1.concept_id
JOIN okt_repository.fact_concepts fc2 ON fc2.fact_id = fc1.fact_id
JOIN okt_repository.concepts cb ON cb.id = fc2.concept_id
WHERE ca.repository_id = $1
  AND cb.repository_id = $1
  AND lower(ca.canonical_name) = lower($2)
  AND lower(cb.canonical_name) = lower($3)
GROUP BY ca.context
ORDER BY shared_fact_count DESC, ca.context ASC;

-- name: GetConceptByNameContext :one
-- Lookup a concept by (repository_id, canonical_name, context),
-- case-insensitively on both name and context. Used by the
-- migrate_context worker to decide whether a target (name,
-- new_context) already exists (merge path) or whether a plain
-- UPDATE is safe (reassign path). Matches the
-- uq_concepts_repo_name_context unique index target.
SELECT * FROM okt_repository.concepts
WHERE repository_id = @repository_id
  AND lower(canonical_name) = lower(@canonical_name)
  AND lower(context) = lower(@context)
LIMIT 1;

-- name: ListConceptsByContext :many
-- Every concept row in a repo assigned to a given context (case-
-- insensitive). The migrate_context worker iterates these to merge
-- or reassign each into the target context. Ordered by id for a
-- stable merge order (deterministic survivor selection).
SELECT * FROM okt_repository.concepts
WHERE repository_id = @repository_id AND lower(context) = lower(@context)
ORDER BY id;

-- name: CountConceptsByContext :one
-- The settings DELETE endpoint uses this to refuse deleting a
-- context that still has concepts (the admin must migrate first).
SELECT COUNT(*) FROM okt_repository.concepts
WHERE repository_id = @repository_id AND lower(context) = lower(@context);

-- name: ReassignFactConceptsToConcept :exec
-- Re-link every fact_concepts row pointing at old_concept_id to
-- new_concept_id, ignoring (fact_id, new_concept_id) pairs that
-- already exist (ON CONFLICT DO NOTHING). Used by the migrate_context
-- merge path before deleting the old concept row. Preserves the
-- promptset_hash of each link so a merge does not silently drop the
-- philosophy tag.
INSERT INTO okt_repository.fact_concepts (fact_id, concept_id, first_seen_at, promptset_hash)
SELECT fc.fact_id, @new_concept_id, fc.first_seen_at, fc.promptset_hash
FROM okt_repository.fact_concepts fc
WHERE fc.concept_id = @old_concept_id
ON CONFLICT (fact_id, concept_id) DO NOTHING;

-- name: DeleteConceptByID :exec
DELETE FROM okt_repository.concepts WHERE id = @id;

-- name: UpdateConceptContext :exec
-- Reassign a single concept's context (the migrate_context path
-- where no target (name, new_context) exists, so a plain UPDATE is
-- safe). The unique index on (repo, lower(name), lower(context))
-- guarantees no conflict at this point because the caller already
-- checked GetConceptByNameContext returned no rows.
UPDATE okt_repository.concepts
SET context = @context
WHERE id = @id;

-- name: ResetConceptEmbedding :exec
-- Force re-embed a concept after its fact set or context changed
-- (the merge path of migrate_context re-links facts onto a target
-- concept, so the target's vector is stale until re-embedded). Sets
-- embedded_at = NULL so the embed_concepts worker picks it up.
UPDATE okt_repository.concepts
SET embedded_at = NULL
WHERE id = @id;

-- name: RelinkFactConcepts :exec
-- Copy a loser fact's concept links onto the winner before the
-- loser is deleted (ON DELETE CASCADE would otherwise drop them).
-- ON CONFLICT DO NOTHING preserves the winner's existing links.
-- Called by the dedup worker's mergeSources, alongside
-- RelinkFactReferences, so a dedup merge preserves all concept
-- mappings from both the winner and the loser. Preserves the
-- promptset_hash of each link so a dedup merge does not silently
-- drop the philosophy tag.
INSERT INTO okt_repository.fact_concepts (fact_id, concept_id, first_seen_at, promptset_hash)
SELECT @winner_id, fc.concept_id, fc.first_seen_at, fc.promptset_hash
FROM okt_repository.fact_concepts fc
WHERE fc.fact_id = @loser_id
ON CONFLICT (fact_id, concept_id) DO NOTHING;

-- name: ResetConceptEmbeddingForReembed :exec
-- Batch version of ResetConceptEmbedding for the CacheReconciler's
-- re-embed path. Clears embedded_at + embedded_model on a set of
-- concepts so the embed_concepts worker (which filters
-- embedded_at IS NULL) re-embeds them with the local model. The
-- Qdrant points for these concepts are deleted separately by the
-- caller (now possible because Qdrant uses local concept UUIDs,
-- not remote registry UUIDs).
UPDATE okt_repository.concepts
SET embedded_at = NULL,
    embedded_model = NULL
WHERE id = ANY($1::uuid[]);

-- name: FindConceptByCanonical :one
-- Same-context canonical name match (case-insensitive). Used by
-- refine_concepts to route a candidate to an existing concept by
-- its canonical name. Mirrors FindConceptByAlias but matches only
-- canonical_name, not aliases.
SELECT * FROM okt_repository.concepts
WHERE repository_id = @repository_id
  AND lower(context) = lower(@context)
  AND lower(canonical_name) = lower(@name)
LIMIT 1;

-- name: UpdateConceptCanonical :exec
-- Rename a concept's canonical_name. Used by refine_concepts when
-- the AI proposes a different canonical than the extracted text
-- (step 3c). The advisory lock held by the worker prevents a
-- parallel refinement from racing on the same canonical.
UPDATE okt_repository.concepts
SET canonical_name = @canonical_name
WHERE id = @id;

-- name: SetConceptRefinedAt :exec
-- Mark a concept as refined at the current time so refine_concepts
-- skips it on future passes. Used when promoting a candidate to a
-- new concept and when importing pre-refined concepts from the
-- registry.
UPDATE okt_repository.concepts
SET aliases_refined_at = now()
WHERE id = @id;

-- name: DeleteConceptAliasByText :exec
-- Prune a specific alias from a concept (case-insensitive). Used
-- by refine_concepts to apply aliases_to_prune from the LLM.
DELETE FROM okt_repository.concept_aliases
WHERE concept_id = @concept_id
  AND lower(alias_text) = lower(@alias_text);

-- name: CountAliasesSinceRefinement :one
-- Count aliases added since the last refinement. Used by the
-- pruning gate: re-prune only when >= X new aliases have
-- accumulated since aliases_refined_at.
SELECT COUNT(*) FROM okt_repository.concept_aliases
WHERE concept_id = @concept_id
  AND @refined_at::timestamptz IS NOT NULL
  AND created_at > @refined_at;

-- name: CreateCandidate :one
-- Insert a concept candidate (routing cache entry + work queue
-- for refine_concepts). ON CONFLICT DO NOTHING so the same
-- (concept_text, context) from multiple facts coalesces into one
-- candidate row; the caller re-fetches by the unique key when it
-- needs the id of an existing unresolved candidate.
INSERT INTO okt_repository.concept_candidates (repository_id, concept_text, context, seed_aliases)
VALUES (@repository_id, @concept_text, @context, @seed_aliases)
ON CONFLICT (repository_id, lower(concept_text), lower(context)) DO NOTHING
RETURNING *;

-- name: FindResolvedCandidate :one
-- Cache lookup: a previously-resolved candidate for this
-- (concept_text, context). Returns the resolved_concept_id so
-- extract_concepts can link the fact directly without refinement.
SELECT * FROM okt_repository.concept_candidates
WHERE repository_id = @repository_id
  AND lower(concept_text) = lower(@concept_text)
  AND lower(context) = lower(@context)
  AND resolved_concept_id IS NOT NULL
LIMIT 1;

-- name: FindUnresolvedCandidate :one
SELECT * FROM okt_repository.concept_candidates
WHERE repository_id = @repository_id
  AND lower(concept_text) = lower(@concept_text)
  AND lower(context) = lower(@context)
  AND resolved_concept_id IS NULL
LIMIT 1;

-- name: AddFactCandidate :one
-- Link a fact to an unresolved candidate. Idempotent via the PK.
INSERT INTO okt_repository.fact_candidates (fact_id, candidate_id)
VALUES (@fact_id, @candidate_id)
ON CONFLICT (fact_id, candidate_id) DO NOTHING
RETURNING *;

-- name: ListUnresolvedCandidatesBySource :many
-- Unresolved candidates whose facts are linked to this source.
-- Used by refine_concepts to list its source-scoped work set.
SELECT DISTINCT cc.*
FROM okt_repository.concept_candidates cc
JOIN okt_repository.fact_candidates fc ON fc.candidate_id = cc.id
JOIN okt_repository.fact_sources fs ON fs.fact_id = fc.fact_id
WHERE fs.source_id = @source_id
  AND cc.resolved_concept_id IS NULL
ORDER BY cc.created_at;

-- name: ListUnresolvedCandidatesByRepo :many
-- All unresolved candidates in a repo. Used by refine_concepts for
-- the repo-wide fallback path (manual re-enqueue / periodic catch-up).
SELECT * FROM okt_repository.concept_candidates
WHERE repository_id = @repository_id
  AND resolved_concept_id IS NULL
ORDER BY created_at;

-- name: ReassignFactCandidatesToConcept :exec
-- Move fact_candidates rows to fact_concepts for the target concept.
-- ON CONFLICT DO NOTHING so facts already linked to the target
-- (via a prior candidate resolution or direct fact_concepts link)
-- are not duplicated.
INSERT INTO okt_repository.fact_concepts (fact_id, concept_id, first_seen_at)
SELECT fc.fact_id, @new_concept_id, fc.first_seen_at
FROM okt_repository.fact_candidates fc
WHERE fc.candidate_id = @old_candidate_id
ON CONFLICT (fact_id, concept_id) DO NOTHING;

-- name: DeleteFactCandidatesByCandidate :exec
-- Clear fact_candidates rows after resolution (the candidate row
-- stays as a cache entry; only its fact links are moved to
-- fact_concepts).
DELETE FROM okt_repository.fact_candidates
WHERE candidate_id = @candidate_id;

-- name: DeleteFactCandidate :exec
-- Delete a single fact_candidates row (one fact off a candidate).
-- Used by refine_concepts' per-fact routing branch: when an alias
-- matches multiple concepts, each fact is routed individually to
-- its cosine-winning concept and removed from the candidate, while
-- deferred facts (no usable vector) stay on the candidate.
DELETE FROM okt_repository.fact_candidates
WHERE fact_id = @fact_id AND candidate_id = @candidate_id;

-- name: ResolveCandidate :exec
-- Mark a candidate as resolved: record which concept it maps to
-- and when. The candidate row stays as a cache entry for future
-- extractions of the same (concept_text, context).
UPDATE okt_repository.concept_candidates
SET resolved_concept_id = @resolved_concept_id,
    resolved_at = now()
WHERE id = @id;
-- ---------------------------------------------------------------------------
-- concept_groups summary table (migration 0061).
-- ---------------------------------------------------------------------------
-- The summary is maintained incrementally by the ingest workers + migrate_context
-- + registry imports: each mutating tx collects the lower(canonical_name) keys
-- it touched and calls UpsertConceptGroups + DeleteStaleConceptGroups at the
-- end. RecomputeAllConceptGroupsForRepo is the full-repo repair path used by the
-- recompute_concept_groups River job (admin "Recompute concept groups" button).
-- Reads are O(page) via idx_concept_groups_repo_count_name.

-- name: UpsertConceptGroups :exec
-- Recompute the summary rows for the touched name_keys from live
-- concepts + fact_concepts, upserting into concept_groups. Called at
-- the end of each mutating tx (extract_concepts, refine_concepts,
-- migrate_context, registry imports). Stale groups (a name_key whose
-- last concept was deleted) are left for DeleteStaleConceptGroups.
INSERT INTO okt_repository.concept_groups
    (repository_id, name_key, canonical_name, context_count, total_fact_count, any_embedded, updated_at)
SELECT c.repository_id,
       lower(c.canonical_name),
       min(c.canonical_name),
       count(*)::int,
       COALESCE(COUNT(fc.fact_id), 0)::bigint,
       bool_or(c.embedded_at IS NOT NULL),
       now()
FROM okt_repository.concepts c
LEFT JOIN okt_repository.fact_concepts fc ON fc.concept_id = c.id
WHERE c.repository_id = sqlc.arg('repository_id')
  AND lower(c.canonical_name) = ANY(sqlc.arg('name_keys')::text[])
GROUP BY c.repository_id, lower(c.canonical_name)
ON CONFLICT (repository_id, name_key) DO UPDATE SET
    canonical_name   = EXCLUDED.canonical_name,
    context_count    = EXCLUDED.context_count,
    total_fact_count = EXCLUDED.total_fact_count,
    any_embedded     = EXCLUDED.any_embedded,
    updated_at       = now();

-- name: DeleteStaleConceptGroups :exec
-- Remove group rows for touched name_keys that no longer have any
-- concepts (e.g. migrate_context deleted the last context of a name).
-- Called right after UpsertConceptGroups.
DELETE FROM okt_repository.concept_groups g
WHERE g.repository_id = sqlc.arg('repository_id')
  AND g.name_key = ANY(sqlc.arg('name_keys')::text[])
  AND NOT EXISTS (
      SELECT 1 FROM okt_repository.concepts c
      WHERE c.repository_id = sqlc.arg('repository_id')
        AND lower(c.canonical_name) = g.name_key
  );

-- name: RecomputeAllConceptGroupsForRepo :exec
-- Full-repo recompute: delete every group row for the repo then
-- reinsert from live concepts + fact_concepts. The repair path used
-- by the recompute_concept_groups River job. Bounded by the repo's
-- concept count; runs in one tx so the table is never half-empty to
-- a concurrent reader (the DELETE + INSERT are atomic per repo).
DELETE FROM okt_repository.concept_groups WHERE repository_id = sqlc.arg('repository_id');
INSERT INTO okt_repository.concept_groups
    (repository_id, name_key, canonical_name, context_count, total_fact_count, any_embedded, updated_at)
SELECT c.repository_id,
       lower(c.canonical_name),
       min(c.canonical_name),
       count(*)::int,
       COALESCE(COUNT(fc.fact_id), 0)::bigint,
       bool_or(c.embedded_at IS NOT NULL),
       now()
FROM okt_repository.concepts c
LEFT JOIN okt_repository.fact_concepts fc ON fc.concept_id = c.id
WHERE c.repository_id = sqlc.arg('repository_id')
GROUP BY c.repository_id, lower(c.canonical_name);

-- name: ListConceptGroupsByRepoPage :many
-- Paginated group list for the q="" concept list endpoint. Reads
-- from concept_groups (the summary), O(page) via
-- idx_concept_groups_repo_count_name. q != "" does NOT use this
-- query (it needs alias visibility the summary lacks); the handler
-- falls back to the live ListGroupedConceptsByRepo for q != "".
-- @limit / @offset apply at the group level.
SELECT repository_id,
       name_key,
       canonical_name,
       context_count,
       total_fact_count,
       any_embedded
FROM okt_repository.concept_groups
WHERE repository_id = @repository_id
ORDER BY total_fact_count DESC NULLS LAST, canonical_name ASC
LIMIT sqlc.arg('limit') OFFSET sqlc.arg('offset');

-- name: CountConceptGroupsByRepo :one
-- Companion count for ListConceptGroupsByRepoPage. Index-only COUNT
-- on the PK prefix (repository_id).
SELECT COUNT(*)::bigint
FROM okt_repository.concept_groups
WHERE repository_id = @repository_id;

-- name: ListConceptsByRepoNameKeys :many
-- Fetch every per-context concept row for the groups on the current
-- page (sibling contexts). Called after ListConceptGroupsByRepoPage
-- with the page's name_keys. Ordered by name_key then fact_count
-- DESC, context ASC so the group representative (first row) matches
-- the highest-fact_count context, mirroring BuildGroups. The
-- correlated fact_count subquery runs only on the page's rows
-- (bounded by page_size × avg contexts), so it's cheap.
SELECT c.id,
       c.repository_id,
       c.canonical_name,
       c.context,
       c.description,
       c.embedded_at,
       c.embedded_model,
       c.created_at,
       (SELECT COUNT(*) FROM okt_repository.fact_concepts fc WHERE fc.concept_id = c.id) AS fact_count
FROM okt_repository.concepts c
WHERE c.repository_id = @repository_id
  AND lower(c.canonical_name) = ANY(@name_keys::text[])
ORDER BY lower(c.canonical_name), fact_count DESC, c.context ASC;

-- name: ListConceptNameKeysByIDs :many
-- Resolve concept ids to their lower(canonical_name) group keys. Used
-- by the ingest workers to collect the touched name_keys for a
-- concept_groups summary recompute after a batch of writes.
SELECT DISTINCT lower(c.canonical_name) AS name_key
FROM okt_repository.concepts c
WHERE c.id = ANY(@ids::uuid[]);
