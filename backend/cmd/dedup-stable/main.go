// Command dedup-stable is a one-time maintenance script that
// re-runs the deduplicate_facts worker's nearest-neighbor pass over
// the existing `stable` facts of a repository. The production
// `deduplicate_facts` worker only processes facts whose status is
// `new`; once both endpoints of a near-duplicate pair are `stable`,
// they are frozen and never re-compared. This script unfreezes them:
// for every `stable` fact it queries Qdrant for the nearest neighbor
// in the same repository at score >= threshold, marks the loser
// `to_delete`, relinks the loser's sources/citations/concepts onto
// the survivor (via the same mergeSources routine the worker uses),
// and flips the Qdrant payload. It does NOT promote any facts —
// `stable` facts stay `stable`. Run cleanup_facts afterwards (or let
// the next periodic catchup pass reap the `to_delete` rows).
//
// The script reuses the production config loader (so it picks up
// `dedup.threshold`, qdrant host/collection, and the database
// registry from configs/config.default.yaml + config.local.yaml +
// .env overrides). It does NOT boot the API server, River workers,
// or run migrations.
//
// Usage:
//
//	go run ./cmd/dedup-stable [--repo=<uuid>|-a] [--dry-run] [--limit=N] [--concurrency=N]
//
// Flags:
//
//	--repo=<uuid>     repository to dedup; omit (or pass -a) to sweep every repo
//	-a, --all         sweep every repository (default when --repo is empty)
//	--dry-run         query Qdrant + report would-be-losers, but do NOT write to Postgres/Qdrant
//	--limit=N         cap the number of stable facts scanned per repo (0 = all; default 0)
//	--concurrency=N   parallel Qdrant nearest-neighbor queries (default 8)
//	--threshold=F     override dedup.threshold from config (0 = use config; default 0)
//	--confirm         without this flag the script runs in --dry-run regardless of other flags
//
// The --confirm gate is intentional: a one-shot pass that marks
// stable facts `to_delete` is destructive (cleanup_facts will
// physically delete the rows + Qdrant points). Run without --confirm
// first to preview, then with --confirm to apply.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"sort"
	"sync"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/openktree/open-knowledge-tree/backend/internal/config"
	"github.com/openktree/open-knowledge-tree/backend/internal/dbpool"
	"github.com/openktree/open-knowledge-tree/backend/internal/qdrantstore"
	"github.com/openktree/open-knowledge-tree/backend/internal/store"
)

