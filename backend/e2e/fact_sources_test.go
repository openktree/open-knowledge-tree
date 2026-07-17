//go:build e2e

package e2e_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/openktree/open-knowledge-tree/backend/e2e/testutil"
	"github.com/openktree/open-knowledge-tree/backend/internal/store"
)

// insertFactWithSource is a test helper that inserts a fact row
// and links it to a source via the fact_sources junction. It
// mirrors what source_decomposition does after the schema
// rewrite: CreateFact (no source_id) + AddFactSource. Returns
// the fact UUID string. Tests use this to set up dedup scenarios
// without running the full decomposition worker.
func insertFactWithSource(t *testing.T, env *testutil.TestEnv, repoID, sourceID pgtype.UUID, text string, chunkIndex int32) string {
	t.Helper()
	ctx := context.Background()
	queries := store.New(env.DB)
	factID := pgtype.UUID{}
	if err := factID.Scan(uuid.New().String()); err != nil {
		t.Fatalf("scanning fact id: %v", err)
	}
	created, err := queries.CreateFact(ctx, store.CreateFactParams{ID: factID, Text: text, FactKind: "text"})
	if err != nil {
		t.Fatalf("create fact: %v", err)
	}
	if err := queries.AddFactSource(ctx, store.AddFactSourceParams{
		FactID:     created.ID,
		SourceID:   sourceID,
		ChunkIndex: chunkIndex,
	}); err != nil {
		t.Fatalf("add fact source: %v", err)
	}
	return pgUUIDString(created.ID)
}

// pgRepoID scans a repo UUID string (the form returned by
// createRepositoryWithDB) into a pgtype.UUID so it can be passed
// to store query params that expect pgtype.UUID. Tests that pass
// the repoID directly to SQL queries (pgx converts string→UUID)
// don't need this; tests that build store params do.
func pgRepoID(t *testing.T, repoID string) pgtype.UUID {
	t.Helper()
	var id pgtype.UUID
	if err := id.Scan(repoID); err != nil {
		t.Fatalf("scanning repo id %q: %v", repoID, err)
	}
	return id
}

// pgUUIDString formats a pgtype.UUID as a canonical lowercase
// UUID string. Used when a worker Args field needs the string
// form of a pgtype.UUID the test holds.
func pgUUIDString(id pgtype.UUID) string {
	if !id.Valid {
		return ""
	}
	b := id.Bytes
	return uuid.UUID(b).String()
}

// TestFactSources_AddFactSourceIdempotent verifies the junction's
// ON CONFLICT clause: linking the same fact to the same source
// twice (e.g. a re-process) doesn't double-count. source_count
// stays 1 after a second AddFactSource call with the same pair.
func TestFactSources_AddFactSourceIdempotent(t *testing.T) {
	env := testutil.NewTestEnv(t)
	defer env.Server.Close()

	admin := bootstrapSysAdmin(t, env, "fs_idem@example.com")
	_, _, repoID := createRepositoryWithDB(t, admin, "FS Idem", "fs-idem", "desc", "")

	// Create a source directly in the DB (skip the fetch worker
	// — the test only needs a row with a repository_id).
	sourceID := pgtype.UUID{}
	if err := sourceID.Scan(uuid.New().String()); err != nil {
		t.Fatalf("scanning source id: %v", err)
	}
	queries := store.New(env.DB)
	if _, err := queries.CreateSource(context.Background(), store.CreateSourceParams{
		ID:           sourceID,
		RepositoryID: pgRepoID(t, repoID),
		Url:          "https://example.com/fs-idem",
		Kind:         "homepage",
		Status:       "fetched",
	}); err != nil {
		t.Fatalf("create source: %v", err)
	}

	factIDStr := insertFactWithSource(t, env, pgRepoID(t, repoID), sourceID, "idempotency-test-fact", 0)

	// Re-link the same fact+source. The ON CONFLICT clause must
	// make this a no-op (no duplicate row, source_count stays 1).
	var factID pgtype.UUID
	if err := factID.Scan(factIDStr); err != nil {
		t.Fatalf("scanning fact id: %v", err)
	}
	if err := queries.AddFactSource(context.Background(), store.AddFactSourceParams{
		FactID:     factID,
		SourceID:   sourceID,
		ChunkIndex: 1, // different chunk_index, same (fact,source) pair
	}); err != nil {
		t.Fatalf("re-add fact source: %v", err)
	}

	// source_count must be 1 (one source linked, even though we
	// called AddFactSource twice).
	rows, err := queries.ListFactsByRepoWithSourceCount(context.Background(), store.ListFactsByRepoWithSourceCountParams{
		RepositoryID: pgRepoID(t, repoID),
		Column2:      "",
		Column3:      "",
		Column4:      "",
		Limit:        1000,
		Offset:       0,
	})
	if err != nil {
		t.Fatalf("listing facts with source_count: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected 1 fact row, got %d", len(rows))
	}
	if rows[0].SourceCount != 1 {
		t.Errorf("source_count = %d, want 1 (AddFactSource must be idempotent)", rows[0].SourceCount)
	}
}

