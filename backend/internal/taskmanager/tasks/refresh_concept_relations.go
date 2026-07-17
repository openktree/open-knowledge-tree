package tasks

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/openktree/open-knowledge-tree/backend/internal/dbpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
)

const QueueRefreshConceptRelations = "refresh_concept_relations"

// RefreshConceptRelationsArgs refreshes the
// okt_repository.concept_relations materialized view for one
// database. The view is per-database (the same migration set runs
// against every database in cfg.Databases, so each carries its own
// okt_repository.concept_relations), and REFRESH MATERIALIZED VIEW
// CONCURRENTLY refreshes the whole view in one statement — so the
// natural dedup dimension is the DATABASE, not the repository. Two
// repositories sharing a database share one view; one refresh serves
// both, so enqueuing per-repo would only pile up redundant
// refreshes that serialize on the view's lock.
//
// DatabaseName is the river unique key (`river:"unique"`): at most one
// refresh_concept_relations job per database may be queued or running
// AT A TIME. A second enqueue for the same database while one is
// available/pending/running/scheduled/retryable is a no-op (River
// drops it), so bursts of extract_concepts batches across repos in
// the same database coalesce into a single refresh rather than N.
//
// Crucially, the unique ByState set EXCLUDES `completed` and
// `discarded`. River's default unique states include `completed`,
// which would keep a finished refresh's row blocking every subsequent
// enqueue until the job cleaner eventually purges it (often many
// hours) — the matview would refresh exactly once at boot and never
// again, silently going stale. By limiting uniqueness to the active
// states (available/pending/running/scheduled/retryable), a finished
// refresh frees the unique slot immediately, so the next periodic
// tick (or the next extract_concepts batch) can enqueue a fresh
// refresh. Concurrent bursts within an active window still coalesce.
//
// RepositoryID is carried only for the per-repo tasks list metadata
// filter (so an operator watching /repositories/{slug}/tasks sees the
// refresh that a given repo's extraction kicked off). It is NOT part
// of the unique key (the `river:"unique"` tag is only on
// DatabaseName), so two repos in the same database share one refresh.
type RefreshConceptRelationsArgs struct {
	DatabaseName string `json:"database_name" river:"unique"`
	RepositoryID string `json:"repository_id,omitempty"`
}

func (RefreshConceptRelationsArgs) Kind() string { return "refresh_concept_relations" }

