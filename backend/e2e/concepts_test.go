//go:build e2e

package e2e_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/openktree/open-knowledge-tree/backend/e2e/testutil"
	"github.com/openktree/open-knowledge-tree/backend/internal/config"
	"github.com/openktree/open-knowledge-tree/backend/internal/dbpool"
	"github.com/openktree/open-knowledge-tree/backend/internal/providers/decomposition"
	"github.com/openktree/open-knowledge-tree/backend/internal/providers/ontology"
	"github.com/openktree/open-knowledge-tree/backend/internal/store"
	"github.com/openktree/open-knowledge-tree/backend/internal/taskmanager/tasks"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivertest"
)

// stubConceptExtractor is a test double for
// decomposition.ConceptExtractionProvider that returns a fixed
// list of concepts for every fact in the batch, tagging each
// concept with the fact's index in the input slice. It lets the
// test drive ExtractConceptsWorker without a real AI provider.
type stubConceptExtractor struct {
	concepts []decomposition.ExtractedConcept
}

func (s *stubConceptExtractor) ExtractConcepts(_ context.Context, _ store.DBTX, facts []decomposition.FactInput, _ []decomposition.ContextEntry, _ decomposition.ConceptExtractionAttribution) ([]decomposition.ExtractedConcept, error) {
	out := make([]decomposition.ExtractedConcept, 0, len(s.concepts)*len(facts))
	for _, f := range facts {
		for _, c := range s.concepts {
			c.FactIndex = f.Index
			out = append(out, c)
		}
	}
	return out, nil
}

func (s *stubConceptExtractor) Describe() decomposition.ProviderDescription {
	return decomposition.ProviderDescription{Name: "stub-concept-extractor", Configured: true}
}

// stubL3Source is a test double for ontology.L3Source that returns
// a fixed list of context classes.
type stubL3Source struct {
	classes []ontology.ContextClass
}

func (s *stubL3Source) ContextClasses(_ context.Context) ([]ontology.ContextClass, error) {
	return s.classes, nil
}

// TestExtractConceptsPipeline drives the ExtractConceptsWorker
// directly against a fresh repo with two stable facts. It asserts:
//   - two concept_candidate rows are created (the two facts extract
//     different surface forms "deoxyribonucleic acid" and "DNA" that
//     don't text-match each other, so they create separate candidates
//     — refine_concepts will later merge them via seed alias / AI
//     canonical matching),
//   - each fact is linked to its candidate via fact_candidates,
//   - the extractor is called twice (once per fact),
//   - no concept rows exist yet (candidates are promoted by the
//     separate refine_concepts task, not by extract_concepts).
func TestExtractConceptsPipeline(t *testing.T) {
	env := testutil.NewTestEnv(t)
	defer env.Server.Close()
	ensureRiverSchema(t, env.DB)

	admin := bootstrapSysAdmin(t, env, "concepts@example.com")
	_, _, repoID := createRepositoryWithDB(t, admin, "Concepts", "concepts", "desc", "")
	queries := store.New(env.DB)

	// One source in the repo.
	srcID := pgtype.UUID{}
	if err := srcID.Scan(uuid.NewString()); err != nil {
		t.Fatalf("scanning source id: %v", err)
	}
	if _, err := queries.CreateSource(context.Background(), store.CreateSourceParams{
		ID: srcID, RepositoryID: pgRepoID(t, repoID), Url: "https://example.com/concept-src", Kind: "homepage", Status: "fetched",
	}); err != nil {
		t.Fatalf("create source: %v", err)
	}

	// Two stable facts mentioning the same concept under different
	// surface forms. The first extraction creates the concept; the
	// second fact's "DNA" surface form must text-search-match the
	// first fact's "deoxyribonucleic acid" seed alias.
	fact1ID := insertFactWithSource(t, env, pgRepoID(t, repoID), srcID, "Deoxyribonucleic acid is the molecule that carries genetic information.", 0)
	fact2ID := insertFactWithSource(t, env, pgRepoID(t, repoID), srcID, "DNA was first isolated by Friedrich Miescher in 1869.", 1)

	// Promote both facts to 'stable' so the worker picks them up.
	if _, err := env.DB.Exec(context.Background(),
		`UPDATE okt_repository.facts SET status = 'stable' WHERE id IN ($1, $2)`,
		fact1ID, fact2ID,
	); err != nil {
		t.Fatalf("promoting facts to stable: %v", err)
	}

	// Build the worker with stubs. The concept extractor returns the
	// same concept for both facts but with a different surface form
	// on the second fact, so the second extraction must match the
	// first via the seed alias "deoxyribonucleic acid".
	// The stub returns a fixed list; to exercise both the new-and-
	// match paths, the worker's per-fact loop will see the same
	// extraction for both facts. The first creates the concept; the
	// second matches it via the canonical name.
	// With FactBatchSize defaulting to 10, both facts are sent in a
	// single LLM call; the stub dispatches per fact_index so fact 0
	// gets the "deoxyribonucleic acid" surface and fact 1 gets "DNA".
	conceptsForFact := func(factIndex int) []decomposition.ExtractedConcept {
		if factIndex == 0 {
			return []decomposition.ExtractedConcept{
				{Concept: "deoxyribonucleic acid", Context: "Molecule", SeedAliases: []string{"DNA", "deoxyribonucleic"}},
			}
		}
		return []decomposition.ExtractedConcept{
			{Concept: "DNA", Context: "Molecule", SeedAliases: []string{"deoxyribonucleic acid"}},
		}
	}
	extractor := &countingConceptExtractor{fn: conceptsForFact}
	conceptCfg := config.DecompositionConceptConfig{Enabled: true}

	registry := testutil.NewForTestPool(env.DB)
	systemQueries := store.New(env.DB)

	worker := tasks.NewExtractConceptsWorker(extractor, conceptCfg, registry, systemQueries, nil, nil)
	// This test exercises the candidate-emission path (refinement
	// enabled), where extract creates candidates and defers routing
	// to refine_concepts. Without this, extract routes directly.
	worker.SetRefinementEnabled(true)

	driver := riverpgxv5.New(env.DB)
	workers := river.NewWorkers()
	river.AddWorker(workers, worker)
	testWorker := rivertest.NewWorker(t, driver, &river.Config{
		Queues:  map[string]river.QueueConfig{tasks.QueueExtractConcepts: {MaxWorkers: 1}},
		Workers: workers,
	}, worker)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	tx, err := env.DB.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	job, err := testWorker.Work(ctx, t, tx, tasks.ExtractConceptsArgs{RepositoryID: repoID}, &river.InsertOpts{Queue: tasks.QueueExtractConcepts})
	if err != nil {
		tx.Rollback(context.Background())
		t.Fatalf("extract_concepts.Work: %v", err)
	}
	if job.EventKind != river.EventKindJobCompleted {
		tx.Rollback(context.Background())
		t.Fatalf("extract_concepts: expected completed, got %s", job.EventKind)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit tx: %v", err)
	}

	// Assert: two concept_candidate rows exist (different surface forms
	// create separate candidates; refine_concepts will merge them later).
	var candidateCount int
	if err := env.DB.QueryRow(ctx,
		`SELECT count(*) FROM okt_repository.concept_candidates`,
	).Scan(&candidateCount); err != nil {
		t.Fatalf("querying candidate count: %v", err)
	}
	if candidateCount != 2 {
		t.Errorf("candidate count = %d, want 2 (two different surface forms create two candidates)", candidateCount)
	}

	// Assert: each fact is linked to its candidate via fact_candidates.
	var factCandidateCount int
	if err := env.DB.QueryRow(ctx,
		`SELECT count(*) FROM okt_repository.fact_candidates`,
	).Scan(&factCandidateCount); err != nil {
		t.Fatalf("querying fact_candidates: %v", err)
	}
	if factCandidateCount != 2 {
		t.Errorf("fact_candidates count = %d, want 2 (each fact linked to its candidate)", factCandidateCount)
	}

	// Assert: no concept rows exist yet (candidates are promoted by
	// refine_concepts, not by extract_concepts).
	var conceptCount int
	if err := env.DB.QueryRow(ctx, `SELECT count(*) FROM okt_repository.concepts`).Scan(&conceptCount); err != nil {
		t.Fatalf("querying concept count: %v", err)
	}
	if conceptCount != 0 {
		t.Errorf("concept count = %d, want 0 (candidates not yet refined)", conceptCount)
	}

	// Assert: the extractor should have been called once (both facts
	// fit in a single FactBatchSize=10 batch, so one LLM call covers
	// both). The per-fact dispatch still happens inside the stub via
	// fact_index.
	if extractor.calls != 1 {
		t.Errorf("extractor calls = %d, want 1 (two facts in one batch)", extractor.calls)
	}
}

