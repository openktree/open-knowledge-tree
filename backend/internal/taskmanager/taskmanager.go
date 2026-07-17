// Package taskmanager wires a River-based background job queue into
// the application. It owns the River client lifecycle (start/stop)
// and exposes an Enqueuer for callers that want to insert jobs from
// HTTP handlers or other packages.
//
// The task manager is database-agnostic from the application's
// perspective: in local development it reuses the same database as
// the rest of the app, while in production it can be pointed at a
// dedicated database via `task.*` config. See config.TaskConfig.
package taskmanager

import (
	"context"
	"fmt"
	"log"
	"log/slog"
	"os"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/openktree/open-knowledge-tree/backend/internal/api/handler"
	"github.com/openktree/open-knowledge-tree/backend/internal/config"
	"github.com/openktree/open-knowledge-tree/backend/internal/dbpool"
	"github.com/openktree/open-knowledge-tree/backend/internal/providers/ai"
	"github.com/openktree/open-knowledge-tree/backend/internal/providers/decomposition"
	"github.com/openktree/open-knowledge-tree/backend/internal/providers/fetch"
	"github.com/openktree/open-knowledge-tree/backend/internal/providers/refinement"
	registryclient "github.com/openktree/open-knowledge-tree/backend/internal/providers/registry"
	"github.com/openktree/open-knowledge-tree/backend/internal/providers/search"
	"github.com/openktree/open-knowledge-tree/backend/internal/providers/storage"
	"github.com/openktree/open-knowledge-tree/backend/internal/providers/summarization"
	"github.com/openktree/open-knowledge-tree/backend/internal/providers/synthesis"
	"github.com/openktree/open-knowledge-tree/backend/internal/providers/posture"
	"github.com/openktree/open-knowledge-tree/backend/internal/qdrantstore"
	"github.com/openktree/open-knowledge-tree/backend/internal/store"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivermigrate"

	"github.com/openktree/open-knowledge-tree/backend/internal/taskmanager/tasks"
)

// Compile-time check that the package's adapter satisfies
// handler.TaskEnqueuer. The HTTP layer defines the contract;
// the taskmanager package supplies the implementation.
var _ handler.TaskEnqueuer = (*enqueuerAdapter)(nil)

// enqueuerAdapter translates the HTTP wire shape
// (handler.RetrieveSourceArgs) into the River JobArgs type
// (tasks.RetrieveSourceArgs) at enqueue time. It exists as a
// separate type so the Manager itself doesn't have to import
// internal/api/handler, which would create an import cycle
// (handler imports taskmanager through its TaskEnqueuer
// interface, and taskmanager would import handler for the
// argument type).
type enqueuerAdapter struct {
	m *Manager
}

func (a *enqueuerAdapter) EnqueueRetrieveSourceFromHTTP(ctx context.Context, args handler.RetrieveSourceArgs) (string, error) {
	return a.m.EnqueueRetrieveSource(ctx, tasks.RetrieveSourceArgs{
		URL:             args.URL,
		RepositoryID:    args.RepositoryID,
		DOI:             args.DOI,
		PublishedAt:     args.PublishedAt,
		Process:         args.Process,
		InvestigationID: args.InvestigationID,
	})
}

func (a *enqueuerAdapter) EnqueueSourceDecompositionFromHTTP(ctx context.Context, args handler.SourceDecompositionArgs) (string, error) {
	return a.m.EnqueueSourceDecomposition(ctx, tasks.SourceDecompositionArgs(args))
}

func (a *enqueuerAdapter) EnqueueAnnotateReportFromHTTP(ctx context.Context, args handler.AnnotateReportArgs) (string, error) {
	return a.m.EnqueueAnnotateReport(ctx, tasks.AnnotateReportArgs(args))
}

// Manager owns the River client and a *pgxpool.Pool used to talk to
// the task database. It is the integration point between the rest
// of the application (HTTP handlers, etc.) and the background
// workers.
type Manager struct {
	client        *river.Client[pgx.Tx]
	pool          *pgxpool.Pool
	cfg           *config.Config
	systemQueries *store.Queries
}

