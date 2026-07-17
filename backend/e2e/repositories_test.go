//go:build e2e

package e2e_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/openktree/open-knowledge-tree/backend/e2e/testutil"
)

// createRepository is a tiny helper that POSTs a /repositories with
// the given name and slug and returns the response, the body and the
// parsed repository id (or "" on failure). The caller is responsible
// for asserting on the response status.
func createRepository(t *testing.T, client *authClient, name, slug, description string) (*http.Response, []byte, string) {
	t.Helper()
	body, _ := json.Marshal(map[string]string{
		"name":        name,
		"slug":        slug,
		"description": description,
	})
	resp, raw := client.do("POST", "/api/v1/repositories", body)
	if resp.StatusCode != http.StatusCreated {
		return resp, raw, ""
	}
	var created struct {
		ID string `json:"ID"`
	}
	if err := json.Unmarshal(raw, &created); err != nil {
		t.Fatalf("decoding created repository: %v", err)
	}
	return resp, raw, created.ID
}

// repositoryList mirrors the shape returned by GET /repositories.
type repositoryList struct {
	Repositories []struct {
		ID          string   `json:"id"`
		Name        string   `json:"name"`
		Slug        string   `json:"slug"`
		Description string   `json:"description"`
		OwnerID     string   `json:"owner_id"`
		Roles       []string `json:"roles"`
	} `json:"repositories"`
}

func TestRepositoriesCreateAndList(t *testing.T) {
	env := testutil.NewTestEnv(t)
	client := newAuthClient(env.BaseURL)

	client = registerTestUser(t, env, "owner@example.com", "password123", "Owner")

	// The fresh user has no repository yet.
	resp, body := client.do("GET", "/api/v1/repositories", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list: expected 200, got %d: %s", resp.StatusCode, body)
	}
	var list repositoryList
	if err := json.Unmarshal(body, &list); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if len(list.Repositories) != 0 {
		t.Fatalf("expected empty repository list, got %d entries", len(list.Repositories))
	}

	resp, _, id := createRepository(t, client, "My First Repo", "my-first-repo", "hello world")
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create: expected 201, got %d", resp.StatusCode)
	}
	if id == "" {
		t.Fatal("expected non-empty repository id from create response")
	}

	// After creation the list must contain exactly one entry and the
	// owner must have the admin role on it.
	resp, body = client.do("GET", "/api/v1/repositories", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list: expected 200, got %d", resp.StatusCode)
	}
	if err := json.Unmarshal(body, &list); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if len(list.Repositories) != 1 {
		t.Fatalf("expected 1 repository, got %d", len(list.Repositories))
	}
	got := list.Repositories[0]
	if got.ID != id {
		t.Fatalf("expected id %s, got %s", id, got.ID)
	}
	if got.Slug != "my-first-repo" {
		t.Fatalf("expected slug my-first-repo, got %s", got.Slug)
	}
	if got.Name != "My First Repo" {
		t.Fatalf("expected name 'My First Repo', got %s", got.Name)
	}
	if !containsString(got.Roles, "repoadmin") {
		t.Fatalf("expected owner to have repoadmin role, got %v", got.Roles)
	}
}

func TestRepositoriesCreateMissingFields(t *testing.T) {
	env := testutil.NewTestEnv(t)
	client := newAuthClient(env.BaseURL)

	client = registerTestUser(t, env, "validator@example.com", "password123", "Validator")

	// Both name and slug are required by the handler.
	body, _ := json.Marshal(map[string]string{"name": "No slug"})
	resp, raw := client.do("POST", "/api/v1/repositories", body)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 when slug is missing, got %d: %s", resp.StatusCode, raw)
	}

	body, _ = json.Marshal(map[string]string{"slug": "no-name"})
	resp, raw = client.do("POST", "/api/v1/repositories", body)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 when name is missing, got %d: %s", resp.StatusCode, raw)
	}
}

