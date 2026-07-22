// Package httputil provides shared HTTP helpers and request-scoped context
// keys used across the API layer.
package httputil

import (
	"context"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/openktree/open-knowledge-tree/backend/internal/store"
)

// ContextKey is a private type for context keys defined in this package,
// to avoid collisions with keys from other packages.
type ContextKey string

const (
	UserIDKey       ContextKey = "userID"
	RepositoryIDKey ContextKey = "repositoryID"
	// APIKeyKey is set on the request context when the caller
	// authenticated via a personal API key (Bearer okt_...). Its
	// value is an *store.OktSystemApiKey. When absent, the caller
	// used a session token and the RequirePermission/RequireRepoPermission
	// middlewares skip the per-key scope check.
	APIKeyKey ContextKey = "apiKey"
)

// WithUserID returns a copy of ctx carrying the given user ID.
func WithUserID(ctx context.Context, uid pgtype.UUID) context.Context {
	return context.WithValue(ctx, UserIDKey, uid)
}

// RequestUserID returns the user ID stored on the request context, or the
// zero pgtype.UUID if none is present.
func RequestUserID(ctx context.Context) pgtype.UUID {
	uid, _ := ctx.Value(UserIDKey).(pgtype.UUID)
	return uid
}

// WithRepositoryID returns a copy of ctx carrying the given repository ID.
func WithRepositoryID(ctx context.Context, repoID string) context.Context {
	return context.WithValue(ctx, RepositoryIDKey, repoID)
}

// RequestRepositoryID returns the repository ID stored on the request
// context, or "" if none is present.
func RequestRepositoryID(ctx context.Context) string {
	repoID, _ := ctx.Value(RepositoryIDKey).(string)
	return repoID
}

// WithAPIKey returns a copy of ctx carrying the given API key. The
// RequirePermission/RequireRepoPermission middlewares read it via
// RequestAPIKey to enforce the key's (object, action) scope.
func WithAPIKey(ctx context.Context, key *store.OktSystemApiKey) context.Context {
	return context.WithValue(ctx, APIKeyKey, key)
}

// RequestAPIKey returns the API key stored on the request context, or
// nil when the caller authenticated via a session token (the common
// case). Middlewares use the nil-vs-non-nil distinction to skip the
// per-key scope check for session-authenticated requests.
func RequestAPIKey(ctx context.Context) *store.OktSystemApiKey {
	k, _ := ctx.Value(APIKeyKey).(*store.OktSystemApiKey)
	return k
}
