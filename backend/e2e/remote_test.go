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
	h := api.NewHandler(queries, cfg, rbacSvc, pool, dbReg)
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
	mu       sync.Mutex
	calls    []pullBatchCall
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
	h := api.NewHandler(queries, cfg, rbacSvc, pool, dbReg)
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
