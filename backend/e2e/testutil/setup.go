package testutil

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	neturl "net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/openktree/open-knowledge-tree/backend/internal/api"
	"github.com/openktree/open-knowledge-tree/backend/internal/api/handler"
	"github.com/openktree/open-knowledge-tree/backend/internal/audit"
	"github.com/openktree/open-knowledge-tree/backend/internal/config"
	"github.com/openktree/open-knowledge-tree/backend/internal/dbpool"
	"github.com/openktree/open-knowledge-tree/backend/internal/oauth"
	"github.com/openktree/open-knowledge-tree/backend/internal/promptset"
	"github.com/openktree/open-knowledge-tree/backend/internal/providers/content_parsing"
	"github.com/openktree/open-knowledge-tree/backend/internal/providers/decomposition"
	"github.com/openktree/open-knowledge-tree/backend/internal/providers/fetch"
	"github.com/openktree/open-knowledge-tree/backend/internal/providers/ontology"
	"github.com/openktree/open-knowledge-tree/backend/internal/providers/search"
	"github.com/openktree/open-knowledge-tree/backend/internal/providers/storage"
	"github.com/openktree/open-knowledge-tree/backend/internal/rbac"
	"github.com/openktree/open-knowledge-tree/backend/internal/store"

	"github.com/jackc/pgx/v5/pgxpool"
)

// WireRepoSettings wires the per-repository settings dependencies
// (ProviderRegistry + OntologySource) onto a Handler so
// CreateRepository seeds provider + context settings for every
// test repo. The registry is built from the same provider maps
// passed to NewSource; the ontology source is the embedded DBpedia
// L3 snapshot (the production wiring). Without this, test repos
// would have no settings and extract_concepts would hard-fail on
// the empty context list. Called once per handler construction.
// Exported so test files that build a custom *api.Handler outside
// of the shared env builders (e.g. remote_test.go's
// registry-wired env) can reuse the wiring.
func WireRepoSettings(h *api.Handler, searchProviders map[string]search.SearchProvider, fetchStrategy *fetch.FetchStrategy) {
	reg := handler.NewProviderRegistry(searchProviders, fetchStrategy)
	h.SetProviderRegistry(reg)
	onto, err := ontology.NewEmbeddedL3Source()
	if err != nil {
		// The embedded file is committed alongside the binary; a
		// parse failure is a build-time issue, not a test issue.
		panic(fmt.Sprintf("testutil: embedded L3 source: %v", err))
	}
	h.SetOntologySource(onto)
	// Wire the promptset resolver (built-in + DB over the system
	// pool) so the promptsets CRUD endpoints and the per-repo
	// promptset settings endpoints work in e2e tests. The resolver
	// is the same one production wiring builds.
	h.SetPromptsetResolver(promptset.NewResolver(promptset.NewDBProvider(h.Deps().Store)))
}

type TestEnv struct {
	Server  *httptest.Server
	BaseURL string
	DB      *pgxpool.Pool
	Config  *config.Config
	RBAC    *rbac.Service
	// TaskEnqueuer is the no-op task enqueuer wired into the source
	// handler. Tests that need to assert that a background job was
	// enqueued can read Enqueued from it.
	TaskEnqueuer *RecordingTaskEnqueuer
	// Storage is the file-storage backend the server's serving
	// endpoints are wired with. Tests that want to seed a stored
	// file (so the serving endpoint finds it) must use this same
	// instance — a separately-constructed backend would write to
	// a different temp directory and the server would 404.
	Storage storage.FileStorage
	// AI is the AI-provider handler bundle wired into the test
	// server. nil in the default env (no AI providers registered);
	// tests that need /ai/providers or /ai/embedding/providers use
	// NewTestEnvWithAI to supply a custom bundle.
	AI *handler.AI
	// MCP is the MCP handler bundle wired into the test server.
	// Exposed so tests can attach a stub TaskClient (via
	// SetTaskClient) to exercise the getSourceTasks/getReportTasks
	// verbose path without booting a real River client. The
	// summary (verbose=false) path uses the real task pool wired
	// via SetTaskPool in wireOAuthAndMCP, so summary-mode tests
	// insert rows directly into river_job on env.DB. nil until
	// wireOAuthAndMCP runs.
	MCP *handler.MCP
}

