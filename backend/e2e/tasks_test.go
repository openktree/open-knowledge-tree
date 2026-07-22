//go:build e2e

package e2e_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/openktree/open-knowledge-tree/backend/internal/audit"
	"github.com/openktree/open-knowledge-tree/backend/e2e/testutil"
	"github.com/openktree/open-knowledge-tree/backend/internal/api"
	"github.com/openktree/open-knowledge-tree/backend/internal/api/handler"
	"github.com/openktree/open-knowledge-tree/backend/internal/config"
	"github.com/openktree/open-knowledge-tree/backend/internal/dbpool"
	"github.com/openktree/open-knowledge-tree/backend/internal/providers/fetch"
	"github.com/openktree/open-knowledge-tree/backend/internal/rbac"
	"github.com/openktree/open-knowledge-tree/backend/internal/store"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
)

// stubTaskClient is a hand-rolled TaskClient. The interface in
// handler/tasks.go exists exactly so tests can substitute a
// deterministic source of JobRow values. The duration fields
// are computed from the timestamps the row already carries, so
// a stub is the cleanest way to assert the math without driving
// a real River worker (which is what taskmanager_test.go does
// for the worker layer; the handler layer doesn't need it).
type stubTaskClient struct {
	listResult *river.JobListResult
	listErr    error
	// listParams captures the last params passed to JobList so
	// tests can assert the handler applied the right filters,
	// sort, and cursor. nil when JobList hasn't been called or
	// when the test doesn't care.
	listParams *river.JobListParams
	getRow     *rivertype.JobRow
	getErr     error
	cancelRow  *rivertype.JobRow
	cancelErr  error
}

func (s *stubTaskClient) JobList(_ context.Context, params *river.JobListParams) (*river.JobListResult, error) {
	s.listParams = params
	return s.listResult, s.listErr
}

func (s *stubTaskClient) JobGet(_ context.Context, _ int64) (*rivertype.JobRow, error) {
	return s.getRow, s.getErr
}

func (s *stubTaskClient) JobCancel(_ context.Context, _ int64) (*rivertype.JobRow, error) {
	return s.cancelRow, s.cancelErr
}

// tasksEnv builds a fresh handler bundle backed by the same
// test database the rest of the e2e suite uses, with a stub
// TaskClient wired in. We can't reuse testutil.NewTestEnv for
// this because it leaves the tasks field nil (intentionally —
// the production e2e path exercises the "task manager not
// configured" branch, and tests that want a real River client
// use NewMultiDBTestEnv and drive the worker directly). The
// handler is small enough that duplicating the wiring inline
// is clearer than adding a third testutil variant.
//
// All four pieces of state the handler needs (the queries
// bundle, the config, the system pool, the dbpool registry)
// come out of dbpool.New, which is the same factory production
// uses. The only difference between this and production is
// that we point TaskEnqueuer at a no-op and Tasks at the
// stub.
func tasksEnv(t *testing.T, client handler.TaskClient) *httptest.Server {
	t.Helper()
	server, _, _, _ := tasksEnvWithRBAC(t, client)
	return server
}

