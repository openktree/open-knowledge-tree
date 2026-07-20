//go:build e2e

package e2e_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/openktree/open-knowledge-tree/backend/e2e/testutil"
	"github.com/openktree/open-knowledge-tree/backend/internal/config"
	"github.com/openktree/open-knowledge-tree/backend/internal/providers/posture"
	"github.com/openktree/open-knowledge-tree/backend/internal/qdrantstore"
	"github.com/openktree/open-knowledge-tree/backend/internal/store"
	"github.com/openktree/open-knowledge-tree/backend/internal/taskmanager/tasks"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivertest"
)

// TestAnnotateReportHybridNumericRetrieval covers the hybrid lexical
// fallback added to the annotate_report worker: when a report
// sentence quotes a numeric value (e.g. "0.9 kg weight gain") and a
// fact states the same value in different surrounding prose ("0.9
// kilograms increase in body weight"), the semantic Qdrant pass may
// miss it (cosine similarity below threshold) but the lexical
// tsvector fallback should surface it as an annotation.
//
// The fixture sets up:
//   - A fact whose text contains "0.9 kilograms increase in body
//     weight" — no shared words with the sentence beyond the number
//     "0.9" and the unit "kg" (the fact spells it "kilograms").
//   - A report sentence "The trial produced 0.9 kg weight gain and
//     nothing else matched."
//
// The stub embedding makes the sentence vector orthogonal to the
// fact vector (cosine below the high threshold we set), so Qdrant
// does NOT return the fact. The lexical fallback extracts "0.9" and
// "kg" from the sentence, builds the tsquery "0.9 & kg", and the
// fact's search_tsv matches because "0.9" is indexed verbatim. The
// worker persists the fact as an annotation with score 0.0 (the
// lexical-hit sentinel) and posture = NULL (the keep-all fallback,
// since no posture classifier is wired).
//
// Skips when QDRANT_HOST is unset.
func TestAnnotateReportHybridNumericRetrieval(t *testing.T) {
	const dim = 8
	qStore, qCleanup := qdrantTestStore(t, dim)
	defer qCleanup()

	env := testutil.NewTestEnv(t)
	defer env.Server.Close()
	ensureRiverSchema(t, env.DB)

	admin := bootstrapSysAdmin(t, env, "hybrid-numeric@example.com")
	_, _, repoID := createRepositoryWithDB(t, admin, "Hybrid Numeric Repo", "hybrid-numeric", "desc", "")
	queries := store.New(env.DB)
	pgRepo := pgRepoID(t, repoID)

	srcID := pgtype.UUID{}
	if err := srcID.Scan(uuid.NewString()); err != nil {
		t.Fatalf("scan source id: %v", err)
	}
	if _, err := queries.CreateSource(context.Background(), store.CreateSourceParams{
		ID: srcID, RepositoryID: pgRepo, Url: "https://example.com/hybrid", Kind: "homepage", Status: "fetched",
	}); err != nil {
		t.Fatalf("create source: %v", err)
	}

	// The fact uses "kilograms" (not "kg") so the stub embedding
	// won't see the "kg" token match; the sentence uses "0.9 kg".
	// Both share the bare token "0.9" which the tsvector index
	// catches. The fact text is deliberately worded to be
	// semantically close (same claim) but lexically distinct in
	// the non-numeric words so the test isolates the lexical path.
	factText := "Hall 2019: 0.9 kilograms increase in body weight versus the unprocessed diet arm."
	factID := insertFactWithSource(t, env, pgRepo, srcID, factText, 0)

	// High threshold + stub embedding that produces orthogonal
	// vectors for sentence vs fact so Qdrant returns NOTHING. The
	// lexical fallback is the only path that can surface the fact.
	embProvider := &stubEmbeddingProvider{dim: dim}
	embCfg := config.EmbeddingConfig{Provider: "stub", Model: "stub-embedding", Dimensions: dim}
	reportsCfg := config.ReportsConfig{
		Enabled:             true,
		SimilarityThreshold: 1.01, // >1.0 rejects every Qdrant hit (cosine <= 1.0); isolates the lexical path
		MaxFactsPerSentence: 5,
		MinSentenceRunes:    10,
	}

	registry := testutil.NewForTestPool(env.DB)
	systemQueries := store.New(env.DB)

	// Embed the fact so Qdrant has a vector for it (even though we
	// set the threshold too high for it to surface).
	embedWorker := tasks.NewEmbedFactsWorker(embProvider, embCfg, qStore, registry, systemQueries)
	driver := riverpgxv5.New(env.DB)
	workers := river.NewWorkers()
	river.AddWorker(workers, embedWorker)
	cfg := &river.Config{Workers: workers,
		Queues: map[string]river.QueueConfig{tasks.QueueEmbedFacts: {MaxWorkers: 1}}}
	testEmbed := rivertest.NewWorker(t, driver, cfg, embedWorker)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	tx, err := env.DB.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	if _, err := testEmbed.Work(ctx, t, tx, tasks.EmbedFactsArgs{
		SourceID:     pgUUIDString(srcID),
		RepositoryID: repoID,
	}, &river.InsertOpts{Queue: tasks.QueueEmbedFacts}); err != nil {
		tx.Rollback(context.Background())
		t.Fatalf("embed_facts.Work: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit embed tx: %v", err)
	}

	// annotate_report worker with NO posture classifier (keep-all
	// fallback) so the lexical hit persists with posture = NULL.
	annotateWorker := tasks.NewAnnotateReportWorker(embProvider, embCfg, reportsCfg, posture.Classifier(nil), qStore, registry, systemQueries, nil, nil)
	annotateWorkers := river.NewWorkers()
	river.AddWorker(annotateWorkers, annotateWorker)
	annotateCfg := &river.Config{Workers: annotateWorkers,
		Queues: map[string]river.QueueConfig{tasks.QueueAnnotateReport: {MaxWorkers: 1}}}
	testAnnotate := rivertest.NewWorker(t, driver, annotateCfg, annotateWorker)

	reportID := pgtype.UUID{}
	if err := reportID.Scan(uuid.NewString()); err != nil {
		t.Fatalf("scan report id: %v", err)
	}
	body := "The trial produced 0.9 kg weight gain and nothing else matched."
	if _, err := queries.CreateReport(ctx, store.CreateReportParams{
		ID:           reportID,
		RepositoryID: pgRepo,
		Title:        "Hybrid numeric test",
		BodyMd:       body,
		Status:       "pending",
	}); err != nil {
		t.Fatalf("create report: %v", err)
	}

	tx2, err := env.DB.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		t.Fatalf("begin tx2: %v", err)
	}
	job, err := testAnnotate.Work(ctx, t, tx2, tasks.AnnotateReportArgs{
		ReportID:     pgUUIDString(reportID),
		RepositoryID: repoID,
	}, &river.InsertOpts{Queue: tasks.QueueAnnotateReport})
	if err != nil {
		tx2.Rollback(context.Background())
		t.Fatalf("annotate_report.Work: %v", err)
	}
	if err := tx2.Commit(ctx); err != nil {
		t.Fatalf("commit annotate tx: %v", err)
	}
	if job.EventKind != river.EventKindJobCompleted {
		t.Fatalf("annotate_report: expected completed, got %s", job.EventKind)
	}

	anns, err := queries.ListReportAnnotationsByReport(ctx, reportID)
	if err != nil {
		t.Fatalf("list annotations: %v", err)
	}

	// The lexical fallback should have produced exactly one
	// annotation pointing at the fact we seeded. The semantic gate
	// (lexical_similarity_floor) re-checked the cosine between the
	// sentence embedding and the fact vector; the hit survived
	// because the stub embedding produces a non-trivial cosine for
	// the two texts. The persisted score is the actual cosine (NOT
	// a sentinel 0.0) so the UI's score badge reflects how strong
	// the semantic match really was and the hit ranks naturally
	// among the Qdrant hits.
	var foundLexical bool
	for _, a := range anns {
		if a.FactID.String() == factID {
			foundLexical = true
			// Lexical hits carry the real cosine similarity
			// against the sentence embedding (computed by the
			// worker via GetFactVectorsByIDs), not a sentinel.
			// The stub embedding produces a non-zero cosine
			// for any two non-empty inputs, so the score is
			// > 0. We assert it's in a sane range rather than
			// pinning the exact value (the stub's byte-seeded
			// vectors make exact values brittle).
			if a.Score < 0.0 || a.Score > 1.0 {
				t.Errorf("lexical annotation score = %v, want in [0.0, 1.0]", a.Score)
			}
			// The score must be at least the lexical floor
			// (the gate dropped anything below). The default
			// floor is 0.6, so the surviving hit must be >= 0.6.
			if a.Score < 0.6 {
				t.Errorf("lexical annotation score = %v, want >= 0.6 (the lexical_similarity_floor)", a.Score)
			}
			// Keep-all fallback: posture is NULL because no
			// classifier was wired.
			if a.Posture != nil {
				t.Errorf("lexical annotation posture = %v, want nil (keep-all)", *a.Posture)
			}
		}
	}
	if !foundLexical {
		t.Errorf("lexical fallback did not surface fact %s; annotations: %d", factID, len(anns))
		for i, a := range anns {
			t.Logf("  ann[%d] fact=%s score=%v", i, a.FactID, a.Score)
		}
	}
}

