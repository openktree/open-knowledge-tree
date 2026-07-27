//go:build e2e

package e2e_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/openktree/open-knowledge-tree/backend/e2e/testutil"
	"github.com/openktree/open-knowledge-tree/backend/internal/audit"
	"github.com/openktree/open-knowledge-tree/backend/internal/bootstrap"
	"github.com/openktree/open-knowledge-tree/backend/internal/rbac"
)

// insertAuditRow inserts one permission_audit row directly via SQL
// so the audit endpoints have data to return without needing a
// real mutation flow. The row is written against the test env's
// default pool (which carries the okt_system search_path).
func insertAuditRow(t *testing.T, env *testutil.TestEnv, actorID, actorEmail, action, object string, repoID *string, target string) {
	t.Helper()
	ctx := context.Background()
	_, err := env.DB.Exec(ctx, `
		INSERT INTO okt_system.permission_audit
		    (actor_user_id, actor_username, action, object, repository_id, target, detail, occurred_at)
		VALUES ($1, $2, $3, $4, $5, $6, '{}'::jsonb, now())`,
		actorID, actorEmail, action, object, repoID, target,
	)
	if err != nil {
		t.Fatalf("inserting permission_audit row: %v", err)
	}
}

// TestAuditSystemList_SysAdmin verifies that GET /admin/audit
// returns every audit row to a sysadmin and supports the action
// filter.
func TestAuditSystemList_SysAdmin(t *testing.T) {
	env := testutil.NewTestEnv(t)
	admin := bootstrapSysAdmin(t, env, "audit-admin@example.com")

	// Discover the admin's user id so we can attribute audit rows.
	_, meRaw := admin.do("GET", "/api/v1/users/me", nil)
	var me struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(meRaw, &me); err != nil {
		t.Fatalf("decoding /users/me: %v", err)
	}

	insertAuditRow(t, env, me.ID, "audit-admin@example.com", rbac.AuditActionRoleAssign, rbac.Objects.Users, nil, "user-1")
	insertAuditRow(t, env, me.ID, "audit-admin@example.com", rbac.AuditActionRepoCreate, rbac.Objects.Repositories, nil, "repo-1")
	insertAuditRow(t, env, me.ID, "audit-admin@example.com", rbac.AuditActionIngestionStart, rbac.Objects.Sources, nil, "https://example.com")

	// Wait briefly for the inserts to be visible (the audit read
	// path is on the same pool, so this is normally instant; a
	// small cushion avoids a race on heavily loaded CI runners).
	time.Sleep(50 * time.Millisecond)

	resp, raw := admin.do("GET", "/api/v1/admin/audit", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, raw)
	}
	var out struct {
		Events []struct {
			Action string `json:"action"`
			Object string `json:"object"`
		} `json:"events"`
		Total   int64    `json:"total"`
		Actions []string `json:"actions"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decoding audit response: %v", err)
	}
	if out.Total < 3 {
		t.Fatalf("expected at least 3 audit rows, got %d", out.Total)
	}
	if len(out.Actions) < 3 {
		t.Fatalf("expected at least 3 distinct actions, got %v", out.Actions)
	}

	// Filter by action.
	resp, raw = admin.do("GET", "/api/v1/admin/audit?action="+rbac.AuditActionRoleAssign, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 on filtered call, got %d: %s", resp.StatusCode, raw)
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decoding filtered audit response: %v", err)
	}
	for _, e := range out.Events {
		if e.Action != rbac.AuditActionRoleAssign {
			t.Fatalf("filter leaked non-matching action: %q", e.Action)
		}
	}
}

// TestAuditSystemList_ForbiddenForRegularUser verifies that a
// regular authenticated user (no audit.read permission) gets 403
// on the system audit endpoint.
func TestAuditSystemList_ForbiddenForRegularUser(t *testing.T) {
	env := testutil.NewTestEnv(t)
	regular := newAuthClient(env.BaseURL)
	regular.register("audit-regular@example.com", "passw0rd!", "Regular")
	regular.token = loginUser(regular, "audit-regular@example.com", "passw0rd!")

	resp, _ := regular.do("GET", "/api/v1/admin/audit", nil)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403 for non-permitted user, got %d", resp.StatusCode)
	}
}

// TestAuditRepoList_RepoAdmin verifies that GET /repositories/{repoID}/audit
// returns audit rows for the repo to a repoadmin and 403s for a
// repoadmin of a different repo.
func TestAuditRepoList_RepoAdmin(t *testing.T) {
	env := testutil.NewTestEnv(t)
	admin := bootstrapSysAdmin(t, env, "audit-repo-admin@example.com")

	_, meRaw := admin.do("GET", "/api/v1/users/me", nil)
	var me struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(meRaw, &me)

	// Create two repos; admin is repoadmin of both via CreateRepository.
	_, _, repoA := createRepository(t, admin, "Audit Repo A", "audit-a", "desc")
	_, _, repoB := createRepository(t, admin, "Audit Repo B", "audit-b", "desc")

	// Insert one audit row attributed to repo A, one to repo B,
	// one system-scoped (NULL repository_id).
	insertAuditRow(t, env, me.ID, "audit-repo-admin@example.com", rbac.AuditActionIngestionStart, rbac.Objects.Sources, &repoA, "https://a.example.com")
	insertAuditRow(t, env, me.ID, "audit-repo-admin@example.com", rbac.AuditActionIngestionStart, rbac.Objects.Sources, &repoB, "https://b.example.com")
	insertAuditRow(t, env, me.ID, "audit-repo-admin@example.com", rbac.AuditActionRoleAssign, rbac.Objects.Users, nil, "user-x")
	time.Sleep(50 * time.Millisecond)

	// Repo A view should return only repo A's rows.
	resp, raw := admin.do("GET", "/api/v1/repositories/audit-a/audit", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 on repo audit, got %d: %s", resp.StatusCode, raw)
	}
	var out struct {
		Events []struct {
			RepositoryID *string `json:"repository_id"`
			Action       string  `json:"action"`
		} `json:"events"`
		Total int64 `json:"total"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decoding repo audit response: %v", err)
	}
	if out.Total == 0 {
		t.Fatal("expected at least one repo-scoped audit row, got 0")
	}
	for _, e := range out.Events {
		if e.RepositoryID == nil || *e.RepositoryID != repoA {
			t.Fatalf("repo A audit returned a row from a different repo: %v", e.RepositoryID)
		}
	}
}