// tasksEnvWithRBAC is the same as tasksEnv but also returns the
// RBAC service and the system pool so tests that need to seed
// casbin rules and reload the enforcer can do so. Used by the
// admin_tasks tests.
func tasksEnvWithRBAC(t *testing.T, client handler.TaskClient) (*httptest.Server, *rbac.Service, *pgxpool.Pool, *dbpool.Registry) {
	t.Helper()
	ctx := context.Background()

	dbURL := os.Getenv("OKT_TEST_DATABASE_URL")
	if dbURL == "" {
		dbURL = "postgres://okt:okt_test@localhost:5433/okt?sslmode=disable"
	}

	// Drop both application schemas so the test starts clean.
	// Mirrors testutil.NewTestEnv exactly (including the
	// dev-DB guard that refuses to reset port 5432).
	testutil.ResetTestDatabaseForTest(ctx, t, dbURL)

	cfg := &config.Config{
		Auth: config.AuthConfig{
			JWTSecret: "test-jwt-secret-key",
			TokenTTL:  24 * time.Hour,
		},
		Bootstrap: config.BootstrapConfig{DefaultRepository: false},
	}
	cfg.Databases = map[string]config.DatabaseConfig{
		"default": parseTestDBConfigLocal(t, dbURL),
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

	fetchStrategy := fetch.NewFetchStrategy(fetch.NewFetchResolutionProvider())
	queries := store.New(pool)
	h := api.NewHandler(queries, cfg, rbacSvc, pool, registry, audit.NoopRecorder{})
	h.SetSource(handler.NewSource(nil, fetchStrategy, nil, nil, nil, nil, testutil.TestParsers()))
	h.SetStorage(handler.NewStorage(nil))
	h.SetTasks(handler.NewTasks(client, pool, nil))
	// Wire the ontology source + provider registry so
	// createRepository's settings-seeding path (which expands the
	// "all" context against the ontology) doesn't 400. Mirrors
	// the shared env builder's WireRepoSettings call.
	testutil.WireRepoSettings(h, nil, fetchStrategy)

	server := httptest.NewServer(h.Router())
	t.Cleanup(func() { server.Close() })
	t.Cleanup(func() { registry.Close() })
	return server, rbacSvc, pool, registry
}

// parseTestDBConfigLocal is a copy of the helper in
// testutil/setup.go. We can't import the unexported symbol, and
// adding it to the testutil exports just for this test isn't
// worth it — three other test files keep their config wiring
// local too. The function is small and the URL shape is fixed
// by the docker-compose test service, so the copy is safe.
func parseTestDBConfigLocal(t testing.TB, dbURL string) config.DatabaseConfig {
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

func registerAndGrantTasksUser(t *testing.T, server *httptest.Server, rbacSvc *rbac.Service, pool *pgxpool.Pool, email, password, displayName string) *authClient {
	t.Helper()
	client := newAuthClient(server.URL)
	if r, _ := client.register(email, password, displayName); r.StatusCode != http.StatusCreated {
		t.Fatalf("register: status %d", r.StatusCode)
	}
	client.token = loginUser(client, email, password)

	resp, body := client.do("GET", "/api/v1/users/me", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /users/me: %d: %s", resp.StatusCode, body)
	}
	var me struct{ ID string `json:"id"` }
	if err := json.Unmarshal(body, &me); err != nil {
		t.Fatalf("decode /users/me: %v", err)
	}

	if _, err := pool.Exec(context.Background(),
		`INSERT INTO casbin_rule (p_type, v0, v1, v2) VALUES ('g', $1, 'sysadmin', '*')`, me.ID); err != nil {
		t.Fatalf("grant sysadmin *: %v", err)
	}
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO casbin_rule (p_type, v0, v1, v2) VALUES ('g', $1, 'sysadmin', 'system')`, me.ID); err != nil {
		t.Fatalf("grant sysadmin system: %v", err)
	}
	if err := rbacSvc.LoadPolicy(); err != nil {
		t.Fatalf("reload RBAC: %v", err)
	}

	client.token = loginUser(client, email, password)
	return client
}

// jobResponse mirrors the JSON shape the handler emits. It is
// not a stable contract across handlers — only this one
// response — so a local decode struct is the right granularity.
type jobResponse struct {
	ID           int64   `json:"id"`
	State        string  `json:"state"`
	Kind         string  `json:"kind"`
	CreatedAt    string  `json:"created_at"`
	AttemptedAt  *string `json:"attempted_at"`
	FinalizedAt  *string `json:"finalized_at"`
	DurationMS   *int64  `json:"duration_ms"`
	QueueWaitMS  *int64  `json:"queue_wait_ms"`
}

type listResponse struct {
	Jobs        []jobResponse `json:"jobs"`
	HasMore     bool          `json:"has_more"`
	NextCursor  *string       `json:"next_cursor"`
}

// timePtr is a tiny helper. We can't take the address of a
// time.Time literal (Go forbids &literal), and the JSON
// encoder renders *time.Time as RFC3339 — the same shape we
// see when the row was produced by River. We use this to build
// the *time.Time fields on a stubbed JobRow.
func timePtr(t time.Time) *time.Time { return &t }

// TestTasksEndpointNotConfiguredByDefault verifies the
// production wiring: when SetTasks was never called, the
// tasks router returns 503 with a "service not configured"
// body, not 404. This is the contract taskmanager_test.go
// and the wiring layer both rely on; a regression here means
// the whole task surface silently disappears for users.
func TestTasksEndpointNotConfiguredByDefault(t *testing.T) {
	server, rbacSvc, pool, _ := tasksEnvWithRBAC(t, nil)
	// When client is nil, NewTasks sets client=nil, which
	// makes ListJobs/GetJob short-circuit to 503 ("task
	// manager not configured"). Verify that path.

	client := registerAndGrantTasksUser(t, server, rbacSvc, pool, "notconfigured@example.com", "password123", "No Tasks")

	resp, body := client.do("GET", "/api/v1/tasks/", nil)
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d: %s", resp.StatusCode, body)
	}

	resp, body = client.do("GET", "/api/v1/tasks/1", nil)
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d: %s", resp.StatusCode, body)
	}
}

// TestTasksListIncludesDurationAndQueueWait verifies the two
// new fields land on the wire with the right shape. The test
// drives both terminals states (completed + not-yet-attempted)
// plus a running job, so the per-state branch in jobDurationMs
// is covered end to end.
func TestTasksListIncludesDurationAndQueueWait(t *testing.T) {
	createdAt := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)
	attemptedAt := timePtr(createdAt.Add(2 * time.Second))  // 2s queue wait
	finalizedAt := timePtr(attemptedAt.Add(7 * time.Second)) // 7s execution
	runningAttempted := timePtr(createdAt.Add(500 * time.Millisecond))

	client := &stubTaskClient{
		listResult: &river.JobListResult{
			Jobs: []*rivertype.JobRow{
				// Completed job: 2s queue wait, 7s run.
				makeJobRow(1, "completed", createdAt, attemptedAt, finalizedAt),
				// Running job: 0.5s queue wait, no finalised timestamp.
				makeJobRow(2, "running", createdAt, runningAttempted, nil),
				// Scheduled job: never attempted, so both
				// duration fields are null.
				makeJobRow(3, "scheduled", createdAt, nil, nil),
			},
		},
	}

	server, rbacSvc, pool, _ := tasksEnvWithRBAC(t, client)
	auth := registerAndGrantTasksUser(t, server, rbacSvc, pool, "duration@example.com", "password123", "Duration Tester")

	resp, body := auth.do("GET", "/api/v1/tasks/", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list: expected 200, got %d: %s", resp.StatusCode, body)
	}

	var got listResponse
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Jobs) != 3 {
		t.Fatalf("expected 3 jobs, got %d", len(got.Jobs))
	}

	// Job 1: completed, 7000ms duration, 2000ms queue wait.
	j1 := got.Jobs[0]
	if j1.State != "completed" {
		t.Errorf("job 1: expected state=completed, got %q", j1.State)
	}
	if j1.DurationMS == nil || *j1.DurationMS != 7000 {
		t.Errorf("job 1: expected duration_ms=7000, got %v", j1.DurationMS)
	}
	if j1.QueueWaitMS == nil || *j1.QueueWaitMS != 2000 {
		t.Errorf("job 1: expected queue_wait_ms=2000, got %v", j1.QueueWaitMS)
	}

	// Job 2: running, 500ms queue wait, duration is null
	// (we don't fabricate a running job's "now" — the
	// server's clock is the source of truth, so a
	// deterministic assertion on the duration would couple
	// us to the test machine's clock). The point of this
	// row is to prove duration_ms is present and numeric
	// for the running branch; the exact value drifts.
	j2 := got.Jobs[1]
	if j2.State != "running" {
		t.Errorf("job 2: expected state=running, got %q", j2.State)
	}
	if j2.QueueWaitMS == nil || *j2.QueueWaitMS != 500 {
		t.Errorf("job 2: expected queue_wait_ms=500, got %v", j2.QueueWaitMS)
	}
	if j2.DurationMS == nil {
		t.Error("job 2: expected duration_ms to be a number (running), got null")
	}

	// Job 3: scheduled, never attempted. Both fields null.
	j3 := got.Jobs[2]
	if j3.State != "scheduled" {
		t.Errorf("job 3: expected state=scheduled, got %q", j3.State)
	}
	if j3.DurationMS != nil {
		t.Errorf("job 3: expected duration_ms=null, got %d", *j3.DurationMS)
	}
	if j3.QueueWaitMS != nil {
		t.Errorf("job 3: expected queue_wait_ms=null, got %d", *j3.QueueWaitMS)
	}
}

// TestTasksGetIncludesDuration covers the single-job endpoint.
// The shape and math are identical to the list path; the
// regression risk is purely that jobRowToMap is shared by
// both routes, so an accidental refactor that drops the
// field on one of them slips past the list test.
func TestTasksGetIncludesDuration(t *testing.T) {
	createdAt := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)
	attemptedAt := timePtr(createdAt.Add(3 * time.Second))
	finalizedAt := timePtr(attemptedAt.Add(4 * time.Second))

	client := &stubTaskClient{
		getRow: makeJobRow(42, "completed", createdAt, attemptedAt, finalizedAt),
	}

	server, rbacSvc, pool, _ := tasksEnvWithRBAC(t, client)
	auth := registerAndGrantTasksUser(t, server, rbacSvc, pool, "getduration@example.com", "password123", "Get Duration")

	resp, body := auth.do("GET", "/api/v1/tasks/42", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get: expected 200, got %d: %s", resp.StatusCode, body)
	}

	var got jobResponse
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.ID != 42 {
		t.Errorf("expected id=42, got %d", got.ID)
	}
	if got.DurationMS == nil || *got.DurationMS != 4000 {
		t.Errorf("expected duration_ms=4000, got %v", got.DurationMS)
	}
	if got.QueueWaitMS == nil || *got.QueueWaitMS != 3000 {
		t.Errorf("expected queue_wait_ms=3000, got %v", got.QueueWaitMS)
	}
}

// TestTasksGet404WhenStubMisses verifies the handler still
// returns 404 (not 200-with-null-fields) when the underlying
// client reports a missing job. This is a guard against an
// over-eager refactor of jobRowToMap that swallows the "not
// found" case.
func TestTasksGet404WhenStubMisses(t *testing.T) {
	client := &stubTaskClient{
		getErr: fmt.Errorf("not found"),
	}
	server, rbacSvc, pool, _ := tasksEnvWithRBAC(t, client)
	auth := registerAndGrantTasksUser(t, server, rbacSvc, pool, "missingjob@example.com", "password123", "Missing Job")

	resp, body := auth.do("GET", "/api/v1/tasks/9999", nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", resp.StatusCode, body)
	}
}

// makeJobRow is a small constructor that fills in the bare
// minimum of JobRow fields the handler actually reads. Most
// fields (EncodedArgs, Metadata, Errors, Tags, AttemptedBy)
// are left zero so the JSON shape stays minimal and the test
// focuses on the duration contract.
func makeJobRow(id int64, state string, createdAt time.Time, attemptedAt, finalizedAt *time.Time) *rivertype.JobRow {
	return &rivertype.JobRow{
		ID:          id,
		Kind:        "retrieve_source",
		State:       rivertype.JobState(state),
		Queue:       "retrieve_source",
		Attempt:     1,
		MaxAttempts: 3,
		CreatedAt:   createdAt,
		ScheduledAt: createdAt,
		AttemptedAt: attemptedAt,
		FinalizedAt: finalizedAt,
	}
}

// TestTasksListPagination verifies the handler's cursor pagination
// and default sort contract:
//
//  1. The response carries has_more + next_cursor fields.
//  2. When River returns a LastCursor and the page is full,
//     has_more=true and next_cursor is the marshaled cursor
//     string (opaque to the UI, round-tripped as ?cursor=...).
//  3. When the page is partial (fewer than limit rows) or
//     LastCursor is nil, has_more=false and next_cursor=null.
//
// The sort direction (id-desc / "most recent first") is enforced
// by writeJobListResponse's OrderBy call; the stub captures the
// params but the JobListParams sort fields are private, so we
// rely on the OrderBy call being correct by construction (the
// helper applies it unconditionally) and on the integration
// suite (TestExtractConceptsPipeline etc.) to catch a regression
// where River rejects the params.
func TestTasksListPagination(t *testing.T) {
	createdAt := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)

	// Build a cursor that River's MarshalText accepts.
	// JobListCursorFromJob produces a cursor that can't be
	// marshaled (it carries the job row, not the serialized
	// shape), so we hand-roll the marshaled form. The shape is
	// jobListPaginationCursorJSON in river/job_list_params.go:
	// {id, kind, queue, sort_field, time}. We don't assert on
	// the cursor's content from the UI; we only need the handler
	// to round-trip it as an opaque string.
	cursorBytes, err := json.Marshal(struct {
		ID        int64     `json:"id"`
		Kind      string    `json:"kind"`
		Queue     string    `json:"queue"`
		SortField string    `json:"sort_field"`
		Time      time.Time `json:"time"`
	}{ID: 7, Kind: "retrieve_source", Queue: "retrieve_source", SortField: "id", Time: createdAt})
	if err != nil {
		t.Fatalf("marshal cursor payload: %v", err)
	}
	cursorStr := base64.URLEncoding.EncodeToString(cursorBytes)

	// Construct a JobListCursor by decoding the marshaled string
	// (the same path the handler uses for ?cursor=...).
	cursor := &river.JobListCursor{}
	if err := cursor.UnmarshalText([]byte(cursorStr)); err != nil {
		t.Fatalf("unmarshal cursor: %v", err)
	}

	// Full page (limit=3, 3 jobs returned, LastCursor set):
	// has_more=true, next_cursor=<marshaled cursor>.
	fullPageClient := &stubTaskClient{
		listResult: &river.JobListResult{
			Jobs: []*rivertype.JobRow{
				makeJobRow(3, "completed", createdAt, timePtr(createdAt), timePtr(createdAt)),
				makeJobRow(2, "completed", createdAt, timePtr(createdAt), timePtr(createdAt)),
				makeJobRow(1, "completed", createdAt, timePtr(createdAt), timePtr(createdAt)),
			},
			LastCursor: cursor,
		},
	}
	server, rbacSvc, pool, _ := tasksEnvWithRBAC(t, fullPageClient)
	auth := registerAndGrantTasksUser(t, server, rbacSvc, pool, "paging@example.com", "password123", "Paging Tester")

	resp, body := auth.do("GET", "/api/v1/tasks/?limit=3", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list: expected 200, got %d: %s", resp.StatusCode, body)
	}
	var got listResponse
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !got.HasMore {
		t.Error("expected has_more=true when page is full and LastCursor is set")
	}
	if got.NextCursor == nil || *got.NextCursor == "" {
		t.Fatal("expected non-empty next_cursor when page is full and LastCursor is set")
	}

	// Partial page (limit=3, only 2 jobs returned): has_more=false,
	// next_cursor=null even though LastCursor is set (the cursor
	// is for the last row of the returned slice; there is no
	// next page because we didn't fill the page).
	partialClient := &stubTaskClient{
		listResult: &river.JobListResult{
			Jobs: []*rivertype.JobRow{
				makeJobRow(2, "completed", createdAt, timePtr(createdAt), timePtr(createdAt)),
				makeJobRow(1, "completed", createdAt, timePtr(createdAt), timePtr(createdAt)),
			},
			LastCursor: cursor,
		},
	}
	server2, rbacSvc2, pool2, _ := tasksEnvWithRBAC(t, partialClient)
	auth2 := registerAndGrantTasksUser(t, server2, rbacSvc2, pool2, "partial@example.com", "password123", "Partial Tester")

	resp2, body2 := auth2.do("GET", "/api/v1/tasks/?limit=3", nil)
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("partial list: expected 200, got %d: %s", resp2.StatusCode, body2)
	}
	var got2 listResponse
	if err := json.Unmarshal(body2, &got2); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got2.HasMore {
		t.Error("expected has_more=false when page is partial (fewer than limit rows)")
	}
	if got2.NextCursor != nil {
		t.Errorf("expected next_cursor=null when page is partial, got %q", *got2.NextCursor)
	}

	// Empty result (LastCursor nil): has_more=false, next_cursor=null.
	emptyClient := &stubTaskClient{
		listResult: &river.JobListResult{Jobs: nil},
	}
	server3, rbacSvc3, pool3, _ := tasksEnvWithRBAC(t, emptyClient)
	auth3 := registerAndGrantTasksUser(t, server3, rbacSvc3, pool3, "empty@example.com", "password123", "Empty Tester")

	resp3, body3 := auth3.do("GET", "/api/v1/tasks/", nil)
	if resp3.StatusCode != http.StatusOK {
		t.Fatalf("empty list: expected 200, got %d: %s", resp3.StatusCode, body3)
	}
	var got3 listResponse
	if err := json.Unmarshal(body3, &got3); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got3.HasMore {
		t.Error("expected has_more=false when result is empty")
	}
	if got3.NextCursor != nil {
		t.Errorf("expected next_cursor=null when result is empty, got %q", *got3.NextCursor)
	}
}

// TestRepoTaskStats_RepoScoped verifies the per-repo tasks stats
// endpoint (GET /api/v1/repositories/{slug}/tasks/stats) returns
// the queue/state aggregation restricted to jobs whose metadata
// carries this repo's repo_id. Uses tasksEnvWithRBAC so the
// tasks bundle is wired (the default NewTestEnv leaves h.tasks
// nil, which would make the route 404). River jobs are inserted
// directly with metadata tags for two repos; only the matching
// repo's rows appear in the aggregation.
func TestRepoTaskStats_RepoScoped(t *testing.T) {
	server, rbacSvc, pool, _ := tasksEnvWithRBAC(t, &stubTaskClient{})
	ctx := context.Background()

	if err := ensureRiverSchemaOnPool(pool); err != nil {
		t.Fatalf("running River migrations: %v", err)
	}

	admin := registerAndGrantTasksUser(t, server, rbacSvc, pool, "repotasks-stats-admin@example.com", "passw0rd!", "Stats Admin")
	resp, body, repoID := createRepository(t, admin, "RepoTasks Stats", "repotasks-stats", "")
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create repo: %d %s", resp.StatusCode, body)
	}

	otherRepoID := "11111111-1111-1111-1111-111111111111"
	ourMeta := `{"repo_id":"` + repoID + `"}`
	otherMeta := `{"repo_id":"` + otherRepoID + `"}`
	_, err := pool.Exec(ctx, `
		INSERT INTO river_job (kind, state, args, metadata, created_at, scheduled_at, attempt, max_attempts, priority, queue)
		VALUES
		  ('retrieve_source', 'available', '{}'::jsonb, $1::jsonb, now(), now(), 0, 3, 1, 'retrieve_source'),
		  ('retrieve_source', 'available', '{}'::jsonb, $1::jsonb, now(), now(), 0, 3, 1, 'retrieve_source'),
		  ('source_decomposition', 'available', '{}'::jsonb, $2::jsonb, now(), now(), 0, 3, 1, 'source_decomposition')
	`, ourMeta, otherMeta)
	if err != nil {
		t.Fatalf("inserting river jobs: %v", err)
	}

	resp, body = admin.do("GET", "/api/v1/repositories/repotasks-stats/tasks/stats", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, body)
	}
	var out struct {
		Queues []struct {
			Queue  string            `json:"queue"`
			States map[string]int64  `json:"states"`
			Total  int64             `json:"total"`
		} `json:"queues"`
		Totals map[string]int64 `json:"totals"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decode stats: %v", err)
	}

	// Only our two retrieve_source rows should appear.
	if len(out.Queues) != 1 {
		t.Fatalf("expected 1 queue (retrieve_source), got %d: %v", len(out.Queues), out.Queues)
	}
	q := out.Queues[0]
	if q.Queue != "retrieve_source" {
		t.Fatalf("expected queue=retrieve_source, got %q", q.Queue)
	}
	if q.Total != 2 {
		t.Fatalf("expected total=2 for our repo, got %d", q.Total)
	}
	if q.States["available"] != 2 {
		t.Fatalf("expected {available:2}, got %v", q.States)
	}
	if out.Totals["total"] != 2 {
		t.Fatalf("expected totals.total=2, got %d", out.Totals["total"])
	}
}

// TestRepoTaskStats_DenyNonAdmin verifies a regular user without
// task.read gets 403 on the repo-scoped stats endpoint. Uses
// tasksEnvWithRBAC so the route is actually registered.
func TestRepoTaskStats_DenyNonAdmin(t *testing.T) {
	server, rbacSvc, pool, _ := tasksEnvWithRBAC(t, &stubTaskClient{})
	if err := ensureRiverSchemaOnPool(pool); err != nil {
		t.Fatalf("running River migrations: %v", err)
	}

	admin := registerAndGrantTasksUser(t, server, rbacSvc, pool, "repotasks-deny-admin@example.com", "passw0rd!", "Deny Admin")
	resp, body, _ := createRepository(t, admin, "RepoTasks Deny", "repotasks-deny", "")
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create repo: %d %s", resp.StatusCode, body)
	}

	regular := newAuthClient(server.URL)
	regular.register("repotasks-regular@example.com", "passw0rd!", "Regular")
	regular.token = loginUser(regular, "repotasks-regular@example.com", "passw0rd!")

	resp, _ = regular.do("GET", "/api/v1/repositories/repotasks-deny/tasks/stats", nil)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403 for non-permitted user, got %d", resp.StatusCode)
	}
}
