//go:build e2e

package e2e_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/openktree/open-knowledge-tree/backend/e2e/testutil"
	"github.com/openktree/open-knowledge-tree/backend/internal/api"
	"github.com/openktree/open-knowledge-tree/backend/internal/api/handler"
	"github.com/openktree/open-knowledge-tree/backend/internal/audit"
	"github.com/openktree/open-knowledge-tree/backend/internal/config"
	"github.com/openktree/open-knowledge-tree/backend/internal/dbpool"
	"github.com/openktree/open-knowledge-tree/backend/internal/oauth"
	"github.com/openktree/open-knowledge-tree/backend/internal/providers/content_parsing"
	"github.com/openktree/open-knowledge-tree/backend/internal/providers/fetch"
	"github.com/openktree/open-knowledge-tree/backend/internal/providers/registry"
	"github.com/openktree/open-knowledge-tree/backend/internal/rbac"
	"github.com/openktree/open-knowledge-tree/backend/internal/store"
)

// TestRemote_DetailNotConfiguredReturns503 verifies the
// notConfigured fallback for the per-source detail and
// decomposition-by-model proxy endpoints. The default test
// env does not wire a registry client (mirrors the
// deployment shape where the API boots without a registry),
// so the routes must return 503 (service not configured)
// instead of 500. The detail endpoint is the read-only
// browse path that the frontend detail dialog uses.
func TestRemote_DetailNotConfiguredReturns503(t *testing.T) {
	env := testutil.NewTestEnv(t)
	defer env.Server.Close()

	admin := bootstrapSysAdmin(t, env, "remote-detail-notcfg@example.com")
	_, _, repoID := createRepository(t, admin, "RemoteDetail", "remote-detail", "desc")

	resp, body := admin.do("GET", "/api/v1/repositories/"+repoID+"/remote/src-abc", nil)
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("get source detail (not configured): expected 503, got %d: %s", resp.StatusCode, body)
	}

	resp, body = admin.do("GET", "/api/v1/repositories/"+repoID+"/remote/src-abc/decompositions/gpt-4o", nil)
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("get decomposition (not configured): expected 503, got %d: %s", resp.StatusCode, body)
	}
}

// TestRemote_DetailRequiresPermission verifies the new
// detail / decomposition proxy endpoints are gated on the
// remote:read permission just like the list endpoint. A
// regular user (no role assigned) must get 403 on both
// the per-source detail and the per-model decomposition
// paths. This is the deny path the frontend dialog falls
// back to when an under-privileged user reaches the dialog.
//
// The test spins up its own minimal env (mirroring the
// tasksEnvWithRBAC pattern in tasks_test.go) because the
// shared testutil.NewTestEnv doesn't wire a registry
// client — the notConfigured fallback (503) short-circuits
// the permission check. Wiring a stub registry client
// pointed at a local httptest server lets the route run
// its real handler chain, which is the only way the
// repoPerm middleware can produce 403.
func TestRemote_DetailRequiresPermission(t *testing.T) {
	stubRegistry := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Mirror the registry's /api/v1/sources/{id} and
		// /api/v1/sources/{id}/decompositions/{model}
		// shapes just enough to satisfy the proxy handler.
		// The test only cares that the route reaches the
		// repoPerm middleware (and returns 403 for the
		// under-privileged caller); the stub payload is
		// never read.
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(r.URL.Path, "/decompositions/"):
			_, _ = w.Write([]byte(`{"model_id":"stub","facts":[],"concepts":[]}`))
		default:
			_, _ = w.Write([]byte(`{"source":{"id":"src-abc"},"decompositions":[]}`))
		}
	}))
	t.Cleanup(stubRegistry.Close)

	env, _, _, _ := newRemoteEnvWithRegistry(t, stubRegistry.URL)

	admin := bootstrapSysAdmin(t, env, "remote-detail-perms-admin@example.com")
	_, _, repoID := createRepository(t, admin, "RemoteDetailPerms", "remote-detail-perms", "desc")

	plain := registerAndLogin(t, env, "remote-detail-perms-other@example.com")

	resp, body := plain.do("GET", "/api/v1/repositories/"+repoID+"/remote/src-abc", nil)
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("non-permissioned user GET source detail: expected 403, got %d: %s", resp.StatusCode, body)
	}

	resp, body = plain.do("GET", "/api/v1/repositories/"+repoID+"/remote/src-abc/decompositions/gpt-4o", nil)
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("non-permissioned user GET decomposition: expected 403, got %d: %s", resp.StatusCode, body)
	}
}

