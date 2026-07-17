//go:build e2e

package e2e_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/openktree/open-knowledge-tree/backend/internal/api/handler"
	"github.com/openktree/open-knowledge-tree/backend/internal/rbac"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
)

// grantSysAdminOnPool inserts a system_admin grouping policy for
// the given user and reloads the in-process RBAC enforcer so the
// next request sees the new role. Mirrors the pattern in
// bootstrapSysAdmin (multi_db_test.go) but operates against the
// tasksEnv's pool + RBAC service, which the standard
// bootstrapSysAdmin helper doesn't reach.
func grantSysAdminOnPool(t *testing.T, ctx context.Context, pool *pgxpool.Pool, rbacSvc *rbac.Service, userID string) {
	t.Helper()
	if _, err := pool.Exec(ctx,
		`INSERT INTO okt_system.casbin_rule (p_type, v0, v1, v2) VALUES ('g', $1, 'sysadmin', 'system')`,
		userID,
	); err != nil {
		t.Fatalf("seed sysadmin grouping: %v", err)
	}
	if err := rbacSvc.LoadPolicy(); err != nil {
		t.Fatalf("reload RBAC policy: %v", err)
	}
}

// TestAdminTasks_CancelRequiresPermission verifies the
// POST /api/v1/admin/tasks/{id}/cancel endpoint is gated on the
// task:cancel permission. A regular user (no role assigned) gets
// 403; only a sysadmin (or a repoadmin with the seeded
// task:cancel policy) can cancel a stuck River job. This is the
// recovery path for an extract_concepts pass holding a
// transaction for hours because the upstream LLM provider hung.
func TestAdminTasks_CancelRequiresPermission(t *testing.T) {
	createdAt := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)
	cancelRow := makeJobRow(99, "running", createdAt, timePtr(createdAt.Add(time.Second)), nil)

	client := &stubTaskClient{cancelRow: cancelRow}
	server, _, _, _ := tasksEnvWithRBAC(t, client)

	// Regular user: no role, no task:cancel permission.
	regular := newAuthClient(server.URL)
	regular.register("regular-tasks@example.com", "password123", "Regular")
	regular.token = loginUser(regular, "regular-tasks@example.com", "password123")

	resp, body := regular.do("POST", "/api/v1/admin/tasks/99/cancel", nil)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("regular user: expected 403, got %d: %s", resp.StatusCode, body)
	}

	// Also verify GET is gated the same way.
	resp, body = regular.do("GET", "/api/v1/admin/tasks/99", nil)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("regular user GET: expected 403, got %d: %s", resp.StatusCode, body)
	}
}

// TestAdminTasks_CancelSucceeds verifies a sysadmin can cancel a
// stuck River job via POST /api/v1/admin/tasks/{id}/cancel. The
// stub TaskClient returns the cancelled row; the handler must
// return it as JSON with state=cancelled.
func TestAdminTasks_CancelSucceeds(t *testing.T) {
	createdAt := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)
	cancelRow := makeJobRow(77, "cancelled", createdAt, timePtr(createdAt.Add(time.Second)), timePtr(createdAt.Add(2*time.Second)))

	client := &stubTaskClient{cancelRow: cancelRow}
	server, rbacSvc, pool, _ := tasksEnvWithRBAC(t, client)

	admin := newAuthClient(server.URL)
	if r, _ := admin.register("admin-tasks@example.com", "passw0rd!", "Admin"); r.StatusCode != http.StatusCreated {
		t.Fatalf("admin register: status %d", r.StatusCode)
	}
	admin.token = loginUser(admin, "admin-tasks@example.com", "passw0rd!")

	resp, body := admin.do("GET", "/api/v1/users/me", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("admin /users/me: %d: %s", resp.StatusCode, body)
	}
	var me struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(body), &me); err != nil {
		t.Fatalf("decode /users/me: %v", err)
	}

	ctx := context.Background()
	grantSysAdminOnPool(t, ctx, pool, rbacSvc, me.ID)
	// Re-login so the cached role is fresh.
	admin.token = loginUser(admin, "admin-tasks@example.com", "passw0rd!")

	resp, body = admin.do("POST", "/api/v1/admin/tasks/77/cancel", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("admin cancel: expected 200, got %d: %s", resp.StatusCode, body)
	}
	var got jobResponse
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("decode cancel response: %v", err)
	}
	if got.State != "cancelled" {
		t.Errorf("cancel response state = %q, want cancelled", got.State)
	}
	if got.ID != 77 {
		t.Errorf("cancel response id = %d, want 77", got.ID)
	}
}