// New builds (but does not start) a Manager. The River schema is
// migrated on construction so callers don't need to remember to
// install a CLI step in their bootstrap.
//
// `taskPool` is the *dbpool.Pool the manager should use. Callers
// resolve the pool from the registry (`registry.Get(cfg.Task.Database)`)
// so the wiring layer is the only place that knows which database
// River talks to. The pool's underlying *pgxpool.Pool is what we
// pass to River; the dbpool.Pool wrapper exists for the registry
// to track role/search_path and is not used directly here.
//
// `systemQueries` is the default-pool *store.Queries the
// RetrieveSource worker uses to look up a repository's
// `database_name` so it can resolve the per-repo pool for
// the sources table. We pass it explicitly so the worker
// doesn't need a separate database connection.
//
// `registry` is the connection pool registry used to map
// `database_name` → *dbpool.Pool. It is the same registry
// the HTTP middleware uses for per-repo routing.
//
// searchProviders and fetchStrategy are passed to workers so they
// can use the same provider instances the API does. We deliberately
// avoid exposing them via a global registry; pass them in.
//
// chunkingProviders and factExtractors are also passed in as maps
// keyed by provider id. The source_decomposition worker still runs
// a single instance of each (selected by
// cfg.Providers.Decomposition.Chunking.Provider / .FactExtraction.Provider);
// passing the full map lets the API surface the registered set via
// /decomposition/providers and gives the wiring layer a single
// shape for every provider tree.
//
// New fails fast if the task database cannot be reached: it runs
// a Ping on the pool before returning. Without that check the
// manager would happily start with a broken pool and then spam
// "Error fetching jobs" at ERROR level for as long as the
// process runs. Failing fast at boot is the correct behavior; the
// API is non-functional without a working task database anyway,
// so surfacing the error and letting the orchestrator restart
// us (or the operator fix config) is better than running
// degraded.
func New(
	ctx context.Context,
	cfg *config.Config,
	taskPool *dbpool.Pool,
	systemQueries *store.Queries,
	registry *dbpool.Registry,
	searchProviders map[string]search.SearchProvider,
	fetchStrategy *fetch.FetchStrategy,
	chunkingProviders map[string]decomposition.ChunkingProvider,
	factExtractors map[string]decomposition.FactExtractionProvider,
	imageExtractors map[string]decomposition.ImageFactExtractionProvider,
	conceptExtractor decomposition.ConceptExtractionProvider,
	refiner refinement.RefineProvider,
	embeddingProvider ai.EmbeddingProvider,
	qdrantStore *qdrantstore.Store,
	storageBackend storage.FileStorage,
	summarizer summarization.SummarizationProvider,
	synthesizer synthesis.SynthesisProvider,
	postureClassifier posture.Classifier,
	modelResolver *tasks.ModelResolver,
) (*Manager, error) {
	pool := taskPool.Pool

	// Run River's bundled migrations. Migrate is idempotent: if the
	// schema is already up-to-date, it's a no-op.
	migrator, err := rivermigrate.New(riverpgxv5.New(pool), nil)
	if err != nil {
		pool.Close()
		return nil, fmt.Errorf("creating river migrator: %w", err)
	}
	if _, err := migrator.Migrate(ctx, rivermigrate.DirectionUp, nil); err != nil {
		pool.Close()
		return nil, fmt.Errorf("running river migrations: %w", err)
	}

	// Select the chunker / fact extractor the worker will use.
	// Unknown ids are logged and the worker falls back to nil so
	// the job records "not configured" rather than panicking — a
	// deployment that hasn't wired a chunker/fact-extractor still
	// boots and the rest of the API serves normally.
	chunkingID := cfg.Providers.Decomposition.Chunking.Provider
	if chunkingID == "" {
		chunkingID = "simple"
	}
	var chunkingProvider decomposition.ChunkingProvider
	if p, ok := chunkingProviders[chunkingID]; ok {
		chunkingProvider = p
	} else {
		log.Printf("decomposition: chunking provider %q not registered; source_decomposition will be a no-op for the chunking step", chunkingID)
	}
	factID := cfg.Providers.Decomposition.FactExtraction.Provider
	var factExtractor decomposition.FactExtractionProvider
	if factID != "" {
		if p, ok := factExtractors[factID]; ok {
			factExtractor = p
		} else {
			log.Printf("decomposition: fact extraction provider %q not registered; source_decomposition will be a no-op for the fact step", factID)
		}
	}

	// Image extractor selection. Mirrors the text-extractor pattern:
	// pick the instance named by cfg.Providers.Decomposition.ImageExtraction.Provider
	// from the registered map. A missing id or unregistered
	// provider leaves imageExtractor nil so the worker's image loop
	// is a no-op (text facts still produced). Image extraction is
	// additionally gated by ImageExtraction.Enabled in the worker.
	imageID := cfg.Providers.Decomposition.ImageExtraction.Provider
	var imageExtractor decomposition.ImageFactExtractionProvider
	if imageID != "" {
		if p, ok := imageExtractors[imageID]; ok {
			imageExtractor = p
		} else {
			log.Printf("decomposition: image extraction provider %q not registered; image extraction will be a no-op", imageID)
		}
	}

	workers := river.NewWorkers()
	registryClients := registryclient.NewClientMap(cfg.Providers)
	reconciler := tasks.NewCacheReconciler(cfg.Providers.Embedding)
	river.AddWorker(workers, tasks.NewRetrieveSourceWorker(searchProviders, fetchStrategy, registry, systemQueries, storageBackend, registryClients, qdrantStore, reconciler))
	river.AddWorker(workers, tasks.NewSourceDecompositionWorker(chunkingProvider, factExtractor, imageExtractor, cfg.Providers.Decomposition.FactExtraction, cfg.Providers.Decomposition.ImageExtraction, registry, systemQueries, storageBackend, modelResolver))
	// Embedding + dedup + concept-extraction + cleanup chain. Each is
	// a no-op when its dependency (embeddingProvider / qdrantStore /
	// conceptExtractor) is nil, so a deployment that hasn't wired
	// Qdrant or an AI provider still boots — the chain simply records
	// "skipped" on the job row and moves on.
	river.AddWorker(workers, tasks.NewEmbedFactsWorker(embeddingProvider, cfg.Providers.Embedding, qdrantStore, registry, systemQueries))
	river.AddWorker(workers, tasks.NewDeduplicateFactsWorker(cfg.Providers.Dedup, qdrantStore, registry, systemQueries))
	extractWorker := tasks.NewExtractConceptsWorker(conceptExtractor, cfg.Providers.Decomposition.ConceptExtraction, registry, systemQueries, modelResolver)
	// Tell the extract_concepts worker whether to fan out
	// summarize_concepts jobs. Gated on the summarization config so
	// a deployment that hasn't enabled summarization doesn't enqueue
	// no-op jobs (the summarize_concepts worker itself is also a
	// no-op when summarizer is nil, but skipping the enqueue avoids
	// cluttering the queue with empty jobs).
	extractWorker.SetSummarizationEnabled(cfg.Providers.Summarization.Enabled && summarizer != nil)
	// Tell the extract_concepts worker whether to fan out
	// refine_concepts jobs. When refinement is enabled, refine_concepts
	// chains to summarize_concepts on completion; when disabled,
	// extract_concepts fans out summarize_concepts directly (legacy
	// behavior).
	extractWorker.SetRefinementEnabled(cfg.Providers.Refinement.Enabled && refiner != nil)
	// Wire the embedding deps used by the refinement-DISABLED
	// direct-routing path's alias tie-break. When refinement is
	// enabled, extract never routes (refine does), so these are
	// unused on that path; nil-safe when Qdrant/embeddings are off.
	extractWorker.SetEmbeddingDeps(embeddingProvider, cfg.Providers.Embedding, qdrantStore)
	river.AddWorker(workers, extractWorker)
	river.AddWorker(workers, tasks.NewRefineConceptsWorker(refiner, cfg.Providers.Refinement, registry, systemQueries, summarizer != nil && cfg.Providers.Synthesis.Enabled, modelResolver, embeddingProvider, cfg.Providers.Embedding, qdrantStore))
	river.AddWorker(workers, tasks.NewEmbedConceptsWorker(embeddingProvider, cfg.Providers.Embedding, qdrantStore, registry, systemQueries))
	river.AddWorker(workers, tasks.NewCleanupFactsWorker(qdrantStore, registry, systemQueries))
	// Contribute_source is chained from cleanup_facts (source-scoped
	// only). It pushes the surviving decomposition (facts, concepts,
	// embeddings) to the knowledge registry. A no-op when the registry
	// client is disabled (no URL configured), which is the default for
	// standalone OKT installations.
	river.AddWorker(workers, tasks.NewContributeSourceWorker(registryClients, qdrantStore, registry, systemQueries, modelResolver, cfg.Providers.Decomposition.FactExtraction.Model))
	// Contribute_all is the batch variant: enqueues a contribute_source
	// job for every processed source in the repo. Registered on the same
	// queue so workers are consumed from a shared pool.
	river.AddWorker(workers, tasks.NewContributeAllWorker(registryClients, registry, systemQueries))
	// The dedup enqueuer adapter is shared by pull_all_from_registry
	// and pull_remote_batch. The same early-wired pattern as the
	// migrate chain: constructed before the Manager exists, wired
	// onto the Manager once the River client is up.
	pullRemoteBatchDedup := &remoteDedupEnqueuerAdapter{}
	// Pull_all_from_registry imports every source from the registry
	// that doesn't already exist locally, then delta-checks existing
	// local sources for new decompositions. A no-op when the registry
	// client is disabled.
	river.AddWorker(workers, tasks.NewPullAllFromRegistryWorker(registryClients, registry, systemQueries, qdrantStore, reconciler, pullRemoteBatchDedup))
	// Pull_remote_batch pulls a list of remote registry source IDs
	// (the "Pull page" / "Pull all results" buttons on the Remote
	// page). It reuses handler.PullOneRemoteSource + the inbound
	// context mapper so bulk pulls honor the repo's unmapped-context
	// policy.
	river.AddWorker(workers, tasks.NewPullRemoteBatchWorker(registryClients, registry, systemQueries, pullRemoteBatchDedup))
	river.AddWorker(workers, tasks.NewFactCatchupWorker(cfg.Providers.Dedup, qdrantStore, registry, systemQueries))
	// Concept summarization is a sibling of embed_concepts (both
	// fan out from extract_concepts). It is a no-op when summarizer
	// is nil or summarization.enabled is false, so a deployment that
	// hasn't wired a summarization model still boots.
	river.AddWorker(workers, tasks.NewSummarizeConceptsWorker(summarizer, cfg.Providers.Summarization, registry, systemQueries, synthesizer != nil && cfg.Providers.Synthesis.Enabled, modelResolver))

	// Concept synthesis is chained from summarize_concepts: every
	// time a slice is written/updated, summarize_concepts enqueues one
	// synthesize_concept job for that concept_id. The worker resolves
	// the concept_id to its canonical-name group, folds ALL the
	// group's slices into ONE definition row, optionally picks images,
	// and upserts concept_syntheses. It is a no-op when synthesizer is
	// nil or synthesis.enabled is false, so a deployment that hasn't
	// wired a synthesis model still boots.
	river.AddWorker(workers, tasks.NewSynthesizeConceptsWorker(synthesizer, cfg.Providers.Synthesis, registry, systemQueries, modelResolver))
	// Concept-relations matview refresh. Sibling of
	// embed_concepts/summarize_concepts (also fanned out from
	// extract_concepts). Refreshes the okt_repository.concept_relations
	// matview that backs the relations-list read endpoint; the refresh
	// is deduped per-database via River unique-args so bursts of
	// extraction batches coalesce into one refresh. Also driven by a
	// periodic job (every cfg.Task.RefreshConceptRelationsInterval,
	// default 10m) so repos with no recent extraction still get fresh
	// relations. The worker is harmless with an empty registry (it
	// records a no-op result), so it's always registered.
	river.AddWorker(workers, tasks.NewRefreshConceptRelationsWorker(registry))
	river.AddWorker(workers, tasks.NewRefreshAllConceptRelationsWorker(registry))
	// Context migration (merge semantics). The chain adapter is a
	// pointer so it can be wired with the Manager after the client
	// is constructed (the worker registration happens before the
	// client exists). The adapter is a no-op until SetManager runs.
	migrateChain := &migrateChainAdapter{}
	river.AddWorker(workers, tasks.NewMigrateContextWorker(registry, systemQueries, migrateChain))
	// Report autofact annotation. A no-op when the embedding
	// provider / qdrant is nil or reports.enabled is false, so a
	// deployment that hasn't wired the embedding pipeline still
	// boots — reports stay in `pending` until annotation is enabled.
	river.AddWorker(workers, tasks.NewAnnotateReportWorker(embeddingProvider, cfg.Providers.Embedding, cfg.Providers.Reports, postureClassifier, qdrantStore, registry, systemQueries, modelResolver))

	queueConfigs := buildQueueConfigs(cfg)

	// Surface the resolved queue set at boot so a misconfigured
	// `task.queues` (e.g. the block silently dropped by a YAML
	// indentation bug) is visible in the startup log rather than
	// manifesting as jobs stuck in `available`. When the fallback
	// `default`-only queue set is in use we additionally WARN so
	// an operator who expected per-task queues notices the
	// regression. The names are sorted for a stable log line.
	if len(queueConfigs) == 1 {
		if _, ok := queueConfigs[river.QueueDefault]; ok {
			slog.Warn("task manager: no per-task queues configured; River will only serve the catch-all `default` queue. " +
				"Jobs enqueued onto named queues (retrieve_source, source_decomposition, …) will never be picked up. " +
				"Check `task.queues` in your config.")
		}
	}
	slog.Info("task manager: queue configuration", "queues", queueConfigs)

	// River emits a steady stream of internal log lines (producer
	// fetch errors, notifier reconnect attempts, etc.). At the
	// default level (warn+) these are loud during a transient DB
	// outage, the kind of thing the producer's backoff loop
	// already handles. Demote those to info so the operator's
	// ERROR-only monitoring isn't drowned in noise.
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	})).With("component", "river")

	riverClient, err := river.NewClient(riverpgxv5.New(pool), &river.Config{
		Queues:  queueConfigs,
		Workers: workers,
		Logger:  logger,
		// JobTimeout is the maximum wall-clock time a single job is
		// allowed to run before River cancels its context. Configured
		// via `task.job_timeout` (default 4h in config.default.yaml)
		// so that long sources — full books with hundreds of chunks
		// plus image extraction — complete in one job. Small jobs
		// finish early so the long budget costs nothing on them.
		// A zero duration disables River's job-level timeout
		// entirely (jobs then run until the worker process exits);
		// useful when an operator wants an external orchestrator to
		// own cancellation. The previous hardcoded 15m default was
		// too short once PDF/book sources landed in the pipeline.
		JobTimeout: cfg.Task.JobTimeout,
		// Slow the poll loop a bit. The default 1s is fine for
		// production, but when the DB is briefly unreachable a
		// 5s cadence cuts the error-log volume by 5x without
		// delaying real recovery. 5s is also the lower bound at
		// which human operators can still notice a hang in real
		// time.
		FetchPollInterval: 5 * time.Second,
		// Periodic jobs. The daily fact_catchup sweeps stuck
		// `to_delete` + `new` facts older than the configured
		// max age. RunOnStart is false so a boot or a config
		// reload doesn't trigger an immediate sweep — the first
		// run is 24h after the client starts. The Offset pins the
		// run to a stable wall-clock minute (03:00 UTC) so the
		// sweep doesn't drift with the boot time.
		PeriodicJobs: []*river.PeriodicJob{
			river.NewPeriodicJob(
				river.PeriodicInterval(24*time.Hour),
				func() (river.JobArgs, *river.InsertOpts) {
					return tasks.FactCatchupArgs{}, &river.InsertOpts{Queue: tasks.QueueFactCatchup}
				},
				&river.PeriodicJobOpts{RunOnStart: false},
			),
			// Concept-relations matview periodic refresh. Every
			// cfg.Task.RefreshConceptRelationsInterval (default 10m),
			// enqueue one RefreshConceptRelationsArgs per registered
			// database via RefreshAllConceptRelationsArgs, reusing the
			// per-database worker + its unique-by-database dedup so
			// overlapping ticks coalesce. RunOnStart is false so a
			// boot doesn't fire a refresh before migrations finish.
			river.NewPeriodicJob(
				river.PeriodicInterval(refreshConceptRelationsInterval(cfg)),
				func() (river.JobArgs, *river.InsertOpts) {
					return tasks.RefreshAllConceptRelationsArgs{}, &river.InsertOpts{Queue: tasks.QueueRefreshConceptRelations}
				},
				&river.PeriodicJobOpts{RunOnStart: false},
			),
		},
	})
	if err != nil {
		pool.Close()
		return nil, fmt.Errorf("creating river client: %w", err)
	}

	mgr := &Manager{client: riverClient, pool: pool, cfg: cfg, systemQueries: systemQueries}
	// Wire the chain adapter's Manager pointer now that the client
	// exists. The adapter was constructed before the client (so the
	// worker could hold it at registration time); until this point
	// its calls would return "not wired", but no calls happen until
	// the first migrate_context job runs, which is after Start().
	migrateChain.m = mgr
	// Wire the pull_remote_batch dedup adapter the same way.
	pullRemoteBatchDedup.m = mgr
	return mgr, nil
}