// TestExtractConcepts_NotConfiguredIsNoop verifies the worker
// degrades gracefully when the concept extractor is nil: it records
// a no-op result and returns nil (River doesn't retry). This is the
// deployment shape where concept extraction is not configured but
// the API still boots.
func TestExtractConcepts_NotConfiguredIsNoop(t *testing.T) {
	env := testutil.NewTestEnv(t)
	defer env.Server.Close()
	ensureRiverSchema(t, env.DB)

	_, _, repoID := createRepositoryWithDB(t, bootstrapSysAdmin(t, env, "noconcept@example.com"), "NoConcept", "no-concept", "desc", "")
	registry := testutil.NewForTestPool(env.DB)
	systemQueries := store.New(env.DB)
	// nil concept extractor + nil alias provider.
	worker := tasks.NewExtractConceptsWorker(nil, config.DecompositionConceptConfig{Enabled: false}, registry, systemQueries, nil, nil)

	driver := riverpgxv5.New(env.DB)
	workers := river.NewWorkers()
	river.AddWorker(workers, worker)
	testWorker := rivertest.NewWorker(t, driver, &river.Config{
		Queues:  map[string]river.QueueConfig{tasks.QueueExtractConcepts: {MaxWorkers: 1}},
		Workers: workers,
	}, worker)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	tx, err := env.DB.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer tx.Rollback(context.Background())
	job, err := testWorker.Work(ctx, t, tx, tasks.ExtractConceptsArgs{RepositoryID: repoID}, &river.InsertOpts{Queue: tasks.QueueExtractConcepts})
	if err != nil {
		t.Fatalf("extract_concepts.Work (not configured): %v", err)
	}
	if job.EventKind != river.EventKindJobCompleted {
		t.Fatalf("expected completed, got %s", job.EventKind)
	}
}

// TestExtractConcepts_InvalidRepoID verifies the worker returns an
// error (so River retries / marks failed) when repository_id is
// missing or invalid.
func TestExtractConcepts_InvalidRepoID(t *testing.T) {
	env := testutil.NewTestEnv(t)
	defer env.Server.Close()
	ensureRiverSchema(t, env.DB)

	registry := testutil.NewForTestPool(env.DB)
	systemQueries := store.New(env.DB)
	worker := tasks.NewExtractConceptsWorker(
		&stubConceptExtractor{concepts: nil},
		config.DecompositionConceptConfig{Enabled: true},
		registry,
		systemQueries,
		nil,
		nil,
	)

	driver := riverpgxv5.New(env.DB)
	workers := river.NewWorkers()
	river.AddWorker(workers, worker)
	testWorker := rivertest.NewWorker(t, driver, &river.Config{
		Queues:  map[string]river.QueueConfig{tasks.QueueExtractConcepts: {MaxWorkers: 1}},
		Workers: workers,
	}, worker)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	tx, err := env.DB.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer tx.Rollback(context.Background())
	_, err = testWorker.Work(ctx, t, tx, tasks.ExtractConceptsArgs{RepositoryID: ""}, &river.InsertOpts{Queue: tasks.QueueExtractConcepts})
	if err == nil {
		t.Fatal("expected error for empty repository_id, got nil")
	}
}

// TestConcepts_Unauthenticated verifies the HTTP endpoints return
// 401 (AuthRequired) without a session. Uses a real repo slug so the
// WithRepoQueries middleware resolves the pool before AuthRequired
// runs (the middleware order is: WithRepoQueries → AuthRequired).
func TestConcepts_Unauthenticated(t *testing.T) {
	env := testutil.NewTestEnv(t)
	defer env.Server.Close()

	admin := bootstrapSysAdmin(t, env, "conceptunauth@example.com")
	_, _, _ = createRepositoryWithDB(t, admin, "ConceptUnauth", "concept-unauth", "desc", "")

	// No login; hit the endpoint directly via an anonymous client.
	anon := newAuthClient(env.BaseURL)
	resp, _ := anon.do("GET", "/api/v1/repositories/concept-unauth/concepts", nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("GET /concepts without auth: status %d, want 401", resp.StatusCode)
	}
}

// TestEmbeddedL3Source verifies the embedded context vocabulary
// loads and returns a non-empty class list. This is a unit-style
// test (no DB) but lives in the e2e package because it exercises
// the ontology package the wiring layer depends on.
func TestEmbeddedL3Source(t *testing.T) {
	src, err := ontology.NewEmbeddedL3Source()
	if err != nil {
		t.Fatalf("NewEmbeddedL3Source: %v", err)
	}
	classes, err := src.ContextClasses(context.Background())
	if err != nil {
		t.Fatalf("ContextClasses: %v", err)
	}
	if len(classes) == 0 {
		t.Fatal("embedded context vocabulary is empty")
	}
	// The vocabulary must contain a few known context categories so
	// the concept-extraction prompt has a recognizable vocabulary.
	// The labels are matched verbatim against the embedded
	// contexts.json file (see backend/internal/providers/ontology/
	// contexts.go).
	want := map[string]bool{"person": false, "chemical substance": false, "organisation": false}
	for _, c := range classes {
		key := strings.ToLower(c.Label)
		if _, ok := want[key]; ok {
			want[key] = true
		}
	}
	for k, found := range want {
		if !found {
			t.Errorf("embedded context vocabulary missing expected class %q", k)
		}
	}
}

// countingConceptExtractor wraps a function so the test can return a
// different concept list per fact (the first fact creates the
// concept, the second matches it) and count how many times the
// extractor was invoked per batch call. The fn receives the
// 0-based fact index within the batch and returns that fact's
// concepts (already tagged with FactIndex by the stub).
type countingConceptExtractor struct {
	calls int
	fn    func(factIndex int) []decomposition.ExtractedConcept
}

func (c *countingConceptExtractor) ExtractConcepts(_ context.Context, _ store.DBTX, facts []decomposition.FactInput, _ []decomposition.ContextEntry, _ decomposition.ConceptExtractionAttribution) ([]decomposition.ExtractedConcept, error) {
	c.calls++
	var out []decomposition.ExtractedConcept
	for _, f := range facts {
		for _, concept := range c.fn(f.Index) {
			concept.FactIndex = f.Index
			out = append(out, concept)
		}
	}
	return out, nil
}

func (c *countingConceptExtractor) Describe() decomposition.ProviderDescription {
	return decomposition.ProviderDescription{Name: "counting-concept-extractor", Configured: true}
}

// guard against unused imports when the file is edited.
var _ = dbpool.Pool{}
var _ = errors.New

