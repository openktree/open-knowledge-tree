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

// fetchProvider is the wire shape the /sources/providers
// endpoint returns for one resolution-provider entry. We
// declare it locally instead of importing the handler's
// internal map type so the test asserts on a stable
// contract: any new field the handler adds would simply be
// ignored here until a test opts in.
type fetchProvider struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Type        string   `json:"type"`
	Description string   `json:"description"`
	Requires    string   `json:"requires"`
	Configured  bool     `json:"configured"`
	Supports    []string `json:"supports"`
	Timeout     string   `json:"timeout"`
	Notes       string   `json:"notes"`
	Priority    int      `json:"priority"`
}

type providersResponse struct {
	Search                 []map[string]interface{}    `json:"search"`
	Resolution            []fetchProvider              `json:"resolution"`
	FlareSkipCandidates   []flareSkipCandidate         `json:"flare_skip_candidates"`
	HostFailuresByProvider map[string][]hostCandidate  `json:"host_failures_by_provider"`
	HostFailureMatrix     []hostMatrixRow              `json:"host_failure_matrix"`
	HostSkipProviders     []hostSkipEntry              `json:"host_skip_providers"`
}

// hostMatrixRow mirrors one row of the host_failure_matrix
// array surfaced by /sources/providers.
type hostMatrixRow struct {
	Host                 string `json:"host"`
	TotalAttempts        int32  `json:"total_attempts"`
	FetchFailures        int32  `json:"fetch_failures"`
	FetchSuccesses       int32  `json:"fetch_successes"`
	TlsFailures          int32  `json:"tls_failures"`
	TlsSuccesses         int32  `json:"tls_successes"`
	FlaresolverrFailures  int32 `json:"flaresolverr_failures"`
	FlaresolverrSuccesses int32 `json:"flaresolverr_successes"`
	UnpaywallFailures    int32  `json:"unpaywall_failures"`
	UnpaywallSuccesses   int32  `json:"unpaywall_successes"`
}

// hostSkipEntry mirrors one row of the host_skip_providers list
// surfaced by /sources/providers. Used by the skip-endpoint e2e.
type hostSkipEntry struct {
	Host        string  `json:"host"`
	ProviderID  string  `json:"provider_id"`
	FailureRate float64 `json:"failure_rate"`
	SampleSize  int32   `json:"sample_size"`
	Manual      bool    `json:"manual"`
	ExpiresAt   string  `json:"expires_at"`
}

// hostCandidate is the per-host entry in
// host_failures_by_provider. The field names differ from
// flareSkipCandidate (failures/successes vs
// flare_failures/flare_successes) because the generalized
// query is provider-agnostic.
type hostCandidate struct {
	Host            string `json:"host"`
	TotalAttempts   int64  `json:"total_attempts"`
	Failures        int32  `json:"failures"`
	Successes       int32  `json:"successes"`
}

// flareSkipCandidate is the wire shape of one
// flare_skip_candidates entry on /sources/providers.
type flareSkipCandidate struct {
	Host            string `json:"host"`
	TotalAttempts   int64  `json:"total_attempts"`
	FlareFailures   int64  `json:"flare_failures"`
	FlareSuccesses  int64  `json:"flare_successes"`
}

// TestProvidersEndpointRequiresAuth asserts that
// /sources/providers rejects unauthenticated callers (401).
// We do not want a future regression to make this endpoint
// publicly readable just because the data is non-sensitive.
func TestProvidersEndpointRequiresAuth(t *testing.T) {
	env := testutil.NewTestEnv(t)
	client := newAuthClient(env.BaseURL)

	resp, body := client.do("GET", "/api/v1/sources/providers", nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 for unauthenticated request, got %d: %s",
			resp.StatusCode, body)
	}
}

