//go:build e2e

package e2e_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/openktree/open-knowledge-tree/backend/e2e/testutil"
	"github.com/openktree/open-knowledge-tree/backend/internal/oauth"
	"github.com/openktree/open-knowledge-tree/backend/internal/providers/search"
	"github.com/openktree/open-knowledge-tree/backend/internal/store"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
)

// mcpCall posts a single JSON-RPC request to the MCP endpoint with
// the given bearer access token and returns the raw response body.
// Tests use this for initialize / tools/list / tools/call. The
// endpoint is stateless (WithStateLess), so each call is
// independent — no session id, no initialize handshake needed for
// tools/list or tools/call.
func mcpCall(t *testing.T, baseURL, accessToken string, req map[string]any) (int, []byte) {
	t.Helper()
	body, _ := json.Marshal(req)
	httpReq, _ := http.NewRequest(http.MethodPost, baseURL+"/api/v1/mcp", bytes.NewReader(body))
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json, text/event-stream")
	if accessToken != "" {
		httpReq.Header.Set("Authorization", "Bearer "+accessToken)
	}
	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		t.Fatalf("mcp call: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, raw
}

// mintAccessToken builds a valid OAuth access JWT for the given user
// directly via oauth.IssueAccessToken. The MCP e2e tests use this to
// skip the browser authorize flow (which the oauth_test covers) and
// exercise the MCP resource-server side in isolation. The issuer is
// the test env's configured issuer (or the localhost fallback).
func mintAccessToken(t *testing.T, env *testutil.TestEnv, userID pgtype.UUID, email, clientID string) string {
	t.Helper()
	issuer := env.Config.OAuth.Issuer
	if issuer == "" {
		issuer = "http://localhost:8080"
	}
	tok, err := oauth.IssueAccessToken(env.Config.Auth.JWTSecret, issuer, 15*time.Minute, userID, email, clientID, oauth.Scope)
	if err != nil {
		t.Fatalf("minting access token: %v", err)
	}
	return tok
}

// resolveUserID looks up the OKT user id by email so the test can
// mint a token carrying it. Mirrors the flow the authorize endpoint
// would have resolved.
func resolveUserID(t *testing.T, env *testutil.TestEnv, email string) pgtype.UUID {
	t.Helper()
	user, err := store.New(env.DB).GetUserByEmail(context.Background(), email)
	if err != nil {
		t.Fatalf("resolving user id for %s: %v", email, err)
	}
	return user.ID
}

// TestMCP_NoBearer_Rejects verifies the MCP endpoint requires a
// valid OAuth bearer token. A request with no Authorization header
// must get a 401 with a WWW-Authenticate pointing at the
// protected-resource metadata URL.
func TestMCP_NoBearer_Rejects(t *testing.T) {
	env := testutil.NewTestEnv(t)
	defer env.Server.Close()

	status, body := mcpCall(t, env.BaseURL, "", map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/list",
	})
	if status != http.StatusUnauthorized {
		t.Fatalf("no bearer: expected 401, got %d: %s", status, body)
	}
}

// TestMCP_InvalidBearer_Rejects verifies a malformed JWT is rejected
// with 401. A random string is not a valid HS256-signed token.
func TestMCP_InvalidBearer_Rejects(t *testing.T) {
	env := testutil.NewTestEnv(t)
	defer env.Server.Close()

	status, body := mcpCall(t, env.BaseURL, "not-a-jwt", map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/list",
	})
	if status != http.StatusUnauthorized {
		t.Fatalf("invalid bearer: expected 401, got %d: %s", status, body)
	}
}

// TestMCP_ToolsList verifies the three tools are advertised with the
// expected names. The tools/list response shape (per MCP spec) is
// {result: {tools: [{name, description, inputSchema}]}}.
func TestMCP_ToolsList(t *testing.T) {
	env := testutil.NewTestEnv(t)
	defer env.Server.Close()

	_ = registerTestUser(t, env, "mcp-list@example.com", "password123", "MCP List")
	uid := resolveUserID(t, env, "mcp-list@example.com")
	tok := mintAccessToken(t, env, uid, "mcp-list@example.com", "test-client")

	status, body := mcpCall(t, env.BaseURL, tok, map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/list",
	})
	if status != http.StatusOK {
		t.Fatalf("tools/list: expected 200, got %d: %s", status, body)
	}
	var resp struct {
		Result struct {
			Tools []struct {
				Name string `json:"name"`
			} `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("tools/list unmarshal: %v", body)
	}
	names := map[string]bool{}
	for _, tool := range resp.Result.Tools {
		names[tool.Name] = true
	}
	for _, want := range []string{
		"getRepositories", "searchFacts", "getFact",
		"searchConcepts", "getConcept", "getConceptSummaries",
		"getRelatedConcepts", "getInvestigation",
		"createInvestigation", "addInvestigationSource",
		"fetchAndProcessSource", "getSourceTasks", "searchSources",
		"listSearchProviders", "listReports",
	} {
		if !names[want] {
			t.Fatalf("tools/list: missing %q (have %v)", want, names)
		}
	}
}

// TestMCP_GetRepositories verifies the getRepositories tool returns
// the repositories the authenticated user can see. A fresh user
// with a single owned repo should see exactly that one with the
// admin role.
func TestMCP_GetRepositories(t *testing.T) {
	env := testutil.NewTestEnv(t)
	defer env.Server.Close()

	admin := bootstrapSysAdmin(t, env, "mcp-repos@example.com")
	_, _, repoID := createRepositoryWithDB(t, admin, "MCP Repo", "mcp-repo", "desc", "")
	if repoID == "" {
		t.Fatal("setup: failed to create repository")
	}

	uid := resolveUserID(t, env, "mcp-repos@example.com")
	tok := mintAccessToken(t, env, uid, "mcp-repos@example.com", "test-client")

	status, body := mcpCall(t, env.BaseURL, tok, map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/call",
		"params": map[string]any{
			"name": "getRepositories",
			"arguments": map[string]any{},
		},
	})
	if status != http.StatusOK {
		t.Fatalf("getRepositories: expected 200, got %d: %s", status, body)
	}
	var resp struct {
		Result struct {
			StructuredContent struct {
				Repositories []struct {
					ID    string   `json:"id"`
					Slug  string   `json:"slug"`
					Name  string   `json:"name"`
					Roles []string `json:"roles"`
				} `json:"repositories"`
			} `json:"structuredContent"`
		} `json:"result"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("getRepositories unmarshal: %v: %s", err, body)
	}
	if len(resp.Result.StructuredContent.Repositories) == 0 {
		t.Fatal("getRepositories: expected at least one repository")
	}
	found := false
	for _, r := range resp.Result.StructuredContent.Repositories {
		if r.ID == repoID {
			found = true
			if r.Slug != "mcp-repo" {
				t.Fatalf("getRepositories: expected slug mcp-repo, got %q", r.Slug)
			}
		}
	}
	if !found {
		t.Fatalf("getRepositories: created repo %s not in list", repoID)
	}
}

// TestMCP_SearchFacts verifies searchFacts returns up to 10 facts
// with source_count, paginated by offset. The test seeds 3 facts
// (one stable, two new) and asserts only the stable one is
// returned by default (the REST default status=stable carries over).
func TestMCP_SearchFacts(t *testing.T) {
	env := testutil.NewTestEnv(t)
	defer env.Server.Close()

	admin := bootstrapSysAdmin(t, env, "mcp-search@example.com")
	_, _, repoID := createRepositoryWithDB(t, admin, "MCP Search", "mcp-search", "desc", "")
	pgRepo := pgRepoID(t, repoID)

	// Seed a source + 3 facts. The repo-wide facts query defaults
	// to status='stable' (mirroring the REST endpoint), so we
	// mark one stable and the others new to verify the filter.
	sourceID := pgtype.UUID{}
	if err := sourceID.Scan(uuid.New().String()); err != nil {
		t.Fatal(err)
	}
	queries := store.New(env.DB)
	if _, err := queries.CreateSource(context.Background(), store.CreateSourceParams{
		ID:           sourceID,
		RepositoryID: pgRepo,
		Url:          "https://example.com/mcp-search",
		Kind:         "homepage",
		Status:       "fetched",
	}); err != nil {
		t.Fatalf("create source: %v", err)
	}
	stableFact := insertFactWithSource(t, env, pgRepo, sourceID, "The sky is blue.", 0)
	// Promote to stable directly via MarkFactStatus.
	stableUUID := pgtype.UUID{}
	stableUUID.Scan(stableFact)
	if _, err := queries.MarkFactStatus(context.Background(), store.MarkFactStatusParams{
		ID:     stableUUID,
		Status: "stable",
	}); err != nil {
		t.Fatalf("mark stable: %v", err)
	}
	insertFactWithSource(t, env, pgRepo, sourceID, "A new fact one.", 1)
	insertFactWithSource(t, env, pgRepo, sourceID, "A new fact two.", 2)

	uid := resolveUserID(t, env, "mcp-search@example.com")
	tok := mintAccessToken(t, env, uid, "mcp-search@example.com", "test-client")

	status, body := mcpCall(t, env.BaseURL, tok, map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/call",
		"params": map[string]any{
			"name": "searchFacts",
			"arguments": map[string]any{
				"repository": repoID,
			},
		},
	})
	if status != http.StatusOK {
		t.Fatalf("searchFacts: expected 200, got %d: %s", status, body)
	}
	var resp struct {
		Result struct {
			StructuredContent struct {
				Facts []struct {
					ID          string `json:"id"`
					Text        string `json:"text"`
					SourceCount int64  `json:"source_count"`
				} `json:"facts"`
				Total int `json:"total"`
			} `json:"structuredContent"`
		} `json:"result"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("searchFacts unmarshal: %v: %s", err, body)
	}
	if len(resp.Result.StructuredContent.Facts) != 1 {
		t.Fatalf("searchFacts: expected 1 stable fact, got %d (total=%d)", len(resp.Result.StructuredContent.Facts), resp.Result.StructuredContent.Total)
	}
	if resp.Result.StructuredContent.Facts[0].Text != "The sky is blue." {
		t.Fatalf("searchFacts: unexpected text %q", resp.Result.StructuredContent.Facts[0].Text)
	}
	if resp.Result.StructuredContent.Facts[0].SourceCount != 1 {
		t.Fatalf("searchFacts: expected source_count=1, got %d", resp.Result.StructuredContent.Facts[0].SourceCount)
	}

	// Search with a query that doesn't match — expect zero facts.
	status, body = mcpCall(t, env.BaseURL, tok, map[string]any{
		"jsonrpc": "2.0",
		"id":      2,
		"method":  "tools/call",
		"params": map[string]any{
			"name": "searchFacts",
			"arguments": map[string]any{
				"repository": repoID,
				"query":      "nonexistent-term-xyz",
			},
		},
	})
	if status != http.StatusOK {
		t.Fatalf("searchFacts no-match: expected 200, got %d: %s", status, body)
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("searchFacts no-match unmarshal: %v", err)
	}
	if len(resp.Result.StructuredContent.Facts) != 0 {
		t.Fatalf("searchFacts no-match: expected 0 facts, got %d", len(resp.Result.StructuredContent.Facts))
	}
}

// TestMCP_SearchFacts_LimitCapsAt200 verifies the searchFacts limit
// argument is clamped at 200 (the upper bound) even when the caller
// asks for more. A request for 1000 must be clamped to 200.
func TestMCP_SearchFacts_LimitCapsAt200(t *testing.T) {
	env := testutil.NewTestEnv(t)
	defer env.Server.Close()

	admin := bootstrapSysAdmin(t, env, "mcp-cap@example.com")
	_, _, repoID := createRepositoryWithDB(t, admin, "MCP Cap", "mcp-cap", "desc", "")
	pgRepo := pgRepoID(t, repoID)

	sourceID := pgtype.UUID{}
	sourceID.Scan(uuid.New().String())
	queries := store.New(env.DB)
	queries.CreateSource(context.Background(), store.CreateSourceParams{
		ID: sourceID, RepositoryID: pgRepo, Url: "https://example.com/cap", Kind: "homepage", Status: "fetched",
	})
	// Seed 3 stable facts (fewer than 200 so the clamp is what's
	// being asserted, not the returned count).
	for i := 0; i < 3; i++ {
		fidStr := insertFactWithSource(t, env, pgRepo, sourceID, "cap fact", int32(i))
		var fid pgtype.UUID
		fid.Scan(fidStr)
		queries.MarkFactStatus(context.Background(), store.MarkFactStatusParams{ID: fid, Status: "stable"})
	}

	uid := resolveUserID(t, env, "mcp-cap@example.com")
	tok := mintAccessToken(t, env, uid, "mcp-cap@example.com", "test-client")

	status, body := mcpCall(t, env.BaseURL, tok, map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/call",
		"params": map[string]any{
			"name": "searchFacts",
			"arguments": map[string]any{
				"repository": repoID,
				"limit":      1000, // ask for 1000, must be clamped to 200
			},
		},
	})
	if status != http.StatusOK {
		t.Fatalf("searchFacts cap: expected 200, got %d: %s", status, body)
	}
	var resp struct {
		Result struct {
			StructuredContent struct {
				Facts []struct {
					ID string `json:"id"`
				} `json:"facts"`
				Limit int `json:"limit"`
			} `json:"structuredContent"`
		} `json:"result"`
	}
	json.Unmarshal(body, &resp)
	if resp.Result.StructuredContent.Limit != 200 {
		t.Fatalf("searchFacts cap: expected limit=200, got %d", resp.Result.StructuredContent.Limit)
	}
	if len(resp.Result.StructuredContent.Facts) != 3 {
		t.Fatalf("searchFacts cap: expected 3 facts returned (seeded), got %d", len(resp.Result.StructuredContent.Facts))
	}
}

// TestMCP_GetFact verifies getFact returns the fact metadata + the
// full source URL list. The seeded source has a known URL the test
// asserts on.
func TestMCP_GetFact(t *testing.T) {
	env := testutil.NewTestEnv(t)
	defer env.Server.Close()

	admin := bootstrapSysAdmin(t, env, "mcp-getfact@example.com")
	_, _, repoID := createRepositoryWithDB(t, admin, "MCP GetFact", "mcp-getfact", "desc", "")
	pgRepo := pgRepoID(t, repoID)

	sourceID := pgtype.UUID{}
	sourceID.Scan(uuid.New().String())
	const sourceURL = "https://example.com/mcp-getfact-source"
	queries := store.New(env.DB)
	if _, err := queries.CreateSource(context.Background(), store.CreateSourceParams{
		ID: sourceID, RepositoryID: pgRepo, Url: sourceURL, Kind: "homepage", Status: "fetched",
	}); err != nil {
		t.Fatalf("create source: %v", err)
	}
	factIDStr := insertFactWithSource(t, env, pgRepo, sourceID, "A verifiable fact.", 0)
	var factID pgtype.UUID
	factID.Scan(factIDStr)
	queries.MarkFactStatus(context.Background(), store.MarkFactStatusParams{ID: factID, Status: "stable"})

	// Seed a concept and link it to the fact so getFact surfaces the
	// concepts the extract_concepts worker would have produced.
	conceptDesc := "The concept of verifiability"
	concept, err := queries.CreateConcept(context.Background(), store.CreateConceptParams{
		RepositoryID:  pgRepo,
		CanonicalName: "Verifiability",
		Context:       "ScientificMethod",
		Description:   &conceptDesc,
	})
	if err != nil {
		t.Fatalf("create concept: %v", err)
	}
	if _, err := queries.AddFactConcept(context.Background(), store.AddFactConceptParams{
		FactID:    factID,
		ConceptID: concept.ID,
	}); err != nil {
		t.Fatalf("link fact concept: %v", err)
	}

	uid := resolveUserID(t, env, "mcp-getfact@example.com")
	tok := mintAccessToken(t, env, uid, "mcp-getfact@example.com", "test-client")

	status, body := mcpCall(t, env.BaseURL, tok, map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/call",
		"params": map[string]any{
			"name": "getFact",
			"arguments": map[string]any{
				"repository": repoID,
				"factId":     factIDStr,
			},
		},
	})
	if status != http.StatusOK {
		t.Fatalf("getFact: expected 200, got %d: %s", status, body)
	}
	var resp struct {
		Result struct {
			StructuredContent struct {
				Fact struct {
					Text string `json:"text"`
				} `json:"fact"`
				Sources []struct {
					URL string `json:"url"`
				} `json:"sources"`
				SourceCount int `json:"source_count"`
				Concepts []struct {
					ID            string `json:"id"`
					CanonicalName string `json:"canonical_name"`
					Context      string `json:"context"`
					Slug         string `json:"slug"`
					Description  string `json:"description"`
				} `json:"concepts"`
				ConceptCount int `json:"concept_count"`
			} `json:"structuredContent"`
		} `json:"result"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("getFact unmarshal: %v: %s", err, body)
	}
	if resp.Result.StructuredContent.Fact.Text != "A verifiable fact." {
		t.Fatalf("getFact: unexpected text %q", resp.Result.StructuredContent.Fact.Text)
	}
	if resp.Result.StructuredContent.SourceCount != 1 {
		t.Fatalf("getFact: expected source_count=1, got %d", resp.Result.StructuredContent.SourceCount)
	}
	if len(resp.Result.StructuredContent.Sources) != 1 || resp.Result.StructuredContent.Sources[0].URL != sourceURL {
		t.Fatalf("getFact: expected source url %q, got %+v", sourceURL, resp.Result.StructuredContent.Sources)
	}
	// The linked concept must appear with its canonical name, context,
	// and description.
	if resp.Result.StructuredContent.ConceptCount != 1 {
		t.Fatalf("getFact: expected concept_count=1, got %d", resp.Result.StructuredContent.ConceptCount)
	}
	if len(resp.Result.StructuredContent.Concepts) != 1 {
		t.Fatalf("getFact: expected 1 concept, got %d", len(resp.Result.StructuredContent.Concepts))
	}
	got := resp.Result.StructuredContent.Concepts[0]
	if got.CanonicalName != "Verifiability" {
		t.Fatalf("getFact: concept canonical_name: expected Verifiability, got %q", got.CanonicalName)
	}
	if got.Context != "ScientificMethod" {
		t.Fatalf("getFact: concept context: expected ScientificMethod, got %q", got.Context)
	}
	if got.Description != conceptDesc {
		t.Fatalf("getFact: concept description: expected %q, got %q", conceptDesc, got.Description)
	}
	if got.ID != concept.ID.String() {
		t.Fatalf("getFact: concept id: expected %s, got %s", concept.ID.String(), got.ID)
	}
}