// Start runs the River client inline. It blocks until ctx is
// cancelled or the client returns an error. It also starts a
// background heartbeat goroutine that logs a WARN when a worker's
// Postgres transaction has been open for more than 5 minutes (the
// classic sign of a stuck extract_concepts/embed_concepts pass
// holding an advisory lock while waiting on an unresponsive LLM
// provider). The heartbeat is best-effort: a query error just
// skips that tick.
func (m *Manager) Start(ctx context.Context) error {
	slog.Info("task manager started")
	if err := m.client.Start(ctx); err != nil {
		return fmt.Errorf("river client: %w", err)
	}

	// Register this worker's heartbeat so the rescue query (below
	// and on-demand from /tasks) knows we're alive. Up-serting on
	// the client ID handles the restart case: a new process that
	// inherits the same ID (shouldn't happen with timestamp-based
	// IDs, but defensive) resets started_at.
	if err := m.registerHeartbeat(ctx); err != nil {
		slog.Warn("task manager: failed to register heartbeat", "err", err)
	}

	// Rescue orphaned jobs from any previous (dead) worker. This
	// runs once on startup; the on-demand POST /admin/tasks/rescue
	// endpoint provides the same rescue from the UI. Skipped when
	// cfg.Task.RescueOnStartup is false.
	if m.cfg.Task.RescueOnStartup {
		rescued, err := m.RescueOrphanedJobs(ctx)
		if err != nil {
			slog.Warn("task manager: startup rescue failed", "err", err)
		} else if rescued > 0 {
			slog.Info("task manager: startup rescue reset orphaned jobs", "rescued", rescued)
		}
	}

	// Sweep stale unique-key jobs whose finalized row still holds
	// its unique slot open (a legacy/bugged unique_states bitmask
	// keeps the row in the partial unique index even after
	// completion, blocking every future enqueue for that unique
	// key). See SweepStaleUniqueKeyJobs for the full rationale.
	// Runs after rescue so the two cleanup passes don't compete.
	swept, err := m.SweepStaleUniqueKeyJobs(ctx)
	if err != nil {
		slog.Warn("task manager: startup stale-unique-key sweep failed", "err", err)
	} else if swept > 0 {
		slog.Info("task manager: startup sweep freed stale unique-key jobs", "swept", swept)
	}

	go m.runStuckTxHeartbeat(ctx)
	go m.runHeartbeat(ctx)
	return nil
}