// TestRemote_DetailProxiesRegistryPayload verifies the
// happy path: a sysadmin calls the new detail endpoint
// and the registry's SourcePackage is returned verbatim
// (no enrichment, no shape change). The same for the
// per-model decomposition endpoint. Mirrors the contract
// the frontend dialog relies on.
func TestRemote_DetailProxiesRegistryPayload(t *testing.T) {
	wantSource := `{"source":{"id":"src-abc","url":"https://example.com/p","title":"A Paper","sha256":"abc","doi":"","s3_key":""},"decompositions":[{"model_id":"gpt-4o","fact_count":3,"has_embeddings":true,"presigned_url":"","s3_key":"k1"}]}`
	wantDecomp := `{"model_id":"gpt-4o","facts":[{"content":"hello","content_hash":"h1","confidence":0.9,"sentence_index":0}],"concepts":[{"canonical_name":"Foo","context":"Bar","aliases":[],"ontology_class":""}]}`

	stubRegistry := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/sources/src-abc"):
			_, _ = w.Write([]byte(wantSource))
		case strings.HasSuffix(r.URL.Path, "/decompositions/gpt-4o"):
			_, _ = w.Write([]byte(wantDecomp))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(stubRegistry.Close)

	env, _, _, _ := newRemoteEnvWithRegistry(t, stubRegistry.URL)

	admin := bootstrapSysAdmin(t, env, "remote-detail-happy@example.com")
	_, _, repoID := createRepository(t, admin, "RemoteDetailHappy", "remote-detail-happy", "desc")

	resp, body := admin.do("GET", "/api/v1/repositories/"+repoID+"/remote/src-abc", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get source detail (happy): expected 200, got %d: %s", resp.StatusCode, body)
	}
	// Round-trip the response into a generic map and assert the
	// shape — avoids coupling the test to the exact Go struct
	// definition in the registry package, so a future refactor
	// (renaming a field) doesn't have to chase this test.
	var got map[string]interface{}
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("decode source detail: %v", err)
	}
	src, ok := got["source"].(map[string]interface{})
	if !ok {
		t.Fatalf("source detail missing `source` object: %s", body)
	}
	if src["id"] != "src-abc" {
		t.Errorf("source.id = %v, want src-abc", src["id"])
	}
	if src["title"] != "A Paper" {
		t.Errorf("source.title = %v, want A Paper", src["title"])
	}
	decomps, ok := got["decompositions"].([]interface{})
	if !ok || len(decomps) != 1 {
		t.Fatalf("decompositions array missing or wrong size: %s", body)
	}
	d0 := decomps[0].(map[string]interface{})
	if d0["model_id"] != "gpt-4o" || d0["fact_count"].(float64) != 3 {
		t.Errorf("decomp[0] = %+v, want model_id=gpt-4o, fact_count=3", d0)
	}

	resp, body = admin.do("GET", "/api/v1/repositories/"+repoID+"/remote/src-abc/decompositions/gpt-4o", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get decomposition (happy): expected 200, got %d: %s", resp.StatusCode, body)
	}
	var decompGot map[string]interface{}
	if err := json.Unmarshal([]byte(body), &decompGot); err != nil {
		t.Fatalf("decode decomposition: %v", err)
	}
	if decompGot["model_id"] != "gpt-4o" {
		t.Errorf("decomp.model_id = %v, want gpt-4o", decompGot["model_id"])
	}
	facts, ok := decompGot["facts"].([]interface{})
	if !ok || len(facts) != 1 {
		t.Fatalf("decomp.facts missing or wrong size: %s", body)
	}
}

// parseTestDBConfigRemote is a copy of testutil's unexported
// parseTestDBConfig. We keep it local because exporting a
// helper just for this one test isn't worth the surface
// area; three other test files (admin_tasks, tasks, etc.) keep
// their config wiring local for the same reason. The URL
// shape is fixed by the docker-compose test service so the
// copy is safe.
func parseTestDBConfigRemote(t testing.TB, dbURL string) config.DatabaseConfig {
	t.Helper()
	u, err := url.Parse(dbURL)
	if err != nil {
		t.Fatalf("parsing test database URL: %v", err)
	}
	host := u.Hostname()
	port := 5432
	if p := u.Port(); p != "" {
		port, _ = strconv.Atoi(p)
	}
	user := ""
	if u.User != nil {
		user = u.User.Username()
	}
	password, _ := u.User.Password()
	name := strings.TrimPrefix(u.Path, "/")
	sslMode := u.Query().Get("sslmode")
	return config.DatabaseConfig{
		Host:     host,
		Port:     port,
		User:     user,
		Password: password,
		Name:     name,
		SSLMode:  sslMode,
		MaxConns: 5,
	}
}

