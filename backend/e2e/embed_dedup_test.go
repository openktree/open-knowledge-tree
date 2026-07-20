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

// TestDeduplicateFacts_SameBatchSameSourceDedup verifies the dedup
// worker collapses two near-duplicate facts extracted from the SAME
// source in the SAME embed batch. This is the regression test for
// the new-vs-new tie-break fix: with the old rule (lexicographically-
// larger UUID loses) the worker could mark one twin `to_delete`
// against an unrelated stable fact from elsewhere in the repo before
// the loop ever compared it to its same-batch twin — leaving both
// twins `stable` after promotion. The new rule (the hit loses, and
// is skipped when the loop reaches it) guarantees the pair collapses
// to one survivor regardless of what other facts exist in the repo.
//
// Reproduces the production bug observed on the Shadow Fleet
// investigation, where 1010 pairs of same-source same-batch facts at
// score >= 0.94 remained unmerged because both endpoints were
// promoted to `stable` without ever being compared to each other.
//
// Skips when QDRANT_HOST is unset.
func TestDeduplicateFacts_SameBatchSameSourceDedup(t *testing.T) {
	const dim = 8
	qStore, qCleanup := qdrantTestStore(t, dim)
	defer qCleanup()

	env := testutil.NewTestEnv(t)
	defer env.Server.Close()
	ensureRiverSchema(t, env.DB)

	admin := bootstrapSysAdmin(t, env, "samebatch@example.com")
	_, _, repoID := createRepositoryWithDB(t, admin, "SameBatch", "same-batch", "desc", "")
	queries := store.New(env.DB)

	// One source in the repo. Both near-duplicate facts come from
	// this single source — the bug is about same-source same-batch
	// duplicates, not cross-source.
	src := pgtype.UUID{}
	if err := src.Scan(uuid.NewString()); err != nil {
		t.Fatalf("scanning source id: %v", err)
	}
	if _, err := queries.CreateSource(context.Background(), store.CreateSourceParams{
		ID: src, RepositoryID: pgRepoID(t, repoID), Url: "https://example.com/same-batch", Kind: "homepage", Status: "fetched",
	}); err != nil {
		t.Fatalf("create source: %v", err)
	}

	// Two near-duplicate facts from the same source. Identical
	// text produces a bit-identical vector under the stub embedding
	// (score 1.0), so the dedup query at T=0.94 always matches.
	// Both facts are inserted as `new` (the default status) and
	// will be embedded in a single embed_facts pass below.
	insertFactWithSource(t, env, pgRepoID(t, repoID), src, "NF-kB is a transcription factor involved in cancer.", 0)
	insertFactWithSource(t, env, pgRepoID(t, repoID), src, "NF-kB is a transcription factor involved in cancer.", 1)

	registry := testutil.NewForTestPool(env.DB)
	systemQueries := store.New(env.DB)
	embCfg := config.EmbeddingConfig{Provider: "stub", Model: "stub-embedding", Dimensions: dim}
	dedupCfg := config.DedupConfig{Threshold: 0.94, CatchupMaxAge: "168h"}

	embedWorker := tasks.NewEmbedFactsWorker(&stubEmbeddingProvider{dim: dim}, embCfg, qStore, registry, systemQueries)
	dedupWorker := tasks.NewDeduplicateFactsWorker(dedupCfg, qStore, registry, systemQueries)

	driver := riverpgxv5.New(env.DB)
	workers := river.NewWorkers()
	river.AddWorker(workers, embedWorker)
	river.AddWorker(workers, dedupWorker)
	cfg := &river.Config{
		Queues: map[string]river.QueueConfig{
			tasks.QueueEmbedFacts:       {MaxWorkers: 1},
			tasks.QueueDeduplicateFacts: {MaxWorkers: 1},
		},
		Workers: workers,
	}
	testEmbed := rivertest.NewWorker(t, driver, cfg, embedWorker)
	testDedup := rivertest.NewWorker(t, driver, cfg, dedupWorker)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// 1. embed_facts — vectorize both same-source facts into Qdrant
	// in a single batch (mirrors production: one embed_facts job per
	// source).
	tx, err := env.DB.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		t.Fatalf("begin embed tx: %v", err)
	}
	if _, err := testEmbed.Work(ctx, t, tx, tasks.EmbedFactsArgs{
		SourceID:     pgUUIDString(src),
		RepositoryID: repoID,
	}, &river.InsertOpts{Queue: tasks.QueueEmbedFacts}); err != nil {
		tx.Rollback(context.Background())
		t.Fatalf("embed_facts.Work: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit embed tx: %v", err)
	}

	// Both facts are now embedded; both still `new`.
	var newCount int
	if err := env.DB.QueryRow(ctx,
		`SELECT count(*) FROM okt_repository.facts WHERE status = 'new' AND embedded_at IS NOT NULL`,
	).Scan(&newCount); err != nil {
		t.Fatalf("counting embedded new facts: %v", err)
	}
	if newCount != 2 {
		t.Fatalf("embedded new facts = %d, want 2 before dedup", newCount)
	}

	// 2. deduplicate_facts — the two same-batch same-source twins
	// must collapse to ONE survivor + ONE to_delete.
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

	// Assert: one stable, one to_delete. This is the core
	// regression assertion — with the old rule both would be
	// `stable` because the worker marked one twin `to_delete`
	// against itself or never compared them.
	var stableCount, toDeleteCount int
	if err := env.DB.QueryRow(ctx,
		`SELECT count(*) FILTER (WHERE status = 'stable'),
		        count(*) FILTER (WHERE status = 'to_delete')
		 FROM okt_repository.facts`,
	).Scan(&stableCount, &toDeleteCount); err != nil {
		t.Fatalf("counting statuses: %v", err)
	}
	if stableCount != 1 {
		t.Errorf("stable count = %d, want 1 (one twin must survive)", stableCount)
	}
	if toDeleteCount != 1 {
		t.Errorf("to_delete count = %d, want 1 (one twin must lose)", toDeleteCount)
	}

	// Assert the survivor still has its original source linked
	// (mergeSources is idempotent; the loser's source — which is
	// the same single source — is already linked via ON CONFLICT
	// DO NOTHING, so the survivor ends with exactly 1 source link,
	// not 2). This guards against a regression that dropped the
	// survivor's source link during the merge.
	var survivorSourceCount int
	if err := env.DB.QueryRow(ctx,
		`SELECT count(*) FROM okt_repository.fact_sources fs
		 JOIN okt_repository.facts f ON f.id = fs.fact_id
		 WHERE f.status = 'stable'`,
	).Scan(&survivorSourceCount); err != nil {
		t.Fatalf("counting survivor sources: %v", err)
	}
	if survivorSourceCount != 1 {
		t.Errorf("survivor source_count = %d, want 1 (same source, idempotent merge)", survivorSourceCount)
	}

	// Assert the dedup result output recorded one marked + one
	// promoted.
	out := dedupJob.Job.Output()
	if out == nil {
		t.Fatal("expected recorded output on dedup job row")
	}
	var dedupResult tasks.DeduplicateFactsResult
	if err := json.Unmarshal(out, &dedupResult); err != nil {
		t.Fatalf("unmarshal dedup output: %v", err)
	}
	if dedupResult.MarkedToDelete != 1 {
		t.Errorf("dedup MarkedToDelete = %d, want 1", dedupResult.MarkedToDelete)
	}
	if dedupResult.PromotedToStable != 1 {
		t.Errorf("dedup PromotedToStable = %d, want 1", dedupResult.PromotedToStable)
	}
}