func main() {
	repoFlag := flag.String("repo", "", "repository UUID to dedup; empty or -a for all repos")
	allFlag := flag.Bool("all", false, "sweep every repository (default when --repo is empty)")
	dryRunFlag := flag.Bool("dry-run", false, "query Qdrant + report would-be-losers, but do NOT write to Postgres/Qdrant")
	limitFlag := flag.Int("limit", 0, "cap the number of stable facts scanned per repo (0 = all)")
	concurrencyFlag := flag.Int("concurrency", 8, "parallel Qdrant nearest-neighbor queries")
	thresholdFlag := flag.Float64("threshold", 0, "override dedup.threshold from config (0 = use config)")
	confirmFlag := flag.Bool("confirm", false, "without this flag the script always runs in dry-run mode")
	cleanupFlag := flag.Bool("cleanup", false, "physically delete facts already marked `to_delete` (Postgres rows + Qdrant points); skips the dedup pass entirely")
	configPathFlag := flag.String("config", "", "path to a config file or directory (same search rules as the API server)")
	progressEveryFlag := flag.Int("progress-every", 200, "log a progress line every N facts scanned")
	flag.Parse()

	dryRun := *dryRunFlag || !*confirmFlag
	if dryRun && !*cleanupFlag {
		log.Printf("dedup-stable: DRY RUN (no writes). Pass --confirm to apply.")
	} else if !*cleanupFlag {
		log.Printf("dedup-stable: APPLYING (writes enabled). Re-run with --dry-run to preview.")
	}

	cfg, err := config.Load(*configPathFlag)
	if err != nil {
		log.Fatalf("loading config: %v", err)
	}

	if cfg.Providers.Qdrant.Host == "" {
		log.Fatalf("dedup-stable: providers.qdrant.host is empty in config; the script needs Qdrant to query nearest neighbors or delete points")
	}
	qStore, err := qdrantstore.NewClient(cfg.Providers.Qdrant)
	if err != nil {
		log.Fatalf("dedup-stable: building qdrant client: %v", err)
	}
	defer qStore.Close()
	hcCtx, hcCancel := context.WithTimeout(context.Background(), 5*time.Second)
	if _, err := qStore.HealthCheck(hcCtx); err != nil {
		hcCancel()
		log.Fatalf("dedup-stable: qdrant health check failed: %v", err)
	}
	hcCancel()
	log.Printf("dedup-stable: qdrant connected (collection %q)", qStore.Collection())

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	registry, err := dbpool.New(ctx, cfg)
	if err != nil {
		log.Fatalf("dedup-stable: opening database pools: %v", err)
	}
	defer registry.Close()

	defaultPool := registry.Default()
	systemQueries := store.New(defaultPool.Pool)

	repos, err := systemQueries.ListAllRepositories(ctx)
	if err != nil {
		log.Fatalf("dedup-stable: listing repositories: %v", err)
	}
	if *repoFlag != "" && !*allFlag {
		target, err := uuid.Parse(*repoFlag)
		if err != nil {
			log.Fatalf("dedup-stable: parsing --repo %q: %v", *repoFlag, err)
		}
		repos = filterReposByID(repos, target)
		if len(repos) == 0 {
			log.Fatalf("dedup-stable: no repository with id %s", *repoFlag)
		}
	}

	if *cleanupFlag {
		log.Printf("dedup-stable: CLEANUP mode — physically deleting `to_delete` facts (skips the dedup pass)")
		if !*confirmFlag {
			log.Fatalf("dedup-stable: --cleanup requires --confirm (it physically deletes rows; preview is not supported)")
		}
		runCleanup(ctx, systemQueries, registry, qStore, repos)
		return
	}

	log.Printf("dedup-stable: sweeping %d repo(s)", len(repos))

	threshold := cfg.Providers.Dedup.Threshold
	if *thresholdFlag > 0 {
		threshold = *thresholdFlag
	}
	if threshold <= 0 {
		threshold = 0.94
	}
	log.Printf("dedup-stable: threshold = %.4f (config: %.4f)", threshold, cfg.Providers.Dedup.Threshold)

	totalMarked := 0
	totalScanned := 0
	totalSkipped := 0
	start := time.Now()
	for i, repo := range repos {
		repoIDStr := pgUUIDToString(repo.ID)
		repoName := repo.Name
		log.Printf("dedup-stable: [%d/%d] repo %s (%q)", i+1, len(repos), repoIDStr, repoName)

		dbName, err := systemQueries.GetRepositoryDatabaseName(ctx, repo.ID)
		if err != nil {
			log.Printf("dedup-stable: repo %s: resolving database name: %v; skipping", repoIDStr, err)
			continue
		}
		pool := registry.Get(dbName)

		scanned, marked, skipped, err := sweepRepo(ctx, pool.Pool, qStore, repo.ID, threshold, *limitFlag, *concurrencyFlag, *progressEveryFlag, dryRun)
		if err != nil {
			log.Printf("dedup-stable: repo %s: ERROR: %v", repoIDStr, err)
			continue
		}
		log.Printf("dedup-stable: repo %s: scanned=%d marked=%d skipped=%d", repoIDStr, scanned, marked, skipped)
		totalScanned += scanned
		totalMarked += marked
		totalSkipped += skipped
	}

	elapsed := time.Since(start)
	log.Printf("dedup-stable: DONE. repos=%d scanned=%d marked=%d skipped=%d elapsed=%s mode=%s",
		len(repos), totalScanned, totalMarked, totalSkipped, elapsed.Round(time.Millisecond), modeString(dryRun))
}

func modeString(dryRun bool) string {
	if dryRun {
		return "DRY-RUN"
	}
	return "APPLIED"
}

