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
	"github.com/openktree/open-knowledge-tree/backend/e2e/testutil"
	"github.com/openktree/open-knowledge-tree/backend/internal/providers/fetch"
	"github.com/openktree/open-knowledge-tree/backend/internal/store"
	"github.com/openktree/open-knowledge-tree/backend/internal/taskmanager/tasks"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivertest"
)

// oupCaptchaHTML is the OUP "Validate User" captcha interstitial
// observed in the real corpus. It is 221 chars when trafilatura
// extracts it — just above the historical 200-char MinExtractedLength
// guard — which is why it slipped through as a "successful" fetch
// before the boilerplate-phrase guard was widened. The body matches
// the exact phrasing the guard now matches on ("validate user",
// "could not validate captcha", "experiencing unusual traffic").
const oupCaptchaHTML = `<!doctype html>
<html><head><title>Validate User</title></head>
<body><main>
<h1>Validate User</h1>
<p>We are sorry, but we are experiencing unusual traffic at this time.
Please help us confirm that you are not a robot and we will take you to your content.
Could not validate captcha. Please try again. Take me to my Content.</p>
</main></body></html>`

// TestRetrieveSourceWorkerBoilerplateGuardFailed verifies the
// expanded boilerplate guard in internal/providers/fetch/resolution.go
// prevents the OUP "Validate User" captcha page from being stored as
// a successful fetch. The captcha body is longer than
// MinExtractedLength (200), so the length guard alone would have
// accepted it; the IsJSBoilerplate phrase guard must catch it and
// return ErrInsufficientContent so the chain falls through (and,
// when no heavier tier is wired in the test env, the row is marked
// failed, not fetched).
//
// This is the regression test for the "oup_validate_user_captcha"
// silent-failure mode in scripts/diagnose-sources.
func TestRetrieveSourceWorkerBoilerplateGuardFailed(t *testing.T) {
	env := testutil.NewTestEnv(t)
	defer env.Server.Close()
	ensureRiverSchema(t, env.DB)

	admin := bootstrapSysAdmin(t, env, "boilerplate_admin@example.com")
	repoResp, repoBody, repoID := createRepositoryWithDB(t, admin, "Boilerplate Repo", "boilerplate-repo", "desc", "")
	if repoResp.StatusCode != http.StatusCreated {
		t.Fatalf("create repo: %d %s", repoResp.StatusCode, repoBody)
	}

	// Serve the OUP captcha HTML. trafilatura extracts the
	// visible text (~221 chars), which passes the length guard
	// but must be caught by the phrase guard.
	contentServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(oupCaptchaHTML))
	}))
	defer contentServer.Close()

	registry := testutil.NewForTestPool(env.DB)
	systemQueries := store.New(env.DB)
	strategy := fetch.NewFetchStrategy(fetch.NewFetchResolutionProvider())
	worker := tasks.NewRetrieveSourceWorker(nil, strategy, registry, systemQueries, nil, nil, nil, nil, nil, nil)

	driver := riverpgxv5.New(env.DB)
	workers := river.NewWorkers()
	river.AddWorker(workers, worker)
	testWorker := rivertest.NewWorker(t, driver, &river.Config{
		Queues: map[string]river.QueueConfig{
			tasks.QueueRetrieveSource: {MaxWorkers: 1},
		},
		Workers: workers,
	}, worker)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	tx, err := env.DB.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer tx.Rollback(context.Background())

	job, err := testWorker.Work(ctx, t, tx, tasks.RetrieveSourceArgs{
		URL:          contentServer.URL + "/captcha",
		RepositoryID: repoID,
	}, &river.InsertOpts{Queue: tasks.QueueRetrieveSource})
	if err != nil {
		t.Fatalf("worker.Work: %v", err)
	}

	// The job itself completes (the worker always returns nil
	// from Work when persistence succeeds, even on fetch
	// failure — it marks the row failed and records the
	// output). What matters is the row state.
	output := job.Job.Output()
	var result tasks.RetrieveSourceResult
	if output != nil {
		_ = json.Unmarshal(output, &result)
	}
	if result.Fetched {
		t.Fatalf("expected Fetched=false (boilerplate guard should have rejected the captcha page), got %+v", result)
	}

	// The source row must be status='failed' with
	// parse_status='failed', not 'fetched'/'ok'.
	var (
		rowStatus  string
		rowParse   *string
		rowError   *string
		rowParsed  *string
	)
	row := env.DB.QueryRow(ctx, `
		SELECT status, parse_status, error, parsed_text
		FROM okt_repository.sources
		WHERE repository_id = $1 AND url = $2
	`, repoID, contentServer.URL+"/captcha")
	if err := row.Scan(&rowStatus, &rowParse, &rowError, &rowParsed); err != nil {
		t.Fatalf("querying source row: %v", err)
	}
	if rowStatus != "failed" {
		t.Errorf("expected status='failed' (boilerplate detected), got %q", rowStatus)
	}
	if rowParse == nil || *rowParse != "failed" {
		got := "(null)"
		if rowParse != nil {
			got = *rowParse
		}
		t.Errorf("expected parse_status='failed', got %q", got)
	}
	if rowError == nil || *rowError == "" {
		t.Error("expected non-empty error message on the failed row")
	}
}