// TestFactSources_ListWithSourceCount verifies the
// ListFactsByRepoWithSourceCount query returns the correct
// source_count per fact when a fact is confirmed by multiple
// sources. The test inserts one fact linked to three sources and
// asserts source_count=3.
func TestFactSources_ListWithSourceCount(t *testing.T) {
	env := testutil.NewTestEnv(t)
	defer env.Server.Close()

	admin := bootstrapSysAdmin(t, env, "fs_count@example.com")
	_, _, repoID := createRepositoryWithDB(t, admin, "FS Count", "fs-count", "desc", "")
	queries := store.New(env.DB)

	// Three sources in the same repo.
	sourceIDs := make([]pgtype.UUID, 3)
	for i := 0; i < 3; i++ {
		id := pgtype.UUID{}
		if err := id.Scan(uuid.New().String()); err != nil {
			t.Fatalf("scanning source id: %v", err)
		}
		sourceIDs[i] = id
		if _, err := queries.CreateSource(context.Background(), store.CreateSourceParams{
			ID:           id,
			RepositoryID: pgRepoID(t, repoID),
			Url:          "https://example.com/fs-count/" + uuid.NewString()[:8],
			Kind:          "homepage",
			Status:        "fetched",
		}); err != nil {
			t.Fatalf("create source %d: %v", i, err)
		}
	}

	// One fact linked to all three sources (the cross-source
	// confirmation scenario the dedup pipeline produces).
	factID := pgtype.UUID{}
	if err := factID.Scan(uuid.New().String()); err != nil {
		t.Fatalf("scanning fact id: %v", err)
	}
	if _, err := queries.CreateFact(context.Background(), store.CreateFactParams{ID: factID, Text: "confirmed-by-three", FactKind: "text"}); err != nil {
		t.Fatalf("create fact: %v", err)
	}
	for i, sid := range sourceIDs {
		if err := queries.AddFactSource(context.Background(), store.AddFactSourceParams{
			FactID:     factID,
			SourceID:   sid,
			ChunkIndex: int32(i),
		}); err != nil {
			t.Fatalf("add fact source: %v", err)
		}
	}

	rows, err := queries.ListFactsByRepoWithSourceCount(context.Background(), store.ListFactsByRepoWithSourceCountParams{
		RepositoryID: pgRepoID(t, repoID),
		Column2:      "",
		Column3:      "",
		Column4:      "",
		Limit:        1000,
		Offset:       0,
	})
	if err != nil {
		t.Fatalf("listing facts with source_count: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected 1 fact row, got %d", len(rows))
	}
	if rows[0].SourceCount != 3 {
		t.Errorf("source_count = %d, want 3", rows[0].SourceCount)
	}
	if rows[0].Text != "confirmed-by-three" {
		t.Errorf("text = %q, want %q", rows[0].Text, "confirmed-by-three")
	}
}

