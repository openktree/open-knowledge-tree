-- name: CreateReport :one
INSERT INTO okt_repository.reports (id, repository_id, title, topic, body_md, status, parent_id)
VALUES ($1, $2, $3, $4, $5, $6, $7)
RETURNING *;

-- name: GetReportByID :one
SELECT * FROM okt_repository.reports WHERE id = $1;

-- name: SetReportsParent :exec
-- Bulk reparent: sets parent_id on every id in the list. Caller is
-- responsible for validating all ids belong to the same repository
-- and that no id equals the new parent (self-parent) or is an
-- ancestor of the new parent (cycle prevention). repository_id is
-- checked to prevent cross-repo reparenting.
UPDATE okt_repository.reports
SET parent_id = $1,
    updated_at = now()
WHERE id = ANY($2::uuid[])
  AND repository_id = $3;

-- name: GetReportAncestors :many
-- Returns the chain of ancestors for a single report id (used for
-- cycle detection before reparenting). Ordered from immediate parent
-- up to the root.
WITH RECURSIVE ancestors AS (
    SELECT r.id, r.parent_id, r.repository_id, 1 AS depth
    FROM okt_repository.reports r
    WHERE r.id = $1
    UNION
    SELECT r.id, r.parent_id, r.repository_id, a.depth + 1
    FROM okt_repository.reports r
    JOIN ancestors a ON r.id = a.parent_id
    WHERE a.depth < 100
)
SELECT id, parent_id, repository_id FROM ancestors ORDER BY depth DESC;

-- name: ListReportsByRepo :many
-- Paginated, searchable repo-level report list. The optional `$2`
-- (search) matches title or topic with ILIKE (reports are few per
-- repo). An optional status filter (`$3`) narrows to a single
-- lifecycle state (pending/processing/annotated/failed); '' = all.
SELECT * FROM okt_repository.reports
WHERE repository_id = $1
  AND ($2::text = '' OR title ILIKE '%' || $2 || '%' OR topic ILIKE '%' || $2 || '%')
  AND ($3::text = '' OR status = $3)
ORDER BY created_at DESC
LIMIT $4 OFFSET $5;

-- name: CountReportsByRepo :one
SELECT COUNT(*) FROM okt_repository.reports
WHERE repository_id = $1
  AND ($2::text = '' OR title ILIKE '%' || $2 || '%' OR topic ILIKE '%' || $2 || '%')
  AND ($3::text = '' OR status = $3);

-- name: UpdateReport :one
-- Updates title, topic, and body_md. Re-annotation is triggered
-- separately by the caller enqueuing an annotate_report job when
-- body_md changed.
UPDATE okt_repository.reports
SET title = $2,
    topic = $3,
    body_md = $4,
    updated_at = now()
WHERE id = $1
RETURNING *;

-- name: MarkReportStatus :exec
-- Updates the report's annotation lifecycle state. error is set
-- on failure (and NULL on success); annotation_job_id records the
-- River job id that owns the in-flight annotation; sentence_count
-- and embedded_model record what the worker produced.
UPDATE okt_repository.reports
SET status = $2,
    error = $3,
    annotation_job_id = $4,
    sentence_count = $5,
    embedded_model = $6,
    similarity_threshold = $7,
    updated_at = now()
WHERE id = $1;

-- name: DeleteReport :exec
DELETE FROM okt_repository.reports WHERE id = $1;

-- name: ClearReportAnnotations :exec
-- Removes all annotations for a report before a re-annotation pass
-- so stale matches (e.g. a fact that was since deleted) don't linger.
DELETE FROM okt_repository.report_annotations WHERE report_id = $1;

-- name: AddReportAnnotation :exec
-- Idempotent on the PK so re-running annotation (or a retry that
-- reaches the same match) doesn't double-insert. posture is the
-- autocite classifier label ('related','supports','contradicts');
-- NULL on legacy rows or when the classifier was not configured
-- (the keep-all fallback).
INSERT INTO okt_repository.report_annotations (report_id, sentence_index, sentence_text, fact_id, score, posture)
VALUES ($1, $2, $3, $4, $5, $6)
ON CONFLICT (report_id, sentence_index, fact_id) DO NOTHING;

-- name: ListReportAnnotationsByReport :many
-- UI-facing: every annotation for a report, joined with the fact
-- row so the frontend can group facts by sentence_index and render
-- the autocitation view. Ordered by sentence_index then score DESC
-- so the best match surfaces first within each sentence.
SELECT ra.report_id, ra.sentence_index, ra.sentence_text, ra.fact_id, ra.score, ra.posture, ra.created_at,
       f.text, f.status, f.fact_kind, f.image_url, f.created_at AS fact_created_at,
       COALESCE(fs_count.source_count, 0) AS source_count
FROM okt_repository.report_annotations ra
JOIN okt_repository.facts f ON f.id = ra.fact_id
LEFT JOIN (
    SELECT fact_id, COUNT(*) AS source_count
    FROM okt_repository.fact_sources
    GROUP BY fact_id
) fs_count ON fs_count.fact_id = f.id
WHERE ra.report_id = $1
ORDER BY ra.sentence_index, ra.score DESC;