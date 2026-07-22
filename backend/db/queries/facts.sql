-- name: CreateFact :one
-- fact_kind defaults to 'text' (the column default) when the caller
-- does not specify it; image facts pass 'image' plus a non-null
-- image_url so the frontend can render the picture next to the fact
-- text. image_url is nullable so text facts leave it NULL.
-- promptset_hash tags the fact with the philosophy that produced it
-- so downstream queries (synthesis, registry pull) can filter to a
-- single promptset and decompositions from different promptsets do
-- not mix. The caller passes the repo's effective promptset hash;
-- NULL is allowed (legacy rows) and interpreted as the built-in.
INSERT INTO okt_repository.facts (id, text, fact_kind, image_url, promptset_hash)
VALUES ($1, $2, $3, $4, $5)
RETURNING *;

-- name: AddFactSource :exec
-- Idempotent: a fact linked to the same source twice (e.g. re-processing)
-- doesn't double-count. ON CONFLICT preserves the original first_seen_at.
-- Uses :exec (no RETURNING) so the conflict path returns no error —
-- callers that need the row back can re-query by (fact_id, source_id).
INSERT INTO okt_repository.fact_sources (fact_id, source_id, chunk_index)
VALUES ($1, $2, $3)
ON CONFLICT (fact_id, source_id) DO NOTHING;

-- name: MarkFactEmbedded :one
UPDATE okt_repository.facts
SET embedded_at = now(),
    embedded_model = $2
WHERE id = $1
RETURNING *;

-- name: MarkFactStatus :one
UPDATE okt_repository.facts
SET status = $2
WHERE id = $1
RETURNING *;

-- name: MarkFactsStableByRepo :exec
-- Promote surviving 'new' facts in a repository to 'stable'.
-- Repository filter goes through fact_sources + sources so the
-- query works the same on a shared (filtered) and an isolated
-- (physically separate) database.
UPDATE okt_repository.facts f
SET status = 'stable'
WHERE f.status = 'new'
  AND EXISTS (
      SELECT 1 FROM okt_repository.fact_sources fs
      JOIN okt_repository.sources s ON fs.source_id = s.id
      WHERE fs.fact_id = f.id AND s.repository_id = $1
  );

-- name: DeleteFactByID :exec
DELETE FROM okt_repository.facts WHERE id = $1;

-- name: GetFactByID :one
SELECT * FROM okt_repository.facts WHERE id = $1;

-- name: GetFactsByIDs :many
-- Batch lookup used by the annotate_report worker to fetch the text
-- of every candidate fact Qdrant returned for a sentence batch, so
-- the posture classifier can render (sentence, fact) pairs without
-- N+1 GetFactByID round-trips. The caller passes the deduped fact
-- ids across the whole batch; order is unspecified.
SELECT * FROM okt_repository.facts WHERE id = ANY($1::uuid[]);

-- name: SearchFactsByNumericTokens :many
-- Hybrid-retrieval lexical fallback for the annotate_report worker.
-- Given a repository_id and a tsquery string (e.g. '508 & 0.9 & kg')
-- built by the caller from numeric tokens extracted from a report
-- sentence, returns up to `limit` facts in that repository whose
-- search_tsv matches the query (plain tsquery AND semantics). Reuses
-- the existing search_tsv generated column + idx_facts_search_tsv
-- GIN index added by migration 0015 (the 'english' config indexes
-- numbers and short unit tokens verbatim — only long prose words are
-- stemmed, which is fine because the lexical fallback is for exact
-- numeric/unit matches, not prose synonyms). The caller unions these
-- lexical hits with the Qdrant semantic hits, dedupes by fact_id,
-- and feeds the combined set to the posture classifier.
--
-- Scoping: the facts table is per-repo on isolated databases and
-- interleaved on shared databases; the JOIN through fact_sources +
-- sources filters by repository_id in both layouts (mirrors
-- ListFactsForDedup). Excluded fact ids ($3) lets the caller skip
-- facts the Qdrant pass already surfaced (avoids double-counting).
SELECT DISTINCT ON (f.id) f.*
FROM okt_repository.facts f
JOIN okt_repository.fact_sources fs ON fs.fact_id = f.id
JOIN okt_repository.sources s ON fs.source_id = s.id
WHERE s.repository_id = sqlc.arg('repository_id')
  AND f.search_tsv @@ to_tsquery('english', sqlc.arg('tsquery'))
  AND (sqlc.arg('exclude_ids')::uuid[] = '{}' OR NOT (f.id = ANY(sqlc.arg('exclude_ids')::uuid[])))
