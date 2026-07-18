//go:build e2e

package e2e_test

import (
	"context"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/openktree/open-knowledge-tree/backend/internal/config"
	"github.com/openktree/open-knowledge-tree/backend/internal/providers/ai"
	"github.com/openktree/open-knowledge-tree/backend/internal/providers/refinement"
	"github.com/openktree/open-knowledge-tree/backend/internal/qdrantstore"
	"github.com/openktree/open-knowledge-tree/backend/internal/store"
	"github.com/openktree/open-knowledge-tree/backend/internal/taskmanager/tasks"
	"github.com/openktree/open-knowledge-tree/backend/e2e/testutil"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivertest"
)

// failingRefiner is a refinement.RefineProvider whose Refine call
// records that it was invoked and returns an error. Tests use it to
// assert the pre-LLM routing path handled a candidate WITHOUT
// invoking the LLM refiner (the per-fact alias disambiguation runs
// in Step 2, before the LLM call).
type failingRefiner struct {
	calls int
}

func (r *failingRefiner) Refine(_ context.Context, _ store.DBTX, _ refinement.RefinementRequest) (refinement.RefinementResult, error) {
	r.calls++
	return refinement.RefinementResult{}, nil
}

func (r *failingRefiner) Describe() refinement.ProviderDescription {
	return refinement.ProviderDescription{Name: "failing-refiner"}
}