// TestExtractConcepts_AlreadyLinkedFactIsNoop verifies that a
// second pass over a fact that is already linked to a concept is
// a no-op: the worker's AddFactConcept ON CONFLICT DO NOTHING
// returns pgx.ErrNoRows, and the worker treats that as success
// (not as a fatal error that inflates result.Errors). This is the
// regression guard for the bug where a duplicate (fact, concept)
// pair surfaced pgx.ErrNoRows as "add fact_concept (match): no
// rows in result set" and flooded the error log.
func TestExtractConcepts_AlreadyLinkedFactIsNoop(t *testing.T) {
	env := testutil.NewTestEnv(t)
	defer env.Server.Close()
	ensureRiverSchema(t, env.DB)

	admin := bootstrapSysAdmin(t, env, "already-linked@example.com")
	_, _, repoID := createRepositoryWithDB(t, admin, "AlreadyLinked", "already-linked", "desc", "")
	queries := store.New(env.DB)

	srcID := pgtype.UUID{}
	if err := srcID.Scan(uuid.NewString()); err != nil {
		t.Fatalf("scanning source id: %v", err)
	}
	if _, err := queries.CreateSource(context.Background(), store.CreateSourceParams{
		ID: srcID, RepositoryID: pgRepoID(t, repoID), Url: "https://example.com/x", Kind: "homepage", Status: "fetched",
	}); err != nil {
		t.Fatalf("create source: %v", err)
	}
	factID := insertFactWithSource(t, env, pgRepoID(t, repoID), srcID, "A fact about nucleotides.", 0)
	if _, err := env.DB.Exec(context.Background(),
		`UPDATE okt_repository.facts SET status = 'stable' WHERE id = $1`, factID,
	); err != nil {
		t.Fatalf("promote fact: %v", err)
	}

	extractor := &stubConceptExtractor{concepts: []decomposition.ExtractedConcept{
		{Concept: "nucleotides", Context: "Molecule", SeedAliases: []string{"nucleotide"}},
	}}
	conceptCfg := config.DecompositionConceptConfig{Enabled: true}
	registry := testutil.NewForTestPool(env.DB)
	systemQueries := store.New(env.DB)
	worker := tasks.NewExtractConceptsWorker(extractor, conceptCfg, registry, systemQueries, nil, nil)

	driver := riverpgxv5.New(env.DB)
	workers := river.NewWorkers()
	river.AddWorker(workers, worker)
	testWorker := rivertest.NewWorker(t, driver, &river.Config{
		Queues:  map[string]river.QueueConfig{tasks.QueueExtractConcepts: {MaxWorkers: 1}},
		Workers: workers,
	}, worker)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// First pass: creates the concept + links the fact.
	tx1, err := env.DB.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	job1, err := testWorker.Work(ctx, t, tx1, tasks.ExtractConceptsArgs{RepositoryID: repoID}, &river.InsertOpts{Queue: tasks.QueueExtractConcepts})
	if err != nil {
		tx1.Rollback(context.Background())
		t.Fatalf("first pass: %v", err)
	}
	if job1.EventKind != river.EventKindJobCompleted {
		tx1.Rollback(context.Background())
		t.Fatalf("first pass: expected completed, got %s", job1.EventKind)
	}
	if err := tx1.Commit(ctx); err != nil {
		t.Fatalf("commit first pass: %v", err)
	}

	// The fact is now linked. Re-run the worker; the candidate
	// list is empty (NOT EXISTS fact_concepts + NOT EXISTS
	// fact_candidates), so the second pass must process 0 facts and record 0 errors. The
	// already-linked fact is not re-processed.
	tx2, err := env.DB.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		t.Fatalf("begin tx2: %v", err)
	}
	defer tx2.Rollback(context.Background())
	job2, err := testWorker.Work(ctx, t, tx2, tasks.ExtractConceptsArgs{RepositoryID: repoID}, &river.InsertOpts{Queue: tasks.QueueExtractConcepts})
	if err != nil {
		t.Fatalf("second pass: %v", err)
	}
	if job2.EventKind != river.EventKindJobCompleted {
		t.Fatalf("second pass: expected completed, got %s", job2.EventKind)
	}

	// Decode the recorded output to confirm Errors==0. The output
	// is a JSON-encoded ExtractConceptsResult.
	var result tasks.ExtractConceptsResult
	if raw := job2.Job.Output(); len(raw) > 0 {
		if err := json.Unmarshal(raw, &result); err != nil {
			t.Fatalf("decoding output: %v", err)
		}
	}
	if result.Errors != 0 {
		t.Errorf("second pass errors = %d, want 0 (already-linked fact must be a silent no-op)", result.Errors)
	}
	if result.FactsProcessed != 0 {
		t.Errorf("second pass facts_processed = %d, want 0 (candidate list excludes linked facts)", result.FactsProcessed)
	}
}

// TestExtractConcepts_CreateConceptConflictIsRecovered verifies
// that the worker recovers from a CreateConcept ON CONFLICT DO
// NOTHING by re-fetching the survivor and linking the fact to it
// (instead of bailing out with "create concept: no rows in result
// set"). This is the regression guard for the bug where two
// extract_concepts passes racing on the same (repo, name, context)
// triple — or a single pass whose FindConceptByAlias missed because
// the alias had not been inserted yet but a previous pass had
// already created the concept — caused the worker to spin forever
// on the same facts (the fact_concept link was never written, so
// ListStableFactsForConceptExtraction kept returning them), logging
// "create concept: no rows in result set" on every attempt.
func TestExtractConcepts_CreateConceptConflictIsRecovered(t *testing.T) {
	env := testutil.NewTestEnv(t)
	defer env.Server.Close()
	ensureRiverSchema(t, env.DB)

	admin := bootstrapSysAdmin(t, env, "conflict@example.com")
	_, _, repoID := createRepositoryWithDB(t, admin, "Conflict", "conflict", "desc", "")
	queries := store.New(env.DB)

	srcID := pgtype.UUID{}
	if err := srcID.Scan(uuid.NewString()); err != nil {
		t.Fatalf("scan source id: %v", err)
	}
	if _, err := queries.CreateSource(context.Background(), store.CreateSourceParams{
		ID: srcID, RepositoryID: pgRepoID(t, repoID), Url: "https://example.com/conflict", Kind: "homepage", Status: "fetched",
	}); err != nil {
		t.Fatalf("create source: %v", err)
	}
	factID := insertFactWithSource(t, env, pgRepoID(t, repoID), srcID, "A fact about insulin.", 0)
	if _, err := env.DB.Exec(context.Background(),
		`UPDATE okt_repository.facts SET status = 'stable' WHERE id = $1`, factID,
	); err != nil {
		t.Fatalf("promote fact: %v", err)
	}

	// Under the Option B architecture, extract_concepts no longer
	// routes — it only emits a keyword list by creating (or reusing)
	// a concept_candidate and linking the fact via fact_candidates.
	// Routing (alias match, canonical match, concept creation) lives
	// in refine_concepts. So this test now asserts the candidate
	// emission path: extract must create a candidate for
	// (concept_text="insulin", context="Biomolecule") and link the
	// fact via fact_candidates, with 0 errors, even when a candidate
	// for the same (concept_text, context) already exists (the ON
	// CONFLICT DO NOTHING + re-fetch path).
	//
	// Pre-insert an unresolved candidate for the same
	// (concept_text, context) so the worker's CreateCandidate hits
	// the ON CONFLICT and must re-fetch. This is the analogous
	// regression guard to the old CreateConcept-conflict test.
	pgRepo := pgRepoID(t, repoID)
	if _, err := queries.CreateCandidate(context.Background(), store.CreateCandidateParams{
		RepositoryID: pgRepo,
		ConceptText:  "insulin",
		Context:      "Biomolecule",
		SeedAliases:  []string{"insulin"},
	}); err != nil {
		t.Fatalf("seed candidate: %v", err)
	}

	extractor := &stubConceptExtractor{concepts: []decomposition.ExtractedConcept{
		{Concept: "insulin", Context: "Biomolecule", SeedAliases: []string{"insulin"}},
	}}
	conceptCfg := config.DecompositionConceptConfig{Enabled: true}
	registry := testutil.NewForTestPool(env.DB)
	systemQueries := store.New(env.DB)
	worker := tasks.NewExtractConceptsWorker(extractor, conceptCfg, registry, systemQueries, nil, nil)
	// Candidate-emission path (refinement enabled): extract creates
	// candidates, defers routing to refine_concepts.
	worker.SetRefinementEnabled(true)

	driver := riverpgxv5.New(env.DB)
	workers := river.NewWorkers()
	river.AddWorker(workers, worker)
	testWorker := rivertest.NewWorker(t, driver, &river.Config{
		Queues:  map[string]river.QueueConfig{tasks.QueueExtractConcepts: {MaxWorkers: 1}},
		Workers: workers,
	}, worker)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	tx, err := env.DB.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer tx.Rollback(context.Background())
	job, err := testWorker.Work(ctx, t, tx, tasks.ExtractConceptsArgs{RepositoryID: repoID}, &river.InsertOpts{Queue: tasks.QueueExtractConcepts})
	if err != nil {
		t.Fatalf("work: %v", err)
	}
	if job.EventKind != river.EventKindJobCompleted {
		t.Fatalf("expected completed, got %s", job.EventKind)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}

	// The worker must have linked the fact to the candidate via
	// fact_candidates (no direct fact_concepts link — that's
	// refine's job now).
	var linked int
	if err := env.DB.QueryRow(context.Background(),
		`SELECT count(*) FROM okt_repository.fact_candidates WHERE fact_id = $1`, factID,
	).Scan(&linked); err != nil {
		t.Fatalf("query fact_candidates: %v", err)
	}
	if linked == 0 {
		t.Fatal("fact was not linked to a candidate (CreateCandidate conflict was not recovered)")
	}

	// And there must be NO direct fact_concepts link (routing is
	// refine's responsibility).
	var directLinked int
	if err := env.DB.QueryRow(context.Background(),
		`SELECT count(*) FROM okt_repository.fact_concepts WHERE fact_id = $1`, factID,
	).Scan(&directLinked); err != nil {
		t.Fatalf("query fact_concepts: %v", err)
	}
	if directLinked != 0 {
		t.Errorf("fact_concepts link count = %d, want 0 (extract must not route directly)", directLinked)
	}

	var result tasks.ExtractConceptsResult
	if raw := job.Job.Output(); len(raw) > 0 {
		if err := json.Unmarshal(raw, &result); err != nil {
			t.Fatalf("decoding output: %v", err)
		}
	}
	if result.Errors != 0 {
		t.Errorf("errors = %d, want 0 (CreateCandidate conflict must be recovered, not counted as an error)", result.Errors)
	}
	if result.FactsProcessed != 1 {
		t.Errorf("facts_processed = %d, want 1", result.FactsProcessed)
	}
}

