//go:build e2e

package e2e_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/openktree/open-knowledge-tree/backend/e2e/testutil"
	"github.com/openktree/open-knowledge-tree/backend/internal/config"
	"github.com/openktree/open-knowledge-tree/backend/internal/providers/ai"
	"github.com/openktree/open-knowledge-tree/backend/internal/providers/posture"
	"github.com/openktree/open-knowledge-tree/backend/internal/store"
	"github.com/openktree/open-knowledge-tree/backend/internal/taskmanager/tasks"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivertest"
)

// stubPostureAIProvider is a test double for ai.AIProvider whose
// Chat returns a canned posture-classification JSON array built
// from the input batch. It lets the annotate_report worker exercise
// the posture classifier path without hitting a real LLM. The
// response posture for each (sentence_index, fact_id) pair is
// driven by a map the test sets up upfront so the test controls
// which pairs are supports/contradicts/related/irrelevant.
type stubPostureAIProvider struct {
	// postures maps "<sentence_index>:<fact_id_prefix>" to a
	// posture string the stub emits in its JSON response. The
	// fact_id_prefix is the first 8 chars of the UUID so the test
	// setup reads cleanly. Missing pairs default to "irrelevant".
	postures map[string]string
}

func (p *stubPostureAIProvider) Chat(ctx context.Context, db store.DBTX, req ai.ChatRequest) (ai.ChatResponse, error) {
	// Parse the user message (the batch JSON) and emit a
	// classification row for every input pair.
	var batches []struct {
		SentenceIndex int `json:"sentence_index"`
		Candidates    []struct {
			FactID   string `json:"fact_id"`
			FactText string `json:"fact_text"`
		} `json:"candidates"`
	}
	if err := json.Unmarshal([]byte(extractLastJSON(req.Messages[len(req.Messages)-1].Content)), &batches); err != nil {
		return ai.ChatResponse{}, fmt.Errorf("stubPosture: parse batch: %w", err)
	}
	out := make([]map[string]any, 0, len(batches)*4)
	for _, b := range batches {
		for _, c := range b.Candidates {
			key := fmt.Sprintf("%d:%s", b.SentenceIndex, c.FactID[:8])
			post := p.postures[key]
			if post == "" {
				post = "irrelevant"
			}
			out = append(out, map[string]any{
				"sentence_index": b.SentenceIndex,
				"fact_id":         c.FactID,
				"posture":         post,
			})
		}
	}
	b, _ := json.Marshal(out)
	return ai.ChatResponse{
		Model:    "stub-posture",
		Messages: []ai.ChatMessage{{Role: "assistant", Content: string(b)}},
	}, nil
}

func (p *stubPostureAIProvider) Describe() ai.ProviderDescription {
	return ai.ProviderDescription{Name: "stub-posture", Configured: true}
}

// extractLastJSON pulls the last JSON array out of a message that
// may have a leading prose line (the buildUserMessage format is
// "Classify ...\n\n<json>"). It finds the first '[' and returns
// from there to the matching end. Good enough for the test stub.
func extractLastJSON(s string) string {
	idx := strings.Index(s, "[")
	if idx < 0 {
		return "[]"
	}
	return s[idx:]
}