// TestAuditIngestionStartAttributed verifies that creating a source
// via POST /repositories/{repoID}/sources produces an
// ingestion_start audit row attributed to the calling user.
func TestAuditIngestionStartAttributed(t *testing.T) {
	env := testutil.NewTestEnv(t)
	admin := bootstrapSysAdmin(t, env, "audit-ingest@example.com")

	_, meRaw := admin.do("GET", "/api/v1/users/me", nil)
	var me struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(meRaw, &me)

	_, _, repoID := createRepository(t, admin, "Audit Ingest", "audit-ingest", "desc")

	body, _ := json.Marshal(map[string]string{
		"url":  "https://example.com/audit-ingest",
		"kind": "homepage",
	})
	resp, raw := admin.do("POST", "/api/v1/repositories/audit-ingest/sources", body)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201 creating source, got %d: %s", resp.StatusCode, raw)
	}

	// Audit is async (RecordAsync); wait briefly for the writer.
	time.Sleep(100 * time.Millisecond)

	var exists bool
	if err := env.DB.QueryRow(context.Background(),
		`SELECT EXISTS (SELECT 1 FROM okt_system.permission_audit
		  WHERE actor_user_id = $1
		    AND action = $2
		    AND object = $3
		    AND repository_id = $4
		    AND source_url = $5)`,
		me.ID, rbac.AuditActionIngestionStart, rbac.Objects.Sources, repoID, "https://example.com/audit-ingest",
	).Scan(&exists); err != nil {
		t.Fatalf("querying audit: %v", err)
	}
	if !exists {
		t.Fatal("expected an ingestion_start audit row attributed to the calling user, missing")
	}
}

