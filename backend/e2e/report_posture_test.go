//go:build e2e

package e2e_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
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
//
// The stub also records the context_before / context_after slices
// the worker threaded into each batch entry ( keyed by
// sentence_index) so a test can assert the context-window wiring
// without a real LLM. The last call wins for each sentence_index.
type stubPostureAIProvider struct {
	// postures maps "<sentence_index>:<fact_id_prefix>" to a
	// posture string the stub emits in its JSON response. The
	// fact_id_prefix is the first 8 chars of the UUID so the test
	// setup reads cleanly. Missing pairs default to "irrelevant".
	postures map[string]string
	// capturedContext records the context_before / context_after
	// slices the worker sent for each sentence_index, keyed by
	// sentence_index. The mutex guards the map because the worker
	// runs batches concurrently.
	capturedContext map[int]stubCapturedContext
	mu              sync.Mutex
}

// stubCapturedContext is the per-sentence context the stub records.
type stubCapturedContext struct {
	ContextBefore []string
	ContextAfter  []string
}

func (p *stubPostureAIProvider) Chat(ctx context.Context, db store.DBTX, req ai.ChatRequest) (ai.ChatResponse, error) {
	// Parse the user message (the batch JSON) and emit a
	// classification row for every input pair. Capture the
	// context_before / context_after slices so the test can assert
	// the context-window wiring.
	var batches []struct {
		SentenceIndex int      `json:"sentence_index"`
		ContextBefore []string `json:"context_before"`
		ContextAfter  []string `json:"context_after"`
		Candidates    []struct {
			FactID   string `json:"fact_id"`
			FactText string `json:"fact_text"`
		} `json:"candidates"`
	}
	if err := json.Unmarshal([]byte(extractLastJSON(req.Messages[len(req.Messages)-1].Content)), &batches); err != nil {
		return ai.ChatResponse{}, fmt.Errorf("stubPosture: parse batch: %w", err)
	}
	p.mu.Lock()
	if p.capturedContext == nil {
		p.capturedContext = make(map[int]stubCapturedContext)
	}
	for _, b := range batches {
		cap := stubCapturedContext{}
		if len(b.ContextBefore) > 0 {
			cap.ContextBefore = append([]string(nil), b.ContextBefore...)
		}
		if len(b.ContextAfter) > 0 {
			cap.ContextAfter = append([]string(nil), b.ContextAfter...)
		}
		p.capturedContext[b.SentenceIndex] = cap
	}
	p.mu.Unlock()

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

// capturedContextFor returns the context the stub recorded for the
// given sentence_index. Returns zero value when the index was never
// seen (defensive — the worker never called the classifier for that
// sentence, e.g. no Qdrant hits).
func (p *stubPostureAIProvider) capturedContextFor(sentenceIndex int) stubCapturedContext {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.capturedContext[sentenceIndex]
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
	annotateWorker := tasks.NewAnnotateReportWorker(embProvider, embCfg, reportsCfg, postureClassifier, nil, qStore, registry, systemQueries, nil, nil)
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
	// worker resolves SimilarityThresholdOr(0.84) when the config
	// value is 0.0 (the zero value), so the persisted threshold is
	// 0.84, not 0.0.
	rep, err := queries.GetReportByID(ctx, reportID)
	if err != nil {
		t.Fatalf("get report: %v", err)
	}
	if rep.Status != "annotated" {
		t.Errorf("report status = %q, want %q", rep.Status, "annotated")
	}
	if rep.SimilarityThreshold == nil || *rep.SimilarityThreshold != 0.84 {
		t.Errorf("report threshold = %v, want 0.84 (SimilarityThresholdOr default)", rep.SimilarityThreshold)
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
	annotateWorker := tasks.NewAnnotateReportWorker(embProvider, embCfg, reportsCfg, nil, nil, qStore, registry, systemQueries, nil, nil)
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
	if got["max_facts_per_sentence"] != nil {
		t.Errorf("initial max_facts_per_sentence = %v, want nil", got["max_facts_per_sentence"])
	}
	if got["lexical_similarity_floor"] != nil {
		t.Errorf("initial lexical_similarity_floor = %v, want nil", got["lexical_similarity_floor"])
	}

	// 2. PUT an override: threshold 0.90, posture off, max_facts 12,
	//    lexical floor 0.65.
	putBody, _ := json.Marshal(map[string]any{
		"similarity_threshold":       0.90,
		"posture_classifier_enabled": false,
		"max_facts_per_sentence":     12,
		"lexical_similarity_floor":   0.65,
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
	if put["max_facts_per_sentence"] != float64(12) {
		t.Errorf("PUT max_facts_per_sentence = %v, want 12", put["max_facts_per_sentence"])
	}
	if put["lexical_similarity_floor"] != 0.65 {
		t.Errorf("PUT lexical_similarity_floor = %v, want 0.65", put["lexical_similarity_floor"])
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
	if got2["max_facts_per_sentence"] != float64(12) {
		t.Errorf("override max_facts_per_sentence = %v, want 12", got2["max_facts_per_sentence"])
	}
	if got2["lexical_similarity_floor"] != 0.65 {
		t.Errorf("override lexical_similarity_floor = %v, want 0.65", got2["lexical_similarity_floor"])
	}

	// 4. Error case: invalid threshold (>1) is rejected.
	badBody, _ := json.Marshal(map[string]any{"similarity_threshold": 1.5})
	badResp, _ := admin.do("PUT", "/api/v1/repositories/"+repoID+"/settings/reports", badBody)
	if badResp.StatusCode != 400 {
		t.Errorf("invalid threshold: status = %d, want 400", badResp.StatusCode)
	}

	// 5. Error case: invalid max_facts_per_sentence (>50) is rejected.
	badMaxBody, _ := json.Marshal(map[string]any{"max_facts_per_sentence": 51})
	badMaxResp, _ := admin.do("PUT", "/api/v1/repositories/"+repoID+"/settings/reports", badMaxBody)
	if badMaxResp.StatusCode != 400 {
		t.Errorf("invalid max_facts_per_sentence: status = %d, want 400", badMaxResp.StatusCode)
	}

	// 6. Error case: invalid lexical_similarity_floor (>1) is rejected.
	badFloorBody, _ := json.Marshal(map[string]any{"lexical_similarity_floor": 1.5})
	badFloorResp, _ := admin.do("PUT", "/api/v1/repositories/"+repoID+"/settings/reports", badFloorBody)
	if badFloorResp.StatusCode != 400 {
		t.Errorf("invalid lexical_similarity_floor: status = %d, want 400", badFloorResp.StatusCode)
	}

	// 6a. Error case: invalid context_window_before (>10) rejected.
	badCtxBeforeBody, _ := json.Marshal(map[string]any{"context_window_before": 11})
	badCtxBeforeResp, _ := admin.do("PUT", "/api/v1/repositories/"+repoID+"/settings/reports", badCtxBeforeBody)
	if badCtxBeforeResp.StatusCode != 400 {
		t.Errorf("invalid context_window_before: status = %d, want 400", badCtxBeforeResp.StatusCode)
	}

	// 6b. Error case: invalid context_window_after (>10) rejected.
	badCtxAfterBody, _ := json.Marshal(map[string]any{"context_window_after": 11})
	badCtxAfterResp, _ := admin.do("PUT", "/api/v1/repositories/"+repoID+"/settings/reports", badCtxAfterBody)
	if badCtxAfterResp.StatusCode != 400 {
		t.Errorf("invalid context_window_after: status = %d, want 400", badCtxAfterResp.StatusCode)
	}

	// 6c. Error case: negative context_window_before rejected.
	negCtxBeforeBody, _ := json.Marshal(map[string]any{"context_window_before": -1})
	negCtxBeforeResp, _ := admin.do("PUT", "/api/v1/repositories/"+repoID+"/settings/reports", negCtxBeforeBody)
	if negCtxBeforeResp.StatusCode != 400 {
		t.Errorf("negative context_window_before: status = %d, want 400", negCtxBeforeResp.StatusCode)
	}

	// 6d. Happy case: context_window_before=1 / context_window_after=3
	//     upserts and round-trips; the response carries the global
	//     default_* values for the unset sides.
	ctxBody, _ := json.Marshal(map[string]any{
		"context_window_before": 1,
		"context_window_after":  3,
	})
	ctxResp, ctxBodyResp := admin.do("PUT", "/api/v1/repositories/"+repoID+"/settings/reports", ctxBody)
	if ctxResp.StatusCode != 200 {
		t.Fatalf("PUT context_window: %d %s", ctxResp.StatusCode, ctxBodyResp)
	}
	var ctxRow map[string]any
	if err := json.Unmarshal(ctxBodyResp, &ctxRow); err != nil {
		t.Fatalf("decode ctx PUT: %v", err)
	}
	if ctxRow["context_window_before"] != float64(1) {
		t.Errorf("PUT context_window_before = %v, want 1", ctxRow["context_window_before"])
	}
	if ctxRow["context_window_after"] != float64(3) {
		t.Errorf("PUT context_window_after = %v, want 3", ctxRow["context_window_after"])
	}
	if ctxRow["default_context_window_before"] == nil {
		t.Errorf("PUT response missing default_context_window_before")
	}
	if ctxRow["default_context_window_after"] == nil {
		t.Errorf("PUT response missing default_context_window_after")
	}

	// 6e. GET reflects the override.
	getCtxResp, getCtxBody := admin.do("GET", "/api/v1/repositories/"+repoID+"/settings/reports", nil)
	if getCtxResp.StatusCode != 200 {
		t.Fatalf("GET after ctx PUT: %d %s", getCtxResp.StatusCode, getCtxBody)
	}
	var getCtxRow map[string]any
	if err := json.Unmarshal(getCtxBody, &getCtxRow); err != nil {
		t.Fatalf("decode GET after ctx: %v", err)
	}
	if getCtxRow["context_window_before"] != float64(1) {
		t.Errorf("GET context_window_before = %v, want 1", getCtxRow["context_window_before"])
	}
	if getCtxRow["context_window_after"] != float64(3) {
		t.Errorf("GET context_window_after = %v, want 3", getCtxRow["context_window_after"])
	}

	// 7. Clear max_facts + lexical_floor + context_window overrides
	//    by sending null while keeping threshold. Confirms
	//    partial-null upsert works (the four other fields remain
	//    null-inherit while threshold stays 0.90).
	clearBody, _ := json.Marshal(map[string]any{
		"similarity_threshold":     0.90,
		"max_facts_per_sentence":    nil,
		"lexical_similarity_floor":  nil,
		"context_window_before":     nil,
		"context_window_after":      nil,
	})
	clearResp, clearBodyResp := admin.do("PUT", "/api/v1/repositories/"+repoID+"/settings/reports", clearBody)
	if clearResp.StatusCode != 200 {
		t.Fatalf("clear overrides PUT: %d %s", clearResp.StatusCode, clearBodyResp)
	}
	var cleared map[string]any
	if err := json.Unmarshal(clearBodyResp, &cleared); err != nil {
		t.Fatalf("decode cleared: %v", err)
	}
	if cleared["max_facts_per_sentence"] != nil {
		t.Errorf("cleared max_facts_per_sentence = %v, want nil", cleared["max_facts_per_sentence"])
	}
	if cleared["lexical_similarity_floor"] != nil {
		t.Errorf("cleared lexical_similarity_floor = %v, want nil", cleared["lexical_similarity_floor"])
	}
	if cleared["context_window_before"] != nil {
		t.Errorf("cleared context_window_before = %v, want nil", cleared["context_window_before"])
	}
	if cleared["context_window_after"] != nil {
		t.Errorf("cleared context_window_after = %v, want nil", cleared["context_window_after"])
	}
	if cleared["similarity_threshold"] != 0.90 {
		t.Errorf("cleared threshold = %v, want 0.90 (untouched)", cleared["similarity_threshold"])
	}
}

// TestAnnotateReportContextWindow asserts the annotate_report worker
// threads context_before / context_after sentences into the posture
// classifier prompt. It seeds a 5-sentence report, runs the worker
// with the stub posture classifier (which records the context slices
// it received for each sentence_index), and asserts:
//   - the global config defaults (ContextWindowBefore=2,
//     ContextWindowAfter=2) yield 2 sentences before and 2 after for
//     a middle sentence;
//   - the first sentence yields 0 context_before and 2 context_after
//     (clamped to the available range, no synthesized padding);
//   - the last sentence yields 2 context_before and 0 context_after;
//   - a per-repo override (context_window_before=1,
//     context_window_after=0) shrinks the window on the before side
//     and disables the after side entirely.
//
// Skips when QDRANT_HOST is unset.
func TestAnnotateReportContextWindow(t *testing.T) {
	const dim = 8
	qStore, qCleanup := qdrantTestStore(t, dim)
	defer qCleanup()

	env := testutil.NewTestEnv(t)
	defer env.Server.Close()
	ensureRiverSchema(t, env.DB)

	admin := bootstrapSysAdmin(t, env, "ctxwin@example.com")
	_, _, repoID := createRepositoryWithDB(t, admin, "Context Window Repo", "ctx-win-repo", "desc", "")
	queries := store.New(env.DB)
	pgRepo := pgRepoID(t, repoID)

	srcID := pgtype.UUID{}
	if err := srcID.Scan(uuid.NewString()); err != nil {
		t.Fatalf("scan source id: %v", err)
	}
	if _, err := queries.CreateSource(context.Background(), store.CreateSourceParams{
		ID: srcID, RepositoryID: pgRepo, Url: "https://example.com/ctx", Kind: "homepage", Status: "fetched",
	}); err != nil {
		t.Fatalf("create source: %v", err)
	}

	// One fact the stub classifier will label "supports" for every
	// sentence_index so the classifier runs for every candidate.
	factID := insertFactWithSource(t, env, pgRepo, srcID, "Coffee grows well at 1800m in Costa Rica.", 0)

	// Build the embedding + posture workers. The reportsCfg sets
	// context_window_before=2 / context_window_after=2 (the global
	// default); a later subtest upserts a per-repo override.
	embProvider := &stubEmbeddingProvider{dim: dim}
	embCfg := config.EmbeddingConfig{Provider: "stub", Model: "stub-embedding", Dimensions: dim}
	reportsCfg := config.ReportsConfig{
		Enabled:             true,
		SimilarityThreshold: 0.0, // accept every Qdrant hit
		MaxFactsPerSentence: 5,
		MinSentenceRunes:    10,
		PostureClassifier: config.PostureClassifierConfig{
			Enabled:             true,
			Provider:            "stub",
			Model:               "stub-posture",
			BatchSize:           8,
			MaxConcurrent:       2,
			MaxTokens:           800,
			ContextWindowBefore: 2,
			ContextWindowAfter:  2,
		},
	}
	stub := &stubPostureAIProvider{postures: map[string]string{
		fmt.Sprintf("0:%s", factID[:8]): "supports",
		fmt.Sprintf("1:%s", factID[:8]): "supports",
		fmt.Sprintf("2:%s", factID[:8]): "supports",
		fmt.Sprintf("3:%s", factID[:8]): "supports",
		fmt.Sprintf("4:%s", factID[:8]): "supports",
	}}
	postureClassifier := posture.NewAIClassifier(stub, "stub-posture")

	registry := testutil.NewForTestPool(env.DB)
	systemQueries := store.New(env.DB)

	// Embed the fact so Qdrant has a vector to surface for every
	// sentence (the stub embedding makes every sentence match the
	// fact closely).
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

	// 5-sentence report. The candidate sentence that triggers a
	// classifier call is the only one carrying "Coffee" so its
	// stub embedding surfaces the fact as a Qdrant hit. We mark
	// every sentence with "Coffee" so all 5 are candidates and we
	// can assert the context window at every position.
	reportID := pgtype.UUID{}
	if err := reportID.Scan(uuid.NewString()); err != nil {
		t.Fatalf("scan report id: %v", err)
	}
	body := strings.Join([]string{
		"Coffee is a popular beverage brewed from roasted beans.",
		"Coffee grows well at 1800m in Costa Rica highlands.",
		"Coffee contains caffeine which is a mild stimulant.",
		"Coffee production exceeds 10 million metric tons yearly.",
		"Coffee consumption has been linked to several health effects.",
	}, "\n\n")
	if _, err := queries.CreateReport(ctx, store.CreateReportParams{
		ID:           reportID,
		RepositoryID: pgRepo,
		Title:        "Context window test",
		BodyMd:       body,
		Status:       "pending",
	}); err != nil {
		t.Fatalf("create report: %v", err)
	}

	runAnnotate := func(t *testing.T, wantCtxBefore, wantCtxAfter map[int][]string) {
		t.Helper()
		stub.mu.Lock()
		stub.capturedContext = nil
		stub.mu.Unlock()

		annotateWorker := tasks.NewAnnotateReportWorker(embProvider, embCfg, reportsCfg, postureClassifier, nil, qStore, registry, systemQueries, nil, nil)
		annotateWorkers := river.NewWorkers()
		river.AddWorker(annotateWorkers, annotateWorker)
		annotateCfg := &river.Config{Workers: annotateWorkers,
			Queues: map[string]river.QueueConfig{tasks.QueueAnnotateReport: {MaxWorkers: 1}}}
		testAnnotate := rivertest.NewWorker(t, driver, annotateCfg, annotateWorker)

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

		for idx, want := range wantCtxBefore {
			got := stub.capturedContextFor(idx)
			if !equalStrings(got.ContextBefore, want) {
				t.Errorf("sentence %d context_before = %v, want %v", idx, got.ContextBefore, want)
			}
		}
		for idx, want := range wantCtxAfter {
			got := stub.capturedContextFor(idx)
			if !equalStrings(got.ContextAfter, want) {
				t.Errorf("sentence %d context_after = %v, want %v", idx, got.ContextAfter, want)
			}
		}
	}

	// Subtest 1: global defaults (2/2). Sentence 0 yields 0 before
	// (clamped to the report boundary) and 2 after; sentence 4
	// yields 2 before and 0 after; sentence 2 (middle) yields 2/2.
	t.Run("global_defaults_2_2", func(t *testing.T) {
		runAnnotate(t,
			map[int][]string{
				0: nil,
				1: {"Coffee is a popular beverage brewed from roasted beans."},
				2: {
					"Coffee is a popular beverage brewed from roasted beans.",
					"Coffee grows well at 1800m in Costa Rica highlands.",
				},
				4: {
					"Coffee contains caffeine which is a mild stimulant.",
					"Coffee production exceeds 10 million metric tons yearly.",
				},
			},
			map[int][]string{
				0: {
					"Coffee grows well at 1800m in Costa Rica highlands.",
					"Coffee contains caffeine which is a mild stimulant.",
				},
				2: {
					"Coffee production exceeds 10 million metric tons yearly.",
					"Coffee consumption has been linked to several health effects.",
				},
				4: nil,
			},
		)
	})

	// Subtest 2: per-repo override context_window_before=1 /
	// context_window_after=0. Sentence 2 yields 1 before and 0 after.
	t.Run("per_repo_override_1_0", func(t *testing.T) {
		// Upsert the per-repo override before the worker runs so
		// GetRepositoryReportSettings returns it.
		one := int32(1)
		zero := int32(0)
		if _, err := systemQueries.UpsertRepositoryReportSettings(ctx, store.UpsertRepositoryReportSettingsParams{
			RepositoryID:            pgRepo,
			PostureClassifierEnabled: true,
			ContextWindowBefore:     &one,
			ContextWindowAfter:      &zero,
		}); err != nil {
			t.Fatalf("upsert report settings: %v", err)
		}
		defer func() {
			_ = systemQueries.DeleteRepositoryReportSettings(ctx, pgRepo)
		}()

		runAnnotate(t,
			map[int][]string{
				2: {"Coffee grows well at 1800m in Costa Rica highlands."},
				0: nil,
			},
			map[int][]string{
				2: nil,
				0: nil,
			},
		)
	})
}

// equalStrings compares two string slices for value equality, treating
// a nil slice and an empty slice as equal (both "no context").
func equalStrings(a, b []string) bool {
	if len(a) == 0 && len(b) == 0 {
		return true
	}
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// _ guards against unused imports when the env-gated Qdrant tests
// are skipped (the os import is only used by qdrantTestStore).
var _ = os.Getenv