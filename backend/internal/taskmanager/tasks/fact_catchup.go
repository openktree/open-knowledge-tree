package tasks

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/openktree/open-knowledge-tree/backend/internal/config"
	"github.com/openktree/open-knowledge-tree/backend/internal/dbpool"
	"github.com/openktree/open-knowledge-tree/backend/internal/qdrantstore"
	"github.com/openktree/open-knowledge-tree/backend/internal/store"
	"github.com/riverqueue/river"
)

const QueueFactCatchup = "fact_catchup"

// FactCatchupArgs is the (empty) argument shape for the daily
// fact_catchup periodic job. River periodic jobs return a
// JobArgs; the periodic-job constructor in taskmanager.New
// returns FactCatchupArgs{}.
type FactCatchupArgs struct{}

func (FactCatchupArgs) Kind() string { return "fact_catchup" }

func (FactCatchupArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{Queue: QueueFactCatchup}
}

type FactCatchupResult struct {
	Databases  int `json:"databases"`
	TotalDeleted int `json:"total_deleted"`
}

type FactCatchupWorker struct {
	river.WorkerDefaults[FactCatchupArgs]

	qdrant       *qdrantstore.Store
	registry     *dbpool.Registry
	systemQueries *store.Queries
	dedupCfg     config.DedupConfig
}

func NewFactCatchupWorker(
	dedupCfg config.DedupConfig,
	qdrant *qdrantstore.Store,
	registry *dbpool.Registry,
	systemQueries *store.Queries,
) *FactCatchupWorker {
	return &FactCatchupWorker{
		dedupCfg:      dedupCfg,
		qdrant:        qdrant,
		registry:      registry,
		systemQueries: systemQueries,
	}
}

// Work removes stuck `to_delete` + `new` facts older than
// `dedup.catchup_max_age` (default 168h) from every registered
// database, in bounded batches (LIMIT 10000) per iteration, and
// deletes the matching Qdrant points per batch. Bounded deletes
// avoid multi-minute WAL spikes at millions of facts; the loop
// runs until a batch returns 0 rows. `new` facts older than the
// cutoff are reaped because a fact that has sat `new` for a week
// is stuck (the embed_facts / deduplicate_facts chain should
// have moved it to `stable` or `to_delete` within minutes).
func (w *FactCatchupWorker) Work(ctx context.Context, job *river.Job[FactCatchupArgs]) error {
	if w.registry == nil {
		return river.RecordOutput(ctx, &FactCatchupResult{})
	}
	cutoff := pgtype.Timestamptz{Time: time.Now().Add(-w.dedupCfg.CatchupMaxAgeDuration()), Valid: true}
	statusSet := []string{"to_delete", "new"}
	const batchLimit = int32(10000)

	totalDeleted := 0
	dbCount := 0
	for _, name := range w.registry.Names() {
		dbCount++
		pool := w.registry.Get(name)
		queries := store.New(pool.Pool)

		for {
			ids, err := queries.DeleteStaleFactsInDB(ctx, store.DeleteStaleFactsInDBParams{
				Column1:   statusSet,
				CreatedAt: cutoff,
				Limit:     batchLimit,
			})
			if err != nil {
				log.Printf("fact_catchup: deleting stale facts in db %s: %v", name, err)
				break
			}
			if len(ids) == 0 {
				break
			}
			// Delete the matching Qdrant points for this batch.
			// Best-effort: a Qdrant failure is logged and the
			// loop continues — the Postgres rows are already
			// gone, so the next pass won't re-find them; the
			// orphaned Qdrant points will be invisible to
			// searches (no Postgres row to join) and a future
			// Qdrant-side reaper can sweep them by absence.
			if w.qdrant != nil {
				qdrantIDs := make([]uuid.UUID, 0, len(ids))
				for _, id := range ids {
					s := pgUUIDToString(id)
					if s == "" {
						continue
					}
					if u, err := uuid.Parse(s); err == nil {
						qdrantIDs = append(qdrantIDs, u)
					}
				}
				if len(qdrantIDs) > 0 {
					if err := w.qdrant.DeleteFactVectors(ctx, qdrantIDs); err != nil {
						log.Printf("fact_catchup: deleting qdrant points in db %s batch: %v", name, err)
					}
				}
			}
			totalDeleted += len(ids)
			if len(ids) < int(batchLimit) {
				break
			}
		}
	}

	log.Printf("fact_catchup: swept %d databases, deleted %d stale facts", dbCount, totalDeleted)
	return river.RecordOutput(ctx, &FactCatchupResult{
		Databases:    dbCount,
		TotalDeleted: totalDeleted,
	})
}

// Compile-time check that FactCatchupWorker satisfies
// river.Worker. Keeping the assertion next to the type catches
// signature drift at compile time rather than at wiring.
var _ river.Worker[FactCatchupArgs] = (*FactCatchupWorker)(nil)

// fmt is referenced in error wrapping above; keep the import so
// gofmt doesn't reorder it out when the file is edited.
var _ = fmt.Sprintf