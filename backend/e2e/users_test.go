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

func TestUsersGetMe(t *testing.T) {
	env := testutil.NewTestEnv(t)
	client := registerTestUser(t, env, "me-test@example.com", "password123", "Me User")

	resp, body := client.do("GET", "/api/v1/users/me", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, body)
	}

	var user struct {
		ID          string `json:"id"`
		Email       string `json:"email"`
		DisplayName string `json:"display_name"`
	}
	json.Unmarshal(body, &user)

	if user.Email != "me-test@example.com" {
		t.Fatalf("expected me-test@example.com, got %s", user.Email)
	}
	if user.DisplayName != "Me User" {
		t.Fatalf("expected 'Me User', got %s", user.DisplayName)
	}
	if user.ID == "" {
		t.Fatal("expected non-empty user ID")
	}
}

func TestUsersGetProfileByID(t *testing.T) {
	env := testutil.NewTestEnv(t)
	client := registerTestUser(t, env, "profile@example.com", "password123", "Profile User")

	resp, body := client.do("GET", "/api/v1/users/me", nil)
	var me struct {
		ID string `json:"id"`
	}
	json.Unmarshal(body, &me)

	resp, body = client.do("GET", "/api/v1/users/"+me.ID, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, body)
	}

	var user struct {
		Email string `json:"email"`
	}
	json.Unmarshal(body, &user)
	if user.Email != "profile@example.com" {
		t.Fatalf("expected profile@example.com, got %s", user.Email)
	}
}

func TestUsersUpdateProfile(t *testing.T) {
	env := testutil.NewTestEnv(t)
	client := registerTestUser(t, env, "update@example.com", "password123", "Old Name")

	resp, body := client.do("GET", "/api/v1/users/me", nil)
	var me struct {
		ID string `json:"id"`
	}
	json.Unmarshal(body, &me)

	updateBody, _ := json.Marshal(map[string]string{
		"display_name": "Updated Display Name",
	})
	resp, body = client.do("PUT", "/api/v1/users/"+me.ID, updateBody)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, body)
	}

	var updated struct {
		DisplayName string `json:"display_name"`
	}
	json.Unmarshal(body, &updated)
	if updated.DisplayName != "Updated Display Name" {
		t.Fatalf("expected 'Updated Display Name', got %s", updated.DisplayName)
	}
}

func TestUsersUpdateProfileEmptyName(t *testing.T) {
	env := testutil.NewTestEnv(t)
	client := registerTestUser(t, env, "empty-name@example.com", "password123", "Original")

	resp, body := client.do("GET", "/api/v1/users/me", nil)
	var me struct {
		ID string `json:"id"`
	}
	json.Unmarshal(body, &me)

	updateBody, _ := json.Marshal(map[string]string{
		"display_name": "",
	})
	resp, _ = client.do("PUT", "/api/v1/users/"+me.ID, updateBody)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
}

func TestUsersUpdateAnotherProfile(t *testing.T) {
	env := testutil.NewTestEnv(t)
	client := newAuthClient(env.BaseURL)

	client.register("attacker@example.com", "password123", "Attacker")
	client.token = loginUser(client, "attacker@example.com", "password123")

	updateBody, _ := json.Marshal(map[string]string{
		"display_name": "Hacked Name",
	})
	resp, _ := client.do("PUT", "/api/v1/users/00000000-0000-0000-0000-000000000001", updateBody)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", resp.StatusCode)
	}
}

func TestUsersGetMeNoAuth(t *testing.T) {
	env := testutil.NewTestEnv(t)
	client := newAuthClient(env.BaseURL)

	resp, _ := client.do("GET", "/api/v1/users/me", nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", resp.StatusCode)
	}
}

func TestUsersGetMeInvalidToken(t *testing.T) {
	env := testutil.NewTestEnv(t)
	client := newAuthClient(env.BaseURL)
	client.token = "deadbeefdeadbeefdeadbeefdeadbeef"

	resp, _ := client.do("GET", "/api/v1/users/me", nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", resp.StatusCode)
	}
}

func TestUsersGetByInvalidUUID(t *testing.T) {
	env := testutil.NewTestEnv(t)
	client := registerTestUser(t, env, "valid@example.com", "password123", "Valid")

	resp, _ := client.do("GET", "/api/v1/users/not-a-valid-uuid", nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", resp.StatusCode)
	}
}