// TestAdminTasks_GetJobReturnsRow verifies GET
// /api/v1/admin/tasks/{id} returns the same shape as the read-side
// /tasks/{id} endpoint, so operators can inspect a stuck job
// from the admin surface without switching contexts.
func TestAdminTasks_GetJobReturnsRow(t *testing.T) {
	createdAt := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)
	getRow := makeJobRow(123, "running", createdAt, timePtr(createdAt.Add(time.Second)), nil)

	client := &stubTaskClient{getRow: getRow}
	server, rbacSvc, pool, _ := tasksEnvWithRBAC(t, client)

	admin := newAuthClient(server.URL)
	if r, _ := admin.register("admin-get@example.com", "passw0rd!", "Admin"); r.StatusCode != http.StatusCreated {
		t.Fatalf("admin register: status %d", r.StatusCode)
	}
	admin.token = loginUser(admin, "admin-get@example.com", "passw0rd!")
	resp, body := admin.do("GET", "/api/v1/users/me", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("admin /users/me: %d: %s", resp.StatusCode, body)
	}
	var me struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(body), &me); err != nil {
		t.Fatalf("decode /users/me: %v", err)
	}
	ctx := context.Background()
	grantSysAdminOnPool(t, ctx, pool, rbacSvc, me.ID)
	admin.token = loginUser(admin, "admin-get@example.com", "passw0rd!")

	resp, body = admin.do("GET", "/api/v1/admin/tasks/123", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("admin get: expected 200, got %d: %s", resp.StatusCode, body)
	}
	var got jobResponse
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("decode get response: %v", err)
	}
	if got.ID != 123 {
		t.Errorf("get response id = %d, want 123", got.ID)
	}
	if got.State != "running" {
		t.Errorf("get response state = %q, want running", got.State)
	}
}

// TestAdminTasks_NotConfiguredReturns503 verifies the
// notConfigured fallback: when the task manager is not wired,
// the admin task endpoints return 503 (service not configured)
// instead of 500. This is the deployment shape where the API
// boots without River (e.g. a read-only replica).
func TestAdminTasks_NotConfiguredReturns503(t *testing.T) {
	server, rbacSvc, pool, _ := tasksEnvWithRBAC(t, nil)

	admin := newAuthClient(server.URL)
	if r, _ := admin.register("admin-notcfg@example.com", "passw0rd!", "Admin"); r.StatusCode != http.StatusCreated {
		t.Fatalf("admin register: status %d", r.StatusCode)
	}
	admin.token = loginUser(admin, "admin-notcfg@example.com", "passw0rd!")
	resp, body := admin.do("GET", "/api/v1/users/me", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("admin /users/me: %d: %s", resp.StatusCode, body)
	}
	var me struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(body), &me); err != nil {
		t.Fatalf("decode /users/me: %v", err)
	}
	ctx := context.Background()
	grantSysAdminOnPool(t, ctx, pool, rbacSvc, me.ID)
	admin.token = loginUser(admin, "admin-notcfg@example.com", "passw0rd!")

	resp, body = admin.do("POST", "/api/v1/admin/tasks/1/cancel", nil)
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("cancel not-configured: expected 503, got %d: %s", resp.StatusCode, body)
	}
	resp, body = admin.do("GET", "/api/v1/admin/tasks/1", nil)
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("get not-configured: expected 503, got %d: %s", resp.StatusCode, body)
	}
}

