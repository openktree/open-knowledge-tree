package auth

import (
	"net/http"
	"strings"

	"github.com/openktree/knowledge-registry/internal/config"
)

var roleRank = map[string]int{
	"viewer": 1,
	"editor": 2,
	"admin":  3,
}

func RankRole(role string) int { return roleRank[role] }

type Middleware struct {
	secret string
	cfg    *config.AuthConfig
}

func NewMiddleware(cfg *config.AuthConfig) *Middleware {
	return &Middleware{secret: cfg.JWTSecret, cfg: cfg}
}

// extractToken returns the bearer token from either the
// Authorization header or the `token` cookie (the latter is set by
// the server-rendered UI's /ui/login). API clients should keep
// using the Authorization header; the cookie fallback exists so
// browser sessions on /ui/* work without the client having to
// glue Authorization headers onto every navigation.
func extractToken(r *http.Request) string {
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
		return strings.TrimPrefix(h, "Bearer ")
	}
	if c, err := r.Cookie("token"); err == nil {
		return c.Value
	}
	return ""
}

// AuthRequired extracts a JWT from the Authorization header (or the
// `token` cookie for browser sessions) and puts the user ID and role
// on the request context.
func (m *Middleware) AuthRequired(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tokenStr := extractToken(r)
		if tokenStr == "" {
			http.Error(w, `{"error":"authentication required"}`, http.StatusUnauthorized)
			return
		}
		claims, err := ParseToken(m.secret, tokenStr)
		if err != nil {
			http.Error(w, `{"error":"invalid or expired token"}`, http.StatusUnauthorized)
			return
		}
		r = r.WithContext(WithUser(r.Context(), claims.UserID, claims.Role))
		next.ServeHTTP(w, r)
	})
}

// RequireRole returns a middleware that checks the user's role is at least minRole.
// Usage: r.Use(authMW.RequireRole("admin"))
func (m *Middleware) RequireRole(minRole string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			role := RequestUserRole(r.Context())
			if role == "" || RankRole(role) < RankRole(minRole) {
				http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// OptionalAuth gates access based on auth_mode config:
//   - "open": all requests pass through
//   - "read-open": reads (GET/HEAD/OPTIONS) pass; writes require a valid JWT
//   - "closed": all requests require a valid JWT
//   - Exempts: /health, /api/v1/auth/*
func (m *Middleware) OptionalAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tokenStr := extractToken(r)
		hasToken := tokenStr != ""

		if hasToken {
			claims, err := ParseToken(m.secret, tokenStr)
			if err != nil {
				http.Error(w, `{"error":"invalid or expired token"}`, http.StatusUnauthorized)
				return
			}
			r = r.WithContext(WithUser(r.Context(), claims.UserID, claims.Role))
			next.ServeHTTP(w, r)
			return
		}

		allowRead := r.Method == http.MethodGet || r.Method == http.MethodHead || r.Method == http.MethodOptions
		switch m.cfg.AuthMode {
		case "closed":
			http.Error(w, `{"error":"authentication required"}`, http.StatusUnauthorized)
		case "read-open":
			if allowRead {
				next.ServeHTTP(w, r)
			} else {
				http.Error(w, `{"error":"authentication required"}`, http.StatusUnauthorized)
			}
		default: // "open"
			next.ServeHTTP(w, r)
		}
	})
}
