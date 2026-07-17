//go:build e2e

package e2e_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/google/uuid"
	"github.com/openktree/open-knowledge-tree/backend/e2e/testutil"
	"github.com/openktree/open-knowledge-tree/backend/internal/config"
	"github.com/openktree/open-knowledge-tree/backend/internal/providers/content_parsing"
	"github.com/openktree/open-knowledge-tree/backend/internal/providers/fetch"
	"github.com/openktree/open-knowledge-tree/backend/internal/store"
	"github.com/openktree/open-knowledge-tree/backend/internal/taskmanager/tasks"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivermigrate"
	"github.com/riverqueue/river/rivertest"
)

// realisticHTML is a minimal but parseable HTML page whose
// extracted text exceeds fetch.MinExtractedLength (200 chars)
// so the fetch strategy's insufficient-content guard does not
// reject it. The e2e stubs serve this body so the worker's
// fetch → persist path exercises a successful parse without
// coupling to a specific article's content.
const realisticHTML = `<!doctype html>
<html><head><title>Test Article</title></head>
<body><article>
<h1>Test Article</h1>
<p>This is a realistic test article body that is long enough to exceed the fetch strategy's minimum extracted length threshold so the parser is confident the page is a real article and the worker persists the row as fetched rather than failing on insufficient content.</p>
<p>A second paragraph to give the parser more context. The body needs to be substantial enough that trafilatura treats it as the main content and not as chrome or navigation.</p>
</article></body></html>`

// grantSourceProviderExecute is a small helper that grants the
// "user" role the "source_provider:execute" permission the
// /sources/* endpoints require. We grant it directly through
// casbin_rule (mirroring the pattern in users_test.go for
// system_admin) so the tests stay independent of the role
// assignment HTTP API.
//
// The casbin model treats this as a grouping: g(user_id, role,
// domain). The seed policy p(user, *, source_provider, execute)
// then matches because the user is grouped into role "user" in
// domain "*".
func grantSourceProviderExecute(t *testing.T, env *testutil.TestEnv, userID string) {
	t.Helper()
	// Grant sysadmin on both domains so EnforceSystemAdmin
	// short-circuits and the user can access all RBAC-gated
	// endpoints (sources, repositories, etc.).
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

// TestSourcesRetrieveEnqueues covers the happy path: an
// authenticated user POSTs a URL to /sources/retrieve, the
// handler classifies it, enqueues a job, and returns 202 with
// a job id.
func TestSourcesRetrieveEnqueues(t *testing.T) {
	env := testutil.NewTestEnv(t)
	client := newAuthClient(env.BaseURL)

	client.register("retrieve@example.com", "password123", "Retrieve User")
	client.token = loginUser(client, "retrieve@example.com", "password123")

	// Grant the user the source_provider:execute permission so
	// the /sources/retrieve middleware lets us through.
	_, meBody := client.do("GET", "/api/v1/users/me", nil)
	var me struct {
		ID string `json:"id"`
	}
	json.Unmarshal(meBody, &me)
	grantSourceProviderExecute(t, env, me.ID)
	client.token = loginUser(client, "retrieve@example.com", "password123")

	body, _ := json.Marshal(map[string]string{
		"url": "https://example.com/some-article",
	})

	resp, raw := client.do("POST", "/api/v1/sources/retrieve", body)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("expected 202, got %d: %s", resp.StatusCode, raw)
	}

	var out struct {
		JobID        string `json:"job_id"`
		ClassifiedAs string `json:"classified_as"`
		Value        string `json:"value"`
		Status       string `json:"status"`
	}
	json.Unmarshal(raw, &out)

	if out.JobID == "" {
		t.Fatal("expected non-empty job_id")
	}
	if out.ClassifiedAs != "url" {
		t.Fatalf("expected classified_as=url, got %q", out.ClassifiedAs)
	}
	if out.Status != "queued" {
		t.Fatalf("expected status=queued, got %q", out.Status)
	}

	// The recording enqueuer should have seen exactly one call
	// with the URL we sent.
	if len(env.TaskEnqueuer.Enqueued) != 1 {
		t.Fatalf("expected 1 enqueued job, got %d", len(env.TaskEnqueuer.Enqueued))
	}
	if env.TaskEnqueuer.Enqueued[0].URL != "https://example.com/some-article" {
		t.Fatalf("expected URL to be passed through, got %q",
			env.TaskEnqueuer.Enqueued[0].URL)
	}
}

// TestSourcesRetrieveProcessFlagForwarded verifies the HTTP
// layer forwards the new "process" flag from the request body
// to the task enqueuer, so a "Fetch and Process" click lands as
// RetrieveSourceArgs{Process: true} on the queue.
func TestSourcesRetrieveProcessFlagForwarded(t *testing.T) {
	env := testutil.NewTestEnv(t)
	client := newAuthClient(env.BaseURL)

	client.register("processflag@example.com", "password123", "Process Flag User")
	client.token = loginUser(client, "processflag@example.com", "password123")

	_, meBody := client.do("GET", "/api/v1/users/me", nil)
	var me struct {
		ID string `json:"id"`
	}
	json.Unmarshal(meBody, &me)
	grantSourceProviderExecute(t, env, me.ID)
	client.token = loginUser(client, "processflag@example.com", "password123")

	body, _ := json.Marshal(map[string]interface{}{
		"url":     "https://example.com/auto-process",
		"process": true,
	})

	resp, raw := client.do("POST", "/api/v1/sources/retrieve", body)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("expected 202, got %d: %s", resp.StatusCode, raw)
	}

	if len(env.TaskEnqueuer.Enqueued) != 1 {
		t.Fatalf("expected 1 enqueued job, got %d", len(env.TaskEnqueuer.Enqueued))
	}
	if !env.TaskEnqueuer.Enqueued[0].Process {
		t.Fatalf("expected Process=true to be forwarded, got %+v", env.TaskEnqueuer.Enqueued[0])
	}
}

// TestSourcesRetrieveDOI proves that a bare DOI is classified
// correctly (the worker will then resolve it via doi.org).
func TestSourcesRetrieveDOI(t *testing.T) {
	env := testutil.NewTestEnv(t)
	client := newAuthClient(env.BaseURL)

	client.register("doi@example.com", "password123", "DOI User")
	client.token = loginUser(client, "doi@example.com", "password123")

	_, meBody := client.do("GET", "/api/v1/users/me", nil)
	var me struct {
		ID string `json:"id"`
	}
	json.Unmarshal(meBody, &me)
	grantSourceProviderExecute(t, env, me.ID)
	client.token = loginUser(client, "doi@example.com", "password123")

	body, _ := json.Marshal(map[string]string{
		"url": "10.1038/nature12373",
	})

	resp, raw := client.do("POST", "/api/v1/sources/retrieve", body)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("expected 202, got %d: %s", resp.StatusCode, raw)
	}

	var out struct {
		ClassifiedAs string `json:"classified_as"`
		Value        string `json:"value"`
	}
	json.Unmarshal(raw, &out)

	if out.ClassifiedAs != "doi" {
		t.Fatalf("expected classified_as=doi, got %q", out.ClassifiedAs)
	}
	if out.Value != "10.1038/nature12373" {
		t.Fatalf("expected value=10.1038/nature12373, got %q", out.Value)
	}
}

// TestSourcesRetrieveRequiresAuth ensures the endpoint is gated
// by the auth + permission middleware like the other
// source-provider endpoints.
func TestSourcesRetrieveRequiresAuth(t *testing.T) {
	env := testutil.NewTestEnv(t)
	client := newAuthClient(env.BaseURL)

	body, _ := json.Marshal(map[string]string{
		"url": "https://example.com",
	})

	resp, _ := client.do("POST", "/api/v1/sources/retrieve", body)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 without auth, got %d", resp.StatusCode)
	}
}