// TestExtractConcepts_DedupsMultiSourceFacts verifies that
// ListStableFactsForConceptExtraction returns each fact at most
// once even when the fact has multiple source rows. The previous
// JOIN shape returned the same fact once per source, causing the
// worker to invoke the LLM N times for one fact and then hit the
// fact_concepts unique index on the second insert.
func TestExtractConcepts_DedupsMultiSourceFacts(t *testing.T) {
	env := testutil.NewTestEnv(t)
	defer env.Server.Close()
	ensureRiverSchema(t, env.DB)

	admin := bootstrapSysAdmin(t, env, "multisrc@example.com")
	_, _, repoID := createRepositoryWithDB(t, admin, "MultiSrc", "multi-src", "desc", "")
	queries := store.New(env.DB)

	// Two sources in the same repo.
	src1 := pgtype.UUID{}
	src2 := pgtype.UUID{}
	if err := src1.Scan(uuid.NewString()); err != nil {
		t.Fatalf("scan src1: %v", err)
	}
	if err := src2.Scan(uuid.NewString()); err != nil {
		t.Fatalf("scan src2: %v", err)
	}
	for _, s := range []pgtype.UUID{src1, src2} {
		if _, err := queries.CreateSource(context.Background(), store.CreateSourceParams{
			ID: s, RepositoryID: pgRepoID(t, repoID), Url: "https://example.com/" + pgUUIDString(s), Kind: "homepage", Status: "fetched",
		}); err != nil {
			t.Fatalf("create source: %v", err)
		}
	}

	// One fact linked to both sources (the dedup-critical case).
	factIDStr := insertFactWithSource(t, env, pgRepoID(t, repoID), src1, "A shared fact about molecules.", 0)
	factID := pgtype.UUID{}
	if err := factID.Scan(factIDStr); err != nil {
		t.Fatalf("scan fact id: %v", err)
	}
	if err := queries.AddFactSource(context.Background(), store.AddFactSourceParams{
		FactID: factID, SourceID: src2, ChunkIndex: 1,
	}); err != nil {
		t.Fatalf("add second fact source: %v", err)
	}
	if _, err := env.DB.Exec(context.Background(),
		`UPDATE okt_repository.facts SET status = 'stable' WHERE id = $1`, factID,
	); err != nil {
		t.Fatalf("promote fact: %v", err)
	}

	// The extractor counts how many times it was called. The
	// worker must invoke it exactly once for the one shared fact.
	extractor := &countingConceptExtractor{fn: func(factIndex int) []decomposition.ExtractedConcept {
		return []decomposition.ExtractedConcept{
			{Concept: "molecules", Context: "Molecule", SeedAliases: []string{"molecule"}},
		}
	}}
	conceptCfg := config.DecompositionConceptConfig{Enabled: true}
	registry := testutil.NewForTestPool(env.DB)
	systemQueries := store.New(env.DB)
	worker := tasks.NewExtractConceptsWorker(extractor, conceptCfg, registry, systemQueries, nil, nil)
	// Candidate-emission path (refinement enabled): extract creates
	// candidates, defers routing to refine_concepts.
	worker.SetRefinementEnabled(true)

	driver := riverpgxv5.New(env.DB)
	workers := river.NewWorkers()
	river.AddWorker(workers, worker)
	testWorker := rivertest.NewWorker(t, driver, &river.Config{
		Queues:  map[string]river.QueueConfig{tasks.QueueExtractConcepts: {MaxWorkers: 1}},
		Workers: workers,
	}, worker)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	tx, err := env.DB.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	job, err := testWorker.Work(ctx, t, tx, tasks.ExtractConceptsArgs{RepositoryID: repoID}, &river.InsertOpts{Queue: tasks.QueueExtractConcepts})
	if err != nil {
		tx.Rollback(context.Background())
		t.Fatalf("work: %v", err)
	}
	if job.EventKind != river.EventKindJobCompleted {
		tx.Rollback(context.Background())
		t.Fatalf("expected completed, got %s", job.EventKind)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}

	if extractor.calls != 1 {
		t.Errorf("extractor calls = %d, want 1 (multi-source fact must be processed once)", extractor.calls)
	}

	// Confirm exactly one fact_candidates row exists for this fact.
	var fcCount int
	if err := env.DB.QueryRow(ctx,
		`SELECT count(*) FROM okt_repository.fact_candidates WHERE fact_id = $1`, factID,
	).Scan(&fcCount); err != nil {
		t.Fatalf("query fact_candidates: %v", err)
	}
	if fcCount != 1 {
		t.Errorf("fact_candidates rows for shared fact = %d, want 1", fcCount)
	}
}

// TestExtractConcepts_LLMFailureMarksSkip verifies that a
// transient LLM failure (timeout, 5xx, parse error) writes a
// permanent fact_concept_skips row for the failing fact so the
// next pass doesn't retry it forever (there is no periodic
// re-driver). The skip is recorded even though the worker
// continues processing other facts in the batch.
func TestExtractConcepts_LLMFailureMarksSkip(t *testing.T) {
	env := testutil.NewTestEnv(t)
	defer env.Server.Close()
	ensureRiverSchema(t, env.DB)

	admin := bootstrapSysAdmin(t, env, "llmskip@example.com")
	_, _, repoID := createRepositoryWithDB(t, admin, "LLMSkip", "llm-skip", "desc", "")
	queries := store.New(env.DB)

	srcID := pgtype.UUID{}
	if err := srcID.Scan(uuid.NewString()); err != nil {
		t.Fatalf("scan src id: %v", err)
	}
	if _, err := queries.CreateSource(context.Background(), store.CreateSourceParams{
		ID: srcID, RepositoryID: pgRepoID(t, repoID), Url: "https://example.com/s", Kind: "homepage", Status: "fetched",
	}); err != nil {
		t.Fatalf("create source: %v", err)
	}
	factIDStr := insertFactWithSource(t, env, pgRepoID(t, repoID), srcID, "A fact that will fail extraction.", 0)
	factID := pgtype.UUID{}
	if err := factID.Scan(factIDStr); err != nil {
		t.Fatalf("scan fact id: %v", err)
	}
	if _, err := env.DB.Exec(context.Background(),
		`UPDATE okt_repository.facts SET status = 'stable' WHERE id = $1`, factID,
	); err != nil {
		t.Fatalf("promote fact: %v", err)
	}

	// An extractor that always fails (simulating an OpenRouter
	// timeout). The worker must record a skip row and continue.
	failingExtractor := &failingConceptExtractor{err: errors.New("upstream LLM timeout")}
	conceptCfg := config.DecompositionConceptConfig{Enabled: true}
	registry := testutil.NewForTestPool(env.DB)
	systemQueries := store.New(env.DB)
	worker := tasks.NewExtractConceptsWorker(failingExtractor, conceptCfg, registry, systemQueries, nil, nil)

	driver := riverpgxv5.New(env.DB)
	workers := river.NewWorkers()
	river.AddWorker(workers, worker)
	testWorker := rivertest.NewWorker(t, driver, &river.Config{
		Queues:  map[string]river.QueueConfig{tasks.QueueExtractConcepts: {MaxWorkers: 1}},
		Workers: workers,
	}, worker)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	tx, err := env.DB.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	job, err := testWorker.Work(ctx, t, tx, tasks.ExtractConceptsArgs{RepositoryID: repoID}, &river.InsertOpts{Queue: tasks.QueueExtractConcepts})
	if err != nil {
		tx.Rollback(context.Background())
		t.Fatalf("work: %v", err)
	}
	if job.EventKind != river.EventKindJobCompleted {
		tx.Rollback(context.Background())
		t.Fatalf("expected completed (LLM failure is not a fatal job error), got %s", job.EventKind)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}

	// The skip row must exist.
	var skipCount int
	if err := env.DB.QueryRow(ctx,
		`SELECT count(*) FROM okt_repository.fact_concept_skips WHERE fact_id = $1`, factID,
	).Scan(&skipCount); err != nil {
		t.Fatalf("query skip row: %v", err)
	}
	if skipCount != 1 {
		t.Errorf("skip rows for failing fact = %d, want 1", skipCount)
	}

	// The worker must have recorded one error in its result.
	var result tasks.ExtractConceptsResult
	if raw := job.Job.Output(); len(raw) > 0 {
		if err := json.Unmarshal(raw, &result); err != nil {
			t.Fatalf("decode output: %v", err)
		}
	}
	if result.Errors != 1 {
		t.Errorf("result.Errors = %d, want 1", result.Errors)
	}
	if result.FactsProcessed != 1 {
		t.Errorf("result.FactsProcessed = %d, want 1", result.FactsProcessed)
	}

	// Re-run: the candidate list now excludes the skipped fact
	// (NOT EXISTS fact_concept_skips), so the second pass is a
	// zero-fact no-op even though the fact is still unlinked.
	tx2, err := env.DB.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		t.Fatalf("begin tx2: %v", err)
	}
	defer tx2.Rollback(context.Background())
	job2, err := testWorker.Work(ctx, t, tx2, tasks.ExtractConceptsArgs{RepositoryID: repoID}, &river.InsertOpts{Queue: tasks.QueueExtractConcepts})
	if err != nil {
		t.Fatalf("second work: %v", err)
	}
	if job2.EventKind != river.EventKindJobCompleted {
		t.Fatalf("second pass: expected completed, got %s", job2.EventKind)
	}
	var result2 tasks.ExtractConceptsResult
	if raw := job2.Job.Output(); len(raw) > 0 {
		if err := json.Unmarshal(raw, &result2); err != nil {
			t.Fatalf("decode output2: %v", err)
		}
	}
	if result2.FactsProcessed != 0 {
		t.Errorf("second pass FactsProcessed = %d, want 0 (skipped fact must be excluded)", result2.FactsProcessed)
	}
}

