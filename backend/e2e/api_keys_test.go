//go:build e2e

package e2e_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/openktree/open-knowledge-tree/backend/e2e/testutil"
)

// createRepoRetry creates a repository, retrying up to 5 times on 500.
// The CreateRepository handler seeds per-repo settings (providers,
// contexts) in a transaction, and two rapid sequential creates can
// deadlock on the repository_contexts / repository_provider_settings
// rows. This is a pre-existing flakiness in the seeding path (the same
// pattern audit_test.go works around with a 50ms sleep); retrying with
// a backoff is the pragmatic workaround until the seeding is made
// deadlock-safe. Returns "" only after all retries fail (fatal).
func createRepoRetry(t *testing.T, client *authClient, name, slug, desc string) string {
	t.Helper()
	backoff := []time.Duration{50 * time.Millisecond, 150 * time.Millisecond, 300 * time.Millisecond, 500 * time.Millisecond, 1 * time.Second}
	for i := 0; i < len(backoff)+1; i++ {
		resp, _, id := createRepository(t, client, name, slug, desc)
		if resp.StatusCode == http.StatusCreated {
			return id
		}
		if resp.StatusCode == http.StatusInternalServerError && i < len(backoff) {
			time.Sleep(backoff[i])
			continue
		}
		t.Fatalf("create repo %s: unexpected status %d", name, resp.StatusCode)
	}
	t.Fatalf("create repo %s: gave up after retries", name)
	return ""
}

// apiCreateKey POSTs to /users/me/api-keys with the given payload and
// returns (response, body, parsedRawToken). The raw token is only
// returned by the create endpoint; the list endpoint strips it.
func apiCreateKey(t *testing.T, client *authClient, body map[string]any) (*http.Response, []byte, string) {
	t.Helper()
	raw, _ := json.Marshal(body)
	resp, data := client.do("POST", "/api/v1/users/me/api-keys", raw)
	if resp.StatusCode != http.StatusCreated {
		return resp, data, ""
	}
	var out struct {
		Token string `json:"token"`
	}
	_ = json.Unmarshal(data, &out)
	return resp, data, out.Token
}

// apiKeyClient returns an authClient configured to use the given raw
// API key as its bearer token (instead of a session token). Used to
// exercise the APIKeyAuth path.
func apiKeyClient(baseURL, rawKey string) *authClient {
	c := newAuthClient(baseURL)
	c.token = rawKey
	return c
}

func TestAPIKeysCreateListRevoke(t *testing.T) {
	env := testutil.NewTestEnv(t)
	client := registerTestUser(t, env, "apikeys@example.com", "password123", "API Keys User")

	resp, body, raw := apiCreateKey(t, client, map[string]any{
		"name":        "CI ingest",
		"permissions": []string{"source:read", "fact:write"},
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create: expected 201, got %d: %s", resp.StatusCode, body)
	}
	if !strings.HasPrefix(raw, "okt_") {
		t.Fatalf("expected token to start with okt_, got %q", raw)
	}

	var created struct {
		ID          string   `json:"id"`
		Name        string   `json:"name"`
		Prefix      string   `json:"prefix"`
		Permissions []string `json:"permissions"`
	}
	if err := json.Unmarshal(body, &created); err != nil {
		t.Fatalf("decode create response: %v", err)
	}
	if created.Name != "CI ingest" {
		t.Fatalf("expected name 'CI ingest', got %q", created.Name)
	}
	if created.Prefix != raw[:12] {
		t.Fatalf("expected prefix %q, got %q", raw[:12], created.Prefix)
	}

	// List keys. The raw token must NOT be in the response.
	resp, body = client.do("GET", "/api/v1/users/me/api-keys", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list: expected 200, got %d: %s", resp.StatusCode, body)
	}
	var list struct {
		APIKeys []struct {
			ID         string   `json:"id"`
			Name       string   `json:"name"`
			Prefix     string   `json:"prefix"`
			Permissions []string `json:"permissions"`
			TokenHash  string   `json:"token_hash"`
		} `json:"api_keys"`
	}
	if err := json.Unmarshal(body, &list); err != nil {
		t.Fatalf("decode list response: %v", err)
	}
	if len(list.APIKeys) != 1 {
		t.Fatalf("expected 1 key, got %d", len(list.APIKeys))
	}
	if list.APIKeys[0].Name != "CI ingest" {
		t.Fatalf("expected list name 'CI ingest', got %q", list.APIKeys[0].Name)
	}
	if list.APIKeys[0].TokenHash != "" {
		t.Fatalf("token_hash leaked in list response: %q", list.APIKeys[0].TokenHash)
	}

	// Revoke the key.
	resp, body = client.do("DELETE", "/api/v1/users/me/api-keys/"+created.ID, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("revoke: expected 200, got %d: %s", resp.StatusCode, body)
	}

	// The revoked key no longer authenticates.
	keyClient := apiKeyClient(env.BaseURL, raw)
	resp, _ = keyClient.do("GET", "/api/v1/users/me", nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("revoked key: expected 401, got %d", resp.StatusCode)
	}
}

func TestAPIKeyAuthenticatesAndEnforcesScope(t *testing.T) {
	env := testutil.NewTestEnv(t)
	client := registerTestUser(t, env, "scope@example.com", "password123", "Scope User")

	_, _, raw := apiCreateKey(t, client, map[string]any{
		"name":        "read-only",
		"permissions": []string{"source:read"},
	})

	keyClient := apiKeyClient(env.BaseURL, raw)

	// /users/me is gated by AuthRequired only (no perm check), so the
	// key works for it — proving the key authenticates as the user.
	resp, _ := keyClient.do("GET", "/api/v1/users/me", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("key on /users/me: expected 200, got %d", resp.StatusCode)
	}

	// POST /repositories uses h.perm("repository", "write"). The key
	// lacks repository:write, so the key-scope check rejects — even
	// though the user is a sysadmin (sysadmins are bound by their own
	// key's scope, narrower than their full RBAC).
	createBody, _ := json.Marshal(map[string]string{
		"name": "should-fail", "slug": "should-fail", "description": "",
	})
	resp, _ = keyClient.do("POST", "/api/v1/repositories", createBody)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("key without repository:write on POST /repositories: expected 403, got %d", resp.StatusCode)
	}

	// Sanity: the same user WITH a session (not the key) can POST.
	_ = createRepoRetry(t, client, "should-succeed", "should-succeed", "")
}

