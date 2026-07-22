// Package middleware contains cross-cutting HTTP middlewares used by the
// API layer. Middlewares are exposed as plain functions (not methods on a
// handler struct) so they are easy to test in isolation and free of
// package-level cycle risks.
package middleware

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"net/http"
	"strings"

	"github.com/openktree/open-knowledge-tree/backend/internal/api/httputil"
	"github.com/openktree/open-knowledge-tree/backend/internal/store"
)

// APIKeyPrefix is the literal prefix every personal API key carries.
// The AuthRequired middleware uses it to dispatch between session-token
// and API-key authentication without a DB round-trip: a Bearer token
// starting with "okt_" is looked up in api_keys; everything else is
// hashed and looked up in sessions.
const APIKeyPrefix = "okt_"

// IsAPIKeyToken returns true when the raw bearer token is a personal
// API key (starts with the okt_ prefix). The check is on the raw token
// string only — it does not validate the key against the DB.
func IsAPIKeyToken(raw string) bool {
	return strings.HasPrefix(raw, APIKeyPrefix)
}

// GenerateAPIKey returns the raw token string (okt_ + base64url(32
// random bytes)) and its sha256 hex hash. The raw token is returned to
// the client exactly once at creation; the hash is what lands in
// api_keys.token_hash. The prefix (first 12 chars: okt_ + 8) is the
// recognizable label the management UI shows for a saved key.
//
// Mirrors oauth.GenerateToken (internal/oauth/token.go:123) and
// auth.HashToken (internal/auth/crypto.go:73) — the storage pattern
// (sha256-hex-at-rest) is shared across sessions, OAuth refresh
// tokens, and API keys.
func GenerateAPIKey() (raw, hash, prefix string, err error) {
	b := make([]byte, 32)
	if _, err = rand.Read(b); err != nil {
		return "", "", "", err
	}
	raw = APIKeyPrefix + base64.RawURLEncoding.EncodeToString(b)
	hash = hashAPIKey(raw)
	prefix = raw[:12] // okt_ + 8 chars
	return raw, hash, prefix, nil
}

// hashAPIKey returns the sha256 hex of the raw API key token (the full
// string with the okt_ prefix). This is the value stored in
// api_keys.token_hash and compared in GetAPIKeyByTokenHash.
func hashAPIKey(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

// APIKeyAuth is the API-key authentication path. It is invoked by
// AuthRequired when the bearer token carries the okt_ prefix. The
// token is hashed (sha256 hex) and looked up in api_keys; an active,
// non-expired row sets the user ID on the request context (the same
// slot AuthRequired uses for sessions) so downstream
// RequirePermission/RequireRepoPermission enforce as usual. The API
// key itself is also attached via httputil.WithAPIKey so the
// permission middlewares can additionally enforce the key's
// (object, action) scope and repository_id restriction.
//
// last_used_at is touched best-effort on a successful lookup: a
// failure to update the column never fails the request (the key is
// valid; the touch is a telemetry convenience).
func APIKeyAuth(queries *store.Queries, next http.HandlerFunc, rawToken string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		hash := hashAPIKey(rawToken)
		key, err := queries.GetAPIKeyByTokenHash(r.Context(), hash)
		if err != nil {
			httputil.WriteError(w, http.StatusUnauthorized, "invalid or revoked api key")
			return
		}

		ctx := httputil.WithUserID(r.Context(), key.UserID)
		ctx = httputil.WithAPIKey(ctx, &key)
		// Best-effort last-used touch. Run inline (not a goroutine) so
		// the touch is visible to the e2e tests that assert on it; the
		// single-row UPDATE is cheap enough that the latency cost is
		// negligible. Errors are swallowed: a touch failure must not
		// fail an otherwise-valid request.
		_ = queries.TouchAPIKeyLastUsed(r.Context(), key.ID)

		next.ServeHTTP(w, r.WithContext(ctx))
	}
}