// TestFactSources_GetFactEndpoint verifies the new
// GET /{slug}/facts/{factID} endpoint returns the fact + full
// source list (id, url, parsed_title, chunk_index,
// first_seen_at) + source_count. Also covers the repo-ownership
// enforcement: a fact whose sources belong to a different
// repository returns 404, not a leak.
func TestFactSources_GetFactEndpoint(t *testing.T) {
	env := testutil.NewTestEnv(t)
	defer env.Server.Close()

	admin := bootstrapSysAdmin(t, env, "fs_getfact@example.com")
	resp, body, repoID := createRepositoryWithDB(t, admin, "FS GetFact", "fs-getfact", "desc", "")
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create repo: %d %s", resp.StatusCode, body)
	}

	queries := store.New(env.DB)
	sourceID := pgtype.UUID{}
	if err := sourceID.Scan(uuid.New().String()); err != nil {
		t.Fatalf("scanning source id: %v", err)
	}
	if _, err := queries.CreateSource(context.Background(), store.CreateSourceParams{
		ID:           sourceID,
		RepositoryID: pgRepoID(t, repoID),
		Url:          "https://example.com/fs-getfact-source",
		Kind:          "homepage",
		Status:        "fetched",
	}); err != nil {
		t.Fatalf("create source: %v", err)
	}
	// Set a parsed_title so the endpoint surfaces it.
	title := "FS GetFact Source"
	if _, err := env.DB.Exec(context.Background(),
		`UPDATE okt_repository.sources SET parsed_title = $1 WHERE id = $2`,
		title, sourceID); err != nil {
		t.Fatalf("setting parsed_title: %v", err)
	}

	factIDStr := insertFactWithSource(t, env, pgRepoID(t, repoID), sourceID, "getfact-test-fact", 7)

	// The route uses {repoID} (UUID or slug). We use the slug
	// the create-repository call set, so the middleware's
	// slug→repoID path is exercised.
	gotResp, gotBody := admin.do("GET", "/api/v1/repositories/fs-getfact/facts/"+factIDStr, nil)
	if gotResp.StatusCode != http.StatusOK {
		t.Fatalf("GET fact: status %d, body %s", gotResp.StatusCode, gotBody)
	}
	var parsed struct {
		Fact struct {
			ID   string `json:"id"`
			Text string `json:"text"`
		} `json:"fact"`
		Sources []struct {
			SourceID    string `json:"source_id"`
			Url         string `json:"url"`
			ParsedTitle *string `json:"parsed_title"`
			ChunkIndex  int32  `json:"chunk_index"`
		} `json:"sources"`
		SourceCount int `json:"source_count"`
		Concepts []struct {
			ID            string `json:"id"`
			CanonicalName string `json:"canonical_name"`
			Context       string `json:"context"`
		} `json:"concepts"`
		ConceptCount int `json:"concept_count"`
	}
	if err := json.Unmarshal(gotBody, &parsed); err != nil {
		t.Fatalf("unmarshal fact detail: %v", err)
	}
	if parsed.Fact.ID != factIDStr {
		t.Errorf("fact.id = %q, want %q", parsed.Fact.ID, factIDStr)
	}
	if parsed.Fact.Text != "getfact-test-fact" {
		t.Errorf("fact.text = %q, want %q", parsed.Fact.Text, "getfact-test-fact")
	}
	if len(parsed.Sources) != 1 {
		t.Fatalf("expected 1 source, got %d", len(parsed.Sources))
	}
	if parsed.Sources[0].Url != "https://example.com/fs-getfact-source" {
		t.Errorf("source.url = %q, want the created source url", parsed.Sources[0].Url)
	}
	if parsed.Sources[0].ParsedTitle == nil || *parsed.Sources[0].ParsedTitle != title {
		t.Errorf("source.parsed_title = %v, want %q", parsed.Sources[0].ParsedTitle, title)
	}
	if parsed.Sources[0].ChunkIndex != 7 {
		t.Errorf("source.chunk_index = %d, want 7", parsed.Sources[0].ChunkIndex)
	}
	if parsed.SourceCount != 1 {
		t.Errorf("source_count = %d, want 1", parsed.SourceCount)
	}
	// No concept linked yet → concepts must be empty (but the
	// fields must be present so the frontend's inline path works).
	if parsed.ConceptCount != 0 {
		t.Errorf("concept_count = %d, want 0 (no concept linked yet)", parsed.ConceptCount)
	}
	if len(parsed.Concepts) != 0 {
		t.Errorf("concepts = %d entries, want 0", len(parsed.Concepts))
	}

	// Link a concept to the fact and refetch; the concept must
	// appear inline in the response (the consolidation that
	// mirrors the MCP getFact shape).
	concept, err := queries.CreateConcept(context.Background(), store.CreateConceptParams{
		RepositoryID:  pgRepoID(t, repoID),
		CanonicalName: "GetFactConcept",
		Context:       "TestContext",
	})
	if err != nil {
		t.Fatalf("create concept: %v", err)
	}
	var factID pgtype.UUID
	if err := factID.Scan(factIDStr); err != nil {
		t.Fatalf("scan fact id: %v", err)
	}
	if _, err := queries.AddFactConcept(context.Background(), store.AddFactConceptParams{
		FactID: factID, ConceptID: concept.ID,
	}); err != nil {
		t.Fatalf("link fact concept: %v", err)
	}
	gotResp2, gotBody2 := admin.do("GET", "/api/v1/repositories/fs-getfact/facts/"+factIDStr, nil)
	if gotResp2.StatusCode != http.StatusOK {
		t.Fatalf("GET fact (with concept): status %d, body %s", gotResp2.StatusCode, gotBody2)
	}
	var parsed2 struct {
		Concepts []struct {
			ID            string `json:"id"`
			CanonicalName string `json:"canonical_name"`
			Context       string `json:"context"`
		} `json:"concepts"`
		ConceptCount int `json:"concept_count"`
	}
	if err := json.Unmarshal(gotBody2, &parsed2); err != nil {
		t.Fatalf("unmarshal fact detail (with concept): %v", err)
	}
	if parsed2.ConceptCount != 1 {
		t.Errorf("concept_count = %d, want 1", parsed2.ConceptCount)
	}
	if len(parsed2.Concepts) != 1 {
		t.Fatalf("concepts = %d entries, want 1", len(parsed2.Concepts))
	}
	if parsed2.Concepts[0].CanonicalName != "GetFactConcept" {
		t.Errorf("concept.canonical_name = %q, want GetFactConcept", parsed2.Concepts[0].CanonicalName)
	}
	if parsed2.Concepts[0].Context != "TestContext" {
		t.Errorf("concept.context = %q, want TestContext", parsed2.Concepts[0].Context)
	}
	if parsed2.Concepts[0].ID != concept.ID.String() {
		t.Errorf("concept.id = %q, want %q", parsed2.Concepts[0].ID, concept.ID.String())
	}

	// Repo-ownership enforcement: create a second repo + second
	// source in that repo, link a fact to the second source, and
	// fetch it via the FIRST repo's slug. The endpoint must
	// return 404 (the fact's sources don't belong to repo1).
	_, _, repo2ID := createRepositoryWithDB(t, admin, "FS GetFact 2", "fs-getfact-2", "desc", "")
	source2ID := pgtype.UUID{}
	if err := source2ID.Scan(uuid.New().String()); err != nil {
		t.Fatalf("scanning source2 id: %v", err)
	}
	if _, err := queries.CreateSource(context.Background(), store.CreateSourceParams{
		ID:           source2ID,
		RepositoryID: pgRepoID(t, repo2ID),
		Url:          "https://example.com/fs-getfact-2-source",
		Kind:          "homepage",
		Status:        "fetched",
	}); err != nil {
		t.Fatalf("create source2: %v", err)
	}
	fact2IDStr := insertFactWithSource(t, env, pgRepoID(t, repo2ID), source2ID, "repo2-fact", 0)
	crossResp, _ := admin.do("GET", "/api/v1/repositories/fs-getfact/facts/"+fact2IDStr, nil)
	if crossResp.StatusCode != http.StatusNotFound {
		t.Errorf("cross-repo fetch: status = %d, want 404 (repo ownership must be enforced)", crossResp.StatusCode)
	}
}

