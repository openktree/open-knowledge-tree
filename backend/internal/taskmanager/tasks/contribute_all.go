package tasks

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/openktree/open-knowledge-tree/backend/internal/dbpool"
	"github.com/openktree/open-knowledge-tree/backend/internal/providers/registry"
	"github.com/openktree/open-knowledge-tree/backend/internal/store"
	"github.com/riverqueue/river"
)

const QueueContributeAll = "contribute_source"

type ContributeAllArgs struct {
	RepositoryID string `json:"repository_id"`
}

func (ContributeAllArgs) Kind() string { return "contribute_all" }

func (ContributeAllArgs) InsertOpts() river.InsertOpts { return river.InsertOpts{} }

type ContributeAllResult struct {
	RepositoryID string `json:"repository_id"`
	Enqueued     int    `json:"enqueued"`
}

type ContributeAllWorker struct {
	river.WorkerDefaults[ContributeAllArgs]

	registryClients *registry.ClientMap
	registry        *dbpool.Registry
	systemQueries   *store.Queries
}

func NewContributeAllWorker(
	registryClients *registry.ClientMap,
	registry *dbpool.Registry,
	systemQueries *store.Queries,
) *ContributeAllWorker {
	return &ContributeAllWorker{
		registryClients: registryClients,
		registry:        registry,
		systemQueries:   systemQueries,
	}
}

func (w *ContributeAllWorker) Work(ctx context.Context, job *river.Job[ContributeAllArgs]) error {
	args := job.Args
	if args.RepositoryID == "" {
		return fmt.Errorf("contribute_all: repository_id is required")
	}

	repoID := pgtype.UUID{}
	if err := repoID.Scan(args.RepositoryID); err != nil {
		return fmt.Errorf("contribute_all: invalid repository_id: %w", err)
	}

	// Per-repo gate: no-op when the integration is off or the
	// configured registry is gone. The HTTP gate already rejects
	// the enqueue; this is defense-in-depth for a job enqueued
	// before a toggle-off.
	if _, _, err := resolveRepoRegistryClient(ctx, w.systemQueries, w.registryClients, repoID); err != nil {
		logSkip("contribute_all", args.RepositoryID, err.Error())
		return nil
	}

	dbName, err := w.systemQueries.GetRepositoryDatabaseName(ctx, repoID)
	if err != nil {
		return fmt.Errorf("contribute_all: resolving repository database: %w", err)
	}
	pool := w.registry.Get(dbName)
	if pool == nil || pool.Pool == nil {
		return fmt.Errorf("contribute_all: no pool for database %q", dbName)
	}

	sourceIDs, err := w.listProcessedSourceIDs(ctx, pool.Pool)
	if err != nil {
		return fmt.Errorf("contribute_all: listing processed sources: %w", err)
	}

	client := river.ClientFromContext[pgx.Tx](ctx)
	if client == nil {
		return fmt.Errorf("contribute_all: no river client on context")
	}

	enqueued := 0
	for _, srcID := range sourceIDs {
		chainCtx, chainCancel := context.WithTimeout(context.Background(), 15*time.Second)
		_, err := client.Insert(chainCtx, ContributeSourceArgs{
			RepositoryID: args.RepositoryID,
			SourceID:     srcID,
		}, &river.InsertOpts{
			Queue: QueueContributeSource,
			Metadata: MarshalMetadata(JobMetadata{
				RepositoryID: args.RepositoryID,
				SourceID:     srcID,
			}),
		})
		chainCancel()
		if err != nil {
			log.Printf("contribute_all: enqueueing contribute_source for source %s: %v", srcID, err)
			continue
		}
		enqueued++
	}

	log.Printf("contribute_all: repo %s enqueued %d contribute_source jobs (%d total processed sources)",
		args.RepositoryID, enqueued, len(sourceIDs))
	return river.RecordOutput(ctx, &ContributeAllResult{
		RepositoryID: args.RepositoryID,
		Enqueued:     enqueued,
	})
}

func (w *ContributeAllWorker) listProcessedSourceIDs(ctx context.Context, db pgxpoolLike) ([]string, error) {
	rows, err := db.Query(ctx, `
		SELECT DISTINCT fs.source_id
		FROM okt_repository.facts f
		JOIN okt_repository.fact_sources fs ON fs.fact_id = f.id
		WHERE f.status = 'stable'`)
	if err != nil {
		return nil, fmt.Errorf("querying processed source ids: %w", err)
	}
	defer rows.Close()

	var ids []string
	for rows.Next() {
		var id pgtype.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scanning source id: %w", err)
		}
		ids = append(ids, uuidFromPgtype(id))
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return ids, nil
}

// pgxpoolLike matches both *pgxpool.Pool and pgx.Tx so the worker can
// accept either.
type pgxpoolLike interface {
	Query(ctx context.Context, sql string, args ...interface{}) (pgx.Rows, error)
}