ORDER BY f.id
LIMIT sqlc.arg('row_limit');

-- name: GetFactByTextAndSource :one
-- Exact-text match for the cache import's delta-aware no-op check.
-- If a fact with the exact same text already exists linked to this
-- source, the import skips CreateFact entirely (no new row, no
-- Qdrant point, no downstream job). After the first import + dedup,
-- a merged survivor is re-linked to the source via mergeSources,
-- so this query finds the survivor and the re-import is a no-op.
-- No index on facts.text today; this is a sequential scan, which
-- is acceptable because the cache import runs once per source and
-- the alternative (re-create + re-dedup) is more expensive.
SELECT f.* FROM okt_repository.facts f
JOIN okt_repository.fact_sources fs ON fs.fact_id = f.id
WHERE f.text = $1 AND fs.source_id = $2
LIMIT 1;

-- name: ListNewFactsForSourceEmbedding :many
-- All 'new' + embedded_at IS NULL facts for ONE source, scoped via
-- the fact_sources + sources JOIN. No limit: a source's facts always
-- complete in one embed pass so the dedup chain fires per source.
-- One row per fact; a fact linked to this source multiple times is
-- returned once (the JOIN can't expand it since fact_sources PK is
-- (fact_id, source_id)). Uses SELECT f.* (not an explicit column
-- list) so sqlc emits the row as store.OktRepositoryFact, which the
-- embed worker passes to embedFacts without a type conversion.
SELECT DISTINCT ON (f.id) f.*
FROM okt_repository.facts f
JOIN okt_repository.fact_sources fs ON fs.fact_id = f.id
JOIN okt_repository.sources s ON fs.source_id = s.id
WHERE s.repository_id = $1
  AND fs.source_id = $2
  AND f.status = 'new'
  AND f.embedded_at IS NULL
ORDER BY f.id;

-- name: ListFactsForDedup :many
-- All 'new' + 'stable' facts in the repository. The worker needs
-- the fact row (id, text, status, created_at) to decide dedup
-- ordering and to run nearest-neighbor searches.
SELECT f.* FROM okt_repository.facts f
JOIN okt_repository.fact_sources fs ON fs.fact_id = f.id
JOIN okt_repository.sources s ON fs.source_id = s.id
WHERE s.repository_id = $1
  AND f.status IN ('new', 'stable')
ORDER BY f.created_at;

-- name: ListFactsToDelete :many
-- status='to_delete' facts in the repository. The worker deletes
-- them from Postgres and Qdrant. Returns the fact id only (the
-- Qdrant point id is the fact UUID).
SELECT f.id FROM okt_repository.facts f
JOIN okt_repository.fact_sources fs ON fs.fact_id = f.id
JOIN okt_repository.sources s ON fs.source_id = s.id
WHERE s.repository_id = $1
  AND f.status = 'to_delete';

-- name: ListFactsToDeleteBySource :many
-- Source-scoped variant of ListFactsToDelete. After a cross-source
-- dedup merge, the loser's fact_sources rows still point at its
-- original sources (mergeSources adds the loser's sources to the
-- winner, it does not remove them from the loser), so this query
-- finds the to_delete facts whose original source was this one.
-- DISTINCT because the fact_sources join may expand a fact with
-- multiple sources; we only need the id once.
SELECT DISTINCT f.id FROM okt_repository.facts f
JOIN okt_repository.fact_sources fs ON fs.fact_id = f.id
JOIN okt_repository.sources s ON fs.source_id = s.id
WHERE s.repository_id = $1
  AND fs.source_id = $2
  AND f.status = 'to_delete';