func TestUsersGetNonExistent(t *testing.T) {
	env := testutil.NewTestEnv(t)
	client := registerTestUser(t, env, "exists@example.com", "password123", "Exists")

	resp, _ := client.do("GET", "/api/v1/users/00000000-0000-0000-0000-000000000000", nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", resp.StatusCode)
	}
}

func TestUsersGetOwnPermissionsNoAuth(t *testing.T) {
	env := testutil.NewTestEnv(t)
	client := newAuthClient(env.BaseURL)

	// /api/v1/permissions must require a valid session. Previously the
	// route was mounted without the AuthRequired middleware, so the
	// handler returned 200 with an empty user ID instead of 401.
	resp, _ := client.do("GET", "/api/v1/permissions", nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 for unauthenticated request, got %d", resp.StatusCode)
	}
}

func TestUsersGetOwnPermissionsInvalidToken(t *testing.T) {
	env := testutil.NewTestEnv(t)
	client := newAuthClient(env.BaseURL)
	client.token = "deadbeefdeadbeefdeadbeefdeadbeef"

	resp, _ := client.do("GET", "/api/v1/permissions", nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 for invalid token, got %d", resp.StatusCode)
	}
}

func TestUsersGetOwnPermissionsRegularUser(t *testing.T) {
	env := testutil.NewTestEnv(t)
	client := newAuthClient(env.BaseURL)
	client.register("perms@example.com", "password123", "Perms User")
	client.token = loginUser(client, "perms@example.com", "password123")

	resp, body := client.do("GET", "/api/v1/permissions", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, body)
	}

	var out struct {
		UserID      string `json:"user_id"`
		Permissions []struct {
			Resource string `json:"resource"`
			Action   string `json:"action"`
		} `json:"permissions"`
		SystemAdmin bool `json:"system_admin"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decoding response: %v", err)
	}

	if out.UserID == "" {
		t.Fatal("expected non-empty user_id in response")
	}
	if out.SystemAdmin {
		t.Fatal("expected system_admin=false for a regular user")
	}
	// A user with no assigned roles has no implicit permissions, and the
	// handler returns a nil slice which JSON-encodes to `null`. The
	// frontend treats both null and [] as an empty list, so both are
	// acceptable. We just want to make sure we got a well-formed
	// response, not a server error.
}

func TestUsersGetOwnPermissionsSystemAdmin(t *testing.T) {
	env := testutil.NewTestEnv(t)
	client := newAuthClient(env.BaseURL)

	client.register("admin@example.com", "password123", "Admin User")
	client.token = loginUser(client, "admin@example.com", "password123")

	// Discover the new user's UUID.
	resp, body := client.do("GET", "/api/v1/users/me", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("fetching /users/me: %d: %s", resp.StatusCode, body)
	}
	var me struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(body, &me); err != nil {
		t.Fatalf("decoding /users/me: %v", err)
	}

	// Grant system_admin directly in casbin_rule. This mirrors the
	// production fix and avoids depending on the role-assignment API,
	// which itself requires role:update.
	if _, err := env.DB.Exec(
		context.Background(),
		`INSERT INTO casbin_rule (p_type, v0, v1, v2) VALUES ('g', $1, $2, $3)`,
		me.ID, rbac.RoleSysAdmin, rbac.DomainSystem,
	); err != nil {
		t.Fatalf("seeding sysadmin grouping policy: %v", err)
	}

	// The RBAC service caches policies in memory; reload so the live
	// enforcer picks up the row we just inserted.
	if err := env.RBAC.LoadPolicy(); err != nil {
		t.Fatalf("reloading RBAC policy: %v", err)
	}

	resp, body = client.do("GET", "/api/v1/permissions", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, body)
	}

	var out struct {
		UserID      string `json:"user_id"`
		SystemAdmin bool   `json:"system_admin"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if out.UserID != me.ID {
		t.Fatalf("expected user_id=%s, got %s", me.ID, out.UserID)
	}
	if !out.SystemAdmin {
		t.Fatalf("expected system_admin=true, got body: %s", body)
	}
}