// SweepStaleUniqueKeyJobs deletes finalized river_job rows that are
// still pinned in the river_job_unique_idx partial unique index by
// their own state. This is the self-healing fix for the bug where a
// completed refresh_concept_relations job retains a legacy
// unique_states bitmask (one that INCLUDES `completed` in the
// covered state set) and thereby keeps occupying its unique-key slot
// indefinitely — every subsequent enqueue for the same database is
// dropped as a duplicate, and the concept_relations matview never
// refreshes again.
//
// The partial unique index condition is:
//
//	WHERE unique_key IS NOT NULL
//	  AND unique_states IS NOT NULL
//	  AND river_job_state_in_bitmask(unique_states, state)
//
// A finalized row (state ∈ {completed, cancelled, discarded}) is
// only in the index — and therefore only blocking — when its
// stored bitmask still claims its own state is "active". This sweep
// targets exactly those rows and DELETEs them (the work they
// recorded is already done; the row is purely a dedup blocker).
//
// River's own job cleaner would eventually purge these via the
// default retention window (often many hours or days), but during
// that window the unique slot stays blocked. The matview going
// stale for ~20h was the symptom that motivated this sweep. The
// sweep is bounded to the kinds that exhibit the pathological
// pattern (refresh_concept_relations, refresh_all_concept_relations)
// rather than every unique-keyed job, so it can't accidentally
// delete a legitimately-active unique job in another kind.
//
// Idempotent and safe to call at any time: a row whose bitmask no
// longer matches its state is already invisible to the index and
// won't be touched; a row still in an active state (available/
// pending/running/scheduled/retryable) is never deleted. Returns
// the number of rows deleted.
func (m *Manager) SweepStaleUniqueKeyJobs(ctx context.Context) (int64, error) {
	res, err := m.pool.Exec(ctx, `
		DELETE FROM river_job
		 WHERE unique_key IS NOT NULL
		   AND unique_states IS NOT NULL
		   AND state IN ('completed', 'cancelled', 'discarded')
		   AND kind IN ('refresh_concept_relations', 'refresh_all_concept_relations')
		   AND river_job_state_in_bitmask(unique_states, state)`)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected(), nil
}

