-- name: CreateInvestigation :one
INSERT INTO okt_repository.investigations (id, repository_id, title, topic)
VALUES ($1, $2, $3, $4)
RETURNING *;

-- name: GetInvestigationByID :one
SELECT * FROM okt_repository.investigations WHERE id = $1;

-- name: ListInvestigationsByRepo :many
-- Paginated, searchable repo-level investigation list. The optional
-- `$2` (search) matches the title or topic with ILIKE so the user
-- can filter by name without the full-text machinery a source/fact
-- search needs (investigations are few per repo). Ordered by
-- created_at DESC; LIMIT/OFFSET apply on top.
SELECT * FROM okt_repository.investigations
WHERE repository_id = $1
  AND ($2::text = '' OR title ILIKE '%' || $2 || '%' OR topic ILIKE '%' || $2 || '%')
ORDER BY created_at DESC
LIMIT $3 OFFSET $4;

-- name: CountInvestigationsByRepo :one
SELECT COUNT(*) FROM okt_repository.investigations
WHERE repository_id = $1
  AND ($2::text = '' OR title ILIKE '%' || $2 || '%' OR topic ILIKE '%' || $2 || '%');

-- name: UpdateInvestigation :one
-- Updates title and topic. NULL topic clears the field.
UPDATE okt_repository.investigations
SET title = $2,
    topic = $3,
    updated_at = now()
WHERE id = $1
RETURNING *;

-- name: DeleteInvestigation :exec
DELETE FROM okt_repository.investigations WHERE id = $1;

-- name: AddInvestigationSource :exec
-- Idempotent: re-adding a source to the same investigation is a
-- no-op. ON CONFLICT preserves the original added_at.
INSERT INTO okt_repository.investigation_sources (investigation_id, source_id)
VALUES ($1, $2)
ON CONFLICT (investigation_id, source_id) DO NOTHING;

-- name: RemoveInvestigationSource :exec
DELETE FROM okt_repository.investigation_sources
WHERE investigation_id = $1 AND source_id = $2;

-- name: ListInvestigationSources :many
-- The investigation's source rows, joined from the junction.
-- Paginated and searchable against the source's search_tsv (url +
-- parsed_title + doi) so the Sources phase mirrors the repo-level
-- source list's search behavior. Ordered by added_at DESC so the
-- most recently added sources surface first; LIMIT/OFFSET on top.
SELECT s.*,
       is_join.added_at
FROM okt_repository.investigation_sources is_join
JOIN okt_repository.sources s ON s.id = is_join.source_id
WHERE is_join.investigation_id = $1
  AND ($2::text = '' OR s.search_tsv @@ websearch_to_tsquery('english', $2))
ORDER BY is_join.added_at DESC
LIMIT $3 OFFSET $4;

-- name: CountInvestigationSources :one
SELECT COUNT(*) FROM okt_repository.investigation_sources is_join
JOIN okt_repository.sources s ON s.id = is_join.source_id
WHERE is_join.investigation_id = $1
  AND ($2::text = '' OR s.search_tsv @@ websearch_to_tsquery('english', $2));

-- name: ListInvestigationFacts :many
-- Facts contributed by the investigation's sources, via
-- fact_sources → investigation_sources. Deduped by fact_id (a fact
-- confirmed by N of the investigation's sources is returned once)
-- with a computed source_count over ALL sources in the repo (not
-- just the investigation's) so cross-confirmation still shows.
-- Paginated, searchable, and sortable, mirroring the repo-level
-- fact list. The status filter accepts '' (all), 'stable', 'new',
-- 'to_delete'. The sort accepts 'created_at' (default) or
-- 'source_count'.
SELECT f.id, f.text, f.status, f.embedded_at, f.embedded_model, f.created_at,
       f.fact_kind, f.image_url,
       COALESCE(fs_count.source_count, 0) AS source_count
FROM okt_repository.facts f
JOIN okt_repository.fact_sources fs ON fs.fact_id = f.id
JOIN okt_repository.investigation_sources is_join ON is_join.source_id = fs.source_id
JOIN okt_repository.investigations inv ON inv.id = is_join.investigation_id
LEFT JOIN LATERAL (
    SELECT fs2.fact_id, COUNT(*) AS source_count
    FROM okt_repository.fact_sources fs2
    JOIN okt_repository.sources s2 ON s2.id = fs2.source_id
    WHERE s2.repository_id = inv.repository_id
    GROUP BY fs2.fact_id
) fs_count ON fs_count.fact_id = f.id
WHERE is_join.investigation_id = $1
  AND ($2::text = '' OR f.status = $2)
  AND ($3::text = '' OR f.search_tsv @@ websearch_to_tsquery('english', $3))
GROUP BY f.id, fs_count.source_count
ORDER BY
    CASE WHEN $4 = 'source_count' THEN fs_count.source_count END DESC NULLS LAST,
    f.created_at DESC
LIMIT $5 OFFSET $6;

-- name: CountInvestigationFacts :one
-- Companion count for ListInvestigationFacts. Counts DISTINCT facts
-- contributed by the investigation's sources and matching the
-- status/search filters.
SELECT COUNT(DISTINCT f.id)
FROM okt_repository.facts f
JOIN okt_repository.fact_sources fs ON fs.fact_id = f.id
JOIN okt_repository.investigation_sources is_join ON is_join.source_id = fs.source_id
WHERE is_join.investigation_id = $1
  AND ($2::text = '' OR f.status = $2)
  AND ($3::text = '' OR f.search_tsv @@ websearch_to_tsquery('english', $3));