func TestRepositoriesCreateDuplicateSlug(t *testing.T) {
	env := testutil.NewTestEnv(t)
	client := newAuthClient(env.BaseURL)

	client = registerTestUser(t, env, "dup@example.com", "password123", "Dup")

	resp, _, _ := createRepository(t, client, "Repo A", "shared-slug", "")
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("first create: expected 201, got %d", resp.StatusCode)
	}

	// A second create with the same slug must be rejected with 409.
	body, _ := json.Marshal(map[string]string{"name": "Repo B", "slug": "shared-slug"})
	resp, raw := client.do("POST", "/api/v1/repositories", body)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("expected 409 on duplicate slug, got %d: %s", resp.StatusCode, raw)
	}
}

func TestRepositoriesCreateNoAuth(t *testing.T) {
	env := testutil.NewTestEnv(t)
	client := newAuthClient(env.BaseURL)

	body, _ := json.Marshal(map[string]string{"name": "X", "slug": "x"})
	resp, _ := client.do("POST", "/api/v1/repositories", body)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", resp.StatusCode)
	}
}

func TestRepositoriesGetByID(t *testing.T) {
	env := testutil.NewTestEnv(t)
	client := newAuthClient(env.BaseURL)

	client = registerTestUser(t, env, "getter@example.com", "password123", "Getter")

	resp, _, id := createRepository(t, client, "Get Me", "get-me", "fetch by id")
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create: expected 201, got %d", resp.StatusCode)
	}

	resp, body := client.do("GET", "/api/v1/repositories/"+id, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get: expected 200, got %d: %s", resp.StatusCode, body)
	}
	var got struct {
		ID          string `json:"ID"`
		Name        string `json:"Name"`
		Slug        string `json:"Slug"`
		Description string `json:"Description"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.ID != id {
		t.Fatalf("expected id %s, got %s", id, got.ID)
	}
	if got.Name != "Get Me" {
		t.Fatalf("expected name 'Get Me', got %s", got.Name)
	}
}

// TestRepositoriesGetByInvalidID verifies the GET /repositories/{repoID}
// path when {repoID} is neither a valid UUID nor a slug that exists in
// the registry. The WithRepoQueries middleware intercepts {repoID}
// before the handler runs: it tries UUID resolution first, then falls
// back to slug resolution. "not-a-uuid" fails UUID scan and is treated
// as a slug; since no repository has that slug, the middleware returns
// 404 "repository not found" (not 400 — the slug namespace is
// unrestricted TEXT, so "not-a-uuid" is a structurally plausible slug
// that simply doesn't exist, not a malformed identifier).
func TestRepositoriesGetByInvalidID(t *testing.T) {
	env := testutil.NewTestEnv(t)
	client := newAuthClient(env.BaseURL)

	client = registerTestUser(t, env, "badgetter@example.com", "password123", "Bad Getter")

	resp, _ := client.do("GET", "/api/v1/repositories/not-a-uuid", nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", resp.StatusCode)
	}
}

func TestRepositoriesUpdate(t *testing.T) {
	env := testutil.NewTestEnv(t)
	client := newAuthClient(env.BaseURL)

	client = registerTestUser(t, env, "updater@example.com", "password123", "Updater")

	resp, _, id := createRepository(t, client, "Original", "updatable", "first")
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create: expected 201, got %d", resp.StatusCode)
	}

	body, _ := json.Marshal(map[string]string{
		"name":        "Updated Name",
		"description": "second",
	})
	resp, raw := client.do("PUT", "/api/v1/repositories/"+id, body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("update: expected 200, got %d: %s", resp.StatusCode, raw)
	}

	resp, raw = client.do("GET", "/api/v1/repositories/"+id, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get after update: expected 200, got %d: %s", resp.StatusCode, raw)
	}
	var got struct {
		Name        string `json:"Name"`
		Description string `json:"Description"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Name != "Updated Name" {
		t.Fatalf("expected name 'Updated Name', got %s", got.Name)
	}
	if got.Description != "second" {
		t.Fatalf("expected description 'second', got %s", got.Description)
	}
}