func TestAPIKeyAllReposWorksOnRepoRoute(t *testing.T) {
	env := testutil.NewTestEnv(t)
	client := registerTestUser(t, env, "allrepos@example.com", "password123", "All Repos User")

	repoID := createRepoRetry(t, client, "APIKey Repo", "apikey-repo", "desc")

	_, _, raw := apiCreateKey(t, client, map[string]any{
		"name":        "all-repos read",
		"permissions": []string{"source:read"},
	})

	keyClient := apiKeyClient(env.BaseURL, raw)

	resp, body := keyClient.doWithHeaders("GET", "/api/v1/repositories/"+repoID+"/sources", nil, map[string]string{
		"X-Repository-ID": repoID,
	})
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		t.Fatalf("all-repos key on repo sources: expected auth+scope ok, got %d: %s", resp.StatusCode, body)
	}
}

func TestAPIKeySingleRepoRejectsOtherRepo(t *testing.T) {
	env := testutil.NewTestEnv(t)
	client := registerTestUser(t, env, "singlerepo@example.com", "password123", "Single Repo User")

	repoA := createRepoRetry(t, client, "Key Repo A", "key-repo-a", "desc")
	repoB := createRepoRetry(t, client, "Key Repo B", "key-repo-b", "desc")

	_, _, raw := apiCreateKey(t, client, map[string]any{
		"name":          "repoA-only",
		"permissions":   []string{"source:read"},
		"repository_id": repoA,
	})

	keyClient := apiKeyClient(env.BaseURL, raw)

	resp, body := keyClient.doWithHeaders("GET", "/api/v1/repositories/"+repoA+"/sources", nil, map[string]string{
		"X-Repository-ID": repoA,
	})
	if resp.StatusCode == http.StatusForbidden {
		t.Fatalf("repoA key on repoA sources: expected pass, got 403: %s", body)
	}

	resp, _ = keyClient.doWithHeaders("GET", "/api/v1/repositories/"+repoB+"/sources", nil, map[string]string{
		"X-Repository-ID": repoB,
	})
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("repoA key on repoB sources: expected 403, got %d", resp.StatusCode)
	}
}

func TestAPIKeyRejectsInvalidPermissions(t *testing.T) {
	env := testutil.NewTestEnv(t)
	client := registerTestUser(t, env, "invalid@example.com", "password123", "Invalid User")

	resp, _, _ := apiCreateKey(t, client, map[string]any{
		"name":        "bad",
		"permissions": []string{"bogus:read"},
	})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 for unknown resource, got %d", resp.StatusCode)
	}

	resp, _, _ = apiCreateKey(t, client, map[string]any{
		"name":        "bad",
		"permissions": []string{"source:frob"},
	})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 for unknown action, got %d", resp.StatusCode)
	}

	resp, _, _ = apiCreateKey(t, client, map[string]any{
		"name":        "bad",
		"permissions": []string{"source"},
	})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 for malformed entry, got %d", resp.StatusCode)
	}
}

