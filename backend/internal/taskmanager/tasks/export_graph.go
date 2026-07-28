package tasks

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"

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
	IncludeBodies bool     `json:"include_bodies,omitempty"` // embed source PDFs in the bundle (opt-in)
	IncludeImages bool     `json:"include_images"`           // embed source images in the bundle (default true; the handler defaults a missing field to true)
}

func (ExportGraphArgs) Kind() string { return "export_graph" }

func (ExportGraphArgs) InsertOpts() river.InsertOpts {
	// MaxAttempts is capped at 2 so a known-OOM-prone large export
	// doesn't auto-retry into a second host crash. An operator can
	// re-trigger from the UI after fixing the root cause.
	return river.InsertOpts{Queue: QueueExportGraph, MaxAttempts: 2}
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
	tempDir         string // "" = OS default (os.CreateTemp)
}

func NewExportGraphWorker(
	registryClients *registryclient.ClientMap,
	poolRegistry *dbpool.Registry,
	systemQueries *store.Queries,
	qdrant *qdrantstore.Store,
	storageBackend storage.FileStorage,
	embeddingModel string,
	embeddingDims int,
	tempDir string,
) *ExportGraphWorker {
	return &ExportGraphWorker{
		registryClients: registryClients,
		registry:        poolRegistry,
		systemQueries:   systemQueries,
		qdrant:          qdrant,
		storageBackend:  storageBackend,
		embeddingModel:  embeddingModel,
		embeddingDims:   embeddingDims,
		tempDir:         tempDir,
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

	// Build the bundle in streaming mode: two passes through
	// StreamBuild so the metadata.sha256 (the registry's dedup key)
	// is populated in the pushed bytes without materializing the
	// whole ~11 GB bundle in memory.
	//
	// Pass 1: stream to io.Discard with shaOverride="" — computes the
	// canonical sha (over the bytes with embeddings/images/bodies
	// zeroed, matching marshalCanonical) and returns it in
	// stats.SHA256. No temp file written.
	//
	// Pass 2: stream to a temp file with shaOverride=stats.SHA256 —
	// writes the dedup-correct bundle (metadata.sha256 populated) to
	// disk, then push the temp file straight to the registry via
	// PushGraphStream (no []byte buffering).
	builder := graph.NewBundleBuilder(queries, w.qdrant, w.storageBackend, repoID, w.embeddingModel, w.embeddingDims, args.IncludeBodies, args.IncludeImages)
	meta := graph.BundleMetadata{
		Name:        args.Name,
		Description: args.Description,
		Owner:       "", // the registry fills this from the auth email
		Tags:        args.Tags,
	}

	// Pass 1: hash-only.
	pass1Stats, err := builder.StreamBuild(ctx, meta, io.Discard, "")
	if err != nil {
		return fmt.Errorf("export_graph: streaming bundle (pass 1 hash): %w", err)
	}

	// Pass 2: stream to a temp file with the real sha in metadata.
	tmp, err := os.CreateTemp(w.tempDir, "okt-export-*.json.gz")
	if err != nil {
		return fmt.Errorf("export_graph: creating temp file: %w", err)
	}
	tmpName := tmp.Name()
	defer func() {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
	}()
	pass2Stats, err := builder.StreamBuild(ctx, meta, tmp, pass1Stats.SHA256)
	if err != nil {
		return fmt.Errorf("export_graph: streaming bundle (pass 2 write): %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("export_graph: closing temp file: %w", err)
	}

	// Push the temp file. Re-open for reading, then stream to the
	// registry. Content-Length is known (stat the temp file) so the
	// HTTP client can set it (avoids chunked encoding when possible).
	// Indexing metadata travels in X-Graph-* headers (set by
	// PushGraphStream from pushMeta); the body is a pure byte pipe
	// to the registry's S3 — the registry never parses it.
	info, err := os.Stat(tmpName)
	if err != nil {
		return fmt.Errorf("export_graph: stating temp file: %w", err)
	}
	tmpRead, err := os.Open(tmpName)
	if err != nil {
		return fmt.Errorf("export_graph: reopening temp file: %w", err)
	}
	defer tmpRead.Close()
	pushMeta := &registryclient.GraphMeta{
		Name:          args.Name,
		Description:   args.Description,
		Tags:          args.Tags,
		SourceCount:   pass2Stats.SourceCount,
		FactCount:     pass2Stats.FactCount,
		ConceptCount:  pass2Stats.ConceptCount,
		SHA256:        pass2Stats.SHA256,
		SchemaVersion: graph.SchemaVersion,
	}
	result, err := client.PushGraphStream(ctx, pushMeta, tmpRead, info.Size())
	if err != nil {
		return fmt.Errorf("export_graph: pushing graph: %w", err)
	}

	log.Printf("export_graph: repo %s pushed graph %s (sources=%d facts=%d concepts=%d bytes=%d)",
		args.RepositoryID, result.GraphID,
		pass2Stats.SourceCount, pass2Stats.FactCount, pass2Stats.ConceptCount,
		info.Size())

	return river.RecordOutput(ctx, &ExportGraphResult{
		RepositoryID: args.RepositoryID,
		GraphID:      result.GraphID,
		SourceCount:  pass2Stats.SourceCount,
		FactCount:    pass2Stats.FactCount,
		ConceptCount: pass2Stats.ConceptCount,
		Bytes:        int(info.Size()),
	})
}