// runCleanup physically deletes facts already marked `to_delete`
// from Postgres + Qdrant for each repo. This mirrors the
// cleanup_facts worker's behavior (ListFactsToDelete → DeleteFactVectors
// → DeleteFactByID) but runs inline instead of through River, so the
// operator can reap the to_delete rows left by a --confirm dedup pass
// (or by any previous deduplicate_facts run) without waiting for the
// periodic fact_catchup job.
//
// The pass for each repo runs in its own short transaction (no
// advisory lock — cleanup is idempotent: deleting a row that's
// already gone is a no-op). Qdrant deletes are best-effort: a
// failure is logged but doesn't block the Postgres delete, matching
// the worker's "Postgres is the source of truth" contract.
func runCleanup(
	ctx context.Context,
	systemQueries *store.Queries,
	registry *dbpool.Registry,
	qStore *qdrantstore.Store,
	repos []store.Repository,
) {
	totalDeleted := 0
	start := time.Now()
	for i, repo := range repos {
		repoIDStr := pgUUIDToString(repo.ID)
		log.Printf("dedup-stable: cleanup [%d/%d] repo %s (%q)", i+1, len(repos), repoIDStr, repo.Name)

		dbName, err := systemQueries.GetRepositoryDatabaseName(ctx, repo.ID)
		if err != nil {
			log.Printf("dedup-stable: cleanup repo %s: resolving database name: %v; skipping", repoIDStr, err)
			continue
		}
		pool := registry.Get(dbName)
		queries := store.New(pool.Pool)

		ids, err := queries.ListFactsToDelete(ctx, repo.ID)
		if err != nil {
			log.Printf("dedup-stable: cleanup repo %s: listing to_delete facts: %v; skipping", repoIDStr, err)
			continue
		}
		if len(ids) == 0 {
			log.Printf("dedup-stable: cleanup repo %s: no `to_delete` facts", repoIDStr)
			continue
		}
		log.Printf("dedup-stable: cleanup repo %s: %d `to_delete` facts to reap", repoIDStr, len(ids))

		// Delete from Qdrant first (best-effort).
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
		if len(qdrantIDs) > 0 {
			if err := qStore.DeleteFactVectors(ctx, qdrantIDs); err != nil {
				log.Printf("dedup-stable: cleanup repo %s: deleting qdrant points: %v (continuing with Postgres delete)", repoIDStr, err)
			}
		}

		// Delete Postgres rows one by one (mirrors the worker;
		// DeleteFactByID has no batch variant).
		deleted := 0
		for _, id := range ids {
			if err := queries.DeleteFactByID(ctx, id); err != nil {
				log.Printf("dedup-stable: cleanup repo %s: deleting fact %s: %v", repoIDStr, pgUUIDToString(id), err)
				continue
			}
			deleted++
		}
		log.Printf("dedup-stable: cleanup repo %s: deleted %d facts", repoIDStr, deleted)
		totalDeleted += deleted
	}

	elapsed := time.Since(start)
	log.Printf("dedup-stable: CLEANUP DONE. repos=%d deleted=%d elapsed=%s",
		len(repos), totalDeleted, elapsed.Round(time.Millisecond))
}

func filterReposByID(repos []store.Repository, target uuid.UUID) []store.Repository {
	for _, r := range repos {
		id, err := uuid.Parse(pgUUIDToString(r.ID))
		if err == nil && id == target {
			return []store.Repository{r}
		}
	}
	return nil
}