// failingConceptExtractor is a stub that always returns an error,
// simulating a transient LLM provider failure.
type failingConceptExtractor struct {
	err error
}

func (f *failingConceptExtractor) ExtractConcepts(_ context.Context, _ store.DBTX, _ []decomposition.FactInput, _ []decomposition.ContextEntry, _ decomposition.ConceptExtractionAttribution) ([]decomposition.ExtractedConcept, error) {
	return nil, f.err
}

func (f *failingConceptExtractor) Describe() decomposition.ProviderDescription {
	return decomposition.ProviderDescription{Name: "failing-concept-extractor", Configured: true}
}

// TestExtractConcepts_SourceScoped is the regression guard for the
// overlap bug: before source-scoping, extract_concepts re-scanned
// the whole repo's stable unlinked facts every time any source
// completed processing, so concurrent source passes raced and
// reprocessed facts from other sources. With SourceID propagated
// through the chain, a source-scoped extract_concepts pass must
// only process stable facts linked to THAT source and must not
// touch facts linked only to a different source.
//
// Setup: one repo, two sources, one stable fact per source. Run
// extract_concepts with SourceID = src1. Assert:
//   - only fact1 (linked to src1) is processed (extractor called once),
//   - fact2 (linked to src2, stable, unlinked to any concept) is
//     NOT processed (extractor not called for it),
//   - a repo-wide pass (SourceID empty) still picks up fact2.
func TestExtractConcepts_SourceScoped(t *testing.T) {
	env := testutil.NewTestEnv(t)
	defer env.Server.Close()
	ensureRiverSchema(t, env.DB)

	admin := bootstrapSysAdmin(t, env, "srcscoped@example.com")
	_, _, repoID := createRepositoryWithDB(t, admin, "SrcScoped", "src-scoped", "desc", "")
	queries := store.New(env.DB)

	// Two sources in the repo.
	src1 := pgtype.UUID{}
	src2 := pgtype.UUID{}
	if err := src1.Scan(uuid.NewString()); err != nil {
		t.Fatalf("scan src1: %v", err)
	}
	if err := src2.Scan(uuid.NewString()); err != nil {
		t.Fatalf("scan src2: %v", err)
	}
	for _, s := range []pgtype.UUID{src1, src2} {
		if _, err := queries.CreateSource(context.Background(), store.CreateSourceParams{
			ID: s, RepositoryID: pgRepoID(t, repoID), Url: "https://example.com/" + pgUUIDString(s), Kind: "homepage", Status: "fetched",
		}); err != nil {
			t.Fatalf("create source: %v", err)
		}
	}

	// One stable fact per source.
	fact1ID := insertFactWithSource(t, env, pgRepoID(t, repoID), src1, "A fact about DNA from source 1.", 0)
	fact2ID := insertFactWithSource(t, env, pgRepoID(t, repoID), src2, "A fact about insulin from source 2.", 0)
	f1 := pgtype.UUID{}
	if err := f1.Scan(fact1ID); err != nil {
		t.Fatalf("scan fact1: %v", err)
	}
	f2 := pgtype.UUID{}
	if err := f2.Scan(fact2ID); err != nil {
		t.Fatalf("scan fact2: %v", err)
	}
	if _, err := env.DB.Exec(context.Background(),
		`UPDATE okt_repository.facts SET status = 'stable' WHERE id IN ($1, $2)`, f1, f2,
	); err != nil {
		t.Fatalf("promote facts to stable: %v", err)
	}

	// Extractor counts calls so we can assert it was invoked only
	// for fact1 (src1's fact) during the source-scoped pass.
	callCount := 0
	extractor := &countingConceptExtractor{fn: func(factIndex int) []decomposition.ExtractedConcept {
		callCount++
		return []decomposition.ExtractedConcept{
			{Concept: "molecule", Context: "Biomolecule", SeedAliases: []string{"molecule"}},
		}
	}}
	conceptCfg := config.DecompositionConceptConfig{Enabled: true}
	registry := testutil.NewForTestPool(env.DB)
	systemQueries := store.New(env.DB)
	worker := tasks.NewExtractConceptsWorker(extractor, conceptCfg, registry, systemQueries, nil, nil)
	// Candidate-emission path (refinement enabled): extract creates
	// candidates, defers routing to refine_concepts.
	worker.SetRefinementEnabled(true)

	driver := riverpgxv5.New(env.DB)
	workers := river.NewWorkers()
	river.AddWorker(workers, worker)
	testWorker := rivertest.NewWorker(t, driver, &river.Config{
		Queues:  map[string]river.QueueConfig{tasks.QueueExtractConcepts: {MaxWorkers: 1}},
		Workers: workers,
	}, worker)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Source-scoped pass for src1: must process only fact1.
	tx, err := env.DB.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	job, err := testWorker.Work(ctx, t, tx, tasks.ExtractConceptsArgs{RepositoryID: repoID, SourceID: pgUUIDString(src1)}, &river.InsertOpts{Queue: tasks.QueueExtractConcepts})
	if err != nil {
		tx.Rollback(context.Background())
		t.Fatalf("source-scoped work: %v", err)
	}
	if job.EventKind != river.EventKindJobCompleted {
		tx.Rollback(context.Background())
		t.Fatalf("source-scoped: expected completed, got %s", job.EventKind)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}

	if callCount != 1 {
		t.Errorf("source-scoped pass extractor calls = %d, want 1 (only src1's fact1 must be processed)", callCount)
	}

	// fact1 must be linked to a concept; fact2 must NOT (it belongs
	// to src2 and was out of the source-scoped candidate set).
	var linked1, linked2 int
	if err := env.DB.QueryRow(ctx,
		`SELECT count(*) FROM okt_repository.fact_candidates WHERE fact_id = $1`, f1,
	).Scan(&linked1); err != nil {
		t.Fatalf("query fact1 candidates: %v", err)
	}
	if err := env.DB.QueryRow(ctx,
		`SELECT count(*) FROM okt_repository.fact_candidates WHERE fact_id = $1`, f2,
	).Scan(&linked2); err != nil {
		t.Fatalf("query fact2 candidates: %v", err)
	}
	if linked1 != 1 {
		t.Errorf("fact1 (src1) fact_candidates = %d, want 1 (source-scoped pass must link it)", linked1)
	}
	if linked2 != 0 {
		t.Errorf("fact2 (src2) fact_candidates = %d, want 0 (source-scoped pass for src1 must not touch src2's facts)", linked2)
	}

	// Repo-wide pass (SourceID empty) must now pick up fact2.
	callCount = 0
	tx2, err := env.DB.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		t.Fatalf("begin tx2: %v", err)
	}
	job2, err := testWorker.Work(ctx, t, tx2, tasks.ExtractConceptsArgs{RepositoryID: repoID}, &river.InsertOpts{Queue: tasks.QueueExtractConcepts})
	if err != nil {
		tx2.Rollback(context.Background())
		t.Fatalf("repo-wide work: %v", err)
	}
	if job2.EventKind != river.EventKindJobCompleted {
		tx2.Rollback(context.Background())
		t.Fatalf("repo-wide: expected completed, got %s", job2.EventKind)
	}
	if err := tx2.Commit(ctx); err != nil {
		t.Fatalf("commit tx2: %v", err)
	}

	if callCount != 1 {
		t.Errorf("repo-wide pass extractor calls = %d, want 1 (only fact2 remains unlinked)", callCount)
	}
	if err := env.DB.QueryRow(ctx,
		`SELECT count(*) FROM okt_repository.fact_candidates WHERE fact_id = $1`, f2,
	).Scan(&linked2); err != nil {
		t.Fatalf("query fact2 candidates after repo-wide: %v", err)
	}
	if linked2 != 1 {
		t.Errorf("fact2 (src2) fact_candidates after repo-wide pass = %d, want 1", linked2)
	}
}

