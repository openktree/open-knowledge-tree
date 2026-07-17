package tasks

import (
	"context"
	"fmt"
	"log"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/openktree/open-knowledge-tree/backend/internal/api/handler"
	"github.com/openktree/open-knowledge-tree/backend/internal/dbpool"
	"github.com/openktree/open-knowledge-tree/backend/internal/providers/registry"
	"github.com/openktree/open-knowledge-tree/backend/internal/store"
	"github.com/riverqueue/river"
)

const QueuePullRemoteBatch = "pull_remote_batch"

// PullRemoteBatchArgs pulls a list of remote registry source IDs
// into the local repository. Enqueued by the POST /remote/pull-batch
// handler ("Pull page" / "Pull all results" buttons in the Remote
// UI). The worker resolves the repo's registry client, builds the
// inbound context mapper (so bulk pulls honor the repo's
// unmapped-context policy), and calls handler.PullOneRemoteSource for
// each ID. A per-source error is logged and skipped; the batch
// continues so one bad source doesn't fail the whole job.
type PullRemoteBatchArgs struct {
	RepositoryID     string   `json:"repository_id"`
	RemoteSourceIDs  []string `json:"remote_source_ids"`
}

func (PullRemoteBatchArgs) Kind() string { return "pull_remote_batch" }

func (PullRemoteBatchArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{Queue: QueuePullRemoteBatch}
}

type PullRemoteBatchResult struct {
	RepositoryID string `json:"repository_id"`
	Pulled       int    `json:"pulled"`
	Skipped      int    `json:"skipped"`
	ImportedFacts int   `json:"imported_facts"`
}

type PullRemoteBatchWorker struct {
	river.WorkerDefaults[PullRemoteBatchArgs]

	registryClients *registry.ClientMap
	registry        *dbpool.Registry
	systemQueries   *store.Queries
	dedupEnqueuer   handler.RemoteDedupEnqueuer
}

func NewPullRemoteBatchWorker(
	registryClients *registry.ClientMap,
	poolRegistry *dbpool.Registry,
	systemQueries *store.Queries,
	dedupEnqueuer handler.RemoteDedupEnqueuer,
) *PullRemoteBatchWorker {
	return &PullRemoteBatchWorker{
		registryClients: registryClients,
		registry:        poolRegistry,
		systemQueries:   systemQueries,
		dedupEnqueuer:   dedupEnqueuer,
	}
}

func (w *PullRemoteBatchWorker) Work(ctx context.Context, job *river.Job[PullRemoteBatchArgs]) error {
	args := job.Args
	if args.RepositoryID == "" {
		return fmt.Errorf("pull_remote_batch: repository_id is required")
	}
	if len(args.RemoteSourceIDs) == 0 {
		return fmt.Errorf("pull_remote_batch: remote_source_ids is required")
	}

	repoID := pgtype.UUID{}
	if err := repoID.Scan(args.RepositoryID); err != nil {
		return fmt.Errorf("pull_remote_batch: invalid repository_id: %w", err)
	}

	// Resolve the repo's registry client (defense-in-depth — the
	// HTTP gate already rejects when the integration is off).
	rc, err := resolveRepoRegistryClient(ctx, w.systemQueries, w.registryClients, repoID)
	if err != nil {
		logSkip("pull_remote_batch", args.RepositoryID, err.Error())
		return river.RecordOutput(ctx, &PullRemoteBatchResult{RepositoryID: args.RepositoryID, Skipped: len(args.RemoteSourceIDs)})
	}

	dbName, err := w.systemQueries.GetRepositoryDatabaseName(ctx, repoID)
	if err != nil {
		return fmt.Errorf("pull_remote_batch: resolving repository database: %w", err)
	}
	pool := w.registry.Get(dbName)
	if pool == nil || pool.Pool == nil {
		return fmt.Errorf("pull_remote_batch: no pool for database %q", dbName)
	}
	queries := store.New(pool.Pool)

	// Build the inbound context mapper so bulk pulls honor the
	// repo's unmapped-context policy (skip | auto_add | catch_all).
	// A repo with no mappings + default skip policy will skip
	// concepts whose registry context isn't in the inbound map.
	mapper, err := NewInboundContextMapper(ctx, w.systemQueries, repoID)
	if err != nil {
		return fmt.Errorf("pull_remote_batch: building context mapper: %w", err)
	}

	// Per-repo pull level (migration 0044). Controls whether the
	// import includes concepts/links/concept-embeddings or only
	// sources + facts + fact embeddings. Defaults to "concepts".
	syncLevels, err := w.systemQueries.GetRepositorySyncLevels(ctx, repoID)
	if err != nil {
		return fmt.Errorf("pull_remote_batch: reading sync levels: %w", err)
	}
	pullFilter := registry.NewSyncLevelFilter(registry.ParseSyncLevel(syncLevels.RegistryPullLevel))

	result := PullRemoteBatchResult{RepositoryID: args.RepositoryID}
	for _, remoteID := range args.RemoteSourceIDs {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("pull_remote_batch: ctx cancelled: %w", err)
		}
		if remoteID == "" {
			result.Skipped++
			continue
		}
		pr, err := handler.PullOneRemoteSource(ctx, handler.RemotePullDeps{
			Client:        rc,
			Queries:       queries,
			SystemQueries: w.systemQueries,
			RepoID:        repoID,
			Mapper:        mapper,
			DedupEnqueuer: w.dedupEnqueuer,
			PullFilter:    pullFilter,
		}, remoteID)
		if err != nil {
			log.Printf("pull_remote_batch: repo %s source %s: %v", args.RepositoryID, remoteID, err)
			result.Skipped++
			continue
		}
		result.Pulled++
		result.ImportedFacts += pr.ImportedFacts
	}

	log.Printf("pull_remote_batch: repo %s pulled=%d skipped=%d imported_facts=%d",
		args.RepositoryID, result.Pulled, result.Skipped, result.ImportedFacts)
	return river.RecordOutput(ctx, &result)
}