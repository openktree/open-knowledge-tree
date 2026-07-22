-- name: RecordAuditEvent :exec
INSERT INTO okt_system.permission_audit
    (actor_user_id, actor_username, action, object, repository_id, target, detail, source_url)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8);

-- name: ListAuditEvents :many
SELECT
    a.id,
    a.occurred_at,
    a.actor_user_id,
    a.actor_username,
    a.action,
    a.object,
    a.repository_id,
    a.target,
    a.detail,
    a.source_url,
    u.email AS actor_email
FROM okt_system.permission_audit a
LEFT JOIN users u ON u.id = a.actor_user_id
WHERE (sqlc.narg('repository_id')::uuid IS NULL OR a.repository_id = sqlc.narg('repository_id'))
  AND (sqlc.narg('action')::text IS NULL OR a.action = sqlc.narg('action'))
  AND (sqlc.narg('actor_user_id')::uuid IS NULL OR a.actor_user_id = sqlc.narg('actor_user_id'))
  AND (sqlc.narg('object')::text IS NULL OR a.object = sqlc.narg('object'))
  AND (sqlc.narg('from')::timestamptz IS NULL OR a.occurred_at >= sqlc.narg('from'))
  AND (sqlc.narg('to')::timestamptz IS NULL OR a.occurred_at <= sqlc.narg('to'))
ORDER BY a.occurred_at DESC
LIMIT @limit_rows OFFSET @page_offset;

-- name: CountAuditEvents :one
SELECT count(*) FROM okt_system.permission_audit a
WHERE (sqlc.narg('repository_id')::uuid IS NULL OR a.repository_id = sqlc.narg('repository_id'))
  AND (sqlc.narg('action')::text IS NULL OR a.action = sqlc.narg('action'))
  AND (sqlc.narg('actor_user_id')::uuid IS NULL OR a.actor_user_id = sqlc.narg('actor_user_id'))
  AND (sqlc.narg('object')::text IS NULL OR a.object = sqlc.narg('object'))
  AND (sqlc.narg('from')::timestamptz IS NULL OR a.occurred_at >= sqlc.narg('from'))
  AND (sqlc.narg('to')::timestamptz IS NULL OR a.occurred_at <= sqlc.narg('to'));

-- name: DeleteAuditEventsOlderThan :execrows
DELETE FROM okt_system.permission_audit WHERE occurred_at < $1;

-- name: ListAuditActions :many
SELECT DISTINCT action FROM okt_system.permission_audit ORDER BY action;