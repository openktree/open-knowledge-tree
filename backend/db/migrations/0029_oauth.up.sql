-- 0029_oauth.up.sql
--
-- OAuth 2.1 authorization server tables. OKT acts as its own
-- authorization server so that MCP clients (Claude Desktop, etc.)
-- can connect to the OKT MCP server endpoint using the OAuth 2.1
-- Authorization Code + PKCE flow instead of a static API token.
--
-- The tables live in `okt_system` alongside users / sessions /
-- casbin_rule because they are system-scoped (a single client
-- registration is reusable across every repository the user can
-- access; the per-request RBAC check still gates repo data).
--
-- The access token is a self-contained HS256 JWT (no table here);
-- only refresh tokens (opaque, hashed at rest) and the short-lived
-- authorization codes are persisted. RFC 7591 Dynamic Client
-- Registration is supported via `oauth_clients`, which records the
-- client_id and the registered redirect_uri list.

CREATE TABLE IF NOT EXISTS okt_system.oauth_clients (
    id                   UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    client_id            TEXT NOT NULL UNIQUE,
    client_secret_hash   TEXT,                       -- NULL for public clients (PKCE-only)
    redirect_uris        TEXT[] NOT NULL DEFAULT '{}',
    grant_types          TEXT[] NOT NULL DEFAULT ARRAY['authorization_code','refresh_token'],
    response_types       TEXT[] NOT NULL DEFAULT ARRAY['code'],
    token_endpoint_auth_method TEXT NOT NULL DEFAULT 'none',  -- 'none' = public client (PKCE)
    client_name          TEXT NOT NULL DEFAULT '',
    scope                TEXT NOT NULL DEFAULT 'mcp',
    client_id_issued_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_at           TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_oauth_clients_client_id
    ON okt_system.oauth_clients(client_id);

-- Short-lived authorization codes (10 minute TTL by default). The
-- code_challenge + code_challenge_method are stored here so the
-- token endpoint can verify the PKCE verifier without a round-trip
-- to the client. The user_id is the OKT user who authorized the
-- client (resolved by the login + consent screens). One row per
-- code; consumed once by DELETE in the token endpoint.
CREATE TABLE IF NOT EXISTS okt_system.oauth_authorization_codes (
    id                    UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    code_hash              TEXT NOT NULL UNIQUE,       -- SHA-256 hex of the raw code
    client_id             TEXT NOT NULL REFERENCES okt_system.oauth_clients(client_id) ON DELETE CASCADE,
    redirect_uri          TEXT NOT NULL,
    scope                 TEXT NOT NULL DEFAULT 'mcp',
    user_id               UUID NOT NULL REFERENCES okt_system.users(id) ON DELETE CASCADE,
    code_challenge        TEXT NOT NULL,               -- base64url(SHA-256(verifier))
    code_challenge_method TEXT NOT NULL DEFAULT 'S256',
    expires_at            TIMESTAMPTZ NOT NULL,
    created_at            TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_oauth_auth_codes_expires_at
    ON okt_system.oauth_authorization_codes(expires_at);

-- Opaque refresh tokens, stored as SHA-256 hashes (same pattern as
-- sessions.token_hash). Rotation: every refresh issues a new token
-- and deletes the old one, so reuse of a revoked token is
-- detectable (the row is gone). A `revoked` flag is kept for the
-- /revoke endpoint path and for auditing; the normal refresh path
-- deletes the old row.
CREATE TABLE IF NOT EXISTS okt_system.oauth_refresh_tokens (
    id           UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    token_hash   TEXT NOT NULL UNIQUE,
    client_id    TEXT NOT NULL REFERENCES okt_system.oauth_clients(client_id) ON DELETE CASCADE,
    user_id      UUID NOT NULL REFERENCES okt_system.users(id) ON DELETE CASCADE,
    scope        TEXT NOT NULL DEFAULT 'mcp',
    revoked      BOOLEAN NOT NULL DEFAULT FALSE,
    expires_at   TIMESTAMPTZ NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_oauth_refresh_tokens_expires_at
    ON okt_system.oauth_refresh_tokens(expires_at);