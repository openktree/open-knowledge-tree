// Package middleware contains cross-cutting HTTP middlewares used by the
// API layer. Middlewares are exposed as plain functions (not methods on a
// handler struct) so they are easy to test in isolation and free of
// package-level cycle risks.
package middleware

import (
	"net/http"
	"strings"

	"github.com/openktree/open-knowledge-tree/backend/internal/api/httputil"
	"github.com/openktree/open-knowledge-tree/backend/internal/auth"
	"github.com/openktree/open-knowledge-tree/backend/internal/store"
)

// AuthRequired enforces a valid authentication on the request. It
// dispatches on the bearer token's prefix:
//
//   - tokens starting with "okt_" are personal API keys → APIKeyAuth
//     (sha256-hex lookup in api_keys, attaches the key on context so
//     RequirePermission/RequireRepoPermission enforce the key's scope);
//   - everything else is a session opaque token → the original session
//     lookup (sha256-hex lookup in sessions).
//
// Both paths set the same context slot (httputil.WithUserID) so the
// downstream permission middlewares work unchanged. The API-key path
// additionally attaches the key via httputil.WithAPIKey, which the
// permission middlewares read to decide whether to enforce the key's
// per-(object, action) scope on top of the user's RBAC roles.
func AuthRequired(queries *store.Queries, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tokenStr := r.Header.Get("Authorization")
		if tokenStr == "" || !strings.HasPrefix(tokenStr, "Bearer ") {
			httputil.WriteError(w, http.StatusUnauthorized, "missing authorization header")
			return
		}

		raw := tokenStr[7:]
		if IsAPIKeyToken(raw) {
			APIKeyAuth(queries, next, raw).ServeHTTP(w, r)
			return
		}

		tokenHash := auth.HashToken(raw)
		session, err := queries.GetSessionByTokenHash(r.Context(), tokenHash)
		if err != nil {
			httputil.WriteError(w, http.StatusUnauthorized, "invalid or expired session")
			return
		}

		ctx := httputil.WithUserID(r.Context(), session.UserID)
		next.ServeHTTP(w, r.WithContext(ctx))
	}
}