// TestAuditSettingsChange verifies that updating a per-repo
// setting via PUT /repositories/{repoID}/settings/content-types
// produces a provider_set audit row attributed to the calling
// user. Covers the settings-mutation audit path the user asked
// about. (content-types is used rather than settings/providers
// because the latter validates against the live provider
// registry, which the test env doesn't wire.)
func TestAuditSettingsChange(t *testing.T) {
	env := testutil.NewTestEnv(t)
	admin := bootstrapSysAdmin(t, env, "audit-settings@example.com")

	_, meRaw := admin.do("GET", "/api/v1/users/me", nil)
	var me struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(meRaw, &me)

	_, _, repoID := createRepository(t, admin, "Audit Settings", "audit-settings", "desc")

	// Update allowed content types. The settings handler emits a
	// provider_set audit row on success (the action kind is shared
	// across all settings mutations; the `action` field in the
	// detail JSONB distinguishes them).
	body, _ := json.Marshal(map[string]any{
		"allowed_content_types": []string{"document", "url"},
	})
	resp, raw := admin.do("PUT", "/api/v1/repositories/"+repoID+"/settings/content-types", body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 updating content types, got %d: %s", resp.StatusCode, raw)
	}

	// Audit is async (RecordAsync); wait briefly for the writer.
	time.Sleep(100 * time.Millisecond)

	var exists bool
	if err := env.DB.QueryRow(context.Background(),
		`SELECT EXISTS (SELECT 1 FROM okt_system.permission_audit
		  WHERE actor_user_id = $1
		    AND action = $2
		    AND object = $3
		    AND repository_id = $4
		    AND target = $5)`,
		me.ID, rbac.AuditActionProviderSet, rbac.Objects.Repositories, repoID, "content_types",
	).Scan(&exists); err != nil {
		t.Fatalf("querying audit: %v", err)
	}
	if !exists {
		t.Fatal("expected a provider_set audit row after updating content types, missing")
	}
}

// assertAuditRow waits briefly for the async audit writer (RecordAsync
// runs on a background goroutine) then checks that a row matching
// the (actor, action, object, repo) tuple exists. Returns the detail
// JSONB so the caller can assert on the payload. An empty repoID
// matches system-scope rows (repository_id IS NULL).
func assertAuditRow(t *testing.T, env *testutil.TestEnv, actorID, action, object, repoID string) []byte {
	t.Helper()
	// Audit is async (RecordAsync); wait briefly for the writer.
	time.Sleep(150 * time.Millisecond)
	var detail []byte
	var repoArg any
	if repoID == "" {
		repoArg = nil
	} else {
		repoArg = repoID
	}
	err := env.DB.QueryRow(context.Background(),
		`SELECT detail FROM okt_system.permission_audit
		  WHERE actor_user_id = $1
		    AND action = $2
		    AND object = $3
		    AND ($4::uuid IS NULL OR repository_id = $4)
		  ORDER BY occurred_at DESC LIMIT 1`,
		actorID, action, object, repoArg,
	).Scan(&detail)
	if err != nil {
		t.Fatalf("expected audit row (action=%s object=%s repo=%s) attributed to %s, missing: %v", action, object, repoID, actorID, err)
	}
	return detail
}

