package dbpool

import (
	"context"
	"os"
	"testing"

	"github.com/openktree/open-knowledge-tree/backend/internal/config"
)

// testConfig returns a *config.Config pointing at the e2e
// Postgres instance. The tests below use the same config shape
// the production code does; the registry opens the pools the
// same way at boot.
func testConfig(t *testing.T) *config.Config {
	t.Helper()
	_ = os.Getenv("OKT_TEST_DATABASE_URL")
	def := config.DatabaseConfig{
		Host: "localhost", Port: 5433, User: "okt", Password: "okt_test", Name: "okt", SSLMode: "disable", MaxConns: 5,
	}
	return &config.Config{
		Databases: map[string]config.DatabaseConfig{
			"default": def,
			"tasks":   def,
		},
	}
}

func TestNew_OpensAndPings(t *testing.T) {
	if os.Getenv("OKT_TEST_DATABASE_URL") == "" {
		t.Skip("OKT_TEST_DATABASE_URL not set; skipping live db test")
	}
	ctx := context.Background()
	reg, err := New(ctx, testConfig(t))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer reg.Close()

	if reg.Default() == nil {
		t.Fatal("Default() returned nil")
	}
	if reg.Get("default") == nil {
		t.Fatal("Get(\"default\") returned nil")
	}
	if reg.Get("tasks") == nil {
		t.Fatal("Get(\"tasks\") returned nil")
	}
}

func TestNew_RunsMigrations(t *testing.T) {
	if os.Getenv("OKT_TEST_DATABASE_URL") == "" {
		t.Skip("OKT_TEST_DATABASE_URL not set; skipping live db test")
	}
	ctx := context.Background()
	reg, err := New(ctx, testConfig(t))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer reg.Close()

	// Migrations should have created these tables on every
	// registered database. The presence of the system tables
	// on the default pool is the canonical post-migration
	// signal.
	pool := reg.Default()
	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM users`).Scan(&count); err != nil {
		t.Fatalf("querying users (post-migration): %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM repositories`).Scan(&count); err != nil {
		t.Fatalf("querying repositories (post-migration): %v", err)
	}
}

func TestGet_PanicsOnUnknown(t *testing.T) {
	reg := &Registry{
		pools: map[string]*Pool{"default": {Name: "default"}},
		cfg:   &config.Config{Databases: map[string]config.DatabaseConfig{"default": {}}},
	}
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("Get(\"nope\") did not panic")
		}
	}()
	_ = reg.Get("nope")
}