// conceptGroupRow is the minimal decode of a grouped concept list
// entry for the grouping tests below. The backend groups per-
// context rows by lower(canonical_name) into one entry per
// canonical name, with a `contexts` array of per-context slices.
type conceptGroupRow struct {
	CanonicalName  string `json:"canonical_name"`
	TotalFactCount int64  `json:"total_fact_count"`
}

// conceptGroupDetail is the minimal decode of the grouped detail
// response (GET /concepts/{conceptID}). It extends conceptGroupRow
// with the full contexts array (each context entry has its own
// concept_id, context label, fact_count, and aliases).
type conceptGroupDetail struct {
	CanonicalName  string             `json:"canonical_name"`
	TotalFactCount int64              `json:"total_fact_count"`
	Contexts       []conceptContext   `json:"contexts"`
}

type conceptContext struct {
	ConceptID string   `json:"concept_id"`
	Context   string   `json:"context"`
	FactCount int64    `json:"fact_count"`
	Aliases   []string `json:"aliases"`
}

// TestConcepts_GroupedListCollapsesContexts verifies the API-level
// unification: two concept rows with the same canonical_name but
// different contexts collapse into one list entry with two contexts
// and a total_fact_count summed across contexts. Also covers
// case-insensitive grouping ("Trump" + "trump" different contexts
// collapse) and the ?q= substring search.
func TestConcepts_GroupedListCollapsesContexts(t *testing.T) {
	env := testutil.NewTestEnv(t)
	defer env.Server.Close()

	admin := bootstrapSysAdmin(t, env, "grouped@example.com")
	const slug = "grouped-repo"
	_, _, repoID := createRepositoryWithDB(t, admin, "Grouped Repo", slug, "desc", "")
	pgRepo := pgRepoID(t, repoID)
	queries := store.New(env.DB)

	// Two contexts for the same canonical name ("Trump") — these
	// must collapse into one group with two contexts.
	// Politician gets 3 facts; Politician + "Person" gets 1 fact.
	mkConceptWithFacts := func(name, contextLabel string, nFacts int, srcID pgtype.UUID) pgtype.UUID {
		ctx := context.Background()
		c, err := queries.CreateConcept(ctx, store.CreateConceptParams{
			RepositoryID: pgRepo, CanonicalName: name, Context: contextLabel,
		})
		if err != nil {
			t.Fatalf("create concept %s/%s: %v", name, contextLabel, err)
		}
		for i := 0; i < nFacts; i++ {
			fidStr := insertFactWithSource(t, env, pgRepo, srcID, "A fact about "+name+" "+contextLabel, int32(i))
			fid := pgtype.UUID{}
			if err := fid.Scan(fidStr); err != nil {
				t.Fatalf("scan fact id: %v", err)
			}
			if _, err := queries.AddFactConcept(ctx, store.AddFactConceptParams{
				FactID: fid, ConceptID: c.ID,
			}); err != nil {
				t.Fatalf("link fact→concept: %v", err)
			}
		}
		return c.ID
	}

	srcID := pgtype.UUID{}
	if err := srcID.Scan(uuid.NewString()); err != nil {
		t.Fatalf("scan src id: %v", err)
	}
	if _, err := queries.CreateSource(context.Background(), store.CreateSourceParams{
		ID: srcID, RepositoryID: pgRepo, Url: "https://example.com/grouped", Kind: "homepage", Status: "fetched",
	}); err != nil {
		t.Fatalf("create source: %v", err)
	}

	mkConceptWithFacts("Trump", "Politician", 3, srcID)
	// Different casing of the same name under a different context
	// must collapse into the same group (case-insensitive grouping).
	mkConceptWithFacts("trump", "Person", 1, srcID)

	// A second, unrelated concept to verify the list returns
	// multiple groups.
	mkConceptWithFacts("DNA", "Biomolecule", 2, srcID)

	// GET /concepts returns 2 groups (Trump, DNA), not 3 rows.
	listResp, listRaw := admin.do("GET", "/api/v1/repositories/"+slug+"/concepts", nil)
	if listResp.StatusCode != http.StatusOK {
		t.Fatalf("GET /concepts: %d %s", listResp.StatusCode, listRaw)
	}
	var list pageEnvelope
	if err := json.Unmarshal(listRaw, &list); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if list.Total != 2 {
		t.Errorf("grouped list total = %d, want 2 (Trump + DNA groups)", list.Total)
	}

	// Decode each Data element into a conceptGroupRow.
	groups := make([]conceptGroupRow, 0, len(list.Data))
	for _, raw := range list.Data {
		var g conceptGroupRow
		if err := json.Unmarshal(raw, &g); err != nil {
			t.Fatalf("decode group row: %v", err)
		}
		groups = append(groups, g)
	}

	// The first group must be "Trump" (total fact_count 4 > DNA's 2,
	// so the backend orders it first by total_fact_count DESC).
	if len(groups) == 0 {
		t.Fatal("no groups returned")
	}
	if groups[0].CanonicalName != "Trump" {
		t.Errorf("first group canonical_name = %q, want %q", groups[0].CanonicalName, "Trump")
	}
	if groups[0].TotalFactCount != 4 {
		t.Errorf("Trump total_fact_count = %d, want 4 (3 Politician + 1 Person)", groups[0].TotalFactCount)
	}

	// ?q= substring search: "tru" matches Trump only.
	qResp, qRaw := admin.do("GET", "/api/v1/repositories/"+slug+"/concepts?q=tru", nil)
	if qResp.StatusCode != http.StatusOK {
		t.Fatalf("GET /concepts?q=tru: %d %s", qResp.StatusCode, qRaw)
	}
	var qList pageEnvelope
	if err := json.Unmarshal(qRaw, &qList); err != nil {
		t.Fatalf("decode q list: %v", err)
	}
	if qList.Total != 1 {
		t.Errorf("q=tru total = %d, want 1 (only Trump matches)", qList.Total)
	}
}