// TestRetrieveSourceWorkerRealArticlePassesGuard is the
// positive counterpart: a realistic article whose body is
// longer than MinExtractedLength and contains none of the
// boilerplate phrases must still be stored as status='fetched'
// with parse_status='ok'. This guards against an over-eager
// phrase addition that would drop legitimate articles into the
// failed bucket.
func TestRetrieveSourceWorkerRealArticlePassesGuard(t *testing.T) {
	env := testutil.NewTestEnv(t)
	defer env.Server.Close()
	ensureRiverSchema(t, env.DB)

	admin := bootstrapSysAdmin(t, env, "realarticle_admin@example.com")
	repoResp, repoBody, repoID := createRepositoryWithDB(t, admin, "Real Article Repo", "real-article-repo", "desc", "")
	if repoResp.StatusCode != http.StatusCreated {
		t.Fatalf("create repo: %d %s", repoResp.StatusCode, repoBody)
	}

	contentServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(realisticHTML))
	}))
	defer contentServer.Close()

	registry := testutil.NewForTestPool(env.DB)
	systemQueries := store.New(env.DB)
	strategy := fetch.NewFetchStrategy(fetch.NewFetchResolutionProvider())
	worker := tasks.NewRetrieveSourceWorker(nil, strategy, registry, systemQueries, nil, nil, nil, nil, nil, nil)

	driver := riverpgxv5.New(env.DB)
	workers := river.NewWorkers()
	river.AddWorker(workers, worker)
	testWorker := rivertest.NewWorker(t, driver, &river.Config{
		Queues: map[string]river.QueueConfig{
			tasks.QueueRetrieveSource: {MaxWorkers: 1},
		},
		Workers: workers,
	}, worker)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	tx, err := env.DB.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer tx.Rollback(context.Background())

	job, err := testWorker.Work(ctx, t, tx, tasks.RetrieveSourceArgs{
		URL:          contentServer.URL + "/real",
		RepositoryID: repoID,
	}, &river.InsertOpts{Queue: tasks.QueueRetrieveSource})
	if err != nil {
		t.Fatalf("worker.Work: %v", err)
	}
	if job.EventKind != river.EventKindJobCompleted {
		t.Fatalf("expected job completed, got %s", job.EventKind)
	}

	var rowStatus, rowParse string
	row := env.DB.QueryRow(ctx, `
		SELECT status, COALESCE(parse_status, '') AS parse_status
		FROM okt_repository.sources
		WHERE repository_id = $1 AND url = $2
	`, repoID, contentServer.URL+"/real")
	if err := row.Scan(&rowStatus, &rowParse); err != nil {
		t.Fatalf("querying source row: %v", err)
	}
	if rowStatus != "fetched" {
		t.Errorf("expected status='fetched' for real article, got %q", rowStatus)
	}
	if rowParse != "ok" {
		t.Errorf("expected parse_status='ok' for real article, got %q", rowParse)
	}
}