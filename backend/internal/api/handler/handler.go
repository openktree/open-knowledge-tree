// Package handler contains the HTTP handler implementations for the API,
// grouped by domain (auth, user, admin, repository, source). Handlers are
// exposed as plain functions and structs that receive only the
// dependencies they need, so they are easy to compose and test.
package handler

import (
	"context"

	"github.com/openktree/open-knowledge-tree/backend/internal/audit"
	"github.com/openktree/open-knowledge-tree/backend/internal/config"
	"github.com/openktree/open-knowledge-tree/backend/internal/dbpool"
	"github.com/openktree/open-knowledge-tree/backend/internal/providers/ai"
	"github.com/openktree/open-knowledge-tree/backend/internal/providers/ontology"
	"github.com/openktree/open-knowledge-tree/backend/internal/qdrantstore"
	"github.com/openktree/open-knowledge-tree/backend/internal/rbac"
	"github.com/openktree/open-knowledge-tree/backend/internal/store"
)

// Deps bundles the shared dependencies passed to each handler constructor.
//
// `Store` is the default-pool Queries (used for system tables: users,
// sessions, casbin_rule, the repositories registry). Repository-scoped
// queries that need to hit a non-default pool build a per-request
// `store.Queries` from `Registry.Get(repo.DatabaseName)` and use that
// instead.
//
// `Registry` is the connection-pool registry; the schema-aware
// repository handler uses it to resolve the pool for a given
// repository's database_name.
//
// `LazyEnsureRepository` is an optional callback the repository
// handler invokes from GET /repositories when the caller owns no
// repositories and the bootstrap flag is enabled. The function
// signature mirrors bootstrap.EnsureDefaultRepository's
// post-refactor form (no return value: errors are logged and
// swallowed so a misconfigured bootstrap never breaks the list
// endpoint). The wiring layer (cmd/app/api.go) sets this on the
// Handler after the bootstrap is configured; tests that don't
// exercise the lazy path simply leave it nil and the handler
// short-circuits.
type Deps struct {
	Store               *store.Queries
	Config              *config.Config
	RBAC                *rbac.Service
	Groups              *rbac.GroupManager
	Users               *rbac.UserManager
	Registry            *dbpool.Registry
	LazyEnsureRepository func(ctx context.Context, ownerID string) error

	// Audit is the recorder for okt_system.permission_audit. Nil in
	// tests that don't exercise the audit pipeline; handlers guard
	// with a nil check before calling Audit.RecordAsync. Wired by
	// the api.Handler.SetAudit path (cmd/app/api.go builds the
	// PostgresRecorder from the system pool).
	Audit audit.Recorder

	// ProviderRegistry is the live provider catalog the
	// CreateRepository seeding iterates and the gate intersects
	// stored settings with. Wired by the api.Handler.SetSource
	// path (cmd/app/api.go builds it from the same maps passed to
	// NewSource). Nil in tests that don't exercise seeding; the
	// seeding helper treats a nil registry as "no providers to
	// seed" and the gate treats it as "no enforcement".
	ProviderRegistry *ProviderRegistry

	// OntologySource is the embedded curated context vocabulary
	// that CreateRepository seeding reads to populate
	// repository_contexts with is_custom=FALSE rows. Always
	// EmbeddedL3Source in production. Wired alongside ProviderRegistry.
	OntologySource ontology.L3Source

	// Qdrant is the vector store used by the hybrid search path
	// (fact/concept search fuses lexical tsvector results with
	// Qdrant cosine similarity via Reciprocal Rank Fusion). Nil
	// when Qdrant is not configured at boot (the search endpoints
	// degrade to lexical-only). Wired by the api.Handler.SetQdrant
	// path (cmd/app/api.go passes the same *qdrantstore.Store it
	// builds for the taskmanager).
	Qdrant *qdrantstore.Store

	// EmbeddingProvider is the bulk-embed client used by the
	// hybrid search path to embed the caller's query string before
	// querying Qdrant. Nil when no embedding provider is configured
	// at boot (chat-only AI configs). Wired by the
	// api.Handler.SetEmbeddingProvider path; in production it is
	// the same provider instance passed to NewAI / the embed_facts
	// worker.
	EmbeddingProvider ai.EmbeddingProvider

	// DefaultSettingsSeeder seeds the per-repository provider +
	// context settings for a freshly-created repository using the
	// default-preset resolution path (the same path CreateRepository
	// uses when the create body omits `preset`). It is wired by the
	// api.Handler.SetOntologySource step (once the ProviderRegistry
	// and OntologySource are both in place) so the lazy
	// default-repository bootstrap (wired in NewHandler) can seed a
	// functional default repo out of the box — without this, the
	// bootstrapped default repo has no settings rows and every
	// search/retrieve/extract gate denies the request ("search
	// provider not enabled for this repository"). Nil until
	// SetOntologySource runs; tests that don't exercise the lazy
	// bootstrap leave it nil and EnsureDefaultRepository skips
	// seeding (the legacy behavior).
	DefaultSettingsSeeder func(ctx context.Context, repoID string) error
}