// TestConcepts_GetByIDReturnsGroupWithContexts verifies the primary
// detail endpoint GET /concepts/{conceptID} returns the whole group
// with every context entry populated, including per-context aliases.
// Also verifies per-context facts stay scoped to one concept_id
// (compartmentalized per context).
func TestConcepts_GetByIDReturnsGroupWithContexts(t *testing.T) {
	env := testutil.NewTestEnv(t)
	defer env.Server.Close()

	admin := bootstrapSysAdmin(t, env, "slugdetail@example.com")
	const slug = "slug-detail-repo"
	_, _, repoID := createRepositoryWithDB(t, admin, "Slug Detail Repo", slug, "desc", "")
	pgRepo := pgRepoID(t, repoID)
	queries := store.New(env.DB)

	srcID := pgtype.UUID{}
	if err := srcID.Scan(uuid.NewString()); err != nil {
		t.Fatalf("scan src id: %v", err)
	}
	if _, err := queries.CreateSource(context.Background(), store.CreateSourceParams{
		ID: srcID, RepositoryID: pgRepo, Url: "https://example.com/slug-detail", Kind: "homepage", Status: "fetched",
	}); err != nil {
		t.Fatalf("create source: %v", err)
	}

	// Two contexts for "DNA", each with its own facts and aliases.
	dnaBio, err := queries.CreateConcept(context.Background(), store.CreateConceptParams{
		RepositoryID: pgRepo, CanonicalName: "DNA", Context: "Biomolecule",
	})
	if err != nil {
		t.Fatalf("create DNA/Biomolecule: %v", err)
	}
	dnaMol, err := queries.CreateConcept(context.Background(), store.CreateConceptParams{
		RepositoryID: pgRepo, CanonicalName: "DNA", Context: "Molecule",
	})
	if err != nil {
		t.Fatalf("create DNA/Molecule: %v", err)
	}
	// Aliases per context.
	if _, err := queries.AddConceptAlias(context.Background(), store.AddConceptAliasParams{
		ConceptID: dnaBio.ID, AliasText: "deoxyribonucleic acid",
	}); err != nil {
		t.Fatalf("add alias: %v", err)
	}
	if _, err := queries.AddConceptAlias(context.Background(), store.AddConceptAliasParams{
		ConceptID: dnaMol.ID, AliasText: "double helix",
	}); err != nil {
		t.Fatalf("add alias: %v", err)
	}
	// Facts: 2 for Biomolecule, 1 for Molecule.
	for i, c := range []struct{ id pgtype.UUID; n int }{{dnaBio.ID, 2}, {dnaMol.ID, 1}} {
		for j := 0; j < c.n; j++ {
			fidStr := insertFactWithSource(t, env, pgRepo, srcID, "DNA fact "+c.id.String(), int32(i+j))
			fid := pgtype.UUID{}
			if err := fid.Scan(fidStr); err != nil {
				t.Fatalf("scan fid: %v", err)
			}
			if _, err := queries.AddFactConcept(context.Background(), store.AddFactConceptParams{
				FactID: fid, ConceptID: c.id,
			}); err != nil {
				t.Fatalf("link fact: %v", err)
			}
		}
	}

	// GET /concepts/{conceptID} (DNA's Biomolecule id) returns the
	// whole group (both DNA contexts) with aliases populated.
	dnaIDStr := pgUUIDString(dnaBio.ID)
	dResp, dRaw := admin.do("GET", "/api/v1/repositories/"+slug+"/concepts/"+dnaIDStr, nil)
	if dResp.StatusCode != http.StatusOK {
		t.Fatalf("GET by-id: %d %s", dResp.StatusCode, dRaw)
	}
	var detail conceptGroupDetail
	if err := json.Unmarshal(dRaw, &detail); err != nil {
		t.Fatalf("decode detail: %v", err)
	}
	if detail.CanonicalName != "DNA" {
		t.Errorf("detail canonical_name = %q, want DNA", detail.CanonicalName)
	}
	if detail.TotalFactCount != 3 {
		t.Errorf("detail total_fact_count = %d, want 3", detail.TotalFactCount)
	}
	if len(detail.Contexts) != 2 {
		t.Errorf("detail contexts len = %d, want 2", len(detail.Contexts))
	}

	// Each context must carry its own aliases.
	bioFound, molFound := false, false
	for _, ctx := range detail.Contexts {
		switch ctx.Context {
		case "Biomolecule":
			bioFound = true
			if ctx.FactCount != 2 {
				t.Errorf("Biomolecule fact_count = %d, want 2", ctx.FactCount)
			}
			if !containsString(ctx.Aliases, "deoxyribonucleic acid") {
				t.Errorf("Biomolecule aliases missing 'deoxyribonucleic acid': %v", ctx.Aliases)
			}
		case "Molecule":
			molFound = true
			if ctx.FactCount != 1 {
				t.Errorf("Molecule fact_count = %d, want 1", ctx.FactCount)
			}
			if !containsString(ctx.Aliases, "double helix") {
				t.Errorf("Molecule aliases missing 'double helix': %v", ctx.Aliases)
			}
		}
	}
	if !bioFound || !molFound {
		t.Errorf("detail contexts missing: bio=%v mol=%v (got %v)", bioFound, molFound, detail.Contexts)
	}

	// Per-context facts: GET /concepts/{conceptID}/facts for the
	// Biomolecule context must return only its 2 facts, not the
	// group's 3.
	bioIDStr := pgUUIDString(dnaBio.ID)
	fResp, fRaw := admin.do("GET", "/api/v1/repositories/"+slug+"/concepts/"+bioIDStr+"/facts", nil)
	if fResp.StatusCode != http.StatusOK {
		t.Fatalf("GET concept facts: %d %s", fResp.StatusCode, fRaw)
	}
	var fList pageEnvelope
	if err := json.Unmarshal(fRaw, &fList); err != nil {
		t.Fatalf("decode facts: %v", err)
	}
	if fList.Total != 2 {
		t.Errorf("Biomolecule concept facts total = %d, want 2 (per-context, not grouped)", fList.Total)
	}
}

// TestConcepts_ListFactsSearch verifies the optional `q` search
// param on GET /concepts/{conceptID}/facts narrows the per-context
// fact list via websearch_to_tsquery against facts.search_tsv. The
// search is server-side and total-aware, so it reaches facts beyond
// the current page — this is the backing query for the ContextPanel
// search bar.
func TestConcepts_ListFactsSearch(t *testing.T) {
	env := testutil.NewTestEnv(t)
	defer env.Server.Close()

	admin := bootstrapSysAdmin(t, env, "slugsearch@example.com")
	const slug = "slug-search-repo"
	_, _, repoID := createRepositoryWithDB(t, admin, "Slug Search Repo", slug, "desc", "")
	pgRepo := pgRepoID(t, repoID)
	queries := store.New(env.DB)

	srcID := pgtype.UUID{}
	if err := srcID.Scan(uuid.NewString()); err != nil {
		t.Fatalf("scan src id: %v", err)
	}
	if _, err := queries.CreateSource(context.Background(), store.CreateSourceParams{
		ID: srcID, RepositoryID: pgRepo, Url: "https://example.com/slug-search", Kind: "homepage", Status: "fetched",
	}); err != nil {
		t.Fatalf("create source: %v", err)
	}

	concept, err := queries.CreateConcept(context.Background(), store.CreateConceptParams{
		RepositoryID: pgRepo, CanonicalName: "Photosynthesis", Context: "Biology",
	})
	if err != nil {
		t.Fatalf("create concept: %v", err)
	}

	// Seed three facts under the concept with distinct searchable text.
	// websearch_to_tsquery stems, so "chlorophyll" matches
	// "chlorophyll" exactly and "sunlight" matches "sunlight".
	seedTexts := []string{
		"Plants convert sunlight into chemical energy during photosynthesis.",
		"Chlorophyll absorbs red and blue light but reflects green.",
		"Photosynthesis occurs in the chloroplasts of plant cells.",
	}
	factIDs := make([]string, 0, len(seedTexts))
	for _, txt := range seedTexts {
		fidStr := insertFactWithSource(t, env, pgRepo, srcID, txt, 0)
		fid := pgtype.UUID{}
		if err := fid.Scan(fidStr); err != nil {
			t.Fatalf("scan fid: %v", err)
		}
		if _, err := queries.AddFactConcept(context.Background(), store.AddFactConceptParams{
			FactID: fid, ConceptID: concept.ID,
		}); err != nil {
			t.Fatalf("link fact: %v", err)
		}
		factIDs = append(factIDs, fidStr)
	}

	conceptIDStr := pgUUIDString(concept.ID)
	listURL := func(q string) string {
		u := "/api/v1/repositories/" + slug + "/concepts/" + conceptIDStr + "/facts"
		if q != "" {
			u += "?q=" + q
		}
		return u
	}

	// No q → all 3 facts.
	resp, raw := admin.do("GET", listURL(""), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET concept facts (no q): %d %s", resp.StatusCode, raw)
	}
	var all pageEnvelope
	if err := json.Unmarshal(raw, &all); err != nil {
		t.Fatalf("decode facts: %v", err)
	}
	if all.Total != 3 {
		t.Errorf("no-q total = %d, want 3", all.Total)
	}

	// q="chlorophyll" → exactly 1 fact (the second seed). Confirms the
	// search narrows the total AND the returned data, not just the
	// visible page subset.
	resp, raw = admin.do("GET", listURL("chlorophyll"), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET concept facts (q=chlorophyll): %d %s", resp.StatusCode, raw)
	}
	var one pageEnvelope
	if err := json.Unmarshal(raw, &one); err != nil {
		t.Fatalf("decode facts: %v", err)
	}
	if one.Total != 1 {
		t.Errorf("q=chlorophyll total = %d, want 1", one.Total)
	}
	if len(one.Data) != 1 {
		t.Fatalf("q=chlorophyll data len = %d, want 1", len(one.Data))
	}
	var hit struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(one.Data[0], &hit); err != nil {
		t.Fatalf("decode hit: %v", err)
	}
	if !strings.Contains(hit.Text, "Chlorophyll") {
		t.Errorf("q=chlorophyll hit text = %q, want the chlorophyll fact", hit.Text)
	}

	// q="photosynthesis" → stems to "photosynth" which matches the two
	// facts that contain the word "Photosynthesis". Confirms the
	// search is full-text (stemmed), not a literal substring match.
	resp, raw = admin.do("GET", listURL("photosynthesis"), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET concept facts (q=photosynthesis): %d %s", resp.StatusCode, raw)
	}
	var two pageEnvelope
	if err := json.Unmarshal(raw, &two); err != nil {
		t.Fatalf("decode facts: %v", err)
	}
	if two.Total != 2 {
		t.Errorf("q=photosynthesis total = %d, want 2", two.Total)
	}

	// q for a term not in any fact → total 0, empty data, 200 OK
	// (not an error — empty result is a valid search outcome).
	resp, raw = admin.do("GET", listURL("zzzznomatch"), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET concept facts (q=zzzznomatch): %d %s", resp.StatusCode, raw)
	}
	var none pageEnvelope
	if err := json.Unmarshal(raw, &none); err != nil {
		t.Fatalf("decode facts: %v", err)
	}
	if none.Total != 0 {
		t.Errorf("q=zzzznomatch total = %d, want 0", none.Total)
	}
	if len(none.Data) != 0 {
		t.Errorf("q=zzzznomatch data len = %d, want 0", len(none.Data))
	}

	_ = factIDs
}

