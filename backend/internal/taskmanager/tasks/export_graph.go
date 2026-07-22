package tasks

import (
	"context"
	"fmt"
	"log"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/openktree/open-knowledge-tree/backend/internal/dbpool"
	"github.com/openktree/open-knowledge-tree/backend/internal/providers/graph"
	registryclient "github.com/openktree/open-knowledge-tree/backend/internal/providers/registry"
	"github.com/openktree/open-knowledge-tree/backend/internal/providers/storage"
	"github.com/openktree/open-knowledge-tree/backend/internal/qdrantstore"
	"github.com/openktree/open-knowledge-tree/backend/internal/store"
	"github.com/riverqueue/river"
)

const QueueExportGraph = "export_graph"

// ExportGraphArgs triggers a whole-repository graph export: build a
// GraphBundle from the repo's derived layer, gzip it, and push it to
// the shared knowledge registry. Enqueued by the POST /{repoID}/
// export-graph handler. The worker resolves the per-repo pool, runs
// the export queries, fetches Qdrant vectors, and pushes the gzipped
// bundle via the registry client.
type ExportGraphArgs struct {
	RepositoryID  string   `json:"repository_id"`
	RegistryID    string   `json:"registry_id,omitempty"` // "" = repo's configured registry
	Name          string   `json:"name"`
	Description   string   `json:"description,omitempty"`
	Tags          []string `json:"tags,omitempty"`
	IncludeBodies bool     `json:"include_bodies,omitempty"` // embed source PDFs in the bundle
}

func (ExportGraphArgs) Kind() string { return "export_graph" }

func (ExportGraphArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{Queue: QueueExportGraph}
}

// ExportGraphResult is the outcome of an export job, recorded as the
// River job's output so the HTTP status endpoint can read it back.
type ExportGraphResult struct {
	RepositoryID string `json:"repository_id"`
	GraphID      string `json:"graph_id"`
	SourceCount  int    `json:"source_count"`
	FactCount    int    `json:"fact_count"`
	ConceptCount int    `json:"concept_count"`
	Bytes        int    `json:"bytes"`
}

// ExportGraphWorker builds a GraphBundle from a repository and pushes
// it to the registry. Mirrors ContributeSourceWorker: holds the
// registry client map, the dbpool registry (for per-repo pool
// resolution), the system queries (for the database_name lookup), and
// the Qdrant store (for the embeddings section).
type ExportGraphWorker struct {
	river.WorkerDefaults[ExportGraphArgs]

	registryClients *registryclient.ClientMap
	registry        *dbpool.Registry
	systemQueries   *store.Queries
	qdrant          *qdrantstore.Store
	storageBackend  storage.FileStorage
	embeddingModel  string
	embeddingDims   int
}

func NewExportGraphWorker(
	registryClients *registryclient.ClientMap,
	poolRegistry *dbpool.Registry,
	systemQueries *store.Queries,
	qdrant *qdrantstore.Store,
	storageBackend storage.FileStorage,
	embeddingModel string,
	embeddingDims int,
) *ExportGraphWorker {
	return &ExportGraphWorker{
		registryClients: registryClients,
		registry:        poolRegistry,
		systemQueries:   systemQueries,
		qdrant:          qdrant,
		storageBackend:  storageBackend,
		embeddingModel:  embeddingModel,
		embeddingDims:   embeddingDims,
	}
}

func (w *ExportGraphWorker) Work(ctx context.Context, job *river.Job[ExportGraphArgs]) error {
	args := job.Args
	if args.RepositoryID == "" {
		return fmt.Errorf("export_graph: repository_id is required")
	}
	repoID := pgtype.UUID{}
	if err := repoID.Scan(args.RepositoryID); err != nil {
		return fmt.Errorf("export_graph: invalid repository_id: %w", err)
	}

	// Resolve the registry client. Default to the repo's configured
	// registry_id; fall back to "default" when the arg is empty.
	regID := args.RegistryID
	if regID == "" {
		regCfg, err := w.systemQueries.GetRepositoryRegistryConfig(ctx, repoID)
		if err == nil && regCfg.RegistryID != nil && *regCfg.RegistryID != "" {
			regID = *regCfg.RegistryID
		}
	}
	if regID == "" {
		regID = "default"
	}
	client, _, ok := w.registryClients.Client(regID)
	if !ok || !client.IsConfigured() {
		return fmt.Errorf("export_graph: registry %q is not configured", regID)
	}

	// Resolve the per-repo pool.
	dbName, err := w.systemQueries.GetRepositoryDatabaseName(ctx, repoID)
	if err != nil {
		return fmt.Errorf("export_graph: resolving repository database: %w", err)
	}
	pool := w.registry.Get(dbName)
	if pool == nil || pool.Pool == nil {
		return fmt.Errorf("export_graph: no pool for database %q", dbName)
	}
	queries := store.New(pool.Pool)

	// Build the bundle.
	builder := graph.NewBundleBuilder(queries, w.qdrant, w.storageBackend, repoID, w.embeddingModel, w.embeddingDims, args.IncludeBodies)
	bundle, err := builder.Build(ctx, graph.BundleMetadata{
		Name:        args.Name,
		Description: args.Description,
		Owner:       "", // the registry fills this from the auth email
		Tags:        args.Tags,
	})
	if err != nil {
		return fmt.Errorf("export_graph: building bundle: %w", err)
	}

	// Gzip + push.
	gz, err := graph.MarshalGzip(bundle)
	if err != nil {
		return fmt.Errorf("export_graph: gzipping bundle: %w", err)
	}
	result, err := client.PushGraph(ctx, gz)
	if err != nil {
		return fmt.Errorf("export_graph: pushing graph: %w", err)
	}

	log.Printf("export_graph: repo %s pushed graph %s (sources=%d facts=%d concepts=%d bytes=%d)",
		args.RepositoryID, result.GraphID,
		bundle.Metadata.SourceCount, bundle.Metadata.FactCount, bundle.Metadata.ConceptCount,
		len(gz))

	return river.RecordOutput(ctx, &ExportGraphResult{
		RepositoryID: args.RepositoryID,
		GraphID:      result.GraphID,
		SourceCount:  bundle.Metadata.SourceCount,
		FactCount:    bundle.Metadata.FactCount,
		ConceptCount: bundle.Metadata.ConceptCount,
		Bytes:        len(gz),
	})
}