// newRemoteEnvWithRegistry builds a test env identical to
// testutil.NewTestEnv except it wires a real *registry.Client
// pointed at the supplied URL. Returns the *testutil.TestEnv so
// the existing helper (bootstrapSysAdmin, createRepository,
// registerAndLogin) works unchanged. Returns the same extra
// handles as tasksEnvWithRBAC for symmetry, but the env alone
// is enough for these tests.
//
// The local helper lives here (not in testutil) because wiring
// a registry client isn't a general-purpose need; the only
// callers are the remote proxy tests. Mirrors the
// tasksEnvWithRBAC pattern in tasks_test.go.
func newRemoteEnvWithRegistry(t *testing.T, registryURL string) (*testutil.TestEnv, *rbac.Service, *pgxpool.Pool, *dbpool.Registry) {
	t.Helper()
	ctx := context.Background()

	dbURL := os.Getenv("OKT_TEST_DATABASE_URL")
	if dbURL == "" {
		dbURL = "postgres://okt:okt_test@localhost:5433/okt?sslmode=disable"
	}

	testutil.ResetTestDatabaseForTest(ctx, t, dbURL)

	cfg := &config.Config{
		Auth: config.AuthConfig{
			JWTSecret: "test-jwt-secret-key",
			TokenTTL:  24 * time.Hour,
		},
		Bootstrap: config.BootstrapConfig{DefaultRepository: false},
	}
	cfg.Databases = map[string]config.DatabaseConfig{
		"default": parseTestDBConfigRemote(t, dbURL),
	}
	cfg.System.Database = "default"
	cfg.Task.Database = "default"
	cfg.Isolation.DefaultDatabase = "default"

	dbReg, err := dbpool.New(ctx, cfg)
	if err != nil {
		t.Fatalf("opening test pool via registry: %v", err)
	}
	pool := dbReg.Default().Pool

	rbacSvc, err := rbac.SetupRBAC(pool)
	if err != nil {
		t.Fatalf("setting up RBAC: %v", err)
	}

	parsers := []content_parsing.Parser{
		content_parsing.NewTrafilaturaParser(),
		content_parsing.NewFitzPDFParser(),
	}
	resolutionProviders := []fetch.ResolutionProvider{
		fetch.NewFetchResolutionProviderWithParsers(parsers...),
	}
	fetchStrategy := fetch.NewFetchStrategy(resolutionProviders...)
	taskEnqueuer := &testutil.RecordingTaskEnqueuer{}

	queries := store.New(pool)
	storageBackend := testutil.NewTestStorageBackend(t)
	h := api.NewHandler(queries, cfg, rbacSvc, pool, dbReg, audit.NoopRecorder{})
	h.SetSource(handler.NewSource(nil, fetchStrategy, nil, nil, nil, storageBackend, testutil.TestParsers()))
	testutil.WireRepoSettings(h, nil, fetchStrategy)
	h.SetStorage(handler.NewStorage(storageBackend))
	h.SetTaskEnqueuer(taskEnqueuer)

	// Wire the registry client. The HTTP timeout is short to
	// keep a misconfigured test from hanging the suite. Auth
	// is "none" because the stub server has no auth. The test
	// builds a one-entry ClientMap (id "default") so the new
	// per-repo resolver can resolve it; the test repo's
	// registry_id column defaults to "default" (migration 0037).
	registryClients := registry.NewClientMap(config.ProvidersConfig{
		Registry: config.RegistryConfig{
			ID:            "default",
			URL:           registryURL,
			AuthMode:      "none",
			AllowedModels: []string{"*"},
		},
	})
	h.SetRemote(handler.NewRemote(registryClients, config.ProvidersConfig{
		Registry: config.RegistryConfig{
			ID:            "default",
			URL:           registryURL,
			AuthMode:      "none",
			AllowedModels: []string{"*"},
		},
	}))
	h.SetRegistryClients(registryClients)

	// OAuth + MCP wiring mirrors the production stack so
	// request shape matches.
	issuer := "http://localhost:8080"
	oauthCfg := oauth.Config{
		Issuer:          issuer,
		AccessTokenTTL:  15 * time.Minute,
		RefreshTokenTTL: 30 * 24 * time.Hour,
		AuthCodeTTL:     10 * time.Minute,
	}
	oauthServer := oauth.NewServer(oauthCfg, cfg.Auth.JWTSecret, queries, oauth.DefaultUserLookup(queries))
	h.SetOAuth(handler.NewOAuth(oauthServer, issuer, issuer+"/api/v1/mcp"))
	handler.SetLoginCookieSecret(cfg.Auth.JWTSecret)
	mcpHandler := handler.NewMCP(h.Deps(), handler.ResolveRepoPoolFromCaches(dbReg, h.RepoDBCache(), h.SlugCache()))
	mcpHandler.SetTaskEnqueuer(taskEnqueuer)
	h.SetMCP(mcpHandler)

	server := httptest.NewServer(h.Router())
	t.Cleanup(func() { server.Close() })
	t.Cleanup(func() { dbReg.Close() })

	return &testutil.TestEnv{
		Server:       server,
		BaseURL:      server.URL,
		DB:           pool,
		Config:       cfg,
		RBAC:         rbacSvc,
		TaskEnqueuer: taskEnqueuer,
		Storage:      storageBackend,
		MCP:          mcpHandler,
	}, rbacSvc, pool, dbReg
}

// recordingPullBatchEnqueuer is a stub RemotePullBatchEnqueuer that
// records the args it was called with. Used by the pull-batch e2e
// test to assert the HTTP layer hands the right IDs to the task
// manager without booting a real River client.
type recordingPullBatchEnqueuer struct {
	mu        sync.Mutex
	calls     []pullBatchCall
	nextJobID int
}

type pullBatchCall struct {
	RepositoryID    string
	RemoteSourceIDs []string
}

func (r *recordingPullBatchEnqueuer) EnqueuePullRemoteBatch(_ context.Context, repositoryID string, remoteSourceIDs []string) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.nextJobID++
	r.calls = append(r.calls, pullBatchCall{RepositoryID: repositoryID, RemoteSourceIDs: remoteSourceIDs})
	return fmt.Sprintf("test-batch-job-%d", r.nextJobID), nil
}

func (r *recordingPullBatchEnqueuer) Calls() []pullBatchCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]pullBatchCall, len(r.calls))
	copy(out, r.calls)
	return out
}

