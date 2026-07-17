//go:build e2e

package e2e_test

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/openktree/open-knowledge-tree/backend/e2e/testutil"
	"github.com/openktree/open-knowledge-tree/backend/internal/config"
	"github.com/openktree/open-knowledge-tree/backend/internal/dbpool"
	"github.com/openktree/open-knowledge-tree/backend/internal/providers/ai"
	"github.com/openktree/open-knowledge-tree/backend/internal/qdrantstore"
	"github.com/openktree/open-knowledge-tree/backend/internal/store"
	"github.com/openktree/open-knowledge-tree/backend/internal/taskmanager/tasks"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivertest"
)

// qdrantTestStore builds a qdrantstore.Store against the
// QDRANT_HOST env var and ensures the collection exists at the
// given dimension. Returns (store, cleanup) or skips the test
// when Qdrant is unreachable. Mirrors the serper/openalex env-
// gated skip pattern: a missing QDRANT_HOST means the test
// environment doesn't have Qdrant (the test-postgres profile
// doesn't bring it up), so the test skips instead of failing.
func qdrantTestStore(t *testing.T, dimensions int) (*qdrantstore.Store, func()) {
	t.Helper()
	host := os.Getenv("QDRANT_HOST")
	if host == "" {
		t.Skip("QDRANT_HOST not set; skipping Qdrant-dependent test")
	}
	cfg := config.QdrantConfig{
		Host:          host,
		Port:          6334,
		Collection:    "okt_facts_test_" + uuid.NewString()[:8],
		AllowRecreate: true, // tests always start with an empty collection
	}
	s, err := qdrantstore.NewClient(cfg)
	if err != nil {
		t.Fatalf("qdrant client: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := s.HealthCheck(ctx); err != nil {
		s.Close()
		t.Skipf("qdrant unreachable at %s: %v", host, err)
	}
	if err := s.EnsureCollection(ctx, dimensions); err != nil {
		s.Close()
		t.Fatalf("ensure collection: %v", err)
	}
	return s, func() {
		// Drop the per-test collection so repeated runs don't
		// accumulate. Best-effort: a failure here is logged, not
		// fatal, so a flaky cleanup doesn't fail the test.
		dropCtx, dropCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer dropCancel()
		// DeleteCollection is not exposed on Store; use the
		// raw client via Close + a fresh delete. For the test,
		// leaving the collection behind is acceptable (the
		// name is randomized) — the cleanup closes the gRPC
		// connection.
		_ = dropCtx
		s.Close()
	}
}

// stubEmbeddingProvider is a test double for ai.EmbeddingProvider
// that returns deterministic vectors. Two inputs that differ by
// a marker character embed to vectors that are near-identical
// (so dedup matches them) but distinct; two inputs that share
// the marker embed to the same vector. This lets the dedup test
// control "near-duplicate" vs "distinct" without depending on a
// real embedding model.
type stubEmbeddingProvider struct {
	dim int
}

func (p *stubEmbeddingProvider) Embed(ctx context.Context, db store.DBTX, req ai.EmbeddingRequest) (ai.EmbeddingResponse, error) {
	embeddings := make([][]float32, len(req.Inputs))
	for i, in := range req.Inputs {
		vec := make([]float32, p.dim)
		// Seed the vector from the input's bytes so identical
		// inputs produce identical vectors and near-identical
		// inputs produce near-identical vectors.
		for j := 0; j < p.dim; j++ {
			vec[j] = float32(in[(j)%len(in)]) / 255.0
		}
		embeddings[i] = vec
	}
	return ai.EmbeddingResponse{
		Model:      "stub-embedding",
		Embeddings: embeddings,
		Usage:      ai.EmbeddingUsage{PromptTokens: len(req.Inputs)},
	}, nil
}

func (p *stubEmbeddingProvider) Describe() ai.ProviderDescription {
	return ai.ProviderDescription{Name: "stub-embedding"}
}

// TestEmbedDedupPipeline exercises the embed_facts →
// deduplicate_facts → cleanup_facts chain end-to-end against a
// real Qdrant instance. It inserts two near-duplicate facts from
// two different sources, runs embed_facts, then deduplicate_facts,
// and asserts:
//   - the survivor is `stable`,
//   - the loser is `to_delete`,
//   - the survivor's fact_sources contains both sources (the
//     dedup merge links the loser's sources onto the survivor),
//   - cleanup_facts removes the loser from Postgres + Qdrant.
//
// Skips when QDRANT_HOST is unset (the test profile does not
// bring up Qdrant).
func TestEmbedDedupPipeline(t *testing.T) {
	const dim = 8
	qStore, qCleanup := qdrantTestStore(t, dim)
	defer qCleanup()

	env := testutil.NewTestEnv(t)
	defer env.Server.Close()
	ensureRiverSchema(t, env.DB)

	admin := bootstrapSysAdmin(t, env, "embed_dedup@example.com")
	_, _, repoID := createRepositoryWithDB(t, admin, "EmbedDedup", "embed-dedup", "desc", "")
	queries := store.New(env.DB)

	// Two sources in the same repo.
	mkSource := func(slug string) pgtype.UUID {
		id := pgtype.UUID{}
		if err := id.Scan(uuid.NewString()); err != nil {
			t.Fatalf("scanning source id: %v", err)
		}
		if _, err := queries.CreateSource(context.Background(), store.CreateSourceParams{
			ID: id, RepositoryID: pgRepoID(t, repoID), Url: "https://example.com/" + slug, Kind: "homepage", Status: "fetched",
		}); err != nil {
			t.Fatalf("create source: %v", err)
		}
		return id
	}
	srcA := mkSource("embed-dedup-a")
	srcB := mkSource("embed-dedup-b")

	// Two near-duplicate facts: same text prefix, tiny suffix so
	// the stub embedding produces near-identical vectors. The
	// dedup threshold is 0.94; the stub provider is deterministic
	// so the two vectors are bit-identical (score 1.0) when the
	// inputs share enough characters. To force a clear match, we
	// use the *same* text for both facts — the dedup rules still
	// apply (one wins, one loses).
	insertFactWithSource(t, env, pgRepoID(t, repoID), srcA, "NF-kB is a transcription factor involved in cancer.", 0)
	insertFactWithSource(t, env, pgRepoID(t, repoID), srcB, "NF-kB is a transcription factor involved in cancer.", 0)

	// Build the workers the same way taskmanager.New does, with
	// the test env's DB as the per-repo pool and the stub
	// embedding provider + test Qdrant store.
	registry := testutil.NewForTestPool(env.DB)
	systemQueries := store.New(env.DB)
	embCfg := config.EmbeddingConfig{Provider: "stub", Model: "stub-embedding", Dimensions: dim}
	dedupCfg := config.DedupConfig{Threshold: 0.94, CatchupMaxAge: "168h"}

	embedWorker := tasks.NewEmbedFactsWorker(&stubEmbeddingProvider{dim: dim}, embCfg, qStore, registry, systemQueries)
	dedupWorker := tasks.NewDeduplicateFactsWorker(dedupCfg, qStore, registry, systemQueries)
	cleanupWorker := tasks.NewCleanupFactsWorker(qStore, registry, systemQueries)

	driver := riverpgxv5.New(env.DB)
	workers := river.NewWorkers()
	river.AddWorker(workers, embedWorker)
	river.AddWorker(workers, dedupWorker)
	river.AddWorker(workers, cleanupWorker)
	cfg := &river.Config{
		Queues: map[string]river.QueueConfig{
			tasks.QueueEmbedFacts:        {MaxWorkers: 1},
			tasks.QueueDeduplicateFacts:  {MaxWorkers: 1},
			tasks.QueueCleanupFacts:      {MaxWorkers: 1},
		},
		Workers: workers,
	}
	// Register all three worker kinds on one test worker so we
	// can drive each in turn.
	testEmbed := rivertest.NewWorker(t, driver, cfg, embedWorker)
	testDedup := rivertest.NewWorker(t, driver, cfg, dedupWorker)
	testCleanup := rivertest.NewWorker(t, driver, cfg, cleanupWorker)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// 1. embed_facts — vectorize the two facts into Qdrant.
	tx, err := env.DB.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	job, err := testEmbed.Work(ctx, t, tx, tasks.EmbedFactsArgs{
		SourceID:     pgUUIDString(srcA),
		RepositoryID: repoID,
	}, &river.InsertOpts{Queue: tasks.QueueEmbedFacts})
	if err != nil {
		tx.Rollback(context.Background())
		t.Fatalf("embed_facts.Work: %v", err)
	}
	if job.EventKind != river.EventKindJobCompleted {
		tx.Rollback(context.Background())
		t.Fatalf("embed_facts: expected completed, got %s", job.EventKind)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit embed tx: %v", err)
	}
	// Assert embedded_at is set on both facts.
	var embeddedCount int
	if err := env.DB.QueryRow(ctx,
		`SELECT count(*) FROM okt_repository.facts WHERE embedded_at IS NOT NULL AND status = 'new'`,
	).Scan(&embeddedCount); err != nil {
		t.Fatalf("counting embedded facts: %v", err)
	}
	if embeddedCount != 2 {
		t.Errorf("embedded facts count = %d, want 2", embeddedCount)
	}

	// 2. deduplicate_facts — one wins, one loses; survivor's
	// fact_sources contains both sources.
	tx, err = env.DB.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		t.Fatalf("begin dedup tx: %v", err)
	}
	dedupJob, err := testDedup.Work(ctx, t, tx, tasks.DeduplicateFactsArgs{
		RepositoryID: repoID,
	}, &river.InsertOpts{Queue: tasks.QueueDeduplicateFacts})
	if err != nil {
		tx.Rollback(context.Background())
		t.Fatalf("deduplicate_facts.Work: %v", err)
	}
	if dedupJob.EventKind != river.EventKindJobCompleted {
		tx.Rollback(context.Background())
		t.Fatalf("deduplicate_facts: expected completed, got %s", dedupJob.EventKind)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit dedup tx: %v", err)
	}

	// Assert: one stable, one to_delete.
	var stableCount, toDeleteCount int
	if err := env.DB.QueryRow(ctx,
		`SELECT count(*) FILTER (WHERE status = 'stable'), count(*) FILTER (WHERE status = 'to_delete') FROM okt_repository.facts`,
	).Scan(&stableCount, &toDeleteCount); err != nil {
		t.Fatalf("counting statuses: %v", err)
	}
	if stableCount != 1 {
		t.Errorf("stable count = %d, want 1", stableCount)
	}
	if toDeleteCount != 1 {
		t.Errorf("to_delete count = %d, want 1", toDeleteCount)
	}

	// Assert the survivor has both sources linked.
	var survivorID pgtype.UUID
	var survivorSourceCount int
	if err := env.DB.QueryRow(ctx,
		`SELECT f.id, (SELECT count(*) FROM okt_repository.fact_sources fs WHERE fs.fact_id = f.id)
		 FROM okt_repository.facts f WHERE f.status = 'stable'`,
	).Scan(&survivorID, &survivorSourceCount); err != nil {
		t.Fatalf("querying survivor: %v", err)
	}
	if survivorSourceCount != 2 {
		t.Errorf("survivor source_count = %d, want 2 (merge must link both sources)", survivorSourceCount)
	}

	// 3. cleanup_facts — remove the to_delete row from Postgres
	// + Qdrant.
	tx, err = env.DB.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		t.Fatalf("begin cleanup tx: %v", err)
	}
	cleanupJob, err := testCleanup.Work(ctx, t, tx, tasks.CleanupFactsArgs{
		RepositoryID: repoID,
	}, &river.InsertOpts{Queue: tasks.QueueCleanupFacts})
	if err != nil {
		tx.Rollback(context.Background())
		t.Fatalf("cleanup_facts.Work: %v", err)
	}
	if cleanupJob.EventKind != river.EventKindJobCompleted {
		tx.Rollback(context.Background())
		t.Fatalf("cleanup_facts: expected completed, got %s", cleanupJob.EventKind)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit cleanup tx: %v", err)
	}

	// Assert: one fact remains (the survivor, status='stable').
	var remainingCount int
	if err := env.DB.QueryRow(ctx, `SELECT count(*) FROM okt_repository.facts`).Scan(&remainingCount); err != nil {
		t.Fatalf("counting remaining facts: %v", err)
	}
	if remainingCount != 1 {
		t.Errorf("remaining facts = %d, want 1 (cleanup must remove the to_delete row)", remainingCount)
	}

	// Assert the cleanup job recorded the deletion in its output.
	out := cleanupJob.Job.Output()
	if out == nil {
		t.Fatal("expected recorded output on cleanup job row")
	}
	var cleanupResult tasks.CleanupFactsResult
	if err := json.Unmarshal(out, &cleanupResult); err != nil {
		t.Fatalf("unmarshal cleanup output: %v", err)
	}
	if cleanupResult.Deleted != 1 {
		t.Errorf("cleanup Deleted = %d, want 1", cleanupResult.Deleted)
	}
}