// TestConcepts_GetByIDNotFound verifies a conceptID with no matching
// concept in the repo returns 404 (cross-repo isolation: a conceptID
// from repo A is invisible from repo B because the ownership check
// is scoped by repository_id).
func TestConcepts_GetByIDNotFound(t *testing.T) {
	env := testutil.NewTestEnv(t)
	defer env.Server.Close()

	admin := bootstrapSysAdmin(t, env, "slug404@example.com")
	const slug = "slug-404-repo"
	_, _, _ = createRepositoryWithDB(t, admin, "Slug 404 Repo", slug, "desc", "")

	// A random UUID that doesn't exist in the repo.
	nonexistentID := uuid.NewString()
	resp, _ := admin.do("GET", "/api/v1/repositories/"+slug+"/concepts/"+nonexistentID, nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("GET by-id/nonexistent: status %d, want 404", resp.StatusCode)
	}
}

// TestConcepts_CrossRepoIsolation verifies a concept group in repo
// A is invisible from repo B's by-id lookup (the lookup is scoped
// by repository_id, so a conceptID from repo A returns 404 from
// repo B).
func TestConcepts_CrossRepoIsolation(t *testing.T) {
	env := testutil.NewTestEnv(t)
	defer env.Server.Close()

	admin := bootstrapSysAdmin(t, env, "xrepo@example.com")
	const slugA = "xrepo-a"
	const slugB = "xrepo-b"
	_, _, repoAID := createRepositoryWithDB(t, admin, "XRepo A", slugA, "desc", "")
	_, _, repoBID := createRepositoryWithDB(t, admin, "XRepo B", slugB, "desc", "")

	// Create a concept in repo A only.
	pgRepoA := pgRepoID(t, repoAID)
	cA, err := (store.New(env.DB)).CreateConcept(context.Background(), store.CreateConceptParams{
		RepositoryID: pgRepoA, CanonicalName: "Shared Name", Context: "Person",
	})
	if err != nil {
		t.Fatalf("create concept in A: %v", err)
	}
	cAIDStr := pgUUIDString(cA.ID)

	// Repo A sees it.
	aResp, _ := admin.do("GET", "/api/v1/repositories/"+slugA+"/concepts/"+cAIDStr, nil)
	if aResp.StatusCode != http.StatusOK {
		t.Errorf("repo A by-id: status %d, want 200", aResp.StatusCode)
	}
	// Repo B does not (cross-repo isolation: the concept belongs to A).
	bResp, _ := admin.do("GET", "/api/v1/repositories/"+slugB+"/concepts/"+cAIDStr, nil)
	if bResp.StatusCode != http.StatusNotFound {
		t.Errorf("repo B by-id (cross-repo): status %d, want 404 (isolation)", bResp.StatusCode)
	}
	_ = repoBID
}

// TestConcepts_LegacyIDRouteReturnsGroup verifies the
// GET /concepts/{conceptID} route returns the grouped shape keyed
// on the conceptID's canonical_name group (now the primary route,
// formerly "legacy").
func TestConcepts_LegacyIDRouteReturnsGroup(t *testing.T) {
	env := testutil.NewTestEnv(t)
	defer env.Server.Close()

	admin := bootstrapSysAdmin(t, env, "legacyid@example.com")
	const slug = "legacy-id-repo"
	_, _, repoID := createRepositoryWithDB(t, admin, "Legacy ID Repo", slug, "desc", "")
	pgRepo := pgRepoID(t, repoID)
	queries := store.New(env.DB)

	// Two contexts for the same name.
	c1, err := queries.CreateConcept(context.Background(), store.CreateConceptParams{
		RepositoryID: pgRepo, CanonicalName: "Einstein", Context: "Scientist",
	})
	if err != nil {
		t.Fatalf("create c1: %v", err)
	}
	if _, err := queries.CreateConcept(context.Background(), store.CreateConceptParams{
		RepositoryID: pgRepo, CanonicalName: "Einstein", Context: "Person",
	}); err != nil {
		t.Fatalf("create c2: %v", err)
	}

	// Legacy route by c1's id returns the whole group.
	idStr := pgUUIDString(c1.ID)
	resp, raw := admin.do("GET", "/api/v1/repositories/"+slug+"/concepts/"+idStr, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("legacy GET /concepts/{id}: %d %s", resp.StatusCode, raw)
	}
	var detail conceptGroupDetail
	if err := json.Unmarshal(raw, &detail); err != nil {
		t.Fatalf("decode legacy detail: %v", err)
	}
	if len(detail.Contexts) != 2 {
		t.Errorf("legacy detail contexts len = %d, want 2 (grouped shape on legacy route)", len(detail.Contexts))
	}
}

// TestConcepts_CanonicalNameCharsetAcceptsPunctuation verifies the
// loosened CHECK constraint on canonical_name accepts punctuation-
// bearing and accented names (periods, commas, parentheses, accents,
// ...). Migration 0030 dropped the strict ASCII alphabet whitelist
// (which caused extract_concepts to hang on LLM-emitted names like
// "Ana Obregón", "2,6-Diaminopurine") in favor of a check that only
// requires at least one ASCII letter or digit.
func TestConcepts_CanonicalNameCharsetAcceptsPunctuation(t *testing.T) {
	env := testutil.NewTestEnv(t)
	defer env.Server.Close()

	admin := bootstrapSysAdmin(t, env, "charset@example.com")
	const slug = "charset-repo"
	_, _, repoID := createRepositoryWithDB(t, admin, "Charset Repo", slug, "desc", "")
	pgRepo := pgRepoID(t, repoID)
	queries := store.New(env.DB)

	// Allowed: any name with at least one ASCII letter or digit,
	// including punctuation and accented letters (the loosened
	// charset from migration 0030).
	allowed := []string{
		"Donald John Trump", "O'Brien", "DNA", "Graphene-Oxide", "C19",
		// Previously rejected by the strict whitelist, now accepted.
		"Trump, Donald J.", "DNA (deoxyribonucleic)", "A/B",
		"Tom & Jerry", "X.Y.Z",
		// Accented names — the original root cause of the stuck
		// extract_concepts queue.
		"Ana Obregón", "Café Müller", "Müller", "Æsop",
	}
	for _, name := range allowed {
		if _, err := queries.CreateConcept(context.Background(), store.CreateConceptParams{
			RepositoryID: pgRepo, CanonicalName: name, Context: "Molecule",
		}); err != nil {
			t.Errorf("allowed canonical_name %q rejected by CHECK: %v", name, err)
		}
	}

	// Still banned: names with zero ASCII letters or digits (empty,
	// whitespace-only, pure punctuation). The CHECK still enforces
	// "at least one ASCII alnum" so a concept always has a non-empty
	// grouping key.
	banned := []string{"", "   ", "...", "---", "@#$", "éàç"}
	for _, name := range banned {
		if _, err := queries.CreateConcept(context.Background(), store.CreateConceptParams{
			RepositoryID: pgRepo, CanonicalName: name, Context: "Molecule",
		}); err == nil {
			t.Errorf("banned canonical_name %q accepted by CHECK (should be rejected)", name)
		}
	}
}
