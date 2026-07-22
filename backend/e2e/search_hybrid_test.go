//go:build e2e

package e2e_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/openktree/open-knowledge-tree/backend/e2e/testutil"
	"github.com/openktree/open-knowledge-tree/backend/internal/store"
)

// TestSearchHybrid_LexicalFallback verifies the fact and concept
// search endpoints return search_mode="lexical" and preserve the
// pre-hybrid behavior when Qdrant / the embedding provider are not
// wired (the default test env). This is the regression guard: the
// new hybrid code path must be invisible when its dependencies are
// absent. The hybrid path itself (Qdrant + embedding provider
// wired) is exercised via the internal/search unit tests (RRF
// correctness, fail-open) and via manual/CI-gated runs with
// QDRANT_HOST set; the always-on e2e suite does not boot Qdrant.
func TestSearchHybrid_LexicalFallback(t *testing.T) {
	env := testutil.NewTestEnv(t)
	admin := bootstrapSysAdmin(t, env, "hybrid-lexical@example.com")
	slug := "hybrid-lexical-facts"
	_, _, repoID := createRepositoryWithDB(t, admin, "Hybrid Lexical Facts", slug, "desc", "")

	// Insert a source + a fact so the facts list has something to
	// return. The fact text carries a searchable term so the ?q=
	// path matches.
	ctx := context.Background()
	queries := store.New(env.DB)
	srcID := pgtype.UUID{}
	if err := srcID.Scan(uuid.New().String()); err != nil {
		t.Fatalf("scan source id: %v", err)
	}
	pgRepo := pgRepoID(t, repoID)
	if _, err := queries.CreateSource(ctx, store.CreateSourceParams{
		ID:           srcID,
		RepositoryID: pgRepo,
		Url:          "https://example.com/hybrid-lexical",
		Kind:         "homepage",
		Status:       "fetched",
	}); err != nil {
		t.Fatalf("create source: %v", err)
	}
	insertFactWithSource(t, env, pgRepo, srcID, "Mitochondria are the powerhouse of the cell.", -1)
	// The list endpoint defaults to status=stable; new facts are
	// 'new' on insert, so promote the fact to stable for the test.
	if _, err := env.DB.Exec(ctx,
		`UPDATE okt_repository.facts SET status = 'stable' WHERE text = $1`,
		"Mitochondria are the powerhouse of the cell.",
	); err != nil {
		t.Fatalf("promote fact to stable: %v", err)
	}

	// Empty query: lexical path runs, search_mode="lexical".
	resp, raw := admin.do("GET", "/api/v1/repositories/"+slug+"/facts", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /facts (no q): %d %s", resp.StatusCode, raw)
	}
	var factsPage pageEnvelope
	if err := json.Unmarshal(raw, &factsPage); err != nil {
		t.Fatalf("decode facts page: %v", err)
	}
	if factsPage.SearchMode != "lexical" {
		t.Errorf("empty-query facts search_mode = %q, want %q (no qdrant wired -> lexical fallback)", factsPage.SearchMode, "lexical")
	}
	if factsPage.Total < 1 {
		t.Errorf("empty-query facts total = %d, want >= 1 (the inserted fact)", factsPage.Total)
	}

	// Non-empty query: still lexical (no qdrant wired), search_mode="lexical".
	resp, raw = admin.do("GET", "/api/v1/repositories/"+slug+"/facts?q=mitochondria", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /facts?q=mitochondria: %d %s", resp.StatusCode, raw)
	}
	if err := json.Unmarshal(raw, &factsPage); err != nil {
		t.Fatalf("decode facts q page: %v", err)
	}
	if factsPage.SearchMode != "lexical" {
		t.Errorf("q=mitochondria facts search_mode = %q, want %q (no qdrant wired -> fail-open to lexical)", factsPage.SearchMode, "lexical")
	}
	if factsPage.Total != 1 {
		t.Errorf("q=mitochondria facts total = %d, want 1 (single matching fact)", factsPage.Total)
	}

	// Concepts: empty query -> lexical.
	cresp, craw := admin.do("GET", "/api/v1/repositories/"+slug+"/concepts", nil)
	if cresp.StatusCode != http.StatusOK {
		t.Fatalf("GET /concepts (no q): %d %s", cresp.StatusCode, craw)
	}
	var conceptsPage pageEnvelope
	if err := json.Unmarshal(craw, &conceptsPage); err != nil {
		t.Fatalf("decode concepts page: %v", err)
	}
	if conceptsPage.SearchMode != "lexical" {
		t.Errorf("empty-query concepts search_mode = %q, want %q", conceptsPage.SearchMode, "lexical")
	}

	// Concepts: non-empty query that matches nothing -> lexical, total 0.
	mresp, mraw := admin.do("GET", "/api/v1/repositories/"+slug+"/concepts?q=zzznomatch", nil)
	if mresp.StatusCode != http.StatusOK {
		t.Fatalf("GET /concepts?q=zzznomatch: %d %s", mresp.StatusCode, mraw)
	}
	var missPage pageEnvelope
	if err := json.Unmarshal(mraw, &missPage); err != nil {
		t.Fatalf("decode concepts miss page: %v", err)
	}
	if missPage.SearchMode != "lexical" {
		t.Errorf("q=zzznomatch concepts search_mode = %q, want %q", missPage.SearchMode, "lexical")
	}
	if missPage.Total != 0 {
		t.Errorf("q=zzznomatch concepts total = %d, want 0 (no match)", missPage.Total)
	}
}