// TestProvidersEndpointResolutionShape covers the default
// test env: the strategy is wired with only the HTTP Fetch
// provider (Unpaywall is not configured), so the resolution
// slice has exactly one entry. The test pins every new
// field on the response — description, requires,
// configured, supports, timeout, notes, priority — so a
// future refactor that drops one will be caught here.
func TestProvidersEndpointResolutionShape(t *testing.T) {
	env := testutil.NewTestEnv(t)
	client := newAuthClient(env.BaseURL)
	client = registerTestUser(t, env, "providers@example.com", "password123", "Providers User")

	resp, body := client.do("GET", "/api/v1/sources/providers", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, body)
	}

	var out providersResponse
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("unmarshal: %v: %s", err, body)
	}

	if len(out.Resolution) != 1 {
		t.Fatalf("expected 1 resolution provider in test env, got %d: %+v",
			len(out.Resolution), out.Resolution)
	}
	p := out.Resolution[0]

	// Top-level identity fields. The id is the stable slug
	// the UI and the strategy use; a regression that flips
	// it would break the URL path /sources/{id}/search and
	// the frontend "configured" badge lookup.
	if p.ID != "fetch" {
		t.Errorf("expected id=fetch, got %q", p.ID)
	}
	if p.Type != "resolution" {
		t.Errorf("expected type=resolution, got %q", p.Type)
	}
	if p.Name == "" {
		t.Error("expected non-empty name")
	}

	// The new Describe()-backed fields. The HTTP Fetch
	// provider is always-on, so Requires is empty and
	// Configured is true. The Supports slice advertises
	// both url and doi.
	if p.Requires != "" {
		t.Errorf("expected empty requires for always-on fetch provider, got %q", p.Requires)
	}
	if !p.Configured {
		t.Error("expected Configured=true for HTTP Fetch (no env vars required)")
	}
	if len(p.Supports) != 2 || p.Supports[0] != "url" || p.Supports[1] != "doi" {
		t.Errorf("expected Supports=[url, doi], got %v", p.Supports)
	}

	// Priority is 1-based and reflects strategy order.
	// The fetch provider is the only one in the test env,
	// so it must be priority 1.
	if p.Priority != 1 {
		t.Errorf("expected priority=1, got %d", p.Priority)
	}

	// The timeout must round-trip as a Go duration string
	// (e.g. "30s"). We don't pin the exact value because a
	// future config knob could change it; we just require
	// something parseable.
	if p.Timeout == "" {
		t.Error("expected non-empty timeout")
	}
	if p.Description == "" {
		t.Error("expected non-empty description")
	}
	if p.Notes == "" {
		t.Error("expected non-empty notes (Describe() should narrate the strategy behavior)")
	}
}

// TestProvidersEndpointUnpaywallConfigured adds the Unpaywall
// provider to the strategy via a fresh handler wiring, then
// asserts that:
//   - the resolution slice grows to two entries in the
//     order the strategy was built (Unpaywall first, then
//     HTTP Fetch),
//   - priorities are 1 and 2 respectively,
//   - the Unpaywall entry reports Configured=true and
//     Supports=["doi"].
//
// This guards the per-provider Describe() output and the
// priority-ordering in the handler in one go.
func TestProvidersEndpointUnpaywallConfigured(t *testing.T) {
	// The default NewTestEnv only wires the HTTP Fetch
	// provider, so we build a custom env with both
	// Unpaywall and HTTP Fetch. We use a custom API
	// setup that mirrors NewTestEnv but registers
	// Unpaywall first (the production wiring does the
	// same: Unpaywall runs before HTTP Fetch so OA
	// copies are preferred over publisher landing pages).
	env := testutil.NewTestEnvWithUnpaywall(t)
	client := newAuthClient(env.BaseURL)
	client = registerTestUser(t, env, "upw@example.com", "password123", "Unpaywall User")

	resp, body := client.do("GET", "/api/v1/sources/providers", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, body)
	}

	var out providersResponse
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("unmarshal: %v: %s", err, body)
	}

	if len(out.Resolution) != 2 {
		t.Fatalf("expected 2 resolution providers, got %d: %+v",
			len(out.Resolution), out.Resolution)
	}

	// First slot: Unpaywall. Second slot: HTTP Fetch.
	if out.Resolution[0].ID != "unpaywall" {
		t.Errorf("expected resolution[0].id=unpaywall, got %q", out.Resolution[0].ID)
	}
	if out.Resolution[1].ID != "fetch" {
		t.Errorf("expected resolution[1].id=fetch, got %q", out.Resolution[1].ID)
	}

	// Priorities are 1-based and reflect strategy order.
	if out.Resolution[0].Priority != 1 {
		t.Errorf("expected unpaywall priority=1, got %d", out.Resolution[0].Priority)
	}
	if out.Resolution[1].Priority != 2 {
		t.Errorf("expected fetch priority=2, got %d", out.Resolution[1].Priority)
	}

	// Unpaywall specifics: requires the contact email
	// and is configured in this test (we passed one in).
	if out.Resolution[0].Requires != "UNPAYWALL_EMAIL" {
		t.Errorf("expected requires=UNPAYWALL_EMAIL, got %q", out.Resolution[0].Requires)
	}
	if !out.Resolution[0].Configured {
		t.Error("expected Unpaywall Configured=true (we passed an email)")
	}
	if len(out.Resolution[0].Supports) != 1 || out.Resolution[0].Supports[0] != "doi" {
		t.Errorf("expected unpaywall Supports=[doi], got %v", out.Resolution[0].Supports)
	}

	// HTTP Fetch is still always-on in this env.
	if out.Resolution[1].Requires != "" {
		t.Errorf("expected fetch requires='', got %q", out.Resolution[1].Requires)
	}
	if !out.Resolution[1].Configured {
		t.Error("expected fetch Configured=true")
	}
}