// RecordingTaskEnqueuer is a stand-in for the real task manager
// that just records the calls. It exists so e2e tests can verify
// that the HTTP layer correctly hands work to the task queue
// without having to spin up a real River client against the
// test database.
type RecordingTaskEnqueuer struct {
	mu              sync.Mutex
	Enqueued        []handler.RetrieveSourceArgs
	Decompositions  []handler.SourceDecompositionArgs
	ReportAnnotates []handler.AnnotateReportArgs
	ExtractConcepts []handler.ExtractConceptsArgs
	RecomputeGroups []handler.RecomputeConceptGroupsArgs
	Synthesizes     []handler.SynthesizeArgs
	nextID          int
}

// NewForTestPool wraps a pre-opened *pgxpool.Pool in a
// *dbpool.Registry and returns it. It mirrors the production
// dbpool.New behavior at the Registry level (Default / Get
// resolve to the same pool) but skips the open + ping + DDL
// pipeline so callers can manage the underlying pool themselves.
// Used by tests that need to call into packages (like
// internal/bootstrap) whose signatures now take a Registry.
func NewForTestPool(pool *pgxpool.Pool) *dbpool.Registry {
	return dbpool.NewForTest(pool)
}

// NewTestStorageBackend builds a LocalFileStorage rooted at a
// per-test temp directory. Returns nil when the backend can't be
// constructed (a fatal test failure) so callers can pass the result
// directly to handler.NewSource / NewStorage without a separate nil
// check. The temp dir is cleaned up by the testing runtime when the
// test completes, so tests don't need an explicit teardown.
func NewTestStorageBackend(t testing.TB) storage.FileStorage {
	t.Helper()
	root := t.TempDir()
	fs, err := storage.NewLocalFileStorage(root)
	if err != nil {
		t.Fatalf("building test storage backend: %v", err)
	}
	return fs
}

// guardDevDatabase refuses to run the e2e reset against the
// developer's primary Postgres. The e2e harness drops every
// application schema before re-applying migrations (see
// resetTestDatabase), which deletes all users, sessions, facts,
// sources, and other application data. Running that against the
// dev DB (port 5432 with the okt_dev password, as the docker-compose
// `postgres` service uses) silently destroys dev data, so a
// misconfigured OKT_TEST_DATABASE_URL fails loudly here instead.
//
// The check is intentionally narrow: it matches the dev credentials
// baked into backend/docker-compose.yml (port 5432 + password
// okt_dev). The dedicated test container (port 5433 + password
// okt_test) and any custom setup are unaffected.
func guardDevDatabase(t testing.TB, dbURL string) {
	t.Helper()
	u, err := neturl.Parse(dbURL)
	if err != nil {
		t.Fatalf("parsing test database URL for dev-DB guard: %v", err)
		return
	}
	port := u.Port()
	if port == "" {
		port = "5432"
	}
	password, _ := u.User.Password()
	if port == "5432" && password == "okt_dev" {
		t.Fatalf(
			"refusing to reset dev database %s: the e2e harness drops all schemas before re-running migrations, which would delete all dev users/facts/sources. "+
				"Point OKT_TEST_DATABASE_URL at the dedicated test Postgres (port 5433, password okt_test) or run `just test-e2e`.",
			dbURL,
		)
	}
}

// ResetTestDatabaseForTest is the exported form of
// resetTestDatabase for e2e test files outside the testutil
// package (e.g. backend/e2e/tasks_test.go) that need to reset
// the test database with the same dev-DB guard. See
// resetTestDatabase for the behavioral contract.
func ResetTestDatabaseForTest(ctx context.Context, t testing.TB, dbURL string) {
	resetTestDatabase(ctx, t, dbURL)
}

// resetTestDatabase drops both application schemas and recreates
// the public schema so each e2e test starts on a clean slate. The
// migrations re-run when dbpool.New opens the registry afterwards.
// This is destructive: it deletes every user, session, fact,
// source, and other row in the database it targets, so callers
// must first pass dbURL through guardDevDatabase to avoid wiping
// a developer's primary database.
func resetTestDatabase(ctx context.Context, t testing.TB, dbURL string) {
	t.Helper()
	guardDevDatabase(t, dbURL)
	resetPool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		t.Fatalf("connecting to test database %s for reset: %v", dbURL, err)
	}
	if _, err := resetPool.Exec(ctx, `DROP SCHEMA IF EXISTS okt_repository CASCADE; DROP SCHEMA IF EXISTS okt_system CASCADE; DROP SCHEMA public CASCADE; CREATE SCHEMA public;`); err != nil {
		resetPool.Close()
		t.Fatalf("resetting test database %s: %v", dbURL, err)
	}
	resetPool.Close()
}

