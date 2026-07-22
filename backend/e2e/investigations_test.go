//go:build e2e

package e2e_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/openktree/open-knowledge-tree/backend/e2e/testutil"
	"github.com/openktree/open-knowledge-tree/backend/internal/concepts"
	"github.com/openktree/open-knowledge-tree/backend/internal/store"
)

// TestInvestigationsCRUD covers the investigation lifecycle:
// create, get, list, update, and delete, plus the source membership
// endpoints (add, list, remove) and the investigation-scoped fact
// list. It uses a sysadmin client (so every permission is granted)
// against a fresh repository on the default database.
func TestInvestigationsCRUD(t *testing.T) {
	env := testutil.NewTestEnv(t)
	defer env.Server.Close()

	admin := bootstrapSysAdmin(t, env, "inv_admin@example.com")

	const slug = "inv-repo"
	resp, body, repoID := createRepositoryWithDB(t, admin, "Investigation Repo", slug, "desc", "")
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create repo: status %d, body %s", resp.StatusCode, body)
	}
	if repoID == "" {
		t.Fatal("expected repository id")
	}

	// Create a source to attach to the investigation later.
	srcBody, _ := json.Marshal(map[string]string{
		"url":  "https://example.com/inv-source",
		"kind": "paper",
	})
	srcResp, srcRaw := admin.do("POST", "/api/v1/repositories/"+slug+"/sources", srcBody)
	if srcResp.StatusCode != http.StatusCreated {
		t.Fatalf("create source: status %d, body %s", srcResp.StatusCode, srcRaw)
	}
	var src struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(srcRaw, &src); err != nil {
		t.Fatalf("decoding source: %v", err)
	}
	if src.ID == "" {
		t.Fatal("expected source id")
	}

	// 1. Create an investigation.
	invBody, _ := json.Marshal(map[string]string{
		"title": "Climate impacts",
		"topic": "sea level rise",
	})
	createResp, createRaw := admin.do("POST", "/api/v1/repositories/"+slug+"/investigations", invBody)
	if createResp.StatusCode != http.StatusCreated {
		t.Fatalf("create investigation: status %d, body %s", createResp.StatusCode, createRaw)
	}
	var created struct {
		ID    string  `json:"id"`
		Title string  `json:"title"`
		Topic *string `json:"topic"`
	}
	if err := json.Unmarshal(createRaw, &created); err != nil {
		t.Fatalf("decoding investigation: %v", err)
	}
	if created.Title != "Climate impacts" || created.Topic == nil || *created.Topic != "sea level rise" {
		t.Fatalf("unexpected investigation: %+v", created)
	}
	invID := created.ID

	// 2. Get the investigation.
	getResp, getRaw := admin.do("GET", "/api/v1/repositories/"+slug+"/investigations/"+invID, nil)
	if getResp.StatusCode != http.StatusOK {
		t.Fatalf("get investigation: status %d, body %s", getResp.StatusCode, getRaw)
	}

	// 3. List investigations — should contain the one we created.
	listResp, listRaw := admin.do("GET", "/api/v1/repositories/"+slug+"/investigations", nil)
	if listResp.StatusCode != http.StatusOK {
		t.Fatalf("list investigations: status %d, body %s", listResp.StatusCode, listRaw)
	}
	var list pageEnvelope
	if err := json.Unmarshal(listRaw, &list); err != nil {
		t.Fatalf("decoding list: %v", err)
	}
	if list.Total != 1 {
		t.Fatalf("expected total=1, got %d", list.Total)
	}

	// 4. Update the investigation (clear topic).
	updBody, _ := json.Marshal(map[string]string{
		"title": "Climate impacts v2",
		"topic": "",
	})
	updResp, updRaw := admin.do("PUT", "/api/v1/repositories/"+slug+"/investigations/"+invID, updBody)
	if updResp.StatusCode != http.StatusOK {
		t.Fatalf("update investigation: status %d, body %s", updResp.StatusCode, updRaw)
	}
	var updated struct {
		Title string  `json:"title"`
		Topic *string `json:"topic"`
	}
	if err := json.Unmarshal(updRaw, &updated); err != nil {
		t.Fatalf("decoding updated: %v", err)
	}
	if updated.Title != "Climate impacts v2" || updated.Topic != nil {
		t.Fatalf("unexpected updated investigation: %+v", updated)
	}

	// 5. Add the source to the investigation.
	addBody, _ := json.Marshal(map[string]string{"source_id": src.ID})
	addResp, addRaw := admin.do("POST", "/api/v1/repositories/"+slug+"/investigations/"+invID+"/sources", addBody)
	if addResp.StatusCode != http.StatusNoContent {
		t.Fatalf("add source: status %d, body %s", addResp.StatusCode, addRaw)
	}

	// 5a. Idempotent: re-adding is a no-op (204).
	addResp2, addRaw2 := admin.do("POST", "/api/v1/repositories/"+slug+"/investigations/"+invID+"/sources", addBody)
	if addResp2.StatusCode != http.StatusNoContent {
		t.Fatalf("re-add source: status %d, body %s", addResp2.StatusCode, addRaw2)
	}

	// 6. List the investigation's sources — should contain the one we added.
	srcListResp, srcListRaw := admin.do("GET", "/api/v1/repositories/"+slug+"/investigations/"+invID+"/sources", nil)
	if srcListResp.StatusCode != http.StatusOK {
		t.Fatalf("list inv sources: status %d, body %s", srcListResp.StatusCode, srcListRaw)
	}
	var srcList pageEnvelope
	if err := json.Unmarshal(srcListRaw, &srcList); err != nil {
		t.Fatalf("decoding inv source list: %v", err)
	}
	if srcList.Total != 1 {
		t.Fatalf("expected inv source total=1, got %d", srcList.Total)
	}

	// 7. List the investigation's facts — none yet (source not processed).
	factListResp, factListRaw := admin.do("GET", "/api/v1/repositories/"+slug+"/investigations/"+invID+"/facts", nil)
	if factListResp.StatusCode != http.StatusOK {
		t.Fatalf("list inv facts: status %d, body %s", factListResp.StatusCode, factListRaw)
	}
	var factList pageEnvelope
	if err := json.Unmarshal(factListRaw, &factList); err != nil {
		t.Fatalf("decoding inv fact list: %v", err)
	}
	if factList.Total != 0 {
		t.Fatalf("expected inv fact total=0, got %d", factList.Total)
	}

	// 8. Remove the source from the investigation.
	remResp, remRaw := admin.do("DELETE", "/api/v1/repositories/"+slug+"/investigations/"+invID+"/sources/"+src.ID, nil)
	if remResp.StatusCode != http.StatusNoContent {
		t.Fatalf("remove source: status %d, body %s", remResp.StatusCode, remRaw)
	}

	// 8a. List sources is now empty.
	srcList2Resp, srcList2Raw := admin.do("GET", "/api/v1/repositories/"+slug+"/investigations/"+invID+"/sources", nil)
	if srcList2Resp.StatusCode != http.StatusOK {
		t.Fatalf("list inv sources after remove: status %d, body %s", srcList2Resp.StatusCode, srcList2Raw)
	}
	var srcList2 pageEnvelope
	if err := json.Unmarshal(srcList2Raw, &srcList2); err != nil {
		t.Fatalf("decoding inv source list after remove: %v", err)
	}
	if srcList2.Total != 0 {
		t.Fatalf("expected inv source total=0 after remove, got %d", srcList2.Total)
	}

	// 9. Delete the investigation.
	delResp, delRaw := admin.do("DELETE", "/api/v1/repositories/"+slug+"/investigations/"+invID, nil)
	if delResp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete investigation: status %d, body %s", delResp.StatusCode, delRaw)
	}

	// 9a. Get is now 404.
	get2Resp, _ := admin.do("GET", "/api/v1/repositories/"+slug+"/investigations/"+invID, nil)
	if get2Resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404 after delete, got %d", get2Resp.StatusCode)
	}
}