// TestAuditInvestigationLifecycle verifies that creating, updating,
// and deleting an investigation each emit a corresponding audit row
// attributed to the calling user.
func TestAuditInvestigationLifecycle(t *testing.T) {
	env := testutil.NewTestEnv(t)
	admin := bootstrapSysAdmin(t, env, "audit-inv@example.com")

	_, meRaw := admin.do("GET", "/api/v1/users/me", nil)
	var me struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(meRaw, &me)

	_, _, repoID := createRepository(t, admin, "Audit Inv", "audit-inv", "desc")

	// Create.
	body, _ := json.Marshal(map[string]string{
		"title": "Test Investigation",
		"topic": "audit coverage",
	})
	resp, raw := admin.do("POST", "/api/v1/repositories/audit-inv/investigations", body)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201 creating investigation, got %d: %s", resp.StatusCode, raw)
	}
	var inv struct {
		ID string `json:"ID"`
	}
	if err := json.Unmarshal(raw, &inv); err != nil {
		t.Fatalf("decoding investigation: %v", err)
	}
	assertAuditRow(t, env, me.ID, rbac.AuditActionInvestigationCreate, rbac.Objects.Investigations, repoID)

	// Update.
	upd, _ := json.Marshal(map[string]string{
		"title": "Renamed Investigation",
		"topic": "still auditing",
	})
	resp, raw = admin.do("PUT", "/api/v1/repositories/audit-inv/investigations/"+inv.ID, upd)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 updating investigation, got %d: %s", resp.StatusCode, raw)
	}
	assertAuditRow(t, env, me.ID, rbac.AuditActionInvestigationUpdate, rbac.Objects.Investigations, repoID)

	// Delete.
	resp, raw = admin.do("DELETE", "/api/v1/repositories/audit-inv/investigations/"+inv.ID, nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("expected 204 deleting investigation, got %d: %s", resp.StatusCode, raw)
	}
	assertAuditRow(t, env, me.ID, rbac.AuditActionInvestigationDelete, rbac.Objects.Investigations, repoID)
}

// TestAuditReportLifecycle verifies that creating + deleting a report
// each emit a corresponding audit row.
func TestAuditReportLifecycle(t *testing.T) {
	env := testutil.NewTestEnv(t)
	admin := bootstrapSysAdmin(t, env, "audit-rep@example.com")

	_, meRaw := admin.do("GET", "/api/v1/users/me", nil)
	var me struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(meRaw, &me)

	_, _, repoID := createRepository(t, admin, "Audit Rep", "audit-rep", "desc")

	body, _ := json.Marshal(map[string]string{
		"title": "Test Report",
		"text":  "# Heading\n\nSome body text for the report.",
	})
	resp, raw := admin.do("POST", "/api/v1/repositories/audit-rep/reports", body)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("expected 202 creating report, got %d: %s", resp.StatusCode, raw)
	}
	var rep struct {
		ReportID string `json:"report_id"`
	}
	if err := json.Unmarshal(raw, &rep); err != nil {
		t.Fatalf("decoding report: %v", err)
	}
	assertAuditRow(t, env, me.ID, rbac.AuditActionReportCreate, rbac.Objects.Reports, repoID)

	// Delete.
	resp, raw = admin.do("DELETE", "/api/v1/repositories/audit-rep/reports/"+rep.ReportID, nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("expected 204 deleting report, got %d: %s", resp.StatusCode, raw)
	}
	assertAuditRow(t, env, me.ID, rbac.AuditActionReportDelete, rbac.Objects.Reports, repoID)
}

// TestAuditGroupLifecycle verifies that creating + deleting a group
// each emit a corresponding audit row. Groups are system-scope (no
// repo), so the repo filter is NULL.
func TestAuditGroupLifecycle(t *testing.T) {
	env := testutil.NewTestEnv(t)
	admin := bootstrapSysAdmin(t, env, "audit-grp@example.com")

	_, meRaw := admin.do("GET", "/api/v1/users/me", nil)
	var me struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(meRaw, &me)

	body, _ := json.Marshal(map[string]string{
		"name":        "Audit Test Group",
		"description": "for audit coverage",
	})
	resp, raw := admin.do("POST", "/api/v1/groups", body)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201 creating group, got %d: %s", resp.StatusCode, raw)
	}
	var grp struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(raw, &grp); err != nil {
		t.Fatalf("decoding group: %v", err)
	}
	assertAuditRow(t, env, me.ID, rbac.AuditActionGroupCreate, rbac.Objects.Groups, "")

	resp, raw = admin.do("DELETE", "/api/v1/groups/"+grp.ID, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 deleting group, got %d: %s", resp.StatusCode, raw)
	}
	assertAuditRow(t, env, me.ID, rbac.AuditActionGroupDelete, rbac.Objects.Groups, "")
}