// NewMultiDBTestEnv builds a test env that opens TWO
// pools against the same Postgres instance: a "default"
// pool and a "tasks" pool, each with its own database
// name. The Postgres init script mounted at
// docker-entrypoint-initdb.d/01-create-tasks-db.sh creates
// the second database the first time the test container
// boots, so by the time a test runs both `okt` and
// `okt_tasks` exist on the test server.
//
// The point of this env is to verify the production
// "dedicated task database" wiring: when `task.database`
// is "tasks", the River client (or anything we wire to
// it) must talk to the okt_tasks database, while
// application queries stay on the default pool. The
// returned TestEnv exposes the two pools separately so a
// test can assert on row counts in each database
// independently.
//
// Like NewTestEnv, this resets both databases at
// startup so the test starts clean. Both resets go
// through DROP SCHEMA CASCADE to keep the round-trip
// fast (the migrations re-run on pool open).
func NewMultiDBTestEnv(t testing.TB) *MultiDBTestEnv {
	t.Helper()
	ctx := context.Background()

	dbURL := os.Getenv("OKT_TEST_DATABASE_URL")
	if dbURL == "" {
		dbURL = "postgres://okt:okt_test@localhost:5433/okt?sslmode=disable"
	}
	tasksURL := "postgres://okt:okt_test@localhost:5433/okt_tasks?sslmode=disable"

	// Reset both databases. We open a one-off pool for the
	// reset so the registry can later open them with the
	// search_path hook applied.
	for _, url := range []string{dbURL, tasksURL} {
		resetTestDatabase(ctx, t, url)
	}

	cfg := &config.Config{
		Auth: config.AuthConfig{
			JWTSecret: "test-jwt-secret-key",
			TokenTTL:  24 * time.Hour,
		},
		Bootstrap: config.BootstrapConfig{DefaultRepository: false},
	}
	cfg.Databases = map[string]config.DatabaseConfig{
		"default": parseTestDBConfig(t, dbURL),
		"tasks":   parseTestDBConfig(t, tasksURL),
	}
	cfg.System.Database = "default"
	// Pointing the task manager at the dedicated pool is
	// the whole point of this env. The two pools use the
	// same Postgres but different databases, so River
	// tables land in okt_tasks and application tables
	// land in okt.
	cfg.Task.Database = "tasks"
	cfg.Isolation.DefaultDatabase = "default"

	registry, err := dbpool.New(ctx, cfg)
	if err != nil {
		t.Fatalf("opening test registry: %v", err)
	}
	defaultPool := registry.Default().Pool
	tasksPool := registry.Get("tasks").Pool

	rbacSvc, err := rbac.SetupRBAC(defaultPool)
	if err != nil {
		t.Fatalf("setting up RBAC: %v", err)
	}

	fetchStrategy := fetch.NewFetchStrategy(fetch.NewFetchResolutionProvider())
	taskEnqueuer := &RecordingTaskEnqueuer{}

	queries := store.New(defaultPool)
	storageBackend := NewTestStorageBackend(t)
	h := api.NewHandler(queries, cfg, rbacSvc, defaultPool, registry, audit.NewPostgresRecorder(defaultPool))
	h.SetSource(handler.NewSource(nil, fetchStrategy, nil, nil, nil, storageBackend, TestParsers()))
	WireRepoSettings(h, nil, fetchStrategy)
	h.SetStorage(handler.NewStorage(storageBackend))
	h.SetTaskEnqueuer(taskEnqueuer)
	mcpHandler := wireOAuthAndMCP(t, cfg, queries, registry, h, taskEnqueuer, nil, tasksPool)

	server := httptest.NewServer(h.Router())

	// Tear down on test completion. The server is closed first
	// so in-flight handlers can finish and release their pool
	// connections before we close the registry (which would
	// otherwise invalidate those connections mid-request). The
	// registry.Close() covers both the "default" and "tasks"
	// pools, so the MultiDBTestEnv.TasksDB handle is closed
	// implicitly.
	t.Cleanup(func() { server.Close() })
	t.Cleanup(func() { registry.Close() })

	return &MultiDBTestEnv{
		TestEnv: &TestEnv{
			MCP: mcpHandler,
			Server:       server,
			BaseURL:      server.URL,
			DB:           defaultPool,
			Config:       cfg,
			RBAC:         rbacSvc,
			TaskEnqueuer: taskEnqueuer,
			Storage:      storageBackend,
		},
		TasksDB: tasksPool,
	}
}

