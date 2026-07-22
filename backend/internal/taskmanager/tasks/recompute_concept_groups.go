package tasks

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/openktree/open-knowledge-tree/backend/internal/dbpool"
	"github.com/openktree/open-knowledge-tree/backend/internal/store"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
)

const QueueRecomputeConceptGroups = "recompute_concept_groups"

// RecomputeConceptGroupsArgs recomputes the
// okt_repository.concept_groups summary table for one repository.
// The summary is maintained incrementally by the ingest workers +
// migrate_context + registry imports (each calls
// concepts.RecomputeTouchedGroups at the end of its tx), so this job
// exists purely as the operator-driven repair path: an admin
// "Recompute concept groups" button (POST
// /admin/repos/{repoID}/concepts/recompute) enqueues it when a write
// site was missed or a backfill needs redoing. It is NOT enqueued
// periodically — the incremental maintenance keeps the summary
// always-live, and a periodic tick would only duplicate that work.
//
// RecomputeAllConceptGroupsForRepo runs a full-repo DELETE + INSERT
// in one tx, bounded by the repo's concept count (200k now, millions
// in production — a few seconds at 200k, proportionally longer at
// scale). The job's wall-clock is observed by River's JobTimeout.
//
// DatabaseName is the river unique key (`river:"unique"`): at most one
// recompute_concept_groups job per database may be queued or running
// at a time. Per-database (not per-repo) dedup is used because
// RecomputeAllConceptGroupsForRepo issues a DELETE+INSERT on the
// per-database concept_groups table; two repos in the same database
// would otherwise race on the same table, and per-repo dedup would
// pile up redundant recomputes that serialize on the table's lock.
// Two repos in the same database share one recompute; the per-repo
// tasks list metadata filter still surfaces the originating repo.
//
// The unique ByState set EXCLUDES `completed` and `discarded`
// (mirroring refresh_concept_relations): River's default unique
// states include `completed`, which would keep a finished recompute's
// row blocking every subsequent enqueue until the job cleaner purges
// it — the button would work exactly once after boot and then refuse.
// Limiting uniqueness to active states frees the slot immediately on
// completion.
//
// RepositoryID is the target repo to recompute (string form, scanned
// into pgtype.UUID by the worker). It's metadata for the tasks list
// when the originating repo differs from the database's other repos.
type RecomputeConceptGroupsArgs struct {
	DatabaseName string `json:"database_name" river:"unique"`
	RepositoryID string `json:"repository_id,omitempty"`
}

func (RecomputeConceptGroupsArgs) Kind() string { return "recompute_concept_groups" }

func (RecomputeConceptGroupsArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{
		Queue: QueueRecomputeConceptGroups,
		// Modest retry budget: a recompute failure is usually a
		// transient lock conflict or a connection blip. The next
		// button click re-enqueues, so 3 attempts is enough.
		MaxAttempts: 3,
		UniqueOpts: river.UniqueOpts{
			ByArgs: true,
			ByQueue: true,
			// Exclude `completed` and `discarded` from the unique
			// state set. With River's default (which includes
			// `completed`), a finished recompute row keeps blocking
			// re-enqueue until the job cleaner purges it — the button
			// would work once and then refuse. The active states
			// (available/pending/running/scheduled) plus retryable
			// are retained so concurrent button clicks coalesce.
			ByState: []rivertype.JobState{
				rivertype.JobStateAvailable,
				rivertype.JobStatePending,
				rivertype.JobStateRunning,
				rivertype.JobStateScheduled,
				rivertype.JobStateRetryable,
			},
		},
	}
}

// RecomputeConceptGroupsResult is recorded on the job row so the
// Tasks UI shows what the recompute did. The duration is for
// observability; concept_groups carries no per-recompute count.
type RecomputeConceptGroupsResult struct {
	DatabaseName string `json:"database_name"`
	DurationMs   int64  `json:"duration_ms"`
}

type RecomputeConceptGroupsWorker struct {
	river.WorkerDefaults[RecomputeConceptGroupsArgs]
	registry *dbpool.Registry
}

func NewRecomputeConceptGroupsWorker(registry *dbpool.Registry) *RecomputeConceptGroupsWorker {
	return &RecomputeConceptGroupsWorker{registry: registry}
}

// Work resolves the database name to a pool, resolves the repo UUID,
// and runs RecomputeAllConceptGroupsForRepo (a full-repo DELETE +
// INSERT in one tx). A missing database name or an unregistered
// database is a deployment error, not a retryable condition: the
// worker logs and returns nil (River doesn't retry) so a misconfigured
// enqueue doesn't spin forever; the next button click re-enqueues.
func (w *RecomputeConceptGroupsWorker) Work(ctx context.Context, job *river.Job[RecomputeConceptGroupsArgs]) error {
	args := job.Args
	if args.DatabaseName == "" {
		log.Printf("recompute_concept_groups: database_name is required (repo=%s), skipping", args.RepositoryID)
		return river.RecordOutput(ctx, &RecomputeConceptGroupsResult{})
	}
	if w.registry == nil {
		log.Printf("recompute_concept_groups: no pool registry configured, skipping database %s", args.DatabaseName)
		return river.RecordOutput(ctx, &RecomputeConceptGroupsResult{DatabaseName: args.DatabaseName})
	}
	// Guard against an unregistered name (mirrors
	// refresh_concept_relations): Registry.Get panics on unknown names.
	registered := false
	for _, n := range w.registry.Names() {
		if n == args.DatabaseName {
			registered = true
			break
		}
	}
	if !registered {
		log.Printf("recompute_concept_groups: database %q not registered, skipping", args.DatabaseName)
		return river.RecordOutput(ctx, &RecomputeConceptGroupsResult{DatabaseName: args.DatabaseName})
	}
	pool := w.registry.Get(args.DatabaseName)

	if args.RepositoryID == "" {
		log.Printf("recompute_concept_groups: repository_id is required, skipping database %s", args.DatabaseName)
		return river.RecordOutput(ctx, &RecomputeConceptGroupsResult{DatabaseName: args.DatabaseName})
	}
	var repoID pgtype.UUID
	if err := repoID.Scan(args.RepositoryID); err != nil {
		return fmt.Errorf("recompute_concept_groups: invalid repository_id %q: %w", args.RepositoryID, err)
	}

	start := time.Now()
	// Fresh background context so the recompute isn't cancelled by the
	// job's ctx timing out mid-rebuild on a huge repo. The recompute is
	// idempotent and the tx is atomic per repo, so a lingering run is
	// safe; River's JobTimeout still bounds the wall-clock.
	recomputeCtx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	if err := store.New(pool.Pool).RecomputeAllConceptGroupsForRepo(recomputeCtx, repoID); err != nil {
		return fmt.Errorf("recompute_concept_groups: recomputing for database %s repo %s: %w", args.DatabaseName, args.RepositoryID, err)
	}
	duration := time.Since(start)
	log.Printf("recompute_concept_groups: recomputed for database %s repo %s in %s",
		args.DatabaseName, args.RepositoryID, duration)
	return river.RecordOutput(ctx, &RecomputeConceptGroupsResult{
		DatabaseName: args.DatabaseName,
		DurationMs:   duration.Milliseconds(),
	})
}

var _ river.Worker[RecomputeConceptGroupsArgs] = (*RecomputeConceptGroupsWorker)(nil)