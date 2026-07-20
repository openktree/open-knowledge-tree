package tasks

import (
	"context"
	"fmt"
	"log"
	"sort"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/openktree/open-knowledge-tree/backend/internal/config"
	"github.com/openktree/open-knowledge-tree/backend/internal/dbpool"
	"github.com/openktree/open-knowledge-tree/backend/internal/qdrantstore"
	"github.com/openktree/open-knowledge-tree/backend/internal/store"
	"github.com/riverqueue/river"
)

const QueueDeduplicateFacts = "deduplicate_facts"

// DeduplicateFactsArgs triggers a per-repository cross-source
// dedup pass. The working set is all `new` + `stable` facts in the
// repo. One pass per repo at a time is enforced by a Postgres
// advisory lock keyed on hashtext(repository_id), so concurrent
// enqueues for the same repo serialize rather than race.
//
// SourceID is a passthrough only: dedup compares facts across
// sources, so its working set is NOT narrowed by SourceID. The
// field exists so the triggering source's identity propagates
// through the chain (dedup → extract_concepts → embed_concepts →
// cleanup_facts), where each downstream task narrows its candidate
// query by source_id. A manual/re-catchup enqueue may leave
// SourceID empty to request a repo-wide downstream pass.
type DeduplicateFactsArgs struct {
	RepositoryID string `json:"repository_id"`
	SourceID     string `json:"source_id,omitempty"`
}

func (DeduplicateFactsArgs) Kind() string { return "deduplicate_facts" }

func (DeduplicateFactsArgs) InsertOpts() river.InsertOpts { return river.InsertOpts{} }

// DeduplicateFactsResult records the dedup outcome so the River
// UI shows what the pass did. MarkedToDelete is the count of
// facts flagged `to_delete`; PromotedToStable is the count of
// surviving `new` facts promoted to `stable`.
type DeduplicateFactsResult struct {
	RepositoryID     string `json:"repository_id"`
	MarkedToDelete   int    `json:"marked_to_delete"`
	PromotedToStable int    `json:"promoted_to_stable"`
}

type DeduplicateFactsWorker struct {
	river.WorkerDefaults[DeduplicateFactsArgs]

	qdrant        *qdrantstore.Store
	dedupCfg      config.DedupConfig
	registry      *dbpool.Registry
	systemQueries *store.Queries
}

func NewDeduplicateFactsWorker(
	dedupCfg config.DedupConfig,
	qdrant *qdrantstore.Store,
	registry *dbpool.Registry,
	systemQueries *store.Queries,
) *DeduplicateFactsWorker {
	return &DeduplicateFactsWorker{
		dedupCfg:      dedupCfg,
		qdrant:        qdrant,
		registry:      registry,
		systemQueries: systemQueries,
	}
}

