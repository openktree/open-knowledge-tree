package tasks

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/openktree/open-knowledge-tree/backend/internal/dbpool"
	"github.com/openktree/open-knowledge-tree/backend/internal/qdrantstore"
	"github.com/openktree/open-knowledge-tree/backend/internal/store"
	"github.com/riverqueue/river"
)

const QueueCleanupFacts = "cleanup_facts"

// CleanupFactsArgs triggers a delete pass for a repository's
// `to_delete` facts. The worker deletes the matching Qdrant
// points and the Postgres rows (junction cascades). Idempotent:
// re-running after a previous cleanup is a no-op (no `to_delete`
// rows left).
//
// SourceID, when non-empty, narrows the pass to `to_delete` facts
// linked to this source. After a cross-source dedup merge the loser's
// fact_sources rows still point at its original sources, so the
// source-scoped pass finds facts originally from this source; a
// to_delete fact from a different source is cleaned up by that
// source's pass or a repo-wide sweep. Empty SourceID means repo-wide.
type CleanupFactsArgs struct {
	RepositoryID string `json:"repository_id"`
	SourceID     string `json:"source_id,omitempty"`
}

func (CleanupFactsArgs) Kind() string { return "cleanup_facts" }

func (CleanupFactsArgs) InsertOpts() river.InsertOpts { return river.InsertOpts{} }

type CleanupFactsResult struct {
	RepositoryID string `json:"repository_id"`
	Deleted      int    `json:"deleted"`
}

type CleanupFactsWorker struct {
	river.WorkerDefaults[CleanupFactsArgs]

	qdrant        *qdrantstore.Store
	registry      *dbpool.Registry
	systemQueries *store.Queries
}

func NewCleanupFactsWorker(
	qdrant *qdrantstore.Store,
	registry *dbpool.Registry,
	systemQueries *store.Queries,
) *CleanupFactsWorker {
	return &CleanupFactsWorker{
		qdrant:        qdrant,
		registry:      registry,
		systemQueries: systemQueries,
	}
}

// Work resolves the per-repo pool, fetches the repo's
// `to_delete` fact ids, deletes them from Qdrant, then deletes
// the Postgres rows (the fact_sources junction cascades on
// DELETE). Qdrant delete is best-effort relative to Postgres
// delete: a Qdrant failure is logged and the Postgres rows are
// still deleted, so a re-run of the dedup pass won't re-find
// them; the orphaned Qdrant points are reaped by the daily
// fact_catchup periodic job (which sweeps by payload status,
// not by Postgres id).
func (w *CleanupFactsWorker) Work(ctx context.Context, job *river.Job[CleanupFactsArgs]) error {
	args := job.Args
	if args.RepositoryID == "" {
		return fmt.Errorf("cleanup_facts: repository_id is required")
	}

	repoID := pgtype.UUID{}
	if err := repoID.Scan(args.RepositoryID); err != nil {
		return fmt.Errorf("cleanup_facts: invalid repository_id: %w", err)
	}

	var srcID pgtype.UUID
	sourceScoped := false
	if args.SourceID != "" {
		if err := srcID.Scan(args.SourceID); err != nil {
			return fmt.Errorf("cleanup_facts: invalid source_id: %w", err)
		}
		sourceScoped = true
	}

	dbName, err := w.systemQueries.GetRepositoryDatabaseName(ctx, repoID)
	if err != nil {
		return fmt.Errorf("cleanup_facts: resolving repository database: %w", err)
	}
	pool := w.registry.Get(dbName)
	queries := store.New(pool.Pool)

	var ids []pgtype.UUID
	if sourceScoped {
		ids, err = queries.ListFactsToDeleteBySource(ctx, store.ListFactsToDeleteBySourceParams{
			RepositoryID: repoID,
			SourceID:     srcID,
		})
		if err != nil {
			return fmt.Errorf("cleanup_facts: listing to_delete facts by source: %w", err)
		}
	} else {
		ids, err = queries.ListFactsToDelete(ctx, repoID)
		if err != nil {
			return fmt.Errorf("cleanup_facts: listing to_delete facts: %w", err)
		}
	}
	if len(ids) == 0 {
		// No to_delete facts for this source. Still chain to
		// contribute_source: surviving facts are already stable.
		w.chainContributeSource(ctx, args.RepositoryID, args.SourceID)
		return river.RecordOutput(ctx, &CleanupFactsResult{RepositoryID: args.RepositoryID})
	}

	// Delete from Qdrant first (best-effort). We collect the
	// UUIDs up front so a Qdrant failure doesn't block the
	// Postgres cleanup.
	qdrantIDs := make([]uuid.UUID, 0, len(ids))
	for _, id := range ids {
		s := pgUUIDToString(id)
		if s == "" {
			continue
		}
		u, err := uuid.Parse(s)
		if err != nil {
			continue
		}
		qdrantIDs = append(qdrantIDs, u)
	}
	if w.qdrant != nil && len(qdrantIDs) > 0 {
		if err := w.qdrant.DeleteFactVectors(ctx, qdrantIDs); err != nil {
			log.Printf("cleanup_facts: deleting qdrant points for repo %s: %v", args.RepositoryID, err)
		}
	}

	deleted := 0
	for _, id := range ids {
		if err := queries.DeleteFactByID(ctx, id); err != nil {
			log.Printf("cleanup_facts: deleting fact %s: %v", pgUUIDToString(id), err)
			continue
		}
		deleted++
	}

	log.Printf("cleanup_facts: repo %s deleted %d facts", args.RepositoryID, deleted)
	w.chainContributeSource(ctx, args.RepositoryID, args.SourceID)
	return river.RecordOutput(ctx, &CleanupFactsResult{
		RepositoryID: args.RepositoryID,
		Deleted:      deleted,
	})
}

