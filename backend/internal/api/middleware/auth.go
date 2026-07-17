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

// AuthRequired enforces a valid session token on the request. The user ID
// resolved from the session is stored on the request context and can be
// retrieved with httputil.RequestUserID.
func AuthRequired(queries *store.Queries, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tokenStr := r.Header.Get("Authorization")
		if tokenStr == "" || !strings.HasPrefix(tokenStr, "Bearer ") {
			httputil.WriteError(w, http.StatusUnauthorized, "missing authorization header")
			return
		}

		tokenHash := auth.HashToken(tokenStr[7:])
		session, err := queries.GetSessionByTokenHash(r.Context(), tokenHash)
		if err != nil {
			httputil.WriteError(w, http.StatusUnauthorized, "invalid or expired session")
			return
		}

		ctx := httputil.WithUserID(r.Context(), session.UserID)
		next.ServeHTTP(w, r.WithContext(ctx))
	}
}