// TestFactSources_ListRepoFactsSortParam verifies the optional
// sort=source_count query param re-sorts the bounded result set
// by source_count desc. The test inserts two facts: one with
// source_count=1, one with source_count=3, and asserts the
// "most confirmed" sort surfaces the 3-source fact first.
func TestFactSources_ListRepoFactsSortParam(t *testing.T) {
	env := testutil.NewTestEnv(t)
	defer env.Server.Close()

	admin := bootstrapSysAdmin(t, env, "fs_sort@example.com")
	_, _, repoID := createRepositoryWithDB(t, admin, "FS Sort", "fs-sort", "desc", "")
	queries := store.New(env.DB)

	// Three sources for the high-count fact, one for the low.
	mkSource := func(slug string) pgtype.UUID {
		id := pgtype.UUID{}
		if err := id.Scan(uuid.New().String()); err != nil {
			t.Fatalf("scanning source id: %v", err)
		}
		if _, err := queries.CreateSource(context.Background(), store.CreateSourceParams{
			ID:           id,
			RepositoryID: pgRepoID(t, repoID),
			Url:          "https://example.com/fs-sort/" + slug,
			Kind:          "homepage",
			Status:        "fetched",
		}); err != nil {
			t.Fatalf("create source: %v", err)
		}
		return id
	}
	hiSources := []pgtype.UUID{mkSource("hi1"), mkSource("hi2"), mkSource("hi3")}
	loSource := mkSource("lo1")

	mkFact := func(text string, sources []pgtype.UUID) {
		fid := pgtype.UUID{}
		if err := fid.Scan(uuid.New().String()); err != nil {
			t.Fatalf("scanning fact id: %v", err)
		}
		if _, err := queries.CreateFact(context.Background(), store.CreateFactParams{ID: fid, Text: text, FactKind: "text"}); err != nil {
			t.Fatalf("create fact: %v", err)
		}
		for i, sid := range sources {
			if err := queries.AddFactSource(context.Background(), store.AddFactSourceParams{
				FactID: fid, SourceID: sid, ChunkIndex: int32(i),
			}); err != nil {
				t.Fatalf("add fact source: %v", err)
			}
		}
	}
	mkFact("low-count", []pgtype.UUID{loSource})
	mkFact("high-count", hiSources)

	// Default sort (created_at desc): both facts present; order
	// is by created_at. We don't assert order here, just that
	// both are returned with their counts.
	resp, body := admin.do("GET", "/api/v1/repositories/fs-sort/facts?status=all", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list facts default: status %d, body %s", resp.StatusCode, body)
	}
	var defParsed struct {
		Data []struct {
			Text        string `json:"text"`
			SourceCount int64  `json:"source_count"`
		} `json:"data"`
		Total  int64 `json:"total"`
		Limit  int   `json:"limit"`
		Offset int   `json:"offset"`
	}
	if err := json.Unmarshal(body, &defParsed); err != nil {
		t.Fatalf("unmarshal default list: %v", err)
	}
	if len(defParsed.Data) != 2 {
		t.Fatalf("expected 2 facts, got %d", len(defParsed.Data))
	}
	if defParsed.Total != 2 {
		t.Errorf("default list total = %d, want 2", defParsed.Total)
	}
	if defParsed.Limit != 100 {
		t.Errorf("default list limit = %d, want 100 (default page size)", defParsed.Limit)
	}
	if defParsed.Offset != 0 {
		t.Errorf("default list offset = %d, want 0", defParsed.Offset)
	}

	// sort=source_count: high-count must come first. The sort is
	// now pushed into SQL, so this also exercises the CASE WHEN
	// $4 = 'source_count' ORDER BY branch.
	sortResp, sortBody := admin.do("GET", "/api/v1/repositories/fs-sort/facts?status=all&sort=source_count", nil)
	if sortResp.StatusCode != http.StatusOK {
		t.Fatalf("list facts sorted: status %d, body %s", sortResp.StatusCode, sortBody)
	}
	var sortParsed struct {
		Data []struct {
			Text        string `json:"text"`
			SourceCount int64  `json:"source_count"`
		} `json:"data"`
	}
	if err := json.Unmarshal(sortBody, &sortParsed); err != nil {
		t.Fatalf("unmarshal sorted list: %v", err)
	}
	if len(sortParsed.Data) != 2 {
		t.Fatalf("expected 2 facts sorted, got %d", len(sortParsed.Data))
	}
	if sortParsed.Data[0].Text != "high-count" {
		t.Errorf("sort=source_count: first fact = %q, want %q (most confirmed first)", sortParsed.Data[0].Text, "high-count")
	}
	if sortParsed.Data[0].SourceCount != 3 {
		t.Errorf("sort=source_count: first source_count = %d, want 3", sortParsed.Data[0].SourceCount)
	}
	if sortParsed.Data[1].Text != "low-count" {
		t.Errorf("sort=source_count: second fact = %q, want %q", sortParsed.Data[1].Text, "low-count")
	}
	if sortParsed.Data[1].SourceCount != 1 {
		t.Errorf("sort=source_count: second source_count = %d, want 1", sortParsed.Data[1].SourceCount)
	}

	// Pagination: limit=1&offset=0 returns 1 row, total=2.
	pageResp, pageBody := admin.do("GET", "/api/v1/repositories/fs-sort/facts?status=all&limit=1&offset=0", nil)
	if pageResp.StatusCode != http.StatusOK {
		t.Fatalf("list facts paginated: status %d, body %s", pageResp.StatusCode, pageBody)
	}
	var pageParsed struct {
		Data   []json.RawMessage `json:"data"`
		Total  int64             `json:"total"`
		Limit  int               `json:"limit"`
		Offset int               `json:"offset"`
	}
	if err := json.Unmarshal(pageBody, &pageParsed); err != nil {
		t.Fatalf("unmarshal paginated list: %v", err)
	}
	if len(pageParsed.Data) != 1 {
		t.Errorf("paginated: expected 1 fact on page, got %d", len(pageParsed.Data))
	}
	if pageParsed.Total != 2 {
		t.Errorf("paginated: total = %d, want 2", pageParsed.Total)
	}
	if pageParsed.Limit != 1 {
		t.Errorf("paginated: limit = %d, want 1", pageParsed.Limit)
	}

	// limit above the cap (200) must be clamped, not rejected.
	clampResp, clampBody := admin.do("GET", "/api/v1/repositories/fs-sort/facts?status=all&limit=99999", nil)
	if clampResp.StatusCode != http.StatusOK {
		t.Fatalf("list facts clamped: status %d, body %s", clampResp.StatusCode, clampBody)
	}
	var clampParsed struct {
		Limit int `json:"limit"`
	}
	if err := json.Unmarshal(clampBody, &clampParsed); err != nil {
		t.Fatalf("unmarshal clamped list: %v", err)
	}
	if clampParsed.Limit != 200 {
		t.Errorf("clamped: limit = %d, want 200 (max page size cap)", clampParsed.Limit)
	}

	// Search: q=high-count narrows the set to the one matching
	// fact. websearch_to_tsquery parses the hyphenated token.
	searchResp, searchBody := admin.do("GET", "/api/v1/repositories/fs-sort/facts?status=all&q=high-count", nil)
	if searchResp.StatusCode != http.StatusOK {
		t.Fatalf("list facts searched: status %d, body %s", searchResp.StatusCode, searchBody)
	}
	var searchParsed struct {
		Data  []struct {
			Text string `json:"text"`
		} `json:"data"`
		Total int64 `json:"total"`
	}
	if err := json.Unmarshal(searchBody, &searchParsed); err != nil {
		t.Fatalf("unmarshal searched list: %v", err)
	}
	if searchParsed.Total != 1 {
		t.Errorf("search: total = %d, want 1", searchParsed.Total)
	}
	if len(searchParsed.Data) != 1 || searchParsed.Data[0].Text != "high-count" {
		t.Errorf("search: data = %+v, want one high-count fact", searchParsed.Data)
	}

	// Search with no matches: empty page, zero total.
	missResp, missBody := admin.do("GET", "/api/v1/repositories/fs-sort/facts?status=all&q=zzz-no-such-fact", nil)
	if missResp.StatusCode != http.StatusOK {
		t.Fatalf("list facts search-miss: status %d, body %s", missResp.StatusCode, missBody)
	}
	var missParsed struct {
		Data  []json.RawMessage `json:"data"`
		Total int64             `json:"total"`
	}
	if err := json.Unmarshal(missBody, &missParsed); err != nil {
		t.Fatalf("unmarshal search-miss list: %v", err)
	}
	if missParsed.Total != 0 || len(missParsed.Data) != 0 {
		t.Errorf("search-miss: total=%d data=%d, want 0/0", missParsed.Total, len(missParsed.Data))
	}
}

// guard against unused imports when the file is edited.
var _ = strings.Contains
var _ = time.Second