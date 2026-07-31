package tasks

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
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
// needs from the task manager to enqueue post-import maintenance jobs:
// embed_facts + embed_concepts (when the bundle's embedding model
// doesn't match), recompute_concept_groups (the concept_groups summary
// table the concept LIST endpoint paginates over), and
// refresh_concept_relations (the matview the relations endpoint reads).
// The wiring layer adapts the Manager to this interface (same pattern
// as RemoteDedupEnqueuer).
type GraphImportReembedEnqueuer interface {
	EnqueueEmbedFacts(ctx context.Context, repositoryID, sourceID string) error
	EnqueueEmbedConceptsForRepo(ctx context.Context, repositoryID string) error
	EnqueueRecomputeConceptGroups(ctx context.Context, repositoryID string) error
	EnqueueRefreshConceptRelations(ctx context.Context, repositoryID string) error
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
	taskPool        *pgxpool.Pool // for writing progress to river_job.metadata
	tempDir         string         // "" = OS default for download temp file
}

func NewImportGraphWorker(
	registryClients *registryclient.ClientMap,
	poolRegistry *dbpool.Registry,
	systemQueries *store.Queries,
	qdrant *qdrantstore.Store,
	storageBackend storage.FileStorage,
	embeddingModel string,
	reembedEnqueuer GraphImportReembedEnqueuer,
	taskPool *pgxpool.Pool,
	tempDir string,
) *ImportGraphWorker {
	return &ImportGraphWorker{
		registryClients: registryClients,
		registry:        poolRegistry,
		systemQueries:   systemQueries,
		qdrant:          qdrant,
		storageBackend:  storageBackend,
		embeddingModel:  embeddingModel,
		reembedEnqueuer: reembedEnqueuer,
		taskPool:        taskPool,
		tempDir:         tempDir,
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

	now := time.Now().UTC()

	// Resolve the bundle. For registry-sourced bundles, stream the
	// download to a temp file (avoiding the 8+ GB in-memory buffer
	// that OOM-killed the previous io.ReadAll path). For uploaded
	// bundles, stream from storage to a temp file too (same reason).
	var tmpFile *os.File
	var tmpPath string
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
		pw := newThrottledProgressWriter(ctx, w.taskPool, job.ID)
		updateGraphProgress(ctx, w.taskPool, job.ID, GraphProgress{
			Phase:     "downloading",
			StartedAt: now,
		})
		var dlProg func(bytesTransferred, total int64)
		if w.taskPool != nil {
			dlProg = func(transferred, total int64) {
				defer func() { recover() }()
				pw.update(GraphProgress{
					Phase:            "downloading",
					StartedAt:        now,
					BytesTransferred: transferred,
					TotalBytes:       total,
				})
			}
		}
		f, path, size, err := client.FetchGraphPresignedToStreamWithProgress(ctx, args.RegistryGraphID, w.tempDir, dlProg)
		if err != nil {
			progressPhaseError(ctx, w.taskPool, job.ID, "failed", err.Error())
			return fmt.Errorf("import_graph: fetching graph bundle: %w", err)
		}
		tmpFile = f
		tmpPath = path
		updateGraphProgress(ctx, w.taskPool, job.ID, GraphProgress{
			Phase:        "downloading",
			StartedAt:    now,
			BundleBytes:  size,
			TotalBytes:   size,
			BytesTransferred: size,
		})
	case ImportSourceUpload:
		if w.storageBackend == nil {
			return fmt.Errorf("import_graph: storage backend not configured for upload path")
		}
		pw := newThrottledProgressWriter(ctx, w.taskPool, job.ID)
		updateGraphProgress(ctx, w.taskPool, job.ID, GraphProgress{
			Phase:     "downloading",
			StartedAt: now,
		})
		f, err := w.storageBackend.Get(ctx, args.UploadKey)
		if err != nil {
			progressPhaseError(ctx, w.taskPool, job.ID, "failed", err.Error())
			return fmt.Errorf("import_graph: reading uploaded bundle: %w", err)
		}
		defer f.Body.Close()
		tmp, err := os.CreateTemp(w.tempDir, "okt-import-upload-*.json.gz")
		if err != nil {
			progressPhaseError(ctx, w.taskPool, job.ID, "failed", err.Error())
			return fmt.Errorf("import_graph: creating upload temp file: %w", err)
		}
		tmpPath = tmp.Name()
		// Wrap the temp file in a countingWriter so the UI gets
		// live "N MB downloaded" ticks during the upload-path
		// download (streaming from storage to temp file). Total
		// is unknown (streaming), so TotalBytes=0.
		var copyDst io.Writer = tmp
		if w.taskPool != nil {
			copyDst = newCountingWriter(tmp, 0, func(transferred, total int64) {
				defer func() { recover() }()
				pw.update(GraphProgress{
					Phase:            "downloading",
					StartedAt:        now,
					BytesTransferred: transferred,
					TotalBytes:       total,
				})
			})
		}
		if _, err := io.Copy(copyDst, f.Body); err != nil {
			_ = tmp.Close()
			_ = os.Remove(tmpPath)
			progressPhaseError(ctx, w.taskPool, job.ID, "failed", err.Error())
			return fmt.Errorf("import_graph: streaming upload to temp file: %w", err)
		}
		tmpFile = tmp
		// Best-effort cleanup of the temp upload.
		if dErr := w.storageBackend.Delete(ctx, args.UploadKey); dErr != nil {
			log.Printf("import_graph: deleting temp upload %s: %v", args.UploadKey, dErr)
		}
	default:
		return fmt.Errorf("import_graph: unknown source_kind %q", args.SourceKind)
	}

	defer func() {
		if tmpFile != nil {
			_ = tmpFile.Close()
		}
		if tmpPath != "" {
			_ = os.Remove(tmpPath)
		}
	}()

	// If we have a temp file from the upload path, reopen it for
	// reading (FetchGraphPresignedToStream already returns an open
	// reader; the upload path needs us to seek back to 0).
	var bundleReader io.Reader
	if args.SourceKind == ImportSourceUpload && tmpFile != nil {
		if _, err := tmpFile.Seek(0, io.SeekStart); err != nil {
			progressPhaseError(ctx, w.taskPool, job.ID, "failed", err.Error())
			return fmt.Errorf("import_graph: rewinding upload temp file: %w", err)
		}
		bundleReader = tmpFile
	} else {
		bundleReader = tmpFile
	}

	// Resolve the per-repo pool (needed before streaming import
	// because the importer inserts rows as it streams).
	dbName, err := w.systemQueries.GetRepositoryDatabaseName(ctx, repoID)
	if err != nil {
		progressPhaseError(ctx, w.taskPool, job.ID, "failed", err.Error())
		return fmt.Errorf("import_graph: resolving repository database: %w", err)
	}
	pool := w.registry.Get(dbName)
	if pool == nil || pool.Pool == nil {
		progressPhaseError(ctx, w.taskPool, job.ID, "failed", "no pool for database")
		return fmt.Errorf("import_graph: no pool for database %q", dbName)
	}
	queries := store.New(pool.Pool)

	// Resolve the repo slug for image_url remapping on image facts.
	repo, err := w.systemQueries.GetRepositoryByID(ctx, repoID)
	if err != nil {
		progressPhaseError(ctx, w.taskPool, job.ID, "failed", err.Error())
		return fmt.Errorf("import_graph: resolving repository slug: %w", err)
	}

	// Stream the import: gunzip + decode + insert one entity at a
	// time from the temp file. Peak memory is bounded by the idx→UUID
	// slices + one batch buffer, not the full bundle (which can be
	// 8+ GB). The v2 field order (images/bodies/source_images before
	// facts) means no deferred fixups are needed.
	//
	// Wrap the reader in a countingReader so the UI gets live
	// "N MB of M MB processed (X%)" ticks during the import. The
	// total is the temp file size (stat'd below).
	var importTotal int64
	if fi, statErr := os.Stat(tmpPath); statErr == nil {
		importTotal = fi.Size()
	}
	updateGraphProgress(ctx, w.taskPool, job.ID, GraphProgress{
		Phase:      "importing",
		StartedAt:  now,
		TotalBytes: importTotal,
	})
	pw := newThrottledProgressWriter(ctx, w.taskPool, job.ID)
	var importReader io.Reader = bundleReader
	var importCR *countingReader
	if w.taskPool != nil {
		importCR = newCountingReader(bundleReader, importTotal, func(transferred, total int64) {
			defer func() { recover() }()
			pw.update(GraphProgress{
				Phase:            "importing",
				StartedAt:        now,
				BytesTransferred: transferred,
				TotalBytes:       total,
			})
		})
		importReader = importCR
	}
	mode := graph.ImportModeNew
	if args.Mode == "existing" {
		mode = graph.ImportModeExisting
	}
	importer := graph.NewStreamImporter(queries, w.qdrant, w.storageBackend, repoID, repo.Slug, w.embeddingModel)
	result, err := importer.StreamImport(ctx, importReader, mode)
	if err != nil {
		progressPhaseError(ctx, w.taskPool, job.ID, "failed", err.Error())
		return fmt.Errorf("import_graph: applying bundle: %w", err)
	}
	if importCR != nil {
		importCR.flush()
	}

	// Re-embed if the bundle's embedding model didn't match the local
	// config (or Qdrant isn't configured). The re-embed pass makes the
	// imported facts/concepts searchable via the hybrid search path.
	if result.NeedsReembed && w.reembedEnqueuer != nil {
		if err := w.reembedEnqueuer.EnqueueEmbedConceptsForRepo(ctx, args.RepositoryID); err != nil {
			log.Printf("import_graph: enqueuing embed_concepts for repo %s: %v", args.RepositoryID, err)
		}
		log.Printf("import_graph: repo %s needs fact re-embed; enqueue embed_concepts done, facts await periodic sweep",
			args.RepositoryID)
	}

	// Recompute the concept_groups summary table so the concept LIST
	// endpoint paginates correctly. The import inserts concepts +
	// fact_concepts directly (bypassing the extract_concepts worker
	// that normally maintains concept_groups), so the table is empty
	// until this runs.
	if w.reembedEnqueuer != nil {
		if err := w.reembedEnqueuer.EnqueueRecomputeConceptGroups(ctx, args.RepositoryID); err != nil {
			log.Printf("import_graph: enqueuing recompute_concept_groups for repo %s: %v", args.RepositoryID, err)
		}
		if err := w.reembedEnqueuer.EnqueueRefreshConceptRelations(ctx, args.RepositoryID); err != nil {
			log.Printf("import_graph: enqueuing refresh_concept_relations for repo %s: %v", args.RepositoryID, err)
		}
	}

	log.Printf("import_graph: repo %s imported sources=%d facts=%d concepts=%d summaries=%d syntheses=%d reports=%d investigations=%d reembed=%v",
		args.RepositoryID,
		result.ImportedSources, result.ImportedFacts, result.ImportedConcepts,
		result.ImportedSummaries, result.ImportedSyntheses,
		result.ImportedReports, result.ImportedInvestigations, result.NeedsReembed)

	updateGraphProgress(ctx, w.taskPool, job.ID, GraphProgress{
		Phase:        "completed",
		StartedAt:    now,
		SourceCount:  result.ImportedSources,
		FactCount:    result.ImportedFacts,
		ConceptCount: result.ImportedConcepts,
	})

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