// chainContributeSource enqueues a contribute_source job for the
// source when the args carry a SourceID (source-scoped cleanup)
// and the repo has opted into auto-contribute. A repo-wide
// cleanup (SourceID empty) is skipped — contribute_source always
// targets a single source. When auto_contribute is false (the
// default), the chain is skipped so sources are only pushed to
// the registry via the manual "Push All to Registry" endpoint.
// Disabling the flag does not affect jobs already enqueued.
func (w *CleanupFactsWorker) chainContributeSource(ctx context.Context, repositoryID, sourceID string) {
	if sourceID == "" {
		return
	}
	repoID := pgtype.UUID{}
	if err := repoID.Scan(repositoryID); err != nil {
		log.Printf("cleanup_facts: parsing repo id %q for contribute chain: %v", repositoryID, err)
		return
	}
	autoContribute, err := w.systemQueries.GetRepositoryAutoContribute(ctx, repoID)
	if err != nil {
		log.Printf("cleanup_facts: reading auto_contribute for repo %s: %v", repositoryID, err)
		return
	}
	if !autoContribute {
		return
	}
	// Defense-in-depth: auto_contribute can only be enabled when
	// registry_enabled is true (the HTTP gate enforces it), but a
	// repo admin could toggle the integration off after enabling
	// auto_contribute. Re-check here so we don't enqueue a job that
	// the contribute_source worker would no-op on.
	regCfg, err := w.systemQueries.GetRepositoryRegistryConfig(ctx, repoID)
	if err != nil {
		log.Printf("cleanup_facts: reading registry config for repo %s: %v", repositoryID, err)
		return
	}
	if !regCfg.RegistryEnabled {
		return
	}
	if client := river.ClientFromContext[pgx.Tx](ctx); client != nil {
		chainCtx, chainCancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer chainCancel()
		if _, err := client.Insert(chainCtx, ContributeSourceArgs{
			RepositoryID: repositoryID,
			SourceID:     sourceID,
		}, &river.InsertOpts{
			Queue: QueueContributeSource,
			Metadata: MarshalMetadata(JobMetadata{
				RepositoryID: repositoryID,
				SourceID:     sourceID,
			}),
		}); err != nil {
			log.Printf("cleanup_facts: enqueueing contribute_source for repo %s source %s: %v",
				repositoryID, sourceID, err)
		}
	} else {
		log.Printf("cleanup_facts: no river client on context; contribute_source not enqueued for repo %s source %s",
			repositoryID, sourceID)
	}
}