// TestMCP_GetFact_NotFound verifies a non-existent fact id returns a
// tool error (isError=true with a "fact not found" message), not a
// 404 — the MCP spec says tool errors should be in the result so
// the LLM can see them.
func TestMCP_GetFact_NotFound(t *testing.T) {
	env := testutil.NewTestEnv(t)
	defer env.Server.Close()

	admin := bootstrapSysAdmin(t, env, "mcp-404@example.com")
	_, _, repoID := createRepositoryWithDB(t, admin, "MCP 404", "mcp-404", "desc", "")

	uid := resolveUserID(t, env, "mcp-404@example.com")
	tok := mintAccessToken(t, env, uid, "mcp-404@example.com", "test-client")

	// Use a random UUID as the factId — no such fact exists.
	missingID := uuid.New().String()
	status, body := mcpCall(t, env.BaseURL, tok, map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/call",
		"params": map[string]any{
			"name": "getFact",
			"arguments": map[string]any{
				"repository": repoID,
				"factId":     missingID,
			},
		},
	})
	if status != http.StatusOK {
		t.Fatalf("getFact 404: expected 200 (tool error in body), got %d: %s", status, body)
	}
	var resp struct {
		Result struct {
			IsError bool `json:"isError"`
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("getFact 404 unmarshal: %v: %s", err, body)
	}
	if !resp.Result.IsError {
		t.Fatal("getFact 404: expected isError=true")
	}
	if len(resp.Result.Content) == 0 || !containsStr(resp.Result.Content[0].Text, "not found") {
		t.Fatalf("getFact 404: expected 'not found' in content, got %+v", resp.Result.Content)
	}
}

// TestMCP_SearchFacts_BadRepo verifies a non-existent repository
// returns a tool error (not a 500 and not a 404 — the LLM should
// see "repository not found" so it can call getRepositories to
// recover).
func TestMCP_SearchFacts_BadRepo(t *testing.T) {
	env := testutil.NewTestEnv(t)
	defer env.Server.Close()

	_ = registerTestUser(t, env, "mcp-badrepo@example.com", "password123", "MCP BadRepo")
	uid := resolveUserID(t, env, "mcp-badrepo@example.com")
	tok := mintAccessToken(t, env, uid, "mcp-badrepo@example.com", "test-client")

	status, body := mcpCall(t, env.BaseURL, tok, map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/call",
		"params": map[string]any{
			"name": "searchFacts",
			"arguments": map[string]any{
				"repository": "no-such-slug-or-uuid",
			},
		},
	})
	if status != http.StatusOK {
		t.Fatalf("searchFacts bad repo: expected 200 (tool error), got %d: %s", status, body)
	}
	var resp struct {
		Result struct {
			IsError bool `json:"isError"`
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
	}
	json.Unmarshal(body, &resp)
	if !resp.Result.IsError {
		t.Fatal("searchFacts bad repo: expected isError=true")
	}
}

func containsStr(haystack, needle string) bool {
	return bytes.Contains([]byte(haystack), []byte(needle))
}

// ---------------------------------------------------------------------------
// Batch A: concept / investigation tools.
// ---------------------------------------------------------------------------

// mcpResult is the minimal envelope shape the tool-call tests decode.
type mcpResult struct {
	Result struct {
		IsError           bool `json:"isError"`
		StructuredContent json.RawMessage `json:"structuredContent"`
		Content           []struct {
			Text string `json:"text"`
		} `json:"content"`
	} `json:"result"`
}

func (r *mcpResult) errorText() string {
	if len(r.Result.Content) == 0 {
		return ""
	}
	return r.Result.Content[0].Text
}

// seedConceptGroup creates a concept with one context + n facts linked
// to it, returning the concept id and the stable fact ids. Mirrors
// seedConceptWithFacts but promotes the facts to stable so searchFacts
// (which defaults to status=stable) sees them.
func seedConceptGroup(t *testing.T, env *testutil.TestEnv, repoID pgtype.UUID, conceptName, contextLabel string, nFacts int) (pgtype.UUID, []pgtype.UUID) {
	t.Helper()
	ctx := context.Background()
	queries := store.New(env.DB)
	srcID := pgtype.UUID{}
	srcID.Scan(uuid.NewString())
	queries.CreateSource(ctx, store.CreateSourceParams{
		ID: srcID, RepositoryID: repoID, Url: "https://example.com/" + uuid.NewString(), Kind: "homepage", Status: "fetched",
	})
	c, err := queries.CreateConcept(ctx, store.CreateConceptParams{
		RepositoryID: repoID, CanonicalName: conceptName, Context: contextLabel,
	})
	if err != nil {
		t.Fatalf("create concept: %v", err)
	}
	factIDs := make([]pgtype.UUID, 0, nFacts)
	for i := 0; i < nFacts; i++ {
		fidStr := insertFactWithSource(t, env, repoID, srcID, "Fact about "+conceptName, int32(i))
		var fid pgtype.UUID
		fid.Scan(fidStr)
		queries.MarkFactStatus(ctx, store.MarkFactStatusParams{ID: fid, Status: "stable"})
		if _, err := queries.AddFactConcept(ctx, store.AddFactConceptParams{FactID: fid, ConceptID: c.ID}); err != nil && err != pgx.ErrNoRows {
			t.Fatalf("link fact: %v", err)
		}
		factIDs = append(factIDs, fid)
	}
	return c.ID, factIDs
}

// TestMCP_SearchFacts_ConceptFilter verifies the searchFacts `concept`
// filter restricts to facts linked to the named concept group. A
// concept with 2 facts and an unrelated concept with 1 fact must
// return only the 2 when filtered.
func TestMCP_SearchFacts_ConceptFilter(t *testing.T) {
	env := testutil.NewTestEnv(t)
	defer env.Server.Close()

	admin := bootstrapSysAdmin(t, env, "mcp-concept-filter@example.com")
	_, _, repoID := createRepositoryWithDB(t, admin, "MCP CF", "mcp-cf", "desc", "")
	pgRepo := pgRepoID(t, repoID)

	keptID, keptFacts := seedConceptGroup(t, env, pgRepo, "DNA", "Biomolecule", 2)
	_, otherFacts := seedConceptGroup(t, env, pgRepo, "RNA", "Biomolecule", 1)
	_ = otherFacts

	uid := resolveUserID(t, env, "mcp-concept-filter@example.com")
	tok := mintAccessToken(t, env, uid, "mcp-concept-filter@example.com", "test-client")

	// Filter by canonical name "DNA" — expect the 2 DNA facts.
	status, body := mcpCall(t, env.BaseURL, tok, map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{
			"name": "searchFacts",
			"arguments": map[string]any{
				"repository": repoID,
				"concept":    "DNA",
			},
		},
	})
	if status != http.StatusOK {
		t.Fatalf("searchFacts concept: expected 200, got %d: %s", status, body)
	}
	var resp struct {
		Result struct {
			StructuredContent struct {
				Facts []struct {
					ID string `json:"id"`
				} `json:"facts"`
				Total int `json:"total"`
			} `json:"structuredContent"`
		} `json:"result"`
	}
	json.Unmarshal(body, &resp)
	if resp.Result.StructuredContent.Total != 2 {
		t.Fatalf("searchFacts concept: expected total=2, got %d", resp.Result.StructuredContent.Total)
	}
	if len(resp.Result.StructuredContent.Facts) != 2 {
		t.Fatalf("searchFacts concept: expected 2 facts, got %d", len(resp.Result.StructuredContent.Facts))
	}

	// Filter by UUID — expect the same 2.
	status, body = mcpCall(t, env.BaseURL, tok, map[string]any{
		"jsonrpc": "2.0", "id": 2, "method": "tools/call",
		"params": map[string]any{
			"name": "searchFacts",
			"arguments": map[string]any{
				"repository": repoID,
				"concept":    keptID.String(),
			},
		},
	})
	json.Unmarshal(body, &resp)
	if resp.Result.StructuredContent.Total != 2 {
		t.Fatalf("searchFacts concept uuid: expected total=2, got %d", resp.Result.StructuredContent.Total)
	}

	// Non-existent concept — tool error.
	status, body = mcpCall(t, env.BaseURL, tok, map[string]any{
		"jsonrpc": "2.0", "id": 3, "method": "tools/call",
		"params": map[string]any{
			"name": "searchFacts",
			"arguments": map[string]any{
				"repository": repoID,
				"concept":    "NoSuchConcept",
			},
		},
	})
	if status != http.StatusOK {
		t.Fatalf("searchFacts bad concept: expected 200, got %d: %s", status, body)
	}
	var errResp mcpResult
	json.Unmarshal(body, &errResp)
	if !errResp.Result.IsError {
		t.Fatal("searchFacts bad concept: expected isError=true")
	}
	if !containsStr(errResp.errorText(), "not found") {
		t.Fatalf("searchFacts bad concept: expected 'not found' in error, got %q", errResp.errorText())
	}
	_ = keptFacts
}

// TestMCP_SearchConcepts verifies searchConcepts returns concept groups
// with contexts + fact counts, filtered by substring.
func TestMCP_SearchConcepts(t *testing.T) {
	env := testutil.NewTestEnv(t)
	defer env.Server.Close()

	admin := bootstrapSysAdmin(t, env, "mcp-sc@example.com")
	_, _, repoID := createRepositoryWithDB(t, admin, "MCP SC", "mcp-sc", "desc", "")
	pgRepo := pgRepoID(t, repoID)
	seedConceptGroup(t, env, pgRepo, "DNA", "Biomolecule", 2)
	seedConceptGroup(t, env, pgRepo, "RNA", "Biomolecule", 1)

	uid := resolveUserID(t, env, "mcp-sc@example.com")
	tok := mintAccessToken(t, env, uid, "mcp-sc@example.com", "test-client")

	status, body := mcpCall(t, env.BaseURL, tok, map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{
			"name": "searchConcepts",
			"arguments": map[string]any{
				"repository": repoID,
				"query":      "dna",
			},
		},
	})
	if status != http.StatusOK {
		t.Fatalf("searchConcepts: expected 200, got %d: %s", status, body)
	}
	var resp struct {
		Result struct {
			StructuredContent struct {
				Concepts []struct {
					CanonicalName    string `json:"canonical_name"`
					TotalFactCount   int64  `json:"total_fact_count"`
					Contexts         []struct {
						Context   string `json:"context"`
						FactCount int64  `json:"fact_count"`
					} `json:"contexts"`
				} `json:"concepts"`
				Total int `json:"total"`
			} `json:"structuredContent"`
		} `json:"result"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("searchConcepts unmarshal: %v: %s", err, body)
	}
	if resp.Result.StructuredContent.Total != 1 {
		t.Fatalf("searchConcepts: expected total=1, got %d", resp.Result.StructuredContent.Total)
	}
	if len(resp.Result.StructuredContent.Concepts) != 1 {
		t.Fatalf("searchConcepts: expected 1 group, got %d", len(resp.Result.StructuredContent.Concepts))
	}
	c := resp.Result.StructuredContent.Concepts[0]
	if c.CanonicalName != "DNA" {
		t.Fatalf("searchConcepts: expected DNA, got %q", c.CanonicalName)
	}
	if c.TotalFactCount != 2 {
		t.Fatalf("searchConcepts: expected fact_count=2, got %d", c.TotalFactCount)
	}
	if len(c.Contexts) != 1 || c.Contexts[0].Context != "Biomolecule" {
		t.Fatalf("searchConcepts: expected 1 context Biomolecule, got %+v", c.Contexts)
	}
}

// TestMCP_GetConcept verifies getConcept returns the group + the
// synthesis/definition when one exists.
func TestMCP_GetConcept(t *testing.T) {
	env := testutil.NewTestEnv(t)
	defer env.Server.Close()

	admin := bootstrapSysAdmin(t, env, "mcp-gc@example.com")
	_, _, repoID := createRepositoryWithDB(t, admin, "MCP GC", "mcp-gc", "desc", "")
	pgRepo := pgRepoID(t, repoID)
	conceptID, _ := seedConceptGroup(t, env, pgRepo, "DNA", "Biomolecule", 2)

	// Seed a synthesis row directly.
	ctx := context.Background()
	if _, err := env.DB.Exec(ctx, `
INSERT INTO okt_repository.concept_syntheses (repository_id, canonical_name, content, covered_summary_ids, covered_concept_ids, embedded_image_ids, model)
VALUES ($1, 'DNA', 'DNA is a molecule.', '{}', '{}', '{}', 'stub-model')
ON CONFLICT (repository_id, lower(canonical_name)) DO UPDATE SET content = EXCLUDED.content`, pgRepo); err != nil {
		t.Fatalf("seed synthesis: %v", err)
	}

	uid := resolveUserID(t, env, "mcp-gc@example.com")
	tok := mintAccessToken(t, env, uid, "mcp-gc@example.com", "test-client")

	// By UUID.
	status, body := mcpCall(t, env.BaseURL, tok, map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{
			"name": "getConcept",
			"arguments": map[string]any{
				"repository": repoID,
				"concept":    conceptID.String(),
			},
		},
	})
	if status != http.StatusOK {
		t.Fatalf("getConcept: expected 200, got %d: %s", status, body)
	}
	var resp struct {
		Result struct {
			StructuredContent struct {
				Concept struct {
					CanonicalName string `json:"canonical_name"`
					Contexts      []struct {
						Context string `json:"context"`
					} `json:"contexts"`
				} `json:"concept"`
				Synthesis *struct {
					Content string `json:"content"`
				} `json:"synthesis"`
			} `json:"structuredContent"`
		} `json:"result"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("getConcept unmarshal: %v: %s", err, body)
	}
	if resp.Result.StructuredContent.Concept.CanonicalName != "DNA" {
		t.Fatalf("getConcept: expected DNA, got %q", resp.Result.StructuredContent.Concept.CanonicalName)
	}
	if resp.Result.StructuredContent.Synthesis == nil || resp.Result.StructuredContent.Synthesis.Content != "DNA is a molecule." {
		t.Fatalf("getConcept: expected synthesis content, got %+v", resp.Result.StructuredContent.Synthesis)
	}

	// By canonical name.
	status, body = mcpCall(t, env.BaseURL, tok, map[string]any{
		"jsonrpc": "2.0", "id": 2, "method": "tools/call",
		"params": map[string]any{
			"name": "getConcept",
			"arguments": map[string]any{
				"repository": repoID,
				"concept":    "DNA",
			},
		},
	})
	json.Unmarshal(body, &resp)
	if resp.Result.StructuredContent.Concept.CanonicalName != "DNA" {
		t.Fatalf("getConcept by-name: expected DNA, got %q", resp.Result.StructuredContent.Concept.CanonicalName)
	}

	// Non-existent concept — tool error.
	status, body = mcpCall(t, env.BaseURL, tok, map[string]any{
		"jsonrpc": "2.0", "id": 3, "method": "tools/call",
		"params": map[string]any{
			"name": "getConcept",
			"arguments": map[string]any{
				"repository": repoID,
				"concept":    "NoSuchConcept",
			},
		},
	})
	var errResp mcpResult
	json.Unmarshal(body, &errResp)
	if !errResp.Result.IsError || !containsStr(errResp.errorText(), "not found") {
		t.Fatalf("getConcept not-found: expected error, got %q", errResp.errorText())
	}
}

