package dbpool

import (
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/openktree/open-knowledge-tree/backend/internal/config"
)

// NewForTest wraps a pre-opened *pgxpool.Pool in a Registry with
// the `default` entry pointing at it. It exists for the e2e test
// setup, which opens its own pool so it can drop/recreate the
// `public` schema before the migrations run, and does not want
// dbpool.New to open a second one.
//
// Production code MUST go through dbpool.New; this constructor
// name makes the test-only nature of the call obvious at the
// call site. The wrapper here only models the `default` entry —
// the e2e suite uses a single Postgres database and treats
// anything else as a name lookup against that same pool.
func NewForTest(pool *pgxpool.Pool) *Registry {
	return &Registry{
		pools: map[string]*Pool{
			"default": {
				Name: "default",
				Pool: pool,
			},
		},
		cfg: &config.Config{
			Databases: map[string]config.DatabaseConfig{
				"default": {Host: "test", Port: 5432, Name: "test"},
			},
		},
	}
}
