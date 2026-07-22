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
	"github.com/openktree/open-knowledge-tree/backend/internal/providers/claims"
	"github.com/openktree/open-knowledge-tree/backend/internal/providers/posture"
	"github.com/openktree/open-knowledge-tree/backend/internal/qdrantstore"
	"github.com/openktree/open-knowledge-tree/backend/internal/store"
	"github.com/openktree/open-knowledge-tree/backend/internal/taskmanager/tasks"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivertest"
)

// stubClaimExtractor is a test double for claims.Extractor that
// returns a canned claim per sentence_index. The test sets up
// `claimsByIndex` upfront; sentences missing from the map get no
// claims (the worker uses embedding-only retrieval for them).
type stubClaimExtractor struct {
	claimsByIndex map[int][]claims.Claim
}

func (e *stubClaimExtractor) Extract(ctx context.Context, db store.DBTX, req claims.ExtractRequest) ([]claims.SentenceClaims, error) {
	out := make([]claims.SentenceClaims, 0, len(req.Sentences))
	for _, s := range req.Sentences {
		if cs, ok := e.claimsByIndex[s.Index]; ok && len(cs) > 0 {
			out = append(out, claims.SentenceClaims{SentenceIndex: s.Index, Claims: cs})
		}
	}
	return out, nil
}

func (e *stubClaimExtractor) Describe() claims.ProviderDescription {
	return claims.ProviderDescription{Name: "stub-claims", Configured: true}
}
func (e *stubClaimExtractor) Configured() bool { return true }