// TestAnnotateReportPostureClassifier exercises the posture
// classifier branch of the annotate_report worker. It seeds a repo
// with three facts (one supports, one contradicts, one irrelevant
// by the stub's mapping), embeds them, runs the worker with the
// stub classifier, and asserts:
//   - the supports + contradicts pairs are persisted with the
//     correct posture label;
//   - the irrelevant pair is dropped (no annotation row);
//   - the report status is 'annotated' and the result records 1
//     dropped pair.
//
// Skips when QDRANT_HOST is unset (the test profile does not bring
// up Qdrant). Mirrors the env-gated skip pattern in
// embed_dedup_test.go.
func TestAnnotateReportPostureClassifier(t *testing.T) {
	const dim = 8
	qStore, qCleanup := qdrantTestStore(t, dim)
	defer qCleanup()

	env := testutil.NewTestEnv(t)
	defer env.Server.Close()
	ensureRiverSchema(t, env.DB)

	admin := bootstrapSysAdmin(t, env, "posture@example.com")
	_, _, repoID := createRepositoryWithDB(t, admin, "Posture Repo", "posture-repo", "desc", "")
	queries := store.New(env.DB)
	pgRepo := pgRepoID(t, repoID)

	// Seed a source so insertFactWithSource can link facts to it.
	srcID := pgtype.UUID{}
	if err := srcID.Scan(uuid.NewString()); err != nil {
		t.Fatalf("scan source id: %v", err)
	}
	if _, err := queries.CreateSource(context.Background(), store.CreateSourceParams{
		ID: srcID, RepositoryID: pgRepo, Url: "https://example.com/posture", Kind: "homepage", Status: "fetched",
	}); err != nil {
		t.Fatalf("create source: %v", err)
	}

	// Three facts: the stub classifier will label them supports /
	// contradicts / irrelevant. We capture the first 8 chars of
	// each UUID to key the stub's posture map.
	supportsID := insertFactWithSource(t, env, pgRepo, srcID, "Coffee grows well at 1800m in Costa Rica.", 0)
	contradictsID := insertFactWithSource(t, env, pgRepo, srcID, "Banana cultivation fails above 1067m elevation consistently.", 0)
	irrelevantID := insertFactWithSource(t, env, pgRepo, srcID, "The Eiffel Tower is located in Paris, France.", 0)

	stubPostures := map[string]string{
		fmt.Sprintf("0:%s", supportsID[:8]):     "supports",
		fmt.Sprintf("0:%s", contradictsID[:8]): "contradicts",
		fmt.Sprintf("0:%s", irrelevantID[:8]):   "irrelevant",
	}

	// Build the embedding + posture workers the same way
	// taskmanager.New does. The stub embedding produces
	// deterministic vectors from the input bytes; we plant a
	// shared marker ("Coffee") in the supports fact + the report
	// sentence so the stub embedding surfaces it as a Qdrant hit.
	embProvider := &stubEmbeddingProvider{dim: dim}
	embCfg := config.EmbeddingConfig{Provider: "stub", Model: "stub-embedding", Dimensions: dim}
	reportsCfg := config.ReportsConfig{
		Enabled:             true,
		SimilarityThreshold: 0.0, // accept every Qdrant hit; the classifier is the gate
		MaxFactsPerSentence: 5,
		MinSentenceRunes:    10,
		PostureClassifier: config.PostureClassifierConfig{
			Enabled:       true,
			Provider:      "stub",
			Model:         "stub-posture",
			BatchSize:     8,
			MaxConcurrent: 2,
			MaxTokens:     800,
		},
	}
	postureClassifier := posture.NewAIClassifier(&stubPostureAIProvider{postures: stubPostures}, "stub-posture")

	registry := testutil.NewForTestPool(env.DB)
	systemQueries := store.New(env.DB)

	// Embed the three facts so Qdrant has vectors to search.
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

	// Build the annotate_report worker with the posture classifier.
	annotateWorker := tasks.NewAnnotateReportWorker(embProvider, embCfg, reportsCfg, postureClassifier, qStore, registry, systemQueries, nil)
	annotateWorkers := river.NewWorkers()
	river.AddWorker(annotateWorkers, annotateWorker)
	annotateCfg := &river.Config{Workers: annotateWorkers,
		Queues: map[string]river.QueueConfig{tasks.QueueAnnotateReport: {MaxWorkers: 1}}}
	testAnnotate := rivertest.NewWorker(t, driver, annotateCfg, annotateWorker)

	// Create a report whose single sentence is close (in stub
	// embedding space) to all three facts. The stub embedding is
	// deterministic from the input bytes; a sentence containing
	// "Coffee" matches the supports fact closely. The threshold is
	// 0.0 so every fact above 0 similarity is a candidate — the
	// classifier is the gate.
	reportID := pgtype.UUID{}
	if err := reportID.Scan(uuid.NewString()); err != nil {
		t.Fatalf("scan report id: %v", err)
	}
	body := "Coffee grows well at 1800m in Costa Rica highlands."
	if _, err := queries.CreateReport(ctx, store.CreateReportParams{
		ID:           reportID,
		RepositoryID: pgRepo,
		Title:        "Posture test",
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

	// Read the persisted annotations and assert postures + the
	// dropped irrelevant pair.
	anns, err := queries.ListReportAnnotationsByReport(ctx, reportID)
	if err != nil {
		t.Fatalf("list annotations: %v", err)
	}
	if len(anns) != 2 {
		t.Fatalf("expected 2 annotations (supports + contradicts), got %d", len(anns))
	}
	seen := map[string]string{}
	for _, a := range anns {
		if a.Posture == nil {
			t.Errorf("annotation for fact %s has nil posture", a.FactID)
			continue
		}
		seen[a.FactID.String()] = *a.Posture
	}
	if seen[supportsID] != "supports" {
		t.Errorf("supports fact posture = %q, want %q", seen[supportsID], "supports")
	}
	if seen[contradictsID] != "contradicts" {
		t.Errorf("contradicts fact posture = %q, want %q", seen[contradictsID], "contradicts")
	}
	if _, ok := seen[irrelevantID]; ok {
		t.Errorf("irrelevant fact was not dropped: present in annotations")
	}

	// The report should be annotated + carry the threshold. The
	// worker resolves SimilarityThresholdOr(0.8) so a config value
	// of 0.0 falls back to the 0.8 default.
	rep, err := queries.GetReportByID(ctx, reportID)
	if err != nil {
		t.Fatalf("get report: %v", err)
	}
	if rep.Status != "annotated" {
		t.Errorf("report status = %q, want %q", rep.Status, "annotated")
	}
	if rep.SimilarityThreshold == nil || *rep.SimilarityThreshold != 0.8 {
		t.Errorf("report threshold = %v, want 0.8", rep.SimilarityThreshold)
	}
}

// TestAnnotateReportPostureFallback covers the keep-all fallback:
// when no posture classifier is wired (nil), the worker persists
// every Qdrant hit with posture = NULL. Mirrors the classifier test
// but with postureClassifier = nil.
func TestAnnotateReportPostureFallback(t *testing.T) {
	const dim = 8
	qStore, qCleanup := qdrantTestStore(t, dim)
	defer qCleanup()

	env := testutil.NewTestEnv(t)
	defer env.Server.Close()
	ensureRiverSchema(t, env.DB)

	admin := bootstrapSysAdmin(t, env, "posture_fb@example.com")
	_, _, repoID := createRepositoryWithDB(t, admin, "Posture Fallback", "posture-fb", "desc", "")
	queries := store.New(env.DB)
	pgRepo := pgRepoID(t, repoID)

	srcID := pgtype.UUID{}
	if err := srcID.Scan(uuid.NewString()); err != nil {
		t.Fatalf("scan source id: %v", err)
	}
	if _, err := queries.CreateSource(context.Background(), store.CreateSourceParams{
		ID: srcID, RepositoryID: pgRepo, Url: "https://example.com/fb", Kind: "homepage", Status: "fetched",
	}); err != nil {
		t.Fatalf("create source: %v", err)
	}
	factA := insertFactWithSource(t, env, pgRepo, srcID, "Coffee grows well at 1800m in Costa Rica.", 0)
	factB := insertFactWithSource(t, env, pgRepo, srcID, "The Eiffel Tower is in Paris.", 0)

	embProvider := &stubEmbeddingProvider{dim: dim}
	embCfg := config.EmbeddingConfig{Provider: "stub", Model: "stub-embedding", Dimensions: dim}
	reportsCfg := config.ReportsConfig{
		Enabled:             true,
		SimilarityThreshold: 0.0,
		MaxFactsPerSentence: 5,
		MinSentenceRunes:    10,
		PostureClassifier:   config.PostureClassifierConfig{Enabled: false},
	}
	registry := testutil.NewForTestPool(env.DB)
	systemQueries := store.New(env.DB)

	// Embed both facts.
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

	// annotate_report with NO classifier — falls back to keep-all.
	annotateWorker := tasks.NewAnnotateReportWorker(embProvider, embCfg, reportsCfg, nil, qStore, registry, systemQueries, nil)
	annotateWorkers := river.NewWorkers()
	river.AddWorker(annotateWorkers, annotateWorker)
	annotateCfg := &river.Config{Workers: annotateWorkers,
		Queues: map[string]river.QueueConfig{tasks.QueueAnnotateReport: {MaxWorkers: 1}}}
	testAnnotate := rivertest.NewWorker(t, driver, annotateCfg, annotateWorker)

	reportID := pgtype.UUID{}
	if err := reportID.Scan(uuid.NewString()); err != nil {
		t.Fatalf("scan report id: %v", err)
	}
	body := "Coffee grows well at 1800m in Costa Rica highlands."
	if _, err := queries.CreateReport(ctx, store.CreateReportParams{
		ID:           reportID,
		RepositoryID: pgRepo,
		Title:        "Posture fallback test",
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
	if len(anns) == 0 {
		t.Fatalf("fallback: expected at least one keep-all annotation, got 0")
	}
	for _, a := range anns {
		if a.Posture != nil {
			t.Errorf("fallback annotation for fact %s has posture %q, want nil", a.FactID, *a.Posture)
		}
	}
	_ = factA
	_ = factB
}

// TestReportSettingsEndpoint covers the per-repo report settings
// HTTP endpoints (GET/PUT /settings/reports) that the posture
// classifier's per-repo threshold + enable flag depend on. Does
// not need Qdrant — it only exercises the settings CRUD.
func TestReportSettingsEndpoint(t *testing.T) {
	env := testutil.NewTestEnv(t)
	defer env.Server.Close()

	admin := bootstrapSysAdmin(t, env, "rep_settings@example.com")
	_, _, repoID := createRepositoryWithDB(t, admin, "Rep Settings", "rep-settings", "desc", "")

	// 1. GET before any override — threshold null, posture_enabled
	//    inherits the global default (true in config.default.yaml).
	getResp, getBody := admin.do("GET", "/api/v1/repositories/"+repoID+"/settings/reports", nil)
	if getResp.StatusCode != 200 {
		t.Fatalf("GET settings/reports: %d %s", getResp.StatusCode, getBody)
	}
	var got map[string]any
	if err := json.Unmarshal(getBody, &got); err != nil {
		t.Fatalf("decode GET: %v", err)
	}
	if got["similarity_threshold"] != nil {
		t.Errorf("initial threshold = %v, want nil", got["similarity_threshold"])
	}
	if got["posture_classifier_enabled"] != false {
		t.Errorf("initial posture_classifier_enabled = %v, want false (test config inherits zero value)", got["posture_classifier_enabled"])
	}

	// 2. PUT an override: threshold 0.90, posture off.
	putBody, _ := json.Marshal(map[string]any{
		"similarity_threshold":       0.90,
		"posture_classifier_enabled": false,
	})
	putResp, putBodyResp := admin.do("PUT", "/api/v1/repositories/"+repoID+"/settings/reports", putBody)
	if putResp.StatusCode != 200 {
		t.Fatalf("PUT settings/reports: %d %s", putResp.StatusCode, putBodyResp)
	}
	var put map[string]any
	if err := json.Unmarshal(putBodyResp, &put); err != nil {
		t.Fatalf("decode PUT: %v", err)
	}
	if put["similarity_threshold"] != 0.90 {
		t.Errorf("PUT threshold = %v, want 0.90", put["similarity_threshold"])
	}
	if put["posture_classifier_enabled"] != false {
		t.Errorf("PUT posture_classifier_enabled = %v, want false", put["posture_classifier_enabled"])
	}

	// 3. GET again — reflects the override.
	get2Resp, get2Body := admin.do("GET", "/api/v1/repositories/"+repoID+"/settings/reports", nil)
	if get2Resp.StatusCode != 200 {
		t.Fatalf("GET2 settings/reports: %d %s", get2Resp.StatusCode, get2Body)
	}
	var got2 map[string]any
	if err := json.Unmarshal(get2Body, &got2); err != nil {
		t.Fatalf("decode GET2: %v", err)
	}
	if got2["similarity_threshold"] != 0.90 {
		t.Errorf("override threshold = %v, want 0.90", got2["similarity_threshold"])
	}
	if got2["posture_classifier_enabled"] != false {
		t.Errorf("override posture_classifier_enabled = %v, want false", got2["posture_classifier_enabled"])
	}

	// 4. Error case: invalid threshold (>1) is rejected.
	badBody, _ := json.Marshal(map[string]any{"similarity_threshold": 1.5})
	badResp, _ := admin.do("PUT", "/api/v1/repositories/"+repoID+"/settings/reports", badBody)
	if badResp.StatusCode != 400 {
		t.Errorf("invalid threshold: status = %d, want 400", badResp.StatusCode)
	}
}

// _ guards against unused imports when the env-gated Qdrant tests
// are skipped (the os import is only used by qdrantTestStore).
var _ = os.Getenv