-- name: ListFactSources :many
-- Full source rows linked to a fact, joined with the source's url,
-- parsed_title, parsed_sitename (publication name), parsed_author,
-- and published_at so the fact detail endpoint surfaces enough
-- source metadata for downstream consumers (the UI, the MCP getFact
-- tool, and LLM synthesis prompts) to attribute facts to their
-- publications and answer publication- and time-bound questions
-- (e.g. MultiHop-RAG comparison and temporal queries). Ordered by
-- first_seen_at so the UI shows the oldest confirmation first.
SELECT fs.fact_id, fs.source_id, fs.chunk_index, fs.first_seen_at,
       s.url, s.parsed_title, s.parsed_sitename, s.parsed_author,
       s.published_at
FROM okt_repository.fact_sources fs
JOIN okt_repository.sources s ON fs.source_id = s.id
WHERE fs.fact_id = $1
ORDER BY fs.first_seen_at;

-- name: ListFactSourcesByFact :many
-- The raw fact_sources rows for a fact (no source join). The
-- dedup worker uses this to walk a fact's current source set
-- when merging a loser onto a survivor.
SELECT * FROM okt_repository.fact_sources WHERE fact_id = $1;

-- name: ListFactsByRepoWithSourceCount :many
-- Repo-level list with a computed source_count (aggregate JOIN).
-- Paginated and searchable. The optional status filter ($2) accepts
-- '' (all), 'stable', 'new', or 'to_delete'. The optional search
-- ($3) runs through websearch_to_tsquery against facts.search_tsv.
-- The optional sort ($4) accepts 'created_at' (default — newest
-- first) or 'source_count' (most confirmed first); the sort is
-- pushed into SQL so pagination stays globally consistent across
-- pages (an in-memory sort would only re-order the current page).
-- LIMIT $5 / OFFSET $6 apply on top of the ORDER BY.
--
-- `source_id` is MIN(fs.source_id) so the row carries the source
-- that produced the fact (the frontend's repo-level Facts list
-- page does not have a source in the URL; image facts carry a
-- service-routable image_url that points at this source, so the
-- frontend needs the id to resolve it through getSourceImage).
-- Image facts have a single junction row (chunk_index=-1) so MIN
-- is the only source; text facts deduped across N sources
-- arbitrarily pick one — the source_id is unused for text facts.
-- Postgres has no MIN(uuid) aggregate, so cast to text and back.
SELECT f.id, f.text, f.status, f.embedded_at, f.embedded_model, f.created_at,
       f.fact_kind, f.image_url,
       COALESCE(fs_count.source_count, 0) AS source_count,
       MIN(fs.source_id::text)::uuid AS source_id
FROM okt_repository.facts f
JOIN okt_repository.fact_sources fs ON fs.fact_id = f.id
JOIN okt_repository.sources s ON fs.source_id = s.id
LEFT JOIN (
    SELECT fs2.fact_id, COUNT(*) AS source_count
    FROM okt_repository.fact_sources fs2
    JOIN okt_repository.sources s2 ON s2.id = fs2.source_id
    WHERE s2.repository_id = $1
    GROUP BY fs2.fact_id
) fs_count ON fs_count.fact_id = f.id
WHERE s.repository_id = $1
  AND ($2::text = '' OR f.status = $2)
  AND ($3::text = '' OR f.search_tsv @@ websearch_to_tsquery('english', $3))
GROUP BY f.id, fs_count.source_count
ORDER BY
    CASE WHEN $4 = 'source_count' THEN fs_count.source_count END DESC NULLS LAST,
    f.created_at DESC