// TestMCP_GetConceptSummaries verifies getConceptSummaries returns the
// summary slices for a concept group.
func TestMCP_GetConceptSummaries(t *testing.T) {
	env := testutil.NewTestEnv(t)
	defer env.Server.Close()

	admin := bootstrapSysAdmin(t, env, "mcp-gcs@example.com")
	_, _, repoID := createRepositoryWithDB(t, admin, "MCP GCS", "mcp-gcs", "desc", "")
	pgRepo := pgRepoID(t, repoID)
	conceptID, _ := seedConceptGroup(t, env, pgRepo, "DNA", "Biomolecule", 2)

	// Seed 2 summary slices.
	seedSliceForConcept(t, env, conceptID, pgRepo, "Biomolecule", 0, 2, "DNA part 1.")
	seedSliceForConcept(t, env, conceptID, pgRepo, "Biomolecule", 1, 0, "DNA part 2.")

	uid := resolveUserID(t, env, "mcp-gcs@example.com")
	tok := mintAccessToken(t, env, uid, "mcp-gcs@example.com", "test-client")

	status, body := mcpCall(t, env.BaseURL, tok, map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{
			"name": "getConceptSummaries",
			"arguments": map[string]any{
				"repository": repoID,
				"concept":    "DNA",
			},
		},
	})
	if status != http.StatusOK {
		t.Fatalf("getConceptSummaries: expected 200, got %d: %s", status, body)
	}
	var resp struct {
		Result struct {
			StructuredContent struct {
				Summaries []struct {
					SequenceNum int32  `json:"sequence_num"`
					Content     string `json:"content"`
				} `json:"summaries"`
				Count int `json:"count"`
			} `json:"structuredContent"`
		} `json:"result"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("getConceptSummaries unmarshal: %v: %s", err, body)
	}
	if resp.Result.StructuredContent.Count != 2 {
		t.Fatalf("getConceptSummaries: expected 2 summaries, got %d", resp.Result.StructuredContent.Count)
	}
	if len(resp.Result.StructuredContent.Summaries) != 2 {
		t.Fatalf("getConceptSummaries: expected 2 rows, got %d", len(resp.Result.StructuredContent.Summaries))
	}
}

// TestMCP_GetRelatedConcepts verifies getRelatedConcepts returns the
// related concept groups ranked by shared_fact_count.
func TestMCP_GetRelatedConcepts(t *testing.T) {
	env := testutil.NewTestEnv(t)
	defer env.Server.Close()

	admin := bootstrapSysAdmin(t, env, "mcp-grc@example.com")
	const slug = "mcp-grc"
	_, _, repoID := createRepositoryWithDB(t, admin, "MCP GRC", slug, "desc", "")
	pgRepo := pgRepoID(t, repoID)
	ctx := context.Background()
	queries := store.New(env.DB)

	// One source in the repo.
	srcID := pgtype.UUID{}
	srcID.Scan(uuid.NewString())
	queries.CreateSource(ctx, store.CreateSourceParams{
		ID: srcID, RepositoryID: pgRepo, Url: "https://example.com/grc", Kind: "homepage", Status: "fetched",
	})

	mkConcept := func(name, ctxLabel string) pgtype.UUID {
		c, err := queries.CreateConcept(ctx, store.CreateConceptParams{
			RepositoryID: pgRepo, CanonicalName: name, Context: ctxLabel,
		})
		if err != nil {
			t.Fatalf("create concept: %v", err)
		}
		return c.ID
	}
	linkFact := func(conceptID pgtype.UUID, chunk int32) pgtype.UUID {
		fidStr := insertFactWithSource(t, env, pgRepo, srcID, "shared fact", chunk)
		var fid pgtype.UUID
		fid.Scan(fidStr)
		queries.MarkFactStatus(ctx, store.MarkFactStatusParams{ID: fid, Status: "stable"})
		queries.AddFactConcept(ctx, store.AddFactConceptParams{FactID: fid, ConceptID: conceptID})
		return fid
	}
	linkExisting := func(fid, conceptID pgtype.UUID) {
		queries.AddFactConcept(ctx, store.AddFactConceptParams{FactID: fid, ConceptID: conceptID})
	}

	trump := mkConcept("Trump", "Politician")
	musk := mkConcept("Musk", "Person")
	// 3 shared facts.
	f1 := linkFact(trump, 0)
	linkExisting(f1, musk)
	f2 := linkFact(trump, 1)
	linkExisting(f2, musk)
	f3 := linkFact(trump, 2)
	linkExisting(f3, musk)

	// Refresh the matview synchronously.
	if _, err := env.DB.Exec(ctx, `REFRESH MATERIALIZED VIEW okt_repository.concept_relations`); err != nil {
		t.Fatalf("refresh matview: %v", err)
	}

	uid := resolveUserID(t, env, "mcp-grc@example.com")
	tok := mintAccessToken(t, env, uid, "mcp-grc@example.com", "test-client")

	status, body := mcpCall(t, env.BaseURL, tok, map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{
			"name": "getRelatedConcepts",
			"arguments": map[string]any{
				"repository": repoID,
				"concept":    "Trump",
			},
		},
	})
	if status != http.StatusOK {
		t.Fatalf("getRelatedConcepts: expected 200, got %d: %s", status, body)
	}
	var resp struct {
		Result struct {
			StructuredContent struct {
				Related []struct {
					CanonicalName   string `json:"canonical_name"`
					SharedFactCount int64  `json:"shared_fact_count"`
				} `json:"related"`
				Total int `json:"total"`
			} `json:"structuredContent"`
		} `json:"result"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("getRelatedConcepts unmarshal: %v: %s", err, body)
	}
	if resp.Result.StructuredContent.Total != 1 {
		t.Fatalf("getRelatedConcepts: expected total=1, got %d", resp.Result.StructuredContent.Total)
	}
	if len(resp.Result.StructuredContent.Related) != 1 {
		t.Fatalf("getRelatedConcepts: expected 1 row, got %d", len(resp.Result.StructuredContent.Related))
	}
	if resp.Result.StructuredContent.Related[0].CanonicalName != "Musk" {
		t.Fatalf("getRelatedConcepts: expected Musk, got %q", resp.Result.StructuredContent.Related[0].CanonicalName)
	}
	if resp.Result.StructuredContent.Related[0].SharedFactCount != 3 {
		t.Fatalf("getRelatedConcepts: expected shared=3, got %d", resp.Result.StructuredContent.Related[0].SharedFactCount)
	}
}

// TestMCP_GetInvestigation verifies getInvestigation returns the
// investigation metadata + its sources.
func TestMCP_GetInvestigation(t *testing.T) {
	env := testutil.NewTestEnv(t)
	defer env.Server.Close()

	admin := bootstrapSysAdmin(t, env, "mcp-gi@example.com")
	const slug = "mcp-gi"
	_, _, repoID := createRepositoryWithDB(t, admin, "MCP GI", slug, "desc", "")
	pgRepo := pgRepoID(t, repoID)
	ctx := context.Background()
	queries := store.New(env.DB)

	// Create a source + an investigation + link them.
	srcID := pgtype.UUID{}
	srcID.Scan(uuid.NewString())
	queries.CreateSource(ctx, store.CreateSourceParams{
		ID: srcID, RepositoryID: pgRepo, Url: "https://example.com/inv-source", Kind: "homepage", Status: "fetched",
	})
	invID := pgtype.UUID{}
	invID.Scan(uuid.NewString())
	topic := "DNA research"
	inv, err := queries.CreateInvestigation(ctx, store.CreateInvestigationParams{
		ID: invID, RepositoryID: pgRepo, Title: "DNA Investigation", Topic: &topic,
	})
	if err != nil {
		t.Fatalf("create investigation: %v", err)
	}
	if err := queries.AddInvestigationSource(ctx, store.AddInvestigationSourceParams{
		InvestigationID: invID, SourceID: srcID,
	}); err != nil {
		t.Fatalf("add source: %v", err)
	}

	uid := resolveUserID(t, env, "mcp-gi@example.com")
	tok := mintAccessToken(t, env, uid, "mcp-gi@example.com", "test-client")

	status, body := mcpCall(t, env.BaseURL, tok, map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{
			"name": "getInvestigation",
			"arguments": map[string]any{
				"repository":      repoID,
				"investigationId": invID.String(),
			},
		},
	})
	if status != http.StatusOK {
		t.Fatalf("getInvestigation: expected 200, got %d: %s", status, body)
	}
	var resp struct {
		Result struct {
			StructuredContent struct {
				Investigation struct {
					Title string `json:"title"`
					Topic string `json:"topic"`
				} `json:"investigation"`
				Sources []struct {
					URL string `json:"url"`
				} `json:"sources"`
				Count int `json:"count"`
			} `json:"structuredContent"`
		} `json:"result"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("getInvestigation unmarshal: %v: %s", err, body)
	}
	if resp.Result.StructuredContent.Investigation.Title != "DNA Investigation" {
		t.Fatalf("getInvestigation: expected title, got %q", resp.Result.StructuredContent.Investigation.Title)
	}
	if resp.Result.StructuredContent.Investigation.Topic != "DNA research" {
		t.Fatalf("getInvestigation: expected topic, got %q", resp.Result.StructuredContent.Investigation.Topic)
	}
	if resp.Result.StructuredContent.Count != 1 || len(resp.Result.StructuredContent.Sources) != 1 {
		t.Fatalf("getInvestigation: expected 1 source, got count=%d len=%d", resp.Result.StructuredContent.Count, len(resp.Result.StructuredContent.Sources))
	}
	if resp.Result.StructuredContent.Sources[0].URL != "https://example.com/inv-source" {
		t.Fatalf("getInvestigation: expected source URL, got %q", resp.Result.StructuredContent.Sources[0].URL)
	}

	// Non-existent investigation — tool error.
	status, body = mcpCall(t, env.BaseURL, tok, map[string]any{
		"jsonrpc": "2.0", "id": 2, "method": "tools/call",
		"params": map[string]any{
			"name": "getInvestigation",
			"arguments": map[string]any{
				"repository":      repoID,
				"investigationId": uuid.NewString(),
			},
		},
	})
	var errResp mcpResult
	json.Unmarshal(body, &errResp)
	if !errResp.Result.IsError || !containsStr(errResp.errorText(), "not found") {
		t.Fatalf("getInvestigation not-found: expected error, got %q", errResp.errorText())
	}
	_ = inv
}

// TestMCP_SearchFacts_BadRepo_StillError re-asserts the bad-repo path
// is a tool error after the handler grew the concept filter branch.
func TestMCP_SearchFacts_BadRepo_StillError(t *testing.T) {
	env := testutil.NewTestEnv(t)
	defer env.Server.Close()

	_ = registerTestUser(t, env, "mcp-br2@example.com", "password123", "MCP BR2")
	uid := resolveUserID(t, env, "mcp-br2@example.com")
	tok := mintAccessToken(t, env, uid, "mcp-br2@example.com", "test-client")

	status, body := mcpCall(t, env.BaseURL, tok, map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{
			"name": "searchFacts",
			"arguments": map[string]any{
				"repository": "no-such-slug",
			},
		},
	})
	if status != http.StatusOK {
		t.Fatalf("expected 200 (tool error), got %d: %s", status, body)
	}
	var resp mcpResult
	json.Unmarshal(body, &resp)
	if !resp.Result.IsError {
		t.Fatal("expected isError=true")
	}
}

// ---------------------------------------------------------------------------
// Batch B: write tools (createInvestigation, fetchAndProcessSource,
// getSourceTasks).
// ---------------------------------------------------------------------------

// TestMCP_CreateInvestigation verifies createInvestigation creates an
// investigation and returns its metadata.
func TestMCP_CreateInvestigation(t *testing.T) {
	env := testutil.NewTestEnv(t)
	defer env.Server.Close()

	admin := bootstrapSysAdmin(t, env, "mcp-ci@example.com")
	_, _, repoID := createRepositoryWithDB(t, admin, "MCP CI", "mcp-ci", "desc", "")

	uid := resolveUserID(t, env, "mcp-ci@example.com")
	tok := mintAccessToken(t, env, uid, "mcp-ci@example.com", "test-client")

	status, body := mcpCall(t, env.BaseURL, tok, map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{
			"name": "createInvestigation",
			"arguments": map[string]any{
				"repository": repoID,
				"title":      "DNA Research",
				"topic":      "Understanding DNA",
			},
		},
	})
	if status != http.StatusOK {
		t.Fatalf("createInvestigation: expected 200, got %d: %s", status, body)
	}
	var resp struct {
		Result struct {
			StructuredContent struct {
				Investigation struct {
					ID    string `json:"id"`
					Title string `json:"title"`
					Topic string `json:"topic"`
				} `json:"investigation"`
			} `json:"structuredContent"`
		} `json:"result"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("createInvestigation unmarshal: %v: %s", err, body)
	}
	if resp.Result.StructuredContent.Investigation.Title != "DNA Research" {
		t.Fatalf("createInvestigation: expected title, got %q", resp.Result.StructuredContent.Investigation.Title)
	}
	if resp.Result.StructuredContent.Investigation.Topic != "Understanding DNA" {
		t.Fatalf("createInvestigation: expected topic, got %q", resp.Result.StructuredContent.Investigation.Topic)
	}
	if resp.Result.StructuredContent.Investigation.ID == "" {
		t.Fatal("createInvestigation: expected non-empty id")
	}
}

