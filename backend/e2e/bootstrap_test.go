//go:build e2e

package e2e_test

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/openktree/open-knowledge-tree/backend/e2e/testutil"
	"github.com/openktree/open-knowledge-tree/backend/internal/bootstrap"
	"github.com/openktree/open-knowledge-tree/backend/internal/config"
)

// bootstrapTestConfig builds a *config.Config suitable for the
// single-DB e2e test environment. The bootstrap reads
// `Isolation.DefaultDatabase` and `System.Database` to decide
// which pool to write into; mirroring the test environment's
// posture keeps the test self-contained. The DefaultRepository
// flag is on by default; tests that want it off flip the field
// on the returned config.
func bootstrapTestConfig() *config.Config {
	return &config.Config{
		Bootstrap: config.BootstrapConfig{
			DefaultRepository: true,
		},
		Isolation: config.IsolationConfig{DefaultDatabase: "default"},
		System:    config.SystemConfig{Database: "default"},
	}
}

// TestBootstrapDefaultRepository verifies the startup bootstrap
// creates a default repository when the database is empty and at
// least one user exists. It reuses NewTestEnv so the schema and
// RBAC are set up exactly the same way the production server does
// it, then runs the bootstrap step against the same pool the API
// reads from.
func TestBootstrapDefaultRepository(t *testing.T) {
	env := testutil.NewTestEnv(t)

	// Register one user so the bootstrap has an owner to attach the
	// new repository to. The bootstrap step itself does not create
	// users; it only picks an existing one.
	client := registerTestUser(t, env, "owner@example.com", "password123", "Owner")

	// NewTestEnv resets the public schema, so the repositories
	// table is empty at this point.
	var n int
	if err := env.DB.QueryRow(context.Background(), `SELECT count(*) FROM repositories`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Fatalf("expected 0 repositories, got %d", n)
	}

	// Startup-time call: pass "" as ownerID so the bootstrap
	// picks the earliest user.
	cfg := bootstrapTestConfig()
	res, err := bootstrap.EnsureDefaultRepository(context.Background(), testutil.NewForTestPool(env.DB), cfg, "", nil)
	if err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if !res.Created {
		t.Fatalf("expected Created=true, got %+v", res)
	}
	if res.RepositoryID == "" {
		t.Fatal("expected non-empty repository id")
	}
	if res.OwnerID == "" {
		t.Fatal("expected non-empty owner id")
	}

	// The repository must be visible through the API and owned by
	// the registered user. We use the API here instead of peeking
	// at env.DB directly so the test also exercises the wiring
	// between the bootstrap row and the HTTP layer.
	resp, body := client.do("GET", "/api/v1/repositories", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list: expected 200, got %d: %s", resp.StatusCode, body)
	}
	var list repositoryList
	if err := json.Unmarshal(body, &list); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if len(list.Repositories) != 1 {
		t.Fatalf("expected 1 repository, got %d", len(list.Repositories))
	}
	got := list.Repositories[0]
	if got.ID != res.RepositoryID {
		t.Fatalf("expected id %s, got %s", res.RepositoryID, got.ID)
	}
	if got.Slug != "default" {
		t.Fatalf("expected slug 'default', got %s", got.Slug)
	}
	if got.Name != "Default" {
		t.Fatalf("expected name 'Default', got %s", got.Name)
	}
	if got.OwnerID != res.OwnerID {
		t.Fatalf("expected owner %s, got %s", res.OwnerID, got.OwnerID)
	}
}

// TestBootstrapDefaultRepositoryNoopWhenExists verifies the
// bootstrap does not create a second repository when one already
// exists.
func TestBootstrapDefaultRepositoryNoopWhenExists(t *testing.T) {
	env := testutil.NewTestEnv(t)

	_ = registerTestUser(t, env, "owner@example.com", "password123", "Owner")

	// Pre-seed a repository with a different slug so we can tell
	// that the bootstrap left it alone.
	ownerID := firstUserID(t, env.DB)
	var existingID string
	if err := env.DB.QueryRow(context.Background(), `
		INSERT INTO repositories (name, slug, description, owner_id)
		VALUES ('Existing', 'existing', 'pre-existing', $1)
		RETURNING id::text
	`, ownerID).Scan(&existingID); err != nil {
		t.Fatalf("inserting seed repository: %v", err)
	}

	cfg := bootstrapTestConfig()
	res, err := bootstrap.EnsureDefaultRepository(context.Background(), testutil.NewForTestPool(env.DB), cfg, "", nil)
	if err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if res.Created {
		t.Fatalf("expected Created=false when repositories already exist, got %+v", res)
	}
	if !res.Skipped {
		t.Fatalf("expected Skipped=true, got %+v", res)
	}

	var n int
	if err := env.DB.QueryRow(context.Background(), `SELECT count(*) FROM repositories`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Fatalf("expected 1 repository after no-op bootstrap, got %d", n)
	}
}