// TestSourcesRetrieveMissingURL is the error case for the
// "url is required" path.
func TestSourcesRetrieveMissingURL(t *testing.T) {
	env := testutil.NewTestEnv(t)
	client := newAuthClient(env.BaseURL)

	client.register("missing@example.com", "password123", "Missing User")
	client.token = loginUser(client, "missing@example.com", "password123")

	_, meBody := client.do("GET", "/api/v1/users/me", nil)
	var me struct {
		ID string `json:"id"`
	}
	json.Unmarshal(meBody, &me)
	grantSourceProviderExecute(t, env, me.ID)
	client.token = loginUser(client, "missing@example.com", "password123")

	// Empty body with a valid Content-Type. The Decode helper on
	// the handler will read it as {} which is fine; we then assert
	// on the "url is required" error.
	resp, raw := client.do("POST", "/api/v1/sources/retrieve", []byte(`{}`))
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", resp.StatusCode, raw)
	}
}

// TestSourcesClassifyMovedOutOfSourceHandler sanity-checks the
// pure-string classifier that the new fetch.ClassifyURL helper
// exposes. The HTTP /sources/classify endpoint now delegates to
// it; the worker uses it too.
func TestSourcesClassifyMovedOutOfSourceHandler(t *testing.T) {
	env := testutil.NewTestEnv(t)
	client := newAuthClient(env.BaseURL)

	client.register("classify@example.com", "password123", "Classify User")
	client.token = loginUser(client, "classify@example.com", "password123")

	_, meBody := client.do("GET", "/api/v1/users/me", nil)
	var me struct {
		ID string `json:"id"`
	}
	json.Unmarshal(meBody, &me)
	grantSourceProviderExecute(t, env, me.ID)
	client.token = loginUser(client, "classify@example.com", "password123")

	body, _ := json.Marshal(map[string]string{
		"url": "https://doi.org/10.1038/nature12373",
	})
	resp, raw := client.do("POST", "/api/v1/sources/classify", body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, raw)
	}

	var out struct {
		Type  string `json:"type"`
		Value string `json:"value"`
	}
	json.Unmarshal(raw, &out)
	if out.Type != "doi" {
		t.Fatalf("expected type=doi, got %q", out.Type)
	}
	if out.Value != "10.1038/nature12373" {
		t.Fatalf("expected value to strip doi.org prefix, got %q", out.Value)
	}
}

// TestRetrieveSourceWorkerPersistsSourceRow proves that when
// the worker is given a RepositoryID it creates a row in
// okt_repository.sources and stamps the body the fetch
// strategy returned into the `content` column. The row must
// end up in 'fetched' state, the content must be a prefix of
// the response body, and the job result must include the
// matching source_id so operators can correlate the River
// job with the per-repo row.
func TestRetrieveSourceWorkerPersistsSourceRow(t *testing.T) {
	env := testutil.NewTestEnv(t)
	defer env.Server.Close()
	ensureRiverSchema(t, env.DB)

	// Set up a sys admin and create a repository. The worker
	// uses the row's `database_name` to resolve the per-repo
	// pool; in the single-DB test env the "default" entry
	// resolves to env.DB, so any per-repo write lands in
	// okt_repository.sources on that same pool.
	admin := bootstrapSysAdmin(t, env, "persist_admin@example.com")
	repoResp, repoBody, repoID := createRepositoryWithDB(t, admin, "Persist Repo", "persist-repo", "desc", "")
	if repoResp.StatusCode != http.StatusCreated {
		t.Fatalf("create repo: %d %s", repoResp.StatusCode, repoBody)
	}

	// Tiny content server: returns a recognizable body that
	// is short enough to fit in the worker's content
	// preview window.
	contentServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(realisticHTML))
	}))
	defer contentServer.Close()

	// Build the worker the same way taskmanager.New would,
	// using the test env's DB and a single-DB registry.
	registry := testutil.NewForTestPool(env.DB)
	systemQueries := store.New(env.DB)
	strategy := fetch.NewFetchStrategy(fetch.NewFetchResolutionProvider())
	worker := tasks.NewRetrieveSourceWorker(nil, strategy, registry, systemQueries, nil, nil, nil, nil)

	driver := riverpgxv5.New(env.DB)
	workers := river.NewWorkers()
	river.AddWorker(workers, worker)
	testWorker := rivertest.NewWorker(t, driver, &river.Config{
		Queues: map[string]river.QueueConfig{
			tasks.QueueRetrieveSource: {MaxWorkers: 1},
		},
		Workers: workers,
	}, worker)

	ctx, cancel := context.WithTimeout(context.Background(), 10*1000*1000*1000)
	defer cancel()

	tx, err := env.DB.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer tx.Rollback(context.Background())

	job, err := testWorker.Work(ctx, t, tx, tasks.RetrieveSourceArgs{
		URL:          contentServer.URL + "/persist",
		RepositoryID: repoID,
	}, &river.InsertOpts{Queue: tasks.QueueRetrieveSource})
	if err != nil {
		t.Fatalf("worker.Work: %v", err)
	}
	if job.EventKind != river.EventKindJobCompleted {
		t.Fatalf("expected job to be completed, got %s", job.EventKind)
	}

	// The job result must include the source_id so callers
	// can correlate the row with the River job.
	output := job.Job.Output()
	if output == nil {
		t.Fatal("expected recorded output on job row")
	}
	var result tasks.RetrieveSourceResult
	if err := json.Unmarshal(output, &result); err != nil {
		t.Fatalf("unmarshal output: %v", err)
	}
	if result.SourceID == "" {
		t.Fatalf("expected non-empty SourceID in job output; got %+v", result)
	}
	if !result.Fetched {
		t.Fatalf("expected Fetched=true; got %+v", result)
	}

	// The row must be present in okt_repository.sources with
	// status='fetched' and the body persisted to `content`.
	var (
		rowID   pgtype.UUID
		rowURL  string
		rowKind string
		rowStat string
		rowCont []byte
		rowErr  *string
	)
	row := env.DB.QueryRow(ctx, `
		SELECT id, url, kind, status, content, error
		FROM okt_repository.sources
		WHERE repository_id = $1 AND url = $2
	`, repoID, contentServer.URL+"/persist")
	if err := row.Scan(&rowID, &rowURL, &rowKind, &rowStat, &rowCont, &rowErr); err != nil {
		t.Fatalf("querying source row: %v", err)
	}
	if !rowID.Valid {
		t.Fatalf("expected non-null source id, got %+v", rowID)
	}
	if rowURL != contentServer.URL+"/persist" {
		t.Errorf("URL = %q, want %q", rowURL, contentServer.URL+"/persist")
	}
	if rowKind != "url" {
		t.Errorf("kind = %q, want url", rowKind)
	}
	if rowStat != "fetched" {
		t.Errorf("status = %q, want fetched", rowStat)
	}
	if len(rowCont) == 0 {
		t.Fatal("expected non-empty content, got empty")
	}
	if rowErr != nil {
		t.Errorf("expected nil error on success, got %q", *rowErr)
	}
	if rowID.String() != result.SourceID {
		t.Errorf("row id %s != job result SourceID %s", rowID.String(), result.SourceID)
	}
}

