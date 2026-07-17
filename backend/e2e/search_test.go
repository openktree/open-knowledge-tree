//go:build e2e

package e2e_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/openktree/open-knowledge-tree/backend/e2e/testutil"
	"github.com/openktree/open-knowledge-tree/backend/internal/providers/search"
)

// stubSearchProvider is a deterministic, in-memory SearchProvider
// the e2e tests use to exercise the TestSearch HTTP layer without
// taking a runtime dependency on Serper/OpenAlex API keys. It
// returns a fixed result set and a configurable next cursor so
// the pagination tests can assert on cursor advancement.
type stubSearchProvider struct {
	results    []search.SearchResult
	total      int64
	nextCursor string
}

func (p *stubSearchProvider) Search(ctx context.Context, query string, opts search.SearchOptions) (search.SearchResponse, error) {
	// Echo a next cursor that depends on the inbound cursor so the
	// test can prove the handler forwards opts.Cursor to the
	// provider and returns the provider's NextCursor verbatim.
	return search.SearchResponse{
		Results:    p.results,
		Total:      p.total,
		NextCursor:  p.nextCursor,
	}, nil
}

// grantSourceRead grants the "user" role the "source_provider:read"
// and "source_provider:execute" permissions so /sources/providers
// and /sources/{provider}/search succeed. Mirrors
// grantSourceProviderExecute in sources_test.go but adds the read
// permission too (TestSearch route uses source_provider:execute,
// ListProviders uses source_provider:read).
func grantSourceReadAndExecute(t *testing.T, env *testutil.TestEnv, userID string) {
	t.Helper()
	// Grant sysadmin on both domains so the user can access
	// source_provider and repository endpoints.
	if _, err := env.DB.Exec(
		context.Background(),
		`INSERT INTO casbin_rule (p_type, v0, v1, v2) VALUES ('g', $1, 'sysadmin', '*')`,
		userID,
	); err != nil {
		t.Fatalf("seeding sysadmin grouping policy: %v", err)
	}
	if _, err := env.DB.Exec(
		context.Background(),
		`INSERT INTO casbin_rule (p_type, v0, v1, v2) VALUES ('g', $1, 'sysadmin', 'system')`,
		userID,
	); err != nil {
		t.Fatalf("seeding sysadmin system grouping policy: %v", err)
	}
	if err := env.RBAC.LoadPolicy(); err != nil {
		t.Fatalf("reloading RBAC policy: %v", err)
	}
}

// insertSourceRow inserts a source row directly into the
// per-repo okt_repository.sources table with the given URL, DOI,
// and status. Used to seed an "already fetched" source so the
// TestSearch already-exists tagging has something to match
// against. Returns the row's UUID.
func insertSourceRow(t *testing.T, env *testutil.TestEnv, repoID pgtype.UUID, url, doi, status string) {
	t.Helper()
	ctx := context.Background()
	var id pgtype.UUID
	if err := id.Scan("00000000-0000-0000-0000-000000000001"); err != nil {
		t.Fatalf("scanning seed id: %v", err)
	}
	_, err := env.DB.Exec(ctx, `
		INSERT INTO okt_repository.sources (id, repository_id, url, kind, status, doi)
		VALUES ($1, $2, $3, 'homepage', $4, NULLIF($5, ''))
	`, id, repoID, url, status, doi)
	if err != nil {
		t.Fatalf("inserting seed source row: %v", err)
	}
}

