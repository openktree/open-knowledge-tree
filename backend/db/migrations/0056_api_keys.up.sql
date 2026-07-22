-- 0056_api_keys.up.sql
--
-- Personal API keys (personal access tokens). A user can create
-- long-lived opaque tokens that authenticate the same REST surface
-- a browser session does, but scoped to:
--   - a single repository (repository_id NOT NULL) or every
--     repository the user can access (repository_id NULL);
--   - a subset of (object, action) permission pairs drawn from the
--     rbac.Objects / rbac.Actions vocabulary (permissions TEXT[]).
--
-- The token itself is never stored. The raw token returned to the
-- client at creation time is `okt_<base64url(32 random bytes)>`; the
-- `token_hash` column holds sha256hex(raw_full_token_with_prefix),
-- mirroring the sessions.token_hash + oauth_refresh_tokens.token_hash
-- pattern. The `prefix` column stores the first 12 chars (okt_ + 8)
-- so the management UI can show a recognizable label without leaking
-- the secret.
--
-- Scopes are enforced in the RequirePermission / RequireRepoPermission
-- middlewares: after the user's RBAC check passes, the middleware also
-- verifies the request's (object, action) is in the key's permissions
-- array and (for repo routes) that the key's repository_id is NULL or
-- matches. Session-authenticated requests skip this extra check.
--
-- Lifecycle: `revoked_at` (NULL while active), `expires_at` (NULL =
-- no expiry), `last_used_at` (best-effort touch on each use). The
-- table lives in okt_system alongside sessions / oauth_refresh_tokens
-- because it is system-scoped (a single key is reusable across every
-- repository the user can access; the per-request RBAC check still
-- gates repo data).

CREATE TABLE IF NOT EXISTS okt_system.api_keys (
    id            UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    user_id       UUID NOT NULL REFERENCES okt_system.users(id) ON DELETE CASCADE,
    name          TEXT NOT NULL,
    token_hash    TEXT NOT NULL UNIQUE,                       -- sha256hex("okt_<random>")
    prefix        TEXT NOT NULL,                              -- first 12 chars of the raw token (okt_ + 8), for UI display
    repository_id UUID NULL,                                  -- NULL = all repos the user can access; non-NULL = single repo
    permissions   TEXT[] NOT NULL DEFAULT '{}',               -- array of "object:action" e.g. {"source:read","fact:write"}
    expires_at    TIMESTAMPTZ NULL,                           -- NULL = no expiry
    last_used_at  TIMESTAMPTZ NULL,
    revoked_at    TIMESTAMPTZ NULL,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_api_keys_user_id ON okt_system.api_keys(user_id);
CREATE INDEX IF NOT EXISTS idx_api_keys_expires_at ON okt_system.api_keys(expires_at);