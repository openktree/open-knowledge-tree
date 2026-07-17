//go:build e2e

package e2e_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/openktree/open-knowledge-tree/backend/e2e/testutil"
	"github.com/openktree/open-knowledge-tree/backend/internal/config"
	"github.com/openktree/open-knowledge-tree/backend/internal/providers/ai"
	"github.com/openktree/open-knowledge-tree/backend/internal/providers/decomposition"
	"github.com/openktree/open-knowledge-tree/backend/internal/providers/summarization"
	"github.com/openktree/open-knowledge-tree/backend/internal/store"
	"github.com/openktree/open-knowledge-tree/backend/internal/taskmanager/tasks"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivertest"
)

// stubSummarizer is a test double for
// summarization.SummarizationProvider that returns a fixed markdown
// body built from the concept name + fact count. It records every
// call so tests can assert how many slices were produced.
type stubSummarizer struct {
	mu    sync.Mutex
	calls int
	body  func(concept string, factIDs []string) string
}

func (s *stubSummarizer) Summarize(_ context.Context, _ store.DBTX, req summarization.SummarizationRequest) (string, error) {
	s.mu.Lock()
	s.calls++
	s.mu.Unlock()
	ids := make([]string, 0, len(req.Facts))
	for _, f := range req.Facts {
		ids = append(ids, f.ID)
	}
	if s.body != nil {
		return s.body(req.ConceptCanonicalName, ids), nil
	}
	// Default body: cite the first fact id so we can assert the
	// [text](<fact:fact_id>) syntax survives the round-trip.
	body := "# Summary of " + req.ConceptCanonicalName + "\n\nCovers " + itoa(len(req.Facts)) + " facts."
	if len(ids) > 0 {
		body += " Key fact: [see](<fact:" + ids[0] + ">)."
	}
	return body, nil
}

func (s *stubSummarizer) Describe() summarization.ProviderDescription {
	return summarization.ProviderDescription{Name: "stub-summarizer", Configured: true}
}

func (s *stubSummarizer) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

// itoa is a tiny strconv.Itoa wrapper kept local so the test file
// doesn't import strconv for a single call.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// summarizeTestEnv bundles the per-test wiring (worker + river test
// worker + stub) so each test below can spin up a fresh summarizer
// without repeating the boilerplate.
type summarizeTestEnv struct {
	env       *testutil.TestEnv
	worker    *tasks.SummarizeConceptsWorker
	stub      *stubSummarizer
	testWorker *rivertest.Worker[tasks.SummarizeConceptsArgs, pgx.Tx]
}

func newSummarizeTestEnv(t *testing.T, cfg config.SummarizationConfig) *summarizeTestEnv {
	t.Helper()
	env := testutil.NewTestEnv(t)
	ensureRiverSchema(t, env.DB)
	stub := &stubSummarizer{}
	registry := testutil.NewForTestPool(env.DB)
	systemQueries := store.New(env.DB)
	worker := tasks.NewSummarizeConceptsWorker(stub, cfg, registry, systemQueries, false, nil)
	driver := riverpgxv5.New(env.DB)
	workers := river.NewWorkers()
	river.AddWorker(workers, worker)
	tw := rivertest.NewWorker(t, driver, &river.Config{
		Queues:  map[string]river.QueueConfig{tasks.QueueSummarizeConcepts: {MaxWorkers: 1}},
		Workers: workers,
	}, worker)
	return &summarizeTestEnv{env: env, worker: worker, stub: stub, testWorker: tw}
}

// seedConceptWithFacts creates a concept + N stable facts linked to it
// (and to a source so the repo scoping works). Returns the concept_id
// and the fact_ids in insertion order. The fact_concepts.first_seen_at
// is set by AddFactConcept's DEFAULT now(); tests don't need to control
// it beyond insertion order.
func seedConceptWithFacts(t *testing.T, env *testutil.TestEnv, repoID pgtype.UUID, conceptName, contextLabel string, nFacts int) (pgtype.UUID, []pgtype.UUID) {
	t.Helper()
	ctx := context.Background()
	queries := store.New(env.DB)

	srcID := pgtype.UUID{}
	if err := srcID.Scan(uuid.NewString()); err != nil {
		t.Fatalf("scan src: %v", err)
	}
	if _, err := queries.CreateSource(ctx, store.CreateSourceParams{
		ID: srcID, RepositoryID: repoID, Url: "https://example.com/" + uuid.NewString(), Kind: "homepage", Status: "fetched",
	}); err != nil {
		t.Fatalf("create source: %v", err)
	}
	concept, err := queries.CreateConcept(ctx, store.CreateConceptParams{
		RepositoryID: repoID, CanonicalName: conceptName, Context: contextLabel,
	})
	if err != nil {
		t.Fatalf("create concept: %v", err)
	}
	factIDs := make([]pgtype.UUID, 0, nFacts)
	for i := 0; i < nFacts; i++ {
		fidStr := insertFactWithSource(t, env, repoID, srcID, "Fact "+itoa(i)+" about "+conceptName, int32(i))
		fid := pgtype.UUID{}
		if err := fid.Scan(fidStr); err != nil {
			t.Fatalf("scan fact: %v", err)
		}
		if _, err := queries.AddFactConcept(ctx, store.AddFactConceptParams{FactID: fid, ConceptID: concept.ID}); err != nil && !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("link fact: %v", err)
		}
		factIDs = append(factIDs, fid)
	}
	return concept.ID, factIDs
}