LIMIT $5 OFFSET $6;

-- name: CountFactsByRepo :one
-- Companion count for ListFactsByRepoWithSourceCount. Counts
-- DISTINCT facts (a fact with N sources has N rows in
-- fact_sources) matching the same WHERE clause as the list query,
-- minus the ORDER BY. Used by the API envelope to report `total`
-- without a window-function COUNT(*) OVER ().
SELECT COUNT(DISTINCT f.id)
FROM okt_repository.facts f
JOIN okt_repository.fact_sources fs ON fs.fact_id = f.id
JOIN okt_repository.sources s ON fs.source_id = s.id
WHERE s.repository_id = $1
  AND ($2::text = '' OR f.status = $2)
  AND ($3::text = '' OR f.search_tsv @@ websearch_to_tsquery('english', $3));

-- name: GetFactsByIDsForSearch :many
-- Fetches the full ListFactsByRepoWithSourceCount-shaped row for an
-- arbitrary set of fact ids in a single round-trip. Used by the
-- hybrid search path: the lexical channel over-fetches its own
-- rows, the semantic channel (Qdrant) returns only ids, and this
-- query fills in the rows for any semantic-only ids the lexical
-- batch didn't already return. No ordering is applied here — the
-- caller re-orders the combined row set in Go to match the fused
-- ranking (sqlc can't express array_position(...) cleanly and the
-- result set is small: at most the over-fetch cap per channel).
-- Repository scoping is enforced so a cross-repo id in the input
-- set is silently dropped (defense in depth alongside Qdrant's
-- repository payload filter).
SELECT f.id, f.text, f.status, f.embedded_at, f.embedded_model, f.created_at,
       f.fact_kind, f.image_url,
       COALESCE(fs_count.source_count, 0) AS source_count,
       MIN(fs.source_id::text)::uuid AS source_id
FROM okt_repository.facts f
JOIN okt_repository.fact_sources fs ON fs.fact_id = f.id
JOIN okt_repository.sources s ON fs.source_id = s.id
LEFT JOIN (
    SELECT fact_id, COUNT(*) AS source_count
    FROM okt_repository.fact_sources
    GROUP BY fact_id
) fs_count ON fs_count.fact_id = f.id
WHERE f.id = ANY(@ids::uuid[])
  AND s.repository_id = @repository_id
GROUP BY f.id, fs_count.source_count;

-- name: ListFactsBySource :many
-- Facts linked to a specific source, via the junction. Paginated
-- and searchable. The optional status filter ($2) accepts '' (all).
-- The optional search ($3) runs through websearch_to_tsquery against
-- facts.search_tsv. Ordered by chunk_index then first_seen_at so the
-- UI shows facts in extraction order within the source; LIMIT $4 /
-- OFFSET $5 apply on top. Includes the computed source_count so a
-- fact confirmed by other sources shows that cross-confirmation here
-- too.
SELECT f.id, f.text, f.status, f.embedded_at, f.embedded_model, f.created_at,
       f.fact_kind, f.image_url,
       COALESCE(fs_count.source_count, 0) AS source_count
FROM okt_repository.facts f
JOIN okt_repository.fact_sources fs ON fs.fact_id = f.id
LEFT JOIN (
    SELECT fact_id, COUNT(*) AS source_count
    FROM okt_repository.fact_sources
    GROUP BY fact_id
) fs_count ON fs_count.fact_id = f.id
WHERE fs.source_id = $1
  AND ($2::text = '' OR f.status = $2)
  AND ($3::text = '' OR f.search_tsv @@ websearch_to_tsquery('english', $3))
ORDER BY fs.chunk_index, fs.first_seen_at
LIMIT $4 OFFSET $5;