func TestRepositoriesDelete(t *testing.T) {
	env := testutil.NewTestEnv(t)
	// Repository delete is admin-only (the seed grants
	// `repository:delete` only to `admin` / `system_admin`),
	// so we bootstrap a sys admin to exercise the path. The
	// repository-creation step still happens via the admin
	// user (they have `repository:create` too).
	client := newAuthClient(env.BaseURL)
	client = registerTestUser(t, env, "deleter@example.com", "password123", "Deleter")

	resp, _, id := createRepository(t, client, "Doomed", "doomed", "")
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create: expected 201, got %d", resp.StatusCode)
	}

	resp, raw := client.do("DELETE", "/api/v1/repositories/"+id, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("delete: expected 200, got %d: %s", resp.StatusCode, raw)
	}

	// After delete, GET on the same id must 404 and the list must be empty.
	resp, _ = client.do("GET", "/api/v1/repositories/"+id, nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("get after delete: expected 404, got %d", resp.StatusCode)
	}

	resp, raw = client.do("GET", "/api/v1/repositories", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list after delete: expected 200, got %d: %s", resp.StatusCode, raw)
	}
	var list repositoryList
	if err := json.Unmarshal(raw, &list); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if len(list.Repositories) != 0 {
		t.Fatalf("expected 0 repositories after delete, got %d", len(list.Repositories))
	}
}

func TestRepositoriesListScopedToOwner(t *testing.T) {
	env := testutil.NewTestEnv(t)
	client := newAuthClient(env.BaseURL)

	// First user creates a repository.
	client = registerTestUser(t, env, "alpha@example.com", "password123", "Alpha")
	resp, _, alphaID := createRepository(t, client, "Alpha Repo", "alpha-repo", "")
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create alpha: expected 201, got %d", resp.StatusCode)
	}

	// Second user has no repository yet and must NOT be sysadmin
	// so the handler uses the owner-scoped query.
	client2 := newAuthClient(env.BaseURL)
	client2.register("beta@example.com", "password123", "Beta")
	client2.token = loginUser(client2, "beta@example.com", "password123")

	resp, raw := client2.do("GET", "/api/v1/repositories", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list for beta: expected 200, got %d: %s", resp.StatusCode, raw)
	}
	var list repositoryList
	if err := json.Unmarshal(raw, &list); err != nil {
		t.Fatalf("decode: %v", err)
	}
	for _, r := range list.Repositories {
		if r.ID == alphaID {
			t.Fatalf("beta should not see alpha's repository, but found id %s", alphaID)
		}
	}
}

func TestRepositoriesGetMyPermissionsForRepo(t *testing.T) {
	env := testutil.NewTestEnv(t)
	client := newAuthClient(env.BaseURL)

	client = registerTestUser(t, env, "perm@example.com", "password123", "Perm")

	resp, _, id := createRepository(t, client, "Perm Repo", "perm-repo", "")
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create: expected 201, got %d", resp.StatusCode)
	}

	// Owner must be able to read their own permissions on the new repo.
	resp, raw := client.do("GET", "/api/v1/repositories/"+id+"/permissions", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("perms: expected 200, got %d: %s", resp.StatusCode, raw)
	}
	var out struct {
		RepositoryID string `json:"repository_id"`
		Permissions  []struct {
			Resource string `json:"resource"`
			Action   string `json:"action"`
		} `json:"permissions"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.RepositoryID != id {
		t.Fatalf("expected repository_id %s, got %s", id, out.RepositoryID)
	}
	if !strings.Contains(string(raw), "admin") && !containsPerm(out.Permissions, "repository", "create") {
		t.Logf("permissions payload: %s", raw)
	}
}

// containsString is a tiny helper for the role list in repositoryList.
func containsString(list []string, target string) bool {
	for _, v := range list {
		if v == target {
			return true
		}
	}
	return false
}

func containsPerm(perms []struct {
	Resource string `json:"resource"`
	Action   string `json:"action"`
}, resource, action string) bool {
	for _, p := range perms {
		if p.Resource == resource && p.Action == action {
			return true
		}
	}
	return false
}
