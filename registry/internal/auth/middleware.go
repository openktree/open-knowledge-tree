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

// AuthRequired extracts a JWT from Authorization: Bearer <token>
// and puts the user ID and role on the request context.
func (m *Middleware) AuthRequired(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tokenStr := r.Header.Get("Authorization")
		if tokenStr == "" || !strings.HasPrefix(tokenStr, "Bearer ") {
			http.Error(w, `{"error":"authentication required"}`, http.StatusUnauthorized)
			return
		}
		claims, err := ParseToken(m.secret, strings.TrimPrefix(tokenStr, "Bearer "))
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
		tokenStr := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		hasToken := tokenStr != "" && tokenStr != r.Header.Get("Authorization")

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