// TestRetrieveSourceWorkerLinksInvestigation covers the
// fetchAndProcessSource + investigationId path: when the caller
// sets InvestigationID on RetrieveSourceArgs, the worker must
// insert the investigation_sources junction row linking the
// just-persisted source into that investigation once
// persistSource returns the source_id. This is the preferred
// one-call fetch + organize flow for MCP agents; the link is
// best-effort and must not fail the fetch on a bad
// investigation_id (covered by the cross-repo case below).
func TestRetrieveSourceWorkerLinksInvestigation(t *testing.T) {
	env := testutil.NewTestEnv(t)
	defer env.Server.Close()
	ensureRiverSchema(t, env.DB)

	admin := bootstrapSysAdmin(t, env, "invlink_admin@example.com")
	repoResp, repoBody, repoID := createRepositoryWithDB(t, admin, "Inv Link Repo", "inv-link-repo", "desc", "")
	if repoResp.StatusCode != http.StatusCreated {
		t.Fatalf("create repo: %d %s", repoResp.StatusCode, repoBody)
	}

	contentServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(realisticHTML))
	}))
	defer contentServer.Close()

	// Seed an investigation in the repo for the worker to link into.
	pgRepo := pgRepoID(t, repoID)
	ctx := context.Background()
	queries := store.New(env.DB)
	invID := pgtype.UUID{}
	invID.Scan(uuid.NewString())
	topic := "worker-linked sources"
	if _, err := queries.CreateInvestigation(ctx, store.CreateInvestigationParams{
		ID: invID, RepositoryID: pgRepo, Title: "Worker Link Inv", Topic: &topic,
	}); err != nil {
		t.Fatalf("create investigation: %v", err)
	}

	registry := testutil.NewForTestPool(env.DB)
	systemQueries := store.New(env.DB)
	strategy := fetch.NewFetchStrategy(fetch.NewFetchResolutionProvider())
	worker := tasks.NewRetrieveSourceWorker(nil, strategy, registry, systemQueries, nil, nil, nil, nil)

	driver := riverpgxv5.New(env.DB)
	workers := river.NewWorkers()
	river.AddWorker(workers, worker)
	testWorker := rivertest.NewWorker(t, driver, &river.Config{
		Queues: map[string]river.QueueConfig{
			tasks.QueueRetrieveSource: {MaxWorkers: 1},
		},
		Workers: workers,
	}, worker)

	tx, err := env.DB.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer tx.Rollback(context.Background())

	job, err := testWorker.Work(ctx, t, tx, tasks.RetrieveSourceArgs{
		URL:             contentServer.URL + "/inv-link",
		RepositoryID:    repoID,
		InvestigationID: invID.String(),
	}, &river.InsertOpts{Queue: tasks.QueueRetrieveSource})
	if err != nil {
		t.Fatalf("worker.Work: %v", err)
	}
	if job.EventKind != river.EventKindJobCompleted {
		t.Fatalf("expected job to be completed, got %s", job.EventKind)
	}

	// The junction row must exist, linking the fetched source
	// to the investigation.
	var linkCount int
	err = env.DB.QueryRow(ctx, `
		SELECT COUNT(*) FROM okt_repository.investigation_sources
		WHERE investigation_id = $1
	`, invID).Scan(&linkCount)
	if err != nil {
		t.Fatalf("querying investigation_sources: %v", err)
	}
	if linkCount != 1 {
		t.Fatalf("expected 1 investigation_sources row, got %d", linkCount)
	}
}

// TestRetrieveSourceWorkerInvestigationLinkCrossRepoSkipped
// covers the ownership guard: when InvestigationID points to an
// investigation in a different repository, the worker must skip
// the link (and log) rather than silently inserting a cross-repo
// junction row. The fetch itself must still succeed.
func TestRetrieveSourceWorkerInvestigationLinkCrossRepoSkipped(t *testing.T) {
	env := testutil.NewTestEnv(t)
	defer env.Server.Close()
	ensureRiverSchema(t, env.DB)

	admin := bootstrapSysAdmin(t, env, "invlinkxr_admin@example.com")
	repoRespA, repoBodyA, repoAID := createRepositoryWithDB(t, admin, "Inv Link A", "inv-link-a", "desc", "")
	if repoRespA.StatusCode != http.StatusCreated {
		t.Fatalf("create repo A: %d %s", repoRespA.StatusCode, repoBodyA)
	}
	repoRespB, repoBodyB, repoBID := createRepositoryWithDB(t, admin, "Inv Link B", "inv-link-b", "desc", "")
	if repoRespB.StatusCode != http.StatusCreated {
		t.Fatalf("create repo B: %d %s", repoRespB.StatusCode, repoBodyB)
	}

	contentServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(realisticHTML))
	}))
	defer contentServer.Close()

	// Investigation in repo B; we'll fetch into repo A and ask
	// the worker to link into B's investigation — a cross-repo
	// attempt the guard must reject.
	pgRepoB := pgRepoID(t, repoBID)
	ctx := context.Background()
	queries := store.New(env.DB)
	invID := pgtype.UUID{}
	invID.Scan(uuid.NewString())
	if _, err := queries.CreateInvestigation(ctx, store.CreateInvestigationParams{
		ID: invID, RepositoryID: pgRepoB, Title: "Repo B Inv",
	}); err != nil {
		t.Fatalf("create investigation: %v", err)
	}

	registry := testutil.NewForTestPool(env.DB)
	systemQueries := store.New(env.DB)
	strategy := fetch.NewFetchStrategy(fetch.NewFetchResolutionProvider())
	worker := tasks.NewRetrieveSourceWorker(nil, strategy, registry, systemQueries, nil, nil, nil, nil)

	driver := riverpgxv5.New(env.DB)
	workers := river.NewWorkers()
	river.AddWorker(workers, worker)
	testWorker := rivertest.NewWorker(t, driver, &river.Config{
		Queues: map[string]river.QueueConfig{
			tasks.QueueRetrieveSource: {MaxWorkers: 1},
		},
		Workers: workers,
	}, worker)

	tx, err := env.DB.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer tx.Rollback(context.Background())

	job, err := testWorker.Work(ctx, t, tx, tasks.RetrieveSourceArgs{
		URL:             contentServer.URL + "/cross-repo",
		RepositoryID:    repoAID,
		InvestigationID: invID.String(),
	}, &river.InsertOpts{Queue: tasks.QueueRetrieveSource})
	if err != nil {
		t.Fatalf("worker.Work: %v", err)
	}
	// The fetch must still succeed — the bad investigation_id
	// must not fail the job.
	if job.EventKind != river.EventKindJobCompleted {
		t.Fatalf("expected job to be completed despite cross-repo investigation_id, got %s", job.EventKind)
	}

	// No junction row should exist for the investigation
	// (the source is in repo A, the investigation is in repo B).
	var linkCount int
	err = env.DB.QueryRow(ctx, `
		SELECT COUNT(*) FROM okt_repository.investigation_sources
		WHERE investigation_id = $1
	`, invID).Scan(&linkCount)
	if err != nil {
		t.Fatalf("querying investigation_sources: %v", err)
	}
	if linkCount != 0 {
		t.Fatalf("expected 0 investigation_sources rows for cross-repo link, got %d", linkCount)
	}
}

// TestRetrieveSourceWorkerPersistsDoiFromArgs covers the
// search-result click-through path: the URL the caller hands
// to the worker is a non-DOI form (e.g. an openalex.org/W…
// landing page that the cheap classifier can't see through)
// but the caller already knows the bare DOI. The worker
// must persist the DOI on the source row so the UI can
// render it without re-classifying. The classifier-extraction
// path (a bare "10.1038/…" or a doi.org URL) is covered by
// the unit test in internal/providers/fetch/classify_test.go
// and is not duplicated here to avoid needing real network
// access in the e2e suite.
func TestRetrieveSourceWorkerPersistsDoiFromArgs(t *testing.T) {
	env := testutil.NewTestEnv(t)
	defer env.Server.Close()
	ensureRiverSchema(t, env.DB)

	admin := bootstrapSysAdmin(t, env, "doi_admin@example.com")
	repoResp, repoBody, repoID := createRepositoryWithDB(t, admin, "Doi Repo", "doi-repo", "desc", "")
	if repoResp.StatusCode != http.StatusCreated {
		t.Fatalf("create repo: %d %s", repoResp.StatusCode, repoBody)
	}

	contentServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(realisticHTML))
	}))
	defer contentServer.Close()

	registry := testutil.NewForTestPool(env.DB)
	systemQueries := store.New(env.DB)
	strategy := fetch.NewFetchStrategy(fetch.NewFetchResolutionProvider())
	worker := tasks.NewRetrieveSourceWorker(nil, strategy, registry, systemQueries, nil, nil, nil, nil)

	driver := riverpgxv5.New(env.DB)
	workers := river.NewWorkers()
	river.AddWorker(workers, worker)
	testWorker := rivertest.NewWorker(t, driver, &river.Config{
		Queues: map[string]river.QueueConfig{
			tasks.QueueRetrieveSource: {MaxWorkers: 1},
		},
		Workers: workers,
	}, worker)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	tx, err := env.DB.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer tx.Rollback(context.Background())

	const wantDOI = "10.1038/nature12373"
	job, err := testWorker.Work(ctx, t, tx, tasks.RetrieveSourceArgs{
		URL:          contentServer.URL + "/landing",
		RepositoryID: repoID,
		DOI:          wantDOI,
	}, &river.InsertOpts{Queue: tasks.QueueRetrieveSource})
	if err != nil {
		t.Fatalf("worker.Work: %v", err)
	}
	if job.EventKind != river.EventKindJobCompleted {
		t.Fatalf("expected job to be completed, got %s", job.EventKind)
	}

	var rowDoi *string
	if err := env.DB.QueryRow(ctx, `
		SELECT doi FROM okt_repository.sources
		WHERE repository_id = $1 AND url = $2
	`, repoID, contentServer.URL+"/landing").Scan(&rowDoi); err != nil {
		t.Fatalf("querying doi: %v", err)
	}
	if rowDoi == nil {
		t.Fatal("expected non-nil doi on source row")
	}
	if *rowDoi != wantDOI {
		t.Errorf("doi = %q, want %q", *rowDoi, wantDOI)
	}
}