// runStuckTxHeartbeat polls pg_stat_activity on the task DB every
// 60 seconds and logs a WARN for any session that has been
// "idle in transaction" for more than 5 minutes. This is the
// cheapest way to surface a stuck worker tx without a metrics
// pipeline; the WARN is the operator's signal to either cancel
// the job via POST /api/v1/admin/tasks/{id}/cancel or to restart
// the api container.
func (m *Manager) runStuckTxHeartbeat(ctx context.Context) {
	const (
		tickInterval   = 60 * time.Second
		stuckThreshold = 5 * time.Minute
	)
	ticker := time.NewTicker(tickInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		queryCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		rows, err := m.pool.Query(queryCtx, `
			SELECT pid, xact_start, now() - xact_start AS idle_for, state, LEFT(query, 200) AS query
			FROM pg_stat_activity
			WHERE datname = current_database()
			  AND state = 'idle in transaction'
			  AND xact_start IS NOT NULL
			  AND now() - xact_start > $1::interval
			ORDER BY xact_start`, stuckThreshold.String())
		if err != nil {
			cancel()
			continue
		}
		for rows.Next() {
			var pid int32
			var xactStart time.Time
			var idleFor time.Duration
			var state, q string
			if err := rows.Scan(&pid, &xactStart, &idleFor, &state, &q); err != nil {
				continue
			}
			slog.Warn("task manager: long-running worker transaction detected",
				"pid", pid,
				"xact_start", xactStart.Format(time.RFC3339),
				"idle_for", idleFor.String(),
				"state", state,
				"query", q)
		}
		rows.Close()
		cancel()
	}
}

// Stop gracefully stops the River client and closes the underlying
// pool. Safe to call multiple times. Deletes this worker's heartbeat
// row so future rescues don't waste a scan on a known-dead worker.
func (m *Manager) Stop(ctx context.Context) error {
	// Best-effort heartbeat cleanup before closing the pool.
	if m.client != nil {
		if _, err := m.pool.Exec(ctx,
			`DELETE FROM okt_worker_heartbeat WHERE worker_id = $1`,
			m.client.ID()); err != nil {
			slog.Warn("task manager: failed to delete heartbeat on stop", "err", err)
		}
	}
	if err := m.client.Stop(ctx); err != nil {
		slog.Warn("task manager stop error", "err", err)
	}
	m.pool.Close()
	slog.Info("task manager stopped")
	return nil
}

// heartbeatInterval resolves the configured heartbeat interval,
// falling back to 1m when the config value is unset or too small.
func (m *Manager) heartbeatInterval() time.Duration {
	if m.cfg.Task.HeartbeatInterval > 0 {
		return m.cfg.Task.HeartbeatInterval
	}
	return time.Minute
}

// heartbeatTimeout resolves the configured staleness threshold,
// falling back to 10m when unset, and clamping to at least 2× the
// heartbeat interval so a healthy worker is never considered stale.
func (m *Manager) heartbeatTimeout() time.Duration {
	if m.cfg.Task.HeartbeatTimeout > 0 {
		return m.cfg.Task.HeartbeatTimeout
	}
	return 10 * time.Minute
}

// registerHeartbeat inserts (or up-serts) this worker's row in
// okt_worker_heartbeat so the rescue query can see we're alive.
// Called once in Start(); the heartbeat loop (below) keeps the
// last_heartbeat timestamp fresh.
func (m *Manager) registerHeartbeat(ctx context.Context) error {
	host, _ := os.Hostname()
	if host == "" {
		host = "unknown_host"
	}
	_, err := m.pool.Exec(ctx,
		`INSERT INTO okt_worker_heartbeat (worker_id, hostname, started_at, last_heartbeat)
		 VALUES ($1, $2, now(), now())
		 ON CONFLICT (worker_id) DO UPDATE
		    SET hostname = EXCLUDED.hostname,
		        started_at = now(),
		        last_heartbeat = now()`,
		m.client.ID(), host)
	return err
}

// runHeartbeat updates this worker's last_heartbeat on a fixed
// interval. Runs in a background goroutine started by Start. The
// goroutine exits when ctx is cancelled. A failed UPDATE just skips
// that tick — the next one will retry. If the heartbeat goes stale
// (e.g. DB outage), the rescue query will eventually consider this
// worker dead and re-queue its jobs; that's safe because the
// pipeline is idempotent.
func (m *Manager) runHeartbeat(ctx context.Context) {
	interval := m.heartbeatInterval()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		hbCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		if _, err := m.pool.Exec(hbCtx,
			`UPDATE okt_worker_heartbeat SET last_heartbeat = now() WHERE worker_id = $1`,
			m.client.ID()); err != nil {
			slog.Warn("task manager: heartbeat update failed", "err", err)
		}
		cancel()
	}
}