// TestAnnotateReportClaimDrivenNumericRetrieval covers the
// claim-driven numeric retrieval path: when a report sentence
// quotes a numeric value (e.g. "0.9 kg weight gain") and a fact
// states the same value in different surrounding prose ("0.9
// kilograms increase in body weight"), the semantic Qdrant pass may
// miss it (cosine similarity below threshold) but the claim
// extractor emits a numeric claim whose Term "0.9 kg" drives a
// tsvector search that surfaces the fact.
//
// The fixture sets up:
//   - A fact whose text contains "0.9 kilograms increase in body
//     weight" — no shared words with the sentence beyond the number
//     "0.9" and the unit "kg"/"kilograms".
//   - A report sentence "The trial produced 0.9 kg weight gain and
//     nothing else matched."
//
// The stub embedding makes the sentence vector orthogonal to the
// fact vector (cosine below the high threshold we set), so Qdrant
// does NOT return the fact. The stub claim extractor emits a
// numeric claim with Term "0.9 kg" for the sentence; the worker
// runs extractNumericTsquery("0.9 kg") → tsquery, the fact's
// search_tsv matches because "0.9" is indexed verbatim. The worker
// persists the fact as an annotation with the actual cosine (NOT a
// sentinel) and posture = NULL (the keep-all fallback, since no
// posture classifier is wired).
//
// Skips when QDRANT_HOST is unset.
func TestAnnotateReportClaimDrivenNumericRetrieval(t *testing.T) {
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
	// the non-numeric words so the test isolates the claim-driven
	// numeric path.
	factText := "Hall 2019: 0.9 kilograms increase in body weight versus the unprocessed diet arm."
	factID := insertFactWithSource(t, env, pgRepo, srcID, factText, 0)

	// High threshold + stub embedding that produces orthogonal
	// vectors for sentence vs fact so Qdrant returns NOTHING. The
	// claim-driven numeric path is the only path that can surface
	// the fact.
	embProvider := &stubEmbeddingProvider{dim: dim}
	embCfg := config.EmbeddingConfig{Provider: "stub", Model: "stub-embedding", Dimensions: dim}
	reportsCfg := config.ReportsConfig{
		Enabled:             true,
		SimilarityThreshold: 1.01, // >1.0 rejects every Qdrant hit (cosine <= 1.0); isolates the claim-driven path
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

	// Stub claim extractor emits one numeric claim with Term "0.9 kg"
	// for the sentence at index 0 (the only candidate after the
	// min-runes filter). The worker runs extractNumericTsquery("0.9 kg")
	// → `"0.9" & ( "kg" | "kilogram" | "kilograms" )` so the fact
	// whose search_tsv contains "0.9 kilograms" matches.
	claimExtractor := &stubClaimExtractor{claimsByIndex: map[int][]claims.Claim{
		0: {{Type: claims.ClaimNumeric, Term: "0.9 kg", Context: "0.9 kg weight gain"}},
	}}

	// annotate_report worker with NO posture classifier (keep-all
	// fallback) so the claim-driven hit persists with posture = NULL.
	annotateWorker := tasks.NewAnnotateReportWorker(embProvider, embCfg, reportsCfg, posture.Classifier(nil), claimExtractor, qStore, registry, systemQueries, nil, nil)
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
		Title:        "Claim-driven numeric test",
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

	// The claim-driven numeric path should have produced exactly one
	// annotation pointing at the fact we seeded. The semantic gate
	// (lexical_similarity_floor) re-checked the cosine between the
	// sentence embedding and the fact vector; the hit survived
	// because the stub embedding produces a non-trivial cosine for
	// the two texts. The persisted score is the actual cosine (NOT a
	// sentinel 0.0) so the UI's score badge reflects how strong the
	// semantic match really was.
	var found bool
	for _, a := range anns {
		if a.FactID.String() == factID {
			found = true
			if a.Score < 0.0 || a.Score > 1.0 {
				t.Errorf("claim-driven annotation score = %v, want in [0.0, 1.0]", a.Score)
			}
			// The score must be at least the lexical floor (the
			// gate dropped anything below). The default floor is
			// 0.6, so the surviving hit must be >= 0.6.
			if a.Score < 0.6 {
				t.Errorf("claim-driven annotation score = %v, want >= 0.6 (the lexical_similarity_floor)", a.Score)
			}
			// Keep-all fallback: posture is NULL because no
			// classifier was wired.
			if a.Posture != nil {
				t.Errorf("claim-driven annotation posture = %v, want nil (keep-all)", *a.Posture)
			}
		}
	}
	if !found {
		t.Errorf("claim-driven numeric path did not surface fact %s; annotations: %d", factID, len(anns))
		for i, a := range anns {
			t.Logf("  ann[%d] fact=%s score=%v", i, a.FactID, a.Score)
		}
	}
}

// TestAnnotateReportClaimDrivenNoFalsePositives covers the deny
// case: when a sentence has no extracted numeric claims (the stub
// claim extractor returns nothing for it), the claim-driven numeric
// path must NOT run and must not produce annotations for facts that
// happen to share a common prose word. This guards against the
// claim-driven retrieval degenerating into a generic prose-
// similarity search (which the semantic Qdrant pass already
// covers).
func TestAnnotateReportClaimDrivenNoFalsePositives(t *testing.T) {
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
	// non-numeric sentence — the claim-driven path should NOT fire
	// because the stub extractor emits no claims for the sentence.
	factText := "The regulatory environment across jurisdictions reflects institutional heterogeneity."
	_ = insertFactWithSource(t, env, pgRepo, srcID, factText, 0)

	embProvider := &stubEmbeddingProvider{dim: dim}
	embCfg := config.EmbeddingConfig{Provider: "stub", Model: "stub-embedding", Dimensions: dim}
	reportsCfg := config.ReportsConfig{
		Enabled:             true,
		SimilarityThreshold: 1.01, // >1.0 rejects every Qdrant hit; isolates the deny path
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

	// Stub extractor with an EMPTY claims map — no claims for any
	// sentence. Confirms a no-claim sentence produces no claim-driven
	// retrieval.
	claimExtractor := &stubClaimExtractor{claimsByIndex: map[int][]claims.Claim{}}

	annotateWorker := tasks.NewAnnotateReportWorker(embProvider, embCfg, reportsCfg, posture.Classifier(nil), claimExtractor, qStore, registry, systemQueries, nil, nil)
	annotateWorkers := river.NewWorkers()
	river.AddWorker(annotateWorkers, annotateWorker)
	annotateCfg := &river.Config{Workers: annotateWorkers,
		Queues: map[string]river.QueueConfig{tasks.QueueAnnotateReport: {MaxWorkers: 1}}}
	testAnnotate := rivertest.NewWorker(t, driver, annotateCfg, annotateWorker)

	reportID := pgtype.UUID{}
	if err := reportID.Scan(uuid.NewString()); err != nil {
		t.Fatalf("scan report id: %v", err)
	}
	body := "The author concludes with remarks about the broader research agenda."
	if _, err := queries.CreateReport(ctx, store.CreateReportParams{
		ID:           reportID,
		RepositoryID: pgRepo,
		Title:        "Claim-driven neg test",
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
		t.Errorf("no-claim sentence produced %d annotations, want 0 (no claim-driven retrieval)", len(anns))
		for i, a := range anns {
			t.Logf("  ann[%d] fact=%s score=%v", i, a.FactID, a.Score)
		}
	}
}

// silence unused import warnings if QDRANT_HOST is unset and tests
// skip — qdrantstore is referenced only inside the test bodies.
var _ = qdrantstore.Store{}

// TestAnnotateReportClaimDrivenSemanticGateDrops exercises the
// apples-to-oranges guard on the claim-driven path: when a sentence
// quotes "0.9 kg weight gain" and a fact about the same number
// exists but in a different claim context, the claim-driven numeric
// retrieval surfaces the fact (tsquery matches on "0.9"), but the
// semantic gate (lexical_similarity_floor) re-checks the cosine
// similarity against the sentence embedding and drops the hit when
// the cosine is below the floor. We set the floor to an impossibly
// high value (2.0 — cosine is bounded ≤ 1.0) so EVERY claim-driven
// hit is dropped by the gate. This confirms the gate runs and drops
// on the claim-driven path; the inverse (gate accepts a real-
// semantic-match claim-driven hit) is covered by
// TestAnnotateReportClaimDrivenNumericRetrieval which uses the
// default 0.6 floor and asserts the hit survives.
func TestAnnotateReportClaimDrivenSemanticGateDrops(t *testing.T) {
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
	// cosine (1.0), so EVERY claim-driven hit gets dropped by the
	// gate. The Qdrant pass is also suppressed (threshold 1.01) so
	// no semantic hit leaks through; the test isolates the gate's
	// drop behavior on the claim-driven path.
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

	claimExtractor := &stubClaimExtractor{claimsByIndex: map[int][]claims.Claim{
		0: {{Type: claims.ClaimNumeric, Term: "0.9 kg", Context: "0.9 kg weight gain"}},
	}}

	annotateWorker := tasks.NewAnnotateReportWorker(embProvider, embCfg, reportsCfg, posture.Classifier(nil), claimExtractor, qStore, registry, systemQueries, nil, nil)
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
		Title:        "Claim-driven gate test",
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
	// The semantic gate must have dropped the claim-driven hit. No
	// annotation should reference the CO2-emissions fact.
	for _, a := range anns {
		if a.FactID.String() == factID {
			t.Errorf("semantic gate failed to drop claim-driven hit for fact %s (apples-to-oranges match should be rejected by lexical_similarity_floor)", factID)
		}
	}
	if len(anns) != 0 {
		t.Errorf("expected 0 annotations after semantic gate dropped the claim-driven hit, got %d", len(anns))
		for i, a := range anns {
			t.Logf("  ann[%d] fact=%s score=%v", i, a.FactID, a.Score)
		}
	}
}

// TestAnnotateReportDirectCitationExtraction covers Phase 0: when
// a report body contains inline fact:uuid citations (the synthesis
// convention [text](<fact:uuid>)), the worker persists each cited
// fact as an annotation with posture="supports" and score=1.0,
// OUTSIDE the maxFacts cap and WITHOUT going through the posture
// classifier (the author's explicit citation overrides the LLM).
//
// The fixture seeds two facts, embeds them, then creates a report
// whose single sentence cites both via [text](<fact:uuid>) links.
// We set maxFactsPerSentence=1 so the top-N embedding pool would
// only hold 1 fact — but both direct citations persist as extras.
// We also set threshold=1.01 so NO embedding hit surfaces; the only
// annotations should be the two direct citations.
func TestAnnotateReportDirectCitationExtraction(t *testing.T) {
	const dim = 8
	qStore, qCleanup := qdrantTestStore(t, dim)
	defer qCleanup()

	env := testutil.NewTestEnv(t)
	defer env.Server.Close()
	ensureRiverSchema(t, env.DB)

	admin := bootstrapSysAdmin(t, env, "direct-cite@example.com")
	_, _, repoID := createRepositoryWithDB(t, admin, "Direct Cite Repo", "direct-cite", "desc", "")
	queries := store.New(env.DB)
	pgRepo := pgRepoID(t, repoID)

	srcID := pgtype.UUID{}
	if err := srcID.Scan(uuid.NewString()); err != nil {
		t.Fatalf("scan source id: %v", err)
	}
	if _, err := queries.CreateSource(context.Background(), store.CreateSourceParams{
		ID: srcID, RepositoryID: pgRepo, Url: "https://example.com/direct", Kind: "homepage", Status: "fetched",
	}); err != nil {
		t.Fatalf("create source: %v", err)
	}

	factA := insertFactWithSource(t, env, pgRepo, srcID, "Coffee grows well at 1800m in Costa Rica.", 0)
	factB := insertFactWithSource(t, env, pgRepo, srcID, "Banana cultivation fails above 1067m elevation consistently.", 0)

	embProvider := &stubEmbeddingProvider{dim: dim}
	embCfg := config.EmbeddingConfig{Provider: "stub", Model: "stub-embedding", Dimensions: dim}
	// threshold=1.01 rejects every Qdrant hit; maxFacts=1 so the
	// embedding pool holds at most 1. The two direct citations
	// persist as extras (outside the cap) so we expect 2 annotations
	// total even though maxFacts=1.
	reportsCfg := config.ReportsConfig{
		Enabled:             true,
		SimilarityThreshold: 1.01,
		MaxFactsPerSentence: 1,
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

	annotateWorker := tasks.NewAnnotateReportWorker(embProvider, embCfg, reportsCfg, posture.Classifier(nil), nil, qStore, registry, systemQueries, nil, nil)
	annotateWorkers := river.NewWorkers()
	river.AddWorker(annotateWorkers, annotateWorker)
	annotateCfg := &river.Config{Workers: annotateWorkers,
		Queues: map[string]river.QueueConfig{tasks.QueueAnnotateReport: {MaxWorkers: 1}}}
	testAnnotate := rivertest.NewWorker(t, driver, annotateCfg, annotateWorker)

	reportID := pgtype.UUID{}
	if err := reportID.Scan(uuid.NewString()); err != nil {
		t.Fatalf("scan report id: %v", err)
	}
	// Body with two inline citations in a single sentence. The
	// sentence_index 0 (the only candidate) should get both facts as
	// direct-citation annotations, even though maxFactsPerSentence=1.
	body := "Coffee grows well at 1800m in Costa Rica highlands, supported by [costa rica elevation](<fact:" + factA + ">) and [banana limit](<fact:" + factB + ">)."
	if _, err := queries.CreateReport(ctx, store.CreateReportParams{
		ID:           reportID,
		RepositoryID: pgRepo,
		Title:        "Direct citation test",
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
	if len(anns) != 2 {
		t.Fatalf("expected 2 direct-citation annotations (outside the maxFacts=1 cap), got %d", len(anns))
	}
	seen := map[string]bool{}
	for _, a := range anns {
		if a.Posture == nil || *a.Posture != "supports" {
			t.Errorf("direct citation posture = %v, want %q", a.Posture, "supports")
		}
		if a.Score != 1.0 {
			t.Errorf("direct citation score = %v, want 1.0", a.Score)
		}
		seen[a.FactID.String()] = true
	}
	if !seen[factA] {
		t.Errorf("direct citation fact A (%s) not persisted", factA)
	}
	if !seen[factB] {
		t.Errorf("direct citation fact B (%s) not persisted", factB)
	}
}