// stubDOIProvider is a test-only fetch.ResolutionProvider that
// claims SourceDOI support and returns a canned ResolvedContent
// without touching the network. It lets the DOI-only worker
// test exercise the full Work path (classify → resolve →
// persist) deterministically, instead of depending on the real
// doi.org redirect chain the production FetchResolutionProvider
// would follow. The parsed text is long enough to clear
// fetch.MinExtractedLength so the worker treats the row as
// fetched with parse_status=ok.
type stubDOIProvider struct {
	result fetch.ResolvedContent
}

func (s *stubDOIProvider) Resolve(ctx context.Context, _ fetch.Resource) (fetch.ResolvedContent, error) {
	return s.result, nil
}

func (s *stubDOIProvider) Supports(t fetch.SourceType) bool { return t == fetch.SourceDOI }

func (s *stubDOIProvider) Describe() fetch.ProviderDescription {
	return fetch.ProviderDescription{Name: "stub-doi", Configured: true, Supports: []string{"doi"}}
}

// TestRetrieveSourceWorkerAcceptsDoiOnly covers the MCP
// fetchAndProcessSource path: an agent that ran searchSources and
// got back hits with `doi` but no canonical `url` enqueues the DOI
// directly, leaving RetrieveSourceArgs.URL empty. Before the fix
// the worker rejected these with "url is required" on every retry
// and River kept re-running them up to MaxAttempts, producing the
// stuck-task storm the operator saw. The worker must now accept a
// bare DOI, synthesize a SourceDOI resource, persist a row whose
// url is the doi.org form (so the (repository_id, url) UNIQUE
// constraint stays meaningful and the UI has a clickable link),
// and record classified_as=doi on the job output.
func TestRetrieveSourceWorkerAcceptsDoiOnly(t *testing.T) {
	env := testutil.NewTestEnv(t)
	defer env.Server.Close()
	ensureRiverSchema(t, env.DB)

	admin := bootstrapSysAdmin(t, env, "doionly_admin@example.com")
	repoResp, repoBody, repoID := createRepositoryWithDB(t, admin, "DOI Only Repo", "doi-only-repo", "desc", "")
	if repoResp.StatusCode != http.StatusCreated {
		t.Fatalf("create repo: %d %s", repoResp.StatusCode, repoBody)
	}

	// A stub provider that handles SourceDOI and returns a
	// canned body the worker will persist as fetched. Using a
	// stub (instead of the real FetchResolutionProvider) keeps
	// the test off the network — the real provider would
	// rewrite the DOI to https://doi.org/<doi> and follow the
	// live redirect chain, which is not viable for a
	// deterministic e2e check.
	stub := &stubDOIProvider{result: fetch.ResolvedContent{
		StatusCode:  http.StatusOK,
		Body:        []byte(realisticHTML),
		ContentType: "text/html",
		FinalURL:    "https://publisher.example.com/article",
		Parsed: content_parsing.ParsedDoc{
			Title: "Test Article",
			Text:  "This is a realistic test article body that is long enough to exceed the fetch strategy's minimum extracted length threshold so the parser is confident the page is a real article and the worker persists the row as fetched rather than failing on insufficient content.",
		},
	}}
	strategy := fetch.NewFetchStrategy(stub)

	registry := testutil.NewForTestPool(env.DB)
	systemQueries := store.New(env.DB)
	worker := tasks.NewRetrieveSourceWorker(nil, strategy, registry, systemQueries, nil, nil, nil, nil)

	driver := riverpgxv5.New(env.DB)
	workers := river.NewWorkers()
	river.AddWorker(workers, worker)
	testWorker := rivertest.NewWorker(t, driver, &river.Config{
		Queues: map[string]river.QueueConfig{
			tasks.QueueRetrieveSource: {MaxWorkers: 1},
		},
		Workers: workers,
	}, worker)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	tx, err := env.DB.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer tx.Rollback(context.Background())

	const wantDOI = "10.1038/nature12373"
	job, err := testWorker.Work(ctx, t, tx, tasks.RetrieveSourceArgs{
		// URL intentionally empty — this is the MCP DOI-only
		// shape that previously failed every retry.
		RepositoryID: repoID,
		DOI:          wantDOI,
	}, &river.InsertOpts{Queue: tasks.QueueRetrieveSource})
	if err != nil {
		t.Fatalf("worker.Work: %v", err)
	}
	if job.EventKind != river.EventKindJobCompleted {
		t.Fatalf("expected job to be completed, got %s", job.EventKind)
	}

	// The job output must report classified_as=doi and a
	// non-empty source_id so callers can correlate the row.
	output := job.Job.Output()
	if output == nil {
		t.Fatal("expected recorded output on job row")
	}
	var result tasks.RetrieveSourceResult
	if err := json.Unmarshal(output, &result); err != nil {
		t.Fatalf("unmarshal output: %v", err)
	}
	if result.ClassifiedAs != fetch.SourceDOI {
		t.Errorf("classified_as = %q, want %q", result.ClassifiedAs, fetch.SourceDOI)
	}
	if result.SourceID == "" {
		t.Fatal("expected non-empty SourceID in job output")
	}
	if !result.Fetched {
		t.Error("expected Fetched=true")
	}

	// The source row must be persisted with the doi.org form
	// as url (the UNIQUE constraint key) and the bare DOI in
	// the doi column. An empty url would have collapsed every
	// DOI-only job onto one row; the doi.org form matches what
	// ClassifyURL produces for the equivalent doi.org URL.
	wantURL := "https://doi.org/" + wantDOI
	var (
		rowURL  string
		rowDoi  *string
		rowKind string
		rowStat string
	)
	row := env.DB.QueryRow(ctx, `
		SELECT url, doi, kind, status
		FROM okt_repository.sources
		WHERE repository_id = $1 AND url = $2
	`, repoID, wantURL)
	if err := row.Scan(&rowURL, &rowDoi, &rowKind, &rowStat); err != nil {
		t.Fatalf("querying source row: %v", err)
	}
	if rowURL != wantURL {
		t.Errorf("url = %q, want %q", rowURL, wantURL)
	}
	if rowDoi == nil || *rowDoi != wantDOI {
		t.Errorf("doi = %v, want %q", rowDoi, wantDOI)
	}
	if rowKind != "doi" {
		t.Errorf("kind = %q, want doi", rowKind)
	}
	if rowStat != "fetched" {
		t.Errorf("status = %q, want fetched", rowStat)
	}
}

// TestRetrieveSourceWorkerRejectsEmptyURLAndDOI covers the new
// error path: when both URL and DOI are empty the worker returns
// a terminal error and River marks the job failed. This guards
// against a regression that silently accepts malformed jobs
// (e.g. an MCP caller passing neither field) and lets them
// retry forever like the pre-fix DOI-only bug did.
func TestRetrieveSourceWorkerRejectsEmptyURLAndDOI(t *testing.T) {
	env := testutil.NewTestEnv(t)
	defer env.Server.Close()
	ensureRiverSchema(t, env.DB)

	// The worker reads neither the registry nor systemQueries
	// when the args are empty (it returns before reaching
	// persistSource), so nil is safe here.
	strategy := fetch.NewFetchStrategy(fetch.NewFetchResolutionProvider())
	worker := tasks.NewRetrieveSourceWorker(nil, strategy, nil, nil, nil, nil, nil, nil)

	driver := riverpgxv5.New(env.DB)
	workers := river.NewWorkers()
	river.AddWorker(workers, worker)
	testWorker := rivertest.NewWorker(t, driver, &river.Config{
		Queues: map[string]river.QueueConfig{
			tasks.QueueRetrieveSource: {MaxWorkers: 1},
		},
		Workers: workers,
	}, worker)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	tx, err := env.DB.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer tx.Rollback(context.Background())

	_, err = testWorker.Work(ctx, t, tx, tasks.RetrieveSourceArgs{}, &river.InsertOpts{Queue: tasks.QueueRetrieveSource})
	if err == nil {
		t.Fatal("expected error for empty URL+DOI, got nil")
	}
}