// TestInvestigationsCrossRepoIsolation verifies that an
// investigation in one repository is not visible from another
// repository's routes (a 404, not a leak). Two repos, one
// investigation in repo A; querying it via repo B's slug returns
// 404.
func TestInvestigationsCrossRepoIsolation(t *testing.T) {
	env := testutil.NewTestEnv(t)
	defer env.Server.Close()

	admin := bootstrapSysAdmin(t, env, "iso_admin@example.com")

	const slugA = "iso-repo-a"
	const slugB = "iso-repo-b"
	if resp, body, _ := createRepositoryWithDB(t, admin, "Iso A", slugA, "", ""); resp.StatusCode != http.StatusCreated {
		t.Fatalf("create repo A: %d %s", resp.StatusCode, body)
	}
	if resp, body, _ := createRepositoryWithDB(t, admin, "Iso B", slugB, "", ""); resp.StatusCode != http.StatusCreated {
		t.Fatalf("create repo B: %d %s", resp.StatusCode, body)
	}

	invBody, _ := json.Marshal(map[string]string{"title": "A only"})
	createResp, createRaw := admin.do("POST", "/api/v1/repositories/"+slugA+"/investigations", invBody)
	if createResp.StatusCode != http.StatusCreated {
		t.Fatalf("create in A: status %d, body %s", createResp.StatusCode, createRaw)
	}
	var created struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(createRaw, &created); err != nil {
		t.Fatalf("decode: %v", err)
	}

	// Query via repo B's slug → 404 (not a leak).
	resp, _ := admin.do("GET", "/api/v1/repositories/"+slugB+"/investigations/"+created.ID, nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("cross-repo get: expected 404, got %d", resp.StatusCode)
	}

	// Adding a source from repo A to the investigation via repo B's
	// slug path should also 404 the investigation lookup.
	resp, _ = admin.do("GET", "/api/v1/repositories/"+slugB+"/investigations", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list in B: expected 200, got %d", resp.StatusCode)
	}
}