// MultiDBTestEnv bundles the standard TestEnv with a
// pointer to the "tasks" pool, so multi-DB tests can
// inspect River's tables (or any other rows in the
// tasks database) directly.
type MultiDBTestEnv struct {
	*TestEnv
	TasksDB *pgxpool.Pool
}

func (r *RecordingTaskEnqueuer) EnqueueRetrieveSourceFromHTTP(_ context.Context, args handler.RetrieveSourceArgs) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.nextID++
	r.Enqueued = append(r.Enqueued, args)
	return "test-job-1", nil
}

func (r *RecordingTaskEnqueuer) EnqueueSourceDecompositionFromHTTP(_ context.Context, args handler.SourceDecompositionArgs) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.nextID++
	r.Decompositions = append(r.Decompositions, args)
	return "test-decomp-1", nil
}

func (r *RecordingTaskEnqueuer) EnqueueAnnotateReportFromHTTP(_ context.Context, args handler.AnnotateReportArgs) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.nextID++
	r.ReportAnnotates = append(r.ReportAnnotates, args)
	return "test-annotate-1", nil
}

func (r *RecordingTaskEnqueuer) EnqueueExtractConceptsFromHTTP(_ context.Context, args handler.ExtractConceptsArgs) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.nextID++
	r.ExtractConcepts = append(r.ExtractConcepts, args)
	return "test-extract-concepts-1", nil
}

func (r *RecordingTaskEnqueuer) EnqueueRecomputeConceptGroupsFromHTTP(_ context.Context, args handler.RecomputeConceptGroupsArgs) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.nextID++
	r.RecomputeGroups = append(r.RecomputeGroups, args)
	return "test-recompute-concept-groups-1", nil
}

func (r *RecordingTaskEnqueuer) EnqueueSynthesizeFromHTTP(_ context.Context, args handler.SynthesizeArgs) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.nextID++
	r.Synthesizes = append(r.Synthesizes, args)
	return "test-synthesize-concept-1", nil
}

// ExtractConceptsCount returns the number of extract_concepts calls
// recorded so far. Thread-safe.
func (r *RecordingTaskEnqueuer) ExtractConceptsCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.ExtractConcepts)
}

// ExtractConceptsSnapshot returns a copy of the recorded
// extract_concepts calls so tests can inspect them.
func (r *RecordingTaskEnqueuer) ExtractConceptsSnapshot() []handler.ExtractConceptsArgs {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]handler.ExtractConceptsArgs, len(r.ExtractConcepts))
	copy(out, r.ExtractConcepts)
	return out
}

// ReportAnnotateCount returns the number of annotate_report calls
// recorded so far. Thread-safe so tests can assert without racing
// the handler goroutine.
func (r *RecordingTaskEnqueuer) ReportAnnotateCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.ReportAnnotates)
}

// ReportAnnotatesSnapshot returns a copy of the recorded
// annotate_report calls so tests can inspect them without holding
// the lock while asserting.
func (r *RecordingTaskEnqueuer) ReportAnnotatesSnapshot() []handler.AnnotateReportArgs {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]handler.AnnotateReportArgs, len(r.ReportAnnotates))
	copy(out, r.ReportAnnotates)
	return out
}

// NewTestEnv wires the same default handler bundle every
// other test uses. The list of resolution providers in
// the strategy is just the plain HTTP fetch — Unpaywall
// is omitted because the test env never sets UNPAYWALL_EMAIL.
// Tests that need Unpaywall in the chain use
// NewTestEnvWithUnpaywall.
func NewTestEnv(t testing.TB) *TestEnv {
	return newTestEnv(t, nil, nil, nil, nil)
}

// NewTestEnvWithChunker is a variant of NewTestEnv that
// registers a single SimpleChunkingProvider under the
// "simple" id, so /sources/decomposition/providers surfaces
// it. The chunkSize and chunkOverlap are passed straight
// through to NewSimpleChunkingProvider (which applies its
// own defaults for invalid values). Fact-extraction maps
// stay nil, matching the default env.
func NewTestEnvWithChunker(t testing.TB, chunkSize, chunkOverlap int) *TestEnv {
	chunker := decomposition.NewSimpleChunkingProvider(chunkSize, chunkOverlap)
	chunkers := map[string]decomposition.ChunkingProvider{"simple": chunker}
	return newTestEnv(t, nil, chunkers, nil, nil)
}

