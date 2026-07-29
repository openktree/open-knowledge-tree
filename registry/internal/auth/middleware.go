package auth

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/openktree/knowledge-registry/internal/config"
	"github.com/openktree/knowledge-registry/internal/model"
)

var roleRank = map[string]int{
	"viewer": 1,
	"editor": 2,
	"admin":  3,
}

func RankRole(role string) int { return roleRank[role] }

// TokenStore is the subset of store.MetadataStore the auth
// middleware needs to resolve opaque API tokens. Keeping it
// narrow avoids importing the full interface (and lets tests
// stub just the two methods).
type TokenStore interface {
	GetAPITokenByHash(ctx context.Context, hash string) (*model.APIToken, error)
	GetUserByID(ctx context.Context, id string) (*model.User, error)
}

type Middleware struct {
	secret string
	cfg    *config.AuthConfig
	store  TokenStore
}

// NewMiddleware builds a middleware that only validates JWTs.
// Use SetStore to also accept opaque API tokens (okr_…).
func NewMiddleware(cfg *config.AuthConfig) *Middleware {
	return &Middleware{secret: cfg.JWTSecret, cfg: cfg}
}

// SetStore enables API-token authentication. When set, a bearer
// that fails JWT parsing is hashed and looked up in the
// api_tokens table; if found (and not expired), the token's
// user + role are placed on the request context just like a JWT
// session. Without a store, the middleware is JWT-only (the
// legacy behavior).
func (m *Middleware) SetStore(s TokenStore) { m.store = s }

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

// resolveToken tries to authenticate the bearer as a JWT first,
// then (if a store is configured) as an opaque API token. On
// success it returns the userID + role to put on the context.
// On failure it returns "" + "" so the caller can emit a 401.
//
// Role is ALWAYS read from the DB (not the JWT claim) when a
// store is configured. This keeps role changes made via /ui/users
// effective immediately — without it, a user promoted to admin
// would have to log out + log back in before the admin-only
// UI (delete buttons, role forms) appears, because their JWT
// still carried the old role snapshot.
func (m *Middleware) resolveToken(ctx context.Context, tokenStr string) (userID, role string) {
	// Fast path: JWT session token. We parse the JWT to get the
	// userID, but discard the role claim if a store is available
	// (the DB is the source of truth for the current role).
	var jwtUserID, jwtRole string
	if claims, err := ParseToken(m.secret, tokenStr); err == nil {
		jwtUserID, jwtRole = claims.UserID, claims.Role
	} else if m.store == nil {
		// No store → can't fall back to API tokens either.
		return "", ""
	}

	if jwtUserID != "" && m.store != nil {
		// JWT authenticated; re-fetch the current role from the
		// DB so role changes take effect without a re-login.
		user, err := m.store.GetUserByID(ctx, jwtUserID)
		if err != nil || user == nil {
			return "", ""
		}
		return user.ID, user.Role
	}

	if jwtUserID != "" {
		// JWT authenticated but no store is configured — trust
		// the role baked into the token (legacy behavior, e.g.
		// unit tests that don't inject a store).
		return jwtUserID, jwtRole
	}

	// Fallback: opaque API token (okr_…). Hash it and look it
	// up. If no store is configured, API tokens are simply not
	// accepted (the JWT-only legacy behavior).
	if m.store == nil {
		return "", ""
	}
	tok, err := m.store.GetAPITokenByHash(ctx, HashToken(tokenStr))
	if err != nil || tok == nil {
		return "", ""
	}
	if tok.ExpiresAt != nil && time.Now().After(*tok.ExpiresAt) {
		return "", ""
	}
	user, err := m.store.GetUserByID(ctx, tok.UserID)
	if err != nil || user == nil {
		return "", ""
	}
	return user.ID, user.Role
}

// AuthRequired extracts a JWT from the Authorization header (or the
// `token` cookie for browser sessions) and puts the user ID and role
// on the request context. Falls back to opaque API tokens (okr_…)
// when a store is configured (see SetStore).
func (m *Middleware) AuthRequired(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tokenStr := extractToken(r)
		if tokenStr == "" {
			http.Error(w, `{"error":"authentication required"}`, http.StatusUnauthorized)
			return
		}
		userID, role := m.resolveToken(r.Context(), tokenStr)
		if userID == "" {
			http.Error(w, `{"error":"invalid or expired token"}`, http.StatusUnauthorized)
			return
		}
		r = r.WithContext(WithUser(r.Context(), userID, role))
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
			userID, role := m.resolveToken(r.Context(), tokenStr)
			if userID == "" {
				http.Error(w, `{"error":"invalid or expired token"}`, http.StatusUnauthorized)
				return
			}
			r = r.WithContext(WithUser(r.Context(), userID, role))
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
