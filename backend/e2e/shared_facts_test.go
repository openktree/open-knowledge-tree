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

// sharedFactsSeed sets up a repo with three concepts (A, B, C) and a
// single source, then creates facts linked to concept groups so the
// intersection (shared) facts are exercisable:
//   - 2 facts linked to A+B+C (true 3-way intersection)
//   - 1 fact  linked to A+B only
//   - 1 fact  linked to A only
// It returns the three concept canonical names and the fact texts in
// each bucket so tests can assert membership precisely.
type sharedFactsSeed struct {
	slug     string
	repoID   string
	pgRepo   pgtype.UUID
	admin    *authClient
	a, b, c  string // canonical names
	abcTexts []string
	abText   string
	aOnly    string
}

func seedSharedFacts(t *testing.T, env *testutil.TestEnv, slug string) sharedFactsSeed {
	t.Helper()
	admin := bootstrapSysAdmin(t, env, slug+"@example.com")
	_, _, repoID := createRepositoryWithDB(t, admin, slug, slug, "desc", "")
	pgRepo := pgRepoID(t, repoID)
	ctx := context.Background()
	queries := store.New(env.DB)

	srcID := pgtype.UUID{}
	if err := srcID.Scan(uuid.NewString()); err != nil {
		t.Fatalf("scan source id: %v", err)
	}
	if _, err := queries.CreateSource(ctx, store.CreateSourceParams{
		ID:           srcID,
		RepositoryID: pgRepo,
		Url:          "https://example.com/" + slug,
		Kind:         "homepage",
		Status:       "fetched",
	}); err != nil {
		t.Fatalf("create source: %v", err)
	}

	mkConcept := func(name, ctxLabel string) string {
		c, err := queries.CreateConcept(ctx, store.CreateConceptParams{
			RepositoryID:  pgRepo,
			CanonicalName: name,
			Context:       ctxLabel,
		})
		if err != nil {
			t.Fatalf("create concept %q: %v", name, err)
		}
		return c.CanonicalName
	}
	a := mkConcept("Alpha", "Topic")
	b := mkConcept("Beta", "Topic")
	c := mkConcept("Gamma", "Topic")

	var chunk int32
	mkLinkedFact := func(text string, conceptIDs ...pgtype.UUID) {
		fidStr := insertFactWithSource(t, env, pgRepo, srcID, text, chunk)
		chunk++
		var fid pgtype.UUID
		if err := fid.Scan(fidStr); err != nil {
			t.Fatalf("scan fact id: %v", err)
		}
		if _, err := queries.MarkFactStatus(ctx, store.MarkFactStatusParams{ID: fid, Status: "stable"}); err != nil {
			t.Fatalf("mark fact stable: %v", err)
		}
		for _, cid := range conceptIDs {
			if _, err := queries.AddFactConcept(ctx, store.AddFactConceptParams{FactID: fid, ConceptID: cid}); err != nil {
				t.Fatalf("link fact %q to concept: %v", text, err)
			}
		}
	}
	conceptIDByName := func(name string) pgtype.UUID {
		rows, err := queries.ListConceptsByRepoName(ctx, store.ListConceptsByRepoNameParams{
			RepositoryID:  pgRepo,
			CanonicalName: name,
		})
		if err != nil || len(rows) == 0 {
			t.Fatalf("resolve concept %q: %v (rows=%d)", name, err, len(rows))
		}
		return rows[0].ID
	}
	idA := conceptIDByName(a)
	idB := conceptIDByName(b)
	idC := conceptIDByName(c)

	abcTexts := []string{"ABC-shared-1", "ABC-shared-2"}
	mkLinkedFact(abcTexts[0], idA, idB, idC)
	mkLinkedFact(abcTexts[1], idA, idB, idC)
	mkLinkedFact("AB-only-shared", idA, idB)
	mkLinkedFact("Alpha-only-fact", idA)

	return sharedFactsSeed{
		slug:     slug,
		repoID:   repoID,
		pgRepo:   pgRepo,
		admin:    admin,
		a:        a,
		b:        b,
		c:        c,
		abcTexts: abcTexts,
		abText:   "AB-only-shared",
		aOnly:    "Alpha-only-fact",
	}
}