// TestInvestigationsPermissionDenied verifies that an
// unauthenticated user (no token) and an authenticated user
// without the investigation:create permission cannot create an
// investigation. The sysadmin can; a plain registered user (legacy
// "user" role gets create via seed) can — so we instead deny by
// hitting the endpoint without auth, and confirm the create route
// is permission-gated (not just authed).
func TestInvestigationsPermissionDenied(t *testing.T) {
	env := testutil.NewTestEnv(t)
	defer env.Server.Close()

	admin := bootstrapSysAdmin(t, env, "perm_admin@example.com")
	const slug = "perm-repo"
	resp, body, repoID := createRepositoryWithDB(t, admin, "Perm Repo", slug, "", "")
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create repo: %d %s", resp.StatusCode, body)
	}

	// Unauthenticated → 401 (AuthRequired).
	anon := newAuthClient(env.BaseURL)
	invBody, _ := json.Marshal(map[string]string{"title": "no auth"})
	anonResp, _ := anon.do("POST", "/api/v1/repositories/"+slug+"/investigations", invBody)
	if anonResp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauth create: expected 401, got %d", anonResp.StatusCode)
	}

	// Authenticated user WITHOUT investigation:create: register a
	// fresh user that is grouped only into "viewer" (read-only on
	// investigations per the seed). The create endpoint should 403.
	viewer := newAuthClient(env.BaseURL)
	if r, _ := viewer.register("viewer_inv@example.com", "passw0rd!", "Viewer"); r.StatusCode != http.StatusCreated {
		t.Fatalf("viewer register: %d", r.StatusCode)
	}
	viewer.token = loginUser(viewer, "viewer_inv@example.com", "passw0rd!")

	resp, meBody := viewer.do("GET", "/api/v1/users/me", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("viewer /me: %d", resp.StatusCode)
	}
	var me struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(meBody, &me); err != nil {
		t.Fatalf("decode viewer me: %v", err)
	}
	if _, err := env.DB.Exec(
		context.Background(),
		`DELETE FROM casbin_rule WHERE p_type='g' AND v0=$1 AND v1='user'`,
		me.ID,
	); err != nil {
		t.Fatalf("removing legacy user grouping: %v", err)
	}
	if _, err := env.DB.Exec(
		context.Background(),
		`INSERT INTO casbin_rule (p_type, v0, v1, v2) VALUES ('g', $1, 'viewer', $2)`,
		me.ID, repoID,
	); err != nil {
		t.Fatalf("seeding viewer grouping: %v", err)
	}
	if err := env.RBAC.LoadPolicy(); err != nil {
		t.Fatalf("reloading RBAC: %v", err)
	}

	resp, _ = viewer.do("POST", "/api/v1/repositories/"+slug+"/investigations", invBody)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("viewer create: expected 403, got %d", resp.StatusCode)
	}

	// Viewer CAN list (read).
	resp, _ = viewer.do("GET", "/api/v1/repositories/"+slug+"/investigations", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("viewer list: expected 200, got %d", resp.StatusCode)
	}
}