// TestSearchReturnsEnvelope proves the TestSearch handler returns
// the new paginated envelope (results, total, next_cursor,
// per_page) rather than a bare array, and that the query is
// forwarded to the provider.
func TestSearchReturnsEnvelope(t *testing.T) {
	provider := &stubSearchProvider{
		results: []search.SearchResult{
			{Title: "Hit 1", URL: "https://example.com/1", Snippet: "s1"},
			{Title: "Hit 2", URL: "https://example.com/2", Snippet: "s2"},
		},
		total:      42,
		nextCursor: "page2",
	}
	env := testutil.NewTestEnvWithSearch(t, map[string]search.SearchProvider{"stub": provider})
	client := newAuthClient(env.BaseURL)

	client.register("envelope@example.com", "password123", "Envelope User")
	client.token = loginUser(client, "envelope@example.com", "password123")
	_, meBody := client.do("GET", "/api/v1/users/me", nil)
	var me struct{ ID string `json:"id"` }
	json.Unmarshal(meBody, &me)
	grantSourceReadAndExecute(t, env, me.ID)
	client.token = loginUser(client, "envelope@example.com", "password123")

	body, _ := json.Marshal(map[string]interface{}{
		"query":    "machine learning",
		"per_page": 2,
		"cursor":   "page1",
	})
	resp, raw := client.do("POST", "/api/v1/sources/stub/search", body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, raw)
	}

	var out struct {
		Results    []search.SearchResult `json:"results"`
		Total      int64                 `json:"total"`
		NextCursor string                 `json:"next_cursor"`
		PerPage    int                    `json:"per_page"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal envelope: %v\nraw: %s", err, raw)
	}
	if len(out.Results) != 2 {
		t.Fatalf("expected 2 results, got %d", len(out.Results))
	}
	if out.Total != 42 {
		t.Errorf("total = %d, want 42", out.Total)
	}
	if out.NextCursor != "page2" {
		t.Errorf("next_cursor = %q, want page2", out.NextCursor)
	}
	if out.PerPage != 2 {
		t.Errorf("per_page = %d, want 2", out.PerPage)
	}
}

// TestSearchMarksAlreadyAdded proves that when the request carries
// a repository_id and the repository already has a matching source
// row, the handler tags the matching result with already_exists
// and existing_status. Non-matching results stay untagged.
func TestSearchMarksAlreadyAdded(t *testing.T) {
	provider := &stubSearchProvider{
		results: []search.SearchResult{
			{Title: "Fetched", URL: "https://example.com/fetched", DOI: "10.1/abc"},
			{Title: "New", URL: "https://example.com/new", DOI: "10.1/new"},
		},
		total: 2,
	}
	env := testutil.NewTestEnvWithSearch(t, map[string]search.SearchProvider{"stub": provider})
	client := newAuthClient(env.BaseURL)

	client.register("already@example.com", "password123", "Already User")
	client.token = loginUser(client, "already@example.com", "password123")
	_, meBody := client.do("GET", "/api/v1/users/me", nil)
	var me struct{ ID string `json:"id"` }
	json.Unmarshal(meBody, &me)
	grantSourceReadAndExecute(t, env, me.ID)
	client.token = loginUser(client, "already@example.com", "password123")

	// Bootstrap a repo (sysadmin not required for create — any
	// authenticated user can create), then seed a source row that
	// matches the first result by URL.
	_, _, repoIDStr := createRepositoryWithDB(t, client, "Search Repo", "search-repo", "desc", "")
	var repoID pgtype.UUID
	if err := repoID.Scan(repoIDStr); err != nil {
		t.Fatalf("scanning repo id: %v", err)
	}
	insertSourceRow(t, env, repoID, "https://example.com/fetched", "10.1/abc", "fetched")

	body, _ := json.Marshal(map[string]interface{}{
		"query":         "anything",
		"repository_id": repoIDStr,
	})
	resp, raw := client.do("POST", "/api/v1/sources/stub/search", body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, raw)
	}

	var out struct {
		Results []search.SearchResult `json:"results"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(out.Results) != 2 {
		t.Fatalf("expected 2 results, got %d", len(out.Results))
	}
	if !out.Results[0].AlreadyExists {
		t.Errorf("result[0] (URL match) already_exists = false, want true")
	}
	if out.Results[0].ExistingStatus == nil || *out.Results[0].ExistingStatus != "fetched" {
		t.Errorf("result[0] existing_status = %v, want \"fetched\"", out.Results[0].ExistingStatus)
	}
	if out.Results[1].AlreadyExists {
		t.Errorf("result[1] (no match) already_exists = true, want false")
	}
}