// TestDeduplicateFacts_NewVsNewThirdFactWins verifies that when a new
// fact A has a near-duplicate new fact B as its nearest neighbor AND
// also a near-duplicate stable fact C, the new-vs-new rule fires (A
// wins over B, B is marked to_delete) and does NOT fall through to
// the new-vs-stable branch (which would have marked A to_delete
// against C). This pins the precedence: the worker takes the FIRST
// hit Qdrant returns (limit=1) and applies the rule for that hit's
// status, so the test sets up the vectors so B (new) is closer than
// C (stable).
//
// Skips when QDRANT_HOST is unset.
func TestDeduplicateFacts_NewVsNewThirdFactWins(t *testing.T) {
	const dim = 8
	qStore, qCleanup := qdrantTestStore(t, dim)
	defer qCleanup()

	env := testutil.NewTestEnv(t)
	defer env.Server.Close()
	ensureRiverSchema(t, env.DB)

	admin := bootstrapSysAdmin(t, env, "newvsnew@example.com")
	_, _, repoID := createRepositoryWithDB(t, admin, "NewVsNew", "new-vs-new", "desc", "")
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
	srcA := mkSource("newvsnew-a")
	srcC := mkSource("newvsnew-c")

	// Three facts. A and B are near-duplicate (same text → score
	// 1.0). C is also similar but with a different text → score
	// < 1.0 but still >= 0.94 (the stub embedding is deterministic
	// so two strings that share most characters will be close).
	// We embed C FIRST and run a dedup pass to promote C to
	// stable. Then we insert A and B, embed them together, and run
	// a second dedup pass — the bug is that A's nearest neighbor
	// might be C (stable) instead of B (new), causing A to be
	// marked to_delete against C and leaving B unmerged.
	//
	// To make the test deterministic, we use IDENTICAL text for A
	// and B (so A↔B score = 1.0) and a DIFFERENT text for C (so
	// A↔C score < 1.0). Qdrant returns the highest-scoring
	// neighbor first, so A's limit=1 query always returns B
	// (score 1.0), not C.
	//
	// We insert C FIRST (before pass 1) and A/B AFTER pass 1
	// because the worker's `MarkFactsStableByRepo` step promotes
	// ALL `new` facts in the repo to `stable` — including
	// not-yet-embedded ones. Inserting A/B after pass 1 keeps
	// them `new` for pass 2.
	insertFactWithSource(t, env, pgRepoID(t, repoID), srcC, "Coffee grows well at 1800m elevation in Costa Rica.", 0)

	registry := testutil.NewForTestPool(env.DB)
	systemQueries := store.New(env.DB)
	embCfg := config.EmbeddingConfig{Provider: "stub", Model: "stub-embedding", Dimensions: dim}
	dedupCfg := config.DedupConfig{Threshold: 0.94, CatchupMaxAge: "168h"}

	embedWorker := tasks.NewEmbedFactsWorker(&stubEmbeddingProvider{dim: dim}, embCfg, qStore, registry, systemQueries)
	dedupWorker := tasks.NewDeduplicateFactsWorker(dedupCfg, qStore, registry, systemQueries)

	driver := riverpgxv5.New(env.DB)
	workers := river.NewWorkers()
	river.AddWorker(workers, embedWorker)
	river.AddWorker(workers, dedupWorker)
	cfg := &river.Config{
		Queues: map[string]river.QueueConfig{
			tasks.QueueEmbedFacts:       {MaxWorkers: 1},
			tasks.QueueDeduplicateFacts: {MaxWorkers: 1},
		},
		Workers: workers,
	}
	testEmbed := rivertest.NewWorker(t, driver, cfg, embedWorker)
	testDedup := rivertest.NewWorker(t, driver, cfg, dedupWorker)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Pass 1: embed + dedup source C alone — promotes C to stable.
	tx, err := env.DB.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		t.Fatalf("begin embed tx: %v", err)
	}
	if _, err := testEmbed.Work(ctx, t, tx, tasks.EmbedFactsArgs{
		SourceID: pgUUIDString(srcC), RepositoryID: repoID,
	}, &river.InsertOpts{Queue: tasks.QueueEmbedFacts}); err != nil {
		tx.Rollback(context.Background())
		t.Fatalf("embed_facts.Work (C): %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit embed tx: %v", err)
	}
	tx, err = env.DB.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		t.Fatalf("begin dedup tx: %v", err)
	}
	if _, err := testDedup.Work(ctx, t, tx, tasks.DeduplicateFactsArgs{RepositoryID: repoID},
		&river.InsertOpts{Queue: tasks.QueueDeduplicateFacts}); err != nil {
		tx.Rollback(context.Background())
		t.Fatalf("dedup.Work (pass 1): %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit dedup tx: %v", err)
	}

	// After pass 1: C is stable.
	var stableCount1 int
	if err := env.DB.QueryRow(ctx,
		`SELECT count(*) FROM okt_repository.facts WHERE status = 'stable'`,
	).Scan(&stableCount1); err != nil {
		t.Fatalf("counting stable after pass 1: %v", err)
	}
	if stableCount1 != 1 {
		t.Fatalf("stable after pass 1 = %d, want 1 (only C)", stableCount1)
	}

	// NOW insert A and B (after pass 1) so they stay `new` for
	// pass 2. MarkFactsStableByRepo in pass 1 would have promoted
	// them if they'd existed earlier.
	insertFactWithSource(t, env, pgRepoID(t, repoID), srcA, "DNA was first isolated by Friedrich Miescher in 1869.", 0)
	insertFactWithSource(t, env, pgRepoID(t, repoID), srcA, "DNA was first isolated by Friedrich Miescher in 1869.", 1)

	// Pass 2: embed source A (embeds both A and B in one batch),
	// then dedup — the new-vs-new rule must collapse A and B.
	tx, err = env.DB.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		t.Fatalf("begin embed tx: %v", err)
	}
	if _, err := testEmbed.Work(ctx, t, tx, tasks.EmbedFactsArgs{
		SourceID: pgUUIDString(srcA), RepositoryID: repoID,
	}, &river.InsertOpts{Queue: tasks.QueueEmbedFacts}); err != nil {
		tx.Rollback(context.Background())
		t.Fatalf("embed_facts.Work (A+B): %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit embed tx: %v", err)
	}

	tx, err = env.DB.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		t.Fatalf("begin dedup tx: %v", err)
	}
	if _, err := testDedup.Work(ctx, t, tx, tasks.DeduplicateFactsArgs{RepositoryID: repoID},
		&river.InsertOpts{Queue: tasks.QueueDeduplicateFacts}); err != nil {
		tx.Rollback(context.Background())
		t.Fatalf("dedup.Work (pass 2): %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit dedup tx: %v", err)
	}

	// Final assertion: 2 stable (C + the survivor of A/B), 1
	// to_delete (the loser of A/B). With the old rule, A would be
	// marked to_delete against C if C was A's nearest neighbor —
	// but A and B share identical text so B is always A's nearest
	// neighbor at score 1.0, and the new-vs-new rule fires.
	var stableCount, toDeleteCount int
	if err := env.DB.QueryRow(ctx,
		`SELECT count(*) FILTER (WHERE status = 'stable'),
		        count(*) FILTER (WHERE status = 'to_delete')
		 FROM okt_repository.facts`,
	).Scan(&stableCount, &toDeleteCount); err != nil {
		t.Fatalf("counting final statuses: %v", err)
	}
	if stableCount != 2 {
		t.Errorf("stable count = %d, want 2 (C + A/B survivor)", stableCount)
	}
	if toDeleteCount != 1 {
		t.Errorf("to_delete count = %d, want 1 (A/B loser)", toDeleteCount)
	}
}