// TestBootstrapDefaultRepositorySkipsWhenNoUsers verifies the
// startup-time bootstrap gracefully skips when there are no users
// to own the repository. The repositories table has a non-null FK
// to users, so this is the only safe behavior. The lazy path in
// GET /repositories picks up the slack once the first user
// authenticates.
func TestBootstrapDefaultRepositorySkipsWhenNoUsers(t *testing.T) {
	env := testutil.NewTestEnv(t)

	cfg := bootstrapTestConfig()
	res, err := bootstrap.EnsureDefaultRepository(context.Background(), testutil.NewForTestPool(env.DB), cfg, "", nil)
	if err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if res.Created {
		t.Fatalf("expected Created=false when no users exist, got %+v", res)
	}
	if !res.Skipped {
		t.Fatalf("expected Skipped=true, got %+v", res)
	}
}

// TestBootstrapDefaultRepositoryDisabled verifies the bootstrap
// respects the config flag and does nothing when disabled.
func TestBootstrapDefaultRepositoryDisabled(t *testing.T) {
	env := testutil.NewTestEnv(t)

	_ = registerTestUser(t, env, "owner@example.com", "password123", "Owner")

	cfg := &config.Config{
		Bootstrap:  config.BootstrapConfig{DefaultRepository: false},
		Isolation:  config.IsolationConfig{DefaultDatabase: "default"},
		System:     config.SystemConfig{Database: "default"},
	}
	res, err := bootstrap.EnsureDefaultRepository(context.Background(), testutil.NewForTestPool(env.DB), cfg, "", nil)
	if err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if res.Created {
		t.Fatalf("expected Created=false when bootstrap disabled, got %+v", res)
	}

	var n int
	if err := env.DB.QueryRow(context.Background(), `SELECT count(*) FROM repositories`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Fatalf("expected 0 repositories when bootstrap disabled, got %d", n)
	}
}

// TestBootstrapDefaultRepositoryLazyAttachesToCaller verifies the
// HTTP-level lazy path: when the bootstrap flag is on and the
// repositories table is empty, the first call to GET
// /repositories after a user authenticates creates a starter
// repository owned by that user. This is the regression test for
// the "I registered but the Repositories page is empty" bug.
//
// The lazy hook is wired in the production api.NewHandler. NewTestEnv
// already uses api.NewHandler, so the only thing this test does
// is flip the bootstrap flag on env.Config before the request
// fires.
func TestBootstrapDefaultRepositoryLazyAttachesToCaller(t *testing.T) {
	env := testutil.NewTestEnv(t)
	env.Config.Bootstrap.DefaultRepository = true

	client := registerTestUser(t, env, "alice@example.com", "password123", "Alice")

	resp, body := client.do("GET", "/api/v1/repositories", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list: expected 200, got %d: %s", resp.StatusCode, body)
	}
	var list repositoryList
	if err := json.Unmarshal(body, &list); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if len(list.Repositories) != 1 {
		t.Fatalf("expected lazy bootstrap to create 1 repository, got %d", len(list.Repositories))
	}
	got := list.Repositories[0]
	if got.Slug != "default" {
		t.Fatalf("expected slug 'default', got %s", got.Slug)
	}

	userID := firstUserID(t, env.DB)
	if got.OwnerID != userID {
		t.Fatalf("expected owner %s, got %s", userID, got.OwnerID)
	}
}