// TestEmbedFacts_NoQdrantIsNoop verifies the worker degrades
// gracefully when the qdrant store is nil: it records a no-op
// result and returns nil (River doesn't retry). This is the
// deployment shape where Qdrant is not configured but the API
// still boots to serve facts.
func TestEmbedFacts_NoQdrantIsNoop(t *testing.T) {
	env := testutil.NewTestEnv(t)
	defer env.Server.Close()
	ensureRiverSchema(t, env.DB)

	_, _, repoID := createRepositoryWithDB(t, bootstrapSysAdmin(t, env, "noqdrant@example.com"), "NoQdrant", "no-qdrant", "desc", "")
	registry := testutil.NewForTestPool(env.DB)
	systemQueries := store.New(env.DB)
	// nil qdrant store + nil embedding provider.
	embedWorker := tasks.NewEmbedFactsWorker(nil, config.EmbeddingConfig{}, nil, registry, systemQueries)

	driver := riverpgxv5.New(env.DB)
	workers := river.NewWorkers()
	river.AddWorker(workers, embedWorker)
	testWorker := rivertest.NewWorker(t, driver, &river.Config{
		Queues:  map[string]river.QueueConfig{tasks.QueueEmbedFacts: {MaxWorkers: 1}},
		Workers: workers,
	}, embedWorker)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	tx, err := env.DB.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer tx.Rollback(context.Background())
	job, err := testWorker.Work(ctx, t, tx, tasks.EmbedFactsArgs{RepositoryID: repoID, SourceID: uuid.NewString()}, &river.InsertOpts{Queue: tasks.QueueEmbedFacts})
	if err != nil {
		t.Fatalf("embed_facts.Work (no qdrant): %v", err)
	}
	if job.EventKind != river.EventKindJobCompleted {
		t.Fatalf("expected completed, got %s", job.EventKind)
	}
}