// TestRetrieveSourceWorkerProcessFlagChainsDecompose covers the
// "Fetch and Process" path: when the caller sets Process=true on
// RetrieveSourceArgs, a successful fetch that yields non-empty
// parsed text must enqueue a source_decomposition job so the
// user gets the full retrieve → decompose → embed pipeline from
// a single action. The chain uses river.ClientFromContext, which
// the rivertest worker populates, and we assert the chained job
// landed in the same transaction via RequireInsertedTx.
func TestRetrieveSourceWorkerProcessFlagChainsDecompose(t *testing.T) {
	env := testutil.NewTestEnv(t)
	defer env.Server.Close()
	ensureRiverSchema(t, env.DB)

	admin := bootstrapSysAdmin(t, env, "process_chain_admin@example.com")
	repoResp, repoBody, repoID := createRepositoryWithDB(t, admin, "Process Chain Repo", "process-chain-repo", "desc", "")
	if repoResp.StatusCode != http.StatusCreated {
		t.Fatalf("create repo: %d %s", repoResp.StatusCode, repoBody)
	}

	// Content server returns a short HTML body the content
	// parser will turn into non-empty parsed_text, satisfying
	// the chain precondition.
	contentServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(realisticHTML))
	}))
	defer contentServer.Close()

	registry := testutil.NewForTestPool(env.DB)
	systemQueries := store.New(env.DB)
	strategy := fetch.NewFetchStrategy(fetch.NewFetchResolutionProvider())
	worker := tasks.NewRetrieveSourceWorker(nil, strategy, registry, systemQueries, nil, nil, nil, nil)
	// Register a stub decomposition worker alongside the
	// retrieve worker so the River client on the test context
	// accepts the chained source_decomposition insert. The
	// stub's Work is never exercised here — we only assert the
	// job was inserted.
	decompWorker := tasks.NewSourceDecompositionWorker(stubChunker{}, stubFactExtractor{}, nil, config.DecompositionFactConfig{}, config.DecompositionImageConfig{Enabled: false}, registry, systemQueries, nil, nil)

	driver := riverpgxv5.New(env.DB)
	workers := river.NewWorkers()
	river.AddWorker(workers, worker)
	river.AddWorker(workers, decompWorker)
	testWorker := rivertest.NewWorker(t, driver, &river.Config{
		Queues: map[string]river.QueueConfig{
			tasks.QueueRetrieveSource:      {MaxWorkers: 1},
			tasks.QueueSourceDecomposition: {MaxWorkers: 1},
		},
		Workers: workers,
	}, worker)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	tx, err := env.DB.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer tx.Rollback(context.Background())

	job, err := testWorker.Work(ctx, t, tx, tasks.RetrieveSourceArgs{
		URL:          contentServer.URL + "/chain",
		RepositoryID: repoID,
		Process:      true,
	}, &river.InsertOpts{Queue: tasks.QueueRetrieveSource})
	if err != nil {
		t.Fatalf("worker.Work: %v", err)
	}
	if job.EventKind != river.EventKindJobCompleted {
		t.Fatalf("expected job to be completed, got %s", job.EventKind)
	}

	// Pull the source_id out of the recorded job output so the
	// chained-decompose assertion can match on the exact row.
	output := job.Job.Output()
	if output == nil {
		t.Fatal("expected recorded output on job row")
	}
	var result tasks.RetrieveSourceResult
	if err := json.Unmarshal(output, &result); err != nil {
		t.Fatalf("unmarshal output: %v", err)
	}
	if result.SourceID == "" {
		t.Fatalf("expected non-empty SourceID in job output; got %+v", result)
	}

	// The chained source_decomposition job must have been inserted
	// in the same transaction.
	rivertest.RequireInsertedTx[*riverpgxv5.Driver](ctx, t, tx, tasks.SourceDecompositionArgs{
		SourceID:     result.SourceID,
		RepositoryID: repoID,
	}, &rivertest.RequireInsertedOpts{
		Queue: tasks.QueueSourceDecomposition,
	})
}

// TestRetrieveSourceWorkerProcessFlagNoTextFails covers the
// failure precondition: when Process=true but the fetch yielded
// no parseable text (the parser couldn't claim the content type),
// the worker must return a terminal error so River marks the job
// failed — the user explicitly asked for the full pipeline and
// the first stage did not deliver what was needed. We use a
// content type the content parser does not claim (application/
// octet-stream with non-text bytes) so parsed_text stays empty.
func TestRetrieveSourceWorkerProcessFlagNoTextFails(t *testing.T) {
	env := testutil.NewTestEnv(t)
	defer env.Server.Close()
	ensureRiverSchema(t, env.DB)

	admin := bootstrapSysAdmin(t, env, "process_no_text_admin@example.com")
	repoResp, repoBody, repoID := createRepositoryWithDB(t, admin, "Process No Text Repo", "process-no-text-repo", "desc", "")
	if repoResp.StatusCode != http.StatusCreated {
		t.Fatalf("create repo: %d %s", repoResp.StatusCode, repoBody)
	}

	// Serve a content type the parser won't claim (binary
	// octet-stream) with non-text bytes so parsed_text ends up
	// empty/unsupported.
	contentServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte{0x00, 0x01, 0x02, 0x03, 0xFF})
	}))
	defer contentServer.Close()

	registry := testutil.NewForTestPool(env.DB)
	systemQueries := store.New(env.DB)
	strategy := fetch.NewFetchStrategy(fetch.NewFetchResolutionProvider())
	worker := tasks.NewRetrieveSourceWorker(nil, strategy, registry, systemQueries, nil, nil, nil, nil)

	driver := riverpgxv5.New(env.DB)
	workers := river.NewWorkers()
	river.AddWorker(workers, worker)
	testWorker := rivertest.NewWorker(t, driver, &river.Config{
		Queues: map[string]river.QueueConfig{
			tasks.QueueRetrieveSource: {MaxWorkers: 1},
		},
		Workers: workers,
	}, worker)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	tx, err := env.DB.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer tx.Rollback(context.Background())

	_, err = testWorker.Work(ctx, t, tx, tasks.RetrieveSourceArgs{
		URL:          contentServer.URL + "/binary",
		RepositoryID: repoID,
		Process:      true,
	}, &river.InsertOpts{Queue: tasks.QueueRetrieveSource})
	if err == nil {
		t.Fatal("expected error when Process=true and source has no parseable text, got nil")
	}
}

// TestRetrieveSourceWorkerProcessFlagFalseDoesNotChain is the
// negative control: the default (Process=false) path must NOT
// enqueue a source_decomposition job. This guards against the
// chain accidentally firing on every fetch.
func TestRetrieveSourceWorkerProcessFlagFalseDoesNotChain(t *testing.T) {
	env := testutil.NewTestEnv(t)
	defer env.Server.Close()
	ensureRiverSchema(t, env.DB)

	admin := bootstrapSysAdmin(t, env, "process_false_admin@example.com")
	repoResp, repoBody, repoID := createRepositoryWithDB(t, admin, "Process False Repo", "process-false-repo", "desc", "")
	if repoResp.StatusCode != http.StatusCreated {
		t.Fatalf("create repo: %d %s", repoResp.StatusCode, repoBody)
	}

	contentServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(realisticHTML))
	}))
	defer contentServer.Close()

	registry := testutil.NewForTestPool(env.DB)
	systemQueries := store.New(env.DB)
	strategy := fetch.NewFetchStrategy(fetch.NewFetchResolutionProvider())
	worker := tasks.NewRetrieveSourceWorker(nil, strategy, registry, systemQueries, nil, nil, nil, nil)

	driver := riverpgxv5.New(env.DB)
	workers := river.NewWorkers()
	river.AddWorker(workers, worker)
	testWorker := rivertest.NewWorker(t, driver, &river.Config{
		Queues: map[string]river.QueueConfig{
			tasks.QueueRetrieveSource: {MaxWorkers: 1},
		},
		Workers: workers,
	}, worker)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	tx, err := env.DB.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer tx.Rollback(context.Background())

	if _, err := testWorker.Work(ctx, t, tx, tasks.RetrieveSourceArgs{
		URL:          contentServer.URL + "/no-chain",
		RepositoryID: repoID,
	}, &river.InsertOpts{Queue: tasks.QueueRetrieveSource}); err != nil {
		t.Fatalf("worker.Work: %v", err)
	}

	rivertest.RequireNotInsertedTx[*riverpgxv5.Driver](ctx, t, tx, tasks.SourceDecompositionArgs{}, nil)
}