// TestBootstrapDefaultRepositoryLazyIsIdempotent verifies a
// second call to GET /repositories doesn't create a second
// repository. The lazy path is safe to invoke on every list.
func TestBootstrapDefaultRepositoryLazyIsIdempotent(t *testing.T) {
	env := testutil.NewTestEnv(t)
	env.Config.Bootstrap.DefaultRepository = true

	client := registerTestUser(t, env, "bob@example.com", "password123", "Bob")

	for i := 0; i < 3; i++ {
		resp, body := client.do("GET", "/api/v1/repositories", nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("iter %d: expected 200, got %d: %s", i, resp.StatusCode, body)
		}
		var list repositoryList
		if err := json.Unmarshal(body, &list); err != nil {
			t.Fatalf("iter %d: decode: %v", i, err)
		}
		if len(list.Repositories) != 1 {
			t.Fatalf("iter %d: expected 1 repository, got %d", i, len(list.Repositories))
		}
	}
}

// TestBootstrapDefaultRepositoryLazyNoOpWhenDisabled verifies the
// lazy path respects the bootstrap flag and does nothing when
// disabled. The first GET /repositories returns an empty list,
// and no row appears in the table.
func TestBootstrapDefaultRepositoryLazyNoOpWhenDisabled(t *testing.T) {
	env := testutil.NewTestEnv(t)
	// NewTestEnv defaults DefaultRepository to false, so the
	// lazy hook is wired but the bootstrap short-circuits.
	// We re-assert it here so the test reads as a no-op
	// regardless of NewTestEnv's future default.
	env.Config.Bootstrap.DefaultRepository = false

	client := registerTestUser(t, env, "carol@example.com", "password123", "Carol")

	resp, body := client.do("GET", "/api/v1/repositories", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list: expected 200, got %d: %s", resp.StatusCode, body)
	}
	var list repositoryList
	if err := json.Unmarshal(body, &list); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(list.Repositories) != 0 {
		t.Fatalf("expected 0 repositories when bootstrap disabled, got %d", len(list.Repositories))
	}

	var n int
	if err := env.DB.QueryRow(context.Background(), `SELECT count(*) FROM repositories`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Fatalf("expected 0 rows in repositories table, got %d", n)
	}
}

// TestBootstrapDefaultRepositoryLazyNoOpForExistingOwners
// verifies the lazy path doesn't attach a second starter
// repository to a user who already owns one. The list path
// should only create a starter when the caller has zero
// repositories of their own (and the table is empty).
func TestBootstrapDefaultRepositoryLazyNoOpForExistingOwners(t *testing.T) {
	env := testutil.NewTestEnv(t)
	env.Config.Bootstrap.DefaultRepository = true

	client := registerTestUser(t, env, "dave@example.com", "password123", "Dave")

	// Pre-seed a repository owned by Dave with a
	// non-default slug so we can tell that the lazy path
	// didn't create a "default" row on top.
	ownerID := firstUserID(t, env.DB)
	if _, err := env.DB.Exec(context.Background(), `
		INSERT INTO repositories (name, slug, description, owner_id)
		VALUES ('Dave''s', 'daves', 'preexisting', $1)
	`, ownerID); err != nil {
		t.Fatalf("seeding: %v", err)
	}

	resp, body := client.do("GET", "/api/v1/repositories", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list: expected 200, got %d: %s", resp.StatusCode, body)
	}
	var list repositoryList
	if err := json.Unmarshal(body, &list); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(list.Repositories) != 1 {
		t.Fatalf("expected 1 repository (the seeded one), got %d", len(list.Repositories))
	}
	if list.Repositories[0].Slug != "daves" {
		t.Fatalf("expected slug 'daves', got %s", list.Repositories[0].Slug)
	}
}