// TestSearchMarksAlreadyAddedByDOI proves the DOI branch of the
// matcher: a result whose URL differs from the stored source's
// URL but whose DOI matches is still tagged already_exists. This
// is the case the OR clause in the SQL exists to catch (same
// paper fetched via a different URL).
func TestSearchMarksAlreadyAddedByDOI(t *testing.T) {
	provider := &stubSearchProvider{
		results: []search.SearchResult{
			{Title: "Same paper, different URL", URL: "https://publisher.example/landing", DOI: "10.555/xyz"},
		},
		total: 1,
	}
	env := testutil.NewTestEnvWithSearch(t, map[string]search.SearchProvider{"stub": provider})
	client := newAuthClient(env.BaseURL)

	client.register("doi@example.com", "password123", "DOI User")
	client.token = loginUser(client, "doi@example.com", "password123")
	_, meBody := client.do("GET", "/api/v1/users/me", nil)
	var me struct{ ID string `json:"id"` }
	json.Unmarshal(meBody, &me)
	grantSourceReadAndExecute(t, env, me.ID)
	client.token = loginUser(client, "doi@example.com", "password123")

	_, _, repoIDStr := createRepositoryWithDB(t, client, "DOI Repo", "doi-repo", "desc", "")
	var repoID pgtype.UUID
	if err := repoID.Scan(repoIDStr); err != nil {
		t.Fatalf("scanning repo id: %v", err)
	}
	// Stored row fetched via doi.org (a different URL than the
	// search result) but with the same DOI.
	insertSourceRow(t, env, repoID, "https://doi.org/10.555/xyz", "10.555/xyz", "fetched")

	body, _ := json.Marshal(map[string]interface{}{
		"query":         "anything",
		"repository_id": repoIDStr,
	})
	resp, raw := client.do("POST", "/api/v1/sources/stub/search", body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, raw)
	}

	var out struct {
		Results []search.SearchResult `json:"results"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(out.Results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(out.Results))
	}
	if !out.Results[0].AlreadyExists {
		t.Errorf("DOI-only match: already_exists = false, want true")
	}
}

// TestSearchRequiresAuth proves the search endpoint is gated by
// the auth + permission middleware like the other source-provider
// endpoints.
func TestSearchRequiresAuth(t *testing.T) {
	provider := &stubSearchProvider{results: []search.SearchResult{}}
	env := testutil.NewTestEnvWithSearch(t, map[string]search.SearchProvider{"stub": provider})
	client := newAuthClient(env.BaseURL)

	body, _ := json.Marshal(map[string]string{"query": "x"})
	resp, _ := client.do("POST", "/api/v1/sources/stub/search", body)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 without auth, got %d", resp.StatusCode)
	}
}

// TestSearchUnknownProvider proves an unknown provider id yields
// 503 (the "search provider not available" path), matching the
// pre-pagination behavior.
func TestSearchUnknownProvider(t *testing.T) {
	env := testutil.NewTestEnvWithSearch(t, map[string]search.SearchProvider{"stub": &stubSearchProvider{}})
	client := newAuthClient(env.BaseURL)

	client.register("unknown@example.com", "password123", "Unknown User")
	client.token = loginUser(client, "unknown@example.com", "password123")
	_, meBody := client.do("GET", "/api/v1/users/me", nil)
	var me struct{ ID string `json:"id"` }
	json.Unmarshal(meBody, &me)
	grantSourceReadAndExecute(t, env, me.ID)
	client.token = loginUser(client, "unknown@example.com", "password123")

	body, _ := json.Marshal(map[string]string{"query": "x"})
	resp, raw := client.do("POST", "/api/v1/sources/nope/search", body)
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 for unknown provider, got %d: %s", resp.StatusCode, raw)
	}
}

// TestSearchMissingQuery proves the "query is required" error path.
func TestSearchMissingQuery(t *testing.T) {
	env := testutil.NewTestEnvWithSearch(t, map[string]search.SearchProvider{"stub": &stubSearchProvider{}})
	client := newAuthClient(env.BaseURL)

	client.register("noquery@example.com", "password123", "NoQuery User")
	client.token = loginUser(client, "noquery@example.com", "password123")
	_, meBody := client.do("GET", "/api/v1/users/me", nil)
	var me struct{ ID string `json:"id"` }
	json.Unmarshal(meBody, &me)
	grantSourceReadAndExecute(t, env, me.ID)
	client.token = loginUser(client, "noquery@example.com", "password123")

	resp, raw := client.do("POST", "/api/v1/sources/stub/search", []byte(`{}`))
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", resp.StatusCode, raw)
	}
}

// TestSearchRepoIDHeaderFallback proves that when repository_id
// is absent from the body, the handler falls back to the
// X-Repository-ID header (set by the frontend API client) and
// still performs the already-added tagging.
func TestSearchRepoIDHeaderFallback(t *testing.T) {
	provider := &stubSearchProvider{
		results: []search.SearchResult{
			{Title: "X", URL: "https://example.com/header", DOI: "10.0/h"},
		},
		total: 1,
	}
	env := testutil.NewTestEnvWithSearch(t, map[string]search.SearchProvider{"stub": provider})
	client := newAuthClient(env.BaseURL)

	client.register("header@example.com", "password123", "Header User")
	client.token = loginUser(client, "header@example.com", "password123")
	_, meBody := client.do("GET", "/api/v1/users/me", nil)
	var me struct{ ID string `json:"id"` }
	json.Unmarshal(meBody, &me)
	grantSourceReadAndExecute(t, env, me.ID)
	client.token = loginUser(client, "header@example.com", "password123")

	_, _, repoIDStr := createRepositoryWithDB(t, client, "Header Repo", "header-repo", "desc", "")
	var repoID pgtype.UUID
	if err := repoID.Scan(repoIDStr); err != nil {
		t.Fatalf("scanning repo id: %v", err)
	}
	insertSourceRow(t, env, repoID, "https://example.com/header", "10.0/h", "pending")

	// Body deliberately omits repository_id; rely on the header.
	body, _ := json.Marshal(map[string]string{"query": "x"})
	req, _ := http.NewRequest("POST", client.baseURL+"/api/v1/sources/stub/search", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+client.token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Repository-ID", repoIDStr)
	resp, err := client.httpClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, raw)
	}

	var out struct {
		Results []search.SearchResult `json:"results"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(out.Results) != 1 || !out.Results[0].AlreadyExists {
		t.Fatalf("expected header-fallback match, got %+v", out.Results)
	}
	if out.Results[0].ExistingStatus == nil || *out.Results[0].ExistingStatus != "pending" {
		t.Errorf("existing_status = %v, want pending", out.Results[0].ExistingStatus)
	}
}