// TestProvidersEndpointFlareSkipCandidates verifies that
// /sources/providers surfaces the per-host FlareSolverr
// failure/success counts under flare_skip_candidates when the
// X-Repository-ID header is present and the active repository
// has sources with flaresolverr attempts in their
// fetch_attempts audit trail. The handler scopes the query to
// the active repo's per-repo pool; without the header the field
// is nil (omitted).
//
// The test seeds two sources on the same host
// (flare-fail.example.com): one with a failed flaresolverr
// attempt, one with a successful flaresolverr attempt. The
// response must list the host with flare_failures=1 and
// flare_successes=1.
func TestProvidersEndpointFlareSkipCandidates(t *testing.T) {
	env := testutil.NewTestEnv(t)
	client := newAuthClient(env.BaseURL)
	client = registerTestUser(t, env, "flare@example.com", "password123", "Flare User")

	// Grant sysadmin so the user can create a repository and
	// hit /sources/providers.
	_, meBody := client.do("GET", "/api/v1/users/me", nil)
	var me struct {
		ID string `json:"id"`
	}
	json.Unmarshal(meBody, &me)
	grantSourceProviderExecute(t, env, me.ID)
	client.token = loginUser(client, "flare@example.com", "password123")

	_, _, repoID := createRepository(t, client, "FlareSkip", "flare-skip", "desc")
	if repoID == "" {
		t.Fatal("expected repository id")
	}

	// Seed two sources with flaresolverr attempts in
	// fetch_attempts. We use the sqlc-generated
	// MarkSourceFetchAttempts to control the exact audit-trail
	// shape.
	queries := store.New(env.DB)
	repoUUID := pgtype.UUID{}
	if err := repoUUID.Scan(repoID); err != nil {
		t.Fatalf("scan repo id: %v", err)
	}
	seedFlareSource(t, queries, repoUUID, "https://flare-fail.example.com/page-1",
		`[{"provider":"flaresolverr","success":false,"error":"context deadline exceeded","elapsed_ms":60000},
		  {"provider":"tls","success":false,"error":"upstream returned status 403","elapsed_ms":12000}]`)
	seedFlareSource(t, queries, repoUUID, "https://flare-fail.example.com/page-2",
		`[{"provider":"flaresolverr","success":true,"elapsed_ms":5000},
		  {"provider":"tls","success":false,"error":"upstream returned status 403","elapsed_ms":11000}]`)

	// With X-Repository-ID: the flare_skip_candidates field
	// must list flare-fail.example.com with one failure and one
	// success.
	resp, raw := client.doWithHeaders("GET", "/api/v1/sources/providers", nil, map[string]string{
		"X-Repository-ID": repoID,
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, raw)
	}
	var out providersResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal: %v: %s", err, raw)
	}
	found := false
	for _, c := range out.FlareSkipCandidates {
		if c.Host == "flare-fail.example.com" {
			found = true
			if c.FlareFailures != 1 {
				t.Errorf("expected flare_failures=1 for flare-fail.example.com, got %d", c.FlareFailures)
			}
			if c.FlareSuccesses != 1 {
				t.Errorf("expected flare_successes=1 for flare-fail.example.com, got %d", c.FlareSuccesses)
			}
		}
	}
	if !found {
		t.Errorf("expected flare-fail.example.com in flare_skip_candidates, got %+v", out.FlareSkipCandidates)
	}

	// host_failures_by_provider must list both flaresolverr and
	// tls entries for the same host. The generalized per-provider
	// query covers every provider in the chain.
	if len(out.HostFailuresByProvider) == 0 {
		t.Fatal("expected non-empty host_failures_by_provider, got empty map")
	}
	flareEntries, ok := out.HostFailuresByProvider["flaresolverr"]
	if !ok {
		t.Errorf("expected 'flaresolverr' key in host_failures_by_provider, got keys %v", keysOf(out.HostFailuresByProvider))
	} else {
	 flareFound := false
		for _, c := range flareEntries {
			if c.Host == "flare-fail.example.com" {
				flareFound = true
				if c.Failures != 1 {
					t.Errorf("host_failures_by_provider[flaresolverr]: expected failures=1, got %d", c.Failures)
				}
				if c.Successes != 1 {
					t.Errorf("host_failures_by_provider[flaresolverr]: expected successes=1, got %d", c.Successes)
				}
			}
		}
		if !flareFound {
			t.Errorf("expected flare-fail.example.com in host_failures_by_provider[flaresolverr], got %+v", flareEntries)
		}
	}
	tlsEntries, ok := out.HostFailuresByProvider["tls"]
	if !ok {
		t.Errorf("expected 'tls' key in host_failures_by_provider, got keys %v", keysOf(out.HostFailuresByProvider))
	} else {
		tlsFound := false
		for _, c := range tlsEntries {
			if c.Host == "flare-fail.example.com" {
				tlsFound = true
				if c.Failures != 2 {
					t.Errorf("host_failures_by_provider[tls]: expected failures=2, got %d", c.Failures)
				}
				if c.Successes != 0 {
					t.Errorf("host_failures_by_provider[tls]: expected successes=0, got %d", c.Successes)
				}
			}
		}
		if !tlsFound {
			t.Errorf("expected flare-fail.example.com in host_failures_by_provider[tls], got %+v", tlsEntries)
		}
	}

	// Without X-Repository-ID: the field must be nil/empty
	// (the query is repo-scoped; no repo means no candidates).
	resp, raw = client.do("GET", "/api/v1/sources/providers", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 (global), got %d: %s", resp.StatusCode, raw)
	}
	var global providersResponse
	if err := json.Unmarshal(raw, &global); err != nil {
		t.Fatalf("unmarshal global: %v: %s", err, raw)
	}
	if len(global.FlareSkipCandidates) > 0 {
		t.Errorf("expected no flare_skip_candidates without X-Repository-ID, got %+v", global.FlareSkipCandidates)
	}
	if len(global.HostFailuresByProvider) > 0 {
		t.Errorf("expected no host_failures_by_provider without X-Repository-ID, got %+v", global.HostFailuresByProvider)
	}
}