-- name: CountFactsBySource :one
-- Companion count for ListFactsBySource. Counts DISTINCT facts
-- linked to the source and matching the status/search filters so
-- the API envelope can report `total` without a window function.
SELECT COUNT(DISTINCT f.id)
FROM okt_repository.facts f
JOIN okt_repository.fact_sources fs ON fs.fact_id = f.id
WHERE fs.source_id = $1
  AND ($2::text = '' OR f.status = $2)
  AND ($3::text = '' OR f.search_tsv @@ websearch_to_tsquery('english', $3));

-- name: DeleteStaleFactsInDB :many
-- Bounded delete of facts with status in the given set older than
-- the cutoff. Returns the deleted IDs so the caller can delete the
-- matching Qdrant points. Bounded (LIMIT $3) to avoid multi-minute
-- WAL spikes at millions of facts; the catchup job loops until 0
-- rows are returned.
DELETE FROM okt_repository.facts
WHERE id IN (
    SELECT f.id FROM okt_repository.facts f
    WHERE f.status = ANY($1::text[])
      AND f.created_at < $2
    LIMIT $3
)
RETURNING id;

-- name: MarkSourceProcessed :one
UPDATE okt_repository.sources
SET status = 'processed',
    processed_at = now(),
    updated_at = now()
WHERE id = $1
RETURNING *;

-- ---------------------------------------------------------------------------
-- Sentence-level fact references (migration 0022).
-- ---------------------------------------------------------------------------

-- name: SetSentenceOffsets :exec
-- Persist the deterministic global sentence array as a flat
-- [start0, end0, start1, end1, ...] rune-offset array. Written once
-- by retrieve_source after parsing; read by source_decomposition to
-- build labeled chunk text and by the API to serve the UI.
UPDATE okt_repository.sources
SET sentence_offsets = $2
WHERE id = $1;

-- name: AddFactReference :exec
-- One row per (fact, source, sentence_index). Idempotent on the PK
-- so re-processing a source doesn't double-count a citation.
-- promptset_hash tags the citation with the philosophy that produced
-- the fact, mirroring facts.promptset_hash so the junction stays
-- queryable by philosophy.
INSERT INTO okt_repository.fact_references (fact_id, source_id, sentence_index, chunk_index, promptset_hash)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (fact_id, source_id, sentence_index) DO NOTHING;

-- name: ListFactReferencesByFact :many
-- Raw fact_references rows for a fact (no source join). Used by the
-- dedup worker to relink a loser's citations onto a survivor.
SELECT * FROM okt_repository.fact_references WHERE fact_id = $1;

-- name: ListFactReferencesBySource :many
-- UI-facing: every citation in a source, joined with the fact row so
-- the frontend can group facts by sentence_index and render the
-- highlight + modal. Ordered by sentence_index so the frontend can
-- stream the list into a Map in one pass.
SELECT fr.fact_id, fr.source_id, fr.sentence_index, fr.chunk_index, fr.first_seen_at,
       f.text, f.status, f.fact_kind, f.image_url, f.created_at
FROM okt_repository.fact_references fr
JOIN okt_repository.facts f ON f.id = fr.fact_id
WHERE fr.source_id = $1
ORDER BY fr.sentence_index;

-- name: DeleteDuplicateFactReferences :exec
-- Before relinking a loser fact's citations onto the survivor,
-- delete the loser's rows that would collide with the survivor's
-- existing citations (same source_id + sentence_index). The PK
-- (fact_id, source_id, sentence_index) would otherwise reject the
-- UPDATE. Plain integer comparison — no array ops.
DELETE FROM okt_repository.fact_references fr_loser
USING okt_repository.fact_references fr_winner
WHERE fr_loser.fact_id = $1
  AND fr_winner.fact_id = $2
  AND fr_loser.source_id = fr_winner.source_id
  AND fr_loser.sentence_index = fr_winner.sentence_index;

-- name: RelinkFactReferences :exec
-- Move all of a loser fact's citation rows onto the survivor. Run
-- after DeleteDuplicateFactReferences so no PK collision remains.
-- Non-overlapping citations from both facts are preserved, which is
-- the dedup-preserves-all-references guarantee.
UPDATE okt_repository.fact_references
SET fact_id = $2
WHERE fact_id = $1;