// TestBootstrapDefaultAdminSeedsUser verifies the default-admin
// bootstrap creates a user with the system_admin role when the
// users table is empty and the env vars are set.
func TestBootstrapDefaultAdminSeedsUser(t *testing.T) {
	env := testutil.NewTestEnv(t)

	// Wipe the users (and sessions) created by NewTestEnv's
	// RBAC setup. Casbin policies and casbin_rule grouping
	// rows reference users, but those are seeded at the
	// system scope and don't cascade; truncating users is
	// safe here because nothing else in the test depends
	// on the seeded user.
	resetUsersTable(t, env.DB)

	t.Setenv("OKT_BOOTSTRAP_DEFAULT_ADMIN_EMAIL", "admin@example.com")
	t.Setenv("OKT_BOOTSTRAP_DEFAULT_ADMIN_PASSWORD", "supersecret123")
	t.Setenv("OKT_BOOTSTRAP_DEFAULT_ADMIN_DISPLAY_NAME", "Default Admin")

	env.Config.Bootstrap.DefaultAdmin = true

	res, err := bootstrap.EnsureDefaultAdmin(context.Background(), testutil.NewForTestPool(env.DB), env.Config, env.RBAC)
	if err != nil {
		t.Fatalf("ensure admin: %v", err)
	}
	if !res.Created {
		t.Fatalf("expected Created=true, got %+v", res)
	}

	// The user exists in the database.
	var count int
	if err := env.DB.QueryRow(context.Background(), `SELECT count(*) FROM users WHERE email = $1`, "admin@example.com").Scan(&count); err != nil {
		t.Fatalf("count users: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected 1 admin user, got %d", count)
	}

	// The password actually works (regression test for
	// "we hashed it correctly"). Login through the HTTP API
	// so we exercise the full path.
	client := newAuthClient(env.BaseURL)
	resp, body := client.login("admin@example.com", "supersecret123")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("login: expected 200, got %d: %s", resp.StatusCode, body)
	}
	var loginResp struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(body, &loginResp); err != nil {
		t.Fatalf("decode login: %v", err)
	}
	if loginResp.Token == "" {
		t.Fatal("expected non-empty token")
	}
	client.token = loginResp.Token

	// The admin can list users (admin-only endpoint).
	resp, body = client.do("GET", "/api/v1/admin/users", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("admin list users: expected 200, got %d: %s", resp.StatusCode, body)
	}
}

// TestBootstrapDefaultAdminSkipsWhenUsersExist verifies the
// admin bootstrap is a no-op once any user exists. The env vars
// are intentionally set to a unique email; the test asserts
// that user is NOT created.
func TestBootstrapDefaultAdminSkipsWhenUsersExist(t *testing.T) {
	env := testutil.NewTestEnv(t)

	// Register a regular user first.
	_ = registerTestUser(t, env, "regular@example.com", "password123", "Regular")

	t.Setenv("OKT_BOOTSTRAP_DEFAULT_ADMIN_EMAIL", "admin2@example.com")
	t.Setenv("OKT_BOOTSTRAP_DEFAULT_ADMIN_PASSWORD", "supersecret123")
	env.Config.Bootstrap.DefaultAdmin = true

	res, err := bootstrap.EnsureDefaultAdmin(context.Background(), testutil.NewForTestPool(env.DB), env.Config, env.RBAC)
	if err != nil {
		t.Fatalf("ensure admin: %v", err)
	}
	if res.Created {
		t.Fatalf("expected Created=false when users exist, got %+v", res)
	}

	var count int
	if err := env.DB.QueryRow(context.Background(), `SELECT count(*) FROM users WHERE email = $1`, "admin2@example.com").Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 0 {
		t.Fatalf("expected 0 admin2 users (no-op), got %d", count)
	}
}

// TestBootstrapDefaultAdminSkipsWhenEnvMissing verifies the
// admin bootstrap is a no-op when any required env var is
// missing. The flag is on but the password is empty.
func TestBootstrapDefaultAdminSkipsWhenEnvMissing(t *testing.T) {
	env := testutil.NewTestEnv(t)
	resetUsersTable(t, env.DB)

	t.Setenv("OKT_BOOTSTRAP_DEFAULT_ADMIN_EMAIL", "admin3@example.com")
	// Password env var deliberately unset.
	os.Unsetenv("OKT_BOOTSTRAP_DEFAULT_ADMIN_PASSWORD")
	env.Config.Bootstrap.DefaultAdmin = true

	res, err := bootstrap.EnsureDefaultAdmin(context.Background(), testutil.NewForTestPool(env.DB), env.Config, env.RBAC)
	if err != nil {
		t.Fatalf("ensure admin: %v", err)
	}
	if res.Created {
		t.Fatalf("expected Created=false when env missing, got %+v", res)
	}

	var count int
	if err := env.DB.QueryRow(context.Background(), `SELECT count(*) FROM users`).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 0 {
		t.Fatalf("expected 0 users, got %d", count)
	}
}

