// Package httputil provides shared HTTP helpers and request-scoped context
// keys used across the API layer.
package httputil

import (
	"context"

	"github.com/jackc/pgx/v5/pgtype"
)

// ContextKey is a private type for context keys defined in this package,
// to avoid collisions with keys from other packages.
type ContextKey string

const (
	UserIDKey       ContextKey = "userID"
	RepositoryIDKey ContextKey = "repositoryID"
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