// TestMCP_AddInvestigationSource verifies addInvestigationSource
// links an existing source into an investigation via the MCP tool
// (the step the agent previously had no primitive for, leaving
// fetched sources unlinked). The test seeds a source + an
// investigation directly in the DB, calls the tool, and confirms
// the junction row exists by re-reading via getInvestigation.
func TestMCP_AddInvestigationSource(t *testing.T) {
	env := testutil.NewTestEnv(t)
	defer env.Server.Close()

	admin := bootstrapSysAdmin(t, env, "mcp-ais@example.com")
	const slug = "mcp-ais"
	_, _, repoID := createRepositoryWithDB(t, admin, "MCP AIS", slug, "desc", "")
	pgRepo := pgRepoID(t, repoID)
	ctx := context.Background()
	queries := store.New(env.DB)

	// Seed a source row (the post-fetch state an agent would
	// have after polling getSourceTasks).
	srcID := pgtype.UUID{}
	srcID.Scan(uuid.NewString())
	if _, err := queries.CreateSource(ctx, store.CreateSourceParams{
		ID: srcID, RepositoryID: pgRepo, Url: "https://example.com/ais-source", Kind: "homepage", Status: "fetched",
	}); err != nil {
		t.Fatalf("create source: %v", err)
	}
	// Seed an investigation.
	invID := pgtype.UUID{}
	invID.Scan(uuid.NewString())
	topic := "agent-collected sources"
	if _, err := queries.CreateInvestigation(ctx, store.CreateInvestigationParams{
		ID: invID, RepositoryID: pgRepo, Title: "Agent Investigation", Topic: &topic,
	}); err != nil {
		t.Fatalf("create investigation: %v", err)
	}

	uid := resolveUserID(t, env, "mcp-ais@example.com")
	tok := mintAccessToken(t, env, uid, "mcp-ais@example.com", "test-client")

	// Link via the MCP tool.
	status, body := mcpCall(t, env.BaseURL, tok, map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{
			"name": "addInvestigationSource",
			"arguments": map[string]any{
				"repository":      repoID,
				"investigationId": invID.String(),
				"sourceId":        srcID.String(),
			},
		},
	})
	if status != http.StatusOK {
		t.Fatalf("addInvestigationSource: expected 200, got %d: %s", status, body)
	}
	var resp struct {
		Result struct {
			StructuredContent struct {
				Linked          bool   `json:"linked"`
				InvestigationID string `json:"investigation_id"`
				SourceID        string `json:"source_id"`
			} `json:"structuredContent"`
		} `json:"result"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("addInvestigationSource unmarshal: %v: %s", err, body)
	}
	if !resp.Result.StructuredContent.Linked {
		t.Fatal("addInvestigationSource: expected linked=true")
	}
	if resp.Result.StructuredContent.SourceID != srcID.String() {
		t.Errorf("addInvestigationSource: source_id = %q, want %q", resp.Result.StructuredContent.SourceID, srcID.String())
	}

	// Verify the junction row landed by re-reading via getInvestigation.
	status, body = mcpCall(t, env.BaseURL, tok, map[string]any{
		"jsonrpc": "2.0", "id": 2, "method": "tools/call",
		"params": map[string]any{
			"name": "getInvestigation",
			"arguments": map[string]any{
				"repository":      repoID,
				"investigationId": invID.String(),
			},
		},
	})
	if status != http.StatusOK {
		t.Fatalf("getInvestigation: expected 200, got %d: %s", status, body)
	}
	var getResp struct {
		Result struct {
			StructuredContent struct {
				Sources []struct {
					URL string `json:"url"`
				} `json:"sources"`
				Count int `json:"count"`
			} `json:"structuredContent"`
		} `json:"result"`
	}
	if err := json.Unmarshal(body, &getResp); err != nil {
		t.Fatalf("getInvestigation unmarshal: %v: %s", err, body)
	}
	if getResp.Result.StructuredContent.Count != 1 || len(getResp.Result.StructuredContent.Sources) != 1 {
		t.Fatalf("getInvestigation: expected 1 source after link, got count=%d len=%d", getResp.Result.StructuredContent.Count, len(getResp.Result.StructuredContent.Sources))
	}
	if getResp.Result.StructuredContent.Sources[0].URL != "https://example.com/ais-source" {
		t.Errorf("getInvestigation: expected source URL, got %q", getResp.Result.StructuredContent.Sources[0].URL)
	}
}

// TestMCP_AddInvestigationSource_CrossRepo verifies the tool
// rejects a sourceId that belongs to a different repository than
// the investigation (the same ownership guard the REST
// Investigations.AddSource endpoint enforces).
func TestMCP_AddInvestigationSource_CrossRepo(t *testing.T) {
	env := testutil.NewTestEnv(t)
	defer env.Server.Close()

	admin := bootstrapSysAdmin(t, env, "mcp-ais-xr@example.com")
	_, _, repoAID := createRepositoryWithDB(t, admin, "MCP AIS A", "mcp-ais-a", "desc", "")
	_, _, repoBID := createRepositoryWithDB(t, admin, "MCP AIS B", "mcp-ais-b", "desc", "")
	pgRepoA := pgRepoID(t, repoAID)
	pgRepoB := pgRepoID(t, repoBID)
	ctx := context.Background()
	queries := store.New(env.DB)

	// Source in repo B.
	srcID := pgtype.UUID{}
	srcID.Scan(uuid.NewString())
	if _, err := queries.CreateSource(ctx, store.CreateSourceParams{
		ID: srcID, RepositoryID: pgRepoB, Url: "https://example.com/b-source", Kind: "homepage", Status: "fetched",
	}); err != nil {
		t.Fatalf("create source: %v", err)
	}
	// Investigation in repo A.
	invID := pgtype.UUID{}
	invID.Scan(uuid.NewString())
	if _, err := queries.CreateInvestigation(ctx, store.CreateInvestigationParams{
		ID: invID, RepositoryID: pgRepoA, Title: "Repo A Inv",
	}); err != nil {
		t.Fatalf("create investigation: %v", err)
	}

	uid := resolveUserID(t, env, "mcp-ais-xr@example.com")
	tok := mintAccessToken(t, env, uid, "mcp-ais-xr@example.com", "test-client")

	// Try to link repo B's source into repo A's investigation.
	status, body := mcpCall(t, env.BaseURL, tok, map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{
			"name": "addInvestigationSource",
			"arguments": map[string]any{
				"repository":      repoAID,
				"investigationId": invID.String(),
				"sourceId":        srcID.String(),
			},
		},
	})
	if status != http.StatusOK {
		t.Fatalf("expected 200 (tool error), got %d: %s", status, body)
	}
	var errResp mcpResult
	json.Unmarshal(body, &errResp)
	if !errResp.Result.IsError {
		t.Fatal("expected isError=true for cross-repo source")
	}
	if !containsStr(errResp.errorText(), "source not found") {
		t.Fatalf("expected 'source not found' in error, got %q", errResp.errorText())
	}
}

// TestMCP_FetchAndProcessSource verifies fetchAndProcessSource enqueues
// a retrieve_source job via the recording enqueuer and returns the
// job id.
func TestMCP_FetchAndProcessSource(t *testing.T) {
	env := testutil.NewTestEnv(t)
	defer env.Server.Close()

	admin := bootstrapSysAdmin(t, env, "mcp-fetch@example.com")
	_, _, repoID := createRepositoryWithDB(t, admin, "MCP Fetch", "mcp-fetch", "desc", "")

	uid := resolveUserID(t, env, "mcp-fetch@example.com")
	tok := mintAccessToken(t, env, uid, "mcp-fetch@example.com", "test-client")

	status, body := mcpCall(t, env.BaseURL, tok, map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{
			"name": "fetchAndProcessSource",
			"arguments": map[string]any{
				"repository": repoID,
				"url":        "https://example.com/dna-paper",
			},
		},
	})
	if status != http.StatusOK {
		t.Fatalf("fetchAndProcessSource: expected 200, got %d: %s", status, body)
	}
	var resp struct {
		Result struct {
			StructuredContent struct {
				JobID       string `json:"job_id"`
				ClassifiedAs string `json:"classified_as"`
				Status       string `json:"status"`
			} `json:"structuredContent"`
		} `json:"result"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("fetchAndProcessSource unmarshal: %v: %s", err, body)
	}
	if resp.Result.StructuredContent.JobID == "" {
		t.Fatal("fetchAndProcessSource: expected non-empty job_id")
	}
	if resp.Result.StructuredContent.Status != "queued" {
		t.Fatalf("fetchAndProcessSource: expected status=queued, got %q", resp.Result.StructuredContent.Status)
	}
	// Verify the enqueuer recorded the request. The test env's
	// RecordingTaskEnqueuer guards Enqueued with an unexported mu,
	// but the test runs single-threaded so a direct read is safe.
	if len(env.TaskEnqueuer.Enqueued) == 0 {
		t.Fatal("fetchAndProcessSource: expected enqueued job")
	}
	if env.TaskEnqueuer.Enqueued[0].URL != "https://example.com/dna-paper" {
		t.Fatalf("fetchAndProcessSource: expected URL recorded, got %q", env.TaskEnqueuer.Enqueued[0].URL)
	}
	// The tool must set Process=true so the worker chains
	// source_decomposition after a successful fetch — the tool
	// is named fetchAndProcessSource and promises "extracts
	// facts, and links them". Without it the agent would only
	// get a fetched row and no decomposition/embed pipeline.
	if !env.TaskEnqueuer.Enqueued[0].Process {
		t.Fatal("fetchAndProcessSource: expected Process=true on enqueued job")
	}
}