-- name: ListFactsByRepoWithSourceCountForConcept :many
-- concept-filtered variant of ListFactsByRepoWithSourceCount. The
-- caller resolves the `concept` argument (a UUID, or a canonical
-- name optionally narrowed by a `context`) into an array of
-- concept_ids (the whole group, or just the matching contexts),
-- then passes that array here. A fact matches when it links to ANY
-- of those concept_ids via fact_concepts. Same select list, status
-- filter, search, sort, and pagination as the unfiltered query so
-- the MCP searchFacts tool can branch into this query without
-- changing its output shape.
SELECT f.id, f.text, f.status, f.embedded_at, f.embedded_model, f.created_at,
       f.fact_kind, f.image_url,
       COALESCE(fs_count.source_count, 0) AS source_count,
       MIN(fs.source_id::text)::uuid AS source_id
FROM okt_repository.facts f
JOIN okt_repository.fact_sources fs ON fs.fact_id = f.id
JOIN okt_repository.sources s ON fs.source_id = s.id
LEFT JOIN (
    SELECT fact_id, COUNT(*) AS source_count
    FROM okt_repository.fact_sources
    GROUP BY fact_id
) fs_count ON fs_count.fact_id = f.id
WHERE s.repository_id = $1
  AND ($2::text = '' OR f.status = $2)
  AND ($3::text = '' OR f.search_tsv @@ websearch_to_tsquery('english', $3))
  AND EXISTS (
      SELECT 1 FROM okt_repository.fact_concepts fc
      WHERE fc.fact_id = f.id AND fc.concept_id = ANY($4::uuid[])
  )
GROUP BY f.id, fs_count.source_count
ORDER BY
    CASE WHEN $5 = 'source_count' THEN fs_count.source_count END DESC NULLS LAST,
    f.created_at DESC
LIMIT $6 OFFSET $7;

-- name: CountFactsByRepoForConcept :one
-- Companion count for ListFactsByRepoWithSourceCountForConcept. Same
-- concept-id array filter, minus the ORDER BY / LIMIT / OFFSET.
SELECT COUNT(DISTINCT f.id)
FROM okt_repository.facts f
JOIN okt_repository.fact_sources fs ON fs.fact_id = f.id
JOIN okt_repository.sources s ON fs.source_id = s.id
WHERE s.repository_id = $1
  AND ($2::text = '' OR f.status = $2)
  AND ($3::text = '' OR f.search_tsv @@ websearch_to_tsquery('english', $3))
  AND EXISTS (
      SELECT 1 FROM okt_repository.fact_concepts fc
      WHERE fc.fact_id = f.id AND fc.concept_id = ANY($4::uuid[])
  );

-- name: ResetFactEmbeddingForReembed :exec
-- Clear embedded_at + embedded_model on a set of facts so the
-- embed_facts worker (which filters embedded_at IS NULL) re-embeds
-- them with the local model. Used by the CacheReconciler when an
-- imported decomposition's embedding model differs from the local
-- embedding config. The Qdrant points for these facts are deleted
-- separately by the caller (now possible because Qdrant uses local
-- fact UUIDs, not remote registry UUIDs).
UPDATE okt_repository.facts
SET embedded_at = NULL,
    embedded_model = NULL
WHERE id = ANY($1::uuid[]);