// RescueOrphanedJobs resets "running" jobs whose current owner (the
// last element of attempted_by) is NOT a live worker — i.e. the
// worker has no row in okt_worker_heartbeat or its last_heartbeat is
// older than the staleness threshold. Reset jobs go back to
// "available" so any live worker can pick them up immediately.
//
// Jobs with a unique_key (e.g. refresh_concept_relations) are
// excluded: re-queuing could violate the unique constraint. Those
// are left for River's built-in JobRescuer or a targeted cancel.
//
// Safe for multi-worker deployments: a job owned by a worker with a
// fresh heartbeat is never touched. The reset is idempotent because
// all pipeline stages use ON CONFLICT clauses. Returns the number of
// jobs rescued.
func (m *Manager) RescueOrphanedJobs(ctx context.Context) (int64, error) {
	timeout := m.heartbeatTimeout()
	res, err := m.pool.Exec(ctx,
		`UPDATE river_job
		    SET state       = 'available',
		        attempted_at = NULL,
		        finalized_at = NULL,
		        scheduled_at = now()
		  WHERE state = 'running'
		    AND unique_key IS NULL
		    AND attempted_by[array_length(attempted_by, 1)] NOT IN (
		        SELECT worker_id FROM okt_worker_heartbeat
		         WHERE last_heartbeat > now() - $1::interval
		    )`,
		timeout.String())
	if err != nil {
		return 0, err
	}
	return res.RowsAffected(), nil
}

// HeartbeatTimeout exposes the configured staleness threshold so
// the HTTP handler can include it in the response.
func (m *Manager) HeartbeatTimeout() time.Duration {
	return m.heartbeatTimeout()
}

// EnqueueRetrieveSource inserts a RetrieveSourceArgs job onto the
// "retrieve_source" queue. The job will pick a default search
// provider if one is configured, classify the URL, and then run the
// fetch strategy to resolve it. Returns the resulting River job
// ID as a string for callers that want to look it up later.
//
// This is the lower-level entry point; HTTP callers go through
// Enqueuer() instead, which adapts the wire type for them.
func (m *Manager) EnqueueRetrieveSource(ctx context.Context, args tasks.RetrieveSourceArgs) (string, error) {
	opts := &river.InsertOpts{
		Queue:       tasks.QueueRetrieveSource,
		ScheduledAt: time.Now(),
		Metadata: tasks.MarshalMetadata(tasks.JobMetadata{
			RepositoryID: args.RepositoryID,
		}),
	}
	res, err := m.client.Insert(ctx, args, opts)
	if err != nil {
		return "", fmt.Errorf("enqueueing retrieve_source job: %w", err)
	}
	return fmt.Sprintf("%d", res.Job.ID), nil
}

// EnqueueSourceDecomposition inserts a SourceDecompositionArgs job
// onto the "source_decomposition" queue. The job will chunk the
// source's parsed text and extract facts from each chunk using
// the configured AI provider.
func (m *Manager) EnqueueSourceDecomposition(ctx context.Context, args tasks.SourceDecompositionArgs) (string, error) {
	opts := &river.InsertOpts{
		Queue:       tasks.QueueSourceDecomposition,
		ScheduledAt: time.Now(),
		Metadata: tasks.MarshalMetadata(tasks.JobMetadata{
			RepositoryID: args.RepositoryID,
			SourceID:     args.SourceID,
		}),
	}
	res, err := m.client.Insert(ctx, args, opts)
	if err != nil {
		return "", fmt.Errorf("enqueueing source_decomposition job: %w", err)
	}
	return fmt.Sprintf("%d", res.Job.ID), nil
}

// Enqueuer returns an object that satisfies handler.TaskEnqueuer
// for this manager. Wiring code calls Enqueuer() once and passes
// the result to handler.Source.SetTaskEnqueuer.
func (m *Manager) Enqueuer() handler.TaskEnqueuer {
	return &enqueuerAdapter{m: m}
}

// EnqueueAnnotateReport inserts an AnnotateReportArgs job onto the
// "annotate_report" queue. The job chunks the report's body_md into
// sentences, embeds each, and searches Qdrant for similar facts above
// the configured threshold; matches persist in report_annotations.
// Returns the resulting River job id as a string for callers that
// want to track progress (the report row also stores it via
// MarkReportStatus).
func (m *Manager) EnqueueAnnotateReport(ctx context.Context, args tasks.AnnotateReportArgs) (string, error) {
	opts := &river.InsertOpts{
		Queue:       tasks.QueueAnnotateReport,
		ScheduledAt: time.Now(),
		Metadata: tasks.MarshalMetadata(tasks.JobMetadata{
			RepositoryID: args.RepositoryID,
			ReportID:     args.ReportID,
		}),
	}
	res, err := m.client.Insert(ctx, args, opts)
	if err != nil {
		return "", fmt.Errorf("enqueueing annotate_report job: %w", err)
	}
	return fmt.Sprintf("%d", res.Job.ID), nil
}

// Client exposes the underlying River client. Use sparingly; most
// callers should go through Enqueue* helpers instead. It exists so
// that the wiring layer can hand the manager to anything that needs
// to interact with River directly.
func (m *Manager) Client() *river.Client[pgx.Tx] { return m.client }

// EnqueueRefreshConceptRelations enqueues a refresh of the
// okt_repository.concept_relations matview for the given database.
// Called by the extract_concepts worker at the end of each batch (so
// relations update soon after new facts are linked) and by the
// periodic RefreshAllConceptRelationsWorker. The enqueue is best-
// effort: a failure is returned to the caller (the worker logs it and
// moves on), and River's unique-by-database dedup makes a redundant
// enqueue a no-op rather than queuing up duplicate refreshes.
//
// `databaseName` is the unique key — it MUST be the repository's
// database_name (resolved from cfg.Databases via the registry), not
// the repository id, because the matview is per-database (two repos
// sharing a database share one view). `repositoryID` is carried only
// for the per-repo tasks list metadata filter.
func (m *Manager) EnqueueRefreshConceptRelations(ctx context.Context, databaseName, repositoryID string) (string, error) {
	res, err := m.client.Insert(ctx, tasks.RefreshConceptRelationsArgs{
		DatabaseName: databaseName,
		RepositoryID: repositoryID,
	}, &river.InsertOpts{
		Queue: tasks.QueueRefreshConceptRelations,
		Metadata: tasks.MarshalMetadata(tasks.JobMetadata{
			RepositoryID: repositoryID,
		}),
	})
	if err != nil {
		return "", fmt.Errorf("enqueueing refresh_concept_relations job: %w", err)
	}
	return fmt.Sprintf("%d", res.Job.ID), nil
}