// TestSearchHybrid_ConceptIntersectionStaysLexical verifies that the
// concepts= intersection filter on GET /facts stays on the lexical
// path (search_mode is not "hybrid") even if hybrid were enabled —
// the Qdrant payload doesn't carry concept_id, so the intersection
// semantics don't map to vector search. This guards the carve-out
// documented in the plan.
func TestSearchHybrid_ConceptIntersectionStaysLexical(t *testing.T) {
	env := testutil.NewTestEnv(t)
	admin := bootstrapSysAdmin(t, env, "hybrid-intersect@example.com")
	slug := "hybrid-intersect-facts"
	_, _, repoID := createRepositoryWithDB(t, admin, "Hybrid Intersect Facts", slug, "desc", "")

	ctx := context.Background()
	queries := store.New(env.DB)
	srcID := pgtype.UUID{}
	if err := srcID.Scan(uuid.New().String()); err != nil {
		t.Fatalf("scan source id: %v", err)
	}
	pgRepo := pgRepoID(t, repoID)
	if _, err := queries.CreateSource(ctx, store.CreateSourceParams{
		ID:           srcID,
		RepositoryID: pgRepo,
		Url:          "https://example.com/hybrid-intersect",
		Kind:         "homepage",
		Status:       "fetched",
	}); err != nil {
		t.Fatalf("create source: %v", err)
	}
	factIDStr := insertFactWithSource(t, env, pgRepo, srcID, "Shared fact for intersection test.", -1)
	if _, err := env.DB.Exec(ctx,
		`UPDATE okt_repository.facts SET status = 'stable' WHERE id = $1`,
		factIDStr,
	); err != nil {
		t.Fatalf("promote fact to stable: %v", err)
	}
	var factID pgtype.UUID
	if err := factID.Scan(factIDStr); err != nil {
		t.Fatalf("scan fact id: %v", err)
	}
	// Insert two concepts + link both to the fact via raw SQL.
	// CreateConcept doesn't take an ID (DB generates it), so we
	// insert via SQL to control the UUIDs we pass to concepts=.
	linkConcept := func(name string) string {
		cid := uuid.New().String()
		if _, err := env.DB.Exec(ctx,
			`INSERT INTO okt_repository.concepts (id, repository_id, canonical_name, context) VALUES ($1, $2, $3, $4)`,
			cid, repoID, name, "TestCtx",
		); err != nil {
			t.Fatalf("insert concept %s: %v", name, err)
		}
		if _, err := env.DB.Exec(ctx,
			`INSERT INTO okt_repository.fact_concepts (fact_id, concept_id) VALUES ($1, $2)`,
			factIDStr, cid,
		); err != nil {
			t.Fatalf("link fact-concept %s: %v", name, err)
		}
		return name
	}
	linkConcept("ConceptA")
	linkConcept("ConceptB")

	// concepts=ConceptA,ConceptB with a query: must stay lexical
	// (the intersection path is carved out of hybrid).
	resp, raw := admin.do("GET", "/api/v1/repositories/"+slug+"/facts?q=shared&concepts=ConceptA,ConceptB", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /facts?concepts=...: %d %s", resp.StatusCode, raw)
	}
	var page pageEnvelope
	if err := json.Unmarshal(raw, &page); err != nil {
		t.Fatalf("decode page: %v", err)
	}
	// The intersection path doesn't set SearchMode today (it
	// returns the pre-hybrid pageEnvelope without search_mode).
	// Accept either empty or "lexical" — both mean "not hybrid".
	if page.SearchMode == "hybrid" {
		t.Errorf("concepts= intersection search_mode = %q, want NOT hybrid (intersection is carved out of hybrid)", page.SearchMode)
	}
	// Sanity: the shared fact is in the intersection result.
	if page.Total < 1 {
		t.Errorf("concepts=ConceptA,ConceptB total = %d, want >= 1 (the shared fact)", page.Total)
	}
}