// guard against unused imports when the file is edited.
var _ handler.TaskClient = (*stubTaskClient)(nil)
var _ river.EventKind
var _ rivertype.JobState

// TestAdminTasks_RescueStuckJobs verifies POST /admin/tasks/rescue
// resets "running" jobs owned by dead workers back to "available",
// while leaving jobs owned by live workers untouched, and excludes
// jobs with a unique_key. The rescue is based on the
// okt_worker_heartbeat table: a worker with a fresh heartbeat is
// alive; a worker with a stale or missing heartbeat is dead.
func TestAdminTasks_RescueStuckJobs(t *testing.T) {
	server, rbacSvc, pool, _ := tasksEnvWithRBAC(t, &stubTaskClient{})

	// Ensure River schema + the new heartbeat table exist.
	ctx := context.Background()
	if err := ensureRiverSchemaOnPool(pool); err != nil {
		t.Fatalf("ensure river schema: %v", err)
	}

	// Bootstrap a sysadmin so we can call the rescue endpoint.
	admin := newAuthClient(server.URL)
	if r, _ := admin.register("admin-rescue@example.com", "passw0rd!", "Admin"); r.StatusCode != http.StatusCreated {
		t.Fatalf("admin register: status %d", r.StatusCode)
	}
	admin.token = loginUser(admin, "admin-rescue@example.com", "passw0rd!")
	resp, body := admin.do("GET", "/api/v1/users/me", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("admin /users/me: %d: %s", resp.StatusCode, body)
	}
	var me struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(body), &me); err != nil {
		t.Fatalf("decode /users/me: %v", err)
	}
	grantSysAdminOnPool(t, ctx, pool, rbacSvc, me.ID)
	admin.token = loginUser(admin, "admin-rescue@example.com", "passw0rd!")

	// Clean slate for both tables so the test is deterministic.
	if _, err := pool.Exec(ctx, "DELETE FROM river_job"); err != nil {
		t.Fatalf("cleanup river_job: %v", err)
	}
	if _, err := pool.Exec(ctx, "DELETE FROM okt_worker_heartbeat"); err != nil {
		t.Fatalf("cleanup okt_worker_heartbeat: %v", err)
	}

	// Seed heartbeat rows:
	//   "live_worker"  — fresh heartbeat (within 10m threshold)
	//   "dead_worker"  — stale heartbeat (1 hour ago, beyond threshold)
	//   (no row for "ghost_worker" — completely missing)
	now := time.Now().UTC()
	if _, err := pool.Exec(ctx,
		`INSERT INTO okt_worker_heartbeat (worker_id, hostname, started_at, last_heartbeat) VALUES ($1, 'live-host', $2, $2)`,
		"live_worker", now); err != nil {
		t.Fatalf("seed live heartbeat: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO okt_worker_heartbeat (worker_id, hostname, started_at, last_heartbeat) VALUES ($1, 'dead-host', $2, $2)`,
		"dead_worker", now.Add(-1*time.Hour)); err != nil {
		t.Fatalf("seed dead heartbeat: %v", err)
	}

	// Seed river_job rows. Each gets attempted_by set to the
	// owning worker ID (as the last element) so the rescue query's
	// attempted_by[array_length(attempted_by,1)] picks it up.
	seedJob := func(id int64, kind, owner string, uniqueKey []byte) {
		args := []byte("{}")
		_, err := pool.Exec(ctx,
			`INSERT INTO river_job (id, state, attempt, max_attempts, created_at, scheduled_at, args, attempted_by, kind, queue, metadata, unique_key, unique_states, priority)
			 VALUES ($1, 'running', 1, 25, $2, $2, $3, ARRAY[$4], $5, $5, '{}'::jsonb, $6, NULL, 1)`,
			id, now, args, owner, kind, uniqueKey)
		if err != nil {
			t.Fatalf("seed river_job %d: %v", id, err)
		}
	}
	// Job 1: owned by dead_worker, no unique key → SHOULD be rescued.
	seedJob(9001, "extract_concepts", "dead_worker", nil)
	// Job 2: owned by ghost_worker (no heartbeat row), no unique key → SHOULD be rescued.
	seedJob(9002, "summarize_concepts", "ghost_worker", nil)
	// Job 3: owned by live_worker, no unique key → should STAY running.
	seedJob(9003, "extract_concepts", "live_worker", nil)
	// Job 4: owned by dead_worker, WITH unique key → should STAY running (excluded).
	seedJob(9004, "refresh_concept_relations", "dead_worker", []byte("unique-placemark"))

	// Call the rescue endpoint.
	resp, body = admin.do("POST", "/api/v1/admin/tasks/rescue", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("rescue: expected 200, got %d: %s", resp.StatusCode, body)
	}
	var result struct {
		Rescued   int64  `json:"rescued"`
		Threshold string `json:"threshold"`
	}
	if err := json.Unmarshal([]byte(body), &result); err != nil {
		t.Fatalf("decode rescue response: %v: %s", err, body)
	}

	// Two jobs should have been rescued (dead_worker + ghost_worker).
	if result.Rescued != 2 {
		t.Errorf("expected rescued=2, got %d", result.Rescued)
	}

	// Verify job states in the DB.
	checkState := func(id int64, want string) {
		var state string
		if err := pool.QueryRow(ctx, "SELECT state FROM river_job WHERE id = $1", id).Scan(&state); err != nil {
			t.Fatalf("query job %d: %v", id, err)
		}
		if state != want {
			t.Errorf("job %d: state = %q, want %q", id, state, want)
		}
	}
	checkState(9001, "available") // dead worker → rescued
	checkState(9002, "available") // ghost worker → rescued
	checkState(9003, "running")  // live worker → untouched
	checkState(9004, "running")  // unique key → untouched
}