// newRemoteEnvWithRegistryAndBatchEnqueuer is a variant of
// newRemoteEnvWithRegistry that also wires a recording
// RemotePullBatchEnqueuer so the pull-batch endpoint can be
// exercised. Returns the env + the recording enqueuer so the test
// can assert on the calls.
func newRemoteEnvWithRegistryAndBatchEnqueuer(t *testing.T, registryURL string) (*testutil.TestEnv, *recordingPullBatchEnqueuer) {
	t.Helper()
	ctx := context.Background()

	dbURL := os.Getenv("OKT_TEST_DATABASE_URL")
	if dbURL == "" {
		dbURL = "postgres://okt:okt_test@localhost:5433/okt?sslmode=disable"
	}

	testutil.ResetTestDatabaseForTest(ctx, t, dbURL)

	cfg := &config.Config{
		Auth: config.AuthConfig{
			JWTSecret: "test-jwt-secret-key",
			TokenTTL:  24 * time.Hour,
		},
		Bootstrap: config.BootstrapConfig{DefaultRepository: false},
	}
	cfg.Databases = map[string]config.DatabaseConfig{
		"default": parseTestDBConfigRemote(t, dbURL),
	}
	cfg.System.Database = "default"
	cfg.Task.Database = "default"
	cfg.Isolation.DefaultDatabase = "default"

	dbReg, err := dbpool.New(ctx, cfg)
	if err != nil {
		t.Fatalf("opening test pool via registry: %v", err)
	}
	pool := dbReg.Default().Pool

	rbacSvc, err := rbac.SetupRBAC(pool)
	if err != nil {
		t.Fatalf("setting up RBAC: %v", err)
	}

	parsers := []content_parsing.Parser{
		content_parsing.NewTrafilaturaParser(),
		content_parsing.NewFitzPDFParser(),
	}
	resolutionProviders := []fetch.ResolutionProvider{
		fetch.NewFetchResolutionProviderWithParsers(parsers...),
	}
	fetchStrategy := fetch.NewFetchStrategy(resolutionProviders...)
	taskEnqueuer := &testutil.RecordingTaskEnqueuer{}

	queries := store.New(pool)
	storageBackend := testutil.NewTestStorageBackend(t)
	h := api.NewHandler(queries, cfg, rbacSvc, pool, dbReg, audit.NoopRecorder{})
	h.SetSource(handler.NewSource(nil, fetchStrategy, nil, nil, nil, storageBackend, testutil.TestParsers()))
	testutil.WireRepoSettings(h, nil, fetchStrategy)
	h.SetStorage(handler.NewStorage(storageBackend))
	h.SetTaskEnqueuer(taskEnqueuer)

	registryClients := registry.NewClientMap(config.ProvidersConfig{
		Registry: config.RegistryConfig{
			ID:            "default",
			URL:           registryURL,
			AuthMode:      "none",
			AllowedModels: []string{"*"},
		},
	})
	remote := handler.NewRemote(registryClients, config.ProvidersConfig{
		Registry: config.RegistryConfig{
			ID:            "default",
			URL:           registryURL,
			AuthMode:      "none",
			AllowedModels: []string{"*"},
		},
	})
	batchEnqueuer := &recordingPullBatchEnqueuer{}
	remote.SetPullBatchEnqueuer(batchEnqueuer)
	h.SetRemote(remote)
	h.SetRegistryClients(registryClients)

	issuer := "http://localhost:8080"
	oauthCfg := oauth.Config{
		Issuer:          issuer,
		AccessTokenTTL:  15 * time.Minute,
		RefreshTokenTTL: 30 * 24 * time.Hour,
		AuthCodeTTL:     10 * time.Minute,
	}
	oauthServer := oauth.NewServer(oauthCfg, cfg.Auth.JWTSecret, queries, oauth.DefaultUserLookup(queries))
	h.SetOAuth(handler.NewOAuth(oauthServer, issuer, issuer+"/api/v1/mcp"))
	handler.SetLoginCookieSecret(cfg.Auth.JWTSecret)
	mcpHandler := handler.NewMCP(h.Deps(), handler.ResolveRepoPoolFromCaches(dbReg, h.RepoDBCache(), h.SlugCache()))
	mcpHandler.SetTaskEnqueuer(taskEnqueuer)
	h.SetMCP(mcpHandler)

	server := httptest.NewServer(h.Router())
	t.Cleanup(func() { server.Close() })
	t.Cleanup(func() { dbReg.Close() })

	return &testutil.TestEnv{
		Server:       server,
		BaseURL:      server.URL,
		DB:           pool,
		Config:       cfg,
		RBAC:         rbacSvc,
		TaskEnqueuer: taskEnqueuer,
		Storage:      storageBackend,
		MCP:          mcpHandler,
	}, batchEnqueuer
}