// Work runs a dedup pass. The advisory lock is taken on the
// per-repo pool (not the task pool) so the lock is keyed to the
// repo's own database — two repos in different databases can
// dedup in parallel, two repos in the same database serialize on
// their distinct hashtext(repository_id) values. The lock is
// transaction-scoped (released on tx commit/rollback), so we run
// the whole pass inside a single transaction.
//
// Dedup rules (per repo, cross-source, sequential):
//
//  1. Load `new` + `stable` facts, dedup by id (the fact_sources JOIN
//     expands multi-source facts), and sort stable-first then new
//     (UUID-ascending within each group). The stable-first order is
//     not strictly necessary (stable facts are never the `nf`) but
//     makes the working set well-ordered; the new-UUID-ascending
//     order makes the new-vs-new tie-break deterministic.
//  2. For each `new` fact nf (skipping any that an earlier iteration
//     already marked `to_delete` — the loser of a previous new-vs-new
//     pair), search Qdrant for the nearest neighbor within the same
//     repository, excluding self, score ≥ threshold, limit=1.
//     - Hit stable m: mark nf `to_delete`; link nf's sources to m
//     via AddFactSource. (Stable always wins over new.)
//     - Hit new m: the current nf wins (it's already being
//     processed); mark m `to_delete` immediately and update
//     statusByID[m] so the loop skips m when it's reached later.
//     The survivor inherits the loser's sources via AddFactSource.
//     - Hit to_delete m: skip (not a valid keeper).
//  3. Promote remaining `new` facts to `stable` (Postgres +
//     Qdrant payload).
//  4. Enqueue cleanup_facts for the repo.
//
// The new-vs-new rule ("the hit loses, not the lex-larger UUID")
// catches same-batch duplicates that the previous rule missed. With
// the old rule, when a new fact's nearest neighbor was a stable fact
// from elsewhere in the repo, the new fact was marked `to_delete`
// against the stable fact before the loop ever compared it to its
// same-batch twin — leaving both twins `stable` after promotion
// (one marked `to_delete` against the cross-source stable, the other
// promoted). With the new rule, the first new fact to be processed
// wins against its twin and the twin is skipped, so the pair always
// collapses to one survivor.
func (w *DeduplicateFactsWorker) Work(ctx context.Context, job *river.Job[DeduplicateFactsArgs]) error {
	args := job.Args
	if args.RepositoryID == "" {
		return fmt.Errorf("deduplicate_facts: repository_id is required")
	}
	if w.qdrant == nil {
		log.Printf("deduplicate_facts: qdrant store not configured, skipping repo %s", args.RepositoryID)
		return river.RecordOutput(ctx, &DeduplicateFactsResult{RepositoryID: args.RepositoryID})
	}

	repoID := pgtype.UUID{}
	if err := repoID.Scan(args.RepositoryID); err != nil {
		return fmt.Errorf("deduplicate_facts: invalid repository_id: %w", err)
	}
	repoUUID, err := uuid.Parse(args.RepositoryID)
	if err != nil {
		return fmt.Errorf("deduplicate_facts: parsing repository_id as uuid: %w", err)
	}

	dbName, err := w.systemQueries.GetRepositoryDatabaseName(ctx, repoID)
	if err != nil {
		return fmt.Errorf("deduplicate_facts: resolving repository database: %w", err)
	}
	pool := w.registry.Get(dbName)

	// The advisory lock is transaction-scoped, so the whole dedup
	// pass runs inside one tx. A long tx is acceptable here
	// because the advisory lock already serializes per-repo
	// dedup — the tx is only held by the one worker that won the
	// lock, and the working set is bounded by the repo's fact
	// count.
	tx, err := pool.Pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("deduplicate_facts: beginning tx: %w", err)
	}
	defer tx.Rollback(context.Background())

	// pg_advisory_xact_lock(hashtext($1)). hashtext returns a
	// 32-bit int; the lock is keyed on that int. Two different
	// repository IDs may hash to the same int (collision), in
	// which case they serialize unnecessarily — a rare, harmless
	// false conflict. The important property is that the same
	// repository_id always hashes to the same int, so two
	// enqueues for the same repo always collide and serialize.
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtext($1))", args.RepositoryID); err != nil {
		return fmt.Errorf("deduplicate_facts: acquiring advisory lock: %w", err)
	}

	queries := store.New(tx)

	facts, err := queries.ListFactsForDedup(ctx, repoID)
	if err != nil {
		return fmt.Errorf("deduplicate_facts: listing facts: %w", err)
	}
	// Deduplicate the ListFactsForDedup result: a fact with
	// multiple sources appears once per source (the JOIN expands
	// it). We only need the fact row, so dedup by id keeping the
	// first occurrence (the query is ordered by created_at, not
	// by id, so we don't rely on order for dedup).
	facts = dedupFactsByID(facts)

	// Sort stable-first, then new, UUID-ascending within each
	// group. The order matters for the new-vs-new tie-break
	// (see the loop below): when two new facts are near-
	// duplicates, the one processed FIRST wins and the other
	// is marked `to_delete` immediately, so the loop skips it
	// when it's reached later. UUID-ascending within `new`
	// makes the winner deterministic. Stable facts are sorted
	// first only so the working set is well-ordered; they are
	// never the `nf` in the loop (the `nf.Status != "new"`
	// guard skips them).
	sort.SliceStable(facts, func(i, j int) bool {
		si, sj := facts[i].Status, facts[j].Status
		if si != sj {
			// "stable" < "new" < "to_delete" by lexicographic
			// order happens to match what we want; be explicit
			// anyway so a future status value doesn't silently
			// re-order things.
			rank := map[string]int{"stable": 0, "new": 1, "to_delete": 2}
			return rank[si] < rank[sj]
		}
		return pgUUIDToString(facts[i].ID) < pgUUIDToString(facts[j].ID)
	})

	markedToDelete := 0
	// Process each `new` fact. `stable` facts are the comparison
	// set; they are never marked in this pass (they're already
	// deduped). The map lets us skip `to_delete` hits and check
	// a hit's status in O(1).
	statusByID := make(map[uuid.UUID]string, len(facts))
	for _, f := range facts {
		id, err := uuid.Parse(pgUUIDToString(f.ID))
		if err != nil {
			continue
		}
		statusByID[id] = f.Status
	}

	for _, nf := range facts {
		// Skip stable facts (they're the comparison set, never
		// the `nf`) and facts an earlier iteration already
		// marked `to_delete` (a previous new-fact's nearest
		// neighbor was this fact — it lost the tie-break and
		// was marked `to_delete` + Qdrant payload flipped).
		// The snapshot `nf.Status` still reads "new" for those
		// (we never re-fetch the row), so we consult
		// `statusByID` — which the new-vs-new branch mutates —
		// to detect the flip. Without this check the loop
		// would re-process the loser, query Qdrant again, and
		// potentially mark its own winner `to_delete` (a
		// oscillation that left both new facts `to_delete`).
		nfID, err := uuid.Parse(pgUUIDToString(nf.ID))
		if err != nil {
			continue
		}
		if cur, ok := statusByID[nfID]; ok && cur != "new" {
			continue
		}
		if nf.Status != "new" {
			continue
		}

		// Fetch the fact's vector from Qdrant by searching for
		// its own nearest neighbor (excluding self). We don't
		// have a "fetch vector by id" helper, but Query with the
		// fact's own vector + self-exclusion + limit=1 returns
		// the nearest *other* fact, which is exactly what we
		// want. We need the vector first; we read it from the
		// upserted point via a Get. To keep the qdrantstore API
		// small, we instead search *by the fact's vector* — but
		// we don't have it here. The cleaner path: search by id
		// (Query with a recommend-by-id input) which Qdrant
		// supports. We use NewQueryID so Qdrant fetches the
		// vector server-side from the point id and finds the
		// nearest neighbor of *that* vector, excluding self via
		// the MustNot has_id filter. This avoids a round-trip
		// to read the vector.
		hits, err := w.qdrant.SearchSimilarByID(ctx, nfID, repoUUID, nfID, float32(w.dedupCfg.Threshold), 1)
		if err != nil {
			log.Printf("deduplicate_facts: searching neighbors for fact %s: %v", nfID, err)
			continue
		}
		if len(hits) == 0 {
			continue
		}
		hit := hits[0]
		hitStatus, ok := statusByID[hit.ID]
		if !ok {
			// The hit is not in our working set (e.g. it's
			// `to_delete` and was filtered out by
			// ListFactsForDedup). Skip — a `to_delete` fact is
			// not a valid keeper.
			continue
		}
		switch hitStatus {
		case "stable":
			// nf is a duplicate of a stable fact. Mark nf
			// `to_delete` and link nf's sources onto the stable
			// survivor.
			if err := mergeSources(ctx, queries, nf.ID, uuidToPg(hit.ID)); err != nil {
				log.Printf("deduplicate_facts: merging new %s onto stable %s: %v", nfID, hit.ID, err)
				continue
			}
			if _, err := queries.MarkFactStatus(ctx, store.MarkFactStatusParams{ID: nf.ID, Status: "to_delete"}); err != nil {
				log.Printf("deduplicate_facts: marking %s to_delete: %v", nfID, err)
				continue
			}
			statusByID[nfID] = "to_delete"
			if err := w.qdrant.UpdateFactStatusPayload(ctx, nfID, "to_delete"); err != nil {
				log.Printf("deduplicate_facts: updating qdrant payload for %s: %v", nfID, err)
			}
			markedToDelete++
		case "new":
			// Two new facts are near-duplicates. The current `nf`
			// (already being processed) wins; the `hit` (the
			// nearest neighbor Qdrant returned) loses and is
			// marked `to_delete` immediately. We update
			// `statusByID[hit.ID]` so when the loop reaches the
			// loser later, the top-of-loop `statusByID` check
			// skips it (the snapshot `nf.Status` still reads
			// "new" — only the statusByID map reflects the flip).
			//
			// The previous rule was "lexicographically-larger UUID
			// loses", which is deterministic but symmetric — it
			// doesn't matter which of {nf, hit} we're processing
			// when deciding the loser. The new rule ("the hit
			// loses") is *order-dependent* and relies on the
			// stable-first, new-UUID-ascending sort above: the
			// first new fact in UUID order to find a near-
			// duplicate twin wins, and the twin is skipped when
			// the loop reaches it. This catches same-batch
			// duplicates that the old rule missed when the
			// winner's nearest neighbor happened to be a stable
			// fact from elsewhere in the repo (so the winner
			// got marked `to_delete` against the stable fact
			// before the loop ever compared it to its twin).
			loserID, winnerID := uuidToPg(hit.ID), nf.ID
			loserStr := hit.ID.String()
			// Merge loser's sources onto the winner.
			if err := mergeSources(ctx, queries, loserID, winnerID); err != nil {
				log.Printf("deduplicate_facts: merging new %s onto new %s: %v", loserStr, pgUUIDToString(winnerID), err)
				continue
			}
			if _, err := queries.MarkFactStatus(ctx, store.MarkFactStatusParams{ID: loserID, Status: "to_delete"}); err != nil {
				log.Printf("deduplicate_facts: marking loser %s to_delete: %v", loserStr, err)
				continue
			}
			statusByID[hit.ID] = "to_delete"
			if err := w.qdrant.UpdateFactStatusPayload(ctx, hit.ID, "to_delete"); err != nil {
				log.Printf("deduplicate_facts: updating qdrant payload for loser %s: %v", loserStr, err)
			}
			markedToDelete++
		case "to_delete":
			// Already marked; not a valid keeper. Skip.
			continue
		}
	}

	// Promote surviving `new` facts to `stable`. The query
	// updates all `new` facts in the repo; the ones we marked
	// `to_delete` above are no longer `new` so they're skipped.
	if err := queries.MarkFactsStableByRepo(ctx, repoID); err != nil {
		return fmt.Errorf("deduplicate_facts: promoting survivors to stable: %w", err)
	}

	// Update Qdrant payloads for the promoted survivors. We
	// re-fetch the `stable` facts that were `new` at the start
	// of this pass. A simpler approach: walk our working set and
	// update payload for every fact still in `new` status that
	// we did NOT mark `to_delete`. But the statusByID map was
	// mutated for losers; survivors still read "new". Use that.
	promoted := 0
	for _, f := range facts {
		if f.Status != "new" {
			continue
		}
		id, err := uuid.Parse(pgUUIDToString(f.ID))
		if err != nil {
			continue
		}
		if statusByID[id] == "to_delete" {
			continue
		}
		if err := w.qdrant.UpdateFactStatusPayload(ctx, id, "stable"); err != nil {
			log.Printf("deduplicate_facts: updating qdrant payload for survivor %s: %v", id, err)
			continue
		}
		promoted++
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("deduplicate_facts: committing tx: %w", err)
	}

	log.Printf("deduplicate_facts: repo %s marked %d to_delete, promoted %d to stable", args.RepositoryID, markedToDelete, promoted)

	// Chain to extract_concepts (concept extraction runs on stable
	// facts, which this pass just promoted survivors to). The
	// cleanup_facts step is moved to the end of the embed_concepts
	// chain so the serial pipeline is: dedup → extract_concepts →
	// embed_concepts → cleanup. Fresh background ctx (see
	// source_decomposition → embed_facts for the rationale).
	if client := river.ClientFromContext[pgx.Tx](ctx); client != nil {
		chainCtx, chainCancel := context.WithTimeout(context.Background(), 15*time.Second)
		if _, err := client.Insert(chainCtx, ExtractConceptsArgs{RepositoryID: args.RepositoryID, SourceID: args.SourceID}, &river.InsertOpts{
			Queue: QueueExtractConcepts,
			Metadata: MarshalMetadata(JobMetadata{
				RepositoryID: args.RepositoryID,
				SourceID:     args.SourceID,
			}),
		}); err != nil {
			log.Printf("deduplicate_facts: enqueueing extract_concepts for repo %s: %v", args.RepositoryID, err)
		}
		chainCancel()
	} else {
		log.Printf("deduplicate_facts: no river client on context; extract_concepts not enqueued for repo %s", args.RepositoryID)
	}

	return river.RecordOutput(ctx, &DeduplicateFactsResult{
		RepositoryID:     args.RepositoryID,
		MarkedToDelete:   markedToDelete,
		PromotedToStable: promoted,
	})
}