// TestAnnotateReportHybridNumericNoFalsePositives covers the deny
// case: when a sentence has no numeric tokens, the lexical fallback
// must NOT run and must not produce annotations for facts that
// happen to share a common prose word. This guards against the
// hybrid retrieval degenerating into a generic prose-similarity
// search (which the semantic Qdrant pass already covers).
func TestAnnotateReportHybridNumericNoFalsePositives(t *testing.T) {
	const dim = 8
	qStore, qCleanup := qdrantTestStore(t, dim)
	defer qCleanup()

	env := testutil.NewTestEnv(t)
	defer env.Server.Close()
	ensureRiverSchema(t, env.DB)

	admin := bootstrapSysAdmin(t, env, "hybrid-neg@example.com")
	_, _, repoID := createRepositoryWithDB(t, admin, "Hybrid Neg Repo", "hybrid-neg", "desc", "")
	queries := store.New(env.DB)
	pgRepo := pgRepoID(t, repoID)

	srcID := pgtype.UUID{}
	if err := srcID.Scan(uuid.NewString()); err != nil {
		t.Fatalf("scan source id: %v", err)
	}
	if _, err := queries.CreateSource(context.Background(), store.CreateSourceParams{
		ID: srcID, RepositoryID: pgRepo, Url: "https://example.com/hybrid-neg", Kind: "homepage", Status: "fetched",
	}); err != nil {
		t.Fatalf("create source: %v", err)
	}

	// A fact with prose words that might coincidentally match a
	// non-numeric sentence — the lexical fallback should NOT fire.
	// The fact text is chosen so the stub embedding (which seeds
	// vectors from input bytes) produces a cosine below the 0.99
	// threshold against the sentence below, so the Qdrant pass
	// also does not surface it — isolating the test to the
	// lexical-fallback deny path.
	factText := "The regulatory environment across jurisdictions reflects institutional heterogeneity."
	_ = insertFactWithSource(t, env, pgRepo, srcID, factText, 0)

	embProvider := &stubEmbeddingProvider{dim: dim}
	embCfg := config.EmbeddingConfig{Provider: "stub", Model: "stub-embedding", Dimensions: dim}
	reportsCfg := config.ReportsConfig{
		Enabled:             true,
		SimilarityThreshold: 1.01, // >1.0 rejects every Qdrant hit (cosine <= 1.0); isolates the lexical deny path
		MaxFactsPerSentence: 5,
		MinSentenceRunes:    10,
	}

	registry := testutil.NewForTestPool(env.DB)
	systemQueries := store.New(env.DB)

	embedWorker := tasks.NewEmbedFactsWorker(embProvider, embCfg, qStore, registry, systemQueries)
	driver := riverpgxv5.New(env.DB)
	workers := river.NewWorkers()
	river.AddWorker(workers, embedWorker)
	cfg := &river.Config{Workers: workers,
		Queues: map[string]river.QueueConfig{tasks.QueueEmbedFacts: {MaxWorkers: 1}}}
	testEmbed := rivertest.NewWorker(t, driver, cfg, embedWorker)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	tx, err := env.DB.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	if _, err := testEmbed.Work(ctx, t, tx, tasks.EmbedFactsArgs{
		SourceID:     pgUUIDString(srcID),
		RepositoryID: repoID,
	}, &river.InsertOpts{Queue: tasks.QueueEmbedFacts}); err != nil {
		tx.Rollback(context.Background())
		t.Fatalf("embed_facts.Work: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit embed tx: %v", err)
	}

	annotateWorker := tasks.NewAnnotateReportWorker(embProvider, embCfg, reportsCfg, posture.Classifier(nil), qStore, registry, systemQueries, nil, nil)
	annotateWorkers := river.NewWorkers()
	river.AddWorker(annotateWorkers, annotateWorker)
	annotateCfg := &river.Config{Workers: annotateWorkers,
		Queues: map[string]river.QueueConfig{tasks.QueueAnnotateReport: {MaxWorkers: 1}}}
	testAnnotate := rivertest.NewWorker(t, driver, annotateCfg, annotateWorker)

	reportID := pgtype.UUID{}
	if err := reportID.Scan(uuid.NewString()); err != nil {
		t.Fatalf("scan report id: %v", err)
	}
	// Sentence with NO numeric tokens — the lexical fallback should
	// skip it entirely. The sentence is deliberately lexically
	// distinct from the fact above so the stub embedding produces
	// a low cosine and the Qdrant pass also does not surface it.
	body := "The author concludes with remarks about the broader research agenda."
	if _, err := queries.CreateReport(ctx, store.CreateReportParams{
		ID:           reportID,
		RepositoryID: pgRepo,
		Title:        "Hybrid neg test",
		BodyMd:       body,
		Status:       "pending",
	}); err != nil {
		t.Fatalf("create report: %v", err)
	}

	tx2, err := env.DB.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		t.Fatalf("begin tx2: %v", err)
	}
	if _, err := testAnnotate.Work(ctx, t, tx2, tasks.AnnotateReportArgs{
		ReportID:     pgUUIDString(reportID),
		RepositoryID: repoID,
	}, &river.InsertOpts{Queue: tasks.QueueAnnotateReport}); err != nil {
		tx2.Rollback(context.Background())
		t.Fatalf("annotate_report.Work: %v", err)
	}
	if err := tx2.Commit(ctx); err != nil {
		t.Fatalf("commit annotate tx: %v", err)
	}

	anns, err := queries.ListReportAnnotationsByReport(ctx, reportID)
	if err != nil {
		t.Fatalf("list annotations: %v", err)
	}
	if len(anns) != 0 {
		t.Errorf("non-numeric sentence produced %d annotations, want 0 (no lexical fallback)", len(anns))
		for i, a := range anns {
			t.Logf("  ann[%d] fact=%s score=%v", i, a.FactID, a.Score)
		}
	}
}