-- name: ListSharedFactsByConceptGroups :many
-- N-ary intersection: facts linked to ALL n input concept groups.
-- The caller resolves each input concept (UUID or canonical name)
-- into its group's concept_ids, flattens them into one uuid[] with a
-- parallel int4[] tagging each concept_id's group index (0..n-1).
-- A fact qualifies when the DISTINCT set of group indices linked to
-- it (via fact_concepts) equals the DISTINCT set of group indices in
-- the input — i.e. the fact is linked to at least one concept_id from
-- EVERY group. COUNT(DISTINCT i.group_idx) dedupes a fact linked to
-- multiple contexts of the same group. Same select list, status
-- filter, search, and pagination shape as
-- ListFactsByRepoWithSourceCountForConcept so the MCP searchFacts
-- tool and the REST /facts endpoint can branch into this query
-- without changing their output shape.
SELECT f.id, f.text, f.status, f.embedded_at, f.embedded_model, f.created_at,
       f.fact_kind, f.image_url,
       COALESCE(fs_count.source_count, 0) AS source_count,
       MIN(fs.source_id::text)::uuid AS source_id
FROM okt_repository.facts f
JOIN okt_repository.fact_sources fs ON fs.fact_id = f.id
JOIN okt_repository.sources s ON fs.source_id = s.id
LEFT JOIN (
    SELECT fs2.fact_id, COUNT(*) AS source_count
    FROM okt_repository.fact_sources fs2
    JOIN okt_repository.sources s2 ON s2.id = fs2.source_id
    WHERE s2.repository_id = $1
    GROUP BY fs2.fact_id
) fs_count ON fs_count.fact_id = f.id
JOIN (
    -- Per-fact coverage: the distinct set of input group indices the
    -- fact is linked to (via fact_concepts). A fact qualifies for the
    -- intersection when its coverage count equals the total distinct
    -- group count in the input. The unnest WITH ORDINALITY pairs the
    -- parallel concept_id[] and group_idx[] arrays positionally.
    SELECT fc.fact_id, COUNT(DISTINCT g2.gv) AS covered_groups
    FROM okt_repository.fact_concepts fc,
         LATERAL unnest($4::uuid[]) WITH ORDINALITY AS c1(cid, c1_idx),
         LATERAL unnest($5::int4[]) WITH ORDINALITY AS g2(gv, g2_idx)
    WHERE c1.c1_idx = g2.g2_idx
      AND c1.cid = fc.concept_id
    GROUP BY fc.fact_id
) cov ON cov.fact_id = f.id
WHERE s.repository_id = $1
  AND ($2::text = '' OR f.status = $2)
  AND ($3::text = '' OR f.search_tsv @@ websearch_to_tsquery('english', $3))
  AND cov.covered_groups = (SELECT COUNT(DISTINCT g) FROM unnest($5::int4[]) AS t(g))
GROUP BY f.id, fs_count.source_count
ORDER BY
    CASE WHEN $6 = 'source_count' THEN COALESCE(fs_count.source_count, 0) END DESC NULLS LAST,
    f.created_at DESC
LIMIT $7 OFFSET $8;

-- name: CountSharedFactsByConceptGroups :one
-- Companion count for ListSharedFactsByConceptGroups. Same
-- concept-id + group-index intersection filter, minus ORDER BY /
-- LIMIT / OFFSET.
SELECT COUNT(DISTINCT f.id)
FROM okt_repository.facts f
JOIN okt_repository.fact_sources fs ON fs.fact_id = f.id
JOIN okt_repository.sources s ON fs.source_id = s.id
JOIN (
    SELECT fc.fact_id, COUNT(DISTINCT g2.gv) AS covered_groups
    FROM okt_repository.fact_concepts fc,
         LATERAL unnest($4::uuid[]) WITH ORDINALITY AS c1(cid, c1_idx),
         LATERAL unnest($5::int4[]) WITH ORDINALITY AS g2(gv, g2_idx)
    WHERE c1.c1_idx = g2.g2_idx
      AND c1.cid = fc.concept_id
    GROUP BY fc.fact_id
) cov ON cov.fact_id = f.id
WHERE s.repository_id = $1
  AND ($2::text = '' OR f.status = $2)
  AND ($3::text = '' OR f.search_tsv @@ websearch_to_tsquery('english', $3))
  AND cov.covered_groups = (SELECT COUNT(DISTINCT g) FROM unnest($5::int4[]) AS t(g));