// keysOf returns the keys of a map for readable error messages.
func keysOf(m map[string][]hostCandidate) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}

// seedFlareSource inserts a source row with the given
// fetch_attempts JSONB. Used by TestProvidersEndpointFlareSkipCandidates
// to set up the per-host FlareSolverr audit trail the
// flare_skip_candidates query reads.
func seedFlareSource(t *testing.T, queries *store.Queries, repoID pgtype.UUID, url, attemptsJSON string) {
	t.Helper()
	ctx := context.Background()
	srcID := pgtype.UUID{}
	if err := srcID.Scan(uuid.NewString()); err != nil {
		t.Fatalf("scan src id: %v", err)
	}
	if _, err := queries.CreateSource(ctx, store.CreateSourceParams{
		ID:           srcID,
		RepositoryID: repoID,
		Url:          url,
		Kind:         "homepage",
		Status:       "failed",
	}); err != nil {
		t.Fatalf("create source: %v", err)
	}
	if _, err := queries.MarkSourceFetchAttempts(ctx, store.MarkSourceFetchAttemptsParams{
		ID:            srcID,
		FetchAttempts: []byte(attemptsJSON),
	}); err != nil {
		t.Fatalf("mark fetch attempts: %v", err)
	}
}

// TestHostSkipEndpoints covers the manual skip management
// endpoints (POST/DELETE /sources/skip, DELETE /sources/skip/all)
// and the host_skip_providers field on /sources/providers.
//
// The test creates a repo, adds a manual skip via POST, asserts it
// appears on GET /sources/providers, removes it via DELETE, asserts
// it's gone, then clears all. It also covers the permission gate
// (a viewer without source_provider:manage gets 403) and the
// repo-scoping (no X-Repository-ID → 400).
func TestHostSkipEndpoints(t *testing.T) {
	env := testutil.NewTestEnv(t)
	client := newAuthClient(env.BaseURL)
	client = registerTestUser(t, env, "skip@example.com", "password123", "Skip User")
	_, meBody := client.do("GET", "/api/v1/users/me", nil)
	var me struct {
		ID string `json:"id"`
	}
	json.Unmarshal(meBody, &me)
	grantSourceProviderExecute(t, env, me.ID)
	client.token = loginUser(client, "skip@example.com", "password123")

	_, _, repoID := createRepository(t, client, "SkipRepo", "skip-repo", "desc")
	if repoID == "" {
		t.Fatal("expected repository id")
	}
	headers := map[string]string{"X-Repository-ID": repoID}

	// A viewer (grouped into the 'viewer' role, which only has
	// source_provider:read, not manage) must be 403'd on POST
	// /sources/skip. registerTestUser grants sysadmin to every
	// user, so we register a plain viewer manually to get a
	// non-admin session.
	viewer := newAuthClient(env.BaseURL)
	vResp, _ := viewer.register("skipviewer@example.com", "password123", "Skip Viewer")
	if vResp.StatusCode != http.StatusCreated {
		t.Fatalf("register viewer: status %d", vResp.StatusCode)
	}
	viewer.token = loginUser(viewer, "skipviewer@example.com", "password123")
	_, vme := viewer.do("GET", "/api/v1/users/me", nil)
	var vmeBody struct {
		ID string `json:"id"`
	}
	json.Unmarshal(vme, &vmeBody)
	// Group the viewer into the 'viewer' role (seed grants only
	// source_provider:read, not manage). No sysadmin grant.
	if _, err := env.DB.Exec(context.Background(),
		`INSERT INTO casbin_rule (p_type, v0, v1, v2) VALUES ('g', $1, 'viewer', '*')`, vmeBody.ID,
	); err != nil {
		t.Fatalf("seed viewer grouping: %v", err)
	}
	viewer.token = loginUser(viewer, "skipviewer@example.com", "password123")
	vResp, _ = viewer.doWithHeaders("POST", "/api/v1/sources/skip",
		[]byte(`{"host":"example.com","provider_id":"fetch"}`),
		map[string]string{"X-Repository-ID": repoID, "Content-Type": "application/json"})
	if vResp.StatusCode != http.StatusForbidden {
		t.Errorf("expected 403 for viewer POST /sources/skip, got %d", vResp.StatusCode)
	}

	// The main user was granted sysadmin via
	// grantSourceProviderExecute above, so it has
	// source_provider:manage (migration 0064 backfills the
	// policy on fresh test DBs). Proceed with mutations.

	// POST /sources/skip without X-Repository-ID → 400.
	badResp, _ := client.do("POST", "/api/v1/sources/skip",
		[]byte(`{"host":"example.com","provider_id":"fetch"}`))
	if badResp.StatusCode != http.StatusBadRequest {
		t.Errorf("expected 400 POST /sources/skip without repo, got %d", badResp.StatusCode)
	}

	// Add a manual skip for example.com → fetch.
	resp, raw := client.doWithHeaders("POST", "/api/v1/sources/skip",
		[]byte(`{"host":"example.com","provider_id":"fetch"}`),
		map[string]string{"X-Repository-ID": repoID, "Content-Type": "application/json"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 POST /sources/skip, got %d: %s", resp.StatusCode, raw)
	}

	// GET /sources/providers must list the skip under
	// host_skip_providers.
	resp, raw = client.doWithHeaders("GET", "/api/v1/sources/providers", nil, headers)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 GET /sources/providers, got %d: %s", resp.StatusCode, raw)
	}
	var out providersResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal: %v: %s", err, raw)
	}
	found := false
	for _, s := range out.HostSkipProviders {
		if s.Host == "example.com" && s.ProviderID == "fetch" {
			found = true
			if !s.Manual {
				t.Errorf("expected manual=true, got false")
			}
		}
	}
	if !found {
		t.Errorf("expected example.com/fetch in host_skip_providers, got %+v", out.HostSkipProviders)
	}

	// DELETE /sources/skip removes the skip.
	resp, raw = client.doWithHeaders("DELETE", "/api/v1/sources/skip",
		[]byte(`{"host":"example.com","provider_id":"fetch"}`),
		map[string]string{"X-Repository-ID": repoID, "Content-Type": "application/json"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 DELETE /sources/skip, got %d: %s", resp.StatusCode, raw)
	}
	resp, raw = client.doWithHeaders("GET", "/api/v1/sources/providers", nil, headers)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 after delete, got %d: %s", resp.StatusCode, raw)
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal after delete: %v", err)
	}
	for _, s := range out.HostSkipProviders {
		if s.Host == "example.com" && s.ProviderID == "fetch" {
			t.Errorf("expected example.com/fetch removed, got %+v", out.HostSkipProviders)
		}
	}

	// Add two skips, then DELETE /sources/skip/all clears them.
	client.doWithHeaders("POST", "/api/v1/sources/skip",
		[]byte(`{"host":"a.example.com","provider_id":"fetch"}`),
		map[string]string{"X-Repository-ID": repoID, "Content-Type": "application/json"})
	client.doWithHeaders("POST", "/api/v1/sources/skip",
		[]byte(`{"host":"b.example.com","provider_id":"tls"}`),
		map[string]string{"X-Repository-ID": repoID, "Content-Type": "application/json"})
	resp, raw = client.doWithHeaders("DELETE", "/api/v1/sources/skip/all", nil, headers)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 DELETE /sources/skip/all, got %d: %s", resp.StatusCode, raw)
	}
	resp, raw = client.doWithHeaders("GET", "/api/v1/sources/providers", nil, headers)
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal after clear: %v", err)
	}
	if len(out.HostSkipProviders) != 0 {
		t.Errorf("expected empty host_skip_providers after clear, got %+v", out.HostSkipProviders)
	}
}

