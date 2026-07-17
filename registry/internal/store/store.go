package store

import (
	"context"

	"github.com/openktree/knowledge-registry/internal/model"
)

type MetadataStore interface {
	// Repositories
	CreateRepository(ctx context.Context, repo *model.Repository) error
	GetRepository(ctx context.Context, id string) (*model.Repository, error)
	ListRepositories(ctx context.Context) ([]model.Repository, error)
	UpdateRepository(ctx context.Context, id string, upd model.RepoUpdate) error
	DeleteRepository(ctx context.Context, id string) error

	// Sources
	IndexSource(ctx context.Context, meta *model.SourceMeta) error
	GetSource(ctx context.Context, repoID, sourceID string) (*model.SourceMeta, error)
	ListSources(ctx context.Context, repoID string, limit, offset int) ([]model.SourceMeta, error)
	ListAllSources(ctx context.Context, limit, offset int) ([]model.SourceMeta, error)

	// Search
	SearchByURL(ctx context.Context, repoID, url string) ([]model.SourceMeta, error)
	SearchByDOI(ctx context.Context, repoID, doi string) ([]model.SourceMeta, error)
	SearchBySHA256(ctx context.Context, repoID, sha256 string) ([]model.SourceMeta, error)
	SearchByText(ctx context.Context, repoID, query string, limit, offset int) ([]model.SourceMeta, error)
	CountByText(ctx context.Context, repoID, query string) (int, error)
	CountAllSources(ctx context.Context) (int, error)

	// Decompositions
	IndexDecomposition(ctx context.Context, meta *model.DecompMeta) error
	ListDecompositions(ctx context.Context, sourceID string) ([]model.DecompMeta, error)
	GetDecompositionBySourceAndModel(ctx context.Context, sourceID, modelID string) (*model.DecompMeta, error)
	// ListAllDecompositions enumerates every decomposition row across
	// all sources, paginated. Used by the one-time embedding-model
	// backfill CLI to rewrite the embedding_model column to the bare
	// model name. limit/offset follow the same conventions as
	// ListAllSources.
	ListAllDecompositions(ctx context.Context, limit, offset int) ([]model.DecompMeta, error)
	// UpdateDecompositionEmbeddingModel rewrites the embedding_model
	// column for a single decomposition row. Used by the backfill
	// CLI; the normal push path doesn't need it because
	// IndexDecomposition's ON CONFLICT clause doesn't touch
	// embedding_model.
	UpdateDecompositionEmbeddingModel(ctx context.Context, id, embeddingModel string) error

	// Fact dedup
	FactHashExists(ctx context.Context, sourceID, hash string) (bool, error)
	InsertFactHash(ctx context.Context, hash, sourceID, decompID, factID string) error
	LinkFactHash(ctx context.Context, hash, sourceID, decompID string) error
	// BatchUpsertFactHashes inserts new fact hashes and re-links
	// existing ones in a single transaction. Replaces the per-fact
	// FactHashExists + InsertFactHash/LinkFactHash loop in the
	// service layer — one tx + one fsync instead of 2N auto-committed
	// queries. Returns the count of new vs linked hashes.
	BatchUpsertFactHashes(ctx context.Context, sourceID, decompID string, entries []model.FactHashEntry) (model.BatchFactHashResult, error)

	// Contexts — the registry's canonical context vocabulary,
	// seeded from the embedded contexts.json snapshot on boot.
	// Read-only via the API (GET /api/v1/contexts); the table is
	// repopulated from the embedded file on every boot so editing
	// contexts.json + restart is the mutation path.
	UpsertContext(ctx context.Context, label, description string) error
	ListContexts(ctx context.Context) ([]model.ContextClass, error)

	// Stats
	Stats(ctx context.Context) (repoCount, sourceCount int, err error)

	// Users
	CreateUser(ctx context.Context, user *model.User) error
	GetUserByEmail(ctx context.Context, email string) (*model.User, error)
	GetUserByID(ctx context.Context, id string) (*model.User, error)
	UpdateUserRole(ctx context.Context, id, role string) error
	ListUsers(ctx context.Context) ([]model.User, error)

	// API tokens
	CreateAPIToken(ctx context.Context, tok *model.APIToken) error
	GetAPITokenByHash(ctx context.Context, hash string) (*model.APIToken, error)
	ListAPITokens(ctx context.Context, userID string) ([]model.APIToken, error)
	RevokeAPIToken(ctx context.Context, id, userID string) error

	Close() error
}
