// Package dbpool owns the set of Postgres connection pools the
// application uses at runtime. The registry is constructed once
// at boot from cfg.Databases, opens a *pgxpool.Pool per
// registered database, runs versioned migrations against each
// (golang-migrate, driven by an embed.FS), pings each pool, and
// hands them out by name through Get / Default.
//
// Every pool has the same shape: a search_path of
// `okt_system, okt_repository, public` set on its AfterConnect
// hook so unqualified table names in the sqlc queries resolve
// to the right schema on every connection. The same migrations
// run on every database; per-tenant databases (cfg.Databases
// entries other than "default") start as empty mirrors of the
// system DDL and are populated by the tier-upgrade flow when a
// customer moves from the shared tier to an isolated one.
package dbpool

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"net/url"
	"sort"
	"time"

	"github.com/golang-migrate/migrate/v4"
	migratepgx "github.com/golang-migrate/migrate/v4/database/pgx/v5"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/openktree/open-knowledge-tree/backend"
	"github.com/openktree/open-knowledge-tree/backend/internal/config"
)

// searchPath is the connection-level search_path the registry
// applies to every pool (and to every *sql.DB opened for
// migrations). Listing okt_system first means unqualified
// `users` resolves to okt_system.users; the okt_repository
// schema is the second match, used for the per-repository
// tables (sources, classifications, future repo data). The
// public schema stays as the trailing fallback for any
// operator-installed extensions (e.g. uuid-ossp).
const searchPath = "okt_system, okt_repository, public"

// Pool wraps a *pgxpool.Pool with the name the registry assigned
// to it. Code that needs the underlying pool (sqlc, River) reads
// `Pool`; the registry hands pools out by name through Get /
// Default.
type Pool struct {
	Name string
	*pgxpool.Pool
}

// Registry owns the pools the application uses at runtime. It is
// safe for concurrent reads after construction; do not mutate
// after New returns.
type Registry struct {
	pools map[string]*Pool
	cfg   *config.Config
}

// New opens a *pgxpool.Pool for every entry in cfg.Databases,
// runs versioned migrations (golang-migrate) against each, and
// pings each pool. It returns a Registry that hands out pools
// by name. New fails fast: if any database is unreachable or
// any migration step fails, New returns an error and the caller
// is expected to abort boot. The point of fail-fast is to
// surface configuration problems before the API starts serving
// traffic.
func New(ctx context.Context, cfg *config.Config) (*Registry, error) {
	if cfg == nil {
		return nil, fmt.Errorf("dbpool: nil config")
	}
	if _, ok := cfg.Databases["default"]; !ok {
		return nil, fmt.Errorf("dbpool: cfg.Databases[\"default\"] is required")
	}

	reg := &Registry{
		pools: make(map[string]*Pool, len(cfg.Databases)),
		cfg:   cfg,
	}

	// Apply migrations first, before opening the long-lived
	// pools. The migration runner opens its own short-lived
	// *sql.DB connections, and we want to fail fast on a
	// migration error before tying up connection slots in
	// pools we will not end up using.
	if err := migrateAll(ctx, cfg); err != nil {
		return nil, fmt.Errorf("dbpool: applying migrations: %w", err)
	}

	// Open every pool, with a fail-fast on the first one that
	// can't be reached. We open them sequentially (not in
	// parallel) because each open + ping costs a round trip
	// and the savings from concurrency are small relative to
	// the diagnostic value of a serialized log of which pool
	// failed to open.
	openCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	for name, db := range cfg.Databases {
		pool, err := openPool(openCtx, name, db)
		if err != nil {
			reg.closeAll()
			return nil, fmt.Errorf("dbpool: opening %q: %w", name, err)
		}
		reg.pools[name] = pool
		log.Printf("dbpool: opened %q → %s@%s:%d/%s (search_path=%s)", name, db.User, db.Host, db.Port, db.Name, searchPath)
	}

	return reg, nil
}