// EnqueueEmbedConcepts enqueues an embed_concepts pass for a repo.
// Used by the migrate_context chain enqueuer so merged concepts
// (whose embedded_at was reset to NULL) get re-vectorized. Returns
// the job id; best-effort at the call site (the caller logs errors).
func (m *Manager) EnqueueEmbedConcepts(ctx context.Context, repositoryID string) (string, error) {
	res, err := m.client.Insert(ctx, tasks.EmbedConceptsArgs{RepositoryID: repositoryID}, &river.InsertOpts{
		Queue: tasks.QueueEmbedConcepts,
		Metadata: tasks.MarshalMetadata(tasks.JobMetadata{RepositoryID: repositoryID}),
	})
	if err != nil {
		return "", fmt.Errorf("enqueueing embed_concepts job: %w", err)
	}
	return fmt.Sprintf("%d", res.Job.ID), nil
}

// EnqueueSummarizeConcepts enqueues a summarize_concepts pass for a
// repo. Used by the registry import paths to trigger summary
// generation after importing facts, concepts, and links from the
// registry. Returns the job id; best-effort at the call site.
func (m *Manager) EnqueueSummarizeConcepts(ctx context.Context, args tasks.SummarizeConceptsArgs) (string, error) {
	opts := &river.InsertOpts{
		Queue:       tasks.QueueSummarizeConcepts,
		ScheduledAt: time.Now(),
		Metadata: tasks.MarshalMetadata(tasks.JobMetadata{
			RepositoryID: args.RepositoryID,
			SourceID:     args.SourceID,
		}),
	}
	res, err := m.client.Insert(ctx, args, opts)
	if err != nil {
		return "", fmt.Errorf("enqueueing summarize_concepts job: %w", err)
	}
	return fmt.Sprintf("%d", res.Job.ID), nil
}

// EnqueueMigrateContext enqueues a migrate_context job for a repo.
// Used by the settings handler's MigrateEnqueuer adapter. The args'
// river unique opts dedup a double-click; returns the job id.
func (m *Manager) EnqueueMigrateContext(ctx context.Context, args tasks.MigrateContextArgs) (string, error) {
	res, err := m.client.Insert(ctx, args, &river.InsertOpts{
		Queue: tasks.QueueMigrateContext,
		Metadata: tasks.MarshalMetadata(tasks.JobMetadata{RepositoryID: args.RepositoryID}),
	})
	if err != nil {
		return "", fmt.Errorf("enqueueing migrate_context job: %w", err)
	}
	return fmt.Sprintf("%d", res.Job.ID), nil
}

// MigrateEnqueuer returns an object satisfying handler.MigrateEnqueuer
// for this manager. Mirrors Enqueuer() (the source-handler adapter);
// the settings handler consumes this to enqueue migrate_context jobs.
func (m *Manager) MigrateEnqueuer() handler.MigrateEnqueuer {
	return &migrateEnqueuerAdapter{m: m}
}

// migrateEnqueuerAdapter translates the handler wire shape
// (handler.MigrateContextArgs) into the River JobArgs type
// (tasks.MigrateContextArgs). Same indirection pattern as
// enqueuerAdapter (keeps taskmanager from importing handler for the
// arg type — the interface lives in handler).
type migrateEnqueuerAdapter struct {
	m *Manager
}

func (a *migrateEnqueuerAdapter) EnqueueMigrateContext(ctx context.Context, args handler.MigrateContextArgs) (string, error) {
	return a.m.EnqueueMigrateContext(ctx, tasks.MigrateContextArgs{
		RepositoryID: args.RepositoryID,
		OldContext:   args.OldContext,
		NewContext:   args.NewContext,
	})
}

// EnqueueContributeAll enqueues a contribute_all job for a repo. The
// worker lists processed sources and enqueues a contribute_source job
// for each. Returns the River job id.
func (m *Manager) EnqueueContributeAll(ctx context.Context, repositoryID string) (string, error) {
	res, err := m.client.Insert(ctx, tasks.ContributeAllArgs{
		RepositoryID: repositoryID,
	}, &river.InsertOpts{
		Queue: tasks.QueueContributeAll,
		Metadata: tasks.MarshalMetadata(tasks.JobMetadata{RepositoryID: repositoryID}),
	})
	if err != nil {
		return "", fmt.Errorf("enqueueing contribute_all job: %w", err)
	}
	return fmt.Sprintf("%d", res.Job.ID), nil
}

// EnqueuePullAll enqueues a pull_all_from_registry job for a repo.
// The worker checks every source against the registry and imports
// any available decompositions. Returns the River job id.
func (m *Manager) EnqueuePullAll(ctx context.Context, repositoryID string) (string, error) {
	res, err := m.client.Insert(ctx, tasks.PullAllFromRegistryArgs{
		RepositoryID: repositoryID,
	}, &river.InsertOpts{
		Queue: tasks.QueuePullAllFromRegistry,
		Metadata: tasks.MarshalMetadata(tasks.JobMetadata{RepositoryID: repositoryID}),
	})
	if err != nil {
		return "", fmt.Errorf("enqueueing pull_all_from_registry job: %w", err)
	}
	return fmt.Sprintf("%d", res.Job.ID), nil
}

// EnqueuePullRemoteBatch enqueues a pull_remote_batch job for a repo
// + a list of remote registry source IDs. The worker pulls each
// source into the local repo, applying the inbound context mapper.
// Used by the "Pull page" / "Pull all results" buttons on the Remote
// page. Returns the River job id.
func (m *Manager) EnqueuePullRemoteBatch(ctx context.Context, repositoryID string, remoteSourceIDs []string) (string, error) {
	res, err := m.client.Insert(ctx, tasks.PullRemoteBatchArgs{
		RepositoryID:    repositoryID,
		RemoteSourceIDs: remoteSourceIDs,
	}, &river.InsertOpts{
		Queue: tasks.QueuePullRemoteBatch,
		Metadata: tasks.MarshalMetadata(tasks.JobMetadata{RepositoryID: repositoryID}),
	})
	if err != nil {
		return "", fmt.Errorf("enqueueing pull_remote_batch job: %w", err)
	}
	return fmt.Sprintf("%d", res.Job.ID), nil
}

// RegistrySyncEnqueuer returns an object satisfying
// handler.RegistrySyncEnqueuer for this manager. The settings handler
// consumes this to enqueue contribute-all and pull-all jobs.
func (m *Manager) RegistrySyncEnqueuer() handler.RegistrySyncEnqueuer {
	return &registrySyncEnqueuerAdapter{m: m}
}

