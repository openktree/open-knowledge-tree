//go:build e2e

package e2e_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
	"github.com/openktree/open-knowledge-tree/backend/internal/providers/synthesis"
	"github.com/openktree/open-knowledge-tree/backend/internal/store"
	"github.com/openktree/open-knowledge-tree/backend/internal/taskmanager/tasks"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivertest"
)

// stubSynthesizer is a test double for synthesis.SynthesisProvider.
// It returns a fixed markdown body built from the concept name +
// slice count, embeds the first candidate image when one is present,
// and records every Synthesize / PickImages call so tests can assert
// on counts and inputs.
type stubSynthesizer struct {
	mu           sync.Mutex
	synthCalls   int
	pickerCalls  int
	pickerFail   bool
	pickedIDs    []string
	lastSynthReq synthesis.SynthesisRequest
	lastPickReq  synthesis.ImagePickRequest
}

func (s *stubSynthesizer) Summarize(_ context.Context, _ store.DBTX, req synthesis.SynthesisRequest) (string, error) {
	s.mu.Lock()
	s.synthCalls++
	s.lastSynthReq = req
	s.mu.Unlock()
	body := "# Definition of " + req.CanonicalName + "\n\nFolds " + itoa(len(req.Slices)) + " slice(s)."
	// Embed the first candidate image (if any) so tests can assert
	// the ![alt](<fact:fact_id>) syntax + embedded_image_ids
	// extraction.
	if len(req.CandidateImages) > 0 {
		body += "\n\n![illustration](<fact:" + req.CandidateImages[0].FactID + ">)"
	}
	// Cite the first slice's first fact id if the slice content
	// carries one — reusing a citation from the summaries. For the
	// stub we just embed a placeholder citation referencing the
	// first slice id so the normalize path has something to rewrite.
	if len(req.Slices) > 0 {
		body += "\n\nKey claim [see](" + req.Slices[0].ID + ")."
	}
	return body, nil
}

// Synthesize satisfies synthesis.SynthesisProvider (the interface
// declares Synthesize; the stub names the method Summarize for
// parity with stubSummarizer — alias it here).
func (s *stubSynthesizer) Synthesize(ctx context.Context, db store.DBTX, req synthesis.SynthesisRequest) (string, error) {
	return s.Summarize(ctx, db, req)
}

func (s *stubSynthesizer) PickImages(_ context.Context, _ store.DBTX, req synthesis.ImagePickRequest) ([]string, error) {
	s.mu.Lock()
	s.pickerCalls++
	s.lastPickReq = req
	s.mu.Unlock()
	if s.pickerFail {
		return nil, errSynthPickerFailed
	}
	if len(s.pickedIDs) > 0 {
		return s.pickedIDs, nil
	}
	// Default: return the first MaxImages candidates (or all when
	// fewer), so the picker "narrows" deterministically.
	max := req.MaxImages
	if max <= 0 || max > len(req.Candidates) {
		max = len(req.Candidates)
	}
	out := make([]string, 0, max)
	for i := 0; i < max; i++ {
		out = append(out, req.Candidates[i].FactID)
	}
	return out, nil
}

func (s *stubSynthesizer) Describe() synthesis.ProviderDescription {
	return synthesis.ProviderDescription{Name: "stub-synthesizer", Configured: true}
}

func (s *stubSynthesizer) synthCallCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.synthCalls
}

func (s *stubSynthesizer) pickerCallCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.pickerCalls
}

// errSynthPickerFailed is the sentinel the stub returns when
// pickerFail is set, so tests can assert the "skip images, still
// synthesize" path.
var errSynthPickerFailed = &synthPickerErr{"stub image picker forced failure"}

type synthPickerErr struct{ msg string }

func (e *synthPickerErr) Error() string { return e.msg }

// synthesizeTestEnv bundles the per-test wiring for the
// synthesize_concept worker.
type synthesizeTestEnv struct {
	env        *testutil.TestEnv
	worker     *tasks.SynthesizeConceptsWorker
	stub       *stubSynthesizer
	testWorker *rivertest.Worker[tasks.SynthesizeConceptArgs, pgx.Tx]
}

func newSynthesizeTestEnv(t *testing.T, cfg config.SynthesisConfig) *synthesizeTestEnv {
	t.Helper()
	env := testutil.NewTestEnv(t)
	ensureRiverSchema(t, env.DB)
	stub := &stubSynthesizer{}
	registry := testutil.NewForTestPool(env.DB)
	systemQueries := store.New(env.DB)
	worker := tasks.NewSynthesizeConceptsWorker(stub, cfg, registry, systemQueries, nil, nil)
	driver := riverpgxv5.New(env.DB)
	workers := river.NewWorkers()
	river.AddWorker(workers, worker)
	tw := rivertest.NewWorker(t, driver, &river.Config{
		Queues:  map[string]river.QueueConfig{tasks.QueueSynthesizeConcept: {MaxWorkers: 1}},
		Workers: workers,
	}, worker)
	return &synthesizeTestEnv{env: env, worker: worker, stub: stub, testWorker: tw}
}