// TestRemote_PullBatch enqueues a pull_remote_batch job with a list
// of remote source IDs. Verifies the 202 + job_id response and that
// the enqueuer received the IDs verbatim. Also covers the 400 paths
// (empty list, oversized list) and the permission gate.
func TestRemote_PullBatch(t *testing.T) {
	stubRegistry := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// The batch endpoint doesn't call the registry directly; it
		// just enqueues. The stub is here so resolveClient passes.
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(stubRegistry.Close)

	env, enqueuer := newRemoteEnvWithRegistryAndBatchEnqueuer(t, stubRegistry.URL)
	admin := bootstrapSysAdmin(t, env, "remote-pull-batch@example.com")
	_, _, repoID := createRepository(t, admin, "RemotePullBatch", "remote-pull-batch", "desc")

	// Happy path: 3 IDs → 202 + job_id.
	body, _ := json.Marshal(map[string][]string{"remote_source_ids": {"src-1", "src-2", "src-3"}})
	resp, raw := admin.do("POST", "/api/v1/repositories/"+repoID+"/remote/pull-batch", body)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("pull-batch: expected 202, got %d, body %s", resp.StatusCode, string(raw))
	}
	var res struct {
		JobID             string `json:"job_id"`
		RemoteSourceCount int    `json:"remote_source_count"`
		Status            string `json:"status"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		t.Fatalf("decode pull-batch response: %v", err)
	}
	if res.JobID == "" {
		t.Errorf("pull-batch: empty job_id")
	}
	if res.RemoteSourceCount != 3 {
		t.Errorf("pull-batch: remote_source_count = %d, want 3", res.RemoteSourceCount)
	}
	if res.Status != "queued" {
		t.Errorf("pull-batch: status = %q, want queued", res.Status)
	}
	calls := enqueuer.Calls()
	if len(calls) != 1 {
		t.Fatalf("enqueuer calls = %d, want 1", len(calls))
	}
	if len(calls[0].RemoteSourceIDs) != 3 {
		t.Errorf("enqueuer received %d IDs, want 3", len(calls[0].RemoteSourceIDs))
	}

	// 400 on empty list.
	emptyBody, _ := json.Marshal(map[string][]string{"remote_source_ids": {}})
	resp, _ = admin.do("POST", "/api/v1/repositories/"+repoID+"/remote/pull-batch", emptyBody)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("pull-batch empty list: expected 400, got %d", resp.StatusCode)
	}

	// 400 on oversized list (> 500).
	var bigIDs []string
	for i := 0; i < 501; i++ {
		bigIDs = append(bigIDs, fmt.Sprintf("src-%d", i))
	}
	bigBody, _ := json.Marshal(map[string][]string{"remote_source_ids": bigIDs})
	resp, _ = admin.do("POST", "/api/v1/repositories/"+repoID+"/remote/pull-batch", bigBody)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("pull-batch oversized list: expected 400, got %d", resp.StatusCode)
	}

	// 403 for non-repo-admin.
	other := registerAndLogin(t, env, "remote-pull-batch-other@example.com")
	resp, _ = other.do("POST", "/api/v1/repositories/"+repoID+"/remote/pull-batch", body)
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("non-admin pull-batch: expected 403, got %d", resp.StatusCode)
	}
}

// recordingDedupEnqueuer is a stub handler.RemoteDedupEnqueuer that
// records each EnqueueEmbedFacts call. Used by the sync pull test to
// assert that pulling facts from the registry kicks off the
// embed→dedup pipeline (the "tasks not starting on pull" symptom).
type recordingDedupEnqueuer struct {
	mu    sync.Mutex
	calls []dedupCall
}

type dedupCall struct {
	RepositoryID string
	SourceID     string
}

func (r *recordingDedupEnqueuer) EnqueueEmbedFacts(_ context.Context, repositoryID, sourceID string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, dedupCall{RepositoryID: repositoryID, SourceID: sourceID})
	return nil
}

func (r *recordingDedupEnqueuer) Calls() []dedupCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]dedupCall, len(r.calls))
	copy(out, r.calls)
	return out
}

// newRemoteEnvWithRegistryAndDedup builds a test env identical to
// newRemoteEnvWithRegistryAndBatchEnqueuer, plus it wires the
// registry ServiceMap + a recording dedup enqueuer onto the remote
// handler so the sync PullSource path takes the filter-aware pull
// (the same path the batch worker takes) and the test can assert an
// embed_facts job was enqueued. Returns the env + the recording
// enqueuer so the test can assert on the calls.
func newRemoteEnvWithRegistryAndDedup(t *testing.T, registryURL string, allowedModels []string, defaultFactModel string) (*testutil.TestEnv, *recordingDedupEnqueuer) {
	t.Helper()
	ctx := context.Background()

	dbURL := os.Getenv("OKT_TEST_DATABASE_URL")
	if dbURL == "" {
		dbURL = "postgres://okt:okt_test@localhost:5433/okt?sslmode=disable"
	}

	testutil.ResetTestDatabaseForTest(ctx, t, dbURL)

	cfg := &config.Config{
		Auth: config.AuthConfig{
			JWTSecret: "test-jwt-secret-key",
			TokenTTL:  24 * time.Hour,
		},
		Bootstrap: config.BootstrapConfig{DefaultRepository: false},
	}
	cfg.Databases = map[string]config.DatabaseConfig{
		"default": parseTestDBConfigRemote(t, dbURL),
	}
	cfg.System.Database = "default"
	cfg.Task.Database = "default"
	cfg.Isolation.DefaultDatabase = "default"

	dbReg, err := dbpool.New(ctx, cfg)
	if err != nil {
		t.Fatalf("opening test pool via registry: %v", err)
	}
	pool := dbReg.Default().Pool

	rbacSvc, err := rbac.SetupRBAC(pool)
	if err != nil {
		t.Fatalf("setting up RBAC: %v", err)
	}

	parsers := []content_parsing.Parser{
		content_parsing.NewTrafilaturaParser(),
		content_parsing.NewFitzPDFParser(),
	}
	resolutionProviders := []fetch.ResolutionProvider{
		fetch.NewFetchResolutionProviderWithParsers(parsers...),
	}
	fetchStrategy := fetch.NewFetchStrategy(resolutionProviders...)
	taskEnqueuer := &testutil.RecordingTaskEnqueuer{}

	queries := store.New(pool)
	storageBackend := testutil.NewTestStorageBackend(t)
	h := api.NewHandler(queries, cfg, rbacSvc, pool, dbReg, audit.NoopRecorder{})
	h.SetSource(handler.NewSource(nil, fetchStrategy, nil, nil, nil, storageBackend, testutil.TestParsers()))
	testutil.WireRepoSettings(h, nil, fetchStrategy)
	h.SetStorage(handler.NewStorage(storageBackend))
	h.SetTaskEnqueuer(taskEnqueuer)

	providersCfg := config.ProvidersConfig{
		Registry: config.RegistryConfig{
			ID:            "default",
			URL:           registryURL,
			AuthMode:      "none",
			AllowedModels: allowedModels,
		},
	}
	providersCfg.Decomposition.FactExtraction.Model = defaultFactModel
	registryClients := registry.NewClientMap(providersCfg)
	remote := handler.NewRemote(registryClients, providersCfg)
	dedupEnqueuer := &recordingDedupEnqueuer{}
	remote.SetDedupEnqueuer(dedupEnqueuer)
	h.SetRemote(remote)
	h.SetRegistryClients(registryClients)
	// Wire the ServiceMap so the sync PullSource takes the
	// filter-aware path (the same path the batch worker takes). The
	// accepted-hashes resolver and inbound mapper factory are left
	// nil so the pull admits the default promptset hash and imports
	// contexts verbatim (the legacy behavior) — this isolates the
	// test to the allowed_models fix.
	registryServices := registry.NewServiceMap(registryClients)
	h.SetRemoteRegistryServices(registryServices, nil, nil)

	issuer := "http://localhost:8080"
	oauthCfg := oauth.Config{
		Issuer:          issuer,
		AccessTokenTTL:  15 * time.Minute,
		RefreshTokenTTL: 30 * 24 * time.Hour,
		AuthCodeTTL:     10 * time.Minute,
	}
	oauthServer := oauth.NewServer(oauthCfg, cfg.Auth.JWTSecret, queries, oauth.DefaultUserLookup(queries))
	h.SetOAuth(handler.NewOAuth(oauthServer, issuer, issuer+"/api/v1/mcp"))
	handler.SetLoginCookieSecret(cfg.Auth.JWTSecret)
	mcpHandler := handler.NewMCP(h.Deps(), handler.ResolveRepoPoolFromCaches(dbReg, h.RepoDBCache(), h.SlugCache()))
	mcpHandler.SetTaskEnqueuer(taskEnqueuer)
	h.SetMCP(mcpHandler)

	server := httptest.NewServer(h.Router())
	t.Cleanup(func() { server.Close() })
	t.Cleanup(func() { dbReg.Close() })

	return &testutil.TestEnv{
		Server:       server,
		BaseURL:      server.URL,
		DB:           pool,
		Config:       cfg,
		RBAC:         rbacSvc,
		TaskEnqueuer: taskEnqueuer,
		Storage:      storageBackend,
		MCP:          mcpHandler,
	}, dedupEnqueuer
}

// TestRemote_PullSource_ImportsFacts is the regression test for the
// "Pulled ... 0 facts, 0 concepts" bug. The sync POST /remote/{id}/pull
// handler used to build RemotePullDeps with Service=nil + Filter=nil,
// forcing the legacy Client.IsAllowedModel branch. That branch
// rejects every model when the registry's allowed_models list is
// empty (the shipped config default before this fix), so the pull
// imported 0 facts and — because the embed_facts enqueue is gated on
// importedFacts > 0 — no tasks started. The UI detail dialog showed
// the decompositions because it proxies the registry directly,
// bypassing the allowed_models filter.
//
// This test stubs the registry with a google/gemma-4-31b-it
// decomposition (the default extraction model) containing 1 fact +
// 1 concept, wires the ServiceMap so the sync pull takes the
// filter-aware path, and asserts:
//   - the pull returns imported_facts > 0 and imported_concepts > 0,
//   - an embed_facts job was enqueued (tasks start on pull).
//
// The fix has two parts: (1) config.default.yaml allowed_models
// changed from [] to ["*"] so the default config admits all models,
// and (2) the sync PullSource handler now builds a RelevanceFilter
// and threads a *registry.Service into RemotePullDeps so it uses
// resolveAllowedModels (per-repo override replaces global) instead
// of Client.IsAllowedModel (global only).
func TestRemote_PullSource_ImportsFacts(t *testing.T) {
	const (
		sourceID    = "src-cbc"
		modelID     = "google/gemma-4-31b-it"
		factContent = "Victims of CIA-linked Montreal brainwashing experiments cleared to sue in class action."
		factHash    = "hash-fact-1"
		conceptName = "MKUltra"
		conceptCtx  = "Law"
	)
	sourcePkg := fmt.Sprintf(
		`{"source":{"id":%q,"url":"https://example.com/cbc","title":"CBC Brainwashing","sha256":"","doi":"","s3_key":""},"content":{"text":"body","markdown":"body"},"decompositions":[{"model_id":%q,"fact_count":1,"has_embeddings":false}]}`,
		sourceID, modelID,
	)
	decompPkg := fmt.Sprintf(
		`{"model_id":%q,"facts":[{"content":%q,"content_hash":%q,"confidence":0.9,"sentence_index":0}],"concepts":[{"canonical_name":%q,"context":%q}],"links":[{"fact_content_hash":%q,"concept_name":%q,"concept_context":%q}]}`,
		modelID, factContent, factHash, conceptName, conceptCtx, factHash, conceptName, conceptCtx,
	)

	stubRegistry := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/sources/"+sourceID):
			_, _ = w.Write([]byte(sourcePkg))
		case strings.HasSuffix(r.URL.Path, "/decompositions/"+modelID):
			_, _ = w.Write([]byte(decompPkg))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(stubRegistry.Close)

	// allowed_models=["*"] mirrors the fixed config.default.yaml
	// default: admit decompositions from any extraction model.
	env, dedup := newRemoteEnvWithRegistryAndDedup(t, stubRegistry.URL, []string{"*"}, "")

	admin := bootstrapSysAdmin(t, env, "remote-pull-imports@example.com")
	_, _, repoID := createRepository(t, admin, "RemotePullImports", "remote-pull-imports", "desc")

	resp, raw := admin.do("POST", "/api/v1/repositories/"+repoID+"/remote/"+sourceID+"/pull", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("pull source: expected 200, got %d, body %s", resp.StatusCode, string(raw))
	}
	var res struct {
		ImportedFacts    int `json:"imported_facts"`
		ImportedConcepts int `json:"imported_concepts"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		t.Fatalf("decode pull response: %v", err)
	}
	if res.ImportedFacts == 0 {
		t.Errorf("imported_facts = 0, want > 0 (the allowed_models filter stripped the decomposition)")
	}
	if res.ImportedConcepts == 0 {
		t.Errorf("imported_concepts = 0, want > 0")
	}
	// The embed_facts enqueue is gated on importedFacts > 0; before
	// the fix the guard short-circuited and no tasks started.
	calls := dedup.Calls()
	if len(calls) != 1 {
		t.Errorf("embed_facts enqueue calls = %d, want 1 (tasks must start on pull)", len(calls))
	}
}