// TestHostSkipSetAndMatrix covers PUT /sources/skip (the full-
// replace endpoint the "Fetch Domains" tab dropdown uses) and
// the host_failure_matrix field on /sources/providers. It sets
// a chain entry point (start at flaresolverr → skip unpaywall,
// tls, fetch), asserts the matrix returns rows for a seeded
// host, asserts the skip rows appear, then sets "do not pull"
// (ban all four) and asserts all four are skipped, then clears.
func TestHostSkipSetAndMatrix(t *testing.T) {
	env := testutil.NewTestEnv(t)
	client := newAuthClient(env.BaseURL)
	client = registerTestUser(t, env, "skipset@example.com", "password123", "Skip Set")
	_, _, repoID := createRepository(t, client, "SkipSet", "skip-set", "desc")
	if repoID == "" {
		t.Fatal("expected repository id")
	}
	headers := map[string]string{"X-Repository-ID": repoID, "Content-Type": "application/json"}

	// Seed a source with fetch_attempts so the matrix has a row.
	queries := store.New(env.DB)
	repoUUID := pgtype.UUID{}
	if err := repoUUID.Scan(repoID); err != nil {
		t.Fatalf("scan repo id: %v", err)
	}
	seedFlareSource(t, queries, repoUUID, "https://matrix.example.com/page-1",
		`[{"provider":"fetch","success":false,"error":"upstream returned status 403","elapsed_ms":12000},
		  {"provider":"tls","success":false,"error":"upstream returned status 403","elapsed_ms":11000},
		  {"provider":"flaresolverr","success":true,"elapsed_ms":5000}]`)

	// GET /sources/providers must surface host_failure_matrix
	// with a row for matrix.example.com.
	resp, raw := client.doWithHeaders("GET", "/api/v1/sources/providers", nil, map[string]string{"X-Repository-ID": repoID})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, raw)
	}
	var out providersResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal: %v: %s", err, raw)
	}
	foundMatrix := false
	for _, m := range out.HostFailureMatrix {
		if m.Host == "matrix.example.com" {
			foundMatrix = true
			if m.FetchFailures != 1 {
				t.Errorf("expected fetch_failures=1, got %d", m.FetchFailures)
			}
			if m.TlsFailures != 1 {
				t.Errorf("expected tls_failures=1, got %d", m.TlsFailures)
			}
			if m.FlaresolverrSuccesses != 1 {
				t.Errorf("expected flaresolverr_successes=1, got %d", m.FlaresolverrSuccesses)
			}
		}
	}
	if !foundMatrix {
		t.Errorf("expected matrix.example.com in host_failure_matrix, got %+v", out.HostFailureMatrix)
	}

	// PUT /sources/skip with choice=flaresolverr → skip
	// unpaywall, tls, fetch (the weaker tiers).
	resp, raw = client.doWithHeaders("PUT", "/api/v1/sources/skip",
		[]byte(`{"host":"matrix.example.com","provider_ids":["unpaywall","tls","fetch"]}`),
		headers)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 PUT /sources/skip, got %d: %s", resp.StatusCode, raw)
	}
	resp, raw = client.doWithHeaders("GET", "/api/v1/sources/providers", nil, map[string]string{"X-Repository-ID": repoID})
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal after put: %v", err)
	}
	skippedIDs := map[string]bool{}
	for _, s := range out.HostSkipProviders {
		if s.Host == "matrix.example.com" {
			skippedIDs[s.ProviderID] = s.Manual
		}
	}
	for _, pid := range []string{"unpaywall", "tls", "fetch"} {
		if !skippedIDs[pid] {
			t.Errorf("expected manual skip for %s on matrix.example.com, got %+v", pid, out.HostSkipProviders)
		}
	}
	if skippedIDs["flaresolverr"] {
		t.Errorf("did not expect flaresolverr skipped for start-at-flaresolverr choice, got %+v", out.HostSkipProviders)
	}

	// PUT /sources/skip with choice=ban → skip all four tiers.
	resp, raw = client.doWithHeaders("PUT", "/api/v1/sources/skip",
		[]byte(`{"host":"matrix.example.com","provider_ids":["unpaywall","tls","fetch","flaresolverr"]}`),
		headers)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 PUT ban, got %d: %s", resp.StatusCode, raw)
	}
	resp, raw = client.doWithHeaders("GET", "/api/v1/sources/providers", nil, map[string]string{"X-Repository-ID": repoID})
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal after ban: %v", err)
	}
	skippedIDs = map[string]bool{}
	for _, s := range out.HostSkipProviders {
		if s.Host == "matrix.example.com" {
			skippedIDs[s.ProviderID] = true
		}
	}
	for _, pid := range []string{"unpaywall", "tls", "fetch", "flaresolverr"} {
		if !skippedIDs[pid] {
			t.Errorf("expected manual skip for %s on ban, got %+v", pid, out.HostSkipProviders)
		}
	}

	// PUT with empty provider_ids clears the host's skips.
	resp, raw = client.doWithHeaders("PUT", "/api/v1/sources/skip",
		[]byte(`{"host":"matrix.example.com","provider_ids":[]}`),
		headers)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 PUT clear, got %d: %s", resp.StatusCode, raw)
	}
	resp, raw = client.doWithHeaders("GET", "/api/v1/sources/providers", nil, map[string]string{"X-Repository-ID": repoID})
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal after clear: %v", err)
	}
	for _, s := range out.HostSkipProviders {
		if s.Host == "matrix.example.com" {
			t.Errorf("expected no skips for matrix.example.com after clear, got %+v", out.HostSkipProviders)
		}
	}
}