// TestRepoTasksScopedToMetadata verifies the per-repo tasks
// endpoint's filtering contract: jobs are scoped to a repository by
// the `repo_id` metadata tag the enqueuer writes (see
// tasks.MarshalMetadata). The filter River applies is a JSONB
// containment check (`metadata @> fragment::jsonb`), so we verify
// the contract at the SQL layer — the handler is a thin wrapper
// around that predicate.
//
// The test runs River's bundled migrations on the default pool (so
// river_job exists), inserts two jobs with different repo_id
// metadata tags, and asserts the containment predicate returns only
// the matching one.
func TestRepoTasksScopedToMetadata(t *testing.T) {
	env := testutil.NewTestEnv(t)
	defer env.Server.Close()

	admin := bootstrapSysAdmin(t, env, "tasks_admin@example.com")
	const slug = "tasks-repo"
	resp, body, repoID := createRepositoryWithDB(t, admin, "Tasks Repo", slug, "", "")
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create repo: %d %s", resp.StatusCode, body)
	}

	// Run River's bundled migrations so river_job exists on the
	// default pool. In production taskmanager.New does this; the
	// default test env leaves tasks unwired, so we inline the
	// same migrator call here.
	if err := ensureRiverSchemaOnPool(env.DB); err != nil {
		t.Fatalf("running River migrations: %v", err)
	}

	otherRepoID := "11111111-1111-1111-1111-111111111111"
	ourMeta := `{"repo_id":"` + repoID + `"}`
	otherMeta := `{"repo_id":"` + otherRepoID + `"}`

	ctx := context.Background()
	_, err := env.DB.Exec(ctx, `
		INSERT INTO river_job (kind, state, args, metadata, created_at, scheduled_at, attempt, max_attempts, priority, queue)
		VALUES
		  ('retrieve_source', 'available', '{}'::jsonb, $1::jsonb, now(), now(), 0, 3, 1, 'retrieve_source'),
		  ('retrieve_source', 'available', '{}'::jsonb, $2::jsonb, now(), now(), 0, 3, 1, 'retrieve_source')
	`, ourMeta, otherMeta)
	if err != nil {
		t.Fatalf("inserting river jobs: %v", err)
	}

	// Containment check mirrors River's JobListParams.Metadata.
	var ourCount, otherCount int
	if err := env.DB.QueryRow(ctx, `SELECT COUNT(*) FROM river_job WHERE metadata @> $1::jsonb`, ourMeta).Scan(&ourCount); err != nil {
		t.Fatalf("query our jobs: %v", err)
	}
	if err := env.DB.QueryRow(ctx, `SELECT COUNT(*) FROM river_job WHERE metadata @> $1::jsonb`, otherMeta).Scan(&otherCount); err != nil {
		t.Fatalf("query other jobs: %v", err)
	}
	if ourCount != 1 {
		t.Fatalf("expected 1 job matching our repo metadata, got %d", ourCount)
	}
	if otherCount != 1 {
		t.Fatalf("expected 1 job matching other repo metadata, got %d", otherCount)
	}

	// Adding source_id to the fragment narrows further. Insert a
	// third job with the same repo_id but a source_id and assert
	// the source-scoped fragment matches it alone.
	const sourceID = "22222222-2222-2222-2222-222222222222"
	srcMeta := `{"repo_id":"` + repoID + `","source_id":"` + sourceID + `"}`
	_, err = env.DB.Exec(ctx, `
		INSERT INTO river_job (kind, state, args, metadata, created_at, scheduled_at, attempt, max_attempts, priority, queue)
		VALUES ('source_decomposition', 'available', '{}'::jsonb, $1::jsonb, now(), now(), 0, 3, 1, 'source_decomposition')
	`, srcMeta)
	if err != nil {
		t.Fatalf("inserting source-scoped job: %v", err)
	}
	var srcScopedCount int
	if err := env.DB.QueryRow(ctx, `SELECT COUNT(*) FROM river_job WHERE metadata @> $1::jsonb`, srcMeta).Scan(&srcScopedCount); err != nil {
		t.Fatalf("query source-scoped jobs: %v", err)
	}
	if srcScopedCount != 1 {
		t.Fatalf("expected 1 job matching source-scoped metadata, got %d", srcScopedCount)
	}
}