// TestRemote_PullSource_DeniesDisallowedModel is the deny-path
// counterpart: when allowed_models excludes the decomposition's
// model, the filter strips it and the pull imports 0 facts (and
// enqueues no embed_facts job). Locks the filter's deny behavior so
// a future change can't silently broaden it.
func TestRemote_PullSource_DeniesDisallowedModel(t *testing.T) {
	const (
		sourceID = "src-deny"
		modelID  = "google/gemma-4-31b-it"
	)
	sourcePkg := fmt.Sprintf(
		`{"source":{"id":%q,"url":"https://example.com/deny","title":"Deny","sha256":"","doi":"","s3_key":""},"decompositions":[{"model_id":%q,"fact_count":1,"has_embeddings":false}]}`,
		sourceID, modelID,
	)
	decompPkg := fmt.Sprintf(
		`{"model_id":%q,"facts":[{"content":"x","content_hash":"h","confidence":0.9,"sentence_index":0}],"concepts":[]}`,
		modelID,
	)

	stubRegistry := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/sources/"+sourceID):
			_, _ = w.Write([]byte(sourcePkg))
		case strings.HasSuffix(r.URL.Path, "/decompositions/"+modelID):
			_, _ = w.Write([]byte(decompPkg))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(stubRegistry.Close)

	// allowed_models=["some-other-model"] excludes the
	// decomposition's model, so the filter must strip it.
	env, dedup := newRemoteEnvWithRegistryAndDedup(t, stubRegistry.URL, []string{"some-other-model"}, "")

	admin := bootstrapSysAdmin(t, env, "remote-pull-deny@example.com")
	_, _, repoID := createRepository(t, admin, "RemotePullDeny", "remote-pull-deny", "desc")

	resp, raw := admin.do("POST", "/api/v1/repositories/"+repoID+"/remote/"+sourceID+"/pull", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("pull source: expected 200, got %d, body %s", resp.StatusCode, string(raw))
	}
	var res struct {
		ImportedFacts int `json:"imported_facts"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		t.Fatalf("decode pull response: %v", err)
	}
	if res.ImportedFacts != 0 {
		t.Errorf("imported_facts = %d, want 0 (model not in allowed_models)", res.ImportedFacts)
	}
	if calls := dedup.Calls(); len(calls) != 0 {
		t.Errorf("embed_facts enqueue calls = %d, want 0 (no facts imported → no tasks)", len(calls))
	}
}

// TestRemote_PullSource_PerRepoOverrideHonored locks the
// handler-side fix: the sync PullSource handler now consults the
// per-repo allowed_models override (via resolveAllowedModels)
// instead of the legacy Client.IsAllowedModel (global config only).
// Before the fix, a repo that enabled a model in Settings still
// imported 0 facts on the sync pull because the legacy path used
// the global allowed_models config, which the operator had left at
// the shipped default [].
//
// Setup: global allowed_models=["some-other-model"] (restrictive),
// per-repo allowed_models=["google/gemma-4-31b-it"] (admits the
// decomposition's model). The legacy path would deny (global only);
// the filter-aware path admits (per-repo replaces global).
func TestRemote_PullSource_PerRepoOverrideHonored(t *testing.T) {
	const (
		sourceID    = "src-override"
		modelID     = "google/gemma-4-31b-it"
		factContent = "A fact admitted by the per-repo override."
		factHash    = "hash-override-1"
	)
	sourcePkg := fmt.Sprintf(
		`{"source":{"id":%q,"url":"https://example.com/override","title":"Override","sha256":"","doi":"","s3_key":""},"decompositions":[{"model_id":%q,"fact_count":1,"has_embeddings":false}]}`,
		sourceID, modelID,
	)
	decompPkg := fmt.Sprintf(
		`{"model_id":%q,"facts":[{"content":%q,"content_hash":%q,"confidence":0.9,"sentence_index":0}],"concepts":[]}`,
		modelID, factContent, factHash,
	)

	stubRegistry := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/sources/"+sourceID):
			_, _ = w.Write([]byte(sourcePkg))
		case strings.HasSuffix(r.URL.Path, "/decompositions/"+modelID):
			_, _ = w.Write([]byte(decompPkg))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(stubRegistry.Close)

	// Global allowed_models is restrictive; the per-repo override
	// (set below via PUT /settings/registry) admits the model.
	env, dedup := newRemoteEnvWithRegistryAndDedup(t, stubRegistry.URL, []string{"some-other-model"}, "")

	// Populate cfg.Providers.Registry so the settings handler's
	// AnyRegistryConfigured / RegistryByID validation passes (the
	// env builder wires registryClients + the remote handler but
	// doesn't populate the config the settings handler reads).
	env.Config.Providers.Registry = config.RegistryConfig{
		ID:            "default",
		URL:           stubRegistry.URL,
		AuthMode:      "none",
		AllowedModels: []string{"some-other-model"},
	}

	admin := bootstrapSysAdmin(t, env, "remote-pull-override@example.com")
	_, _, repoID := createRepository(t, admin, "RemotePullOverride", "remote-pull-override", "desc")

	// Enable the registry integration for the repo so resolveClient
	// passes (the pull endpoint returns 503 when the integration is
	// off) and set the per-repo allowed_models override to admit the
	// decomposition's model. The default for a fresh repo is
	// enabled=true (migration 0035), but we set it explicitly so the
	// test is robust to default changes.
	regBody, _ := json.Marshal(map[string]any{
		"enabled":        true,
		"allowed_models": []string{modelID},
	})
	resp, raw := admin.do("PUT", "/api/v1/repositories/"+repoID+"/settings/registry", regBody)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("set registry settings: status %d, body %s", resp.StatusCode, string(raw))
	}

	resp, raw = admin.do("POST", "/api/v1/repositories/"+repoID+"/remote/"+sourceID+"/pull", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("pull source: expected 200, got %d, body %s", resp.StatusCode, string(raw))
	}
	var res struct {
		ImportedFacts int `json:"imported_facts"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		t.Fatalf("decode pull response: %v", err)
	}
	if res.ImportedFacts == 0 {
		t.Errorf("imported_facts = 0, want > 0 (per-repo allowed_models override must be honored on the sync pull path)")
	}
	if calls := dedup.Calls(); len(calls) != 1 {
		t.Errorf("embed_facts enqueue calls = %d, want 1 (tasks must start on pull)", len(calls))
	}
}

