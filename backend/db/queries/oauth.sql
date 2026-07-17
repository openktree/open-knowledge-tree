-- name: CreateOAuthClient :one
INSERT INTO okt_system.oauth_clients (
    client_id, client_secret_hash, redirect_uris, grant_types,
    response_types, token_endpoint_auth_method, client_name, scope
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
RETURNING *;

-- name: GetOAuthClientByClientID :one
SELECT * FROM okt_system.oauth_clients WHERE client_id = $1;

-- name: ListOAuthClients :many
SELECT * FROM okt_system.oauth_clients ORDER BY created_at DESC;

-- name: DeleteOAuthClientByClientID :exec
DELETE FROM okt_system.oauth_clients WHERE client_id = $1;

-- name: CreateOAuthAuthorizationCode :one
INSERT INTO okt_system.oauth_authorization_codes (
    code_hash, client_id, redirect_uri, scope, user_id,
    code_challenge, code_challenge_method, expires_at
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
RETURNING *;

-- name: GetOAuthAuthorizationCodeByHash :one
SELECT * FROM okt_system.oauth_authorization_codes WHERE code_hash = $1;

-- name: DeleteOAuthAuthorizationCodeByHash :exec
DELETE FROM okt_system.oauth_authorization_codes WHERE code_hash = $1;

-- name: DeleteExpiredOAuthAuthorizationCodes :exec
DELETE FROM okt_system.oauth_authorization_codes WHERE expires_at < now();

-- name: CreateOAuthRefreshToken :one
INSERT INTO okt_system.oauth_refresh_tokens (
    token_hash, client_id, user_id, scope, expires_at
) VALUES ($1, $2, $3, $4, $5)
RETURNING *;

-- name: GetOAuthRefreshTokenByHash :one
SELECT * FROM okt_system.oauth_refresh_tokens WHERE token_hash = $1;

-- name: RevokeOAuthRefreshTokenByHash :exec
UPDATE okt_system.oauth_refresh_tokens SET revoked = TRUE
WHERE token_hash = $1;

-- name: DeleteOAuthRefreshTokenByHash :exec
DELETE FROM okt_system.oauth_refresh_tokens WHERE token_hash = $1;

-- name: DeleteExpiredOAuthRefreshTokens :exec
DELETE FROM okt_system.oauth_refresh_tokens WHERE expires_at < now() OR revoked = TRUE;