// silence unused import warnings if QDRANT_HOST is unset and both
// tests skip — qdrantstore is referenced only inside the test bodies.
var _ = qdrantstore.Store{}

// TestAnnotateReportHybridNumericSemanticGateDrops exercises the
// apples-to-oranges guard: when a sentence quotes "0.9 kg weight gain"
// and a fact about the same number exists but in a different claim
// context, the lexical tsvector match surfaces the fact, but the
// semantic gate (lexical_similarity_floor) re-checks the cosine
// similarity against the sentence embedding and drops the hit when
// the cosine is below the floor. We can't control the stub embedding's
// cosine precisely, so we set the floor to an impossibly high value
// (2.0 — cosine is bounded ≤ 1.0) so EVERY lexical hit is dropped by
// the gate. This confirms the gate runs and drops; the inverse
// (gate accepts a real-semantic-match lexical hit) is covered by
// TestAnnotateReportHybridNumericRetrieval which uses the default
// 0.6 floor and asserts the hit survives.
func TestAnnotateReportHybridNumericSemanticGateDrops(t *testing.T) {
	const dim = 8
	qStore, qCleanup := qdrantTestStore(t, dim)
	defer qCleanup()

	env := testutil.NewTestEnv(t)
	defer env.Server.Close()
	ensureRiverSchema(t, env.DB)

	admin := bootstrapSysAdmin(t, env, "hybrid-gate@example.com")
	_, _, repoID := createRepositoryWithDB(t, admin, "Hybrid Gate Repo", "hybrid-gate", "desc", "")
	queries := store.New(env.DB)
	pgRepo := pgRepoID(t, repoID)

	srcID := pgtype.UUID{}
	if err := srcID.Scan(uuid.NewString()); err != nil {
		t.Fatalf("scan source id: %v", err)
	}
	if _, err := queries.CreateSource(context.Background(), store.CreateSourceParams{
		ID: srcID, RepositoryID: pgRepo, Url: "https://example.com/hybrid-gate", Kind: "homepage", Status: "fetched",
	}); err != nil {
		t.Fatalf("create source: %v", err)
	}

	// A fact with the same numeric token as the sentence ("0.9") but
	// deliberately different surrounding prose so the semantic gate
	// (which the test forces to drop everything via an impossible
	// floor) rejects it.
	factText := "Global CO2 emissions from transport rose by 0.9 percent in 2023."
	factID := insertFactWithSource(t, env, pgRepo, srcID, factText, 0)

	embProvider := &stubEmbeddingProvider{dim: dim}
	embCfg := config.EmbeddingConfig{Provider: "stub", Model: "stub-embedding", Dimensions: dim}
	// lexical_similarity_floor = 2.0 is above the maximum possible
	// cosine (1.0), so EVERY lexical hit gets dropped by the gate.
	// The Qdrant pass is also suppressed (threshold 1.01) so no
	// semantic hit leaks through; the test isolates the gate's
	// drop behavior.
	reportsCfg := config.ReportsConfig{
		Enabled:                true,
		SimilarityThreshold:    1.01,
		LexicalSimilarityFloor: 2.0,
		MaxFactsPerSentence:    5,
		MinSentenceRunes:      10,
	}

	registry := testutil.NewForTestPool(env.DB)
	systemQueries := store.New(env.DB)

	embedWorker := tasks.NewEmbedFactsWorker(embProvider, embCfg, qStore, registry, systemQueries)
	driver := riverpgxv5.New(env.DB)
	workers := river.NewWorkers()
	river.AddWorker(workers, embedWorker)
	cfg := &river.Config{Workers: workers,
		Queues: map[string]river.QueueConfig{tasks.QueueEmbedFacts: {MaxWorkers: 1}}}
	testEmbed := rivertest.NewWorker(t, driver, cfg, embedWorker)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	tx, err := env.DB.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	if _, err := testEmbed.Work(ctx, t, tx, tasks.EmbedFactsArgs{
		SourceID:     pgUUIDString(srcID),
		RepositoryID: repoID,
	}, &river.InsertOpts{Queue: tasks.QueueEmbedFacts}); err != nil {
		tx.Rollback(context.Background())
		t.Fatalf("embed_facts.Work: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit embed tx: %v", err)
	}

	annotateWorker := tasks.NewAnnotateReportWorker(embProvider, embCfg, reportsCfg, posture.Classifier(nil), qStore, registry, systemQueries, nil, nil)
	annotateWorkers := river.NewWorkers()
	river.AddWorker(annotateWorkers, annotateWorker)
	annotateCfg := &river.Config{Workers: annotateWorkers,
		Queues: map[string]river.QueueConfig{tasks.QueueAnnotateReport: {MaxWorkers: 1}}}
	testAnnotate := rivertest.NewWorker(t, driver, annotateCfg, annotateWorker)

	reportID := pgtype.UUID{}
	if err := reportID.Scan(uuid.NewString()); err != nil {
		t.Fatalf("scan report id: %v", err)
	}
	body := "The trial produced 0.9 kg weight gain."
	if _, err := queries.CreateReport(ctx, store.CreateReportParams{
		ID:           reportID,
		RepositoryID: pgRepo,
		Title:        "Hybrid gate test",
		BodyMd:       body,
		Status:       "pending",
	}); err != nil {
		t.Fatalf("create report: %v", err)
	}

	tx2, err := env.DB.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		t.Fatalf("begin tx2: %v", err)
	}
	if _, err := testAnnotate.Work(ctx, t, tx2, tasks.AnnotateReportArgs{
		ReportID:     pgUUIDString(reportID),
		RepositoryID: repoID,
	}, &river.InsertOpts{Queue: tasks.QueueAnnotateReport}); err != nil {
		tx2.Rollback(context.Background())
		t.Fatalf("annotate_report.Work: %v", err)
	}
	if err := tx2.Commit(ctx); err != nil {
		t.Fatalf("commit annotate tx: %v", err)
	}

	anns, err := queries.ListReportAnnotationsByReport(ctx, reportID)
	if err != nil {
		t.Fatalf("list annotations: %v", err)
	}
	// The semantic gate must have dropped the lexical hit. No
	// annotation should reference the CO2-emissions fact.
	for _, a := range anns {
		if a.FactID.String() == factID {
			t.Errorf("semantic gate failed to drop lexical hit for fact %s (apples-to-oranges match should be rejected by lexical_similarity_floor)", factID)
		}
	}
	if len(anns) != 0 {
		t.Errorf("expected 0 annotations after semantic gate dropped the lexical hit, got %d", len(anns))
		for i, a := range anns {
			t.Logf("  ann[%d] fact=%s score=%v", i, a.FactID, a.Score)
		}
	}
}