// TestRemote_PullSource_BareNameMatch verifies the bare-model-name
// matching in IsAllowed: the registry stores a decomposition under
// the full prefixed id "google/gemma-4-31b-it" (the old shape, before
// contribute_source started stripping the provider prefix), and the
// per-repo whitelist is ["gemma-4-31b-it"] (the bare name). The pull
// must import facts because IsAllowed normalizes both sides via
// BareModelID before comparing. This locks backward compatibility:
// old registry decompositions with the provider prefix still match
// a bare-name whitelist.
func TestRemote_PullSource_BareNameMatch(t *testing.T) {
	const (
		sourceID    = "src-barename"
		modelID     = "google/gemma-4-31b-it" // old prefixed shape (registry stores this)
		factContent = "A fact stored under a prefixed model id."
		factHash    = "hash-barename-1"
	)
	sourcePkg := fmt.Sprintf(
		`{"source":{"id":%q,"url":"https://example.com/barename","title":"BareName","sha256":"","doi":"","s3_key":""},"decompositions":[{"model_id":%q,"fact_count":1,"has_embeddings":false}]}`,
		sourceID, modelID,
	)
	decompPkg := fmt.Sprintf(
		`{"model_id":%q,"facts":[{"content":%q,"content_hash":%q,"confidence":0.9,"sentence_index":0}],"concepts":[]}`,
		modelID, factContent, factHash,
	)

	stubRegistry := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/sources/"+sourceID):
			_, _ = w.Write([]byte(sourcePkg))
		case strings.HasSuffix(r.URL.Path, "/decompositions/"+modelID):
			_, _ = w.Write([]byte(decompPkg))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(stubRegistry.Close)

	// Global allowed_models is the bare name (what the picker now
	// offers). IsAllowed strips the prefix from the registry's
	// "google/gemma-4-31b-it" and compares to "gemma-4-31b-it" → match.
	env, dedup := newRemoteEnvWithRegistryAndDedup(t, stubRegistry.URL, []string{"gemma-4-31b-it"}, "")

	admin := bootstrapSysAdmin(t, env, "remote-pull-barename@example.com")
	_, _, repoID := createRepository(t, admin, "RemotePullBareName", "remote-pull-barename", "desc")

	resp, raw := admin.do("POST", "/api/v1/repositories/"+repoID+"/remote/"+sourceID+"/pull", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("pull source: expected 200, got %d, body %s", resp.StatusCode, string(raw))
	}
	var res struct {
		ImportedFacts int `json:"imported_facts"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		t.Fatalf("decode pull response: %v", err)
	}
	if res.ImportedFacts == 0 {
		t.Errorf("imported_facts = 0, want > 0 (bare-name whitelist [gemma-4-31b-it] must match registry decomposition google/gemma-4-31b-it via BareModelID normalization)")
	}
	if calls := dedup.Calls(); len(calls) != 1 {
		t.Errorf("embed_facts enqueue calls = %d, want 1", len(calls))
	}
}

// TestRemote_PullSource_AutoWhitelistFactModel verifies the
// auto-whitelist: the repo's allowed_models is ["some-image-model"]
// (doesn't match the registry's fact-extraction model), but the
// repo's fact_extraction model setting resolves to "gemma-4-31b-it".
// The pull path auto-adds the repo's own fact-extraction model to
// the whitelist, so the registry's decomposition (stored under
// "gemma-4-31b-it" or "google/gemma-4-31b-it") is admitted. This is
// the "if in use, autowhitelisted" behavior: a repo can always pull
// decompositions produced by its own extraction model.
func TestRemote_PullSource_AutoWhitelistFactModel(t *testing.T) {
	const (
		sourceID    = "src-autowl"
		modelID     = "gemma-4-31b-it" // new bare shape (contribute_source strips prefix)
		factContent = "A fact the repo's own extraction model produced."
		factHash    = "hash-autowl-1"
	)
	sourcePkg := fmt.Sprintf(
		`{"source":{"id":%q,"url":"https://example.com/autowl","title":"AutoWL","sha256":"","doi":"","s3_key":""},"decompositions":[{"model_id":%q,"fact_count":1,"has_embeddings":false}]}`,
		sourceID, modelID,
	)
	decompPkg := fmt.Sprintf(
		`{"model_id":%q,"facts":[{"content":%q,"content_hash":%q,"confidence":0.9,"sentence_index":0}],"concepts":[]}`,
		modelID, factContent, factHash,
	)

	stubRegistry := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/sources/"+sourceID):
			_, _ = w.Write([]byte(sourcePkg))
		case strings.HasSuffix(r.URL.Path, "/decompositions/"+modelID):
			_, _ = w.Write([]byte(decompPkg))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(stubRegistry.Close)

	// Global allowed_models is an image model that does NOT match the
	// registry's fact-extraction model. The auto-whitelist must add
	// the repo's fact-extraction model (resolved from the per-task
	// setting or the default) so the pull still imports.
	env, dedup := newRemoteEnvWithRegistryAndDedup(t, stubRegistry.URL, []string{"some-image-model"}, modelID)
	// Set the registry config so SetRegistrySettings validation
	// passes (the test sets the per-repo allowed_models below).
	env.Config.Providers.Registry = config.RegistryConfig{
		ID:            "default",
		URL:           stubRegistry.URL,
		AuthMode:      "none",
		AllowedModels: []string{"some-image-model"},
	}
	admin := bootstrapSysAdmin(t, env, "remote-pull-autowl@example.com")
	_, _, repoID := createRepository(t, admin, "RemotePullAutoWL", "remote-pull-autowl", "desc")

	// Set the per-repo allowed_models to the image model (the foot-gun
	// case). The auto-whitelist must still admit the fact-extraction
	// model's decomposition.
	regBody, _ := json.Marshal(map[string]any{
		"enabled":        true,
		"allowed_models": []string{"some-image-model"},
	})
	resp, raw := admin.do("PUT", "/api/v1/repositories/"+repoID+"/settings/registry", regBody)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("set registry settings: status %d, body %s", resp.StatusCode, string(raw))
	}

	resp, raw = admin.do("POST", "/api/v1/repositories/"+repoID+"/remote/"+sourceID+"/pull", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("pull source: expected 200, got %d, body %s", resp.StatusCode, string(raw))
	}
	var res struct {
		ImportedFacts int `json:"imported_facts"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		t.Fatalf("decode pull response: %v", err)
	}
	if res.ImportedFacts == 0 {
		t.Errorf("imported_facts = 0, want > 0 (repo's own fact-extraction model must be auto-whitelisted even when allowed_models lists a non-matching image model)")
	}
	if calls := dedup.Calls(); len(calls) != 1 {
		t.Errorf("embed_facts enqueue calls = %d, want 1", len(calls))
	}
}