// mergeSources re-links every source of `loser` onto `winner`
// via AddFactSource. The junction's ON CONFLICT clause makes
// this idempotent: a source already linked to the winner (e.g.
// the winner and loser shared a source before dedup) doesn't
// double-count. The chunk_index from the loser's link is
// preserved (the same fact from source A's chunk 3 and source
// B's chunk 7 records both).
//
// Sentence-level references (fact_references) are relinked in the
// same pass: DeleteDuplicateFactReferences drops the loser's
// citations that would collide with the winner's existing
// citations (same source_id + sentence_index), then
// RelinkFactReferences moves the remaining rows onto the winner.
// Non-overlapping citations from both facts are preserved — this
// is the dedup-preserves-all-references guarantee.
func mergeSources(ctx context.Context, queries *store.Queries, loser, winner pgtype.UUID) error {
	loserSources, err := queries.ListFactSourcesByFact(ctx, loser)
	if err != nil {
		return fmt.Errorf("listing loser sources: %w", err)
	}
	for _, ls := range loserSources {
		if err := queries.AddFactSource(ctx, store.AddFactSourceParams{
			FactID:     winner,
			SourceID:   ls.SourceID,
			ChunkIndex: ls.ChunkIndex,
		}); err != nil {
			return fmt.Errorf("linking source %s onto winner: %w", pgUUIDToString(ls.SourceID), err)
		}
	}
	if err := queries.DeleteDuplicateFactReferences(ctx, store.DeleteDuplicateFactReferencesParams{
		FactID:   loser,
		FactID_2: winner,
	}); err != nil {
		return fmt.Errorf("deleting duplicate fact references: %w", err)
	}
	if err := queries.RelinkFactReferences(ctx, store.RelinkFactReferencesParams{
		FactID:   loser,
		FactID_2: winner,
	}); err != nil {
		return fmt.Errorf("relinking fact references: %w", err)
	}
	if err := queries.RelinkFactConcepts(ctx, store.RelinkFactConceptsParams{
		LoserID:  loser,
		WinnerID: winner,
	}); err != nil {
		return fmt.Errorf("relinking fact concepts: %w", err)
	}
	return nil
}

// dedupFactsByID collapses a fact slice that may contain the
// same fact multiple times (due to the fact_sources JOIN
// expansion) to one row per fact, keeping the first occurrence.
func dedupFactsByID(facts []store.OktRepositoryFact) []store.OktRepositoryFact {
	seen := make(map[string]bool, len(facts))
	out := facts[:0]
	for _, f := range facts {
		k := pgUUIDToString(f.ID)
		if k == "" || seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, f)
	}
	return out
}