// TestRetrieveSourceWorkerRecordsFailedFetch covers the
// failure path: the upstream server returns 500, the worker
// flips the row to 'failed' and writes the error message.
// The body must not be persisted (the worker only stores
// successful fetches).
func TestRetrieveSourceWorkerRecordsFailedFetch(t *testing.T) {
	env := testutil.NewTestEnv(t)
	defer env.Server.Close()
	ensureRiverSchema(t, env.DB)

	admin := bootstrapSysAdmin(t, env, "failed_admin@example.com")
	repoResp, repoBody, repoID := createRepositoryWithDB(t, admin, "Failed Repo", "failed-repo", "desc", "")
	if repoResp.StatusCode != http.StatusCreated {
		t.Fatalf("create repo: %d %s", repoResp.StatusCode, repoBody)
	}

	contentServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("boom"))
	}))
	defer contentServer.Close()

	registry := testutil.NewForTestPool(env.DB)
	systemQueries := store.New(env.DB)
	strategy := fetch.NewFetchStrategy(fetch.NewFetchResolutionProvider())
	worker := tasks.NewRetrieveSourceWorker(nil, strategy, registry, systemQueries, nil, nil, nil, nil)

	driver := riverpgxv5.New(env.DB)
	workers := river.NewWorkers()
	river.AddWorker(workers, worker)
	testWorker := rivertest.NewWorker(t, driver, &river.Config{
		Queues:  map[string]river.QueueConfig{tasks.QueueRetrieveSource: {MaxWorkers: 1}},
		Workers: workers,
	}, worker)

	ctx, cancel := context.WithTimeout(context.Background(), 10*1000*1000*1000)
	defer cancel()

	tx, err := env.DB.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer tx.Rollback(context.Background())

	job, err := testWorker.Work(ctx, t, tx, tasks.RetrieveSourceArgs{
		URL:          contentServer.URL + "/will-fail",
		RepositoryID: repoID,
	}, &river.InsertOpts{Queue: tasks.QueueRetrieveSource})
	if err != nil {
		t.Fatalf("worker.Work: %v", err)
	}
	if job.EventKind != river.EventKindJobCompleted {
		t.Fatalf("expected job to be completed (failure recorded in result), got %s", job.EventKind)
	}

	var (
		rowStat string
		rowCont *string
		rowErr  *string
	)
	row := env.DB.QueryRow(ctx, `
		SELECT status, content, error
		FROM okt_repository.sources
		WHERE repository_id = $1 AND url = $2
	`, repoID, contentServer.URL+"/will-fail")
	if err := row.Scan(&rowStat, &rowCont, &rowErr); err != nil {
		t.Fatalf("querying source row: %v", err)
	}
	if rowStat != "failed" {
		t.Errorf("status = %q, want failed", rowStat)
	}
	if rowCont != nil {
		t.Errorf("expected nil content on failure, got %q", *rowCont)
	}
	if rowErr == nil || *rowErr == "" {
		t.Fatal("expected non-empty error message on failure")
	}
}

// TestSourcesHTTPRetryHappyPath covers the retry endpoint happy
// path: the worker runs a fetch against a 500-returning server and
// records a 'failed' row; the user POSTs
// /repositories/{slug}/sources/{sourceID}/retry, which resets the
// row to 'pending' (clearing the error + parse_status) and enqueues
// a fresh retrieve_source job carrying the row's URL. The recording
// enqueuer captures the args so the test asserts URL + repo_id are
// forwarded correctly.
func TestSourcesHTTPRetryHappyPath(t *testing.T) {
	env := testutil.NewTestEnv(t)
	defer env.Server.Close()
	ensureRiverSchema(t, env.DB)

	admin := bootstrapSysAdmin(t, env, "retry_admin@example.com")
	repoResp, repoBody, repoID := createRepositoryWithDB(t, admin, "Retry Repo", "retry-repo", "desc", "")
	if repoResp.StatusCode != http.StatusCreated {
		t.Fatalf("create repo: %d %s", repoResp.StatusCode, repoBody)
	}

	contentServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("boom"))
	}))
	defer contentServer.Close()

	registry := testutil.NewForTestPool(env.DB)
	systemQueries := store.New(env.DB)
	strategy := fetch.NewFetchStrategy(fetch.NewFetchResolutionProvider())
	worker := tasks.NewRetrieveSourceWorker(nil, strategy, registry, systemQueries, nil, nil, nil, nil)

	driver := riverpgxv5.New(env.DB)
	workers := river.NewWorkers()
	river.AddWorker(workers, worker)
	testWorker := rivertest.NewWorker(t, driver, &river.Config{
		Queues:  map[string]river.QueueConfig{tasks.QueueRetrieveSource: {MaxWorkers: 1}},
		Workers: workers,
	}, worker)

	ctx, cancel := context.WithTimeout(context.Background(), 10*1000*1000*1000)
	defer cancel()

	tx, err := env.DB.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer tx.Rollback(context.Background())

	sourceURL := contentServer.URL + "/will-fail"
	if _, err := testWorker.Work(ctx, t, tx, tasks.RetrieveSourceArgs{
		URL:          sourceURL,
		RepositoryID: repoID,
	}, &river.InsertOpts{Queue: tasks.QueueRetrieveSource}); err != nil {
		t.Fatalf("worker.Work: %v", err)
	}

	// Read the source_id the worker just persisted (it's the
	// only row in the repo) so we can target the retry endpoint.
	var sourceID string
	row := env.DB.QueryRow(ctx, `SELECT id::text FROM okt_repository.sources WHERE repository_id = $1 AND url = $2`, repoID, sourceURL)
	if err := row.Scan(&sourceID); err != nil {
		t.Fatalf("querying source id: %v", err)
	}

	enqueuedBefore := len(env.TaskEnqueuer.Enqueued)
	retryResp, retryRaw := admin.do("POST", "/api/v1/repositories/retry-repo/sources/"+sourceID+"/retry", nil)
	if retryResp.StatusCode != http.StatusAccepted {
		t.Fatalf("expected 202, got %d: %s", retryResp.StatusCode, retryRaw)
	}

	var out struct {
		JobID    string `json:"job_id"`
		SourceID string `json:"source_id"`
		Status   string `json:"status"`
	}
	if err := json.Unmarshal(retryRaw, &out); err != nil {
		t.Fatalf("decode retry response: %v: %s", err, retryRaw)
	}
	if out.JobID == "" {
		t.Error("expected non-empty job_id")
	}
	if out.SourceID != sourceID {
		t.Errorf("source_id = %q, want %q", out.SourceID, sourceID)
	}
	if out.Status != "queued" {
		t.Errorf("status = %q, want queued", out.Status)
	}

	// Exactly one new retrieve_source job was enqueued, and
	// it carries the failed row's URL + repository_id so the
	// fetch strategy retries the same resource.
	if got := len(env.TaskEnqueuer.Enqueued) - enqueuedBefore; got != 1 {
		t.Fatalf("expected 1 new enqueued job, got %d", got)
	}
	args := env.TaskEnqueuer.Enqueued[len(env.TaskEnqueuer.Enqueued)-1]
	if args.URL != sourceURL {
		t.Errorf("enqueued URL = %q, want %q", args.URL, sourceURL)
	}
	if args.RepositoryID != repoID {
		t.Errorf("enqueued RepositoryID = %q, want %q", args.RepositoryID, repoID)
	}

	// The row must have been reset to 'pending' with a NULL
	// error / parse_status so the UI shows a clean re-queue
	// state before the worker picks the job up.
	var (
		rowStat   string
		rowErr    *string
		rowParse  *string
	)
	queryRow := env.DB.QueryRow(ctx, `SELECT status, error, parse_status FROM okt_repository.sources WHERE id = $1`, sourceID)
	if err := queryRow.Scan(&rowStat, &rowErr, &rowParse); err != nil {
		t.Fatalf("querying reset row: %v", err)
	}
	if rowStat != "pending" {
		t.Errorf("status = %q, want pending", rowStat)
	}
	if rowErr != nil {
		t.Errorf("error = %v, want nil (cleared on reset)", rowErr)
	}
	if rowParse != nil {
		t.Errorf("parse_status = %v, want nil (cleared on reset)", rowParse)
	}
}

