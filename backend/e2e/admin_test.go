//go:build e2e

package e2e_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/openktree/open-knowledge-tree/backend/e2e/testutil"
	"github.com/openktree/open-knowledge-tree/backend/internal/rbac"
)

func TestAdminListUsers(t *testing.T) {
	env := testutil.NewTestEnv(t)
	admin := bootstrapSysAdmin(t, env, "admin-list@example.com")

	resp, raw := admin.do("GET", "/api/v1/admin/users", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, raw)
	}

	var out struct {
		Users []struct {
			ID          string `json:"id"`
			Email       string `json:"email"`
			DisplayName string `json:"display_name"`
		} `json:"users"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if len(out.Users) == 0 {
		t.Fatal("expected at least one user in list")
	}
}

func TestAdminAssignRole(t *testing.T) {
	env := testutil.NewTestEnv(t)
	admin := bootstrapSysAdmin(t, env, "admin-assign@example.com")

	regular := newAuthClient(env.BaseURL)
	regular.register("assign-target@example.com", "passw0rd!", "Target")
	regular.token = loginUser(regular, "assign-target@example.com", "passw0rd!")
	_, meRaw := regular.do("GET", "/api/v1/users/me", nil)
	var me struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(meRaw, &me)

	body, _ := json.Marshal(map[string]string{
		"user_id":       me.ID,
		"role":          rbac.RoleViewer,
		"repository_id": rbac.DomainAll,
	})
	resp, raw := admin.do("PUT", "/api/v1/admin/users/roles", body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, raw)
	}

	var exists bool
	if err := env.DB.QueryRow(context.Background(),
		`SELECT EXISTS (SELECT 1 FROM casbin_rule WHERE p_type='g' AND v0=$1 AND v1=$2 AND v2=$3)`,
		me.ID, rbac.RoleViewer, rbac.DomainAll,
	).Scan(&exists); err != nil {
		t.Fatalf("query casbin: %v", err)
	}
	if !exists {
		t.Fatal("expected user→viewer casbin link after assign, missing")
	}
}

func TestAdminAssignRoleInvalidRole(t *testing.T) {
	env := testutil.NewTestEnv(t)
	admin := bootstrapSysAdmin(t, env, "admin-invalid@example.com")

	body, _ := json.Marshal(map[string]string{
		"user_id":       "00000000-0000-0000-0000-000000000001",
		"role":          "bogus_role",
		"repository_id": rbac.DomainAll,
	})
	resp, _ := admin.do("PUT", "/api/v1/admin/users/roles", body)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 for invalid role, got %d", resp.StatusCode)
	}
}

func TestAdminAssignRoleSysadmin(t *testing.T) {
	env := testutil.NewTestEnv(t)
	admin := bootstrapSysAdmin(t, env, "admin-sysadmin@example.com")

	regular := newAuthClient(env.BaseURL)
	regular.register("sysadmin-target@example.com", "passw0rd!", "Target")
	regular.token = loginUser(regular, "sysadmin-target@example.com", "passw0rd!")
	_, meRaw := regular.do("GET", "/api/v1/users/me", nil)
	var me struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(meRaw, &me)

	body, _ := json.Marshal(map[string]string{
		"user_id":       me.ID,
		"role":          rbac.RoleSysAdmin,
		"repository_id": rbac.DomainSystem,
	})
	resp, raw := admin.do("PUT", "/api/v1/admin/users/roles", body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, raw)
	}

	var exists bool
	if err := env.DB.QueryRow(context.Background(),
		`SELECT EXISTS (SELECT 1 FROM casbin_rule WHERE p_type='g' AND v0=$1 AND v1=$2 AND v2=$3)`,
		me.ID, rbac.RoleSysAdmin, rbac.DomainSystem,
	).Scan(&exists); err != nil {
		t.Fatalf("query casbin: %v", err)
	}
	if !exists {
		t.Fatal("expected user→sysadmin casbin link after assign, missing")
	}
}

func TestAdminRemoveRole(t *testing.T) {
	env := testutil.NewTestEnv(t)
	admin := bootstrapSysAdmin(t, env, "admin-remove@example.com")

	regular := newAuthClient(env.BaseURL)
	regular.register("remove-target@example.com", "passw0rd!", "Target")
	regular.token = loginUser(regular, "remove-target@example.com", "passw0rd!")
	_, meRaw := regular.do("GET", "/api/v1/users/me", nil)
	var me struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(meRaw, &me)

	assignBody, _ := json.Marshal(map[string]string{
		"user_id":       me.ID,
		"role":          rbac.RoleViewer,
		"repository_id": rbac.DomainAll,
	})
	admin.do("PUT", "/api/v1/admin/users/roles", assignBody)

	removeBody, _ := json.Marshal(map[string]string{
		"user_id":       me.ID,
		"role":          rbac.RoleViewer,
		"repository_id": rbac.DomainAll,
	})
	resp, raw := admin.do("DELETE", "/api/v1/admin/users/roles", removeBody)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, raw)
	}

	var exists bool
	if err := env.DB.QueryRow(context.Background(),
		`SELECT EXISTS (SELECT 1 FROM casbin_rule WHERE p_type='g' AND v0=$1 AND v1=$2 AND v2=$3)`,
		me.ID, rbac.RoleViewer, rbac.DomainAll,
	).Scan(&exists); err != nil {
		t.Fatalf("query casbin: %v", err)
	}
	if exists {
		t.Fatal("expected user→viewer casbin link gone after remove, still present")
	}
}

func TestAdminAssignRoleForbiddenForRegularUser(t *testing.T) {
	env := testutil.NewTestEnv(t)
	regular := newAuthClient(env.BaseURL)
	regular.register("regular-admin@example.com", "passw0rd!", "Regular")
	regular.token = loginUser(regular, "regular-admin@example.com", "passw0rd!")

	body, _ := json.Marshal(map[string]string{
		"user_id":       "00000000-0000-0000-0000-000000000001",
		"role":          rbac.RoleViewer,
		"repository_id": rbac.DomainAll,
	})
	resp, _ := regular.do("PUT", "/api/v1/admin/users/roles", body)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403 for non-sysadmin, got %d", resp.StatusCode)
	}
}

func TestAdminListPermissions(t *testing.T) {
	env := testutil.NewTestEnv(t)
	admin := bootstrapSysAdmin(t, env, "admin-perms@example.com")

	resp, raw := admin.do("GET", "/api/v1/admin/permissions", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, raw)
	}

	var out struct {
		Permissions []struct {
			Resource string `json:"resource"`
			Action   string `json:"action"`
		} `json:"permissions"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if len(out.Permissions) == 0 {
		t.Fatal("expected at least one permission in list")
	}
}