// TestMCP_FetchAndProcessSource_WithInvestigation verifies the
// optional investigationId parameter is validated up front
// (investigation must exist in the resolved repo) and forwarded
// to the enqueuer so the worker links the fetched source into
// the investigation. This is the preferred one-call fetch +
// organize flow for MCP agents.
func TestMCP_FetchAndProcessSource_WithInvestigation(t *testing.T) {
	env := testutil.NewTestEnv(t)
	defer env.Server.Close()

	admin := bootstrapSysAdmin(t, env, "mcp-fetch-inv@example.com")
	_, _, repoID := createRepositoryWithDB(t, admin, "MCP Fetch Inv", "mcp-fetch-inv", "desc", "")
	pgRepo := pgRepoID(t, repoID)
	ctx := context.Background()
	queries := store.New(env.DB)

	// Seed an investigation in the repo.
	invID := pgtype.UUID{}
	invID.Scan(uuid.NewString())
	if _, err := queries.CreateInvestigation(ctx, store.CreateInvestigationParams{
		ID: invID, RepositoryID: pgRepo, Title: "Fetch Inv",
	}); err != nil {
		t.Fatalf("create investigation: %v", err)
	}

	uid := resolveUserID(t, env, "mcp-fetch-inv@example.com")
	tok := mintAccessToken(t, env, uid, "mcp-fetch-inv@example.com", "test-client")

	status, body := mcpCall(t, env.BaseURL, tok, map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{
			"name": "fetchAndProcessSource",
			"arguments": map[string]any{
				"repository":      repoID,
				"url":             "https://example.com/inv-paper",
				"investigationId": invID.String(),
			},
		},
	})
	if status != http.StatusOK {
		t.Fatalf("fetchAndProcessSource: expected 200, got %d: %s", status, body)
	}
	var resp struct {
		Result struct {
			StructuredContent struct {
				JobID string `json:"job_id"`
			} `json:"structuredContent"`
		} `json:"result"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("fetchAndProcessSource unmarshal: %v: %s", err, body)
	}
	if resp.Result.StructuredContent.JobID == "" {
		t.Fatal("fetchAndProcessSource: expected non-empty job_id")
	}
	// The enqueuer must have recorded the investigationId so
	// the worker can link the source once the row exists.
	if len(env.TaskEnqueuer.Enqueued) == 0 {
		t.Fatal("fetchAndProcessSource: expected enqueued job")
	}
	if env.TaskEnqueuer.Enqueued[0].InvestigationID != invID.String() {
		t.Fatalf("fetchAndProcessSource: expected InvestigationID=%q, got %q", invID.String(), env.TaskEnqueuer.Enqueued[0].InvestigationID)
	}
}

// TestMCP_FetchAndProcessSource_CrossRepoInvestigation verifies
// the up-front validation rejects an investigationId that
// belongs to a different repository than the resolved repo,
// so the agent gets a synchronous tool error instead of a
// silently-skipped link after the async fetch.
func TestMCP_FetchAndProcessSource_CrossRepoInvestigation(t *testing.T) {
	env := testutil.NewTestEnv(t)
	defer env.Server.Close()

	admin := bootstrapSysAdmin(t, env, "mcp-fetch-xr@example.com")
	_, _, repoAID := createRepositoryWithDB(t, admin, "MCP Fetch A", "mcp-fetch-a", "desc", "")
	_, _, repoBID := createRepositoryWithDB(t, admin, "MCP Fetch B", "mcp-fetch-b", "desc", "")
	pgRepoB := pgRepoID(t, repoBID)
	ctx := context.Background()
	queries := store.New(env.DB)

	// Investigation in repo B; we'll fetch into repo A.
	invID := pgtype.UUID{}
	invID.Scan(uuid.NewString())
	if _, err := queries.CreateInvestigation(ctx, store.CreateInvestigationParams{
		ID: invID, RepositoryID: pgRepoB, Title: "Repo B Inv",
	}); err != nil {
		t.Fatalf("create investigation: %v", err)
	}

	uid := resolveUserID(t, env, "mcp-fetch-xr@example.com")
	tok := mintAccessToken(t, env, uid, "mcp-fetch-xr@example.com", "test-client")

	status, body := mcpCall(t, env.BaseURL, tok, map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{
			"name": "fetchAndProcessSource",
			"arguments": map[string]any{
				"repository":      repoAID,
				"url":             "https://example.com/xr-paper",
				"investigationId": invID.String(),
			},
		},
	})
	if status != http.StatusOK {
		t.Fatalf("expected 200 (tool error), got %d: %s", status, body)
	}
	var errResp mcpResult
	json.Unmarshal(body, &errResp)
	if !errResp.Result.IsError {
		t.Fatal("expected isError=true for cross-repo investigationId")
	}
	if !containsStr(errResp.errorText(), "investigation not found") {
		t.Fatalf("expected 'investigation not found' in error, got %q", errResp.errorText())
	}
	// No job should have been enqueued for the rejected call.
	if len(env.TaskEnqueuer.Enqueued) != 0 {
		t.Fatalf("expected no enqueued job for cross-repo investigation, got %d", len(env.TaskEnqueuer.Enqueued))
	}
}

// TestMCP_FetchAndProcessSource_NoURLorDOI verifies the tool returns a
// tool error when neither url nor doi is given.
func TestMCP_FetchAndProcessSource_NoURLorDOI(t *testing.T) {
	env := testutil.NewTestEnv(t)
	defer env.Server.Close()

	admin := bootstrapSysAdmin(t, env, "mcp-fetch-err@example.com")
	_, _, repoID := createRepositoryWithDB(t, admin, "MCP Fetch Err", "mcp-fetch-err", "desc", "")

	uid := resolveUserID(t, env, "mcp-fetch-err@example.com")
	tok := mintAccessToken(t, env, uid, "mcp-fetch-err@example.com", "test-client")

	status, body := mcpCall(t, env.BaseURL, tok, map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{
			"name": "fetchAndProcessSource",
			"arguments": map[string]any{
				"repository": repoID,
			},
		},
	})
	if status != http.StatusOK {
		t.Fatalf("expected 200 (tool error), got %d: %s", status, body)
	}
	var resp mcpResult
	json.Unmarshal(body, &resp)
	if !resp.Result.IsError {
		t.Fatal("expected isError=true")
	}
	if !containsStr(resp.errorText(), "required") {
		t.Fatalf("expected 'required' in error, got %q", resp.errorText())
	}
}

// TestMCP_GetSourceTasks_NotConfigured verifies getSourceTasks
// verbose mode returns a tool error when the task client is nil
// (the default test env wires a real task pool but no River client).
// The summary (verbose=false) path uses the task pool, so it works
// even without a task client — it returns an empty global summary
// (complete=true, all counts zero). The verbose path needs
// taskClient (River JobList), so it returns "not configured".
func TestMCP_GetSourceTasks_NotConfigured(t *testing.T) {
	env := testutil.NewTestEnv(t)
	defer env.Server.Close()
	ensureRiverSchema(t, env.DB)

	admin := bootstrapSysAdmin(t, env, "mcp-gst@example.com")
	_, _, repoID := createRepositoryWithDB(t, admin, "MCP GST", "mcp-gst", "desc", "")

	uid := resolveUserID(t, env, "mcp-gst@example.com")
	tok := mintAccessToken(t, env, uid, "mcp-gst@example.com", "test-client")

	// Summary mode (verbose=false): works via the task pool, returns
	// an empty global summary (no jobs seeded).
	status, body := mcpCall(t, env.BaseURL, tok, map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{
			"name": "getSourceTasks",
			"arguments": map[string]any{
				"repository": repoID,
			},
		},
	})
	if status != http.StatusOK {
		t.Fatalf("summary: expected 200, got %d: %s", status, body)
	}
	var sumResp struct {
		Result struct {
			StructuredContent struct {
				Complete     bool `json:"complete"`
				PendingCount int  `json:"pending_count"`
				Total        int  `json:"total"`
			} `json:"structuredContent"`
		} `json:"result"`
	}
	if err := json.Unmarshal(body, &sumResp); err != nil {
		t.Fatalf("summary unmarshal: %v: %s", err, body)
	}
	if !sumResp.Result.StructuredContent.Complete {
		t.Error("expected complete=true (no jobs, summary uses task pool)")
	}
	if sumResp.Result.StructuredContent.Total != 0 {
		t.Errorf("expected total=0, got %d", sumResp.Result.StructuredContent.Total)
	}

	// Verbose mode (verbose=true): needs taskClient (nil here) →
	// "not configured" tool error.
	status, body = mcpCall(t, env.BaseURL, tok, map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{
			"name": "getSourceTasks",
			"arguments": map[string]any{
				"repository": repoID,
				"verbose":    true,
			},
		},
	})
	if status != http.StatusOK {
		t.Fatalf("verbose: expected 200 (tool error), got %d: %s", status, body)
	}
	var resp mcpResult
	json.Unmarshal(body, &resp)
	if !resp.Result.IsError {
		t.Fatal("expected isError=true for verbose without task client")
	}
	if !containsStr(resp.errorText(), "not configured") {
		t.Fatalf("expected 'not configured' in error, got %q", resp.errorText())
	}
}

// ---------------------------------------------------------------------------
// searchSources tool.
// ---------------------------------------------------------------------------

// TestMCP_SearchSources_NotConfigured verifies searchSources returns a
// tool error when no search providers are wired (the default test env
// passes nil to wireOAuthAndMCP). Mirrors getSourceTasks' not-configured
// posture.
func TestMCP_SearchSources_NotConfigured(t *testing.T) {
	env := testutil.NewTestEnv(t)
	defer env.Server.Close()

	admin := bootstrapSysAdmin(t, env, "mcp-ss-nc@example.com")
	_, _, repoID := createRepositoryWithDB(t, admin, "MCP SS NC", "mcp-ss-nc", "desc", "")

	uid := resolveUserID(t, env, "mcp-ss-nc@example.com")
	tok := mintAccessToken(t, env, uid, "mcp-ss-nc@example.com", "test-client")

	status, body := mcpCall(t, env.BaseURL, tok, map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{
			"name": "searchSources",
			"arguments": map[string]any{
				"repository": repoID,
				"query":      "dna replication",
			},
		},
	})
	if status != http.StatusOK {
		t.Fatalf("expected 200 (tool error), got %d: %s", status, body)
	}
	var resp mcpResult
	json.Unmarshal(body, &resp)
	if !resp.Result.IsError {
		t.Fatal("expected isError=true")
	}
	if !containsStr(resp.errorText(), "not configured") {
		t.Fatalf("expected 'not configured' in error, got %q", resp.errorText())
	}
}

// TestMCP_SearchSources_UnknownProvider verifies an explicit provider
// name that isn't registered surfaces as a tool error (not a panic / 500).
func TestMCP_SearchSources_UnknownProvider(t *testing.T) {
	provider := &stubSearchProvider{
		results: []search.SearchResult{{Title: "X", URL: "https://example.com/x"}},
	}
	env := testutil.NewTestEnvWithSearch(t, map[string]search.SearchProvider{"stub": provider})
	defer env.Server.Close()

	admin := bootstrapSysAdmin(t, env, "mcp-ss-up@example.com")
	_, _, repoID := createRepositoryWithDB(t, admin, "MCP SS UP", "mcp-ss-up", "desc", "")

	uid := resolveUserID(t, env, "mcp-ss-up@example.com")
	tok := mintAccessToken(t, env, uid, "mcp-ss-up@example.com", "test-client")

	status, body := mcpCall(t, env.BaseURL, tok, map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{
			"name": "searchSources",
			"arguments": map[string]any{
				"repository": repoID,
				"query":      "x",
				"provider":   "bogus",
			},
		},
	})
	if status != http.StatusOK {
		t.Fatalf("expected 200 (tool error), got %d: %s", status, body)
	}
	var resp mcpResult
	json.Unmarshal(body, &resp)
	if !resp.Result.IsError {
		t.Fatal("expected isError=true")
	}
	if !containsStr(resp.errorText(), "unknown search provider") {
		t.Fatalf("expected 'unknown search provider' in error, got %q", resp.errorText())
	}
}

// TestMCP_SearchSources_HappyPath verifies the tool calls the stub
// provider, returns the results envelope, and echoes the chosen provider.
// It also seeds an existing source row to prove the already-exists
// tagging stamps already_exists=true on the matching hit.
func TestMCP_SearchSources_HappyPath(t *testing.T) {
	provider := &stubSearchProvider{
		results: []search.SearchResult{
			{Title: "Already here", URL: "https://example.com/already", Snippet: "s1"},
			{Title: "New hit", URL: "https://example.com/new", Snippet: "s2", DOI: "10.1234/new"},
		},
		total:      2,
		nextCursor: "page2",
	}
	env := testutil.NewTestEnvWithSearch(t, map[string]search.SearchProvider{"stub": provider})
	defer env.Server.Close()

	admin := bootstrapSysAdmin(t, env, "mcp-ss-ok@example.com")
	_, _, repoID := createRepositoryWithDB(t, admin, "MCP SS OK", "mcp-ss-ok", "desc", "")

	// Seed an existing source so the already-exists tagging matches the first hit.
	var repoUUID pgtype.UUID
	if err := repoUUID.Scan(repoID); err != nil {
		t.Fatalf("scanning repo id: %v", err)
	}
	insertSourceRow(t, env, repoUUID, "https://example.com/already", "", "fetched")

	uid := resolveUserID(t, env, "mcp-ss-ok@example.com")
	tok := mintAccessToken(t, env, uid, "mcp-ss-ok@example.com", "test-client")

	status, body := mcpCall(t, env.BaseURL, tok, map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{
			"name": "searchSources",
			"arguments": map[string]any{
				"repository": repoID,
				"query":      "test",
			},
		},
	})
	if status != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", status, body)
	}
	var resp struct {
		Result struct {
			StructuredContent struct {
				Provider    string `json:"provider"`
				NextCursor  string `json:"next_cursor"`
				Total       int64  `json:"total"`
				Results     []struct {
					URL          string  `json:"url"`
					Title        string  `json:"title"`
					DOI          string  `json:"doi,omitempty"`
					AlreadyExists bool    `json:"already_exists,omitempty"`
					ExistingStatus *string `json:"existing_status,omitempty"`
				} `json:"results"`
			} `json:"structuredContent"`
		} `json:"result"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("searchSources unmarshal: %v: %s", err, body)
	}
	sc := resp.Result.StructuredContent
	if sc.Provider != "stub" {
		t.Fatalf("expected provider=stub, got %q", sc.Provider)
	}
	if sc.Total != 2 {
		t.Fatalf("expected total=2, got %d", sc.Total)
	}
	if sc.NextCursor != "page2" {
		t.Fatalf("expected next_cursor=page2, got %q", sc.NextCursor)
	}
	if len(sc.Results) != 2 {
		t.Fatalf("expected 2 results, got %d", len(sc.Results))
	}
	// First hit was seeded as an existing source.
	if !sc.Results[0].AlreadyExists {
		t.Errorf("expected results[0].already_exists=true for %q", sc.Results[0].URL)
	}
	if sc.Results[0].ExistingStatus == nil || *sc.Results[0].ExistingStatus != "fetched" {
		st := "<nil>"
		if sc.Results[0].ExistingStatus != nil {
			st = *sc.Results[0].ExistingStatus
		}
		t.Errorf("expected results[0].existing_status=fetched, got %s", st)
	}
	// Second hit is new.
	if sc.Results[1].AlreadyExists {
		t.Errorf("expected results[1].already_exists=false for %q", sc.Results[1].URL)
	}
	if sc.Results[1].DOI != "10.1234/new" {
		t.Errorf("expected results[1].doi=10.1234/new, got %q", sc.Results[1].DOI)
	}
}

// TestMCP_SearchSources_RBACDeny verifies a user without facts:read on
// the repository gets a permission-denied tool error. A fresh
// non-sysadmin user with no role on the repo is the negative case.
func TestMCP_SearchSources_RBACDeny(t *testing.T) {
	provider := &stubSearchProvider{
		results: []search.SearchResult{{Title: "X", URL: "https://example.com/x"}},
	}
	env := testutil.NewTestEnvWithSearch(t, map[string]search.SearchProvider{"stub": provider})
	defer env.Server.Close()

	// Sysadmin creates a repo they own; the non-admin user below has no role on it.
	admin := bootstrapSysAdmin(t, env, "mcp-ss-adm@example.com")
	_, _, repoID := createRepositoryWithDB(t, admin, "MCP SS Deny", "mcp-ss-deny", "desc", "")

	// A separate, unprivileged user with no role on the repo.
	client := newAuthClient(env.BaseURL)
	client.register("mcp-ss-noperm@example.com", "password123", "No Perm")
	client.token = loginUser(client, "mcp-ss-noperm@example.com", "password123")
	uid := resolveUserID(t, env, "mcp-ss-noperm@example.com")
	tok := mintAccessToken(t, env, uid, "mcp-ss-noperm@example.com", "test-client")

	status, body := mcpCall(t, env.BaseURL, tok, map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{
			"name": "searchSources",
			"arguments": map[string]any{
				"repository": repoID,
				"query":      "x",
			},
		},
	})
	if status != http.StatusOK {
		t.Fatalf("expected 200 (tool error), got %d: %s", status, body)
	}
	var resp mcpResult
	json.Unmarshal(body, &resp)
	if !resp.Result.IsError {
		t.Fatal("expected isError=true")
	}
	if !containsStr(resp.errorText(), "permission") {
		t.Fatalf("expected 'permission' in error, got %q", resp.errorText())
	}
}

// ---------------------------------------------------------------------------
// getSourceTasks: enhanced progress tracking (verbose summary,
// investigation scope, state filter, pagination).
// ---------------------------------------------------------------------------

// mcpStubTaskClient is an in-memory handler.TaskClient for getSourceTasks
// and getReportTasks e2e tests. It owns a canned set of River JobRows and
// returns the subset matching the metadata-containment filter (repo_id,
// optionally source_id or report_id), optionally narrowed by the states
// and kinds filters and by pagination (First/After).
//
// river.JobListParams' filter fields are unexported, so the stub can't
// read them back from the params the handler builds. Instead, each test
// sets the stub's filter fields (MetaRepoID, MetaSourceID, MetaReportID,
// StateFilter, KindFilter, Limit) to the same values it expects the
// handler to build — a standard test-double pattern. The stub then
// applies those filters to its canned jobs the way River's JobList
// would (metadata JSONB @> containment, state IN, kind IN, limit). This
// lets the tests exercise the summary/verbose modes, the drain-protocol
// complete/complete_unreliable signals, pagination, and the
// investigation-scope expansion without booting a real River client.
type mcpStubTaskClient struct {
	jobs []*rivertype.JobRow

	// Filters the test sets to mirror what the handler builds. When
	// a field is the zero value it's treated as "no filter". The stub
	// applies them in metadata → state → kind → limit order.
	MetaRepoID   string // metadata @> {"repo_id": ...}
	MetaSourceID string // metadata @> {"source_id": ...} (optional)
	MetaReportID string // metadata @> {"report_id": ...} (optional)
	StateFilter  string // single state name, "" = all
	KindFilter   string // single kind, "" = all
	Limit        int    // page size, 0 = return all (no pagination)
}

func (c *mcpStubTaskClient) JobList(ctx context.Context, params *river.JobListParams) (*river.JobListResult, error) {
	out := c.jobs

	// Metadata containment: a job matches when its metadata JSON
	// contains every non-zero filter field the test set. This mirrors
	// River's `metadata @> fragment::jsonb`.
	if c.MetaRepoID != "" || c.MetaSourceID != "" || c.MetaReportID != "" {
		filtered := make([]*rivertype.JobRow, 0, len(out))
		for _, j := range out {
			if stubJobMatchesMeta(j, c.MetaRepoID, c.MetaSourceID, c.MetaReportID) {
				filtered = append(filtered, j)
			}
		}
		out = filtered
	}

	// State filter.
	if c.StateFilter != "" {
		filtered := make([]*rivertype.JobRow, 0, len(out))
		for _, j := range out {
			if string(j.State) == c.StateFilter {
				filtered = append(filtered, j)
			}
		}
		out = filtered
	}

	// Kind filter.
	if c.KindFilter != "" {
		filtered := make([]*rivertype.JobRow, 0, len(out))
		for _, j := range out {
			if j.Kind == c.KindFilter {
				filtered = append(filtered, j)
			}
		}
		out = filtered
	}

	// Pagination: limit. (After-cursor pagination is omitted
	// because the canned jobs are small and the tests that need
	// pagination use the page-full signal via Limit.)
	if c.Limit > 0 && len(out) > c.Limit {
		out = out[:c.Limit]
	}

	res := &river.JobListResult{Jobs: out}
	// Mimic River: LastCursor is set whenever the result is non-empty
	// (River always emits one for the last row). The handler decides
	// next_cursor based on rawPageFull (len(result.Jobs)==limit).
	if len(out) > 0 {
		cur := river.JobListCursorFromJob(out[len(out)-1])
		res.LastCursor = cur
	}
	return res, nil
}

// stubJobMatchesMeta reports whether j's metadata JSON contains the
// supplied repo_id/source_id/report_id (each empty field is skipped,
// matching how the handler builds a partial containment fragment).
func stubJobMatchesMeta(j *rivertype.JobRow, repoID, sourceID, reportID string) bool {
	if len(j.Metadata) == 0 {
		return false
	}
	var m struct {
		RepoID   string `json:"repo_id"`
		SourceID string `json:"source_id"`
		ReportID string `json:"report_id"`
	}
	if err := json.Unmarshal(j.Metadata, &m); err != nil {
		return false
	}
	if repoID != "" && m.RepoID != repoID {
		return false
	}
	if sourceID != "" && m.SourceID != sourceID {
		return false
	}
	if reportID != "" && m.ReportID != reportID {
		return false
	}
	return true
}

func (c *mcpStubTaskClient) JobGet(ctx context.Context, id int64) (*rivertype.JobRow, error) {
	for _, j := range c.jobs {
		if j.ID == id {
			return j, nil
		}
	}
	return nil, pgx.ErrNoRows
}

func (c *mcpStubTaskClient) JobCancel(ctx context.Context, id int64) (*rivertype.JobRow, error) {
	return c.JobGet(ctx, id)
}

// makeJob builds a rivertype.JobRow with the given kind, state, and
// metadata (repo_id required; source_id optional). Used to seed the
// mcpStubTaskClient's canned job set.
func makeJob(id int64, kind, state, repoID, sourceID string, finalized bool) *rivertype.JobRow {
	meta := map[string]string{"repo_id": repoID}
	if sourceID != "" {
		meta["source_id"] = sourceID
	}
	metaBytes, _ := json.Marshal(meta)
	now := time.Now()
	var fin *time.Time
	if finalized {
		t := now
		fin = &t
	}
	return &rivertype.JobRow{
		ID:          id,
		Kind:        kind,
		State:       rivertype.JobState(state),
		Attempt:     1,
		CreatedAt:   now,
		FinalizedAt: fin,
		Metadata:    metaBytes,
	}
}