// TestSourcesHTTPRetryRejectsNonFailed covers the error case:
// the retry endpoint rejects a row whose status is not 'failed'.
// A 'fetched' row can be re-decomposed via /process; retrying its
// fetch is a no-op the API should refuse.
func TestSourcesHTTPRetryRejectsNonFailed(t *testing.T) {
	env := testutil.NewTestEnv(t)
	defer env.Server.Close()
	ensureRiverSchema(t, env.DB)

	admin := bootstrapSysAdmin(t, env, "retry_nonfailed_admin@example.com")
	repoResp, repoBody, repoID := createRepositoryWithDB(t, admin, "Retry NonFailed Repo", "retry-nonfailed-repo", "desc", "")
	if repoResp.StatusCode != http.StatusCreated {
		t.Fatalf("create repo: %d %s", repoResp.StatusCode, repoBody)
	}

	contentServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(realisticHTML))
	}))
	defer contentServer.Close()

	registry := testutil.NewForTestPool(env.DB)
	systemQueries := store.New(env.DB)
	strategy := fetch.NewFetchStrategy(fetch.NewFetchResolutionProvider())
	worker := tasks.NewRetrieveSourceWorker(nil, strategy, registry, systemQueries, nil, nil, nil, nil)

	driver := riverpgxv5.New(env.DB)
	workers := river.NewWorkers()
	river.AddWorker(workers, worker)
	testWorker := rivertest.NewWorker(t, driver, &river.Config{
		Queues:  map[string]river.QueueConfig{tasks.QueueRetrieveSource: {MaxWorkers: 1}},
		Workers: workers,
	}, worker)

	ctx, cancel := context.WithTimeout(context.Background(), 10*1000*1000*1000)
	defer cancel()

	tx, err := env.DB.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer tx.Rollback(context.Background())

	sourceURL := contentServer.URL + "/ok"
	if _, err := testWorker.Work(ctx, t, tx, tasks.RetrieveSourceArgs{
		URL:          sourceURL,
		RepositoryID: repoID,
	}, &river.InsertOpts{Queue: tasks.QueueRetrieveSource}); err != nil {
		t.Fatalf("worker.Work: %v", err)
	}

	var sourceID string
	row := env.DB.QueryRow(ctx, `SELECT id::text FROM okt_repository.sources WHERE repository_id = $1 AND url = $2`, repoID, sourceURL)
	if err := row.Scan(&sourceID); err != nil {
		t.Fatalf("querying source id: %v", err)
	}

	enqueuedBefore := len(env.TaskEnqueuer.Enqueued)
	resp, raw := admin.do("POST", "/api/v1/repositories/retry-nonfailed-repo/sources/"+sourceID+"/retry", nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 for non-failed retry, got %d: %s", resp.StatusCode, raw)
	}
	if got := len(env.TaskEnqueuer.Enqueued) - enqueuedBefore; got != 0 {
		t.Fatalf("expected 0 new enqueued jobs, got %d", got)
	}
}

// TestSourcesHTTPRetryRequiresAuth covers the auth gate: an
// unauthenticated caller must not reach the retry endpoint. The
// per-repo middleware resolves the slug first, so the test seeds a
// real repository (mirroring TestSourcesUploadRequiresAuth) and
// targets its retry route without a Bearer token.
func TestSourcesHTTPRetryRequiresAuth(t *testing.T) {
	env := testutil.NewTestEnv(t)
	defer env.Server.Close()

	admin := bootstrapSysAdmin(t, env, "retry_auth_admin@example.com")
	const slug = "retry-auth-repo"
	resp, body, _ := createRepositoryWithDB(t, admin, "Retry Auth Repo", slug, "desc", "")
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create repo: status %d, body %s", resp.StatusCode, body)
	}

	anon := newAuthClient(env.BaseURL)
	r, _ := anon.do("POST", "/api/v1/repositories/"+slug+"/sources/00000000-0000-0000-0000-000000000000/retry", nil)
	if r.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 unauthenticated, got %d", r.StatusCode)
	}
}

// TestSourcesHTTPListAfterWorkerRun is the end-to-end check
// that the row the worker writes is visible through the
// public /repositories/{slug}/sources endpoint. This is
// the contract the Sources UI consumes.
func TestSourcesHTTPListAfterWorkerRun(t *testing.T) {
	env := testutil.NewTestEnv(t)
	defer env.Server.Close()
	ensureRiverSchema(t, env.DB)

	admin := bootstrapSysAdmin(t, env, "list_admin@example.com")
	const slug = "list-repo"
	repoResp, repoBody, repoID := createRepositoryWithDB(t, admin, "List Repo", slug, "desc", "")
	if repoResp.StatusCode != http.StatusCreated {
		t.Fatalf("create repo: %d %s", repoResp.StatusCode, repoBody)
	}

	contentServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(realisticHTML))
	}))
	defer contentServer.Close()

	registry := testutil.NewForTestPool(env.DB)
	systemQueries := store.New(env.DB)
	strategy := fetch.NewFetchStrategy(fetch.NewFetchResolutionProvider())
	worker := tasks.NewRetrieveSourceWorker(nil, strategy, registry, systemQueries, nil, nil, nil, nil)

	driver := riverpgxv5.New(env.DB)
	workers := river.NewWorkers()
	river.AddWorker(workers, worker)
	testWorker := rivertest.NewWorker(t, driver, &river.Config{
		Queues:  map[string]river.QueueConfig{tasks.QueueRetrieveSource: {MaxWorkers: 1}},
		Workers: workers,
	}, worker)

	ctx, cancel := context.WithTimeout(context.Background(), 10*1000*1000*1000)
	defer cancel()

	tx, err := env.DB.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer tx.Rollback(context.Background())

	if _, err := testWorker.Work(ctx, t, tx, tasks.RetrieveSourceArgs{
		URL:          contentServer.URL + "/listed",
		RepositoryID: repoID,
	}, &river.InsertOpts{Queue: tasks.QueueRetrieveSource}); err != nil {
		t.Fatalf("worker.Work: %v", err)
	}

	// Now query the public list endpoint and assert the
	// worker-written row is visible.
	listResp, listRaw := admin.do("GET", "/api/v1/repositories/"+slug+"/sources", nil)
	if listResp.StatusCode != http.StatusOK {
		t.Fatalf("list sources: %d %s", listResp.StatusCode, listRaw)
	}
	var list struct {
		Data []struct {
			URL     string  `json:"url"`
			Status  string  `json:"status"`
			Content *string `json:"content"`
		} `json:"data"`
		Total  int64 `json:"total"`
		Limit  int   `json:"limit"`
		Offset int   `json:"offset"`
	}
	if err := json.Unmarshal(listRaw, &list); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if len(list.Data) == 0 {
		t.Fatalf("expected at least one source; got body %s", listRaw)
	}
	if list.Total < 1 {
		t.Errorf("list total = %d, want >= 1", list.Total)
	}
	if list.Limit != 100 {
		t.Errorf("list limit = %d, want 100 (default)", list.Limit)
	}
	var found *struct {
		URL     string  `json:"url"`
		Status  string  `json:"status"`
		Content *string `json:"content"`
	}
	for i := range list.Data {
		if list.Data[i].URL == contentServer.URL+"/listed" {
			found = &list.Data[i]
			break
		}
	}
	if found == nil {
		t.Fatalf("expected URL %q in list; got %+v", contentServer.URL+"/listed", list.Data)
	}
	if found.Status != "fetched" {
		t.Errorf("status = %q, want fetched", found.Status)
	}
	if found.Content == nil || *found.Content == "" {
		t.Error("expected non-empty content in list response")
	}
}