// TestREST_ListRepoFacts_ConceptsIntersection verifies the REST
// GET /api/v1/repositories/{slug}/facts?concepts=A,B,C endpoint
// returns the intersection of facts linked to ALL given concepts,
// paginated, with correct counts for 2-way and 3-way intersections
// and the expected error cases.
func TestREST_ListRepoFacts_ConceptsIntersection(t *testing.T) {
	env := testutil.NewTestEnv(t)
	defer env.Server.Close()
	s := seedSharedFacts(t, env, "rest-sf")
	admin := s.admin

	type factRow struct {
		Text        string `json:"text"`
		SourceCount int64  `json:"source_count"`
	}
	type pageResp struct {
		Data   []factRow `json:"data"`
		Total  int64     `json:"total"`
		Limit  int       `json:"limit"`
		Offset int       `json:"offset"`
	}
	texts := func(p pageResp) []string {
		out := make([]string, 0, len(p.Data))
		for _, f := range p.Data {
			out = append(out, f.Text)
		}
		return out
	}

	// 3-way intersection A,B,C -> 2 facts.
	resp, body := admin.do("GET", "/api/v1/repositories/"+s.slug+"/facts?concepts="+s.a+","+s.b+","+s.c, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("3-way: status %d, body %s", resp.StatusCode, body)
	}
	var p3 pageResp
	if err := json.Unmarshal(body, &p3); err != nil {
		t.Fatalf("3-way unmarshal: %v: %s", err, body)
	}
	if p3.Total != 2 {
		t.Errorf("3-way total = %d, want 2", p3.Total)
	}
	if len(p3.Data) != 2 {
		t.Fatalf("3-way data = %d, want 2", len(p3.Data))
	}
	for _, want := range s.abcTexts {
		found := false
		for _, got := range texts(p3) {
			if got == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("3-way missing %q in %v", want, texts(p3))
		}
	}

	// 2-way intersection A,B -> 3 facts (2 ABC + 1 AB-only).
	resp, body = admin.do("GET", "/api/v1/repositories/"+s.slug+"/facts?concepts="+s.a+","+s.b, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("2-way: status %d, body %s", resp.StatusCode, body)
	}
	var p2 pageResp
	if err := json.Unmarshal(body, &p2); err != nil {
		t.Fatalf("2-way unmarshal: %v: %s", err, body)
	}
	if p2.Total != 3 {
		t.Errorf("2-way total = %d, want 3 (2 ABC + 1 AB-only)", p2.Total)
	}
	if len(p2.Data) != 3 {
		t.Fatalf("2-way data = %d, want 3", len(p2.Data))
	}

	// A,C intersection -> 2 facts (the two ABC ones; AB-only is excluded).
	resp, body = admin.do("GET", "/api/v1/repositories/"+s.slug+"/facts?concepts="+s.a+","+s.c, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("AC: status %d, body %s", resp.StatusCode, body)
	}
	var pAC pageResp
	if err := json.Unmarshal(body, &pAC); err != nil {
		t.Fatalf("AC unmarshal: %v: %s", err, body)
	}
	if pAC.Total != 2 {
		t.Errorf("AC total = %d, want 2", pAC.Total)
	}

	// B,C intersection -> 2 facts.
	resp, body = admin.do("GET", "/api/v1/repositories/"+s.slug+"/facts?concepts="+s.b+","+s.c, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("BC: status %d, body %s", resp.StatusCode, body)
	}
	var pBC pageResp
	if err := json.Unmarshal(body, &pBC); err != nil {
		t.Fatalf("BC unmarshal: %v: %s", err, body)
	}
	if pBC.Total != 2 {
		t.Errorf("BC total = %d, want 2", pBC.Total)
	}

	// Pagination: 2-way A,B with limit=1 offset=0 -> 1 row, total=3.
	resp, body = admin.do("GET", "/api/v1/repositories/"+s.slug+"/facts?concepts="+s.a+","+s.b+"&limit=1&offset=0", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("page1: status %d, body %s", resp.StatusCode, body)
	}
	var pp1 pageResp
	if err := json.Unmarshal(body, &pp1); err != nil {
		t.Fatalf("page1 unmarshal: %v: %s", err, body)
	}
	if pp1.Total != 3 {
		t.Errorf("page1 total = %d, want 3", pp1.Total)
	}
	if len(pp1.Data) != 1 {
		t.Errorf("page1 data = %d, want 1", len(pp1.Data))
	}
	if pp1.Offset != 0 {
		t.Errorf("page1 offset = %d, want 0", pp1.Offset)
	}

	// Error: single concept (< 2 distinct).
	resp, _ = admin.do("GET", "/api/v1/repositories/"+s.slug+"/facts?concepts="+s.a, nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("single concept: status = %d, want 400", resp.StatusCode)
	}

	// Error: nonexistent concept -> 400.
	resp, _ = admin.do("GET", "/api/v1/repositories/"+s.slug+"/facts?concepts="+s.a+",NonExistent", nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("nonexistent concept: status = %d, want 400", resp.StatusCode)
	}

	// Backward-compat: no `concepts` param -> all stable facts (4 total).
	resp, body = admin.do("GET", "/api/v1/repositories/"+s.slug+"/facts", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("no-filter: status %d, body %s", resp.StatusCode, body)
	}
	var pAll pageResp
	if err := json.Unmarshal(body, &pAll); err != nil {
		t.Fatalf("no-filter unmarshal: %v: %s", err, body)
	}
	if pAll.Total != 4 {
		t.Errorf("no-filter total = %d, want 4 (all facts)", pAll.Total)
	}
}

