//go:build e2e

package e2e_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/openktree/open-knowledge-tree/backend/e2e/testutil"
	"github.com/openktree/open-knowledge-tree/backend/internal/rbac"
)

// loginUser logs in and returns the JWT token.
func loginUser(client *authClient, email, password string) string {
	_, body := client.login(email, password)
	var resp struct {
		Token string `json:"token"`
	}
	json.Unmarshal(body, &resp)
	return resp.Token
}

// registerTestUser registers a user, logs in, fetches their UUID,
// grants them sysadmin on all domains, reloads RBAC, and re-logins
// to get a fresh JWT. Returns the authenticated client.
//
// Every test that needs a user with real permissions should use this
// instead of manual register+login, since registration no longer
// assigns a default role.
func registerTestUser(t *testing.T, env *testutil.TestEnv, email, password, displayName string) *authClient {
	t.Helper()
	client := newAuthClient(env.BaseURL)

	// Register.
	resp, _ := client.register(email, password, displayName)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("register %s: status %d", email, resp.StatusCode)
	}

	// Login.
	client.token = loginUser(client, email, password)

	// Fetch UUID.
	resp, body := client.do("GET", "/api/v1/users/me", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /users/me: status %d", resp.StatusCode)
	}
	var me struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(body, &me); err != nil {
		t.Fatalf("decode /users/me: %v", err)
	}

	// Grant sysadmin on all domains and on the system domain.
	// DomainAll ("*") covers repository-scoped RBAC checks;
	// DomainSystem ("system") is needed for EnforceSystemAdmin
	// which checks that exact domain string.
	testutil.GrantUserRole(t, env, me.ID, rbac.RoleSysAdmin, rbac.DomainAll)
	testutil.GrantUserRole(t, env, me.ID, rbac.RoleSysAdmin, rbac.DomainSystem)

	// Re-login to get a fresh JWT carrying the new role.
	client.token = loginUser(client, email, password)
	return client
}

// getMeUUID fetches the UUID of the currently-authenticated user.
func getMeUUID(client *authClient) string {
	resp, body := client.do("GET", "/api/v1/users/me", nil)
	if resp.StatusCode != http.StatusOK {
		panic("getMeUUID: /users/me returned " + resp.Status)
	}
	var me struct {
		ID string `json:"id"`
	}
	json.Unmarshal(body, &me)
	return me.ID
}
