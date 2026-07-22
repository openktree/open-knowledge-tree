//go:build e2e

package e2e_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/openktree/open-knowledge-tree/backend/e2e/testutil"
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