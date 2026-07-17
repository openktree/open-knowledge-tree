-- syntheses.sql — concept synthesis queries.
--
-- A concept_synthesis is the single authoritative "definition" the
-- system produces for a canonical-name group, folding together ALL
-- summary slices across every concept_id sharing lower(canonical_name)
-- in a repository. See migration 0028 for the schema and the
-- per-group upsert model. The synthesize_concept worker (chained from
-- summarize_concepts) is the only writer; the HTTP layer is read-only.

-- name: ListSummariesByCanonicalNameGroup :many
-- Every concept_summaries row for every concept_id in the
-- (repository_id, lower(canonical_name)) group, ordered by
-- concept_id then sequence_num so the worker can render the slices
-- in a stable order into the synthesis prompt. Joins concepts to
-- filter by lower(canonical_name).
SELECT cs.*
FROM okt_repository.concept_summaries cs
JOIN okt_repository.concepts c ON c.id = cs.concept_id
WHERE c.repository_id = @repository_id
  AND lower(c.canonical_name) = lower(@canonical_name)
ORDER BY cs.concept_id, cs.sequence_num;

-- name: ListGroupImageFacts :many
-- Image facts (fact_kind = 'image' with a non-null image_url) linked
-- via fact_concepts to any concept_id in the
-- (repository_id, lower(canonical_name)) group. The worker passes
-- these as candidates to the image-picker LLM call (or directly to
-- the synthesis when the candidate count is <= MaxImages). Ordered
-- by fact_concepts.first_seen_at DESC so the most recently linked
-- images come first, then capped at @cap by the caller (default 50).
-- Returns the fact id, text (the image description the extractor
-- produced), and image_url (service-routable; the frontend resolves
-- it to a blob URL via the storage endpoint).
SELECT f.id, f.text, f.image_url
FROM okt_repository.facts f
JOIN okt_repository.fact_concepts fc ON fc.fact_id = f.id
JOIN okt_repository.concepts c ON c.id = fc.concept_id
WHERE c.repository_id = @repository_id
  AND lower(c.canonical_name) = lower(@canonical_name)
  AND f.fact_kind = 'image'
  AND f.image_url IS NOT NULL
ORDER BY fc.first_seen_at DESC
LIMIT @cap;

-- name: ListGroupConceptIDs :many
-- The concept_ids sharing (repository_id, lower(canonical_name)).
-- Used by the worker to populate covered_concept_ids on upsert so the
-- read path knows which per-context concept_ids were folded.
SELECT c.id
FROM okt_repository.concepts c
WHERE c.repository_id = @repository_id
  AND lower(c.canonical_name) = lower(@canonical_name);

-- name: GetSynthesisByGroup :one
-- The single synthesis row for (repository_id, lower(canonical_name)),
-- or no rows when none exists. The unique index
-- uq_concept_syntheses_repo_name guarantees at most one row per
-- group, so this is a scalar lookup.
SELECT * FROM okt_repository.concept_syntheses
WHERE repository_id = sqlc.arg('repository_id')
  AND lower(canonical_name) = lower(sqlc.arg('canonical_name'));

-- name: UpsertSynthesis :one
-- Insert a new synthesis row, or update the existing row for the
-- (repository_id, lower(canonical_name)) group. The ON CONFLICT
-- targets the unique index uq_concept_syntheses_repo_name (the
-- expression index on lower(canonical_name)). On conflict, content,
-- covered_summary_ids, covered_concept_ids, embedded_image_ids, and
-- model are replaced; updated_at is bumped explicitly (the column's
-- DEFAULT now() only applies on insert). canonical_name is NOT
-- overwritten — the original casing is preserved.
INSERT INTO okt_repository.concept_syntheses (
    repository_id, canonical_name, content,
    covered_summary_ids, covered_concept_ids, embedded_image_ids, model
) VALUES ($1, $2, $3, $4, $5, $6, $7)
ON CONFLICT (repository_id, lower(canonical_name)) DO UPDATE SET
    content             = EXCLUDED.content,
    covered_summary_ids = EXCLUDED.covered_summary_ids,
    covered_concept_ids = EXCLUDED.covered_concept_ids,
    embedded_image_ids  = EXCLUDED.embedded_image_ids,
    model               = EXCLUDED.model,
    updated_at          = now()
RETURNING *;

-- name: ListSynthesesByRepo :many
-- Every synthesis in a repository, ordered by canonical_name. Used
-- by the repo-level list endpoint. Paginated by the caller via
-- LIMIT/OFFSET.
SELECT * FROM okt_repository.concept_syntheses
WHERE repository_id = $1
ORDER BY canonical_name
LIMIT $2 OFFSET $3;

-- name: CountSynthesesByRepo :one
SELECT COUNT(*) FROM okt_repository.concept_syntheses
WHERE repository_id = $1;

-- name: ListImageFactsByIDs :many
-- Bulk fetch of image facts by id list for the synthesis read path.
-- The GET /definition endpoint uses this to eager-load the
-- image_url + text of every fact the synthesis embeds via
-- ![alt](<fact_id>), so the frontend can resolve storage URLs to
-- blob URLs without N extra calls. ANY() on an empty array matches
-- nothing, which is correct (a synthesis with no embedded images
-- returns no rows). Filters to fact_kind = 'image' so a stale
-- embedded_image_id that no longer points at an image fact is
-- silently dropped.
SELECT id, text, image_url, fact_kind
FROM okt_repository.facts
WHERE id = ANY($1::uuid[])
  AND fact_kind = 'image';

-- name: DeleteSynthesisByCanonicalName :exec
-- Delete a synthesis row by (repository_id, lower(canonical_name)).
-- Used by refine_concepts when a merge deletes the loser concept:
-- concept_syntheses has no FK to concepts.id (it's keyed by
-- lower(canonical_name)), so deleting the concept leaves the
-- synthesis row orphaned. This query cleans it up so the next
-- synthesis regen for the surviving concept doesn't leave a stale
-- row under the old name.
DELETE FROM okt_repository.concept_syntheses
WHERE repository_id = @repository_id
  AND lower(canonical_name) = lower(@canonical_name);

-- name: TryAdvisoryLockForSynthesis :one
-- Attempt to acquire a session-level Postgres advisory lock keyed by
-- a hash of the group key string. Returns true on success, false when
-- another worker already holds the lock. The lock is per-connection;
-- the caller MUST release it via ReleaseAdvisoryLockForSynthesis in a
-- defer. A crashed worker's connection returns to the pool and pgx
-- releases the lock automatically, so no staleness window is needed.
SELECT pg_try_advisory_lock(hashtext($1)) AS ok;

-- name: ReleaseAdvisoryLockForSynthesis :exec
-- Release the advisory lock acquired by TryAdvisoryLockForSynthesis.
-- Best-effort: a query error is logged and swallowed by the caller so
-- a failing release doesn't poison the synthesis result.
SELECT pg_advisory_unlock(hashtext($1));