// qdrantTestStoreBoth builds a qdrantstore.Store against QDRANT_HOST
// and ensures BOTH the facts and concepts collections exist at the
// given dimension. The alias-disambiguation test needs both (it
// embeds facts to anchor the per-fact cosine comparison AND upserts
// concept vectors for the matched candidates). Skips when
// QDRANT_HOST is unset, mirroring qdrantTestStore.
func qdrantTestStoreBoth(t *testing.T, dim int) (*qdrantstore.Store, func()) {
	t.Helper()
	host := os.Getenv("QDRANT_HOST")
	if host == "" {
		t.Skip("QDRANT_HOST not set; skipping Qdrant-dependent test")
	}
	cfg := config.QdrantConfig{
		Host:             host,
		Port:             6334,
		Collection:       "okt_facts_test_" + uuid.NewString()[:8],
		ConceptCollection: "okt_concepts_test_" + uuid.NewString()[:8],
		AllowRecreate:    true,
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
	if err := s.EnsureCollection(ctx, dim); err != nil {
		s.Close()
		t.Fatalf("ensure facts collection: %v", err)
	}
	if err := s.EnsureConceptCollection(ctx, dim); err != nil {
		s.Close()
		t.Fatalf("ensure concepts collection: %v", err)
	}
	return s, func() { s.Close() }
}

// disambigEmbeddingProvider is a test embedding provider that maps
// inputs containing "nitrogen" to one vector and inputs containing
// "neutron" to a different vector, so the cosine tie-break sends
// nitrogen-related inputs together and neutron-related inputs
// together. Unlike the generic stubEmbeddingProvider (which seeds
// from raw bytes and has no semantic structure), this one encodes
// the two concepts as orthogonal basis vectors, making the
// disambiguation deterministic and meaningful.
type disambigEmbeddingProvider struct {
	dim int
}

func (p *disambigEmbeddingProvider) Embed(_ context.Context, _ store.DBTX, req ai.EmbeddingRequest) (ai.EmbeddingResponse, error) {
	embeddings := make([][]float32, len(req.Inputs))
	for i, in := range req.Inputs {
		vec := make([]float32, p.dim)
		lower := strings.ToLower(in)
		// "nitrogen" marker -> vector dominated by dim 0.
		// "neutron" marker  -> vector dominated by dim 1.
		// Both get a small common component on dim 2 so they're
		// nonzero, but the dominant axis makes cosine similarity
		// group nitrogen-with-nitrogen and neutron-with-neutron.
		if strings.Contains(lower, "nitrogen") {
			vec[0] = 1.0
			vec[2] = 0.1
		} else if strings.Contains(lower, "neutron") {
			vec[1] = 1.0
			vec[2] = 0.1
		} else {
			// Default: small spread so non-matching inputs don't
			// accidentally cluster with either.
			for j := range vec {
				vec[j] = 0.01
			}
		}
		embeddings[i] = vec
	}
	return ai.EmbeddingResponse{
		Model:      "disambig-stub",
		Embeddings: embeddings,
		Usage:      ai.EmbeddingUsage{PromptTokens: len(req.Inputs)},
	}, nil
}

func (p *disambigEmbeddingProvider) Describe() ai.ProviderDescription {
	return ai.ProviderDescription{Name: "disambig-stub"}
}

// TestRefineConcepts_AliasDisambiguationByEmbedding is the
// regression guard for the shared-alias mis-routing bug: an alias
// ("N") set on BOTH Nitrogen and Neutron caused the LIMIT-1
// FindConceptByAlias to link a coffee/nitrogen fact to Neutron
// arbitrarily. The fix routes each fact individually to its
// cosine-closest concept via concepts.ResolveAliasMatchForFact.
//
// Scenario: two existing concepts (Nitrogen, Neutron) both carry
// the alias "N". An unresolved candidate with concept_text "N" is
// linked to two facts: one about nitrogen (coffee plant chemistry),
// one about a neutron (particle physics). Refine's pre-LLM alias
// match (Step 2) finds 2 matches and forks into per-fact routing.
// The nitrogen fact must link to Nitrogen, the neutron fact to
// Neutron, and the refiner LLM must NOT be called (pre-LLM routing
// handled it). The candidate is deleted (all facts routed).
//
// Env-gated on QDRANT_HOST (the test profile does not bring up
// Qdrant). Skips when unset.
func TestRefineConcepts_AliasDisambiguationByEmbedding(t *testing.T) {
	const dim = 8
	qStore, qCleanup := qdrantTestStoreBoth(t, dim)
	defer qCleanup()

	env := testutil.NewTestEnv(t)
	defer env.Server.Close()
	ensureRiverSchema(t, env.DB)

	admin := bootstrapSysAdmin(t, env, "disambig@example.com")
	_, _, repoID := createRepositoryWithDB(t, admin, "Disambig", "disambig", "desc", "")
	queries := store.New(env.DB)
	pgRepo := pgRepoID(t, repoID)

	// Source for the facts.
	srcID := pgtype.UUID{}
	if err := srcID.Scan(uuid.NewString()); err != nil {
		t.Fatalf("scan src: %v", err)
	}
	if _, err := queries.CreateSource(context.Background(), store.CreateSourceParams{
		ID: srcID, RepositoryID: pgRepo, Url: "https://example.com/disambig", Kind: "homepage", Status: "fetched",
	}); err != nil {
		t.Fatalf("create source: %v", err)
	}

	// Two facts with deliberately distinct text so the stub embedding
	// produces distinct vectors (the stub seeds the vector from the
	// input bytes). The nitrogen fact embeds near a nitrogen concept;
	// the neutron fact near a neutron concept.
	//
	// Facts are inserted as status='new' (the default) so embed_facts
	// (which filters status='new' AND embedded_at IS NULL) picks them
	// up. They are promoted to 'stable' AFTER embedding, mirroring the
	// real pipeline (embed_facts → dedup promotes to stable →
	// extract_concepts runs on stable facts).
	nitrogenFactID := insertFactWithSource(t, env, pgRepo, srcID, "Coffee plants fix nitrogen from soil chemistry N.", 0)
	neutronFactID := insertFactWithSource(t, env, pgRepo, srcID, "A neutron is a neutral subatomic particle N.", 1)

	// Embed both facts into Qdrant via the embed_facts worker so the
	// per-fact cosine comparison has real fact vectors.
	embCfg := config.EmbeddingConfig{Provider: "stub", Model: "stub-embedding", Dimensions: dim}
	registry := testutil.NewForTestPool(env.DB)
	systemQueries := store.New(env.DB)
	embProvider := &disambigEmbeddingProvider{dim: dim}
	embedWorker := tasks.NewEmbedFactsWorker(embProvider, embCfg, qStore, registry, systemQueries)
	driver := riverpgxv5.New(env.DB)
	workers := river.NewWorkers()
	river.AddWorker(workers, embedWorker)
	testEmbed := rivertest.NewWorker(t, driver, &river.Config{
		Queues:  map[string]river.QueueConfig{tasks.QueueEmbedFacts: {MaxWorkers: 1}},
		Workers: workers,
	}, embedWorker)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	{
		tx, err := env.DB.BeginTx(ctx, pgx.TxOptions{})
		if err != nil {
			t.Fatalf("begin tx: %v", err)
		}
		if _, err := testEmbed.Work(ctx, t, tx, tasks.EmbedFactsArgs{RepositoryID: repoID, SourceID: pgUUIDString(srcID)}, &river.InsertOpts{Queue: tasks.QueueEmbedFacts}); err != nil {
			tx.Rollback(context.Background())
			t.Fatalf("embed_facts: %v", err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatalf("commit embed: %v", err)
		}
	}

	// Promote the facts to 'stable' after embedding (mirrors the real
	// pipeline: dedup promotes new→stable; extract_concepts and the
	// candidate workflow run on stable facts).
	for _, fid := range []string{nitrogenFactID, neutronFactID} {
		if _, err := env.DB.Exec(context.Background(),
			`UPDATE okt_repository.facts SET status = 'stable' WHERE id = $1`, fid); err != nil {
			t.Fatalf("promote fact: %v", err)
		}
	}

	// Create two existing concepts in the SAME context, both with the
	// alias "N". The same-context alias query (FindConceptsByAlias)
	// is scoped by (repo, lower(context)), so both must share the
	// candidate's context for the multi-match to fire. Different
	// canonical_names so they're distinct concepts.
	nitrogenConcept, err := queries.CreateConcept(context.Background(), store.CreateConceptParams{
		RepositoryID: pgRepo, CanonicalName: "Nitrogen", Context: "Chemical Element",
	})
	if err != nil {
		t.Fatalf("create Nitrogen concept: %v", err)
	}
	neutronConcept, err := queries.CreateConcept(context.Background(), store.CreateConceptParams{
		RepositoryID: pgRepo, CanonicalName: "Neutron", Context: "Chemical Element",
	})
	if err != nil {
		t.Fatalf("create Neutron concept: %v", err)
	}
	// Both share the alias "N" — this is the ambiguity that used to
	// mis-route via LIMIT 1.
	for _, c := range []store.OktRepositoryConcept{nitrogenConcept, neutronConcept} {
		if _, err := queries.AddConceptAlias(context.Background(), store.AddConceptAliasParams{
			ConceptID: c.ID, AliasText: "N",
		}); err != nil {
			t.Fatalf("add alias N: %v", err)
		}
	}
	// Embed both concepts into Qdrant. The stub provider seeds the
	// vector from the input bytes, so "Nitrogen Chemical Element" and
	// "Neutron Chemical Element" produce distinct vectors — exactly
	// what the cosine tie-break needs.
	repoUUID, _ := uuid.Parse(repoID)
	nitUUID, _ := uuid.Parse(pgUUIDString(nitrogenConcept.ID))
	neuUUID, _ := uuid.Parse(pgUUIDString(neutronConcept.ID))
	embResp, err := embProvider.Embed(context.Background(), env.DB, ai.EmbeddingRequest{
		Model: embCfg.Model,
		Inputs: []string{
			nitrogenConcept.CanonicalName + " " + nitrogenConcept.Context,
			neutronConcept.CanonicalName + " " + neutronConcept.Context,
		},
	})
	if err != nil {
		t.Fatalf("embed concepts: %v", err)
	}
	if err := qStore.UpsertConceptVectors(context.Background(), []qdrantstore.ConceptPoint{
		{ID: nitUUID, Vector: embResp.Embeddings[0], RepositoryID: repoUUID},
		{ID: neuUUID, Vector: embResp.Embeddings[1], RepositoryID: repoUUID},
	}); err != nil {
		t.Fatalf("upsert concept vectors: %v", err)
	}

	// Create an unresolved candidate with concept_text "N" (matching
	// the shared alias) in the same context, and link BOTH facts via
	// fact_candidates. This is the state extract_concepts leaves
	// behind when refinement is enabled.
	candidate, err := queries.CreateCandidate(context.Background(), store.CreateCandidateParams{
		RepositoryID: pgRepo, ConceptText: "N", Context: "Chemical Element", SeedAliases: []string{},
	})
	if err != nil {
		// ON CONFLICT: re-fetch.
		c, ferr := queries.FindUnresolvedCandidate(context.Background(), store.FindUnresolvedCandidateParams{
			RepositoryID: pgRepo, ConceptText: "N", Context: "Chemical Element",
		})
		if ferr != nil {
			t.Fatalf("create/re-find candidate: %v / %v", err, ferr)
		}
		candidate = c
	}
	for _, fid := range []string{nitrogenFactID, neutronFactID} {
		var pgFid pgtype.UUID
		if err := pgFid.Scan(fid); err != nil {
			t.Fatalf("scan fact: %v", err)
		}
		if _, err := queries.AddFactCandidate(context.Background(), store.AddFactCandidateParams{
			FactID: pgFid, CandidateID: candidate.ID,
		}); err != nil {
			t.Fatalf("add fact_candidate: %v", err)
		}
	}

	// Run refine_concepts with a refiner that MUST NOT be called
	// (pre-LLM alias routing handles the multi-match). Wire the
	// embedding deps so the per-fact cosine tie-break works.
	refiner := &failingRefiner{}
	refineCfg := config.RefinementConfig{Enabled: true, Model: "stub", MaxTokens: 400}
	refineWorker := tasks.NewRefineConceptsWorker(refiner, refineCfg, registry, systemQueries, false, nil, nil, embProvider, embCfg, qStore)
	rworkers := river.NewWorkers()
	river.AddWorker(rworkers, refineWorker)
	testRefine := rivertest.NewWorker(t, driver, &river.Config{
		Queues:  map[string]river.QueueConfig{tasks.QueueRefineConcepts: {MaxWorkers: 1}},
		Workers: rworkers,
	}, refineWorker)

	{
		tx, err := env.DB.BeginTx(ctx, pgx.TxOptions{})
		if err != nil {
			t.Fatalf("begin refine tx: %v", err)
		}
		job, err := testRefine.Work(ctx, t, tx, tasks.RefineConceptsArgs{
			RepositoryID: repoID,
			CandidateIDs: []string{pgUUIDString(candidate.ID)},
		}, &river.InsertOpts{Queue: tasks.QueueRefineConcepts})
		if err != nil {
			tx.Rollback(context.Background())
			t.Fatalf("refine_concepts.Work: %v", err)
		}
		if job.EventKind != river.EventKindJobCompleted {
			tx.Rollback(context.Background())
			t.Fatalf("expected completed, got %s", job.EventKind)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatalf("commit refine: %v", err)
		}
	}

	// The refiner must NOT have been called (pre-LLM routing handled
	// the candidate via per-fact alias disambiguation).
	if refiner.calls != 0 {
		t.Errorf("refiner was called %d times; want 0 (pre-LLM per-fact routing should handle the multi-alias match)", refiner.calls)
	}

	// The nitrogen fact must be linked to Nitrogen, the neutron fact
	// to Neutron — per-fact split routing.
	var nitrogenLinked, neutronLinked int
	nitrogenFactPg := pgtype.UUID{}
	nitrogenFactPg.Scan(nitrogenFactID)
	neutronFactPg := pgtype.UUID{}
	neutronFactPg.Scan(neutronFactID)
	if err := env.DB.QueryRow(context.Background(),
		`SELECT count(*) FROM okt_repository.fact_concepts WHERE fact_id = $1 AND concept_id = $2`,
		nitrogenFactPg, nitrogenConcept.ID,
	).Scan(&nitrogenLinked); err != nil {
		t.Fatalf("query nitrogen link: %v", err)
	}
	if err := env.DB.QueryRow(context.Background(),
		`SELECT count(*) FROM okt_repository.fact_concepts WHERE fact_id = $1 AND concept_id = $2`,
		neutronFactPg, neutronConcept.ID,
	).Scan(&neutronLinked); err != nil {
		t.Fatalf("query neutron link: %v", err)
	}
	if nitrogenLinked != 1 {
		t.Errorf("nitrogen fact linked to Nitrogen = %d, want 1 (per-fact cosine routing should send the nitrogen fact to Nitrogen)", nitrogenLinked)
	}
	if neutronLinked != 1 {
		t.Errorf("neutron fact linked to Neutron = %d, want 1 (per-fact cosine routing should send the neutron fact to Neutron)", neutronLinked)
	}

	// The candidate must be deleted (all facts routed, none deferred).
	var candidateCount int
	if err := env.DB.QueryRow(context.Background(),
		`SELECT count(*) FROM okt_repository.concept_candidates WHERE id = $1`, candidate.ID,
	).Scan(&candidateCount); err != nil {
		t.Fatalf("query candidate: %v", err)
	}
	if candidateCount != 0 {
		t.Errorf("candidate row still exists; want deleted (all facts routed)")
	}
}

// stubRefiner is a refinement.RefineProvider that returns a canned
// RefinementResult per concept text. It records the number of calls
// so tests can assert the LLM was invoked the expected number of
// times. Used by TestRefineConcepts_ConcurrentChunk to exercise the
// concurrent processing path: multiple candidates needing LLM calls
// are processed in parallel by the errgroup inside Work.
type stubRefiner struct {
	mu    sync.Mutex
	calls int
}

func (r *stubRefiner) Refine(_ context.Context, _ store.DBTX, req refinement.RefinementRequest) (refinement.RefinementResult, error) {
	r.mu.Lock()
	r.calls++
	r.mu.Unlock()
	// Return a canonical name derived from the concept text so each
	// candidate creates a distinct new concept. Add one alias per
	// candidate so the alias-insert path is also exercised.
	canonical := "Canonical " + req.Concept
	return refinement.RefinementResult{
		CanonicalName:  canonical,
		AliasesToAdd:   []string{req.Concept + " alias"},
		AliasesToPrune: []string{},
	}, nil
}

func (r *stubRefiner) Describe() refinement.ProviderDescription {
	return refinement.ProviderDescription{Name: "stub-refiner"}
}

func (r *stubRefiner) CallCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls
}

// TestRefineConcepts_ConcurrentChunk exercises the concurrent
// candidate-processing path in refine_concepts.Work. It creates a
// mix of candidates — some that resolve via pre-LLM DB routing
// (exact canonical / alias matches) and some that need the LLM —
// then runs a single RefineConcepts job with all candidates in the
// chunk. Asserts all candidates are resolved, counters are correct,
// and the LLM was called only for the candidates that missed
// pre-LLM routing.
func TestRefineConcepts_ConcurrentChunk(t *testing.T) {
	env := testutil.NewTestEnv(t)
	defer env.Server.Close()
	ensureRiverSchema(t, env.DB)

	admin := bootstrapSysAdmin(t, env, "concurrent@example.com")
	_, _, repoID := createRepositoryWithDB(t, admin, "Concurrent", "concurrent", "desc", "")
	queries := store.New(env.DB)
	pgRepo := pgRepoID(t, repoID)

	// Source for the facts.
	srcID := pgtype.UUID{}
	if err := srcID.Scan(uuid.NewString()); err != nil {
		t.Fatalf("scan src: %v", err)
	}
	if _, err := queries.CreateSource(context.Background(), store.CreateSourceParams{
		ID: srcID, RepositoryID: pgRepo, Url: "https://example.com/concurrent", Kind: "homepage", Status: "fetched",
	}); err != nil {
		t.Fatalf("create source: %v", err)
	}

	// --- Pre-existing concepts for pre-LLM routing ---
	// 3 concepts that candidates will match by canonical name or alias.
	preExisting := []struct {
		canonical string
		context   string
		alias     string
	}{
		{"Alpha Concept", "Test Context", "alpha"},
		{"Beta Concept", "Test Context", "beta"},
		{"Gamma Concept", "Test Context", "gamma"},
	}
	for _, p := range preExisting {
		c, err := queries.CreateConcept(context.Background(), store.CreateConceptParams{
			RepositoryID: pgRepo, CanonicalName: p.canonical, Context: p.context,
		})
		if err != nil {
			t.Fatalf("create pre-existing concept %s: %v", p.canonical, err)
		}
		if _, err := queries.AddConceptAlias(context.Background(), store.AddConceptAliasParams{
			ConceptID: c.ID, AliasText: p.alias,
		}); err != nil {
			t.Fatalf("add alias %s: %v", p.alias, err)
		}
	}

	// --- Create candidates ---
	// 3 candidates that match pre-existing concepts (pre-LLM routing, no LLM).
	// 4 candidates that need the LLM (no match, will create new concepts).
	// Total: 7 candidates processed concurrently.
	type candidateSpec struct {
		text     string
		context  string
		seed     []string
		needsLLM bool
	}
	specs := []candidateSpec{
		// Pre-LLM canonical matches.
		{"Alpha Concept", "Test Context", []string{"alpha"}, false},
		{"Beta Concept", "Test Context", []string{"beta"}, false},
		// Pre-LLM alias match (candidate text = alias, not canonical).
		{"gamma", "Test Context", []string{}, false},
		// LLM-needing candidates (no match in DB).
		{"delta", "Test Context", []string{"d"}, true},
		{"epsilon", "Test Context", []string{"e"}, true},
		{"zeta", "Test Context", []string{"z"}, true},
		{"eta", "Test Context", []string{"h"}, true},
	}

	var candidateIDs []string
	for _, s := range specs {
		c, err := queries.CreateCandidate(context.Background(), store.CreateCandidateParams{
			RepositoryID: pgRepo, ConceptText: s.text, Context: s.context, SeedAliases: s.seed,
		})
		if err != nil {
			// ON CONFLICT: re-fetch.
			cf, ferr := queries.FindUnresolvedCandidate(context.Background(), store.FindUnresolvedCandidateParams{
				RepositoryID: pgRepo, ConceptText: s.text, Context: s.context,
			})
			if ferr != nil {
				t.Fatalf("create/re-find candidate %s: %v / %v", s.text, err, ferr)
			}
			c = cf
		}
		// Link a fact to each candidate so resolveCandidate has work.
		factID := insertFactWithSource(t, env, pgRepo, srcID, "Fact about "+s.text, int32(len(candidateIDs)))
		var pgFact pgtype.UUID
		if err := pgFact.Scan(factID); err != nil {
			t.Fatalf("scan fact: %v", err)
		}
		if _, err := queries.AddFactCandidate(context.Background(), store.AddFactCandidateParams{
			FactID: pgFact, CandidateID: c.ID,
		}); err != nil {
			t.Fatalf("add fact_candidate: %v", err)
		}
		candidateIDs = append(candidateIDs, pgUUIDString(c.ID))
	}

	// --- Run refine_concepts with concurrency ---
	refiner := &stubRefiner{}
	refineCfg := config.RefinementConfig{
		Enabled:        true,
		Model:          "stub",
		MaxTokens:      400,
		MaxConcurrency: 5,
	}
	registry := testutil.NewForTestPool(env.DB)
	systemQueries := store.New(env.DB)
	refineWorker := tasks.NewRefineConceptsWorker(refiner, refineCfg, registry, systemQueries, false, nil, nil, nil, config.EmbeddingConfig{}, nil)
	driver := riverpgxv5.New(env.DB)
	rworkers := river.NewWorkers()
	river.AddWorker(rworkers, refineWorker)
	testRefine := rivertest.NewWorker(t, driver, &river.Config{
		Queues:  map[string]river.QueueConfig{tasks.QueueRefineConcepts: {MaxWorkers: 1}},
		Workers: rworkers,
	}, refineWorker)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	{
		tx, err := env.DB.BeginTx(ctx, pgx.TxOptions{})
		if err != nil {
			t.Fatalf("begin refine tx: %v", err)
		}
		job, err := testRefine.Work(ctx, t, tx, tasks.RefineConceptsArgs{
			RepositoryID: repoID,
			CandidateIDs: candidateIDs,
		}, &river.InsertOpts{Queue: tasks.QueueRefineConcepts})
		if err != nil {
			tx.Rollback(context.Background())
			t.Fatalf("refine_concepts.Work: %v", err)
		}
		if job.EventKind != river.EventKindJobCompleted {
			tx.Rollback(context.Background())
			t.Fatalf("expected completed, got %s", job.EventKind)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatalf("commit refine: %v", err)
		}
	}

	// --- Assertions ---

	// The LLM should have been called exactly 4 times (for the 4
	// LLM-needing candidates). The 3 pre-LLM candidates should have
	// been resolved by DB routing without invoking the refiner.
	llmCalls := refiner.CallCount()
	if llmCalls != 4 {
		t.Errorf("refiner called %d times; want 4 (3 pre-LLM + 4 LLM = 7 candidates, only 4 need LLM)", llmCalls)
	}

	// All 7 candidates should be resolved (resolved_concept_id set
	// or deleted via per-fact routing). Check that no candidates
	// remain unresolved.
	var unresolvedCount int
	if err := env.DB.QueryRow(context.Background(),
		`SELECT count(*) FROM okt_repository.concept_candidates WHERE repository_id = $1 AND resolved_concept_id IS NULL`,
		pgRepo,
	).Scan(&unresolvedCount); err != nil {
		t.Fatalf("query unresolved candidates: %v", err)
	}
	if unresolvedCount != 0 {
		t.Errorf("%d candidates remain unresolved; want 0", unresolvedCount)
	}

	// The 4 LLM-needing candidates should have created 4 new concepts
	// with the canonical names the stub refiner returned.
	for _, s := range specs {
		if !s.needsLLM {
			continue
		}
		canonical := "Canonical " + s.text
		var conceptCount int
		if err := env.DB.QueryRow(context.Background(),
			`SELECT count(*) FROM okt_repository.concepts WHERE repository_id = $1 AND lower(canonical_name) = lower($2) AND lower(context) = lower($3)`,
			pgRepo, canonical, s.context,
		).Scan(&conceptCount); err != nil {
			t.Fatalf("query concept %s: %v", canonical, err)
		}
		if conceptCount != 1 {
			t.Errorf("concept %q not found (count=%d); want 1", canonical, conceptCount)
		}
	}

	// The 3 pre-LLM candidates should have been merged into the
	// pre-existing concepts (no new concepts created for them).
	for _, p := range preExisting {
		var conceptCount int
		if err := env.DB.QueryRow(context.Background(),
			`SELECT count(*) FROM okt_repository.concepts WHERE repository_id = $1 AND lower(canonical_name) = lower($2) AND lower(context) = lower($3)`,
			pgRepo, p.canonical, "Test Context",
		).Scan(&conceptCount); err != nil {
			t.Fatalf("query pre-existing concept %s: %v", p.canonical, err)
		}
		if conceptCount != 1 {
			t.Errorf("pre-existing concept %q count=%d; want 1 (should not be duplicated)", p.canonical, conceptCount)
		}
	}

	// All 7 facts should be linked to a concept via fact_concepts.
	var linkedCount int
	if err := env.DB.QueryRow(context.Background(),
		`SELECT count(DISTINCT fc.fact_id) FROM okt_repository.fact_concepts fc JOIN okt_repository.facts f ON f.id = fc.fact_id JOIN okt_repository.fact_sources fs ON fs.fact_id = f.id WHERE fs.source_id = $1`,
		srcID,
	).Scan(&linkedCount); err != nil {
		t.Fatalf("query linked facts: %v", err)
	}
	if linkedCount != 7 {
		t.Errorf("linked facts = %d; want 7 (all candidates should have resolved their facts)", linkedCount)
	}
}