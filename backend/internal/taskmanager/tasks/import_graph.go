package tasks

import (
	"context"
	"fmt"
	"io"
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

const QueueImportGraph = "import_graph"

// ImportGraphSourceKind selects where the import reads the bundle from.
const (
	ImportSourceRegistry = "registry" // pull by registry graph id
	ImportSourceUpload   = "upload"   // read from a local storage temp key
)

// ImportGraphArgs triggers a whole-repository graph import: pull a
// shared graph bundle from the registry (or read an uploaded one) and
// re-insert every entity into a fresh (mode="new") or existing
// (mode="existing") repository in a single task. Enqueued by the POST
// /repositories/import-graph (new repo) or POST /{repoID}/import-graph
// (existing repo) handlers.
type ImportGraphArgs struct {
	RepositoryID    string `json:"repository_id"`
	SourceKind      string `json:"source_kind"` // "registry" | "upload"
	RegistryGraphID string `json:"registry_graph_id,omitempty"`
	UploadKey       string `json:"upload_key,omitempty"`
	RegistryID      string `json:"registry_id,omitempty"` // "" = repo's configured registry
	Mode            string `json:"mode"`                  // "new" | "existing"
}

func (ImportGraphArgs) Kind() string { return "import_graph" }

func (ImportGraphArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{Queue: QueueImportGraph}
}

// ImportGraphResult is the outcome of an import job, recorded as the
// River job's output so the HTTP status endpoint can read it back.
type ImportGraphResult struct {
	RepositoryID           string `json:"repository_id"`
	ImportedSources        int    `json:"imported_sources"`
	ImportedFacts          int    `json:"imported_facts"`
	ImportedConcepts       int    `json:"imported_concepts"`
	ImportedSummaries      int    `json:"imported_summaries"`
	ImportedSyntheses      int    `json:"imported_syntheses"`
	ImportedReports        int    `json:"imported_reports"`
	ImportedInvestigations int    `json:"imported_investigations"`
	NeedsReembed           bool   `json:"needs_reembed"`
}

// GraphImportReembedEnqueuer is the minimal contract the import worker
// needs from the task manager to enqueue an embed_facts + embed_concepts
// pass when the bundle's embedding model doesn't match the local
// config. The wiring layer adapts the Manager to this interface (same
// pattern as RemoteDedupEnqueuer).
type GraphImportReembedEnqueuer interface {
	EnqueueEmbedFacts(ctx context.Context, repositoryID, sourceID string) error
	EnqueueEmbedConceptsForRepo(ctx context.Context, repositoryID string) error
}

// ImportGraphWorker pulls a shared graph bundle and re-inserts every
// entity into the target repository. Mirrors PullRemoteBatchWorker:
// holds the registry client map, the dbpool registry, the system
// queries, the Qdrant store, the storage backend (for the upload
// path), and the re-embed enqueuer.
type ImportGraphWorker struct {
	river.WorkerDefaults[ImportGraphArgs]

	registryClients *registryclient.ClientMap
	registry        *dbpool.Registry
	systemQueries   *store.Queries
	qdrant          *qdrantstore.Store
	storageBackend  storage.FileStorage
	embeddingModel  string
	reembedEnqueuer GraphImportReembedEnqueuer
}

func NewImportGraphWorker(
	registryClients *registryclient.ClientMap,
	poolRegistry *dbpool.Registry,
	systemQueries *store.Queries,
	qdrant *qdrantstore.Store,
	storageBackend storage.FileStorage,
	embeddingModel string,
	reembedEnqueuer GraphImportReembedEnqueuer,
) *ImportGraphWorker {
	return &ImportGraphWorker{
		registryClients: registryClients,
		registry:        poolRegistry,
		systemQueries:   systemQueries,
		qdrant:          qdrant,
		storageBackend:  storageBackend,
		embeddingModel:  embeddingModel,
		reembedEnqueuer: reembedEnqueuer,
	}
}

func (w *ImportGraphWorker) Work(ctx context.Context, job *river.Job[ImportGraphArgs]) error {
	args := job.Args
	if args.RepositoryID == "" {
		return fmt.Errorf("import_graph: repository_id is required")
	}
	repoID := pgtype.UUID{}
	if err := repoID.Scan(args.RepositoryID); err != nil {
		return fmt.Errorf("import_graph: invalid repository_id: %w", err)
	}

	// Resolve the bundle bytes.
	var gzBundle []byte
	switch args.SourceKind {
	case ImportSourceRegistry:
		regID := args.RegistryID
		if regID == "" {
			regID = "default"
		}
		client, _, ok := w.registryClients.Client(regID)
		if !ok || !client.IsConfigured() {
			return fmt.Errorf("import_graph: registry %q is not configured", regID)
		}
		data, err := client.FetchGraphPresigned(ctx, args.RegistryGraphID)
		if err != nil {
			return fmt.Errorf("import_graph: fetching graph bundle: %w", err)
		}
		gzBundle = data
	case ImportSourceUpload:
		if w.storageBackend == nil {
			return fmt.Errorf("import_graph: storage backend not configured for upload path")
		}
		f, err := w.storageBackend.Get(ctx, args.UploadKey)
		if err != nil {
			return fmt.Errorf("import_graph: reading uploaded bundle: %w", err)
		}
		defer f.Body.Close()
		data, err := io.ReadAll(f.Body)
		if err != nil {
			return fmt.Errorf("import_graph: reading upload body: %w", err)
		}
		gzBundle = data
		// Best-effort cleanup of the temp upload.
		if dErr := w.storageBackend.Delete(ctx, args.UploadKey); dErr != nil {
			log.Printf("import_graph: deleting temp upload %s: %v", args.UploadKey, dErr)
		}
	default:
		return fmt.Errorf("import_graph: unknown source_kind %q", args.SourceKind)
	}

	// Ungzip.
	bundle, err := graph.UnmarshalGzip(gzBundle)
	if err != nil {
		return fmt.Errorf("import_graph: decoding bundle: %w", err)
	}

	// Resolve the per-repo pool.
	dbName, err := w.systemQueries.GetRepositoryDatabaseName(ctx, repoID)
	if err != nil {
		return fmt.Errorf("import_graph: resolving repository database: %w", err)
	}
	pool := w.registry.Get(dbName)
	if pool == nil || pool.Pool == nil {
		return fmt.Errorf("import_graph: no pool for database %q", dbName)
	}
	queries := store.New(pool.Pool)

	// Resolve the repo slug for image_url remapping on image facts.
	repo, err := w.systemQueries.GetRepositoryByID(ctx, repoID)
	if err != nil {
		return fmt.Errorf("import_graph: resolving repository slug: %w", err)
	}

	// Import.
	mode := graph.ImportModeNew
	if args.Mode == "existing" {
		mode = graph.ImportModeExisting
	}
	importer := graph.NewBundleImporter(queries, w.qdrant, w.storageBackend, repoID, repo.Slug, w.embeddingModel)
	result, err := importer.Import(ctx, bundle, mode)
	if err != nil {
		return fmt.Errorf("import_graph: applying bundle: %w", err)
	}

	// Re-embed if the bundle's embedding model didn't match the local
	// config (or Qdrant isn't configured). The re-embed pass makes the
	// imported facts/concepts searchable via the hybrid search path.
	if result.NeedsReembed && w.reembedEnqueuer != nil {
		if err := w.reembedEnqueuer.EnqueueEmbedConceptsForRepo(ctx, args.RepositoryID); err != nil {
			log.Printf("import_graph: enqueuing embed_concepts for repo %s: %v", args.RepositoryID, err)
		}
		// embed_facts is source-scoped; a repo-wide pass isn't directly
		// available. The imported facts are 'stable' with no embeddings;
		// a periodic embed sweep or a manual re-extract picks them up.
		// For MVP we log; a future EnqueueEmbedFactsForRepo helper would
		// close the gap.
		log.Printf("import_graph: repo %s needs fact re-embed; enqueue embed_concepts done, facts await periodic sweep",
			args.RepositoryID)
	}

	log.Printf("import_graph: repo %s imported sources=%d facts=%d concepts=%d summaries=%d syntheses=%d reports=%d investigations=%d reembed=%v",
		args.RepositoryID,
		result.ImportedSources, result.ImportedFacts, result.ImportedConcepts,
		result.ImportedSummaries, result.ImportedSyntheses,
		result.ImportedReports, result.ImportedInvestigations, result.NeedsReembed)

	return river.RecordOutput(ctx, &ImportGraphResult{
		RepositoryID:           args.RepositoryID,
		ImportedSources:        result.ImportedSources,
		ImportedFacts:          result.ImportedFacts,
		ImportedConcepts:       result.ImportedConcepts,
		ImportedSummaries:      result.ImportedSummaries,
		ImportedSyntheses:      result.ImportedSyntheses,
		ImportedReports:        result.ImportedReports,
		ImportedInvestigations: result.ImportedInvestigations,
		NeedsReembed:           result.NeedsReembed,
	})
}