// TestMCP_SearchFacts_ConceptsIntersection verifies the MCP
// searchFacts tool with the `concepts` array returns the intersection
// of facts linked to ALL given concepts, paginated, plus the error
// cases. Mirrors TestREST_ListRepoFacts_ConceptsIntersection via the
// MCP transport.
func TestMCP_SearchFacts_ConceptsIntersection(t *testing.T) {
	env := testutil.NewTestEnv(t)
	defer env.Server.Close()
	s := seedSharedFacts(t, env, "mcp-sf")

	uid := resolveUserID(t, env, s.slug+"@example.com")
	tok := mintAccessToken(t, env, uid, s.slug+"@example.com", "test-client")

	type factOut struct {
		Text        string `json:"text"`
		SourceCount int64  `json:"source_count"`
	}
	type mcpResult struct {
		Result struct {
			StructuredContent struct {
				Facts    []factOut `json:"facts"`
				Total    int64     `json:"total"`
				Limit    int       `json:"limit"`
				Offset   int       `json:"offset"`
				Concepts []string  `json:"concepts"`
			} `json:"structuredContent"`
		} `json:"result"`
	}
	texts := func(r mcpResult) []string {
		out := make([]string, 0, len(r.Result.StructuredContent.Facts))
		for _, f := range r.Result.StructuredContent.Facts {
			out = append(out, f.Text)
		}
		return out
	}

	// 3-way intersection A,B,C -> 2 facts.
	status, body := mcpCall(t, env.BaseURL, tok, map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{
			"name": "searchFacts",
			"arguments": map[string]any{
				"repository": s.repoID,
				"concepts":   []string{s.a, s.b, s.c},
			},
		},
	})
	if status != http.StatusOK {
		t.Fatalf("3-way: status %d, body %s", status, body)
	}
	var r3 mcpResult
	if err := json.Unmarshal(body, &r3); err != nil {
		t.Fatalf("3-way unmarshal: %v: %s", err, body)
	}
	if r3.Result.StructuredContent.Total != 2 {
		t.Errorf("3-way total = %d, want 2", r3.Result.StructuredContent.Total)
	}
	if len(r3.Result.StructuredContent.Facts) != 2 {
		t.Fatalf("3-way facts = %d, want 2", len(r3.Result.StructuredContent.Facts))
	}
	for _, want := range s.abcTexts {
		found := false
		for _, got := range texts(r3) {
			if got == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("3-way missing %q in %v", want, texts(r3))
		}
	}
	if len(r3.Result.StructuredContent.Concepts) != 3 {
		t.Errorf("3-way concepts echo = %v, want 3 entries", r3.Result.StructuredContent.Concepts)
	}

	// 2-way intersection A,B -> 3 facts (2 ABC + 1 AB-only).
	status, body = mcpCall(t, env.BaseURL, tok, map[string]any{
		"jsonrpc": "2.0", "id": 2, "method": "tools/call",
		"params": map[string]any{
			"name": "searchFacts",
			"arguments": map[string]any{
				"repository": s.repoID,
				"concepts":   []string{s.a, s.b},
			},
		},
	})
	if status != http.StatusOK {
		t.Fatalf("2-way: status %d, body %s", status, body)
	}
	var r2 mcpResult
	if err := json.Unmarshal(body, &r2); err != nil {
		t.Fatalf("2-way unmarshal: %v: %s", err, body)
	}
	if r2.Result.StructuredContent.Total != 3 {
		t.Errorf("2-way total = %d, want 3 (2 ABC + 1 AB-only)", r2.Result.StructuredContent.Total)
	}
	if len(r2.Result.StructuredContent.Facts) != 3 {
		t.Fatalf("2-way facts = %d, want 3", len(r2.Result.StructuredContent.Facts))
	}

	// Pagination: 2-way A,B limit=1 offset=0 -> 1 row, total=3.
	status, body = mcpCall(t, env.BaseURL, tok, map[string]any{
		"jsonrpc": "2.0", "id": 3, "method": "tools/call",
		"params": map[string]any{
			"name": "searchFacts",
			"arguments": map[string]any{
				"repository": s.repoID,
				"concepts":   []string{s.a, s.b},
				"limit":      1,
				"offset":     0,
			},
		},
	})
	if status != http.StatusOK {
		t.Fatalf("page1: status %d, body %s", status, body)
	}
	var rp1 mcpResult
	if err := json.Unmarshal(body, &rp1); err != nil {
		t.Fatalf("page1 unmarshal: %v: %s", err, body)
	}
	if rp1.Result.StructuredContent.Total != 3 {
		t.Errorf("page1 total = %d, want 3", rp1.Result.StructuredContent.Total)
	}
	if len(rp1.Result.StructuredContent.Facts) != 1 {
		t.Errorf("page1 facts = %d, want 1", len(rp1.Result.StructuredContent.Facts))
	}
	if rp1.Result.StructuredContent.Offset != 0 {
		t.Errorf("page1 offset = %d, want 0", rp1.Result.StructuredContent.Offset)
	}

	// Error: single concept (< 2 distinct).
	status, body = mcpCall(t, env.BaseURL, tok, map[string]any{
		"jsonrpc": "2.0", "id": 4, "method": "tools/call",
		"params": map[string]any{
			"name": "searchFacts",
			"arguments": map[string]any{
				"repository": s.repoID,
				"concepts":   []string{s.a},
			},
		},
	})
	if status != http.StatusOK {
		t.Fatalf("single concept call: status %d, body %s", status, body)
	}
	if !mcpResultIsError(body) {
		t.Errorf("single concept: expected error result, got %s", body)
	}

	// Error: nonexistent concept -> tool error.
	status, body = mcpCall(t, env.BaseURL, tok, map[string]any{
		"jsonrpc": "2.0", "id": 5, "method": "tools/call",
		"params": map[string]any{
			"name": "searchFacts",
			"arguments": map[string]any{
				"repository": s.repoID,
				"concepts":   []string{s.a, "NonExistent"},
			},
		},
	})
	if status != http.StatusOK {
		t.Fatalf("nonexistent concept call: status %d, body %s", status, body)
	}
	if !mcpResultIsError(body) {
		t.Errorf("nonexistent concept: expected error result, got %s", body)
	}

	// Error: `concept` and `concepts` mutually exclusive.
	status, body = mcpCall(t, env.BaseURL, tok, map[string]any{
		"jsonrpc": "2.0", "id": 6, "method": "tools/call",
		"params": map[string]any{
			"name": "searchFacts",
			"arguments": map[string]any{
				"repository": s.repoID,
				"concept":    s.a,
				"concepts":   []string{s.a, s.b},
			},
		},
	})
	if status != http.StatusOK {
		t.Fatalf("mutual-exclusion call: status %d, body %s", status, body)
	}
	if !mcpResultIsError(body) {
		t.Errorf("mutual-exclusion: expected error result, got %s", body)
	}

	// Backward-compat: single `concept` (not `concepts`) still works
	// and returns the union of A's facts (4 total for Alpha here:
	// 2 ABC + 1 AB + 1 A-only). This guards against the new branch
	// breaking the existing single-concept path.
	status, body = mcpCall(t, env.BaseURL, tok, map[string]any{
		"jsonrpc": "2.0", "id": 7, "method": "tools/call",
		"params": map[string]any{
			"name": "searchFacts",
			"arguments": map[string]any{
				"repository": s.repoID,
				"concept":    s.a,
			},
		},
	})
	if status != http.StatusOK {
		t.Fatalf("backward-compat single concept: status %d, body %s", status, body)
	}
	var rSingle mcpResult
	if err := json.Unmarshal(body, &rSingle); err != nil {
		t.Fatalf("backward-compat unmarshal: %v: %s", err, body)
	}
	if rSingle.Result.StructuredContent.Total != 4 {
		t.Errorf("backward-compat single concept total = %d, want 4 (all facts linked to Alpha)", rSingle.Result.StructuredContent.Total)
	}
}

// mcpResultIsError returns true when an MCP JSON-RPC response carries
// an `isError: true` flag on the result (tool error path) instead of
// structured content.
func mcpResultIsError(body []byte) bool {
	var probe struct {
		Result struct {
			IsError bool `json:"isError"`
		} `json:"result"`
	}
	_ = json.Unmarshal(body, &probe)
	return probe.Result.IsError
}