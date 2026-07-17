//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/openktree/open-knowledge-tree/backend/internal/providers/fetch"
	"github.com/openktree/open-knowledge-tree/backend/internal/taskmanager/tasks"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivermigrate"
	"github.com/riverqueue/river/rivertest"
)

func testPool(t testing.TB) (*pgxpool.Pool, func()) {
	t.Helper()

	dbURL := os.Getenv("OKT_TEST_DATABASE_URL")
	if dbURL == "" {
		dbURL = "postgres://okt:okt_test@localhost:5433/okt?sslmode=disable"
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		t.Fatalf("connecting to test database: %v", err)
	}

	// Make sure the River schema is in place. We don't drop the
	// public schema here (other tests rely on it) but River's
	// migrator is idempotent.
	migrator, err := rivermigrate.New(riverpgxv5.New(pool), nil)
	if err != nil {
		pool.Close()
		t.Fatalf("creating river migrator: %v", err)
	}
	if _, err := migrator.Migrate(ctx, rivermigrate.DirectionUp, nil); err != nil {
		pool.Close()
		t.Fatalf("running river migrations: %v", err)
	}

	return pool, func() { pool.Close() }
}

// TestRetrieveSourceWorkerClassifiesAndResolves drives the
// worker with a real fetch strategy against a local httptest
// server. The worker must:
//  1. Classify the input URL via fetch.ClassifyURL.
//  2. Pull the resource via the fetch provider.
//  3. Persist a summary via river.RecordOutput.
func TestRetrieveSourceWorkerClassifiesAndResolves(t *testing.T) {
	pool, cleanup := testPool(t)
	defer cleanup()

	// A realistic HTML page whose extracted text exceeds
	// fetch.MinExtractedLength so the insufficient-content
	// guard does not reject it.
	contentServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`<!doctype html><html><head><title>Test</title></head><body><article>
<h1>Test Article</h1>
<p>This is a realistic test article body that is long enough to exceed the fetch strategy minimum extracted length threshold so the parser is confident the page is a real article and the worker persists the row as fetched rather than failing on insufficient content.</p>
<p>A second paragraph to give the parser more context and ensure the body is substantial enough to pass the guard.</p>
</article></body></html>`))
	}))
	defer contentServer.Close()

	fp := fetch.NewFetchResolutionProvider()
	strategy := fetch.NewFetchStrategy(fp)
	// The test only exercises the classify+fetch path of the
	// worker, so we pass nil for the registry and system
	// queries. The worker only consults them when the job
	// carries a non-empty RepositoryID, which this test
	// intentionally omits.
	worker := tasks.NewRetrieveSourceWorker(nil, strategy, nil, nil, nil, nil, nil, nil)

	driver := riverpgxv5.New(pool)

	// rivertest.NewWorker takes the worker and the driver, and
	// exposes a Work method that exercises the real River
	// execution path inside a transaction we control.
	//
	// We must register the worker on the Workers bundle the test
	// client uses, otherwise River refuses the insert (the
	// "job kind is not registered" check is server-side).
	workers := river.NewWorkers()
	river.AddWorker(workers, worker)
	cfg := &river.Config{
		Queues: map[string]river.QueueConfig{
			tasks.QueueRetrieveSource: {MaxWorkers: 1},
		},
		Workers: workers,
	}
	testWorker := rivertest.NewWorker(t, driver, cfg, worker)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	tx, err := pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		t.Fatalf("beginning tx: %v", err)
	}
	defer tx.Rollback(context.Background())

	job, err := testWorker.Work(ctx, t, tx, tasks.RetrieveSourceArgs{
		URL: contentServer.URL + "/some/path",
	}, &river.InsertOpts{
		Queue: tasks.QueueRetrieveSource,
	})
	if err != nil {
		t.Fatalf("worker.Work: %v", err)
	}

	if job.EventKind != river.EventKindJobCompleted {
		t.Fatalf("expected job to be completed, got %s", job.EventKind)
	}

	// Pull the recorded output back off the job row.
	output := job.Job.Output()
	if output == nil {
		t.Fatal("expected recorded output on job row")
	}

	var result tasks.RetrieveSourceResult
	if err := json.Unmarshal(output, &result); err != nil {
		t.Fatalf("unmarshal output: %v", err)
	}

	if result.ClassifiedAs != fetch.SourceURL {
		t.Errorf("expected classified_as=url, got %q", result.ClassifiedAs)
	}
	if !result.Fetched {
		t.Error("expected fetched=true")
	}
	if result.StatusCode != 200 {
		t.Errorf("expected status_code=200, got %d", result.StatusCode)
	}
	if result.Bytes == 0 {
		t.Error("expected non-zero bytes")
	}
}