func (RefreshConceptRelationsArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{
		Queue: QueueRefreshConceptRelations,
		// Modest retry budget: a refresh failure is usually a
		// transient lock conflict or a connection blip; River's
		// exponential backoff resolves it without operator
		// intervention. The next periodic tick also re-enqueues.
		MaxAttempts: 3,
		UniqueOpts: river.UniqueOpts{
			ByArgs: true,
			ByQueue: true,
			// Exclude `completed` and `discarded` from the unique
			// state set. With River's default (which includes
			// `completed`), a finished refresh row keeps blocking
			// re-enqueue until the job cleaner purges it — the
			// matview refreshes once at boot and never again. The
			// required active states (available/pending/running/
			// scheduled) plus retryable are retained so concurrent
			// bursts still coalesce into one refresh.
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

// RefreshConceptRelationsResult is recorded on the job row so the
// River UI / per-repo tasks list shows what the refresh did. Since
// REFRESH CONCURRENTLY doesn't report a row count, the result only
// carries the database name and the wall-clock duration for
// observability.
type RefreshConceptRelationsResult struct {
	DatabaseName string `json:"database_name"`
	DurationMs    int64  `json:"duration_ms"`
}

type RefreshConceptRelationsWorker struct {
	river.WorkerDefaults[RefreshConceptRelationsArgs]

	registry *dbpool.Registry
}

func NewRefreshConceptRelationsWorker(registry *dbpool.Registry) *RefreshConceptRelationsWorker {
	return &RefreshConceptRelationsWorker{registry: registry}
}

// Work resolves the per-database pool and runs
// `REFRESH MATERIALIZED VIEW CONCURRENTLY
// okt_repository.concept_relations`. CONCURRENTLY requires the view's
// unique index (installed by migration 0027) and lets reads proceed
// while the refresh rebuilds the view in the background; without it
// the refresh would block every relations-list query for the whole
// rebuild, defeating the read-side parallelism the matview is for.
//
// The refresh rebuilds the whole view for this database (every repo
// that lives in it); there is no per-repo filter because the view
// definition's GROUP BY already partitions by repository_id. A no-op
// (no concept_concepts rows) is a fast, cheap refresh.
//
// A missing database name or an unregistered database is a deployment
// error, not a retryable condition: the worker logs and returns nil
// (River doesn't retry) so a misconfigured enqueue doesn't spin
// forever. The next periodic tick or extract_concepts batch will
// re-enqueue with the correct name.
func (w *RefreshConceptRelationsWorker) Work(ctx context.Context, job *river.Job[RefreshConceptRelationsArgs]) error {
	args := job.Args
	if args.DatabaseName == "" {
		log.Printf("refresh_concept_relations: database_name is required (repo=%s), skipping", args.RepositoryID)
		return river.RecordOutput(ctx, &RefreshConceptRelationsResult{})
	}
	if w.registry == nil {
		log.Printf("refresh_concept_relations: no pool registry configured, skipping database %s", args.DatabaseName)
		return river.RecordOutput(ctx, &RefreshConceptRelationsResult{DatabaseName: args.DatabaseName})
	}
	// Guard against an unregistered name: Registry.Get panics on
	// unknown names (by construction callers only pass verified
	// names), but a misconfigured enqueue or a database that was
	// removed from cfg.Databases after a job was queued could land
	// here. Treat it as a non-retryable no-op rather than panicking
	// the worker.
	registered := false
	for _, n := range w.registry.Names() {
		if n == args.DatabaseName {
			registered = true
			break
		}
	}
	if !registered {
		log.Printf("refresh_concept_relations: database %q not registered, skipping", args.DatabaseName)
		return river.RecordOutput(ctx, &RefreshConceptRelationsResult{DatabaseName: args.DatabaseName})
	}
	pool := w.registry.Get(args.DatabaseName)

	start := time.Now()
	// Fresh background context so the refresh isn't cancelled by the
	// job's ctx timing out mid-rebuild on a huge repo. The refresh is
	// idempotent and serializes on the view lock, so a lingering run
	// is safe; River's JobTimeout still bounds the wall-clock.
	refreshCtx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	if _, err := pool.Pool.Exec(refreshCtx,
		`REFRESH MATERIALIZED VIEW CONCURRENTLY okt_repository.concept_relations`,
	); err != nil {
		return fmt.Errorf("refresh_concept_relations: refreshing view for database %s: %w", args.DatabaseName, err)
	}
	duration := time.Since(start)
	log.Printf("refresh_concept_relations: refreshed view for database %s in %s (repo=%s)",
		args.DatabaseName, duration, args.RepositoryID)
	return river.RecordOutput(ctx, &RefreshConceptRelationsResult{
		DatabaseName: args.DatabaseName,
		DurationMs:   duration.Milliseconds(),
	})
}

// Compile-time check that RefreshConceptRelationsWorker satisfies
// river.Worker.
var _ river.Worker[RefreshConceptRelationsArgs] = (*RefreshConceptRelationsWorker)(nil)

// RefreshAllConceptRelationsArgs is the periodic-job entry point. The
// periodic tick (every cfg.Task.RefreshConceptRelationsInterval) can
// only enqueue ONE JobArgs per fire, but the refresh needs to cover
// every registered database. This args bridges that gap: its worker
// fans out one RefreshConceptRelationsArgs per database via the River
// client on the context, reusing the per-database worker (and its
// unique-by-database dedup) above. Each fan-out enqueue is best-effort
// (logged on failure), so a transient River hiccup doesn't fail the
// periodic job — the next tick covers it.
type RefreshAllConceptRelationsArgs struct{}

func (RefreshAllConceptRelationsArgs) Kind() string { return "refresh_all_concept_relations" }

func (RefreshAllConceptRelationsArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{Queue: QueueRefreshConceptRelations}
}

type RefreshAllConceptRelationsWorker struct {
	river.WorkerDefaults[RefreshAllConceptRelationsArgs]

	registry *dbpool.Registry
}

func NewRefreshAllConceptRelationsWorker(registry *dbpool.Registry) *RefreshAllConceptRelationsWorker {
	return &RefreshAllConceptRelationsWorker{registry: registry}
}

// Work iterates every registered database and enqueues a
// RefreshConceptRelationsArgs for each. The per-database worker (and
// its unique-by-database dedup) handles the actual refresh; this
// worker only does the fan-out. With no registry or no databases it
// records a no-op result so a deployment that hasn't opened any pools
// still boots cleanly.
func (w *RefreshAllConceptRelationsWorker) Work(ctx context.Context, job *river.Job[RefreshAllConceptRelationsArgs]) error {
	if w.registry == nil {
		return river.RecordOutput(ctx, &RefreshConceptRelationsResult{})
	}
	client := river.ClientFromContext[pgx.Tx](ctx)
	if client == nil {
		log.Printf("refresh_all_concept_relations: no river client on context; skipping periodic fan-out")
		return river.RecordOutput(ctx, &RefreshConceptRelationsResult{})
	}
	names := w.registry.Names()
	enqueued, skipped := 0, 0
	for _, name := range names {
		// Fresh short context per enqueue so one slow Insert doesn't
		// starve the rest.
		insertCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		res, err := client.Insert(insertCtx, RefreshConceptRelationsArgs{DatabaseName: name}, nil)
		cancel()
		if err != nil {
			// A real insert failure (unique-conflict no longer returns
			// an error since River v0.6 — it returns
			// UniqueSkippedAsDuplicate on the result; see below). Log
			// and move on; don't fail the periodic job.
			log.Printf("refresh_all_concept_relations: enqueueing refresh for database %s: %v", name, err)
			continue
		}
		if res != nil && res.UniqueSkippedAsDuplicate {
			// A refresh for this database is already queued/running/
			// scheduled/retryable — exactly the coalescing the unique
			// opts are for. Count separately so the log line
			// distinguishes "actually enqueued" from "deduped", which
			// is what makes a stuck refresh diagnosable (a permanently
			// high `skipped` with `enqueued=0` across ticks would
			// signal a dedup bug like the completed-state one).
			skipped++
			continue
		}
		enqueued++
	}
	log.Printf("refresh_all_concept_relations: enqueued %d refresh job(s), deduped %d (already active) across %d database(s)",
		enqueued, skipped, len(names))
	return river.RecordOutput(ctx, &RefreshConceptRelationsResult{})
}

var _ river.Worker[RefreshAllConceptRelationsArgs] = (*RefreshAllConceptRelationsWorker)(nil)