// makeJobWithReport builds a rivertype.JobRow carrying repo_id +
// report_id in metadata (the shape annotate_report jobs have). Used
// to seed the mcpStubTaskClient for getReportTasks tests.
func makeJobWithReport(id int64, kind, state, repoID, reportID string, finalized bool) *rivertype.JobRow {
	meta := map[string]string{"repo_id": repoID, "report_id": reportID}
	metaBytes, _ := json.Marshal(meta)
	now := time.Now()
	var fin *time.Time
	if finalized {
		t := now
		fin = &t
	}
	return &rivertype.JobRow{
		ID:          id,
		Kind:        kind,
		State:       rivertype.JobState(state),
		Attempt:     1,
		CreatedAt:   now,
		FinalizedAt: fin,
		Metadata:    metaBytes,
	}
}

// seedRiverJob inserts a row directly into river_job on the given
// pool so the global-summary (verbose=false) path of
// getSourceTasks/getReportTasks has something to aggregate. The
// row mirrors the shape production workers enqueue: kind, state,
// metadata (repo_id required; source_id optional; report_id
// optional), and the NOT NULL bookkeeping columns River requires
// (args, created_at, scheduled_at, attempt, max_attempts,
// priority, queue). The caller must have run ensureRiverSchema on
// the pool first.
func seedRiverJob(t *testing.T, pool *pgxpool.Pool, kind, state, repoID, sourceID string) {
	t.Helper()
	meta := map[string]string{"repo_id": repoID}
	if sourceID != "" {
		meta["source_id"] = sourceID
	}
	seedRiverJobWithMeta(t, pool, kind, state, meta)
}

// seedRiverJobWithReport inserts a river_job row carrying repo_id +
// report_id in metadata (the shape annotate_report jobs have).
func seedRiverJobWithReport(t *testing.T, pool *pgxpool.Pool, kind, state, repoID, reportID string) {
	t.Helper()
	seedRiverJobWithMeta(t, pool, kind, state, map[string]string{"repo_id": repoID, "report_id": reportID})
}

// seedRiverJobWithMeta is the shared inserter. It marshals the
// metadata map to JSONB so the global summary's metadata @>
// containment filter matches it the way River's JobList would.
// It also sets finalized_at for finalized states (completed /
// cancelled / discarded) to satisfy river_job's
// finalized_or_finalized_at_null check constraint.
func seedRiverJobWithMeta(t *testing.T, pool *pgxpool.Pool, kind, state string, meta map[string]string) {
	t.Helper()
	ctx := context.Background()
	metaBytes, _ := json.Marshal(meta)
	// River's check constraint requires finalized_at to be
	// non-null when the state is finalized (completed/cancelled/
	// discarded). We pass it as a nullable time.Time: nil for
	// non-finalized states, now() for finalized ones.
	var finalizedAt any
	switch rivertype.JobState(state) {
	case rivertype.JobStateCompleted, rivertype.JobStateCancelled, rivertype.JobStateDiscarded:
		finalizedAt = time.Now()
	}
	_, err := pool.Exec(ctx, `
		INSERT INTO river_job (kind, state, args, metadata, created_at, scheduled_at, attempt, max_attempts, priority, queue, finalized_at)
		VALUES ($1, $2, '{}'::jsonb, $3::jsonb, now(), now(), 0, 3, 1, $1, $4)`,
		kind, state, metaBytes, finalizedAt)
	if err != nil {
		t.Fatalf("seed river_job (%s/%s): %v", kind, state, err)
	}
}

// summary mode runs a single SQL GROUP BY on river_job and returns
// GLOBAL counts (every state, every kind, every page) in one call.
// A job set with a mix of running and completed jobs should report
// complete=false and pending_count>0, with counts_by_kind_and_state
// carrying the cross-tab breakdown.
func TestMCP_GetSourceTasks_VerboseSummary(t *testing.T) {
	env := testutil.NewTestEnv(t)
	defer env.Server.Close()
	ensureRiverSchema(t, env.DB)

	admin := bootstrapSysAdmin(t, env, "mcp-gst-sum@example.com")
	_, _, repoID := createRepositoryWithDB(t, admin, "MCP GST Sum", "mcp-gst-sum", "desc", "")

	// Seed real river_job rows on env.DB (the same pool the MCP
	// handler's taskPool points at). 2 completed + 3 pending (1
	// running, 1 available, 1 scheduled) → pending_count=3,
	// running_count=1, complete=false.
	seedRiverJob(t, env.DB, "retrieve_source", string(rivertype.JobStateCompleted), repoID, "")
	seedRiverJob(t, env.DB, "source_decomposition", string(rivertype.JobStateCompleted), repoID, "src-1")
	seedRiverJob(t, env.DB, "embed_facts", string(rivertype.JobStateRunning), repoID, "src-1")
	seedRiverJob(t, env.DB, "deduplicate_facts", string(rivertype.JobStateAvailable), repoID, "src-1")
	seedRiverJob(t, env.DB, "extract_concepts", string(rivertype.JobStateScheduled), repoID, "src-1")

	uid := resolveUserID(t, env, "mcp-gst-sum@example.com")
	tok := mintAccessToken(t, env, uid, "mcp-gst-sum@example.com", "test-client")

	status, body := mcpCall(t, env.BaseURL, tok, map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{
			"name": "getSourceTasks",
			"arguments": map[string]any{
				"repository": repoID,
				"byKind":      true,
			},
		},
	})
	if status != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", status, body)
	}
	var resp struct {
		Result struct {
			StructuredContent struct {
				Verbose             bool               `json:"verbose"`
				CountsByState       map[string]int     `json:"counts_by_state"`
				CountsByKind        map[string]int     `json:"counts_by_kind"`
				CountsByKindAndState map[string]map[string]int `json:"counts_by_kind_and_state"`
				PendingCount        int                `json:"pending_count"`
				RunningCount        int                `json:"running_count"`
				Complete            bool               `json:"complete"`
				Total               int                `json:"total"`
				NextCursor          string             `json:"next_cursor"`
			} `json:"structuredContent"`
		} `json:"result"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("unmarshal: %v: %s", err, body)
	}
	sc := resp.Result.StructuredContent
	if sc.Verbose {
		t.Fatal("expected verbose=false (default)")
	}
	if sc.NextCursor != "" {
		t.Errorf("expected empty next_cursor (global summary is un-paged), got %q", sc.NextCursor)
	}
	if sc.Complete {
		t.Error("expected complete=false (3 pending jobs)")
	}
	if sc.PendingCount != 3 {
		t.Errorf("expected pending_count=3, got %d", sc.PendingCount)
	}
	if sc.RunningCount != 1 {
		t.Errorf("expected running_count=1, got %d", sc.RunningCount)
	}
	if sc.Total != 5 {
		t.Errorf("expected total=5, got %d", sc.Total)
	}
	if sc.CountsByState["running"] != 1 || sc.CountsByState["available"] != 1 || sc.CountsByState["scheduled"] != 1 || sc.CountsByState["completed"] != 2 {
		t.Errorf("unexpected counts_by_state: %v", sc.CountsByState)
	}
	if sc.CountsByKind["embed_facts"] != 1 || sc.CountsByKind["extract_concepts"] != 1 {
		t.Errorf("unexpected counts_by_kind: %v", sc.CountsByKind)
	}
	// Cross-tab: embed_facts has 1 running, retrieve_source has 1 completed.
	if sc.CountsByKindAndState["embed_facts"]["running"] != 1 {
		t.Errorf("expected counts_by_kind_and_state[embed_facts][running]=1, got %v", sc.CountsByKindAndState["embed_facts"])
	}
	if sc.CountsByKindAndState["retrieve_source"]["completed"] != 1 {
		t.Errorf("expected counts_by_kind_and_state[retrieve_source][completed]=1, got %v", sc.CountsByKindAndState["retrieve_source"])
	}
}

// TestMCP_GetSourceTasks_DefaultSummaryOmitsKindCounts verifies the
// new compact default (no byKind, no verbose): the response carries
// counts_by_state + pending_count + running_count + total + complete,
// and OMITS counts_by_kind and counts_by_kind_and_state entirely
// (they must be absent from the JSON, not just empty). This locks in
// the token-saving contract: the per-kind breakdown is opt-in via
// byKind=true.
func TestMCP_GetSourceTasks_DefaultSummaryOmitsKindCounts(t *testing.T) {
	env := testutil.NewTestEnv(t)
	defer env.Server.Close()
	ensureRiverSchema(t, env.DB)

	admin := bootstrapSysAdmin(t, env, "mcp-gst-compact@example.com")
	_, _, repoID := createRepositoryWithDB(t, admin, "MCP GST Compact", "mcp-gst-compact", "desc", "")

	seedRiverJob(t, env.DB, "retrieve_source", string(rivertype.JobStateCompleted), repoID, "")
	seedRiverJob(t, env.DB, "embed_facts", string(rivertype.JobStateRunning), repoID, "src-1")

	uid := resolveUserID(t, env, "mcp-gst-compact@example.com")
	tok := mintAccessToken(t, env, uid, "mcp-gst-compact@example.com", "test-client")

	status, body := mcpCall(t, env.BaseURL, tok, map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{
			"name": "getSourceTasks",
			"arguments": map[string]any{
				"repository": repoID,
			},
		},
	})
	if status != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", status, body)
	}
	// Decode into a raw map so we can assert KEY ABSENCE. Decoding
	// into a typed struct would silently zero missing fields and
	// hide the regression where the key is still emitted.
	var resp struct {
		Result struct {
			StructuredContent map[string]json.RawMessage `json:"structuredContent"`
		} `json:"result"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("unmarshal: %v: %s", err, body)
	}
	sc := resp.Result.StructuredContent

	// Required keys present.
	for _, key := range []string{"verbose", "counts_by_state", "pending_count", "running_count", "complete", "total"} {
		if _, ok := sc[key]; !ok {
			t.Errorf("expected key %q in default summary, missing", key)
		}
	}
	// Opt-in keys MUST be absent in the default.
	for _, key := range []string{"counts_by_kind", "counts_by_kind_and_state"} {
		if _, ok := sc[key]; ok {
			t.Errorf("expected key %q to be ABSENT in default summary (set byKind=true to opt in), present: %s", key, sc[key])
		}
	}
	// Sanity: the state-only payload still reports the running job.
	var state struct {
		CountsByState map[string]int `json:"counts_by_state"`
		PendingCount  int            `json:"pending_count"`
		Complete      bool           `json:"complete"`
	}
	if err := json.Unmarshal(sc["counts_by_state"], &state.CountsByState); err == nil {
		if state.CountsByState["running"] != 1 || state.CountsByState["completed"] != 1 {
			t.Errorf("unexpected counts_by_state: %v", state.CountsByState)
		}
	} else {
		t.Fatalf("decode counts_by_state: %v", err)
	}
	json.Unmarshal(sc["pending_count"], &state.PendingCount)
	if state.PendingCount != 1 {
		t.Errorf("expected pending_count=1, got %d", state.PendingCount)
	}
	json.Unmarshal(sc["complete"], &state.Complete)
	if state.Complete {
		t.Error("expected complete=false (1 pending job)")
	}
}

// TestMCP_GetSourceTasks_CompleteWhenDrained verifies `complete=true`
// when all jobs are finalized. The global summary sees the whole
// scope in one query, so `complete` is globally trustworthy (no
// complete_unreliable, no drain protocol).
func TestMCP_GetSourceTasks_CompleteWhenDrained(t *testing.T) {
	env := testutil.NewTestEnv(t)
	defer env.Server.Close()
	ensureRiverSchema(t, env.DB)

	admin := bootstrapSysAdmin(t, env, "mcp-gst-done@example.com")
	_, _, repoID := createRepositoryWithDB(t, admin, "MCP GST Done", "mcp-gst-done", "desc", "")

	seedRiverJob(t, env.DB, "retrieve_source", string(rivertype.JobStateCompleted), repoID, "")
	seedRiverJob(t, env.DB, "synthesize_concept", string(rivertype.JobStateCompleted), repoID, "src-1")

	uid := resolveUserID(t, env, "mcp-gst-done@example.com")
	tok := mintAccessToken(t, env, uid, "mcp-gst-done@example.com", "test-client")

	status, body := mcpCall(t, env.BaseURL, tok, map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{
			"name": "getSourceTasks",
			"arguments": map[string]any{"repository": repoID},
		},
	})
	if status != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", status, body)
	}
	var resp struct {
		Result struct {
			StructuredContent struct {
				Complete     bool `json:"complete"`
				PendingCount int  `json:"pending_count"`
			} `json:"structuredContent"`
		} `json:"result"`
	}
	json.Unmarshal(body, &resp)
	if !resp.Result.StructuredContent.Complete {
		t.Error("expected complete=true (all jobs finalized, global summary sees them all)")
	}
	if resp.Result.StructuredContent.PendingCount != 0 {
		t.Errorf("expected pending_count=0, got %d", resp.Result.StructuredContent.PendingCount)
	}
}

// TestMCP_GetSourceTasks_VerboseRows verifies verbose=true returns the
// per-job row list with id/kind/state.
func TestMCP_GetSourceTasks_VerboseRows(t *testing.T) {
	env := testutil.NewTestEnv(t)
	defer env.Server.Close()

	admin := bootstrapSysAdmin(t, env, "mcp-gst-vr@example.com")
	_, _, repoID := createRepositoryWithDB(t, admin, "MCP GST VR", "mcp-gst-vr", "desc", "")

	stub := &mcpStubTaskClient{
		MetaRepoID: repoID,
		jobs: []*rivertype.JobRow{
			makeJob(10, "embed_facts", string(rivertype.JobStateRunning), repoID, "src-x", false),
		},
	}
	env.MCP.SetTaskClient(stub)

	uid := resolveUserID(t, env, "mcp-gst-vr@example.com")
	tok := mintAccessToken(t, env, uid, "mcp-gst-vr@example.com", "test-client")

	status, body := mcpCall(t, env.BaseURL, tok, map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{
			"name": "getSourceTasks",
			"arguments": map[string]any{"repository": repoID, "verbose": true},
		},
	})
	if status != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", status, body)
	}
	var resp struct {
		Result struct {
			StructuredContent struct {
				Tasks []struct {
					ID    int64  `json:"id"`
					Kind  string `json:"kind"`
					State string `json:"state"`
				} `json:"tasks"`
				Count int `json:"count"`
			} `json:"structuredContent"`
		} `json:"result"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("unmarshal: %v: %s", err, body)
	}
	sc := resp.Result.StructuredContent
	if sc.Count != 1 {
		t.Fatalf("expected count=1, got %d", sc.Count)
	}
	if len(sc.Tasks) != 1 || sc.Tasks[0].Kind != "embed_facts" || sc.Tasks[0].State != "running" {
		t.Errorf("unexpected tasks: %+v", sc.Tasks)
	}
}

// TestMCP_GetSourceTasks_InvestigationScope verifies the
// investigationId filter: the tool resolves the investigation's
// source_ids and returns only jobs whose metadata source_id matches.
// A job for a source NOT in the investigation must be filtered out.
func TestMCP_GetSourceTasks_InvestigationScope(t *testing.T) {
	env := testutil.NewTestEnv(t)
	defer env.Server.Close()

	admin := bootstrapSysAdmin(t, env, "mcp-gst-inv@example.com")
	_, _, repoID := createRepositoryWithDB(t, admin, "MCP GST Inv", "mcp-gst-inv", "desc", "")

	// Create an investigation and link one source to it; a second
	// source stays unlinked. Jobs carry source_id in metadata; the
	// tool must keep only the linked source's jobs.
	ctx := context.Background()
	queries := store.New(env.DB)
	var repoUUID pgtype.UUID
	if err := repoUUID.Scan(repoID); err != nil {
		t.Fatalf("scan repo id: %v", err)
	}
	srcIn := pgtype.UUID{}
	srcIn.Scan(uuid.NewString())
	queries.CreateSource(ctx, store.CreateSourceParams{ID: srcIn, RepositoryID: repoUUID, Url: "https://example.com/in", Kind: "homepage", Status: "fetched"})
	srcOut := pgtype.UUID{}
	srcOut.Scan(uuid.NewString())
	queries.CreateSource(ctx, store.CreateSourceParams{ID: srcOut, RepositoryID: repoUUID, Url: "https://example.com/out", Kind: "homepage", Status: "fetched"})
	invID := pgtype.UUID{}
	invID.Scan(uuid.NewString())
	queries.CreateInvestigation(ctx, store.CreateInvestigationParams{ID: invID, RepositoryID: repoUUID, Title: "Inv"})
	queries.AddInvestigationSource(ctx, store.AddInvestigationSourceParams{InvestigationID: invID, SourceID: srcIn})

	stub := &mcpStubTaskClient{
		MetaRepoID: repoID,
		jobs: []*rivertype.JobRow{
			makeJob(1, "embed_facts", string(rivertype.JobStateRunning), repoID, srcIn.String(), false),
			makeJob(2, "embed_facts", string(rivertype.JobStateRunning), repoID, srcOut.String(), false),
		},
	}
	env.MCP.SetTaskClient(stub)

	uid := resolveUserID(t, env, "mcp-gst-inv@example.com")
	tok := mintAccessToken(t, env, uid, "mcp-gst-inv@example.com", "test-client")

	status, body := mcpCall(t, env.BaseURL, tok, map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{
			"name": "getSourceTasks",
			"arguments": map[string]any{
				"repository":      repoID,
				"investigationId": invID.String(),
				"verbose":         true,
			},
		},
	})
	if status != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", status, body)
	}
	var resp struct {
		Result struct {
			StructuredContent struct {
				Tasks []struct {
					ID  int64  `json:"id"`
					Kind string `json:"kind"`
				} `json:"tasks"`
				Count int `json:"count"`
			} `json:"structuredContent"`
		} `json:"result"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("unmarshal: %v: %s", err, body)
	}
	sc := resp.Result.StructuredContent
	if sc.Count != 1 {
		t.Fatalf("expected 1 task (only the investigation's source), got %d: %+v", sc.Count, sc.Tasks)
	}
	if sc.Tasks[0].ID != 1 {
		t.Errorf("expected task id=1 (src-in), got %d", sc.Tasks[0].ID)
	}
}