// runSummarizeJob wraps the rivertest.Work call for
// SummarizeConceptsArgs, returning the recorded result.
func runSummarizeJob(t *testing.T, e *summarizeTestEnv, args tasks.SummarizeConceptsArgs) tasks.SummarizeConceptsResult {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	tx, err := e.env.DB.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	job, err := e.testWorker.Work(ctx, t, tx, args, &river.InsertOpts{Queue: tasks.QueueSummarizeConcepts})
	if err != nil {
		tx.Rollback(context.Background())
		t.Fatalf("summarize_concepts.Work: %v", err)
	}
	if job.EventKind != river.EventKindJobCompleted {
		tx.Rollback(context.Background())
		t.Fatalf("summarize_concepts: expected completed, got %s", job.EventKind)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
	var result tasks.SummarizeConceptsResult
	if raw := job.Job.Output(); len(raw) > 0 {
		if err := json.Unmarshal(raw, &result); err != nil {
			t.Fatalf("decode output: %v", err)
		}
	}
	return result
}

// TestSummarizeConcepts_NotConfiguredIsNoop verifies the worker
// degrades gracefully when the summarizer is nil: it records a no-op
// result and returns nil (River doesn't retry). This is the
// deployment shape where summarization is not configured but the
// API still boots.
func TestSummarizeConcepts_NotConfiguredIsNoop(t *testing.T) {
	env := testutil.NewTestEnv(t)
	defer env.Server.Close()
	ensureRiverSchema(t, env.DB)

	_, _, repoID := createRepositoryWithDB(t, bootstrapSysAdmin(t, env, "nosum@example.com"), "NoSum", "no-sum", "desc", "")
	registry := testutil.NewForTestPool(env.DB)
	systemQueries := store.New(env.DB)
	worker := tasks.NewSummarizeConceptsWorker(nil, config.SummarizationConfig{Enabled: false}, registry, systemQueries, false, nil)

	driver := riverpgxv5.New(env.DB)
	workers := river.NewWorkers()
	river.AddWorker(workers, worker)
	tw := rivertest.NewWorker(t, driver, &river.Config{
		Queues:  map[string]river.QueueConfig{tasks.QueueSummarizeConcepts: {MaxWorkers: 1}},
		Workers: workers,
	}, worker)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	tx, err := env.DB.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer tx.Rollback(context.Background())
	job, err := tw.Work(ctx, t, tx, tasks.SummarizeConceptsArgs{RepositoryID: repoID}, &river.InsertOpts{Queue: tasks.QueueSummarizeConcepts})
	if err != nil {
		t.Fatalf("work: %v", err)
	}
	if job.EventKind != river.EventKindJobCompleted {
		t.Fatalf("expected completed, got %s", job.EventKind)
	}
}

// TestSummarizeConcepts_SubBatchOpenAccumulator verifies the
// incremental scheme when fewer than BatchSize facts exist: one
// open (is_complete=FALSE) summary is created, covering all facts.
// Re-running with a few more facts (< BatchSize total) updates the
// same open row (fact_count grows, content regenerated) rather than
// creating a new one.
func TestSummarizeConcepts_SubBatchOpenAccumulator(t *testing.T) {
	cfg := config.SummarizationConfig{Enabled: true, BatchSize: 20, Model: "stub-model"}
	ste := newSummarizeTestEnv(t, cfg)
	admin := bootstrapSysAdmin(t, ste.env, "sub@example.com")
	_, _, repoID := createRepositoryWithDB(t, admin, "SubBatch", "sub-batch", "desc", "")
	pgRepo := pgRepoID(t, repoID)

	// 5 facts -> one open summary covering 5.
	conceptID, _ := seedConceptWithFacts(t, ste.env, pgRepo, "Insulin", "Biomolecule", 5)
	res := runSummarizeJob(t, ste, tasks.SummarizeConceptsArgs{RepositoryID: repoID, ConceptIDs: []string{pgUUIDString(conceptID)}})
	if res.PairsProcessed != 1 || res.SummariesCreated != 1 || res.SummariesUpdated != 0 {
		t.Fatalf("first run: processed=%d created=%d updated=%d, want 1/1/0", res.PairsProcessed, res.SummariesCreated, res.SummariesUpdated)
	}
	open := mustGetOpenSummary(t, ste.env, conceptID)
	if !open.IsComplete && open.FactCount != 5 {
		t.Errorf("open fact_count = %d, want 5 (sub-batch open accumulator)", open.FactCount)
	}

	// 3 more facts -> the SAME open row is updated (fact_count=8),
	// no new row. The summarizer is called once (regenerate the open).
	seedExtraFacts(t, ste.env, pgRepo, conceptID, "Insulin", 3, 5)
	res2 := runSummarizeJob(t, ste, tasks.SummarizeConceptsArgs{RepositoryID: repoID, ConceptIDs: []string{pgUUIDString(conceptID)}})
	if res2.SummariesCreated != 0 || res2.SummariesUpdated != 1 {
		t.Fatalf("second run: created=%d updated=%d, want 0/1 (open regenerated)", res2.SummariesCreated, res2.SummariesUpdated)
	}
	open2 := mustGetOpenSummary(t, ste.env, conceptID)
	if open2.ID != open.ID {
		t.Errorf("open row id changed: first=%s second=%s (want same row regenerated)", pgUUIDString(open.ID), pgUUIDString(open2.ID))
	}
	if open2.FactCount != 8 {
		t.Errorf("open fact_count after 3 more = %d, want 8", open2.FactCount)
	}
	if ste.stub.callCount() != 2 {
		t.Errorf("summarizer calls = %d, want 2 (one per run)", ste.stub.callCount())
	}
}

// TestSummarizeConcepts_CrossBatchFreezeNoRemainder verifies the
// batch-only slicing rule: when an open accumulator (18 facts) crosses
// BatchSize with 5 new facts (23 total >= 20), the open row freezes
// (is_complete=TRUE, fact_count=20) and NO new open remainder is
// created for the 3 leftover facts — they stay uncovered until a full
// batch accumulates. A subsequent run with no new facts is a no-op
// (PairsSkippedNoDelta=1).
func TestSummarizeConcepts_CrossBatchFreezeNoRemainder(t *testing.T) {
	cfg := config.SummarizationConfig{Enabled: true, BatchSize: 20, Model: "stub-model"}
	ste := newSummarizeTestEnv(t, cfg)
	admin := bootstrapSysAdmin(t, ste.env, "freeze@example.com")
	_, _, repoID := createRepositoryWithDB(t, admin, "Freeze", "freeze", "desc", "")
	pgRepo := pgRepoID(t, repoID)

	// 18 facts -> open accumulator (is_complete=FALSE, 18 facts).
	conceptID, _ := seedConceptWithFacts(t, ste.env, pgRepo, "Penicillin", "Biomolecule", 18)
	runSummarizeJob(t, ste, tasks.SummarizeConceptsArgs{RepositoryID: repoID, ConceptIDs: []string{pgUUIDString(conceptID)}})
	openBefore := mustGetOpenSummary(t, ste.env, conceptID)
	if openBefore.IsComplete || openBefore.FactCount != 18 {
		t.Fatalf("setup: open before freeze: is_complete=%v fact_count=%d, want false/18", openBefore.IsComplete, openBefore.FactCount)
	}
	firstSummaryID := openBefore.ID

	// 5 more facts -> 23 total. The open freezes at 20; the 3
	// leftover facts are NOT summarized (no remainder slice).
	seedExtraFacts(t, ste.env, pgRepo, conceptID, "Penicillin", 5, 18)
	res := runSummarizeJob(t, ste, tasks.SummarizeConceptsArgs{RepositoryID: repoID, ConceptIDs: []string{pgUUIDString(conceptID)}})
	if res.SummariesCreated != 0 || res.SummariesUpdated != 1 {
		t.Fatalf("freeze run: created=%d updated=%d, want 0/1 (freeze open, no remainder)", res.SummariesCreated, res.SummariesUpdated)
	}

	// The first summary must now be frozen at 20 facts.
	var frozen store.OktRepositoryConceptSummary
	if err := ste.env.DB.QueryRow(context.Background(),
		`SELECT id, is_complete, fact_count, sequence_num FROM okt_repository.concept_summaries WHERE id = $1`, firstSummaryID,
	).Scan(&frozen.ID, &frozen.IsComplete, &frozen.FactCount, &frozen.SequenceNum); err != nil {
		t.Fatalf("query frozen: %v", err)
	}
	if !frozen.IsComplete || frozen.FactCount != 20 {
		t.Errorf("frozen slice: is_complete=%v fact_count=%d, want true/20", frozen.IsComplete, frozen.FactCount)
	}

	// No open summary should exist (the 3 leftover facts are
	// uncovered, waiting for a full batch).
	var openCount int
	if err := ste.env.DB.QueryRow(context.Background(),
		`SELECT count(*) FROM okt_repository.concept_summaries WHERE concept_id = $1 AND is_complete = FALSE`, conceptID,
	).Scan(&openCount); err != nil {
		t.Fatalf("count open: %v", err)
	}
	if openCount != 0 {
		t.Errorf("open summaries after freeze = %d, want 0 (no remainder in batch-only mode)", openCount)
	}

	// Re-run with no new facts: PairsSkippedNoDelta=1, no new rows.
	res2 := runSummarizeJob(t, ste, tasks.SummarizeConceptsArgs{RepositoryID: repoID, ConceptIDs: []string{pgUUIDString(conceptID)}})
	if res2.PairsSkippedNoDelta != 1 || res2.SummariesCreated != 0 || res2.SummariesUpdated != 0 {
		t.Errorf("no-delta run: skipped=%d created=%d updated=%d, want 1/0/0", res2.PairsSkippedNoDelta, res2.SummariesCreated, res2.SummariesUpdated)
	}
}

// TestSummarizeConcepts_LargeBurstProducesMultipleFrozenSlices
// verifies that a single run with 50 new facts (no existing open)
// produces floor(50/20)=2 frozen slices only — no open remainder for
// the 10 leftover facts, because the first slice froze in this same
// pass (batch-only mode kicks in once hadComplete). 2 LLM calls.
func TestSummarizeConcepts_LargeBurstProducesMultipleFrozenSlices(t *testing.T) {
	cfg := config.SummarizationConfig{Enabled: true, BatchSize: 20, Model: "stub-model"}
	ste := newSummarizeTestEnv(t, cfg)
	admin := bootstrapSysAdmin(t, ste.env, "burst@example.com")
	_, _, repoID := createRepositoryWithDB(t, admin, "Burst", "burst", "desc", "")
	pgRepo := pgRepoID(t, repoID)

	conceptID, _ := seedConceptWithFacts(t, ste.env, pgRepo, "Aspirin", "Biomolecule", 50)
	res := runSummarizeJob(t, ste, tasks.SummarizeConceptsArgs{RepositoryID: repoID, ConceptIDs: []string{pgUUIDString(conceptID)}})
	if res.SummariesCreated != 2 || res.SummariesUpdated != 0 {
		t.Fatalf("burst: created=%d updated=%d, want 2/0 (2 frozen, no open remainder)", res.SummariesCreated, res.SummariesUpdated)
	}
	if ste.stub.callCount() != 2 {
		t.Errorf("summarizer calls = %d, want 2 (2 frozen, no remainder)", ste.stub.callCount())
	}
	// 2 frozen (seq 1, 2; is_complete=TRUE, 20 facts each). 10 facts
	// stay uncovered — no open remainder is created once a complete
	// slice exists.
	rows, err := store.New(ste.env.DB).ListSummariesByConcept(context.Background(), conceptID)
	if err != nil {
		t.Fatalf("list summaries: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("summary rows = %d, want 2", len(rows))
	}
	wantSeqs := []int32{1, 2}
	wantComplete := []bool{true, true}
	wantCounts := []int32{20, 20}
	for i, r := range rows {
		if r.SequenceNum != wantSeqs[i] {
			t.Errorf("row %d sequence_num = %d, want %d", i, r.SequenceNum, wantSeqs[i])
		}
		if r.IsComplete != wantComplete[i] {
			t.Errorf("row %d is_complete = %v, want %v", i, r.IsComplete, wantComplete[i])
		}
		if r.FactCount != wantCounts[i] {
			t.Errorf("row %d fact_count = %d, want %d", i, r.FactCount, wantCounts[i])
		}
	}
}

// TestSummarizeConcepts_BatchOnlyAfterFirstFreeze verifies the
// cost-saving rule end-to-end: after the first slice freezes, small
// sub-batch deltas are no-ops (no LLM call), and a full BatchSize
// delta produces exactly one new complete slice (one LLM call).
func TestSummarizeConcepts_BatchOnlyAfterFirstFreeze(t *testing.T) {
	cfg := config.SummarizationConfig{Enabled: true, BatchSize: 20, Model: "stub-model"}
	ste := newSummarizeTestEnv(t, cfg)
	admin := bootstrapSysAdmin(t, ste.env, "batchonly@example.com")
	_, _, repoID := createRepositoryWithDB(t, admin, "BatchOnly", "batch-only", "desc", "")
	pgRepo := pgRepoID(t, repoID)

	// Seed 20 facts -> first slice freezes immediately (complete).
	conceptID, _ := seedConceptWithFacts(t, ste.env, pgRepo, "Ibuprofen", "Biomolecule", 20)
	runSummarizeJob(t, ste, tasks.SummarizeConceptsArgs{RepositoryID: repoID, ConceptIDs: []string{pgUUIDString(conceptID)}})
	callsAfterFreeze := ste.stub.callCount()
	if callsAfterFreeze != 1 {
		t.Fatalf("after freeze: summarizer calls = %d, want 1", callsAfterFreeze)
	}

	// 5 more facts (< BatchSize) -> no-op, no LLM call.
	seedExtraFacts(t, ste.env, pgRepo, conceptID, "Ibuprofen", 5, 20)
	res := runSummarizeJob(t, ste, tasks.SummarizeConceptsArgs{RepositoryID: repoID, ConceptIDs: []string{pgUUIDString(conceptID)}})
	if res.PairsSkippedNoDelta != 1 || res.SummariesCreated != 0 || res.SummariesUpdated != 0 {
		t.Fatalf("sub-batch run: skipped=%d created=%d updated=%d, want 1/0/0", res.PairsSkippedNoDelta, res.SummariesCreated, res.SummariesUpdated)
	}
	if ste.stub.callCount() != callsAfterFreeze {
		t.Errorf("sub-batch run: summarizer calls = %d, want %d (no LLM call for sub-batch)", ste.stub.callCount(), callsAfterFreeze)
	}

	// 15 more facts -> total uncovered = 20 (the 5 from before + 15
	// new) -> one complete slice, one LLM call.
	seedExtraFacts(t, ste.env, pgRepo, conceptID, "Ibuprofen", 15, 25)
	res2 := runSummarizeJob(t, ste, tasks.SummarizeConceptsArgs{RepositoryID: repoID, ConceptIDs: []string{pgUUIDString(conceptID)}})
	if res2.SummariesCreated != 1 || res2.SummariesUpdated != 0 {
		t.Fatalf("full-batch run: created=%d updated=%d, want 1/0 (one complete slice)", res2.SummariesCreated, res2.SummariesUpdated)
	}
	if ste.stub.callCount() != callsAfterFreeze+1 {
		t.Errorf("full-batch run: summarizer calls = %d, want %d (one LLM call)", ste.stub.callCount(), callsAfterFreeze+1)
	}

	// Verify: 2 complete slices, no open remainder.
	rows, err := store.New(ste.env.DB).ListSummariesByConcept(context.Background(), conceptID)
	if err != nil {
		t.Fatalf("list summaries: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("summary rows = %d, want 2", len(rows))
	}
	for i, r := range rows {
		if !r.IsComplete {
			t.Errorf("row %d is_complete = false, want true (batch-only mode)", i)
		}
		if r.FactCount != 20 {
			t.Errorf("row %d fact_count = %d, want 20", i, r.FactCount)
		}
	}
}

// TestSummarizeConcepts_ConcurrentLockSkips verifies the per-concept
// summarizing_at lock: when two SummarizeConcepts jobs touch the
// same concept concurrently, only one processes it; the other skips
// with PairsSkippedLocked=1. (Concurrency here is simulated by
// pre-acquiring the lock via a direct UPDATE before running the
// worker, since rivertest runs jobs serially.)
func TestSummarizeConcepts_ConcurrentLockSkips(t *testing.T) {
	cfg := config.SummarizationConfig{Enabled: true, BatchSize: 20, Model: "stub-model", LockStaleness: 2 * time.Hour}
	ste := newSummarizeTestEnv(t, cfg)
	admin := bootstrapSysAdmin(t, ste.env, "lock@example.com")
	_, _, repoID := createRepositoryWithDB(t, admin, "Lock", "lock", "desc", "")
	pgRepo := pgRepoID(t, repoID)

	conceptID, _ := seedConceptWithFacts(t, ste.env, pgRepo, "Morphine", "Biomolecule", 5)

	// Pre-acquire the lock so the worker's ClaimConceptForSummary
	// returns no rows.
	if _, err := ste.env.DB.Exec(context.Background(),
		`UPDATE okt_repository.concepts SET summarizing_at = now() WHERE id = $1`, conceptID,
	); err != nil {
		t.Fatalf("pre-acquire lock: %v", err)
	}

	res := runSummarizeJob(t, ste, tasks.SummarizeConceptsArgs{RepositoryID: repoID, ConceptIDs: []string{pgUUIDString(conceptID)}})
	if res.PairsSkippedLocked != 1 || res.PairsProcessed != 0 {
		t.Fatalf("locked run: skipped=%d processed=%d, want 1/0", res.PairsSkippedLocked, res.PairsProcessed)
	}
	// No summary row should have been written.
	var n int
	if err := ste.env.DB.QueryRow(context.Background(),
		`SELECT count(*) FROM okt_repository.concept_summaries WHERE concept_id = $1`, conceptID,
	).Scan(&n); err != nil {
		t.Fatalf("count summaries: %v", err)
	}
	if n != 0 {
		t.Errorf("summary rows after locked run = %d, want 0 (lock holder owns the concept)", n)
	}
}

// TestSummarizeConcepts_StaleLockIsReclaimed verifies that a stale
// summarizing_at lock (older than LockStaleness) is reclaimable by
// the next worker run so a crashed worker doesn't wedge the concept
// forever.
func TestSummarizeConcepts_StaleLockIsReclaimed(t *testing.T) {
	cfg := config.SummarizationConfig{Enabled: true, BatchSize: 20, Model: "stub-model", LockStaleness: 1 * time.Nanosecond}
	ste := newSummarizeTestEnv(t, cfg)
	admin := bootstrapSysAdmin(t, ste.env, "stale@example.com")
	_, _, repoID := createRepositoryWithDB(t, admin, "Stale", "stale", "desc", "")
	pgRepo := pgRepoID(t, repoID)

	conceptID, _ := seedConceptWithFacts(t, ste.env, pgRepo, "Caffeine", "Biomolecule", 5)

	// Pre-acquire the lock with an OLD timestamp so it's stale.
	if _, err := ste.env.DB.Exec(context.Background(),
		`UPDATE okt_repository.concepts SET summarizing_at = now() - interval '1 hour' WHERE id = $1`, conceptID,
	); err != nil {
		t.Fatalf("pre-acquire stale lock: %v", err)
	}

	res := runSummarizeJob(t, ste, tasks.SummarizeConceptsArgs{RepositoryID: repoID, ConceptIDs: []string{pgUUIDString(conceptID)}})
	if res.PairsProcessed != 1 || res.SummariesCreated != 1 {
		t.Fatalf("stale-lock run: processed=%d created=%d, want 1/1 (stale lock reclaimed)", res.PairsProcessed, res.SummariesCreated)
	}
}

// TestSummaries_HTTPReadEndpoint verifies the read surface:
//   - GET /concepts/{conceptID}/summaries returns 200 with the
//     summary slices in the page envelope.
//   - A cross-repo conceptID is a 404.
//   - Missing auth is a 401.
func TestSummaries_HTTPReadEndpoint(t *testing.T) {
	cfg := config.SummarizationConfig{Enabled: true, BatchSize: 20, Model: "stub-model"}
	ste := newSummarizeTestEnv(t, cfg)
	admin := bootstrapSysAdmin(t, ste.env, "httpsum@example.com")
	const slug = "http-sum"
	_, _, repoID := createRepositoryWithDB(t, admin, "HTTPSum", slug, "desc", "")
	pgRepo := pgRepoID(t, repoID)

	conceptID, _ := seedConceptWithFacts(t, ste.env, pgRepo, "Atropine", "Biomolecule", 5)
	runSummarizeJob(t, ste, tasks.SummarizeConceptsArgs{RepositoryID: repoID, ConceptIDs: []string{pgUUIDString(conceptID)}})

	// 200 with one open summary.
	cidStr := pgUUIDString(conceptID)
	resp, raw := admin.do("GET", "/api/v1/repositories/"+slug+"/concepts/"+cidStr+"/summaries", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET summaries: %d %s", resp.StatusCode, raw)
	}
	var list pageEnvelope
	if err := json.Unmarshal(raw, &list); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if list.Total != 1 {
		t.Errorf("summaries total = %d, want 1", list.Total)
	}
	if len(list.Data) != 1 {
		t.Fatalf("summaries data len = %d, want 1", len(list.Data))
	}
	var s store.OktRepositoryConceptSummary
	if err := json.Unmarshal(list.Data[0], &s); err != nil {
		t.Fatalf("decode summary row: %v", err)
	}
	if s.IsComplete {
		t.Errorf("summary is_complete = true, want false (5 facts < batch 20)")
	}
	if s.FactCount != 5 {
		t.Errorf("summary fact_count = %d, want 5", s.FactCount)
	}
	if !strings.Contains(s.Content, "[see](") {
		t.Errorf("summary content missing [text](<fact:fact_id>) citation: %q", s.Content)
	}

	// Cross-repo conceptID: create a second repo, point the first
	// repo's conceptID at it. The concept belongs to repo A, so
	// querying from repo B's pool must 404. We test this by hitting
	// the endpoint from a different repo slug with the same conceptID.
	admin2 := bootstrapSysAdmin(t, ste.env, "httpsum2@example.com")
	const slug2 = "http-sum-2"
	_, _, _ = createRepositoryWithDB(t, admin2, "HTTPSum2", slug2, "desc", "")
	resp2, _ := admin2.do("GET", "/api/v1/repositories/"+slug2+"/concepts/"+cidStr+"/summaries", nil)
	if resp2.StatusCode != http.StatusNotFound {
		t.Errorf("cross-repo concept summaries: status %d, want 404", resp2.StatusCode)
	}

	// Missing auth: 401.
	anon := newAuthClient(ste.env.BaseURL)
	resp3, _ := anon.do("GET", "/api/v1/repositories/"+slug+"/concepts/"+cidStr+"/summaries", nil)
	if resp3.StatusCode != http.StatusUnauthorized {
		t.Errorf("no-auth summaries: status %d, want 401", resp3.StatusCode)
	}
}

// TestSummarizeConcepts_ExtractConceptsFanOuts verifies that
// extract_concepts enqueues SummarizeConcepts jobs in parallel with
// embed_concepts when summarization is enabled. This is the
// integration guard: the wiring (SetSummarizationEnabled) and the
// fan-out enqueue block in extract_concepts.Work must both fire.
func TestSummarizeConcepts_ExtractConceptsFanOuts(t *testing.T) {
	env := testutil.NewTestEnv(t)
	defer env.Server.Close()
	ensureRiverSchema(t, env.DB)
	admin := bootstrapSysAdmin(t, env, "fanout@example.com")
	_, _, repoID := createRepositoryWithDB(t, admin, "FanOut", "fan-out", "desc", "")
	queries := store.New(env.DB)

	srcID := pgtype.UUID{}
	if err := srcID.Scan(uuid.NewString()); err != nil {
		t.Fatalf("scan src: %v", err)
	}
	if _, err := queries.CreateSource(context.Background(), store.CreateSourceParams{
		ID: srcID, RepositoryID: pgRepoID(t, repoID), Url: "https://example.com/fanout", Kind: "homepage", Status: "fetched",
	}); err != nil {
		t.Fatalf("create source: %v", err)
	}
	factID := insertFactWithSource(t, env, pgRepoID(t, repoID), srcID, "A fact about DNA.", 0)
	fid := pgtype.UUID{}
	if err := fid.Scan(factID); err != nil {
		t.Fatalf("scan fact: %v", err)
	}
	if _, err := env.DB.Exec(context.Background(),
		`UPDATE okt_repository.facts SET status = 'stable' WHERE id = $1`, fid,
	); err != nil {
		t.Fatalf("promote fact: %v", err)
	}

	// Pre-create a concept with an alias matching the extractor's
	// output so the match path fires (fact_concepts, not
	// fact_candidates). The summarize fan-out queries
	// fact_concepts, so the concept must be linked via that table.
	pgRepo := pgRepoID(t, repoID)
	concept, err := store.New(env.DB).CreateConcept(context.Background(), store.CreateConceptParams{
		RepositoryID:  pgRepo,
		CanonicalName: "DNA",
		Context:       "Biomolecule",
	})
	if err != nil {
		t.Fatalf("create concept: %v", err)
	}
	if _, err := store.New(env.DB).AddConceptAlias(context.Background(), store.AddConceptAliasParams{
		ConceptID: concept.ID,
		AliasText: "DNA",
	}); err != nil {
		t.Fatalf("add alias: %v", err)
	}

	extractor := &stubConceptExtractor{concepts: []decomposition.ExtractedConcept{
		{Concept: "DNA", Context: "Biomolecule", SeedAliases: []string{"deoxyribonucleic acid"}},
	}}
	conceptCfg := config.DecompositionConceptConfig{Enabled: true}
	registry := testutil.NewForTestPool(env.DB)
	systemQueries := store.New(env.DB)
	worker := tasks.NewExtractConceptsWorker(extractor, conceptCfg, registry, systemQueries, nil)
	worker.SetSummarizationEnabled(true)

	// A no-op summarize worker is registered alongside the extract
	// worker so the chained Insert (extract_concepts →
	// summarize_concepts) is accepted by rivertest. Without a
	// registered worker, River rejects the Insert with "job kind is
	// not registered" and the fan-out assertion can't see the row.
	summarizeWorker := tasks.NewSummarizeConceptsWorker(
		&stubSummarizer{}, config.SummarizationConfig{Enabled: true, BatchSize: 20, Model: "stub"},
		registry, systemQueries, false, nil,
	)

	driver := riverpgxv5.New(env.DB)
	workers := river.NewWorkers()
	river.AddWorker(workers, worker)
	river.AddWorker(workers, summarizeWorker)
	tw := rivertest.NewWorker(t, driver, &river.Config{
		Queues: map[string]river.QueueConfig{
			tasks.QueueExtractConcepts:   {MaxWorkers: 1},
			tasks.QueueSummarizeConcepts: {MaxWorkers: 1},
		},
		Workers: workers,
	}, worker)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	tx, err := env.DB.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	job, err := tw.Work(ctx, t, tx, tasks.ExtractConceptsArgs{RepositoryID: repoID, SourceID: pgUUIDString(srcID)}, &river.InsertOpts{Queue: tasks.QueueExtractConcepts})
	if err != nil {
		tx.Rollback(context.Background())
		t.Fatalf("extract_concepts.Work: %v", err)
	}
	if job.EventKind != river.EventKindJobCompleted {
		tx.Rollback(context.Background())
		t.Fatalf("expected completed, got %s", job.EventKind)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}

	// The extract_concepts job must have enqueued at least one
	// summarize_concepts job (visible via the River jobs table).
	var n int
	if err := env.DB.QueryRow(ctx,
		`SELECT count(*) FROM river_job WHERE args->>'kind' IS NULL AND metadata->>'repo_id' = $1 AND queue = 'summarize_concepts'`,
		repoID,
	).Scan(&n); err != nil {
		// river_job.args is a JSONB column keyed by the args struct,
		// so 'kind' isn't a top-level key. The metadata filter is the
		// reliable signal; fall back to just the queue + metadata.
		if err2 := env.DB.QueryRow(ctx,
			`SELECT count(*) FROM river_job WHERE metadata->>'repo_id' = $1 AND queue = 'summarize_concepts'`,
			repoID,
		).Scan(&n); err2 != nil {
			t.Fatalf("query summarize_concepts jobs: %v / %v", err, err2)
		}
	}
	if n == 0 {
		t.Errorf("extract_concepts did not enqueue any summarize_concepts jobs for repo %s", repoID)
	}
}

// seedExtraFacts adds n more stable facts linked to the given
// concept (and to a fresh source). Used by the incremental tests to
// grow a concept's fact set after the initial seed.
func seedExtraFacts(t *testing.T, env *testutil.TestEnv, repoID, conceptID pgtype.UUID, conceptName string, n, startIdx int) {
	t.Helper()
	ctx := context.Background()
	queries := store.New(env.DB)
	srcID := pgtype.UUID{}
	if err := srcID.Scan(uuid.NewString()); err != nil {
		t.Fatalf("scan src: %v", err)
	}
	if _, err := queries.CreateSource(ctx, store.CreateSourceParams{
		ID: srcID, RepositoryID: repoID, Url: "https://example.com/extra-" + uuid.NewString(), Kind: "homepage", Status: "fetched",
	}); err != nil {
		t.Fatalf("create source: %v", err)
	}
	for i := 0; i < n; i++ {
		fidStr := insertFactWithSource(t, env, repoID, srcID, "Extra fact "+itoa(startIdx+i)+" about "+conceptName, int32(startIdx+i))
		fid := pgtype.UUID{}
		if err := fid.Scan(fidStr); err != nil {
			t.Fatalf("scan fact: %v", err)
		}
		if _, err := queries.AddFactConcept(ctx, store.AddFactConceptParams{FactID: fid, ConceptID: conceptID}); err != nil && !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("link fact: %v", err)
		}
	}
}

// mustGetOpenSummary fetches the single open summary for a concept,
// failing the test if there isn't exactly one.
func mustGetOpenSummary(t *testing.T, env *testutil.TestEnv, conceptID pgtype.UUID) store.OktRepositoryConceptSummary {
	t.Helper()
	s, err := store.New(env.DB).GetOpenSummary(context.Background(), conceptID)
	if err != nil {
		t.Fatalf("GetOpenSummary for concept %s: %v", pgUUIDString(conceptID), err)
	}
	return s
}

// guard against unused imports when the file is edited.
var _ = ai.Attribution{}
var _ = errors.New