// NewTestEnvWithUnpaywall is a variant of NewTestEnv that
// registers the Unpaywall resolution provider ahead of the
// plain HTTP fetch (mirroring the production wiring in
// cmd/app/api.go). The point of the variant is to let
// e2e tests cover the multi-provider strategy path
// without taking a runtime dependency on the env var
// (which would couple the test to process-wide state).
// The Unpaywall instance is built with the same parser
// set the plain fetch uses (Trafilatura + MuPDF) so the
// OA-location body is parsed the same way production
// parses it.
func NewTestEnvWithUnpaywall(t testing.TB) *TestEnv {
	parsers := []content_parsing.Parser{
		content_parsing.NewTrafilaturaParser(),
		content_parsing.NewFitzPDFParser(),
	}
	return newTestEnv(t, fetch.NewUnpaywallResolutionProviderWithParsers("tester@example.com", parsers...), nil, nil, nil)
}

// NewTestEnvWithAI is a variant of NewTestEnv that wires an
// AI-provider handler bundle (with embedding config + provider)
// so /ai/providers and /ai/embedding/providers serve real
// responses. Tests pass a fully-built *handler.AI (typically
// constructed with stub providers inline) and the env wires it
// via SetAI before the server starts.
func NewTestEnvWithAI(t testing.TB, aiBundle *handler.AI) *TestEnv {
	return newTestEnv(t, nil, nil, nil, aiBundle)
}

// NewTestEnvWithSearch is a variant of NewTestEnv that wires a
// search-providers map into the source handler bundle, so
// /sources/{provider}/search returns real responses. Tests pass
// a pre-built map (typically a single stub provider) and the env
// wires it via SetSource. The fetch strategy and other maps stay
// at the default env's values (plain HTTP fetch, no chunkers).
func NewTestEnvWithSearch(t testing.TB, searchProviders map[string]search.SearchProvider) *TestEnv {
	return newTestEnvWithSearch(t, searchProviders, nil)
}

// newTestEnvWithSearch is the search-variant shared builder. It
// mirrors newTestEnv but plugs the supplied search providers into
// the source bundle instead of passing nil. chunkers stays nil
// (the search tests don't exercise decomposition).
func newTestEnvWithSearch(
	t testing.TB,
	searchProviders map[string]search.SearchProvider,
	aiBundle *handler.AI,
) *TestEnv {
	t.Helper()

	ctx := context.Background()

	dbURL := os.Getenv("OKT_TEST_DATABASE_URL")
	if dbURL == "" {
		dbURL = "postgres://okt:okt_test@localhost:5433/okt?sslmode=disable"
	}

	resetTestDatabase(ctx, t, dbURL)

	cfg := &config.Config{
		Auth: config.AuthConfig{
			JWTSecret: "test-jwt-secret-key",
			TokenTTL:  24 * time.Hour,
		},
		Bootstrap: config.BootstrapConfig{DefaultRepository: false},
	}
	cfg.Databases = map[string]config.DatabaseConfig{
		"default": parseTestDBConfig(t, dbURL),
	}
	cfg.System.Database = "default"
	cfg.Task.Database = "default"
	cfg.Isolation.DefaultDatabase = "default"

	registry, err := dbpool.New(ctx, cfg)
	if err != nil {
		t.Fatalf("opening test pool via registry: %v", err)
	}
	pool := registry.Default().Pool

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
	taskEnqueuer := &RecordingTaskEnqueuer{}

	queries := store.New(pool)
	storageBackend := NewTestStorageBackend(t)
	h := api.NewHandler(queries, cfg, rbacSvc, pool, registry, audit.NewPostgresRecorder(pool))
	h.SetSource(handler.NewSource(searchProviders, fetchStrategy, nil, nil, nil, storageBackend, TestParsers()))
	WireRepoSettings(h, searchProviders, fetchStrategy)
	h.SetStorage(handler.NewStorage(storageBackend))
	h.SetTaskEnqueuer(taskEnqueuer)
	if aiBundle != nil {
		h.SetAI(aiBundle)
	}
	mcpHandler := wireOAuthAndMCP(t, cfg, queries, registry, h, taskEnqueuer, searchProviders, pool)

	server := httptest.NewServer(h.Router())

	t.Cleanup(func() { server.Close() })
	t.Cleanup(func() { registry.Close() })

	return &TestEnv{
		Server:       server,
		BaseURL:      server.URL,
		DB:           pool,
		Config:       cfg,
		RBAC:         rbacSvc,
		TaskEnqueuer: taskEnqueuer,
		Storage:      storageBackend,
		AI:           aiBundle,
		MCP:          mcpHandler,
	}
}