// TestAdminTasks_RescueRequiresPermission verifies the rescue
// endpoint is gated on task:cancel (same as cancel/get). A regular
// user without the role gets 403.
func TestAdminTasks_RescueRequiresPermission(t *testing.T) {
	server, _, _, _ := tasksEnvWithRBAC(t, &stubTaskClient{})

	regular := newAuthClient(server.URL)
	regular.register("regular-rescue@example.com", "password123", "Regular")
	regular.token = loginUser(regular, "regular-rescue@example.com", "password123")

	resp, body := regular.do("POST", "/api/v1/admin/tasks/rescue", nil)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("regular user: expected 403, got %d: %s", resp.StatusCode, body)
	}
}

// TestAdminTasks_RescueNotConfiguredReturns503 verifies the rescue
// endpoint returns 503 when the task manager is not wired (no pool).
func TestAdminTasks_RescueNotConfiguredReturns503(t *testing.T) {
	server, rbacSvc, pool, _ := tasksEnvWithRBAC(t, nil)

	admin := newAuthClient(server.URL)
	if r, _ := admin.register("admin-rescue-nc@example.com", "passw0rd!", "Admin"); r.StatusCode != http.StatusCreated {
		t.Fatalf("admin register: status %d", r.StatusCode)
	}
	admin.token = loginUser(admin, "admin-rescue-nc@example.com", "passw0rd!")
	resp, body := admin.do("GET", "/api/v1/users/me", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("admin /users/me: %d: %s", resp.StatusCode, body)
	}
	var me struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(body), &me); err != nil {
		t.Fatalf("decode /users/me: %v", err)
	}
	ctx := context.Background()
	grantSysAdminOnPool(t, ctx, pool, rbacSvc, me.ID)
	admin.token = loginUser(admin, "admin-rescue-nc@example.com", "passw0rd!")

	resp, body = admin.do("POST", "/api/v1/admin/tasks/rescue", nil)
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("rescue not-configured: expected 503, got %d: %s", resp.StatusCode, body)
	}
}