-- name: CreateAPIKey :one
INSERT INTO okt_system.api_keys (
    user_id, name, token_hash, prefix, repository_id, permissions, expires_at
) VALUES ($1, $2, $3, $4, $5, $6, $7)
RETURNING *;

-- name: GetAPIKeyByTokenHash :one
SELECT * FROM okt_system.api_keys
WHERE token_hash = $1
  AND revoked_at IS NULL
  AND (expires_at IS NULL OR expires_at > now());

-- name: GetAPIKeyByID :one
SELECT * FROM okt_system.api_keys WHERE id = $1;

-- name: ListAPIKeysByUser :many
SELECT id, user_id, name, prefix, repository_id, permissions, expires_at,
       last_used_at, revoked_at, created_at
FROM okt_system.api_keys
WHERE user_id = $1
ORDER BY created_at DESC;

-- name: CountAPIKeysByUser :one
SELECT count(*) FROM okt_system.api_keys WHERE user_id = $1;

-- name: RevokeAPIKey :exec
UPDATE okt_system.api_keys SET revoked_at = now()
WHERE id = $1 AND user_id = $2 AND revoked_at IS NULL;

-- name: TouchAPIKeyLastUsed :exec
UPDATE okt_system.api_keys SET last_used_at = now()
WHERE id = $1;

-- name: DeleteExpiredAPIKeys :exec
DELETE FROM okt_system.api_keys
WHERE expires_at IS NOT NULL AND expires_at < now() AND revoked_at IS NULL;