// newTestEnv is the shared test-env builder. unpaywall is
// nil for the default env (NewTestEnv) and non-nil for
// the Unpaywall variant; when non-nil it is registered
// ahead of the plain HTTP fetch so the strategy order
// matches production. chunkers / factExtractors carry
// decomposition providers; passing nil for either leaves
// the corresponding map empty in the source bundle. aiBundle
// is nil for the default env; when non-nil, SetAI is called
// so /ai/providers and /ai/embedding/providers serve responses.
func newTestEnv(
	t testing.TB,
	unpaywall fetch.ResolutionProvider,
	chunkers map[string]decomposition.ChunkingProvider,
	factExtractors map[string]decomposition.FactExtractionProvider,
	aiBundle *handler.AI,
) *TestEnv {
	t.Helper()

	ctx := context.Background()

	dbURL := os.Getenv("OKT_TEST_DATABASE_URL")
	if dbURL == "" {
		dbURL = "postgres://okt:okt_test@localhost:5433/okt?sslmode=disable"
	}

	// Reset the test database. The e2e suite uses one database
	// for everything; we drop both application schemas so each
	// test starts on a clean slate.
	resetTestDatabase(ctx, t, dbURL)

	// Build a real config + registry so the test pool has the
	// right search_path on every connection. We reuse the
	// production dbpool.New pipeline (which applies DDL, sets
	// the AfterConnect hook, pings). The cfg has a single
	// "default" entry pointing at the test URL.
	cfg := &config.Config{
		Auth: config.AuthConfig{
			JWTSecret: "test-jwt-secret-key",
			TokenTTL:  24 * time.Hour,
		},
		Bootstrap: config.BootstrapConfig{DefaultRepository: false},
	}
	cfg.Databases = map[string]config.DatabaseConfig{
		"default": parseTestDBConfig(t, dbURL),
	}
	cfg.System.Database = "default"
	cfg.Task.Database = "default"
	cfg.Isolation.DefaultDatabase = "default"

	registry, err := dbpool.New(ctx, cfg)
	if err != nil {
		t.Fatalf("opening test pool via registry: %v", err)
	}
	pool := registry.Default().Pool

	rbacSvc, err := rbac.SetupRBAC(pool)
	if err != nil {
		t.Fatalf("setting up RBAC: %v", err)
	}

	// Wire the source handler with no search providers (so the
	// "search" features return 503 the same way as in production
	// without keys), a real fetch strategy (so /classify works),
	// and the recording task enqueuer (so /sources/retrieve can
	// be tested without booting a real task manager).
	//
	// The strategy is built in priority order. Unpaywall, when
	// configured, runs first so DOI-classified sources hit
	// the OA lookup before the plain HTTP fetch on the
	// publisher landing page. This matches the production
	// wiring in cmd/app/api.go. The parser set (Trafilatura
	// + MuPDF) is shared between the plain fetch and Unpaywall
	// so PDF and HTML responses are both extractable.
	var resolutionProviders []fetch.ResolutionProvider
	parsers := []content_parsing.Parser{
		content_parsing.NewTrafilaturaParser(),
		content_parsing.NewFitzPDFParser(),
	}
	if unpaywall != nil {
		// Re-wrap the caller's Unpaywall with the parser set
		// so the OA-location body is parsed the same way the
		// plain fetch parses it. The caller passes the
		// already-constructed provider; we accept it as-is
		// when it already carries parsers (production path)
		// and otherwise rely on the default Trafilatura-only
		// fallback inside the constructor.
		resolutionProviders = append(resolutionProviders, unpaywall)
	}
	resolutionProviders = append(resolutionProviders, fetch.NewFetchResolutionProviderWithParsers(parsers...))
	fetchStrategy := fetch.NewFetchStrategy(resolutionProviders...)
	taskEnqueuer := &RecordingTaskEnqueuer{}

	queries := store.New(pool)
	storageBackend := NewTestStorageBackend(t)
	h := api.NewHandler(queries, cfg, rbacSvc, pool, registry, audit.NewPostgresRecorder(pool))
	h.SetSource(handler.NewSource(nil, fetchStrategy, chunkers, factExtractors, nil, storageBackend, TestParsers()))
	WireRepoSettings(h, nil, fetchStrategy)
	h.SetStorage(handler.NewStorage(storageBackend))
	h.SetTaskEnqueuer(taskEnqueuer)
	if aiBundle != nil {
		h.SetAI(aiBundle)
	}
	mcpHandler := wireOAuthAndMCP(t, cfg, queries, registry, h, taskEnqueuer, nil, pool)

	server := httptest.NewServer(h.Router())

	// Tear down on test completion. The server is closed first
	// so in-flight handlers can finish and release their pool
	// connections before we close the registry (which would
	// otherwise invalidate those connections mid-request).
	t.Cleanup(func() { server.Close() })
	t.Cleanup(func() { registry.Close() })

	return &TestEnv{
		Server:       server,
		BaseURL:      server.URL,
		DB:           pool,
		Config:       cfg,
		RBAC:         rbacSvc,
		TaskEnqueuer: taskEnqueuer,
		Storage:      storageBackend,
		AI:           aiBundle,
		MCP:          mcpHandler,
	}
}