func TestAPIKeyRejectsRepoWithoutAccess(t *testing.T) {
	env := testutil.NewTestEnv(t)
	// owner creates a repo; outsider has no access to it.
	owner := registerTestUser(t, env, "owner2@example.com", "password123", "Owner")
	repoID := createRepoRetry(t, owner, "Owned Repo", "owned-repo", "desc")

	// outsider is a plain user (no role granted).
	outsider := newAuthClient(env.BaseURL)
	outsider.register("outsider@example.com", "password123", "Outsider")
	outsider.token = loginUser(outsider, "outsider@example.com", "password123")

	resp, _, _ := apiCreateKey(t, outsider, map[string]any{
		"name":          "no-access",
		"permissions":   []string{"source:read"},
		"repository_id": repoID,
	})
	// 403 (no read access) is the expected path. 400 is also
	// acceptable: a non-sysadmin without any role may fail an
	// earlier RBAC check in the handler. Either way the key is NOT
	// created. The bug we're guarding against is 201 (key created
	// for a repo the user can't see).
	if resp.StatusCode != http.StatusForbidden && resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 403 or 400 for key on repo without access, got %d", resp.StatusCode)
	}
}

func TestAPIKeyMissingName(t *testing.T) {
	env := testutil.NewTestEnv(t)
	client := registerTestUser(t, env, "noname@example.com", "password123", "No Name User")

	resp, _, _ := apiCreateKey(t, client, map[string]any{
		"permissions": []string{"source:read"},
	})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 for missing name, got %d", resp.StatusCode)
	}
}

func TestAPIKeyRequiresSession(t *testing.T) {
	env := testutil.NewTestEnv(t)
	client := newAuthClient(env.BaseURL)
	resp, _ := client.do("GET", "/api/v1/users/me/api-keys", nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 without session, got %d", resp.StatusCode)
	}
}

func TestAPIKeyCannotRevokeOthersKey(t *testing.T) {
	env := testutil.NewTestEnv(t)
	owner := registerTestUser(t, env, "keyowner@example.com", "password123", "Key Owner")
	_, _, raw := apiCreateKey(t, owner, map[string]any{
		"name":        "owner key",
		"permissions": []string{"source:read"},
	})

	resp, body := owner.do("GET", "/api/v1/users/me/api-keys", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("owner list: expected 200, got %d", resp.StatusCode)
	}
	var list struct {
		APIKeys []struct {
			ID string `json:"id"`
		} `json:"api_keys"`
	}
	_ = json.Unmarshal(body, &list)
	if len(list.APIKeys) == 0 {
		t.Fatal("owner has no keys")
	}
	keyID := list.APIKeys[0].ID

	attacker := registerTestUser(t, env, "attacker@example.com", "password123", "Attacker")
	resp, _ = attacker.do("DELETE", "/api/v1/users/me/api-keys/"+keyID, nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404 for revoking another user's key, got %d", resp.StatusCode)
	}

	keyClient := apiKeyClient(env.BaseURL, raw)
	resp, _ = keyClient.do("GET", "/api/v1/users/me", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("key should still work after attacker revoke attempt, got %d", resp.StatusCode)
	}
}

func TestAPIKeyExpiry(t *testing.T) {
	env := testutil.NewTestEnv(t)
	client := registerTestUser(t, env, "expiry@example.com", "password123", "Expiry User")

	_, body, raw := apiCreateKey(t, client, map[string]any{
		"name":            "short-lived",
		"permissions":     []string{"source:read"},
		"expires_in_days": 1,
	})
	var created struct {
		ExpiresAt string `json:"expires_at"`
	}
	_ = json.Unmarshal(body, &created)
	if created.ExpiresAt == "" {
		t.Fatal("expected non-empty expires_at")
	}

	resp, listBody := client.do("GET", "/api/v1/users/me/api-keys", nil)
	var list struct {
		APIKeys []struct {
			ID string `json:"id"`
		} `json:"api_keys"`
	}
	_ = json.Unmarshal(listBody, &list)
	if resp.StatusCode != http.StatusOK || len(list.APIKeys) == 0 {
		t.Fatal("no keys to expire")
	}
	keyID := list.APIKeys[0].ID

	if _, err := env.DB.Exec(context.Background(), `UPDATE api_keys SET expires_at = now() - interval '1 minute' WHERE id = $1`, keyID); err != nil {
		t.Fatalf("expiring key: %v", err)
	}

	keyClient := apiKeyClient(env.BaseURL, raw)
	resp, _ = keyClient.do("GET", "/api/v1/users/me", nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expired key: expected 401, got %d", resp.StatusCode)
	}
}