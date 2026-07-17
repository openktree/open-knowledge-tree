package middleware

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/openktree/open-knowledge-tree/backend/internal/dbpool"
	"github.com/openktree/open-knowledge-tree/backend/internal/store"
)

// RepoDBCache resolves a repository UUID to the database name
// its data lives in. The lookup is the only thing standing
// between a request and the per-tenant pool the request
// middleware picks, so we cache the result to avoid hitting
// the system database on every request.
//
// The cache is a map keyed by repo UUID (protected by a
// RWMutex for concurrent reads). Each entry carries the
// resolved database name and a TTL; the entry is refreshed
// on miss by a SELECT against the system repositories table.
// The TTL is intentionally short (a few minutes) so a
// tier-upgrade — which updates the row's `database_name`
// column — takes effect on the next request after the TTL
// expires. A direct invalidation is also possible via the
// Invalidate method; the bootstrap / tier-upgrade flows call
// it when they know a row changed.
//
// The cache holds the *store.Queries for the system pool so
// the Get method can issue a SELECT on miss. The Queries type
// is small (a struct wrap of a DBTX) and the SELECT runs
// against the default pool, so the only contention is on the
// single mutex.
type RepoDBCache struct {
	system *store.Queries
	ttl    time.Duration

	mu      sync.RWMutex
	entries map[pgtype.UUID]cacheEntry
}

type cacheEntry struct {
	dbName    string
	expiresAt time.Time
}

// NewRepoDBCache builds an empty cache. `system` is the
// default-pool *store.Queries (the registry's default pool is
// the one the repositories registry lives in for the shared
// tier and for the system database). `ttl` is the per-entry
// time-to-live; zero or negative means "no caching" (every
// request hits the system DB), which is what tests typically
// want.
func NewRepoDBCache(system *store.Queries, ttl time.Duration) *RepoDBCache {
	return &RepoDBCache{
		system:  system,
		ttl:     ttl,
		entries: make(map[pgtype.UUID]cacheEntry),
	}
}

// Get returns the database name for the given repository ID.
// On a cache miss (or expired entry), it consults the system
// pool and caches the result. The error returns from the
// system pool are returned as-is so the caller can decide
// between 500 (database unreachable) and 404 (repository not
// found). A repository that doesn't exist in the registry
// returns pgx.ErrNoRows; the middleware turns that into a
// 500 because at that point the URL is structurally wrong
// (the chi router matched `{repoID}` but the ID has no row).
func (c *RepoDBCache) Get(ctx context.Context, repoID pgtype.UUID) (string, error) {
	if !repoID.Valid {
		return "", errors.New("repo id is not a valid uuid")
	}

	// Fast path: cache hit and entry is still fresh.
	c.mu.RLock()
	entry, ok := c.entries[repoID]
	c.mu.RUnlock()
	if ok && time.Now().Before(entry.expiresAt) {
		return entry.dbName, nil
	}

	// Slow path: cache miss or stale entry. Hit the system
	// database. We use the sqlc-generated
	// GetRepositoryDatabaseName for the SELECT, which only
	// returns the one column we need (the row's
	// `database_name` field). When the row doesn't exist,
	// pgx returns ErrNoRows, which we surface to the caller.
	dbName, err := c.system.GetRepositoryDatabaseName(ctx, repoID)
	if err != nil {
		return "", err
	}

	if c.ttl > 0 {
		c.mu.Lock()
		c.entries[repoID] = cacheEntry{
			dbName:    dbName,
			expiresAt: time.Now().Add(c.ttl),
		}
		c.mu.Unlock()
	}
	return dbName, nil
}

// Invalidate drops the cached entry for a repository. The
// tier-upgrade flow calls this after it has updated the row's
// `database_name` column so the very next request for that
// repository hits the new pool instead of the old one.
//
// Calling Invalidate on an unknown repo is a no-op.
func (c *RepoDBCache) Invalidate(repoID pgtype.UUID) {
	c.mu.Lock()
	delete(c.entries, repoID)
	c.mu.Unlock()
}

// repoPoolKey is the unexported context key for the
// per-request pool. The key type is unexported so external
// packages can't accidentally collide with it.
type repoPoolKey struct{}

// repoIDContextKey is the unexported context key for the
// repository UUID resolved by the slug middleware. Handlers
// read it via RepoIDFromContext as a fallback when the URL
// doesn't carry a {repoID} param.
type repoIDContextKey struct{}