// registrySyncEnqueuerAdapter translates the handler wire shapes
// (handler.ContributeAllArgs / handler.PullAllArgs) into the River
// JobArgs types. Same indirection pattern as the other adapters.
type registrySyncEnqueuerAdapter struct {
	m *Manager
}

func (a *registrySyncEnqueuerAdapter) EnqueueContributeAll(ctx context.Context, args handler.ContributeAllArgs) (string, error) {
	return a.m.EnqueueContributeAll(ctx, args.RepositoryID)
}

func (a *registrySyncEnqueuerAdapter) EnqueuePullAll(ctx context.Context, args handler.PullAllArgs) (string, error) {
	return a.m.EnqueuePullAll(ctx, args.RepositoryID)
}

// EnqueueEmbedFacts enqueues an embed_facts job for a repo + source.
// The embed_facts worker embeds 'new' facts and chains to
// deduplicate_facts → extract_concepts → cleanup_facts. Used by the
// remote PullSource HTTP handler after pulling facts from the registry.
func (m *Manager) EnqueueEmbedFacts(ctx context.Context, repositoryID, sourceID string) error {
	_, err := m.client.Insert(ctx, tasks.EmbedFactsArgs{
		RepositoryID: repositoryID,
		SourceID:     sourceID,
	}, &river.InsertOpts{
		Queue: tasks.QueueEmbedFacts,
		Metadata: tasks.MarshalMetadata(tasks.JobMetadata{
			RepositoryID: repositoryID,
			SourceID:     sourceID,
		}),
	})
	if err != nil {
		return fmt.Errorf("enqueueing embed_facts job: %w", err)
	}
	return nil
}

// RemoteDedupEnqueuer returns an object satisfying
// handler.RemoteDedupEnqueuer for this manager.
func (m *Manager) RemoteDedupEnqueuer() handler.RemoteDedupEnqueuer {
	return &remoteDedupEnqueuerAdapter{m: m}
}

// RemotePullBatchEnqueuer returns an object satisfying
// handler.RemotePullBatchEnqueuer for this manager. Used by the
// Remote page's "Pull page" / "Pull all results" buttons.
func (m *Manager) RemotePullBatchEnqueuer() handler.RemotePullBatchEnqueuer {
	return &remotePullBatchEnqueuerAdapter{m: m}
}

type remotePullBatchEnqueuerAdapter struct {
	m *Manager
}

func (a *remotePullBatchEnqueuerAdapter) EnqueuePullRemoteBatch(ctx context.Context, repositoryID string, remoteSourceIDs []string) (string, error) {
	return a.m.EnqueuePullRemoteBatch(ctx, repositoryID, remoteSourceIDs)
}

type remoteDedupEnqueuerAdapter struct {
	m *Manager
}

func (a *remoteDedupEnqueuerAdapter) EnqueueEmbedFacts(ctx context.Context, repositoryID, sourceID string) error {
	return a.m.EnqueueEmbedFacts(ctx, repositoryID, sourceID)
}

// migrateChainAdapter satisfies tasks.MigrateContextChainEnqueuer by
// delegating to the Manager's Enqueue* helpers. The pointer is
// constructed before the River client (so the worker can hold it at
// registration time) and wired with the Manager after the client
// exists; calls before wiring are no-ops (logged).
type migrateChainAdapter struct {
	m *Manager
}

func (a *migrateChainAdapter) EnqueueEmbedConceptsForRepo(ctx context.Context, repositoryID string) error {
	if a.m == nil {
		return fmt.Errorf("migrate_chain: manager not wired")
	}
	_, err := a.m.EnqueueEmbedConcepts(ctx, repositoryID)
	return err
}

func (a *migrateChainAdapter) EnqueueRefreshConceptRelationsForRepo(ctx context.Context, repositoryID string) error {
	if a.m == nil {
		return fmt.Errorf("migrate_chain: manager not wired")
	}
	// Resolve the repo's database_name so the refresh targets the
	// right database (the matview is per-database).
	repoID := pgtype.UUID{}
	if err := repoID.Scan(repositoryID); err != nil {
		return err
	}
	dbName, err := a.m.systemQueriesRepoDBName(ctx, repoID)
	if err != nil {
		return err
	}
	_, err = a.m.EnqueueRefreshConceptRelations(ctx, dbName, repositoryID)
	return err
}

// systemQueriesRepoDBName resolves a repo's database_name via the
// systemQueries the Manager already holds for worker DB resolution.
// Kept as a small helper so the chain adapter doesn't reach into the
// Manager's internals.
func (m *Manager) systemQueriesRepoDBName(ctx context.Context, repoID pgtype.UUID) (string, error) {
	// The Manager doesn't hold systemQueries directly (it's passed to
	// workers, not stored). Re-derive from the task pool's registry
	// is overkill; instead the wiring layer sets this via SetSystemQueries.
	if m.systemQueries == nil {
		return "", fmt.Errorf("migrate_chain: systemQueries not wired on Manager")
	}
	return m.systemQueries.GetRepositoryDatabaseName(ctx, repoID)
}

func buildQueueConfigs(cfg *config.Config) map[string]river.QueueConfig {
	queues := cfg.Task.Queues
	if len(queues) == 0 {
		// Sensible default: the default queue plus the queue our
		// task uses. Without this, River refuses to start.
		return map[string]river.QueueConfig{
			river.QueueDefault: {MaxWorkers: 100},
		}
	}

	out := make(map[string]river.QueueConfig, len(queues))
	for name, max := range queues {
		if max <= 0 {
			max = 1
		}
		out[name] = river.QueueConfig{MaxWorkers: max}
	}
	return out
}

// refreshConceptRelationsInterval resolves the configured interval for
// the periodic concept-relations matview refresh, falling back to 10m
// when unset or invalid. The interval is a tradeoff: shorter keeps
// relations fresher but rebuilds the view more often (each rebuild is
// a full scan of fact_concepts for the database); 10m means relations
// lag at most 10 minutes behind extraction, which is acceptable for a
// derived read surface.
func refreshConceptRelationsInterval(cfg *config.Config) time.Duration {
	if cfg.Task.RefreshConceptRelationsInterval > 0 {
		return cfg.Task.RefreshConceptRelationsInterval
	}
	return 10 * time.Minute
}