// runSynthesizeJob wraps the rivertest.Work call for
// SynthesizeConceptArgs, returning the recorded result.
func runSynthesizeJob(t *testing.T, e *synthesizeTestEnv, args tasks.SynthesizeConceptArgs) tasks.SynthesizeConceptResult {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	tx, err := e.env.DB.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer tx.Rollback(context.Background())
	job, err := e.testWorker.Work(ctx, t, tx, args, &river.InsertOpts{Queue: tasks.QueueSynthesizeConcept})
	if err != nil {
		t.Fatalf("work: %v", err)
	}
	if job.EventKind != river.EventKindJobCompleted {
		tx.Rollback(context.Background())
		t.Fatalf("synthesize_concept: expected completed, got %s", job.EventKind)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
	var res tasks.SynthesizeConceptResult
	if raw := job.Job.Output(); len(raw) > 0 {
		if err := json.Unmarshal(raw, &res); err != nil {
			t.Fatalf("decode output: %v", err)
		}
	}
	return res
}

// seedSliceForConcept inserts one concept_summaries row for the given
// concept_id with a fixed sequence_num and content. Used to seed the
// inputs the synthesize_concept worker folds, without running the
// summarize_concepts worker.
func seedSliceForConcept(t *testing.T, env *testutil.TestEnv, conceptID, repoID pgtype.UUID, contextLabel string, seq int32, factCount int32, content string) pgtype.UUID {
	t.Helper()
	ctx := context.Background()
	queries := store.New(env.DB)
	model := "stub-summarizer"
	row, err := queries.CreateSummary(ctx, store.CreateSummaryParams{
		ConceptID:      conceptID,
		RepositoryID:   repoID,
		Context:        contextLabel,
		SequenceNum:    seq,
		IsComplete:     true,
		FactCount:      factCount,
		Content:        content,
		CoveredFactIds: []pgtype.UUID{},
		Model:          &model,
	})
	if err != nil {
		t.Fatalf("create summary slice: %v", err)
	}
	return row.ID
}

// seedSource creates a minimal source row for the repo and returns
// its id. Used by image-fact seeding (image facts need a source link
// so ListGroupImageFacts's fact_concepts join has a source to point
// at, though the image query itself doesn't join sources — the link
// keeps the fact consistent with the rest of the pipeline).
func seedSource(t *testing.T, env *testutil.TestEnv, repoID pgtype.UUID) pgtype.UUID {
	t.Helper()
	ctx := context.Background()
	srcID := pgtype.UUID{}
	if err := srcID.Scan(uuid.NewString()); err != nil {
		t.Fatalf("scan src: %v", err)
	}
	if _, err := store.New(env.DB).CreateSource(ctx, store.CreateSourceParams{
		ID: srcID, RepositoryID: repoID, Url: "https://example.com/" + uuid.NewString(), Kind: "homepage", Status: "fetched",
	}); err != nil {
		t.Fatalf("create source: %v", err)
	}
	return srcID
}

// seedImageFact creates one image fact (fact_kind='image' with an
// image_url) linked to the given concept_id, returning the fact_id.
func seedImageFact(t *testing.T, env *testutil.TestEnv, repoID, srcID, conceptID pgtype.UUID, text, imageURL string) pgtype.UUID {
	t.Helper()
	ctx := context.Background()
	queries := store.New(env.DB)
	factID := pgtype.UUID{}
	if err := factID.Scan(uuid.NewString()); err != nil {
		t.Fatalf("scan image fact id: %v", err)
	}
	if _, err := queries.CreateFact(ctx, store.CreateFactParams{
		ID: factID, Text: text, FactKind: "image", ImageUrl: &imageURL,
	}); err != nil {
		t.Fatalf("create image fact: %v", err)
	}
	if err := queries.AddFactSource(ctx, store.AddFactSourceParams{
		FactID: factID, SourceID: srcID, ChunkIndex: -1,
	}); err != nil {
		t.Fatalf("link image fact to source: %v", err)
	}
	if _, err := queries.AddFactConcept(ctx, store.AddFactConceptParams{FactID: factID, ConceptID: conceptID}); err != nil && !pgxErrNoRows(err) {
		t.Fatalf("link image fact to concept: %v", err)
	}
	return factID
}

func pgxErrNoRows(err error) bool { return err != nil && strings.Contains(err.Error(), "no rows") }

// mustGetSynthesis fetches the single synthesis row for a
// (repo_id, canonical_name) group, failing the test when none exists.
func mustGetSynthesis(t *testing.T, env *testutil.TestEnv, repoID pgtype.UUID, canonicalName string) store.OktRepositoryConceptSynthesis {
	t.Helper()
	ctx := context.Background()
	row, err := store.New(env.DB).GetSynthesisByGroup(ctx, store.GetSynthesisByGroupParams{
		RepositoryID:  repoID,
		CanonicalName: canonicalName,
	})
	if err != nil {
		t.Fatalf("get synthesis for %q: %v", canonicalName, err)
	}
	return row
}

// TestSynthesizeConcept_HappyPathNoImages verifies the core path:
// seed one concept with summary slices, enqueue a synthesize_concept
// job, assert one concept_syntheses row exists with content, the
// covered_summary_ids match the seeded slice ids, and no images are
// embedded (no image facts seeded).
func TestSynthesizeConcept_HappyPathNoImages(t *testing.T) {
	cfg := config.SynthesisConfig{Enabled: true, Model: "stub-synth", MaxImages: 10, MaxImageCandidates: 50}
	ste := newSynthesizeTestEnv(t, cfg)
	admin := bootstrapSysAdmin(t, ste.env, "synthhappy@example.com")
	_, _, repoID := createRepositoryWithDB(t, admin, "SynthHappy", "synth-happy", "desc", "")
	pgRepo := pgRepoID(t, repoID)

	conceptID, _ := seedConceptWithFacts(t, ste.env, pgRepo, "Methotrexate", "Biomolecule", 3)
	sliceID := seedSliceForConcept(t, ste.env, conceptID, pgRepo, "Biomolecule", 1, 3, "# Slice 1 about Methotrexate")

	res := runSynthesizeJob(t, ste, tasks.SynthesizeConceptArgs{RepositoryID: repoID, ConceptID: pgUUIDString(conceptID)})
	if res.Created != 1 || res.Updated != 0 || res.Errors != 0 {
		t.Fatalf("result: created=%d updated=%d errors=%d, want 1/0/0", res.Created, res.Updated, res.Errors)
	}
	if res.CanonicalName != "Methotrexate" {
		t.Errorf("result canonical_name = %q, want Methotrexate", res.CanonicalName)
	}

	syn := mustGetSynthesis(t, ste.env, pgRepo, "Methotrexate")
	if !strings.Contains(syn.Content, "Methotrexate") {
		t.Errorf("synthesis content = %q, want it to mention the concept name", syn.Content)
	}
	if len(syn.CoveredSummaryIds) != 1 || syn.CoveredSummaryIds[0] != sliceID {
		t.Errorf("covered_summary_ids = %v, want [%s]", syn.CoveredSummaryIds, sliceID)
	}
	if len(syn.EmbeddedImageIds) != 0 {
		t.Errorf("embedded_image_ids = %v, want empty (no image facts seeded)", syn.EmbeddedImageIds)
	}
	if ste.stub.synthCallCount() != 1 {
		t.Errorf("synthesizer calls = %d, want 1", ste.stub.synthCallCount())
	}
	if ste.stub.pickerCallCount() != 0 {
		t.Errorf("picker calls = %d, want 0 (no image candidates)", ste.stub.pickerCallCount())
	}
}

// TestSynthesizeConcept_WithImages verifies the image-enrichment
// path: seed image facts linked to the group, run the worker, assert
// the synthesis embeds one image (embedded_image_ids non-empty) and
// the content contains a ![alt](<fact:fact_id>) citation for it.
func TestSynthesizeConcept_WithImages(t *testing.T) {
	cfg := config.SynthesisConfig{Enabled: true, Model: "stub-synth", MaxImages: 10, MaxImageCandidates: 50}
	ste := newSynthesizeTestEnv(t, cfg)
	admin := bootstrapSysAdmin(t, ste.env, "synthimg@example.com")
	_, _, repoID := createRepositoryWithDB(t, admin, "SynthImg", "synth-img", "desc", "")
	pgRepo := pgRepoID(t, repoID)

	conceptID, _ := seedConceptWithFacts(t, ste.env, pgRepo, "Riboflavin", "Biomolecule", 2)
	seedSliceForConcept(t, ste.env, conceptID, pgRepo, "Biomolecule", 1, 2, "# Slice 1 about Riboflavin")
	srcID := seedSource(t, ste.env, pgRepo)
	imgID := seedImageFact(t, ste.env, pgRepo, srcID, conceptID, "A diagram of the riboflavin molecule.", "https://example.com/riboflavin.png")

	res := runSynthesizeJob(t, ste, tasks.SynthesizeConceptArgs{RepositoryID: repoID, ConceptID: pgUUIDString(conceptID)})
	if res.Created != 1 || res.Errors != 0 {
		t.Fatalf("result: created=%d errors=%d, want 1/0", res.Created, res.Errors)
	}
	if res.ImagesPicked != 1 {
		t.Errorf("images_picked = %d, want 1 (1 candidate <= MaxImages 10, picker skipped)", res.ImagesPicked)
	}
	if ste.stub.pickerCallCount() != 0 {
		t.Errorf("picker calls = %d, want 0 (1 candidate <= MaxImages, picker skipped)", ste.stub.pickerCallCount())
	}

	syn := mustGetSynthesis(t, ste.env, pgRepo, "Riboflavin")
	if len(syn.EmbeddedImageIds) != 1 || syn.EmbeddedImageIds[0] != imgID {
		t.Errorf("embedded_image_ids = %v, want [%s]", syn.EmbeddedImageIds, imgID)
	}
	if !strings.Contains(syn.Content, "![illustration](<fact:"+pgUUIDString(imgID)+">)") {
		t.Errorf("content missing image citation: %q", syn.Content)
	}
}

// TestSynthesizeConcept_PickerCalledWhenCandidatesExceedMaxImages
// verifies the picker is invoked when the candidate pool exceeds
// MaxImages, and that the picked ids are the ones passed to the
// synthesis.
func TestSynthesizeConcept_PickerCalledWhenCandidatesExceedMaxImages(t *testing.T) {
	cfg := config.SynthesisConfig{Enabled: true, Model: "stub-synth", MaxImages: 3, MaxImageCandidates: 50}
	ste := newSynthesizeTestEnv(t, cfg)
	admin := bootstrapSysAdmin(t, ste.env, "synthpick@example.com")
	_, _, repoID := createRepositoryWithDB(t, admin, "SynthPick", "synth-pick", "desc", "")
	pgRepo := pgRepoID(t, repoID)

	conceptID, _ := seedConceptWithFacts(t, ste.env, pgRepo, "Caffeine", "Biomolecule", 2)
	seedSliceForConcept(t, ste.env, conceptID, pgRepo, "Biomolecule", 1, 2, "# Slice 1 about Caffeine")
	srcID := seedSource(t, ste.env, pgRepo)
	// Seed 5 image candidates (> MaxImages 3) so the picker runs.
	var seeded []pgtype.UUID
	for i := 0; i < 5; i++ {
		seeded = append(seeded, seedImageFact(t, ste.env, pgRepo, srcID, conceptID, "caffeine figure "+itoa(i), "https://example.com/caffeine-"+itoa(i)+".png"))
	}
	// Force the picker to return the first 3 candidate ids.
	ste.stub.pickedIDs = []string{pgUUIDString(seeded[0]), pgUUIDString(seeded[1]), pgUUIDString(seeded[2])}

	res := runSynthesizeJob(t, ste, tasks.SynthesizeConceptArgs{RepositoryID: repoID, ConceptID: pgUUIDString(conceptID)})
	if res.Errors != 0 {
		t.Fatalf("result errors = %d, want 0", res.Errors)
	}
	if ste.stub.pickerCallCount() != 1 {
		t.Fatalf("picker calls = %d, want 1 (5 candidates > MaxImages 3)", ste.stub.pickerCallCount())
	}
	if ste.stub.lastPickReq.MaxImages != 3 {
		t.Errorf("picker MaxImages = %d, want 3", ste.stub.lastPickReq.MaxImages)
	}
	if len(ste.stub.lastSynthReq.CandidateImages) != 3 {
		t.Errorf("synthesis received %d candidate images, want 3 (picker narrowed 5->3)", len(ste.stub.lastSynthReq.CandidateImages))
	}
	if res.ImagesPicked != 3 {
		t.Errorf("images_picked = %d, want 3", res.ImagesPicked)
	}
}

// TestSynthesizeConcept_PickerFailureStillSynthesizes verifies the
// "skip images, still synthesize" path: when the picker returns an
// error, the synthesis is still written with no embedded images.
func TestSynthesizeConcept_PickerFailureStillSynthesizes(t *testing.T) {
	cfg := config.SynthesisConfig{Enabled: true, Model: "stub-synth", MaxImages: 3, MaxImageCandidates: 50}
	ste := newSynthesizeTestEnv(t, cfg)
	ste.stub.pickerFail = true
	admin := bootstrapSysAdmin(t, ste.env, "synthpickfail@example.com")
	_, _, repoID := createRepositoryWithDB(t, admin, "SynthPickFail", "synth-pickfail", "desc", "")
	pgRepo := pgRepoID(t, repoID)

	conceptID, _ := seedConceptWithFacts(t, ste.env, pgRepo, "Theanine", "Biomolecule", 2)
	seedSliceForConcept(t, ste.env, conceptID, pgRepo, "Biomolecule", 1, 2, "# Slice 1 about Theanine")
	srcID := seedSource(t, ste.env, pgRepo)
	for i := 0; i < 5; i++ {
		seedImageFact(t, ste.env, pgRepo, srcID, conceptID, "theanine figure "+itoa(i), "https://example.com/theanine-"+itoa(i)+".png")
	}

	res := runSynthesizeJob(t, ste, tasks.SynthesizeConceptArgs{RepositoryID: repoID, ConceptID: pgUUIDString(conceptID)})
	if res.Created != 1 || res.Errors != 0 {
		t.Fatalf("result: created=%d errors=%d, want 1/0 (synthesis written despite picker failure)", res.Created, res.Errors)
	}
	if res.ImagesPicked != 0 {
		t.Errorf("images_picked = %d, want 0 (picker failed)", res.ImagesPicked)
	}
	syn := mustGetSynthesis(t, ste.env, pgRepo, "Theanine")
	if len(syn.EmbeddedImageIds) != 0 {
		t.Errorf("embedded_image_ids = %v, want empty (picker failed)", syn.EmbeddedImageIds)
	}
	if ste.stub.synthCallCount() != 1 {
		t.Errorf("synthesizer calls = %d, want 1 (ran despite picker failure)", ste.stub.synthCallCount())
	}
}

// TestSynthesizeConcept_UpsertSingleRow verifies the "just one,
// updated when there is a new summary" storage model: enqueue twice
// with a new slice between runs, assert only one row exists, content
// updated, updated_at advanced.
func TestSynthesizeConcept_UpsertSingleRow(t *testing.T) {
	cfg := config.SynthesisConfig{Enabled: true, Model: "stub-synth", MaxImages: 10, MaxImageCandidates: 50}
	ste := newSynthesizeTestEnv(t, cfg)
	admin := bootstrapSysAdmin(t, ste.env, "synthupsert@example.com")
	_, _, repoID := createRepositoryWithDB(t, admin, "SynthUpsert", "synth-upsert", "desc", "")
	pgRepo := pgRepoID(t, repoID)

	conceptID, _ := seedConceptWithFacts(t, ste.env, pgRepo, "Ketamine", "Biomolecule", 2)
	seedSliceForConcept(t, ste.env, conceptID, pgRepo, "Biomolecule", 1, 2, "# Slice 1 about Ketamine")

	res1 := runSynthesizeJob(t, ste, tasks.SynthesizeConceptArgs{RepositoryID: repoID, ConceptID: pgUUIDString(conceptID)})
	if res1.Created != 1 || res1.Updated != 0 {
		t.Fatalf("first run: created=%d updated=%d, want 1/0", res1.Created, res1.Updated)
	}
	first := mustGetSynthesis(t, ste.env, pgRepo, "Ketamine")
	firstUpdated := first.UpdatedAt

	// Add a second slice and re-run. The same row is updated.
	time.Sleep(1 * time.Second) // ensure updated_at advances
	seedSliceForConcept(t, ste.env, conceptID, pgRepo, "Biomolecule", 2, 2, "# Slice 2 about Ketamine")
	res2 := runSynthesizeJob(t, ste, tasks.SynthesizeConceptArgs{RepositoryID: repoID, ConceptID: pgUUIDString(conceptID)})
	if res2.Created != 0 || res2.Updated != 1 {
		t.Fatalf("second run: created=%d updated=%d, want 0/1 (same row updated)", res2.Created, res2.Updated)
	}
	second := mustGetSynthesis(t, ste.env, pgRepo, "Ketamine")
	if second.ID != first.ID {
		t.Errorf("row id changed: first=%s second=%s (want same row upserted)", first.ID, second.ID)
	}
	if !second.UpdatedAt.Time.After(firstUpdated.Time) {
		t.Errorf("updated_at did not advance: first=%s second=%s", firstUpdated.Time, second.UpdatedAt.Time)
	}
	if len(second.CoveredSummaryIds) != 2 {
		t.Errorf("covered_summary_ids = %d, want 2 (both slices folded)", len(second.CoveredSummaryIds))
	}
}

// TestSynthesizeConcept_NoDeltaSkip verifies that re-running the
// worker with no new slices is a no-op (SkippedNoDelta=1, no
// synthesizer call, row unchanged).
func TestSynthesizeConcept_NoDeltaSkip(t *testing.T) {
	cfg := config.SynthesisConfig{Enabled: true, Model: "stub-synth", MaxImages: 10, MaxImageCandidates: 50}
	ste := newSynthesizeTestEnv(t, cfg)
	admin := bootstrapSysAdmin(t, ste.env, "synthskip@example.com")
	_, _, repoID := createRepositoryWithDB(t, admin, "SynthSkip", "synth-skip", "desc", "")
	pgRepo := pgRepoID(t, repoID)

	conceptID, _ := seedConceptWithFacts(t, ste.env, pgRepo, "Melatonin", "Biomolecule", 2)
	seedSliceForConcept(t, ste.env, conceptID, pgRepo, "Biomolecule", 1, 2, "# Slice 1 about Melatonin")

	runSynthesizeJob(t, ste, tasks.SynthesizeConceptArgs{RepositoryID: repoID, ConceptID: pgUUIDString(conceptID)})
	callsAfterFirst := ste.stub.synthCallCount()

	res := runSynthesizeJob(t, ste, tasks.SynthesizeConceptArgs{RepositoryID: repoID, ConceptID: pgUUIDString(conceptID)})
	if res.SkippedNoDelta != 1 || res.Created != 0 || res.Updated != 0 {
		t.Errorf("no-delta run: skipped=%d created=%d updated=%d, want 1/0/0", res.SkippedNoDelta, res.Created, res.Updated)
	}
	if ste.stub.synthCallCount() != callsAfterFirst {
		t.Errorf("synthesizer calls after no-delta = %d, want %d (no new LLM call)", ste.stub.synthCallCount(), callsAfterFirst)
	}
}

// TestSynthesizeConcept_GroupScopeFoldsMultipleContexts verifies the
// per-canonical-name group scope: two concept_ids sharing
// lower(canonical_name) but different contexts -> one synthesis row
// folding both concept_ids' slices.
func TestSynthesizeConcept_GroupScopeFoldsMultipleContexts(t *testing.T) {
	cfg := config.SynthesisConfig{Enabled: true, Model: "stub-synth", MaxImages: 10, MaxImageCandidates: 50}
	ste := newSynthesizeTestEnv(t, cfg)
	admin := bootstrapSysAdmin(t, ste.env, "synthgroup@example.com")
	_, _, repoID := createRepositoryWithDB(t, admin, "SynthGroup", "synth-group", "desc", "")
	pgRepo := pgRepoID(t, repoID)

	// Two concepts with the same canonical name but different contexts.
	c1, _ := seedConceptWithFacts(t, ste.env, pgRepo, "Aspirin", "Biomolecule", 2)
	c2, _ := seedConceptWithFacts(t, ste.env, pgRepo, "Aspirin", "Drug", 3)
	seedSliceForConcept(t, ste.env, c1, pgRepo, "Biomolecule", 1, 2, "# Slice 1 (Biomolecule)")
	seedSliceForConcept(t, ste.env, c2, pgRepo, "Drug", 1, 3, "# Slice 1 (Drug)")

	// Enqueue via either concept_id; both resolve to the same group.
	res := runSynthesizeJob(t, ste, tasks.SynthesizeConceptArgs{RepositoryID: repoID, ConceptID: pgUUIDString(c1)})
	if res.Created != 1 || res.Errors != 0 {
		t.Fatalf("result: created=%d errors=%d, want 1/0", res.Created, res.Errors)
	}
	syn := mustGetSynthesis(t, ste.env, pgRepo, "Aspirin")
	if len(syn.CoveredSummaryIds) != 2 {
		t.Errorf("covered_summary_ids = %d, want 2 (both contexts folded)", len(syn.CoveredSummaryIds))
	}
	if len(syn.CoveredConceptIds) != 2 {
		t.Errorf("covered_concept_ids = %d, want 2 (both concept_ids in group)", len(syn.CoveredConceptIds))
	}
	// The synthesis content should reference both slices.
	if !strings.Contains(syn.Content, "2 slice") {
		t.Errorf("content = %q, want it to mention 2 slices", syn.Content)
	}
}

// TestSynthesizeConcept_HTTPReadEndpoint verifies the
// GET /concepts/{conceptID}/definition endpoint:
//   - 200 with the synthesis + eager-loaded images after the worker
//     has written a row.
//   - 404 when no definition exists yet.
//   - Missing auth is a 401.
func TestSynthesizeConcept_HTTPReadEndpoint(t *testing.T) {
	cfg := config.SynthesisConfig{Enabled: true, Model: "stub-synth", MaxImages: 10, MaxImageCandidates: 50}
	ste := newSynthesizeTestEnv(t, cfg)
	admin := bootstrapSysAdmin(t, ste.env, "synthhttp@example.com")
	const slug = "synth-http"
	_, _, repoID := createRepositoryWithDB(t, admin, "SynthHTTP", slug, "desc", "")
	pgRepo := pgRepoID(t, repoID)

	conceptID, _ := seedConceptWithFacts(t, ste.env, pgRepo, "Valerian", "Biomolecule", 2)
	seedSliceForConcept(t, ste.env, conceptID, pgRepo, "Biomolecule", 1, 2, "# Slice 1 about Valerian")
	srcID := seedSource(t, ste.env, pgRepo)
	imgID := seedImageFact(t, ste.env, pgRepo, srcID, conceptID, "Valerian root diagram.", "https://example.com/valerian.png")
	runSynthesizeJob(t, ste, tasks.SynthesizeConceptArgs{RepositoryID: repoID, ConceptID: pgUUIDString(conceptID)})

	cidStr := pgUUIDString(conceptID)

	// 200 with synthesis + images.
	resp, raw := admin.do("GET", "/api/v1/repositories/"+slug+"/concepts/"+cidStr+"/definition", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET definition: %d %s", resp.StatusCode, raw)
	}
	var body struct {
		Synthesis store.OktRepositoryConceptSynthesis `json:"synthesis"`
		Images    []struct {
			ID       string `json:"id"`
			Text     string `json:"text"`
			ImageURL string `json:"image_url"`
			FactKind string `json:"fact_kind"`
		} `json:"images"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("decode definition: %v", err)
	}
	if body.Synthesis.CanonicalName != "Valerian" {
		t.Errorf("synthesis canonical_name = %q, want Valerian", body.Synthesis.CanonicalName)
	}
	if !strings.Contains(body.Synthesis.Content, "Valerian") {
		t.Errorf("synthesis content = %q, want it to mention the concept", body.Synthesis.Content)
	}
	if len(body.Images) != 1 || body.Images[0].ID != pgUUIDString(imgID) {
		t.Errorf("images = %+v, want 1 image with id %s", body.Images, pgUUIDString(imgID))
	}
	if body.Images[0].ImageURL != "https://example.com/valerian.png" {
		t.Errorf("image url = %q, want the seeded URL", body.Images[0].ImageURL)
	}
	if body.Images[0].FactKind != "image" {
		t.Errorf("image fact_kind = %q, want image", body.Images[0].FactKind)
	}

	// 404 when no definition exists: a second concept with no slices.
	conceptID2, _ := seedConceptWithFacts(t, ste.env, pgRepo, "Echinacea", "Biomolecule", 1)
	resp2, _ := admin.do("GET", "/api/v1/repositories/"+slug+"/concepts/"+pgUUIDString(conceptID2)+"/definition", nil)
	if resp2.StatusCode != http.StatusNotFound {
		t.Errorf("no-definition GET: status %d, want 404", resp2.StatusCode)
	}

	// Missing auth: 401.
	anon := newAuthClient(ste.env.BaseURL)
	resp3, _ := anon.do("GET", "/api/v1/repositories/"+slug+"/concepts/"+cidStr+"/definition", nil)
	if resp3.StatusCode != http.StatusUnauthorized {
		t.Errorf("no-auth definition: status %d, want 401", resp3.StatusCode)
	}
}

// TestSynthesizeConcept_NotEnabledIsNoOp verifies the worker is a
// no-op (records a zero result, no synthesizer call) when
// cfg.Enabled is false.
func TestSynthesizeConcept_NotEnabledIsNoOp(t *testing.T) {
	ste := newSynthesizeTestEnv(t, config.SynthesisConfig{Enabled: false})
	admin := bootstrapSysAdmin(t, ste.env, "synthdisabled@example.com")
	_, _, repoID := createRepositoryWithDB(t, admin, "SynthDisabled", "synth-disabled", "desc", "")
	pgRepo := pgRepoID(t, repoID)
	conceptID, _ := seedConceptWithFacts(t, ste.env, pgRepo, "Glutathione", "Biomolecule", 2)
	seedSliceForConcept(t, ste.env, conceptID, pgRepo, "Biomolecule", 1, 2, "# Slice 1")

	res := runSynthesizeJob(t, ste, tasks.SynthesizeConceptArgs{RepositoryID: repoID, ConceptID: pgUUIDString(conceptID)})
	if res.Created != 0 || res.Updated != 0 || res.Errors != 0 {
		t.Errorf("disabled result: created=%d updated=%d errors=%d, want 0/0/0", res.Created, res.Updated, res.Errors)
	}
	if ste.stub.synthCallCount() != 0 {
		t.Errorf("synthesizer calls = %d, want 0 (disabled)", ste.stub.synthCallCount())
	}
}

// ai.Attribution import guard so the import stays used (the stub
// references ai.Attribution indirectly through the provider types).
var _ = ai.Attribution{}

// refreshConceptRelationsMatview synchronously refreshes the
// concept_relations materialized view so a test can seed shared facts
// and immediately read them via ListConceptRelationsByConceptName
// (the production path is the async refresh_concept_relations worker,
// which tests bypass for determinism).
func refreshConceptRelationsMatview(t *testing.T, env *testutil.TestEnv) {
	t.Helper()
	if _, err := env.DB.Exec(context.Background(),
		`REFRESH MATERIALIZED VIEW okt_repository.concept_relations`); err != nil {
		t.Fatalf("refresh concept_relations matview: %v", err)
	}
}

// linkExistingFactToConcept links an already-created fact to a concept
// via fact_concepts, so two concepts sharing one fact appear as a
// relation after the matview refresh. Errors on conflict are ignored
// (re-linking the same pair is idempotent for the test's purpose).
func linkExistingFactToConcept(t *testing.T, env *testutil.TestEnv, factID, conceptID pgtype.UUID) {
	t.Helper()
	if _, err := store.New(env.DB).AddFactConcept(context.Background(), store.AddFactConceptParams{
		FactID: factID, ConceptID: conceptID,
	}); err != nil && !pgxErrNoRows(err) {
		t.Fatalf("link existing fact→concept: %v", err)
	}
}

// TestSynthesizeConcept_RelatedConceptsEmptyByDefault verifies the
// worker passes a nil/empty RelatedConcepts slice to the synthesizer
// when the concept has no relations in the matview (the common case
// for a freshly-seeded concept with no shared facts). The synthesis
// still succeeds and the prompt's "No related concept data available"
// branch is taken.
func TestSynthesizeConcept_RelatedConceptsEmptyByDefault(t *testing.T) {
	cfg := config.SynthesisConfig{Enabled: true, Model: "stub-synth", MaxImages: 10, MaxImageCandidates: 50, MaxRelatedConcepts: 10, MaxRelatedSyntheses: 3}
	ste := newSynthesizeTestEnv(t, cfg)
	admin := bootstrapSysAdmin(t, ste.env, "synthrelempty@example.com")
	_, _, repoID := createRepositoryWithDB(t, admin, "SynthRelEmpty", "synth-rel-empty", "desc", "")
	pgRepo := pgRepoID(t, repoID)

	conceptID, _ := seedConceptWithFacts(t, ste.env, pgRepo, "Berberine", "Biomolecule", 2)
	seedSliceForConcept(t, ste.env, conceptID, pgRepo, "Biomolecule", 1, 2, "# Slice 1 about Berberine")

	res := runSynthesizeJob(t, ste, tasks.SynthesizeConceptArgs{RepositoryID: repoID, ConceptID: pgUUIDString(conceptID)})
	if res.Created != 1 || res.Errors != 0 {
		t.Fatalf("result: created=%d errors=%d, want 1/0", res.Created, res.Errors)
	}
	// The matview has no rows for Berberine (no shared facts), so the
	// worker's loadRelatedConcepts returns nil and the request carries
	// no related concepts.
	if len(ste.stub.lastSynthReq.RelatedConcepts) != 0 {
		t.Errorf("related concepts = %d, want 0 (no shared facts seeded)", len(ste.stub.lastSynthReq.RelatedConcepts))
	}
}

// TestSynthesizeConcept_RelatedConceptsPassedToSynthesizer verifies
// the worker loads the top N1 related concepts (names + per-context
// shared_fact_count) and the top N2 of those carry their existing
// synthesis text, then passes them to the synthesizer via
// SynthesisRequest.RelatedConcepts.
//
// Setup:
//   - Concept A "Quercetin" (Biomolecule) shares 3 facts with concept B
//     "Rutin" (Biomolecule), and 1 fact with concept C "Kaempferol"
//     (Biomolecule). Rutin is the strongest relation (3), Kaempferol
//     second (1).
//   - A synthesis row is pre-seeded for Rutin so it appears as
//     RelatedConceptInput.Synthesis for the top N2=1.
//   - Kaempferol has no synthesis (Synthesis stays "" — the "keep
//     them with name + count only" decision).
//
// Assertions on lastSynthReq.RelatedConcepts:
//   - len == 2 (Rutin, Kaempferol), ordered by shared_fact_count DESC.
//   - Rutin: SharedFactCount==3, Synthesis non-empty, Contexts has one
//     entry (Biomolecule: 3).
//   - Kaempferol: SharedFactCount==1, Synthesis=="" (no synthesis row).
func TestSynthesizeConcept_RelatedConceptsPassedToSynthesizer(t *testing.T) {
	cfg := config.SynthesisConfig{Enabled: true, Model: "stub-synth", MaxImages: 10, MaxImageCandidates: 50, MaxRelatedConcepts: 10, MaxRelatedSyntheses: 1}
	ste := newSynthesizeTestEnv(t, cfg)
	admin := bootstrapSysAdmin(t, ste.env, "synthrel@example.com")
	_, _, repoID := createRepositoryWithDB(t, admin, "SynthRel", "synth-rel", "desc", "")
	pgRepo := pgRepoID(t, repoID)
	queries := store.New(ste.env.DB)

	// One source for all facts in this repo.
	srcID := pgtype.UUID{}
	if err := srcID.Scan(uuid.NewString()); err != nil {
		t.Fatalf("scan src: %v", err)
	}
	if _, err := queries.CreateSource(context.Background(), store.CreateSourceParams{
		ID: srcID, RepositoryID: pgRepo, Url: "https://example.com/rel-src", Kind: "homepage", Status: "fetched",
	}); err != nil {
		t.Fatalf("create source: %v", err)
	}

	// Three concepts in the same context.
	quercetin, err := queries.CreateConcept(context.Background(), store.CreateConceptParams{
		RepositoryID: pgRepo, CanonicalName: "Quercetin", Context: "Biomolecule",
	})
	if err != nil {
		t.Fatalf("create Quercetin: %v", err)
	}
	rutin, err := queries.CreateConcept(context.Background(), store.CreateConceptParams{
		RepositoryID: pgRepo, CanonicalName: "Rutin", Context: "Biomolecule",
	})
	if err != nil {
		t.Fatalf("create Rutin: %v", err)
	}
	kaempferol, err := queries.CreateConcept(context.Background(), store.CreateConceptParams{
		RepositoryID: pgRepo, CanonicalName: "Kaempferol", Context: "Biomolecule",
	})
	if err != nil {
		t.Fatalf("create Kaempferol: %v", err)
	}

	// Helper to create + link a fact to two concepts (shared fact).
	mkSharedFact := func(text string, chunk int32, c1, c2 pgtype.UUID) pgtype.UUID {
		fidStr := insertFactWithSource(t, ste.env, pgRepo, srcID, text, chunk)
		fid := pgtype.UUID{}
		if err := fid.Scan(fidStr); err != nil {
			t.Fatalf("scan fact: %v", err)
		}
		linkExistingFactToConcept(t, ste.env, fid, c1)
		linkExistingFactToConcept(t, ste.env, fid, c2)
		return fid
	}
	// 3 shared facts Quercetin <-> Rutin.
	mkSharedFact("Quercetin-Rutin shared 1", 0, quercetin.ID, rutin.ID)
	mkSharedFact("Quercetin-Rutin shared 2", 1, quercetin.ID, rutin.ID)
	mkSharedFact("Quercetin-Rutin shared 3", 2, quercetin.ID, rutin.ID)
	// 1 shared fact Quercetin <-> Kaempferol.
	mkSharedFact("Quercetin-Kaempferol shared 1", 3, quercetin.ID, kaempferol.ID)

	// Pre-seed a synthesis row for Rutin so the top-N2 (N2=1) relation
	// carries it. Kaempferol deliberately has no synthesis.
	rutinModel := "stub-synth"
	if _, err := queries.UpsertSynthesis(context.Background(), store.UpsertSynthesisParams{
		RepositoryID: pgRepo, CanonicalName: "Rutin",
		Content:           "# Definition of Rutin\n\nA flavonoid glycoside shared with Quercetin.",
		CoveredSummaryIds: []pgtype.UUID{}, CoveredConceptIds: []pgtype.UUID{}, EmbeddedImageIds: []pgtype.UUID{},
		Model: &rutinModel,
	}); err != nil {
		t.Fatalf("seed Rutin synthesis: %v", err)
	}

	// Refresh the matview so ListConceptRelationsByConceptName sees the
	// just-inserted fact_concepts links.
	refreshConceptRelationsMatview(t, ste.env)

	// Seed a summary slice for Quercetin so the worker has something to fold.
	seedSliceForConcept(t, ste.env, quercetin.ID, pgRepo, "Biomolecule", 1, 4, "# Slice 1 about Quercetin")

	res := runSynthesizeJob(t, ste, tasks.SynthesizeConceptArgs{RepositoryID: repoID, ConceptID: pgUUIDString(quercetin.ID)})
	if res.Created != 1 || res.Errors != 0 {
		t.Fatalf("result: created=%d errors=%d, want 1/0", res.Created, res.Errors)
	}

	related := ste.stub.lastSynthReq.RelatedConcepts
	if len(related) != 2 {
		t.Fatalf("related concepts = %d, want 2 (Rutin + Kaempferol)", len(related))
	}
	// Ordered by shared_fact_count DESC: Rutin (3) first, Kaempferol (1) second.
	if related[0].CanonicalName != "Rutin" {
		t.Errorf("related[0] = %q, want Rutin (strongest, 3 shared)", related[0].CanonicalName)
	}
	if related[0].SharedFactCount != 3 {
		t.Errorf("related[0] shared_fact_count = %d, want 3", related[0].SharedFactCount)
	}
	if related[0].Synthesis == "" {
		t.Errorf("related[0] (Rutin) Synthesis = empty, want the pre-seeded synthesis text (top N2=1 carries it)")
	}
	if !strings.Contains(related[0].Synthesis, "Rutin") {
		t.Errorf("related[0] Synthesis = %q, want it to contain the Rutin synthesis content", related[0].Synthesis)
	}
	// Per-context breakdown: Quercetin has one context (Biomolecule)
	// sharing 3 facts with Rutin.
	if len(related[0].Contexts) != 1 || related[0].Contexts[0].Context != "Biomolecule" || related[0].Contexts[0].SharedFactCount != 3 {
		t.Errorf("related[0] Contexts = %+v, want one Biomolecule context with 3 shared", related[0].Contexts)
	}

	if related[1].CanonicalName != "Kaempferol" {
		t.Errorf("related[1] = %q, want Kaempferol (1 shared)", related[1].CanonicalName)
	}
	if related[1].SharedFactCount != 1 {
		t.Errorf("related[1] shared_fact_count = %d, want 1", related[1].SharedFactCount)
	}
	// Kaempferol has no synthesis row -> Synthesis stays "". This is
	// the "keep them with name + count only" decision.
	if related[1].Synthesis != "" {
		t.Errorf("related[1] (Kaempferol) Synthesis = %q, want empty (no synthesis row seeded, rank > N2)", related[1].Synthesis)
	}
}

// TestSynthesizeConcept_RelatedConceptsDisabledWhenN1Zero verifies
// that setting MaxRelatedConcepts=0 disables the relations block
// entirely (the worker skips loadRelatedConcepts and the synthesizer
// receives no related concepts), even when shared facts exist.
func TestSynthesizeConcept_RelatedConceptsDisabledWhenN1Zero(t *testing.T) {
	cfg := config.SynthesisConfig{Enabled: true, Model: "stub-synth", MaxImages: 10, MaxImageCandidates: 50, MaxRelatedConcepts: 0, MaxRelatedSyntheses: 0}
	ste := newSynthesizeTestEnv(t, cfg)
	admin := bootstrapSysAdmin(t, ste.env, "synthreldisabled@example.com")
	_, _, repoID := createRepositoryWithDB(t, admin, "SynthRelDisabled", "synth-rel-disabled", "desc", "")
	pgRepo := pgRepoID(t, repoID)
	queries := store.New(ste.env.DB)

	srcID := pgtype.UUID{}
	if err := srcID.Scan(uuid.NewString()); err != nil {
		t.Fatalf("scan src: %v", err)
	}
	if _, err := queries.CreateSource(context.Background(), store.CreateSourceParams{
		ID: srcID, RepositoryID: pgRepo, Url: "https://example.com/dis-src", Kind: "homepage", Status: "fetched",
	}); err != nil {
		t.Fatalf("create source: %v", err)
	}

	genistein, err := queries.CreateConcept(context.Background(), store.CreateConceptParams{
		RepositoryID: pgRepo, CanonicalName: "Genistein", Context: "Biomolecule",
	})
	if err != nil {
		t.Fatalf("create Genistein: %v", err)
	}
	daidzein, err := queries.CreateConcept(context.Background(), store.CreateConceptParams{
		RepositoryID: pgRepo, CanonicalName: "Daidzein", Context: "Biomolecule",
	})
	if err != nil {
		t.Fatalf("create Daidzein: %v", err)
	}
	// One shared fact (would appear as a relation if N1>0).
	fidStr := insertFactWithSource(t, ste.env, pgRepo, srcID, "Genistein-Daidzein shared", 0)
	fid := pgtype.UUID{}
	if err := fid.Scan(fidStr); err != nil {
		t.Fatalf("scan fact: %v", err)
	}
	linkExistingFactToConcept(t, ste.env, fid, genistein.ID)
	linkExistingFactToConcept(t, ste.env, fid, daidzein.ID)
	refreshConceptRelationsMatview(t, ste.env)

	seedSliceForConcept(t, ste.env, genistein.ID, pgRepo, "Biomolecule", 1, 1, "# Slice 1 about Genistein")

	res := runSynthesizeJob(t, ste, tasks.SynthesizeConceptArgs{RepositoryID: repoID, ConceptID: pgUUIDString(genistein.ID)})
	if res.Created != 1 || res.Errors != 0 {
		t.Fatalf("result: created=%d errors=%d, want 1/0", res.Created, res.Errors)
	}
	if len(ste.stub.lastSynthReq.RelatedConcepts) != 0 {
		t.Errorf("related concepts = %d, want 0 (MaxRelatedConcepts=0 disables the block)", len(ste.stub.lastSynthReq.RelatedConcepts))
	}
}

// failingSynthesizer is a stub that fails the first N Synthesize calls
// then succeeds, so tests can assert retry-then-recover behavior.
type failingSynthesizer struct {
	stubSynthesizer
	failN int
	calls int
	mu    sync.Mutex
}

func (f *failingSynthesizer) Synthesize(_ context.Context, _ store.DBTX, _ synthesis.SynthesisRequest) (string, error) {
	f.mu.Lock()
	f.calls++
	n := f.calls
	f.mu.Unlock()
	if n <= f.failN {
		return "", &synthRetryErr{fmt.Sprintf("forced failure %d/%d", n, f.failN)}
	}
	return "# Recovered synthesis\n\nFolds slices.", nil
}

func (f *failingSynthesizer) PickImages(_ context.Context, _ store.DBTX, _ synthesis.ImagePickRequest) ([]string, error) {
	return nil, nil
}

func (f *failingSynthesizer) Describe() synthesis.ProviderDescription {
	return synthesis.ProviderDescription{Name: "failing-synthesizer", Configured: true}
}

type synthRetryErr struct{ msg string }

func (e *synthRetryErr) Error() string { return e.msg }

// runSynthesizeJobAllowFail wraps the rivertest.Work call but does
// NOT fatality on a non-completed event — it returns the job + work
// error so the caller can assert on EventKindJobFailed /
// EventKindJobRetry. Mirrors the pattern in image_extraction_test.go.
func runSynthesizeJobAllowFail(t *testing.T, e *synthesizeTestEnv, args tasks.SynthesizeConceptArgs) (river.EventKind, error, tasks.SynthesizeConceptResult) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	tx, err := e.env.DB.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer tx.Rollback(context.Background())
	job, workErr := e.testWorker.Work(ctx, t, tx, args, &river.InsertOpts{Queue: tasks.QueueSynthesizeConcept})
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
	var res tasks.SynthesizeConceptResult
	if job != nil && len(job.Job.Output()) > 0 {
		_ = json.Unmarshal(job.Job.Output(), &res)
	}
	if job == nil {
		return "", workErr, res
	}
	return job.EventKind, workErr, res
}

// TestSynthesizeConcept_FailsAfterMaxAttempts verifies that when the
// LLM call fails on every attempt, the worker returns a non-nil error
// (so River marks the job as retryable/failed) instead of silently
// swallowing it as completed. The stub synthesizer always fails.
func TestSynthesizeConcept_FailsAfterMaxAttempts(t *testing.T) {
	cfg := config.SynthesisConfig{Enabled: true, Model: "stub-synth", MaxImages: 0, MaxImageCandidates: 0}
	ste := newSynthesizeTestEnv(t, cfg)
	admin := bootstrapSysAdmin(t, ste.env, "synthfail@example.com")
	_, _, repoID := createRepositoryWithDB(t, admin, "SynthFail", "synth-fail", "desc", "")
	pgRepo := pgRepoID(t, repoID)
	conceptID, _ := seedConceptWithFacts(t, ste.env, pgRepo, "FailingConcept", "TestCtx", 2)
	seedSliceForConcept(t, ste.env, conceptID, pgRepo, "TestCtx", 1, 2, "# Slice 1")

	// Replace the stub with an always-failing synthesizer.
	alwaysFail := &failingSynthesizer{failN: 999}
	ste.worker = tasks.NewSynthesizeConceptsWorker(alwaysFail, cfg, testutil.NewForTestPool(ste.env.DB), store.New(ste.env.DB), nil, nil)
	driver := riverpgxv5.New(ste.env.DB)
	workers := river.NewWorkers()
	river.AddWorker(workers, ste.worker)
	ste.testWorker = rivertest.NewWorker(t, driver, &river.Config{
		Queues:  map[string]river.QueueConfig{tasks.QueueSynthesizeConcept: {MaxWorkers: 1}},
		Workers: workers,
	}, ste.worker)

	eventKind, workErr, res := runSynthesizeJobAllowFail(t, ste, tasks.SynthesizeConceptArgs{RepositoryID: repoID, ConceptID: pgUUIDString(conceptID)})
	if eventKind != river.EventKindJobFailed {
		t.Fatalf("expected EventKindJobFailed, got %s (workErr=%v res=%+v)", eventKind, workErr, res)
	}
	if workErr == nil {
		t.Error("expected non-nil workErr when all attempts fail")
	}
	if res.Errors == 0 {
		t.Error("expected result.Errors > 0 when synthesis fails")
	}
	// No synthesis row should have been written.
	if _, err := store.New(ste.env.DB).GetSynthesisByGroup(context.Background(), store.GetSynthesisByGroupParams{
		RepositoryID:  pgRepo,
		CanonicalName: "FailingConcept",
	}); !errors.Is(err, pgx.ErrNoRows) {
		t.Errorf("expected no synthesis row, got err=%v", err)
	}
}

// TestSynthesizeConcept_ResynthesizeEndpoint verifies the per-concept
// POST /{repoID}/concepts/{conceptID}/resynthesize endpoint: a
// sysadmin can enqueue a synthesize_concept job for a concept, and a
// regular user (no repositories.*.manage) gets 403.
func TestSynthesizeConcept_ResynthesizeEndpoint(t *testing.T) {
	cfg := config.SynthesisConfig{Enabled: true, Model: "stub-synth", MaxImages: 0, MaxImageCandidates: 0}
	ste := newSynthesizeTestEnv(t, cfg)
	admin := bootstrapSysAdmin(t, ste.env, "resynthadmin@example.com")
	const slug = "resynth-repo"
	_, _, repoID := createRepositoryWithDB(t, admin, "Resynth Repo", slug, "desc", "")
	pgRepo := pgRepoID(t, repoID)
	conceptID, _ := seedConceptWithFacts(t, ste.env, pgRepo, "ResynthConcept", "TestCtx", 2)
	seedSliceForConcept(t, ste.env, conceptID, pgRepo, "TestCtx", 1, 2, "# Slice 1")
	cidStr := pgUUIDString(conceptID)

	// POST resynthesize as admin — should enqueue and return 200.
	resp, raw := admin.do("POST", "/api/v1/repositories/"+slug+"/concepts/"+cidStr+"/resynthesize", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST resynthesize: %d %s", resp.StatusCode, raw)
	}
	var out struct {
		RepositoryID  string `json:"repository_id"`
		ConceptID    string `json:"concept_id"`
		EnqueuedJobID string `json:"enqueued_job_id"`
		Enqueued     bool   `json:"enqueued"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if !out.Enqueued || out.EnqueuedJobID == "" {
		t.Errorf("response enqueued=%v job_id=%q, want enqueued=true with job id", out.Enqueued, out.EnqueuedJobID)
	}
	if out.ConceptID != cidStr {
		t.Errorf("response concept_id = %q, want %q", out.ConceptID, cidStr)
	}

	// Verify the enqueue was recorded by the test enqueuer.
	if len(ste.env.TaskEnqueuer.Synthesizes) != 1 {
		t.Fatalf("expected 1 synthesize enqueue, got %d", len(ste.env.TaskEnqueuer.Synthesizes))
	}
	if ste.env.TaskEnqueuer.Synthesizes[0].ConceptID != cidStr {
		t.Errorf("enqueued concept_id = %q, want %q", ste.env.TaskEnqueuer.Synthesizes[0].ConceptID, cidStr)
	}

	// Regular user (no repositories.*.manage) gets 403.
	regular := newAuthClient(ste.env.BaseURL)
	regular.register("resynth-user@example.com", "passw0rd!", "ResynthUser")
	regular.token = loginUser(regular, "resynth-user@example.com", "passw0rd!")
	if r, _ := regular.do("POST", "/api/v1/repositories/"+slug+"/concepts/"+cidStr+"/resynthesize", nil); r.StatusCode != http.StatusForbidden {
		t.Errorf("regular POST resynthesize: status %d, want 403", r.StatusCode)
	}

	// Cross-repo concept (non-existent) is a 404.
	otherCID := uuid.NewString()
	if r, _ := admin.do("POST", "/api/v1/repositories/"+slug+"/concepts/"+otherCID+"/resynthesize", nil); r.StatusCode != http.StatusNotFound {
		t.Errorf("cross-repo concept resynthesize: status %d, want 404", r.StatusCode)
	}
}