// sweepRepo runs the stable-vs-stable dedup pass for one repository.
// It lists every `stable` fact in the repo, sorts UUID-ascending
// (so the winner of any pair is deterministic), and for each stable
// fact queries Qdrant for the nearest neighbor (excluding self) at
// score >= threshold. When the nearest neighbor is itself `stable`
// (i.e. in the working set), the lex-larger UUID loses: its sources
// are relinked onto the survivor via mergeSources, its Postgres row
// is flipped to `to_delete`, and its Qdrant payload is updated. The
// loser is then skipped when the loop reaches it.
//
// The pass runs inside one transaction per repo with a per-repo
// advisory lock, mirroring the production worker's serialization
// (two concurrent passes for the same repo would race on the same
// nearest-neighbor queries). The lock is transaction-scoped so it's
// released on commit/rollback.
//
// In dry-run mode the tx is rolled back at the end and no Qdrant
// payload updates are issued; only the SELECT + Qdrant search calls
// run, so the operator can preview the would-be losers without any
// writes.
func sweepRepo(
	ctx context.Context,
	pool *pgxpool.Pool,
	qStore *qdrantstore.Store,
	repoID pgtype.UUID,
	threshold float64,
	limit int,
	concurrency int,
	progressEvery int,
	dryRun bool,
) (scanned, marked, skipped int, err error) {
	repoUUID, err := uuid.Parse(pgUUIDToString(repoID))
	if err != nil {
		return 0, 0, 0, fmt.Errorf("parsing repo id: %w", err)
	}

	tx, err := pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return 0, 0, 0, fmt.Errorf("beginning tx: %w", err)
	}
	defer func() {
		if dryRun {
			// Always rollback in dry-run, even on success.
			_ = tx.Rollback(context.Background())
			return
		}
		// Apply mode: commit on success, rollback on error.
		if err != nil {
			_ = tx.Rollback(context.Background())
			return
		}
		if cerr := tx.Commit(ctx); cerr != nil {
			err = fmt.Errorf("committing tx: %w", cerr)
		}
	}()

	// Per-repo advisory lock, same key as the production worker
	// (hashtext(repository_id)). This serializes against any
	// concurrent deduplicate_facts pass on the same repo.
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtext($1))", pgUUIDToString(repoID)); err != nil {
		return 0, 0, 0, fmt.Errorf("acquiring advisory lock: %w", err)
	}

	queries := store.New(tx)

	// List all `new` + `stable` facts (ListFactsForDedup), then keep
	// only `stable`. We use the same query as the worker so the
	// join through fact_sources + sources filters out any orphaned
	// facts and respects the repo boundary on the sources side.
	allFacts, err := queries.ListFactsForDedup(ctx, repoID)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("listing facts: %w", err)
	}
	facts := make([]store.OktRepositoryFact, 0, len(allFacts))
	seen := make(map[string]bool, len(allFacts))
	for _, f := range allFacts {
		key := pgUUIDToString(f.ID)
		if key == "" || seen[key] {
			continue
		}
		seen[key] = true
		if f.Status != "stable" {
			continue
		}
		facts = append(facts, f)
	}
	if limit > 0 && limit < len(facts) {
		facts = facts[:limit]
	}

	// Sort UUID-ascending so the winner of any pair is deterministic:
	// the lex-smaller UUID is processed first and wins; the lex-larger
	// one is its hit and gets marked `to_delete`.
	sort.Slice(facts, func(i, j int) bool {
		return pgUUIDToString(facts[i].ID) < pgUUIDToString(facts[j].ID)
	})

	// Build statusByID so we can detect hits that are NOT in the
	// working set (e.g. `new` or `to_delete` facts). We only merge
	// when both endpoints are `stable` and in the working set; a
	// stable fact's nearest neighbor being `new` is the production
	// worker's job (it processes `new` facts), not ours.
	statusByID := make(map[uuid.UUID]string, len(facts))
	for _, f := range facts {
		id, err := uuid.Parse(pgUUIDToString(f.ID))
		if err != nil {
			continue
		}
		statusByID[id] = f.Status
	}

	// Concurrency: process facts in UUID-ascending order (the
	// winner-marks-loser invariant depends on order — the
	// lex-smaller UUID is processed first and wins; the lex-larger
	// one is its hit and gets marked `to_delete`). The Qdrant
	// nearest-neighbor query is the slow part (~350 q/s serial at
	// 285K facts = ~14min), so we prefetch queries in parallel
	// batches of `concurrency` facts and process each batch in
	// order. This keeps the order invariant while cutting the
	// wall time by ~concurrency.
	//
	// The merge + MarkFactStatus + UpdateFactStatusPayload calls
	// run on the main goroutine (they touch the tx and must
	// serialize); only the Qdrant search is parallelized.
	batchSize := concurrency
	if batchSize < 1 {
		batchSize = 1
	}
	for start := 0; start < len(facts); start += batchSize {
		if ctx.Err() != nil {
			return scanned, marked, skipped, ctx.Err()
		}
		end := start + batchSize
		if end > len(facts) {
			end = len(facts)
		}
		batch := facts[start:end]

		// Prefetch: fire all Qdrant queries in the batch in
		// parallel, collect into a map keyed by factID.
		type queryResult struct {
			factID uuid.UUID
			hits   []qdrantstore.Hit
			err    error
		}
		resultCh := make(chan queryResult, len(batch))
		var qerr error
		go func() {
			var wg sync.WaitGroup
			for _, f := range batch {
				wg.Add(1)
				go func(f store.OktRepositoryFact) {
					defer wg.Done()
					fid, err := uuid.Parse(pgUUIDToString(f.ID))
					if err != nil {
						resultCh <- queryResult{err: fmt.Errorf("parsing fact id: %w", err)}
						return
					}
					hits, qerr := qStore.SearchSimilarByID(ctx, fid, repoUUID, fid, float32(threshold), 1)
					resultCh <- queryResult{factID: fid, hits: hits, err: qerr}
				}(f)
			}
			wg.Wait()
			close(resultCh)
		}()
		results := make(map[uuid.UUID][]qdrantstore.Hit, len(batch))
		errs := make(map[uuid.UUID]error, len(batch))
		for r := range resultCh {
			if r.err != nil && r.factID == uuid.Nil {
				qerr = r.err
				continue
			}
			results[r.factID] = r.hits
			if r.err != nil {
				errs[r.factID] = r.err
			}
		}
		if qerr != nil {
			return scanned, marked, skipped, fmt.Errorf("qdrant query error: %w", qerr)
		}

		// Process the batch in UUID-ascending order (the batch
		// is already in that order because `facts` is sorted).
		for i, f := range batch {
			idx := start + i
			if idx%progressEvery == 0 || idx == len(facts)-1 {
				log.Printf("dedup-stable: repo %s progress [%d/%d] marked=%d", pgUUIDToString(repoID), idx+1, len(facts), marked)
			}
			scanned++

			fid, err := uuid.Parse(pgUUIDToString(f.ID))
			if err != nil {
				skipped++
				continue
			}

			// Skip if an earlier iteration already marked
			// this fact `to_delete` (it was the hit/loser of
			// a previous pair).
			if cur, ok := statusByID[fid]; ok && cur == "to_delete" {
				skipped++
				continue
			}

			hits, ok := results[fid]
			if !ok {
				if qerr, hasErr := errs[fid]; hasErr {
					log.Printf("dedup-stable: repo %s fact %s: qdrant search error: %v", pgUUIDToString(repoID), fid, qerr)
				}
				continue
			}
			if len(hits) == 0 {
				continue
			}
			hit := hits[0]
			hitStatus, inSet := statusByID[hit.ID]
			if !inSet {
				// Hit is not in our stable working set
				// (it's `new` or `to_delete`). Leave it
				// for the production worker.
				continue
			}
			if hitStatus != "stable" {
				continue
			}
			// Both endpoints are stable + in the working
			// set. The current `nf` wins; the hit loses.
			loserID := uuidToPg(hit.ID)
			winnerID := f.ID
			if dryRun {
				log.Printf("dedup-stable: [dry-run] would mark %s to_delete (winner %s, score %.4f)", hit.ID, fid, hit.Score)
				marked++
				statusByID[hit.ID] = "to_delete"
				continue
			}
			if err := mergeSources(ctx, queries, loserID, winnerID); err != nil {
				log.Printf("dedup-stable: repo %s merging %s onto %s: %v", pgUUIDToString(repoID), hit.ID, fid, err)
				continue
			}
			if _, err := queries.MarkFactStatus(ctx, store.MarkFactStatusParams{ID: loserID, Status: "to_delete"}); err != nil {
				log.Printf("dedup-stable: repo %s marking %s to_delete: %v", pgUUIDToString(repoID), hit.ID, err)
				continue
			}
			statusByID[hit.ID] = "to_delete"
			if err := qStore.UpdateFactStatusPayload(ctx, hit.ID, "to_delete"); err != nil {
				log.Printf("dedup-stable: repo %s updating qdrant payload for %s: %v", pgUUIDToString(repoID), hit.ID, err)
			}
			marked++
		}
	}

	return scanned, marked, skipped, nil
}

// mergeSources re-links every source of `loser` onto `winner` via
// AddFactSource, then relinks the loser's fact_references and
// fact_concepts onto the winner. Mirrors the production worker's
// mergeSources exactly (see internal/taskmanager/tasks/deduplicate_facts.go).
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

// pgUUIDToString formats a pgtype.UUID as a canonical lowercase UUID
// string. Returns "" when the UUID is invalid. Mirrors the helper
// in internal/taskmanager/tasks/embed_facts.go.
func pgUUIDToString(id pgtype.UUID) string {
	if !id.Valid {
		return ""
	}
	b := id.Bytes
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// uuidToPg converts a google/uuid.UUID into a pgtype.UUID. Mirrors
// the helper in internal/taskmanager/tasks/embed_facts.go.
func uuidToPg(id uuid.UUID) pgtype.UUID {
	var out pgtype.UUID
	if err := out.Scan(id.String()); err != nil {
		// Should never happen for a valid uuid.UUID.
		return pgtype.UUID{Valid: false}
	}
	return out
}

// silence unused warnings when certain stdlib imports are conditionally
// compiled out by build tags in future variants of this file.
var (
	_ = errors.New
	_ = json.Marshal
	_ = os.Stdin
)