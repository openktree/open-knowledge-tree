//go:build e2e

package e2e_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/openktree/open-knowledge-tree/backend/e2e/testutil"
	"github.com/openktree/open-knowledge-tree/backend/internal/providers/fetch"
	"github.com/openktree/open-knowledge-tree/backend/internal/store"
	"github.com/openktree/open-knowledge-tree/backend/internal/taskmanager/tasks"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivermigrate"
	"github.com/riverqueue/river/rivertest"
)

// TestDedicatedTaskDB_RiverTablesLandInTasksDB is the proof
// that the production docker-compose "split River into its
// own database" shape actually wires the way it's supposed
// to. The test:
//
//  1. Builds a MultiDBTestEnv with two pools: the default
//     pool against `okt` and a `tasks` pool against
//     `okt_tasks` (created by the docker-entrypoint-initdb.d
//     script on first boot of the test-postgres container).
//  2. Runs a real River worker via rivertest against the
//     `tasks` pool, exactly the way production does.
//  3. Asserts River's bookkeeping tables (`river_job` etc.)
//     exist in the `tasks` database, not in `okt`.
//  4. Asserts the per-repo source row the worker writes
//     lands in the `default` database, not in `tasks`.
//     This is the heart of the test: the worker reads and
//     writes application data through one pool while it
//     talks to River through another.
//
// Why this matters: with the old single-DB shape, a flood of
// River traffic (25 workers churning on a long job batch)
// would saturate the same `max_conns: 20` pool the API uses,
// and the per-repo HTTP handlers would start queuing. The
// dedicated task DB makes that interference impossible at
// the connection level.
func TestDedicatedTaskDB_RiverTablesLandInTasksDB(t *testing.T) {
	env := testutil.NewMultiDBTestEnv(t)
	defer env.Server.Close()

	// Build a worker pointed at the `tasks` pool, mirroring
	// what taskmanager.New does in production. The system
	// pool is still the default one (River doesn't need it;
	// the per-repo source-write uses it).
	systemQueries := store.New(env.DB)
	tasksPool := env.TasksDB
	registry := testutil.NewForTestPool(env.DB)
	strategy := fetch.NewFetchStrategy(fetch.NewFetchResolutionProvider())
	worker := tasks.NewRetrieveSourceWorker(nil, strategy, registry, systemQueries, nil, nil, nil, nil, nil, nil)

	// Run River's bundled migrator against the `tasks` pool.
	// In production this is what taskmanager.New does; we
	// inline it here because the test calls rivertest
	// directly. dbpool.New runs the *application* migrations
	// (the okt_system / okt_repository DDL) against every
	// database, but River has its own bundled migration set
	// that the registry does not run. The migrator is
	// idempotent, so calling it on a database River has
	// already touched is a no-op.
	if err := ensureRiverSchemaOnPool(tasksPool); err != nil {
		t.Fatalf("running River migrations on tasks pool: %v", err)
	}

	// Bind the worker to the `tasks` pool. This is the
	// production wiring in a nutshell: River's driver talks
	// to `tasks`; everything else in the worker uses the
	// default + per-repo pools.
	driver := riverpgxv5.New(tasksPool)
	workers := river.NewWorkers()
	river.AddWorker(workers, worker)
	testWorker := rivertest.NewWorker(t, driver, &river.Config{
		Queues: map[string]river.QueueConfig{
			tasks.QueueRetrieveSource: {MaxWorkers: 1},
		},
		Workers: workers,
	}, worker)

	// Spin up an httptest upstream so the worker's fetch
	// strategy has somewhere to land. The body is
	// distinguishable so we can grep for it later.
	contentServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`<!doctype html><html><head><title>Test</title></head><body><article>
<h1>Multi DB Target</h1>
<p>This is a realistic test article body that is long enough to exceed the fetch strategy minimum extracted length threshold so the parser is confident the page is a real article and the worker persists the row as fetched rather than failing on insufficient content.</p>
<p>A second paragraph to give the parser more context and ensure the body is substantial enough to pass the guard.</p>
</article></body></html>`))
	}))
	defer contentServer.Close()

	// Create the repository we'll scope the source under.
	// The test auth client is the regular client from the
	// embedded TestEnv; the bootstrap default repository
	// was disabled at NewMultiDBTestEnv, so we create a
	// fresh one as a sys admin.
	admin := bootstrapSysAdmin(t, env.TestEnv, "multidb_admin@example.com")
	repoResp, repoBody, repoID := createRepositoryWithDB(t, admin, "Multi-DB Repo", "multi-db-repo", "desc", "")
	if repoResp.StatusCode != http.StatusCreated {
		t.Fatalf("create repo: %d %s", repoResp.StatusCode, repoBody)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Run the worker. The job row is inserted into the
	// `tasks` database inside the test's transaction; the
	// source row is written into the `default` database
	// (which is a separate pool, not the tx). The tx
	// is committed at the end so the assertions below can
	// see the river_job row.
	tx, err := tasksPool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		t.Fatalf("begin tx on tasks pool: %v", err)
	}

	url := contentServer.URL + "/multi-db"
	job, err := testWorker.Work(ctx, t, tx, tasks.RetrieveSourceArgs{
		URL:          url,
		RepositoryID: repoID,
	}, &river.InsertOpts{Queue: tasks.QueueRetrieveSource})
	if err != nil {
		tx.Rollback(context.Background())
		t.Fatalf("worker.Work: %v", err)
	}
	if job.EventKind != river.EventKindJobCompleted {
		tx.Rollback(context.Background())
		t.Fatalf("expected job to be completed, got %s", job.EventKind)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit tx: %v", err)
	}

	// Assertion 1: River's bookkeeping tables live in the
	// `tasks` database. The dbpool registry's AfterConnect
	// hook sets `search_path = okt_system, okt_repository,
	// public` on every connection, so when River's
	// CREATE TABLE runs unqualified, the table lands in
	// okt_system (the first schema in the path). That is
	// the production behavior: River queries against
	// `river_job` resolve via the search path on every
	// connection. The test asserts the table exists on
	// the tasks pool and does NOT exist on the default
	// pool.
	var jobCount int
	if err := tasksPool.QueryRow(ctx, `SELECT count(*) FROM okt_system.river_job`).Scan(&jobCount); err != nil {
		t.Fatalf("counting okt_system.river_job on tasks pool: %v", err)
	}
	if jobCount < 1 {
		t.Errorf("expected at least 1 row in okt_system.river_job on the tasks pool, got %d", jobCount)
	}

	// river_job must NOT exist on the default pool. The
	// registry's migrator runs against every database, but
	// only the task manager's call to `rivermigrate.Migrate`
	// creates the River tables — and it runs against the
	// tasks pool, not the default pool. So the table simply
	// does not exist on the application DB.
	var defaultRiverExists bool
	if err := env.DB.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT FROM information_schema.tables
			WHERE table_schema = 'okt_system' AND table_name = 'river_job'
		)
	`).Scan(&defaultRiverExists); err != nil {
		t.Fatalf("querying river_job existence on default pool: %v", err)
	}
	if defaultRiverExists {
		t.Error("river_job exists on the default pool; the dedicated task DB is not isolated as expected")
	}

	// Assertion 2: the per-repo source row lives in the
	// default database, not the tasks one. The default
	// pool must have exactly one row for this URL; the
	// tasks pool must have zero.
	var defaultRowCount int
	if err := env.DB.QueryRow(ctx, `
		SELECT count(*) FROM okt_repository.sources WHERE url = $1
	`, url).Scan(&defaultRowCount); err != nil {
		t.Fatalf("counting source rows on default pool: %v", err)
	}
	if defaultRowCount != 1 {
		t.Errorf("expected 1 source row on the default pool, got %d", defaultRowCount)
	}

	// On the tasks pool, the okt_repository schema exists
	// (the dbpool registry runs every migration against
	// every database) but should be empty. The schema
	// being there is a side effect of running the same
	// migration set against both databases; the data is
	// not.
	var tasksSourceCount int
	if err := tasksPool.QueryRow(ctx, `
		SELECT count(*) FROM okt_repository.sources WHERE url = $1
	`, url).Scan(&tasksSourceCount); err != nil {
		t.Fatalf("counting source rows on tasks pool: %v", err)
	}
	if tasksSourceCount != 0 {
		t.Errorf("expected 0 source rows on the tasks pool, got %d", tasksSourceCount)
	}

	// Sanity-check the job output decoded correctly. The
	// SourceID is the worker-written row, so it must match
	// the row we see in the default database.
	output := job.Job.Output()
	if output == nil {
		t.Fatal("expected recorded output on job row")
	}
	var result tasks.RetrieveSourceResult
	if err := json.Unmarshal(output, &result); err != nil {
		t.Fatalf("unmarshal output: %v", err)
	}
	if result.SourceID == "" {
		t.Fatal("expected non-empty SourceID in job output")
	}
	if !result.Fetched {
		t.Errorf("expected Fetched=true, got %+v", result)
	}

	var rowID string
	if err := env.DB.QueryRow(ctx, `
		SELECT id::text FROM okt_repository.sources WHERE url = $1
	`, url).Scan(&rowID); err != nil {
		t.Fatalf("querying source row id: %v", err)
	}
	if rowID != result.SourceID {
		t.Errorf("row id %s != job result SourceID %s", rowID, result.SourceID)
	}
}

// ensureRiverSchemaOnPool runs River's bundled migrations
// against the given pool. dbpool.New runs the application
// migration set (okt_system + okt_repository DDL) against
// every database, but River has its own migration set that
// lives in a separate schema and is not part of the
// application DDL. Tests that drive a worker directly need
// to apply River's migrator on whichever pool is meant to
// hold the job table. The migrator is idempotent, so
// re-running on a database River has already seen is a
// no-op.
func ensureRiverSchemaOnPool(pool *pgxpool.Pool) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	migrator, err := rivermigrate.New(riverpgxv5.New(pool), nil)
	if err != nil {
		return err
	}
	_, err = migrator.Migrate(ctx, rivermigrate.DirectionUp, nil)
	return err
}