// migrateAll runs the embedded migrations against every
// database declared in cfg.Databases. The migration runner is
// idempotent (golang-migrate tracks applied versions in
// `schema_migrations` on each database). It opens a fresh
// *sql.DB per database with the search_path baked into the
// DSN, so the migration connection's `users` resolves to
// `okt_system.users` without us having to set the search_path
// on every connection.
func migrateAll(ctx context.Context, cfg *config.Config) error {
	// Sort the database names for deterministic log output.
	names := make([]string, 0, len(cfg.Databases))
	for name := range cfg.Databases {
		names = append(names, name)
	}
	sort.Strings(names)

	src, err := iofs.New(backend.MigrationsFS, "db/migrations")
	if err != nil {
		return fmt.Errorf("loading embedded migrations: %w", err)
	}

	for _, name := range names {
		db := cfg.Databases[name]
		// Build a DSN with search_path in the query string so
		// pgx applies it as a server-side option on every new
		// connection. Without this, an unqualified `users` in
		// 0001_init.up.sql would fail to resolve (Postgres'
		// default search_path is `"$user", public`).
		dsn, err := dsnWithSearchPath(db)
		if err != nil {
			return fmt.Errorf("building DSN for %q: %w", name, err)
		}

		sqlDB, err := sql.Open("pgx/v5", dsn)
		if err != nil {
			return fmt.Errorf("opening migration connection to %q: %w", name, err)
		}
		// Bound the migration connection to a single connection
		// (one server-side advisory lock) so a parallel boot
		// doesn't deadlock against itself.
		sqlDB.SetMaxOpenConns(1)
		// Ping so we fail with a clear "database unreachable"
		// rather than a confusing migration-time error.
		pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		err = sqlDB.PingContext(pingCtx)
		cancel()
		if err != nil {
			_ = sqlDB.Close()
			return fmt.Errorf("pinging %q: %w", name, err)
		}

		driver, err := migratepgx.WithInstance(sqlDB, &migratepgx.Config{})
		if err != nil {
			_ = sqlDB.Close()
			return fmt.Errorf("initializing migrate driver for %q: %w", name, err)
		}
		m, err := migrate.NewWithInstance("iofs", src, "pgx5", driver)
		if err != nil {
			_ = sqlDB.Close()
			return fmt.Errorf("initializing migrate for %q: %w", name, err)
		}
		if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
			_ = sqlDB.Close()
			return fmt.Errorf("applying migrations to %q: %w", name, err)
		}
		// We don't need the migration connection anymore; the
		// *pgxpool.Pool we open afterwards uses its own
		// connection slots. Closing the sql.DB here releases
		// the single slot it held.
		if err := sqlDB.Close(); err != nil {
			return fmt.Errorf("closing migration connection to %q: %w", name, err)
		}
		log.Printf("dbpool: migrations applied to %q", name)
	}
	return nil
}

// dsnWithSearchPath takes a DatabaseConfig and returns a DSN
// with the registry's search_path added to the query string.
// If the DSN already has query parameters, the new param is
// appended; otherwise a new query is created.
func dsnWithSearchPath(db config.DatabaseConfig) (string, error) {
	parsed, err := url.Parse(db.DSN())
	if err != nil {
		return "", err
	}
	q := parsed.Query()
	q.Set("search_path", searchPath)
	parsed.RawQuery = q.Encode()
	return parsed.String(), nil
}

// Get returns the pool for the named database. It panics if the
// name is unknown: by construction, callers only Get names that
// the config validation step has already verified, so an unknown
// name is a programming error, not a runtime condition.
func (r *Registry) Get(name string) *Pool {
	p, ok := r.pools[name]
	if !ok {
		panic(fmt.Sprintf("dbpool: unknown database %q (not in cfg.Databases)", name))
	}
	return p
}

// Default returns the pool for the "default" database. Sugar for
// Get("default"); exists because most callers (the system
// RBAC service, the bootstrap package) want the default pool.
func (r *Registry) Default() *Pool {
	return r.Get("default")
}

// Names returns the list of registered database names, sorted
// in alphabetical order. Used by the admin health endpoint and
// by callers that need to iterate every pool.
func (r *Registry) Names() []string {
	out := make([]string, 0, len(r.pools))
	for name := range r.pools {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// Ping pings every pool with a short timeout. Returns the
// per-database latency. Used by the admin health endpoint. The
// map returned uses nanosecond durations; the admin handler
// can format as needed.
func (r *Registry) Ping(ctx context.Context) map[string]time.Duration {
	out := make(map[string]time.Duration, len(r.pools))
	for name, p := range r.pools {
		start := time.Now()
		pingCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		err := p.Ping(pingCtx)
		cancel()
		if err != nil {
			// Record the elapsed time even on failure so the
			// admin can see "it took 2s to give up." A zero
			// would lie.
			out[name] = time.Since(start)
			continue
		}
		out[name] = time.Since(start)
	}
	return out
}

// Close closes every pool the registry owns. Safe to call
// multiple times.
func (r *Registry) Close() {
	r.closeAll()
}

func (r *Registry) closeAll() {
	for _, p := range r.pools {
		p.Pool.Close()
	}
}

// openPool opens a single pool, attaches the registry-wide
// search_path on its AfterConnect hook, and pings the database.
// Returns a wrapped *Pool on success.
func openPool(ctx context.Context, name string, db config.DatabaseConfig) (*Pool, error) {
	poolCfg, err := pgxpool.ParseConfig(db.DSN())
	if err != nil {
		return nil, fmt.Errorf("parsing DSN: %w", err)
	}
	if db.MaxConns > 0 {
		poolCfg.MaxConns = int32(db.MaxConns)
	}
	// Set the search_path on every new connection. We use the
	// full SET statement rather than a session option because
	// pgxpool's AfterConnect runs once per connection, and a
	// SET is clearer in the log if it ever fails.
	poolCfg.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
		_, err := conn.Exec(ctx, fmt.Sprintf("SET search_path TO %s", searchPath))
		return err
	}

	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		return nil, fmt.Errorf("creating pool: %w", err)
	}

	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("unreachable at %s: %w", db.DSN(), err)
	}

	return &Pool{Name: name, Pool: pool}, nil
}