// TestMCP_GetSourceTasks_InvestigationScopeSummary verifies the
// investigationId filter on the GLOBAL summary path (verbose=false).
// The SQL GROUP BY filters by metadata->>'source_id' = ANY($sourceIDs),
// so only jobs for the investigation's sources are counted. A job
// for a source NOT in the investigation is excluded. retrieve_source
// jobs (no source_id) are also excluded (known limitation).
func TestMCP_GetSourceTasks_InvestigationScopeSummary(t *testing.T) {
	env := testutil.NewTestEnv(t)
	defer env.Server.Close()
	ensureRiverSchema(t, env.DB)

	admin := bootstrapSysAdmin(t, env, "mcp-gst-invs@example.com")
	_, _, repoID := createRepositoryWithDB(t, admin, "MCP GST InvS", "mcp-gst-invs", "desc", "")

	ctx := context.Background()
	queries := store.New(env.DB)
	var repoUUID pgtype.UUID
	if err := repoUUID.Scan(repoID); err != nil {
		t.Fatalf("scan repo id: %v", err)
	}
	srcIn := pgtype.UUID{}
	srcIn.Scan(uuid.NewString())
	if _, err := queries.CreateSource(ctx, store.CreateSourceParams{ID: srcIn, RepositoryID: repoUUID, Url: "https://example.com/in", Kind: "homepage", Status: "fetched"}); err != nil {
		t.Fatalf("create src-in: %v", err)
	}
	srcOut := pgtype.UUID{}
	srcOut.Scan(uuid.NewString())
	if _, err := queries.CreateSource(ctx, store.CreateSourceParams{ID: srcOut, RepositoryID: repoUUID, Url: "https://example.com/out", Kind: "homepage", Status: "fetched"}); err != nil {
		t.Fatalf("create src-out: %v", err)
	}
	invID := pgtype.UUID{}
	invID.Scan(uuid.NewString())
	if _, err := queries.CreateInvestigation(ctx, store.CreateInvestigationParams{ID: invID, RepositoryID: repoUUID, Title: "InvS"}); err != nil {
		t.Fatalf("create investigation: %v", err)
	}
	if err := queries.AddInvestigationSource(ctx, store.AddInvestigationSourceParams{InvestigationID: invID, SourceID: srcIn}); err != nil {
		t.Fatalf("link source: %v", err)
	}

	// Seed 2 jobs for src-in (1 running, 1 completed) and 1 job for
	// src-out (running). The investigation-scoped summary must count
	// only the 2 src-in jobs → pending_count=1, total=2.
	seedRiverJob(t, env.DB, "embed_facts", string(rivertype.JobStateRunning), repoID, srcIn.String())
	seedRiverJob(t, env.DB, "source_decomposition", string(rivertype.JobStateCompleted), repoID, srcIn.String())
	seedRiverJob(t, env.DB, "embed_facts", string(rivertype.JobStateRunning), repoID, srcOut.String())

	uid := resolveUserID(t, env, "mcp-gst-invs@example.com")
	tok := mintAccessToken(t, env, uid, "mcp-gst-invs@example.com", "test-client")

	status, body := mcpCall(t, env.BaseURL, tok, map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{
			"name": "getSourceTasks",
			"arguments": map[string]any{
				"repository":      repoID,
				"investigationId": invID.String(),
				"byKind":          true,
			},
		},
	})
	if status != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", status, body)
	}
	var resp struct {
		Result struct {
			StructuredContent struct {
				PendingCount int            `json:"pending_count"`
				RunningCount int            `json:"running_count"`
				Total        int            `json:"total"`
				Complete     bool           `json:"complete"`
				CountsByKind map[string]int `json:"counts_by_kind"`
			} `json:"structuredContent"`
		} `json:"result"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("unmarshal: %v: %s", err, body)
	}
	sc := resp.Result.StructuredContent
	if sc.Total != 2 {
		t.Errorf("expected total=2 (only src-in jobs), got %d", sc.Total)
	}
	if sc.PendingCount != 1 {
		t.Errorf("expected pending_count=1 (1 running src-in job), got %d", sc.PendingCount)
	}
	if sc.RunningCount != 1 {
		t.Errorf("expected running_count=1, got %d", sc.RunningCount)
	}
	if sc.Complete {
		t.Error("expected complete=false (1 pending job)")
	}
	if sc.CountsByKind["embed_facts"] != 1 || sc.CountsByKind["source_decomposition"] != 1 {
		t.Errorf("unexpected counts_by_kind (src-out job should be excluded): %v", sc.CountsByKind)
	}
}

// TestMCP_GetSourceTasks_SourceScopeSummary verifies the sourceId
// filter on the global summary path: the SQL metadata containment
// includes source_id, so only jobs for that source are counted. A
// job for a different source in the same repo is excluded.
func TestMCP_GetSourceTasks_SourceScopeSummary(t *testing.T) {
	env := testutil.NewTestEnv(t)
	defer env.Server.Close()
	ensureRiverSchema(t, env.DB)

	admin := bootstrapSysAdmin(t, env, "mcp-gst-ss@example.com")
	_, _, repoID := createRepositoryWithDB(t, admin, "MCP GST SS", "mcp-gst-ss", "desc", "")

	const srcA = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
	const srcB = "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"
	// 2 jobs for srcA (1 running, 1 completed) + 1 job for srcB (running).
	seedRiverJob(t, env.DB, "embed_facts", string(rivertype.JobStateRunning), repoID, srcA)
	seedRiverJob(t, env.DB, "source_decomposition", string(rivertype.JobStateCompleted), repoID, srcA)
	seedRiverJob(t, env.DB, "embed_facts", string(rivertype.JobStateRunning), repoID, srcB)

	uid := resolveUserID(t, env, "mcp-gst-ss@example.com")
	tok := mintAccessToken(t, env, uid, "mcp-gst-ss@example.com", "test-client")

	status, body := mcpCall(t, env.BaseURL, tok, map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{
			"name": "getSourceTasks",
			"arguments": map[string]any{
				"repository": repoID,
				"sourceId":    srcA,
				"byKind":      true,
			},
		},
	})
	if status != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", status, body)
	}
	var resp struct {
		Result struct {
			StructuredContent struct {
				PendingCount int            `json:"pending_count"`
				Total        int            `json:"total"`
				Complete     bool           `json:"complete"`
				CountsByKind map[string]int `json:"counts_by_kind"`
			} `json:"structuredContent"`
		} `json:"result"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("unmarshal: %v: %s", err, body)
	}
	sc := resp.Result.StructuredContent
	if sc.Total != 2 {
		t.Errorf("expected total=2 (only srcA jobs), got %d", sc.Total)
	}
	if sc.PendingCount != 1 {
		t.Errorf("expected pending_count=1 (1 running srcA job), got %d", sc.PendingCount)
	}
	if sc.Complete {
		t.Error("expected complete=false (1 pending job)")
	}
	if sc.CountsByKind["embed_facts"] != 1 || sc.CountsByKind["source_decomposition"] != 1 {
		t.Errorf("unexpected counts_by_kind (srcB job should be excluded): %v", sc.CountsByKind)
	}
}

// TestMCP_GetSourceTasks_UnknownState verifies a bad state filter
// surfaces as a tool error (not a silent unfiltered list).
func TestMCP_GetSourceTasks_UnknownState(t *testing.T) {
	env := testutil.NewTestEnv(t)
	defer env.Server.Close()

	admin := bootstrapSysAdmin(t, env, "mcp-gst-us@example.com")
	_, _, repoID := createRepositoryWithDB(t, admin, "MCP GST US", "mcp-gst-us", "desc", "")

	env.MCP.SetTaskClient(&mcpStubTaskClient{})

	uid := resolveUserID(t, env, "mcp-gst-us@example.com")
	tok := mintAccessToken(t, env, uid, "mcp-gst-us@example.com", "test-client")

	status, body := mcpCall(t, env.BaseURL, tok, map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{
			"name": "getSourceTasks",
			"arguments": map[string]any{
				"repository": repoID,
				"state":      "bogus",
			},
		},
	})
	if status != http.StatusOK {
		t.Fatalf("expected 200 (tool error), got %d: %s", status, body)
	}
	var resp mcpResult
	json.Unmarshal(body, &resp)
	if !resp.Result.IsError {
		t.Fatal("expected isError=true")
	}
	if !containsStr(resp.errorText(), "unknown state") {
		t.Fatalf("expected 'unknown state' in error, got %q", resp.errorText())
	}
}

// TestMCP_GetSourceTasks_SourceAndInvestigationMutuallyExclusive
// verifies passing both sourceId and investigationId is a tool error.
func TestMCP_GetSourceTasks_SourceAndInvestigationMutuallyExclusive(t *testing.T) {
	env := testutil.NewTestEnv(t)
	defer env.Server.Close()

	admin := bootstrapSysAdmin(t, env, "mcp-gst-me@example.com")
	_, _, repoID := createRepositoryWithDB(t, admin, "MCP GST ME", "mcp-gst-me", "desc", "")

	env.MCP.SetTaskClient(&mcpStubTaskClient{})

	uid := resolveUserID(t, env, "mcp-gst-me@example.com")
	tok := mintAccessToken(t, env, uid, "mcp-gst-me@example.com", "test-client")

	status, body := mcpCall(t, env.BaseURL, tok, map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{
			"name": "getSourceTasks",
			"arguments": map[string]any{
				"repository":      repoID,
				"sourceId":        "00000000-0000-0000-0000-000000000001",
				"investigationId": "00000000-0000-0000-0000-000000000002",
			},
		},
	})
	if status != http.StatusOK {
		t.Fatalf("expected 200 (tool error), got %d: %s", status, body)
	}
	var resp mcpResult
	json.Unmarshal(body, &resp)
	if !resp.Result.IsError {
		t.Fatal("expected isError=true")
	}
	if !containsStr(resp.errorText(), "mutually exclusive") {
		t.Fatalf("expected 'mutually exclusive' in error, got %q", resp.errorText())
	}
}

// TestMCP_GetSourceTasks_StateFilterIgnoredInSummary verifies the
// new global-summary behavior: state/kind filters are IGNORED in
// summary mode (they are inspection-only). A caller passing
// state=completed still gets the GLOBAL counts (including the
// pending running job), and `complete` reflects the true global
// pending_count, NOT the filtered view. This replaces the old
// complete_unreliable behavior — the global SQL query sees every
// state, so there's no filter-blindness to guard against.
func TestMCP_GetSourceTasks_StateFilterIgnoredInSummary(t *testing.T) {
	env := testutil.NewTestEnv(t)
	defer env.Server.Close()
	ensureRiverSchema(t, env.DB)

	admin := bootstrapSysAdmin(t, env, "mcp-gst-cu@example.com")
	_, _, repoID := createRepositoryWithDB(t, admin, "MCP GST CU", "mcp-gst-cu", "desc", "")

	// 2 completed + 1 running. Even with state=completed, the
	// global summary must report pending_count=1 and
	// complete=false (the running job is still counted).
	seedRiverJob(t, env.DB, "retrieve_source", string(rivertype.JobStateCompleted), repoID, "")
	seedRiverJob(t, env.DB, "source_decomposition", string(rivertype.JobStateCompleted), repoID, "src-1")
	seedRiverJob(t, env.DB, "embed_facts", string(rivertype.JobStateRunning), repoID, "src-1")

	uid := resolveUserID(t, env, "mcp-gst-cu@example.com")
	tok := mintAccessToken(t, env, uid, "mcp-gst-cu@example.com", "test-client")

	status, body := mcpCall(t, env.BaseURL, tok, map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{
			"name": "getSourceTasks",
			"arguments": map[string]any{
				"repository": repoID,
				"state":       "completed",
			},
		},
	})
	if status != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", status, body)
	}
	var resp struct {
		Result struct {
			StructuredContent struct {
				PendingCount       int            `json:"pending_count"`
				RunningCount       int            `json:"running_count"`
				Complete           bool           `json:"complete"`
				CompleteUnreliable bool           `json:"complete_unreliable"`
				CountsByState      map[string]int `json:"counts_by_state"`
				Total              int           `json:"total"`
			} `json:"structuredContent"`
		} `json:"result"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("unmarshal: %v: %s", err, body)
	}
	sc := resp.Result.StructuredContent
	// The state filter is IGNORED — the global query sees all 3 jobs.
	if sc.PendingCount != 1 {
		t.Errorf("expected pending_count=1 (global summary ignores state filter, sees the running job), got %d", sc.PendingCount)
	}
	if sc.RunningCount != 1 {
		t.Errorf("expected running_count=1, got %d", sc.RunningCount)
	}
	if sc.Complete {
		t.Error("expected complete=false (1 pending job globally)")
	}
	if sc.CompleteUnreliable {
		t.Error("expected complete_unreliable=false/absent (global summary is always trustworthy; state filter is ignored)")
	}
	if sc.Total != 3 {
		t.Errorf("expected total=3, got %d", sc.Total)
	}
	if sc.CountsByState["running"] != 1 || sc.CountsByState["completed"] != 2 {
		t.Errorf("expected counts_by_state to show ALL states (filter ignored): %v", sc.CountsByState)
	}
}

// TestMCP_GetSourceTasks_KindFilterIgnoredInSummary verifies the
// kind filter is also ignored in summary mode — the global query
// reports every kind, and `complete` reflects the true global
// pending_count (including the running extract_concepts job the
// kind=embed_facts filter would have hidden).
func TestMCP_GetSourceTasks_KindFilterIgnoredInSummary(t *testing.T) {
	env := testutil.NewTestEnv(t)
	defer env.Server.Close()
	ensureRiverSchema(t, env.DB)

	admin := bootstrapSysAdmin(t, env, "mcp-gst-kf@example.com")
	_, _, repoID := createRepositoryWithDB(t, admin, "MCP GST KF", "mcp-gst-kf", "desc", "")

	seedRiverJob(t, env.DB, "embed_facts", string(rivertype.JobStateCompleted), repoID, "src-1")
	seedRiverJob(t, env.DB, "extract_concepts", string(rivertype.JobStateRunning), repoID, "src-1")

	uid := resolveUserID(t, env, "mcp-gst-kf@example.com")
	tok := mintAccessToken(t, env, uid, "mcp-gst-kf@example.com", "test-client")

	status, body := mcpCall(t, env.BaseURL, tok, map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{
			"name": "getSourceTasks",
			"arguments": map[string]any{
				"repository": repoID,
				"kind":        "embed_facts",
				"byKind":      true,
			},
		},
	})
	if status != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", status, body)
	}
	var resp struct {
		Result struct {
			StructuredContent struct {
				Complete           bool           `json:"complete"`
				CompleteUnreliable bool           `json:"complete_unreliable"`
				PendingCount       int            `json:"pending_count"`
				CountsByKind       map[string]int `json:"counts_by_kind"`
			} `json:"structuredContent"`
		} `json:"result"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("unmarshal: %v: %s", err, body)
	}
	sc := resp.Result.StructuredContent
	if sc.Complete {
		t.Error("expected complete=false (1 pending job globally, kind filter ignored)")
	}
	if sc.CompleteUnreliable {
		t.Error("expected complete_unreliable=false/absent (global summary is always trustworthy; kind filter is ignored)")
	}
	if sc.PendingCount != 1 {
		t.Errorf("expected pending_count=1 (the running extract_concepts job), got %d", sc.PendingCount)
	}
	// Both kinds appear even though kind=embed_facts was passed.
	if sc.CountsByKind["embed_facts"] != 1 || sc.CountsByKind["extract_concepts"] != 1 {
		t.Errorf("expected counts_by_kind to show ALL kinds (filter ignored): %v", sc.CountsByKind)
	}
}