// PoolFromContext returns the *pgxpool.Pool the
// WithRepoQueriesBySlug middleware attached to the request.
// Handlers use it to build a per-request *store.Queries:
// `store.New(middleware.PoolFromContext(r))`.
//
// Returns nil when called on a request that didn't go through
// the middleware (e.g. a test that constructs an http.Request
// directly). Handlers in the per-repository routes never see
// nil because the router only puts them behind the middleware;
// this nil tolerance is a safety net.
func PoolFromContext(ctx context.Context) *pgxpool.Pool {
	if p, ok := ctx.Value(repoPoolKey{}).(*pgxpool.Pool); ok {
		return p
	}
	return nil
}

// RepoIDFromContext returns the repository UUID set on the
// request context by WithRepoQueriesBySlug, and a boolean
// indicating whether it was present. Handlers use this as a
// fallback when the URL doesn't carry a {repoID} param
// (i.e. the request came through the /{slug}/sources route).
func RepoIDFromContext(ctx context.Context) (pgtype.UUID, bool) {
	id, ok := ctx.Value(repoIDContextKey{}).(pgtype.UUID)
	return id, ok
}

// SlugCache resolves a repository slug to the repository UUID.
// It mirrors RepoDBCache but is keyed by slug string instead
// of UUID. The cache is populated on miss by a
// GetRepositoryBySlug query against the system pool.
type SlugCache struct {
	system *store.Queries
	ttl    time.Duration

	mu      sync.RWMutex
	entries map[string]slugCacheEntry
}

type slugCacheEntry struct {
	repoID    pgtype.UUID
	expiresAt time.Time
}

// NewSlugCache builds an empty slug→repoID cache.
func NewSlugCache(system *store.Queries, ttl time.Duration) *SlugCache {
	return &SlugCache{
		system:  system,
		ttl:     ttl,
		entries: make(map[string]slugCacheEntry),
	}
}

// Get returns the repository UUID for the given slug.
func (c *SlugCache) Get(ctx context.Context, slug string) (pgtype.UUID, error) {
	c.mu.RLock()
	entry, ok := c.entries[slug]
	c.mu.RUnlock()
	if ok && time.Now().Before(entry.expiresAt) {
		return entry.repoID, nil
	}

	repo, err := c.system.GetRepositoryBySlug(ctx, slug)
	if err != nil {
		return pgtype.UUID{}, err
	}

	if c.ttl > 0 {
		c.mu.Lock()
		c.entries[slug] = slugCacheEntry{
			repoID:    repo.ID,
			expiresAt: time.Now().Add(c.ttl),
		}
		c.mu.Unlock()
	}
	return repo.ID, nil
}

// Invalidate drops the cached entry for a slug.
func (c *SlugCache) Invalidate(slug string) {
	c.mu.Lock()
	delete(c.entries, slug)
	c.mu.Unlock()
}

// WithRepoQueries is an http middleware that resolves a
// {repoID} URL parameter to the repository's database pool.
// The param may be a UUID or a slug; the middleware tries
// UUID resolution first (via RepoDBCache), then falls back
// to slug resolution (via SlugCache). On success it attaches
// both the *pgxpool.Pool and the repository UUID to the
// request context. Handlers retrieve the pool with
// PoolFromContext and the repo UUID with RepoIDFromContext.
//
// Requests without a {repoID} parameter pass through
// unchanged. When the param doesn't resolve to a repository,
// the middleware writes a 404 and aborts.
func WithRepoQueries(registry *dbpool.Registry, repoDBCache *RepoDBCache, slugCache *SlugCache) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			raw := chi.URLParam(r, "repoID")
			if raw == "" {
				next.ServeHTTP(w, r)
				return
			}

			var repoID pgtype.UUID
			var err error

			// Try UUID first.
			if scanErr := repoID.Scan(raw); scanErr == nil {
				_, err = repoDBCache.Get(r.Context(), repoID)
			} else {
				// Not a UUID — treat as slug.
				repoID, err = slugCache.Get(r.Context(), raw)
			}

			if err != nil {
				http.Error(w, "repository not found", http.StatusNotFound)
				return
			}

			dbName, err := repoDBCache.Get(r.Context(), repoID)
			if err != nil {
				http.Error(w, "resolving repository database: "+err.Error(), http.StatusInternalServerError)
				return
			}

			pool := registry.Get(dbName)
			ctx := context.WithValue(r.Context(), repoPoolKey{}, pool.Pool)
			ctx = context.WithValue(ctx, repoIDContextKey{}, repoID)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