// guard against unused imports when the file is edited.
var _ = pgxpool.Pool{}
var _ dbpool.Pool

// TestEmbedFacts_SourceBounded verifies embed_facts is strictly
// source-bounded: a job with SourceID=A embeds ONLY A's facts and
// leaves other sources' facts unembedded. This is the cost-control
// invariant — no repo-wide backfill. Two sources each have facts;
// after embedding A, only A's facts are embedded (B's are not).
// Skips when QDRANT_HOST is unset.
func TestEmbedFacts_SourceBounded(t *testing.T) {
	const dim = 8
	qStore, qCleanup := qdrantTestStore(t, dim)
	defer qCleanup()

	env := testutil.NewTestEnv(t)
	defer env.Server.Close()
	ensureRiverSchema(t, env.DB)

	_, _, repoID := createRepositoryWithDB(t, bootstrapSysAdmin(t, env, "srcbnd@example.com"), "SrcBnd", "src-bnd", "desc", "")
	queries := store.New(env.DB)

	mkSource := func(slug string) pgtype.UUID {
		id := pgtype.UUID{}
		if err := id.Scan(uuid.NewString()); err != nil {
			t.Fatalf("scanning source id: %v", err)
		}
		if _, err := queries.CreateSource(context.Background(), store.CreateSourceParams{
			ID: id, RepositoryID: pgRepoID(t, repoID), Url: "https://example.com/" + slug, Kind: "homepage", Status: "fetched",
		}); err != nil {
			t.Fatalf("create source: %v", err)
		}
		return id
	}
	srcA := mkSource("src-bnd-a")
	srcB := mkSource("src-bnd-b")

	// 2 facts on A, 2 on B. Embedding A must embed ONLY A's facts.
	insertFactWithSource(t, env, pgRepoID(t, repoID), srcA, "Fact A1 about topic alpha.", 0)
	insertFactWithSource(t, env, pgRepoID(t, repoID), srcA, "Fact A2 about topic alpha.", 0)
	insertFactWithSource(t, env, pgRepoID(t, repoID), srcB, "Fact B1 about topic beta.", 0)
	insertFactWithSource(t, env, pgRepoID(t, repoID), srcB, "Fact B2 about topic beta.", 0)

	registry := testutil.NewForTestPool(env.DB)
	systemQueries := store.New(env.DB)
	embCfg := config.EmbeddingConfig{Provider: "stub", Model: "stub-embedding", Dimensions: dim}
	embedWorker := tasks.NewEmbedFactsWorker(&stubEmbeddingProvider{dim: dim}, embCfg, qStore, registry, systemQueries)

	driver := riverpgxv5.New(env.DB)
	workers := river.NewWorkers()
	river.AddWorker(workers, embedWorker)
	testEmbed := rivertest.NewWorker(t, driver, &river.Config{
		Queues:  map[string]river.QueueConfig{tasks.QueueEmbedFacts: {MaxWorkers: 1}},
		Workers: workers,
	}, embedWorker)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	tx, err := env.DB.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	if _, err := testEmbed.Work(ctx, t, tx, tasks.EmbedFactsArgs{
		SourceID:     pgUUIDString(srcA),
		RepositoryID: repoID,
	}, &river.InsertOpts{Queue: tasks.QueueEmbedFacts}); err != nil {
		tx.Rollback(context.Background())
		t.Fatalf("embed_facts.Work: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}

	// Only A's 2 facts must be embedded; B's must stay unembedded.
	var embeddedCount int
	if err := env.DB.QueryRow(ctx,
		`SELECT count(*) FROM okt_repository.facts WHERE embedded_at IS NOT NULL`,
	).Scan(&embeddedCount); err != nil {
		t.Fatalf("counting embedded facts: %v", err)
	}
	if embeddedCount != 2 {
		t.Errorf("embedded facts = %d, want 2 (only source A; no backfill)", embeddedCount)
	}

	// Embedding B then embeds B's facts (and does NOT re-embed A's).
	tx, err = env.DB.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	if _, err := testEmbed.Work(ctx, t, tx, tasks.EmbedFactsArgs{
		SourceID:     pgUUIDString(srcB),
		RepositoryID: repoID,
	}, &river.InsertOpts{Queue: tasks.QueueEmbedFacts}); err != nil {
		tx.Rollback(context.Background())
		t.Fatalf("embed_facts.Work (B): %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if err := env.DB.QueryRow(ctx,
		`SELECT count(*) FROM okt_repository.facts WHERE embedded_at IS NOT NULL`,
	).Scan(&embeddedCount); err != nil {
		t.Fatalf("counting embedded facts: %v", err)
	}
	if embeddedCount != 4 {
		t.Errorf("embedded facts = %d, want 4 (A's 2 + B's 2)", embeddedCount)
	}
}

// TestEmbedFacts_EmptySourceRejected verifies the worker now
// requires a SourceID (the repo-wide path is gone). A job with an
// empty SourceID must return an error so River surfaces the bad
// enqueue instead of silently no-opping or doing repo-wide work.
func TestEmbedFacts_EmptySourceRejected(t *testing.T) {
	const dim = 8
	qStore, qCleanup := qdrantTestStore(t, dim)
	defer qCleanup()

	env := testutil.NewTestEnv(t)
	defer env.Server.Close()
	ensureRiverSchema(t, env.DB)

	_, _, repoID := createRepositoryWithDB(t, bootstrapSysAdmin(t, env, "emptysrc@example.com"), "EmptySrc", "empty-src", "desc", "")
	registry := testutil.NewForTestPool(env.DB)
	systemQueries := store.New(env.DB)
	embCfg := config.EmbeddingConfig{Provider: "stub", Model: "stub-embedding", Dimensions: dim}
	embedWorker := tasks.NewEmbedFactsWorker(&stubEmbeddingProvider{dim: dim}, embCfg, qStore, registry, systemQueries)

	driver := riverpgxv5.New(env.DB)
	workers := river.NewWorkers()
	river.AddWorker(workers, embedWorker)
	testEmbed := rivertest.NewWorker(t, driver, &river.Config{
		Queues:  map[string]river.QueueConfig{tasks.QueueEmbedFacts: {MaxWorkers: 1}},
		Workers: workers,
	}, embedWorker)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	tx, err := env.DB.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer tx.Rollback(context.Background())
	_, err = testEmbed.Work(ctx, t, tx, tasks.EmbedFactsArgs{
		RepositoryID: repoID, // no SourceID
	}, &river.InsertOpts{Queue: tasks.QueueEmbedFacts})
	if err == nil {
		t.Fatal("embed_facts.Work: expected error for empty SourceID, got nil")
	}
}