// TestMCP_GetSourceTasks_VerboseNextCursorOnlyWhenPageFull verifies
// the verbose-mode next_cursor guard: verbose mode must NOT emit a
// next_cursor when the raw page is not full (even though River sets
// LastCursor for any non-empty result). Without this guard, an agent
// in verbose mode would chase a useless trailing page that returns
// an empty list.
func TestMCP_GetSourceTasks_VerboseNextCursorOnlyWhenPageFull(t *testing.T) {
	env := testutil.NewTestEnv(t)
	defer env.Server.Close()

	admin := bootstrapSysAdmin(t, env, "mcp-gst-vnc@example.com")
	_, _, repoID := createRepositoryWithDB(t, admin, "MCP GST VNC", "mcp-gst-vnc", "desc", "")

	// 2 jobs, default limit 50 → page is NOT full → next_cursor
	// must be empty even in verbose mode (River still sets
	// LastCursor for the last row).
	stub := &mcpStubTaskClient{
		MetaRepoID: repoID,
		jobs: []*rivertype.JobRow{
			makeJob(1, "embed_facts", string(rivertype.JobStateCompleted), repoID, "src-1", true),
			makeJob(2, "source_decomposition", string(rivertype.JobStateCompleted), repoID, "src-1", true),
		},
	}
	env.MCP.SetTaskClient(stub)

	uid := resolveUserID(t, env, "mcp-gst-vnc@example.com")
	tok := mintAccessToken(t, env, uid, "mcp-gst-vnc@example.com", "test-client")

	status, body := mcpCall(t, env.BaseURL, tok, map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{
			"name": "getSourceTasks",
			"arguments": map[string]any{
				"repository": repoID,
				"verbose":     true,
			},
		},
	})
	if status != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", status, body)
	}
	var resp struct {
		Result struct {
			StructuredContent struct {
				Tasks      []json.RawMessage `json:"tasks"`
				Count      int               `json:"count"`
				NextCursor string            `json:"next_cursor"`
			} `json:"structuredContent"`
		} `json:"result"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("unmarshal: %v: %s", err, body)
	}
	sc := resp.Result.StructuredContent
	if sc.Count != 2 {
		t.Fatalf("expected count=2, got %d", sc.Count)
	}
	if sc.NextCursor != "" {
		t.Errorf("expected empty next_cursor (page not full), got %q", sc.NextCursor)
	}
}

// TestMCP_GetSourceTasks_StateFilterApplies verifies the stub honors
// the state filter end-to-end: a state=running poll over a job set
// with a mix of states returns only the running jobs.
func TestMCP_GetSourceTasks_StateFilterApplies(t *testing.T) {
	env := testutil.NewTestEnv(t)
	defer env.Server.Close()

	admin := bootstrapSysAdmin(t, env, "mcp-gst-sf@example.com")
	_, _, repoID := createRepositoryWithDB(t, admin, "MCP GST SF", "mcp-gst-sf", "desc", "")

	stub := &mcpStubTaskClient{
		MetaRepoID:  repoID,
		StateFilter: string(rivertype.JobStateRunning),
		jobs: []*rivertype.JobRow{
			makeJob(1, "embed_facts", string(rivertype.JobStateCompleted), repoID, "src-1", true),
			makeJob(2, "embed_facts", string(rivertype.JobStateRunning), repoID, "src-1", false),
			makeJob(3, "source_decomposition", string(rivertype.JobStateRunning), repoID, "src-1", false),
		},
	}
	env.MCP.SetTaskClient(stub)

	uid := resolveUserID(t, env, "mcp-gst-sf@example.com")
	tok := mintAccessToken(t, env, uid, "mcp-gst-sf@example.com", "test-client")

	status, body := mcpCall(t, env.BaseURL, tok, map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{
			"name": "getSourceTasks",
			"arguments": map[string]any{
				"repository": repoID,
				"state":       "running",
				"verbose":     true,
			},
		},
	})
	if status != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", status, body)
	}
	var resp struct {
		Result struct {
			StructuredContent struct {
				Tasks []struct {
					ID    int64  `json:"id"`
					State string `json:"state"`
				} `json:"tasks"`
				Count int `json:"count"`
			} `json:"structuredContent"`
		} `json:"result"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("unmarshal: %v: %s", err, body)
	}
	sc := resp.Result.StructuredContent
	if sc.Count != 2 {
		t.Fatalf("expected 2 running jobs (state filter), got %d", sc.Count)
	}
	for _, task := range sc.Tasks {
		if task.State != "running" {
			t.Errorf("expected all tasks running, got %q (id=%d)", task.State, task.ID)
		}
	}
}

// TestMCP_GetReportTasks_GlobalSummary verifies getReportTasks
// returns a GLOBAL summary in one call (verbose=false): the SQL
// GROUP BY sees every annotate_report job for the repo+report, so
// pending_count and complete are globally trustworthy with no
// paging or drain protocol. Seeds real river_job rows.
func TestMCP_GetReportTasks_GlobalSummary(t *testing.T) {
	env := testutil.NewTestEnv(t)
	defer env.Server.Close()
	ensureRiverSchema(t, env.DB)

	admin := bootstrapSysAdmin(t, env, "mcp-grt@example.com")
	_, _, repoID := createRepositoryWithDB(t, admin, "MCP GRT", "mcp-grt", "desc", "")

	const reportID = "33333333-3333-3333-3333-333333333333"
	// 1 completed + 1 running annotate_report job for this report.
	seedRiverJobWithReport(t, env.DB, "annotate_report", string(rivertype.JobStateCompleted), repoID, reportID)
	seedRiverJobWithReport(t, env.DB, "annotate_report", string(rivertype.JobStateRunning), repoID, reportID)

	uid := resolveUserID(t, env, "mcp-grt@example.com")
	tok := mintAccessToken(t, env, uid, "mcp-grt@example.com", "test-client")

	status, body := mcpCall(t, env.BaseURL, tok, map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{
			"name": "getReportTasks",
			"arguments": map[string]any{
				"repository": repoID,
				"reportId":   reportID,
				"byKind":     true,
			},
		},
	})
	if status != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", status, body)
	}
	var resp struct {
		Result struct {
			StructuredContent struct {
				Verbose         bool            `json:"verbose"`
				PendingCount    int             `json:"pending_count"`
				RunningCount    int             `json:"running_count"`
				Complete        bool            `json:"complete"`
				Total           int             `json:"total"`
				CountsByState   map[string]int  `json:"counts_by_state"`
				CountsByKind    map[string]int  `json:"counts_by_kind"`
				NextCursor      string          `json:"next_cursor"`
			} `json:"structuredContent"`
		} `json:"result"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("unmarshal: %v: %s", err, body)
	}
	sc := resp.Result.StructuredContent
	if sc.Verbose {
		t.Error("expected verbose=false (default)")
	}
	if sc.PendingCount != 1 {
		t.Errorf("expected pending_count=1 (1 running annotate job), got %d", sc.PendingCount)
	}
	if sc.RunningCount != 1 {
		t.Errorf("expected running_count=1, got %d", sc.RunningCount)
	}
	if sc.Total != 2 {
		t.Errorf("expected total=2, got %d", sc.Total)
	}
	if sc.Complete {
		t.Error("expected complete=false (1 pending job)")
	}
	if sc.CountsByState["running"] != 1 || sc.CountsByState["completed"] != 1 {
		t.Errorf("unexpected counts_by_state: %v", sc.CountsByState)
	}
	if sc.CountsByKind["annotate_report"] != 2 {
		t.Errorf("expected counts_by_kind[annotate_report]=2, got %v", sc.CountsByKind)
	}
	if sc.NextCursor != "" {
		t.Errorf("expected empty next_cursor (global summary is un-paged), got %q", sc.NextCursor)
	}
}

// TestMCP_GetReportTasks_VerboseRows verifies the verbose (paginated)
// path of getReportTasks via the stub TaskClient: it returns the
// per-job row list with next_cursor guarded by page-fullness. The
// verbose path keeps the stub because it exercises River's JobList
// (the global summary uses the real pool instead).
func TestMCP_GetReportTasks_VerboseRows(t *testing.T) {
	env := testutil.NewTestEnv(t)
	defer env.Server.Close()

	admin := bootstrapSysAdmin(t, env, "mcp-grtv@example.com")
	_, _, repoID := createRepositoryWithDB(t, admin, "MCP GRTV", "mcp-grtv", "desc", "")

	const reportID = "44444444-4444-4444-4444-444444444444"
	stub := &mcpStubTaskClient{
		MetaRepoID:   repoID,
		MetaReportID: reportID,
		jobs: []*rivertype.JobRow{
			makeJobWithReport(1, "annotate_report", string(rivertype.JobStateCompleted), repoID, reportID, true),
			makeJobWithReport(2, "annotate_report", string(rivertype.JobStateRunning), repoID, reportID, false),
		},
	}
	env.MCP.SetTaskClient(stub)

	uid := resolveUserID(t, env, "mcp-grtv@example.com")
	tok := mintAccessToken(t, env, uid, "mcp-grtv@example.com", "test-client")

	status, body := mcpCall(t, env.BaseURL, tok, map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{
			"name": "getReportTasks",
			"arguments": map[string]any{
				"repository": repoID,
				"reportId":   reportID,
				"verbose":     true,
			},
		},
	})
	if status != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", status, body)
	}
	var vResp struct {
		Result struct {
			StructuredContent struct {
				Tasks []struct {
					ID   int64  `json:"id"`
					Kind string `json:"kind"`
				} `json:"tasks"`
				Count      int    `json:"count"`
				NextCursor string `json:"next_cursor"`
			} `json:"structuredContent"`
		} `json:"result"`
	}
	if err := json.Unmarshal(body, &vResp); err != nil {
		t.Fatalf("unmarshal verbose: %v: %s", err, body)
	}
	vsc := vResp.Result.StructuredContent
	if vsc.Count != 2 {
		t.Fatalf("expected count=2, got %d", vsc.Count)
	}
	if vsc.NextCursor != "" {
		t.Errorf("expected empty next_cursor in verbose (page not full), got %q", vsc.NextCursor)
	}
}

// TestMCP_GetReportTasks_UnknownState verifies a bad state filter
// surfaces as a tool error in getReportTasks too (mirrors
// getSourceTasks' fail-loud behavior).
func TestMCP_GetReportTasks_UnknownState(t *testing.T) {
	env := testutil.NewTestEnv(t)
	defer env.Server.Close()

	admin := bootstrapSysAdmin(t, env, "mcp-grt-us@example.com")
	_, _, repoID := createRepositoryWithDB(t, admin, "MCP GRT US", "mcp-grt-us", "desc", "")

	env.MCP.SetTaskClient(&mcpStubTaskClient{})

	uid := resolveUserID(t, env, "mcp-grt-us@example.com")
	tok := mintAccessToken(t, env, uid, "mcp-grt-us@example.com", "test-client")

	status, body := mcpCall(t, env.BaseURL, tok, map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{
			"name": "getReportTasks",
			"arguments": map[string]any{
				"repository": repoID,
				"state":      "bogus",
			},
		},
	})
	if status != http.StatusOK {
		t.Fatalf("expected 200 (tool error), got %d: %s", status, body)
	}
	var resp mcpResult
	json.Unmarshal(body, &resp)
	if !resp.Result.IsError {
		t.Fatal("expected isError=true")
	}
	if !containsStr(resp.errorText(), "unknown state") {
		t.Fatalf("expected 'unknown state' in error, got %q", resp.errorText())
	}
}

// TestMCP_GetInvestigation_ReturnsSourceID verifies the getInvestigation
// tool now surfaces each source's `id` (needed so the agent can map
// investigation sources to getSourceTasks polling).
func TestMCP_GetInvestigation_ReturnsSourceID(t *testing.T) {
	env := testutil.NewTestEnv(t)
	defer env.Server.Close()

	admin := bootstrapSysAdmin(t, env, "mcp-gi-sid@example.com")
	_, _, repoID := createRepositoryWithDB(t, admin, "MCP GI SID", "mcp-gi-sid", "desc", "")

	ctx := context.Background()
	queries := store.New(env.DB)
	var repoUUID pgtype.UUID
	if err := repoUUID.Scan(repoID); err != nil {
		t.Fatalf("scan repo id: %v", err)
	}
	srcID := pgtype.UUID{}
	srcID.Scan(uuid.NewString())
	queries.CreateSource(ctx, store.CreateSourceParams{ID: srcID, RepositoryID: repoUUID, Url: "https://example.com/s", Kind: "homepage", Status: "fetched"})
	invID := pgtype.UUID{}
	invID.Scan(uuid.NewString())
	queries.CreateInvestigation(ctx, store.CreateInvestigationParams{ID: invID, RepositoryID: repoUUID, Title: "Inv SID"})
	queries.AddInvestigationSource(ctx, store.AddInvestigationSourceParams{InvestigationID: invID, SourceID: srcID})

	uid := resolveUserID(t, env, "mcp-gi-sid@example.com")
	tok := mintAccessToken(t, env, uid, "mcp-gi-sid@example.com", "test-client")

	status, body := mcpCall(t, env.BaseURL, tok, map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{
			"name": "getInvestigation",
			"arguments": map[string]any{
				"repository":      repoID,
				"investigationId": invID.String(),
			},
		},
	})
	if status != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", status, body)
	}
	var resp struct {
		Result struct {
			StructuredContent struct {
				Sources []struct {
					ID  string `json:"id"`
					URL string `json:"url"`
				} `json:"sources"`
			} `json:"structuredContent"`
		} `json:"result"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("unmarshal: %v: %s", err, body)
	}
	if len(resp.Result.StructuredContent.Sources) != 1 {
		t.Fatalf("expected 1 source, got %d", len(resp.Result.StructuredContent.Sources))
	}
	if resp.Result.StructuredContent.Sources[0].ID != srcID.String() {
		t.Errorf("expected source id %s, got %q", srcID.String(), resp.Result.StructuredContent.Sources[0].ID)
	}
}