// parseTestDBConfig turns a Postgres URL into a config.DatabaseConfig
// by parsing it with url.Parse. Tests pass a URL of the form
// `postgres://user:pass@host:port/dbname?sslmode=disable`; the
// DSN we hand to pgxpool is exactly that URL, so this round-trip is
// lossless.
func parseTestDBConfig(t testing.TB, dbURL string) config.DatabaseConfig {
	t.Helper()
	u, err := neturl.Parse(dbURL)
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

// wireOAuthAndMCP builds the OAuth 2.1 authorization server + MCP
// handler and attaches them to the test Handler, mirroring the
// production wiring in cmd/app/api.go. The OAuth server uses the
// test cfg.Auth.JWTSecret for token signing (shared with the
// OAuthBearer middleware's verification) and the same system-pool
// *store.Queries the rest of the test env uses. The MCP handler's
// per-call repo resolver reuses the Handler's RepoDBCache +
// SlugCache so UUID-or-slug resolution matches the REST routes.
//
// searchProviders flows through to the MCP handler via
// SetSearchProviders so the searchSources tool is enabled when a
// test wires a stub provider, and disabled (returns a "not
// configured" tool error) when nil/empty — mirroring production.
//
// taskPool is the pool River's river_job table lives on. The
// getSourceTasks/getReportTasks summary (verbose=false) path runs
// a single SQL GROUP BY against this pool, so tests that exercise
// the summary mode must pass the same pool they insert river_job
// rows into. Pass nil to leave the summary path on the legacy
// per-page client-side aggregation (e.g. verbose-mode tests that
// only use a stub TaskClient).
//
// Returns the constructed *handler.MCP so callers (the TestEnv
// builders) can stash it on TestEnv.MCP for tests that need to
// attach a stub TaskClient (e.g. getSourceTasks verbose tests).
//
// The MCP resource URL is derived from the test server's URL
// (server.URL) so the protected-resource metadata points at the
// httptest server, not localhost:8080. The issuer is likewise the
// test server's URL so access-token `iss` claims match what the
// metadata advertises.
func wireOAuthAndMCP(t testing.TB, cfg *config.Config, queries *store.Queries, registry *dbpool.Registry, h *api.Handler, enqueuer handler.TaskEnqueuer, searchProviders map[string]search.SearchProvider, taskPool *pgxpool.Pool) *handler.MCP {
	t.Helper()
	// The test server URL isn't known until httptest.NewServer runs,
	// so we use a placeholder issuer here and let the MCP/OAuth
	// handlers re-derive it from the request Host when they need an
	// absolute URL. The e2e tests that assert on metadata shape
	// pass an explicit issuer; the ones that exercise the token
	// flow build tokens directly via oauth.IssueAccessToken and
	// don't read the metadata.
	issuer := cfg.OAuth.Issuer
	if issuer == "" {
		issuer = "http://localhost:8080"
	}
	oauthCfg := oauth.Config{
		Issuer:          issuer,
		AccessTokenTTL:  cfg.OAuth.AccessTokenTTL,
		RefreshTokenTTL: cfg.OAuth.RefreshTokenTTL,
		AuthCodeTTL:     cfg.OAuth.AuthCodeTTL,
	}
	if oauthCfg.AccessTokenTTL == 0 {
		oauthCfg.AccessTokenTTL = 15 * time.Minute
	}
	if oauthCfg.RefreshTokenTTL == 0 {
		oauthCfg.RefreshTokenTTL = 30 * 24 * time.Hour
	}
	if oauthCfg.AuthCodeTTL == 0 {
		oauthCfg.AuthCodeTTL = 10 * time.Minute
	}
	oauthServer := oauth.NewServer(oauthCfg, cfg.Auth.JWTSecret, queries, oauth.DefaultUserLookup(queries))
	h.SetOAuth(handler.NewOAuth(oauthServer, issuer, issuer+"/api/v1/mcp"))
	handler.SetLoginCookieSecret(cfg.Auth.JWTSecret)
	mcpHandler := handler.NewMCP(h.Deps(), handler.ResolveRepoPoolFromCaches(registry, h.RepoDBCache(), h.SlugCache()))
	mcpHandler.SetTaskEnqueuer(enqueuer)
	mcpHandler.SetTaskPool(taskPool)
	mcpHandler.SetSearchProviders(searchProviders)
	h.SetMCP(mcpHandler)
	_ = fmt.Sprintf // keep fmt referenced for the placeholder logic above
	return mcpHandler
}

// TestParsers returns the content_parsing.Parser instances used by
// the test Source handler bundle. The upload handler uses these to
// parse uploaded PDF/HTML files in-process; tests that exercise the
// upload endpoint need real parsers, while tests that don't touch
// uploads still get a non-nil slice so the handler can be wired
// without a special case. Exported so e2e tests outside testutil
// (e.g. tasks_test.go's bespoke handler wiring) can reuse the same
// set.
func TestParsers() []content_parsing.Parser {
	return []content_parsing.Parser{
		content_parsing.NewTrafilaturaParser(),
		content_parsing.NewFitzPDFParser(),
	}
}

// GrantUserRole assigns a Casbin role to a user identified by their
// UUID string, reloads the RBAC policy cache, and re-logs the user
// in so their JWT carries the updated role. This mirrors the
// bootstrapSysAdmin pattern but is generic — callers pass the role
// name (e.g. rbac.RoleSysAdmin) and domain (e.g. rbac.DomainAll or
// a specific repo UUID). Fatal on any step so tests fail fast.
func GrantUserRole(t testing.TB, env *TestEnv, userID, role, domain string) {
	t.Helper()
	if _, err := env.DB.Exec(
		context.Background(),
		`INSERT INTO casbin_rule (p_type, v0, v1, v2) VALUES ('g', $1, $2, $3)`,
		userID, role, domain,
	); err != nil {
		t.Fatalf("seeding %s grouping policy for user %s: %v", role, userID, err)
	}
	if err := env.RBAC.LoadPolicy(); err != nil {
		t.Fatalf("reloading RBAC policy: %v", err)
	}
}

// RegisterUser registers a new user via the HTTP API, logs them in,
// fetches their UUID from /users/me, and returns (uuid, token). It
// does NOT assign any RBAC role — callers use GrantUserRole for
// that. Fatal on any HTTP error.
func RegisterUser(t testing.TB, baseURL, email, password, displayName string) (uuid string, token string) {
	t.Helper()
	body := fmt.Sprintf(`{"email":"%s","password":"%s","display_name":"%s"}`, email, password, displayName)
	resp, err := http.Post(baseURL+"/api/v1/auth/register", "application/json", strings.NewReader(body))
	if err != nil || resp.StatusCode != http.StatusCreated {
		t.Fatalf("register %s: status=%v err=%v", email, resp.StatusCode, err)
	}
	resp.Body.Close()

	loginBody := fmt.Sprintf(`{"email":"%s","password":"%s"}`, email, password)
	loginResp, err := http.Post(baseURL+"/api/v1/auth/login", "application/json", strings.NewReader(loginBody))
	if err != nil || loginResp.StatusCode != http.StatusOK {
		t.Fatalf("login %s: status=%v err=%v", email, loginResp.StatusCode, err)
	}
	loginBytes, _ := io.ReadAll(loginResp.Body)
	loginResp.Body.Close()
	var lr struct{ Token string `json:"token"` }
	if err := json.Unmarshal(loginBytes, &lr); err != nil {
		t.Fatalf("decoding login response: %v", err)
	}
	token = lr.Token

	req, _ := http.NewRequest("GET", baseURL+"/api/v1/users/me", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	meResp, err := http.DefaultClient.Do(req)
	if err != nil || meResp.StatusCode != http.StatusOK {
		t.Fatalf("fetching /users/me: status=%v err=%v", meResp.StatusCode, err)
	}
	meBytes, _ := io.ReadAll(meResp.Body)
	meResp.Body.Close()
	var me struct{ ID string `json:"id"` }
	if err := json.Unmarshal(meBytes, &me); err != nil {
		t.Fatalf("decoding /users/me: %v", err)
	}
	return me.ID, token
}
