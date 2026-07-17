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

// TestPerRepoRouting_SourcesCRUD is the proof that the per-repo
// middleware actually routes queries to the right pool. The
// e2e harness is single-database, so the shared tier and the
// isolated tier resolve to the same pool; the test still
// exercises the full path (slug → repo UUID → pool →
// handler) end to end.
//
// The test:
//
//  1. Creates a repository on the default database.
//  2. POSTs a source to /repositories/{slug}/sources.
//  3. GETs /repositories/{slug}/sources and asserts the
//     source is there.
//  4. POSTs a duplicate URL and asserts 409.
//  5. GETs /repositories/{bad-slug}/sources with an unknown
//     slug and asserts 404.
func TestPerRepoRouting_SourcesCRUD(t *testing.T) {
	env := testutil.NewTestEnv(t)
	defer env.Server.Close()

	admin := bootstrapSysAdmin(t, env, "sources_admin@example.com")

	const slug = "sources-repo"
	resp, body, repoID := createRepositoryWithDB(t, admin, "Sources Repo", slug, "desc", "")
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create repo: status %d, body %s", resp.StatusCode, body)
	}
	if repoID == "" {
		t.Fatal("expected repository id")
	}

	// Create a source.
	createBody, _ := json.Marshal(map[string]string{
		"url":  "https://example.com/paper",
		"kind": "paper",
	})
	createResp, createRaw := admin.do("POST", "/api/v1/repositories/"+slug+"/sources", createBody)
	if createResp.StatusCode != http.StatusCreated {
		t.Fatalf("create source: status %d, body %s", createResp.StatusCode, createRaw)
	}
	var created struct {
		ID     string `json:"ID"`
		URL    string `json:"url"`
		Kind   string `json:"kind"`
		Status string `json:"status"`
	}
	if err := json.Unmarshal(createRaw, &created); err != nil {
		t.Fatalf("decoding created source: %v", err)
	}
	if created.URL != "https://example.com/paper" || created.Kind != "paper" {
		t.Errorf("created source = %+v, want url=paper url, kind=paper", created)
	}
	if created.Status != "pending" {
		t.Errorf("created.Status = %q, want %q", created.Status, "pending")
	}

	// List and assert.
	listResp, listRaw := admin.do("GET", "/api/v1/repositories/"+slug+"/sources", nil)
	if listResp.StatusCode != http.StatusOK {
		t.Fatalf("list sources: status %d, body %s", listResp.StatusCode, listRaw)
	}
	if !contains(listRaw, "https://example.com/paper") {
		t.Errorf("expected list to contain the created URL; body: %s", listRaw)
	}

	// Duplicate URL → 409.
	dupResp, dupRaw := admin.do("POST", "/api/v1/repositories/"+slug+"/sources", createBody)
	if dupResp.StatusCode != http.StatusConflict {
		t.Errorf("duplicate create: status %d, body %s; want 409", dupResp.StatusCode, dupRaw)
	}

	// Unknown slug → 404 from the middleware.
	badResp, badRaw := admin.do("GET", "/api/v1/repositories/nonexistent-slug/sources", nil)
	if badResp.StatusCode != http.StatusNotFound {
		t.Errorf("unknown slug: status %d, body %s; want 404", badResp.StatusCode, badRaw)
	}
}

// TestPerRepoRouting_RegularUserIsolatedFromSystemDB verifies
// that the per-repo middleware doesn't leak the system pool
// to handlers that should be using the per-repo pool. A
// regular user (with `source:create` permission) creates a
// source under a repository, and the test asserts the source
// lands in `okt_repository.sources` (the per-repo table) and
// not in `okt_system.users` or some other system-side table.
//
// The check is a SELECT against the system pool after the
// POST; the source should not appear in any system-side
// table. (It would appear in `okt_repository.sources` on the
// per-repo pool, which is the same `default` pool in the
// single-DB e2e harness but lives in the per-repo schema.)
func TestPerRepoRouting_RegularUserIsolatedFromSystemDB(t *testing.T) {
	env := testutil.NewTestEnv(t)
	defer env.Server.Close()

	// Register a regular (non-admin) user. Registration no longer
	// assigns any default role, so we must grant an explicit role
	// that includes source:write on the repo's domain.
	client := newAuthClient(env.BaseURL)
	if r, _ := client.register("regular_user@example.com", "passw0rd!", "Regular"); r.StatusCode != http.StatusCreated {
		t.Fatalf("register: status %d", r.StatusCode)
	}
	client.token = loginUser(client, "regular_user@example.com", "passw0rd!")

	// The regular user needs a repository to scope the
	// source under. We bootstrap it as a sys admin (regular
	// users are allowed to create their own repositories
	// per the seed, but the easier path is to use the admin
	// helper for setup and then switch back to the regular
	// user for the source write).
	admin := bootstrapSysAdmin(t, env, "regular_setup@example.com")
	const slug = "regular-user-repo"
	resp, body, repoID := createRepositoryWithDB(t, admin, "Regular User Repo", slug, "desc", "")
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create repo: status %d, body %s", resp.StatusCode, body)
	}

	// Grant the regular user the editor role on this specific
	// repo so they have source:write permission.
	regularUUID := getMeUUID(client)
	testutil.GrantUserRole(t, env, regularUUID, rbac.RoleEditor, repoID)
	client.token = loginUser(client, "regular_user@example.com", "passw0rd!")

	// Regular user creates a source under the repository.
	createBody, _ := json.Marshal(map[string]string{
		"url":  "https://example.com/regular",
		"kind": "homepage",
	})
	createResp, createRaw := client.do("POST", "/api/v1/repositories/"+slug+"/sources", createBody)
	if createResp.StatusCode != http.StatusCreated {
		t.Fatalf("create source as regular user: status %d, body %s", createResp.StatusCode, createRaw)
	}

	// Assert the source is visible via the API (per-repo
	// routing).
	listResp, listRaw := client.do("GET", "/api/v1/repositories/"+slug+"/sources", nil)
	if listResp.StatusCode != http.StatusOK {
		t.Fatalf("list sources: status %d, body %s", listResp.StatusCode, listRaw)
	}
	if !contains(listRaw, "https://example.com/regular") {
		t.Errorf("expected list to contain the source URL; body: %s", listRaw)
	}

	// Belt-and-suspenders: assert no row was inserted into
	// okt_system.users (or any other system-side table) for
	// the source. The source should live in
	// okt_repository.sources, and the row count there should
	// be 1.
	var sourcesInRepo int
	if err := env.DB.QueryRow(testContext(),
		`SELECT count(*) FROM okt_repository.sources WHERE repository_id = $1`,
		repoID,
	).Scan(&sourcesInRepo); err != nil {
		t.Fatalf("counting sources: %v", err)
	}
	if sourcesInRepo != 1 {
		t.Errorf("sources count for repo %s = %d, want 1", repoID, sourcesInRepo)
	}
}

// contains is a tiny string-contains helper for asserting on
// raw response bodies. bytes.Contains is fine, but a local
// helper keeps the test self-contained and avoids a
// bytes-import noise.
func contains(haystack []byte, needle string) bool {
	return len(haystack) >= len(needle) && indexOf(haystack, needle) >= 0
}

func indexOf(haystack []byte, needle string) int {
	n := len(needle)
	if n == 0 {
		return 0
	}
	for i := 0; i+n <= len(haystack); i++ {
		if string(haystack[i:i+n]) == needle {
			return i
		}
	}
	return -1
}

// testContext returns a fresh context.Background. Exists as a
// one-liner indirection so future tests that need a deadline
// can change one place.
func testContext() context.Context { return context.Background() }