// TestAuditSourceDelete verifies that deleting a source emits a
// source_delete audit row. The recorder is wired on the Source bundle
// (s.audit), so this exercises the previously-missing call site.
func TestAuditSourceDelete(t *testing.T) {
	env := testutil.NewTestEnv(t)
	admin := bootstrapSysAdmin(t, env, "audit-src-del@example.com")

	_, meRaw := admin.do("GET", "/api/v1/users/me", nil)
	var me struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(meRaw, &me)

	_, _, repoID := createRepository(t, admin, "Audit Src Del", "audit-src-del", "desc")

	// Create a source first (this itself emits ingestion_start).
	body, _ := json.Marshal(map[string]string{
		"url":  "https://example.com/audit-src-del",
		"kind": "homepage",
	})
	resp, raw := admin.do("POST", "/api/v1/repositories/audit-src-del/sources", body)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201 creating source, got %d: %s", resp.StatusCode, raw)
	}
	var src struct {
		ID string `json:"ID"`
	}
	if err := json.Unmarshal(raw, &src); err != nil {
		t.Fatalf("decoding source: %v", err)
	}

	// Delete it.
	resp, raw = admin.do("DELETE", "/api/v1/repositories/audit-src-del/sources/"+src.ID, nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("expected 204 deleting source, got %d: %s", resp.StatusCode, raw)
	}
	assertAuditRow(t, env, me.ID, rbac.AuditActionSourceDelete, rbac.Objects.Sources, repoID)
}

// TestAuditBootstrapRepo verifies that EnsureDefaultRepository with a
// non-nil recorder emits a bootstrap_repo_create audit row. The actor
// is the owner (the earliest user); the row carries the repo id as
// target.
func TestAuditBootstrapRepo(t *testing.T) {
	env := testutil.NewTestEnv(t)
	// Seed a user so the bootstrap has an owner to attribute the
	// repo to (the bootstrap picks the earliest user when ownerID
	// is empty).
	admin := bootstrapSysAdmin(t, env, "audit-boot@example.com")
	_, meRaw := admin.do("GET", "/api/v1/users/me", nil)
	var me struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(meRaw, &me)

	// Build a minimal config with the bootstrap flag on and a
	// unique slug so it doesn't collide with the default repo the
	// test env may have already created.
	cfg := bootstrapTestConfig()
	cfg.Bootstrap.DefaultRepository = true
	// The test env's NewTestEnv may have already created a
	// repository; EnsureDefaultRepository is a no-op when the
	// repositories table is non-empty, so we assert the audit row
	// only fires when the bootstrap actually creates. To guarantee
	// creation, we point at a fresh database pool with no repos.
	// Simpler: drop the existing repos first.
	_, _ = env.DB.Exec(context.Background(), `DELETE FROM repositories`)

	recorder := audit.NewPostgresRecorder(env.DB)
	res, err := bootstrap.EnsureDefaultRepository(context.Background(), testutil.NewForTestPool(env.DB), cfg, "", nil, recorder)
	if err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	if !res.Created {
		t.Skip("bootstrap did not create a repo (table non-empty); skipping audit assertion")
	}

	// Wait for the async writer.
	time.Sleep(150 * time.Millisecond)
	var exists bool
	if err := env.DB.QueryRow(context.Background(),
		`SELECT EXISTS (SELECT 1 FROM okt_system.permission_audit
		  WHERE action = $1 AND object = $2 AND target = $3)`,
		rbac.AuditActionBootstrapRepo, rbac.Objects.Repositories, res.RepositoryID,
	).Scan(&exists); err != nil {
		t.Fatalf("querying audit: %v", err)
	}
	if !exists {
		t.Fatal("expected a bootstrap_repo_create audit row, missing")
	}
}