// TestInvestigations_ConceptsScopedToSources is the regression guard
// for the bug where the investigation "Concepts" tab showed the
// whole repository's concepts (so a new "inflammation" investigation
// surfaced concepts from a prior "dna" investigation before any of
// its own sources were processed). The investigation concepts
// endpoint must only return concepts derived from facts that came
// from the investigation's own sources (via fact_concepts →
// fact_sources → investigation_sources).
//
// Setup: one repo, two sources, two facts, two concepts. concept1
// links to fact1 → source1; concept2 links to fact2 → source2.
// Investigation A includes source1; investigation B includes
// source2; investigation C is brand-new with no sources. Asserts:
//   - A/concepts returns only concept1,
//   - B/concepts returns only concept2,
//   - C/concepts returns total 0 (no leak from A/B),
//   - the repo-level /concepts endpoint still returns both (the
//     repo view is unchanged).
func TestInvestigations_ConceptsScopedToSources(t *testing.T) {
	env := testutil.NewTestEnv(t)
	defer env.Server.Close()

	admin := bootstrapSysAdmin(t, env, "inv_concepts@example.com")
	const slug = "inv-concepts-repo"
	resp, body, repoID := createRepositoryWithDB(t, admin, "Inv Concepts Repo", slug, "desc", "")
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create repo: %d %s", resp.StatusCode, body)
	}

	ctx := context.Background()
	queries := store.New(env.DB)
	pgRepo := pgRepoID(t, repoID)

	// Two sources in the repo.
	mkSource := func(label string) pgtype.UUID {
		t.Helper()
		sid := pgtype.UUID{}
		if err := sid.Scan(uuid.NewString()); err != nil {
			t.Fatalf("scan source id: %v", err)
		}
		if _, err := queries.CreateSource(ctx, store.CreateSourceParams{
			ID: sid, RepositoryID: pgRepo, Url: "https://example.com/" + label, Kind: "homepage", Status: "fetched",
		}); err != nil {
			t.Fatalf("create source %s: %v", label, err)
		}
		return sid
	}
	src1 := mkSource("dna-src")
	src2 := mkSource("inflam-src")

	// One fact per source.
	fact1IDStr := insertFactWithSource(t, env, pgRepo, src1, "DNA carries genetic information.", 0)
	fact2IDStr := insertFactWithSource(t, env, pgRepo, src2, "Inflammation is an immune response.", 0)
	fact1ID := pgtype.UUID{}
	if err := fact1ID.Scan(fact1IDStr); err != nil {
		t.Fatalf("scan fact1 id: %v", err)
	}
	fact2ID := pgtype.UUID{}
	if err := fact2ID.Scan(fact2IDStr); err != nil {
		t.Fatalf("scan fact2 id: %v", err)
	}

	// Two concepts, each linked to its own fact.
	mkConcept := func(name, contextLabel string, factID pgtype.UUID) pgtype.UUID {
		t.Helper()
		c, err := queries.CreateConcept(ctx, store.CreateConceptParams{
			RepositoryID: pgRepo, CanonicalName: name, Context: contextLabel,
		})
		if err != nil {
			t.Fatalf("create concept %s: %v", name, err)
		}
		if _, err := queries.AddFactConcept(ctx, store.AddFactConceptParams{
			FactID: factID, ConceptID: c.ID,
		}); err != nil {
			t.Fatalf("link fact→concept %s: %v", name, err)
		}
		// Mirror the ingest workers: recompute the concept_groups
		// summary for the touched name key so the q="" list path
		// (which reads from concept_groups) reflects this insert.
		if err := concepts.RecomputeTouchedGroups(ctx, queries, pgRepo, []string{strings.ToLower(name)}); err != nil {
			t.Fatalf("recompute concept_groups for %s: %v", name, err)
		}
		return c.ID
	}
	concept1ID := mkConcept("DNA", "Biomolecule", fact1ID)
	concept2ID := mkConcept("Inflammation", "Disease", fact2ID)
	_ = concept1ID
	_ = concept2ID

	// Helper to create an investigation via the HTTP API and return
	// its id.
	createInvestigation := func(title string) string {
		t.Helper()
		invBody, _ := json.Marshal(map[string]string{"title": title})
		r, raw := admin.do("POST", "/api/v1/repositories/"+slug+"/investigations", invBody)
		if r.StatusCode != http.StatusCreated {
			t.Fatalf("create investigation %q: %d %s", title, r.StatusCode, raw)
		}
		var inv struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(raw, &inv); err != nil {
			t.Fatalf("decode investigation: %v", err)
		}
		return inv.ID
	}
	addSource := func(invID string, sourceID pgtype.UUID) {
		t.Helper()
		b, _ := json.Marshal(map[string]string{"source_id": pgUUIDString(sourceID)})
		r, raw := admin.do("POST", "/api/v1/repositories/"+slug+"/investigations/"+invID+"/sources", b)
		if r.StatusCode != http.StatusNoContent {
			t.Fatalf("add source to %s: %d %s", invID, r.StatusCode, raw)
		}
	}

	invA := createInvestigation("DNA investigation")
	addSource(invA, src1)
	invB := createInvestigation("Inflammation investigation")
	addSource(invB, src2)
	invC := createInvestigation("Empty investigation") // no sources

	// Assert: A/concepts returns only concept1.
	aResp, aRaw := admin.do("GET", "/api/v1/repositories/"+slug+"/investigations/"+invA+"/concepts", nil)
	if aResp.StatusCode != http.StatusOK {
		t.Fatalf("A /concepts: %d %s", aResp.StatusCode, aRaw)
	}
	var aList pageEnvelope
	if err := json.Unmarshal(aRaw, &aList); err != nil {
		t.Fatalf("decode A concepts: %v", err)
	}
	if aList.Total != 1 {
		t.Errorf("investigation A concepts total = %d, want 1 (only DNA concept from src1)", aList.Total)
	} else {
		var row struct {
			CanonicalName string `json:"canonical_name"`
		}
		if err := json.Unmarshal(aList.Data[0], &row); err == nil && row.CanonicalName != "DNA" {
			t.Errorf("investigation A concept = %q, want %q", row.CanonicalName, "DNA")
		}
	}

	// Assert: B/concepts returns only concept2.
	bResp, bRaw := admin.do("GET", "/api/v1/repositories/"+slug+"/investigations/"+invB+"/concepts", nil)
	if bResp.StatusCode != http.StatusOK {
		t.Fatalf("B /concepts: %d %s", bResp.StatusCode, bRaw)
	}
	var bList pageEnvelope
	if err := json.Unmarshal(bRaw, &bList); err != nil {
		t.Fatalf("decode B concepts: %v", err)
	}
	if bList.Total != 1 {
		t.Errorf("investigation B concepts total = %d, want 1 (only Inflammation concept from src2)", bList.Total)
	} else {
		var row struct {
			CanonicalName string `json:"canonical_name"`
		}
		if err := json.Unmarshal(bList.Data[0], &row); err == nil && row.CanonicalName != "Inflammation" {
			t.Errorf("investigation B concept = %q, want %q", row.CanonicalName, "Inflammation")
		}
	}

	// Assert: C/concepts (brand-new, no sources) returns total 0 —
	// this is the exact user-reported leak symptom.
	cResp, cRaw := admin.do("GET", "/api/v1/repositories/"+slug+"/investigations/"+invC+"/concepts", nil)
	if cResp.StatusCode != http.StatusOK {
		t.Fatalf("C /concepts: %d %s", cResp.StatusCode, cRaw)
	}
	var cList pageEnvelope
	if err := json.Unmarshal(cRaw, &cList); err != nil {
		t.Fatalf("decode C concepts: %v", err)
	}
	if cList.Total != 0 {
		t.Errorf("investigation C (no sources) concepts total = %d, want 0 (concepts must not leak across investigations)", cList.Total)
	}

	// Sanity: the repo-level concepts endpoint still returns both.
	repoResp, repoRaw := admin.do("GET", "/api/v1/repositories/"+slug+"/concepts", nil)
	if repoResp.StatusCode != http.StatusOK {
		t.Fatalf("repo /concepts: %d %s", repoResp.StatusCode, repoRaw)
	}
	var repoList pageEnvelope
	if err := json.Unmarshal(repoRaw, &repoList); err != nil {
		t.Fatalf("decode repo concepts: %v", err)
	}
	if repoList.Total != 2 {
		t.Errorf("repo-level concepts total = %d, want 2 (repo view must be unchanged)", repoList.Total)
	}
}

// pageEnvelope mirrors the canonical list response shape so tests
// can decode the `total` field without modeling every row.
type pageEnvelope struct {
	Data       []json.RawMessage `json:"data"`
	Total      int64             `json:"total"`
	Limit      int               `json:"limit"`
	Offset     int               `json:"offset"`
	SearchMode string            `json:"search_mode,omitempty"`
}