// TestBootstrapDefaultAdminSkipsWhenFlagOff verifies the
// admin bootstrap respects the flag. The env vars are set
// (so we know they're honored) but the flag is off.
func TestBootstrapDefaultAdminSkipsWhenFlagOff(t *testing.T) {
	env := testutil.NewTestEnv(t)
	resetUsersTable(t, env.DB)

	t.Setenv("OKT_BOOTSTRAP_DEFAULT_ADMIN_EMAIL", "admin4@example.com")
	t.Setenv("OKT_BOOTSTRAP_DEFAULT_ADMIN_PASSWORD", "supersecret123")
	env.Config.Bootstrap.DefaultAdmin = false

	res, err := bootstrap.EnsureDefaultAdmin(context.Background(), testutil.NewForTestPool(env.DB), env.Config, env.RBAC)
	if err != nil {
		t.Fatalf("ensure admin: %v", err)
	}
	if res.Created {
		t.Fatalf("expected Created=false when flag off, got %+v", res)
	}
}

// TestBootstrapDefaultRepositoryLazySeedsSettings verifies the
// lazy default-repository bootstrap seeds the per-repository
// provider + context settings so the freshly-created default repo
// is functional out of the box. This is the regression test for the
// "search provider not enabled for this repository" bug: before
// the fix, EnsureDefaultRepository inserted the repo row directly
// and never seeded repository_provider_settings /
// repository_contexts, so every search/retrieve/extract gate
// denied the request and concept extraction hard-failed on the
// empty context list.
func TestBootstrapDefaultRepositoryLazySeedsSettings(t *testing.T) {
	env := testutil.NewTestEnv(t)
	env.Config.Bootstrap.DefaultRepository = true

	client := registerTestUser(t, env, "seeder@example.com", "password123", "Seeder")

	resp, body := client.do("GET", "/api/v1/repositories", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list: expected 200, got %d: %s", resp.StatusCode, body)
	}
	var list repositoryList
	if err := json.Unmarshal(body, &list); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(list.Repositories) != 1 {
		t.Fatalf("expected lazy bootstrap to create 1 repository, got %d", len(list.Repositories))
	}
	repoID := list.Repositories[0].ID

	// The default repo must have context rows (seeded from the
	// embedded vocabulary). NewTestEnv wires the ontology source
	// via WireRepoSettings, so the seeder expands "all" to the
	// full context vocabulary. At least one row must exist.
	var ctxCount int
	if err := env.DB.QueryRow(context.Background(),
		`SELECT count(*) FROM okt_system.repository_contexts WHERE repository_id = $1`, repoID,
	).Scan(&ctxCount); err != nil {
		t.Fatalf("count contexts: %v", err)
	}
	if ctxCount == 0 {
		t.Fatalf("expected default repo to have seeded context rows, got 0")
	}

	// The default repo must have at least one enabled provider
	// row. NewTestEnv wires no search providers but does wire the
	// plain fetch resolution provider, so the seeder seeds at
	// least (resolution, fetch, enabled=true).
	var provCount int
	if err := env.DB.QueryRow(context.Background(),
		`SELECT count(*) FROM okt_system.repository_provider_settings
		 WHERE repository_id = $1 AND enabled = TRUE`, repoID,
	).Scan(&provCount); err != nil {
		t.Fatalf("count providers: %v", err)
	}
	if provCount == 0 {
		t.Fatalf("expected default repo to have seeded enabled provider rows, got 0")
	}
}

// firstUserID returns the id of the earliest-created user. It
// exists so the noop test can attach its pre-seeded repository
// to a real user without duplicating the SQL inline.
func firstUserID(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	var id string
	if err := pool.QueryRow(context.Background(), `SELECT id::text FROM users ORDER BY created_at ASC LIMIT 1`).Scan(&id); err != nil {
		t.Fatalf("finding owner: %v", err)
	}
	return id
}

// resetUsersTable truncates the users table (and the sessions
// that depend on it) so a test can exercise the "no users
// exist" branch of the admin bootstrap. The schema is left
// intact.
func resetUsersTable(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `TRUNCATE TABLE sessions`); err != nil {
		t.Fatalf("truncate sessions: %v", err)
	}
	if _, err := pool.Exec(context.Background(), `TRUNCATE TABLE users CASCADE`); err != nil {
		t.Fatalf("truncate users: %v", err)
	}
}
