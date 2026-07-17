// Package middleware contains cross-cutting HTTP middlewares used by the
// API layer. Middlewares are exposed as plain functions (not methods on a
// handler struct) so they are easy to test in isolation and free of
// package-level cycle risks.
package middleware

import (
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/openktree/open-knowledge-tree/backend/internal/api/httputil"
	"github.com/openktree/open-knowledge-tree/backend/internal/oauth"
)

// OAuthBearer validates an OAuth 2.1 access token (the self-contained
// HS256 JWT issued by OKT's own authorization server) presented as a
// Bearer token in the Authorization header. On success it places the
// resolved user id (pgtype.UUID) on the request context via
// httputil.WithUserID, the same context slot AuthRequired uses, so
// downstream handlers and the rbac service can read it identically.
//
// This middleware is the resource-server side of the OAuth 2.1 flow:
// it does NOT issue tokens (that's internal/oauth + handler.OAuth) and
// it does NOT consult the sessions table (that's AuthRequired). It
// only verifies the JWT signature + expiry and trusts the claims
// inside. A blocklist for revoked access tokens is out of scope —
// the tokens are short-lived (15m default) so the blast radius of a
// leak is bounded; refresh-token rotation handles longer sessions.
//
// On failure the middleware writes a 401 with a WWW-Authenticate
// header pointing at the RFC 9728 protected-resource metadata URL,
// the spec-mandated hint MCP clients use to discover the
// authorization server.
func OAuthBearer(secret, protectedResourceMetadataURL string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		authz := r.Header.Get("Authorization")
		if authz == "" || !strings.HasPrefix(authz, "Bearer ") {
			writeOAuthUnauthorized(w, protectedResourceMetadataURL, "missing bearer token")
			return
		}
		tokenStr := strings.TrimSpace(authz[len("Bearer "):])
		if tokenStr == "" {
			writeOAuthUnauthorized(w, protectedResourceMetadataURL, "missing bearer token")
			return
		}
		claims, err := oauth.VerifyAccessToken(secret, tokenStr)
		if err != nil {
			writeOAuthUnauthorized(w, protectedResourceMetadataURL, "invalid or expired access token")
			return
		}
		if claims.Scope != oauth.Scope {
			writeOAuthUnauthorized(w, protectedResourceMetadataURL, "token does not carry the required scope")
			return
		}
		var uid pgtype.UUID
		if err := uid.Scan(claims.UserID); err != nil {
			writeOAuthUnauthorized(w, protectedResourceMetadataURL, "token carries an invalid user id")
			return
		}
		ctx := httputil.WithUserID(r.Context(), uid)
		next.ServeHTTP(w, r.WithContext(ctx))
	}
}

// writeOAuthUnauthorized writes a 401 with the RFC 9728
// WWW-Authenticate hint. The `resource_metadata` parameter is the
// full URL of the /.well-known/oauth-protected-resource document;
// MCP clients fetch it to discover the authorization server's
// metadata URL and start the OAuth flow.
func writeOAuthUnauthorized(w http.ResponseWriter, protectedResourceMetadataURL, reason string) {
	w.Header().Set("WWW-Authenticate", `Bearer resource_metadata="`+protectedResourceMetadataURL+`"`)
	httputil.WriteError(w, http.StatusUnauthorized, reason)
}