// TestSourcesHTTPGetByID covers the single-source endpoint.
// The test seeds a row via the worker (the same setup the
// list test uses) and then calls the public GET endpoint,
// asserting the row's URL, status, and content are
// preserved end to end.
func TestSourcesHTTPGetByID(t *testing.T) {
	env := testutil.NewTestEnv(t)
	defer env.Server.Close()
	ensureRiverSchema(t, env.DB)

	admin := bootstrapSysAdmin(t, env, "getbyid_admin@example.com")
	const slug = "get-repo"
	repoResp, repoBody, repoID := createRepositoryWithDB(t, admin, "Get Repo", slug, "desc", "")
	if repoResp.StatusCode != http.StatusCreated {
		t.Fatalf("create repo: %d %s", repoResp.StatusCode, repoBody)
	}

	contentServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(realisticHTML))
	}))
	defer contentServer.Close()

	registry := testutil.NewForTestPool(env.DB)
	systemQueries := store.New(env.DB)
	strategy := fetch.NewFetchStrategy(fetch.NewFetchResolutionProvider())
	worker := tasks.NewRetrieveSourceWorker(nil, strategy, registry, systemQueries, nil, nil, nil, nil)

	driver := riverpgxv5.New(env.DB)
	workers := river.NewWorkers()
	river.AddWorker(workers, worker)
	testWorker := rivertest.NewWorker(t, driver, &river.Config{
		Queues:  map[string]river.QueueConfig{tasks.QueueRetrieveSource: {MaxWorkers: 1}},
		Workers: workers,
	}, worker)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	tx, err := env.DB.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer tx.Rollback(context.Background())

	url := contentServer.URL + "/get-target"
	job, err := testWorker.Work(ctx, t, tx, tasks.RetrieveSourceArgs{
		URL:          url,
		RepositoryID: repoID,
	}, &river.InsertOpts{Queue: tasks.QueueRetrieveSource})
	if err != nil {
		t.Fatalf("worker.Work: %v", err)
	}
	output := job.Job.Output()
	if output == nil {
		t.Fatal("expected recorded output on job row")
	}
	var result tasks.RetrieveSourceResult
	if err := json.Unmarshal(output, &result); err != nil {
		t.Fatalf("unmarshal output: %v", err)
	}

	// GET the single source via the HTTP endpoint.
	getResp, getRaw := admin.do("GET", "/api/v1/repositories/"+slug+"/sources/"+result.SourceID, nil)
	if getResp.StatusCode != http.StatusOK {
		t.Fatalf("get source: %d %s", getResp.StatusCode, getRaw)
	}
	var envelope struct {
		Source struct {
			ID      string  `json:"id"`
			URL     string  `json:"url"`
			Status  string  `json:"status"`
			Content *string `json:"content"`
		} `json:"source"`
	}
	if err := json.Unmarshal(getRaw, &envelope); err != nil {
		t.Fatalf("decode: %v", err)
	}
	got := envelope.Source
	if got.ID != result.SourceID {
		t.Errorf("id = %q, want %q", got.ID, result.SourceID)
	}
	if got.URL != url {
		t.Errorf("url = %q, want %q", got.URL, url)
	}
	if got.Status != "fetched" {
		t.Errorf("status = %q, want fetched", got.Status)
	}
	if got.Content == nil || *got.Content == "" {
		t.Error("expected non-empty content")
	}

	// 404 on an unknown id.
	unknown := "00000000-0000-0000-0000-000000000000"
	missResp, _ := admin.do("GET", "/api/v1/repositories/"+slug+"/sources/"+unknown, nil)
	if missResp.StatusCode != http.StatusNotFound {
		t.Errorf("unknown source: status %d, want 404", missResp.StatusCode)
	}
}

// TestSourcesHTTPDelete covers DELETE
// /repositories/{slug}/sources/{sourceID}: a sys admin
// (who has source:delete via the */* wildcard) can remove a
// row, the endpoint returns 204, and a subsequent GET
// returns 404. The test also covers the not-found path on
// a fresh id and the 401 path with no auth.
func TestSourcesHTTPDelete(t *testing.T) {
	env := testutil.NewTestEnv(t)
	defer env.Server.Close()
	ensureRiverSchema(t, env.DB)

	admin := bootstrapSysAdmin(t, env, "delete_admin@example.com")
	const slug = "delete-repo"
	repoResp, repoBody, repoID := createRepositoryWithDB(t, admin, "Delete Repo", slug, "desc", "")
	if repoResp.StatusCode != http.StatusCreated {
		t.Fatalf("create repo: %d %s", repoResp.StatusCode, repoBody)
	}

	contentServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(realisticHTML))
	}))
	defer contentServer.Close()

	registry := testutil.NewForTestPool(env.DB)
	systemQueries := store.New(env.DB)
	strategy := fetch.NewFetchStrategy(fetch.NewFetchResolutionProvider())
	worker := tasks.NewRetrieveSourceWorker(nil, strategy, registry, systemQueries, nil, nil, nil, nil)

	driver := riverpgxv5.New(env.DB)
	workers := river.NewWorkers()
	river.AddWorker(workers, worker)
	testWorker := rivertest.NewWorker(t, driver, &river.Config{
		Queues:  map[string]river.QueueConfig{tasks.QueueRetrieveSource: {MaxWorkers: 1}},
		Workers: workers,
	}, worker)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	tx, err := env.DB.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer tx.Rollback(context.Background())

	url := contentServer.URL + "/delete-target"
	job, err := testWorker.Work(ctx, t, tx, tasks.RetrieveSourceArgs{
		URL:          url,
		RepositoryID: repoID,
	}, &river.InsertOpts{Queue: tasks.QueueRetrieveSource})
	if err != nil {
		t.Fatalf("worker.Work: %v", err)
	}
	output := job.Job.Output()
	if output == nil {
		t.Fatal("expected recorded output on job row")
	}
	var result tasks.RetrieveSourceResult
	if err := json.Unmarshal(output, &result); err != nil {
		t.Fatalf("unmarshal output: %v", err)
	}

	// Happy path: 204 on delete, then 404 on GET.
	delResp, delRaw := admin.do("DELETE", "/api/v1/repositories/"+slug+"/sources/"+result.SourceID, nil)
	if delResp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete source: %d %s", delResp.StatusCode, delRaw)
	}
	getResp, _ := admin.do("GET", "/api/v1/repositories/"+slug+"/sources/"+result.SourceID, nil)
	if getResp.StatusCode != http.StatusNotFound {
		t.Errorf("expected 404 after delete, got %d", getResp.StatusCode)
	}

	// 404 on a fresh, never-existing id.
	unknown := "00000000-0000-0000-0000-000000000000"
	missResp, _ := admin.do("DELETE", "/api/v1/repositories/"+slug+"/sources/"+unknown, nil)
	if missResp.StatusCode != http.StatusNotFound {
		t.Errorf("delete unknown: status %d, want 404", missResp.StatusCode)
	}

	// 401 with no auth: spin up a fresh, unauthenticated client
	// pointing at the same server.
	noAuth := newAuthClient(env.BaseURL)
	unauthResp, _ := noAuth.do("DELETE", "/api/v1/repositories/"+slug+"/sources/"+result.SourceID, nil)
	if unauthResp.StatusCode != http.StatusUnauthorized {
		t.Errorf("delete no-auth: status %d, want 401", unauthResp.StatusCode)
	}
}

// ensureRiverSchema runs the bundled River migrations on
// env.DB. testutil.NewTestEnv resets the public schema
// (which includes the River tables), so any test that
// invokes a River worker must call this first. The
// migrator is idempotent: re-running it on an
// already-migrated pool is a no-op.
//
// We define a local helper rather than reusing
// taskmanager_test.go's testPool because that helper
// opens its own pool; the test env here already
// provides env.DB, so opening a second pool would be
// redundant.
func ensureRiverSchema(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	migrator, err := rivermigrate.New(riverpgxv5.New(pool), nil)
	if err != nil {
		t.Fatalf("creating river migrator: %v", err)
	}
	if _, err := migrator.Migrate(ctx, rivermigrate.DirectionUp, nil); err != nil {
		t.Fatalf("running river migrations: %v", err)
	}
}
