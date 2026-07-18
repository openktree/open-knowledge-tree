//go:build e2e

package e2e_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/openktree/open-knowledge-tree/backend/e2e/testutil"
	"github.com/openktree/open-knowledge-tree/backend/internal/providers/content_parsing"
	"github.com/openktree/open-knowledge-tree/backend/internal/providers/fetch"
	"github.com/openktree/open-knowledge-tree/backend/internal/store"
	"github.com/openktree/open-knowledge-tree/backend/internal/taskmanager/tasks"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivertest"
)

// TestRetrieveSourceWorkerPersistsParsedHTML covers the
// happy path for the new content_parsing pipeline: the
// HTML body the worker fetches is parsed by Trafilatura
// (with image extraction enabled), the parsed text and
// title land on the source row, the inline <img> URLs
// land in okt_repository.source_images, and the GET
// endpoint exposes the structured view to the UI.
//
// The test serves a self-contained HTML page from a
// local httptest server so the suite has no network
// dependency.
func TestRetrieveSourceWorkerPersistsParsedHTML(t *testing.T) {
	env := testutil.NewTestEnv(t)
	defer env.Server.Close()
	ensureRiverSchema(t, env.DB)

	admin := bootstrapSysAdmin(t, env, "parsed_admin@example.com")
	const slug = "parsed-repo"
	repoResp, repoBody, repoID := createRepositoryWithDB(t, admin, "Parsed Repo", slug, "desc", "")
	if repoResp.StatusCode != http.StatusCreated {
		t.Fatalf("create repo: %d %s", repoResp.StatusCode, repoBody)
	}

	const page = `<!doctype html>
<html lang="en">
  <head>
    <meta charset="utf-8" />
    <title>Parsed Article Title</title>
    <meta name="author" content="Parsed Author" />
    <meta property="og:site_name" content="Parsed Site" />
  </head>
  <body>
    <nav><a href="/">Home</a></nav>
    <article>
      <h1>Parsed Article Title</h1>
      <p>
        The first paragraph of the article body should
        be extracted as the main text by the parser.
        A few sentences of meaningful prose give the
        extractor enough context to be confident about
        what counts as the body — a single-paragraph
        fixture often trips the library's "not enough
        content" path and returns a near-empty body.
      </p>
      <p>
        <img src="https://example.com/figure-1.png" alt="figure 1" />
        A second paragraph that hosts an inline image
        so we can assert on the source_images row count.
        The figure is the only image on the page and it
        lives inside the article, so the cleaned content
        node should carry it through to the Images slice
        in the parsed result and the worker should
        persist exactly one row in source_images.
      </p>
      <p>
        A third paragraph to firmly establish this as
        a content-rich page so the parser is confident
        about the body. A fourth paragraph exists for
        the same reason — real article pages are not
        two sentences long, and the fixture should match
        that reality so the assertion reflects how the
        parser behaves on real-world input rather than
        on toy data.
      </p>
    </article>
    <footer>Footer that must be stripped.</footer>
  </body>
</html>`

	contentServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(page))
	}))
	defer contentServer.Close()

	registry := testutil.NewForTestPool(env.DB)
	systemQueries := store.New(env.DB)
	// Wire the resolver with the default parsers (Trafilatura
	// only). The default TrafilaturaParser now enables image
	// extraction from the cleaned content node, so the
	// inline figure inside <article> lands in the result
	// while chrome images (logo, footer, nav icons) are
	// filtered out by the same DOM pass that strips the
	// text. We do not pass WithIncludeImages explicitly
	// because it is the default; the test would still
	// pass with WithExcludeImages, just with no images
	// persisted.
	strategy := fetch.NewFetchStrategy(
		fetch.NewFetchResolutionProviderWithParsers(
			content_parsing.NewTrafilaturaParser(),
		),
	)
	worker := tasks.NewRetrieveSourceWorker(nil, strategy, registry, systemQueries, nil, nil, nil, nil, nil, nil)

	driver := riverpgxv5.New(env.DB)
	workers := river.NewWorkers()
	river.AddWorker(workers, worker)
	testWorker := rivertest.NewWorker(t, driver, &river.Config{
		Queues:  map[string]river.QueueConfig{tasks.QueueRetrieveSource: {MaxWorkers: 1}},
		Workers: workers,
	}, worker)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	tx, err := env.DB.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer tx.Rollback(context.Background())

	url := contentServer.URL + "/article"
	if _, err := testWorker.Work(ctx, t, tx, tasks.RetrieveSourceArgs{
		URL:          url,
		RepositoryID: repoID,
	}, &river.InsertOpts{Queue: tasks.QueueRetrieveSource}); err != nil {
		t.Fatalf("worker.Work: %v", err)
	}

	// Verify the parsed fields on the source row, including
	// parsed_markdown (the AI-friendly rendering of the cleaned
	// content). Markdown must be non-empty on a successful HTML
	// parse and must preserve the heading structure the parser
	// surfaced (an ATX heading marker `#` for the <h1>).
	var (
		parsedTitle     *string
		parsedText      *string
		parsedAuthor    *string
		parsedSitename  *string
		parseStatus     *string
		parsedMarkdown  *string
	)
	row := env.DB.QueryRow(ctx, `
		SELECT parsed_title, parsed_text, parsed_author, parsed_sitename, parse_status, parsed_markdown
		FROM okt_repository.sources
		WHERE repository_id = $1 AND url = $2
	`, repoID, url)
	if err := row.Scan(&parsedTitle, &parsedText, &parsedAuthor, &parsedSitename, &parseStatus, &parsedMarkdown); err != nil {
		t.Fatalf("querying source row: %v", err)
	}
	if parseStatus == nil || *parseStatus != "ok" {
		t.Fatalf("parse_status = %v, want \"ok\"", parseStatus)
	}
	if parsedTitle == nil || *parsedTitle != "Parsed Article Title" {
		t.Errorf("parsed_title = %v, want \"Parsed Article Title\"", parsedTitle)
	}
	if parsedText == nil || !contains([]byte(*parsedText), "first paragraph of the article body") {
		t.Errorf("parsed_text did not contain the body: %v", parsedText)
	}
	if parsedAuthor == nil || *parsedAuthor != "Parsed Author" {
		t.Errorf("parsed_author = %v, want \"Parsed Author\"", parsedAuthor)
	}
	if parsedSitename == nil || *parsedSitename != "Parsed Site" {
		t.Errorf("parsed_sitename = %v, want \"Parsed Site\"", parsedSitename)
	}
	if parsedMarkdown == nil || *parsedMarkdown == "" {
		t.Fatalf("parsed_markdown = %v, want non-empty Markdown on a successful HTML parse", parsedMarkdown)
	}
	if !strings.Contains(*parsedMarkdown, "first paragraph of the article body") {
		t.Errorf("parsed_markdown did not contain the body: %q", *parsedMarkdown)
	}
	// The <h1>Parsed Article Title</h1> must render as an ATX
	// heading (#) so the AI can distinguish titles from body.
	if !strings.Contains(*parsedMarkdown, "# Parsed Article Title") {
		t.Errorf("parsed_markdown missing ATX heading for the title: %q", *parsedMarkdown)
	}

	// Verify the inline image row.
	var (
		imageKind   string
		imageURL    *string
		imagePageNo *int32
	)
	imgRow := env.DB.QueryRow(ctx, `
		SELECT kind, url, page_number
		FROM okt_repository.source_images
		WHERE source_id = (
			SELECT id FROM okt_repository.sources
			WHERE repository_id = $1 AND url = $2
		)
		ORDER BY position LIMIT 1
	`, repoID, url)
	if err := imgRow.Scan(&imageKind, &imageURL, &imagePageNo); err != nil {
		t.Fatalf("querying source_images: %v", err)
	}
	if imageKind != "inline" {
		t.Errorf("kind = %q, want inline", imageKind)
	}
	if imageURL == nil || *imageURL != "https://example.com/figure-1.png" {
		t.Errorf("url = %v, want the absolutized figure URL", imageURL)
	}
	if imagePageNo != nil {
		t.Errorf("page_number = %v, want NULL for inline image", *imagePageNo)
	}

	// Verify the GET endpoint exposes both the source and the
	// image list. The shape changed from a bare source to
	// {source, images} in this migration.
	getResp, getRaw := admin.do("GET", "/api/v1/repositories/"+slug+"/sources/00000000-0000-0000-0000-000000000000", nil)
	// The id above is unknown; we want a real id. Re-query.
	var sourceID string
	if err := env.DB.QueryRow(ctx,
		`SELECT id::text FROM okt_repository.sources WHERE repository_id = $1 AND url = $2`,
		repoID, url,
	).Scan(&sourceID); err != nil {
		t.Fatalf("querying source id: %v", err)
	}
	getResp, getRaw = admin.do("GET", "/api/v1/repositories/"+slug+"/sources/"+sourceID, nil)
	if getResp.StatusCode != http.StatusOK {
		t.Fatalf("get source: %d %s", getResp.StatusCode, getRaw)
	}
	var get struct {
		Source struct {
			ParsedTitle *string `json:"parsed_title"`
			ParseStatus *string `json:"parse_status"`
		} `json:"source"`
		Images []struct {
			Kind    string  `json:"kind"`
			URL     *string `json:"url"`
			AltText *string `json:"alt_text"`
		} `json:"images"`
	}
	if err := json.Unmarshal(getRaw, &get); err != nil {
		t.Fatalf("decode get: %v\n%s", err, getRaw)
	}
	if get.Source.ParsedTitle == nil || *get.Source.ParsedTitle != "Parsed Article Title" {
		t.Errorf("GET response parsed_title = %v", get.Source.ParsedTitle)
	}
	if get.Source.ParseStatus == nil || *get.Source.ParseStatus != "ok" {
		t.Errorf("GET response parse_status = %v", get.Source.ParseStatus)
	}
	if len(get.Images) != 1 {
		t.Fatalf("GET response images len = %d, want 1", len(get.Images))
	}
	if get.Images[0].Kind != "inline" {
		t.Errorf("image kind = %q, want inline", get.Images[0].Kind)
	}
}

// TestRetrieveSourceWorkerFiltersChromeImages is the
// end-to-end regression guard for the chrome-image
// pollution bug. A page that puts images in the header
// (logo + Wikipedia-style icon), in a sidebar, in the
// footer, in a hidden tracking pixel, and only one
// image inside the article body must result in exactly
// one row in okt_repository.source_images for the
// surviving URL. The old behavior would have persisted
// every <img> from the raw HTML.
func TestRetrieveSourceWorkerFiltersChromeImages(t *testing.T) {
	env := testutil.NewTestEnv(t)
	defer env.Server.Close()
	ensureRiverSchema(t, env.DB)

	admin := bootstrapSysAdmin(t, env, "chrome_admin@example.com")
	repoResp, repoBody, repoID := createRepositoryWithDB(t, admin, "Chrome Repo", "chrome-repo", "desc", "")
	if repoResp.StatusCode != http.StatusCreated {
		t.Fatalf("create repo: %d %s", repoResp.StatusCode, repoBody)
	}

	const page = `<!doctype html>
<html lang="en">
  <head><title>Knowledge</title></head>
  <body>
    <header>
      <a href="/"><img src="https://example.com/header-logo.png" alt="logo" /></a>
      <img src="https://en.wikipedia.org/static/images/icons/enwiki-25.svg" alt="" width="25" height="25" />
    </header>
    <nav>
      <img src="https://example.com/share-twitter.svg" alt="share on twitter" />
    </nav>
    <main>
      <article>
        <h1>Knowledge</h1>
        <p>The first paragraph of the article body that introduces the topic and sets up the main argument for the rest of the piece. Knowledge is a familiar concept that has been studied for centuries, and modern accounts tend to treat it as a justified true belief held by an epistemic agent about some proposition or state of affairs in the world.</p>
        <p>
          <img src="https://example.com/diagram-of-knowledge.png" alt="diagram" />
          A second paragraph hosting the only article image and continuing the body. A few sentences of meaningful prose give the extractor enough context to be confident about what counts as the body — a single-paragraph fixture often trips the library's "not enough content" path and returns a near-empty body, which is not what this test is exercising.
        </p>
        <p>A third paragraph to firmly establish this as a content-rich page so the parser is confident about the body. A fourth paragraph exists for the same reason — real article pages are not two sentences long, and the fixture should match that reality so the assertion reflects how the parser behaves on real-world input rather than on toy data.</p>
      </article>
      <aside>
        <img src="https://example.com/sidebar-promo.png" alt="promo" />
      </aside>
    </main>
    <footer>
      <img src="https://example.com/footer-donate.png" alt="donate" />
    </footer>
    <img src="https://tracker.example.com/pixel.gif" alt="" width="1" height="1" style="display:none" />
  </body>
</html>`

	contentServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(page))
	}))
	defer contentServer.Close()

	registry := testutil.NewForTestPool(env.DB)
	systemQueries := store.New(env.DB)
	strategy := fetch.NewFetchStrategy(
		fetch.NewFetchResolutionProviderWithParsers(
			content_parsing.NewTrafilaturaParser(),
		),
	)
	worker := tasks.NewRetrieveSourceWorker(nil, strategy, registry, systemQueries, nil, nil, nil, nil, nil, nil)

	driver := riverpgxv5.New(env.DB)
	workers := river.NewWorkers()
	river.AddWorker(workers, worker)
	testWorker := rivertest.NewWorker(t, driver, &river.Config{
		Queues:  map[string]river.QueueConfig{tasks.QueueRetrieveSource: {MaxWorkers: 1}},
		Workers: workers,
	}, worker)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	tx, err := env.DB.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer tx.Rollback(context.Background())

	url := contentServer.URL + "/article"
	if _, err := testWorker.Work(ctx, t, tx, tasks.RetrieveSourceArgs{
		URL:          url,
		RepositoryID: repoID,
	}, &river.InsertOpts{Queue: tasks.QueueRetrieveSource}); err != nil {
		t.Fatalf("worker.Work: %v", err)
	}

	// Read every image row for the source. We expect
	// exactly one — the article figure. The chrome
	// images (header logo, wikipedia icon, share icon,
	// sidebar promo, footer donate, tracking pixel)
	// must not be persisted.
	rows, err := env.DB.Query(ctx, `
		SELECT url
		FROM okt_repository.source_images
		WHERE source_id = (
			SELECT id FROM okt_repository.sources
			WHERE repository_id = $1 AND url = $2
		)
	`, repoID, url)
	if err != nil {
		t.Fatalf("querying source_images: %v", err)
	}
	defer rows.Close()

	var got []string
	for rows.Next() {
		var u *string
		if err := rows.Scan(&u); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if u != nil {
			got = append(got, *u)
		}
	}
	if len(got) != 1 {
		t.Fatalf("expected exactly 1 article image, got %d: %v", len(got), got)
	}
	if got[0] != "https://example.com/diagram-of-knowledge.png" {
		t.Errorf("expected the article image, got %q", got[0])
	}

	// Belt-and-suspenders: confirm none of the chrome
	// URLs leaked into the table under any other
	// position.
	chrome := []string{
		"https://example.com/header-logo.png",
		"https://en.wikipedia.org/static/images/icons/enwiki-25.svg",
		"https://example.com/share-twitter.svg",
		"https://example.com/sidebar-promo.png",
		"https://example.com/footer-donate.png",
		"https://tracker.example.com/pixel.gif",
	}
	for _, bad := range chrome {
		for _, u := range got {
			if u == bad {
				t.Errorf("chrome image %q leaked into source_images", bad)
			}
		}
	}
}

// TestRetrieveSourceWorkerPersistsParsedPDF covers the
// PDF path: a PDF served over HTTP is parsed, text is
// stored on the source row, and one page-render row
// per page is stored in source_images with kind="page"
// and a non-null page_number. The test reads the same
// committed two_page.pdf fixture the unit tests use.
func TestRetrieveSourceWorkerPersistsParsedPDF(t *testing.T) {
	env := testutil.NewTestEnv(t)
	defer env.Server.Close()
	ensureRiverSchema(t, env.DB)

	admin := bootstrapSysAdmin(t, env, "pdfparsed_admin@example.com")
	repoResp, repoBody, repoID := createRepositoryWithDB(t, admin, "PDF Parsed Repo", "pdfparsed-repo", "desc", "")
	if repoResp.StatusCode != http.StatusCreated {
		t.Fatalf("create repo: %d %s", repoResp.StatusCode, repoBody)
	}

	pdfBytes := loadTwoPagePDF(t)
	contentServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/pdf")
		_, _ = w.Write(pdfBytes)
	}))
	defer contentServer.Close()

	registry := testutil.NewForTestPool(env.DB)
	systemQueries := store.New(env.DB)
	// Wire both parsers. The resolver picks the PDF one
	// based on the Content-Type the upstream returns.
	strategy := fetch.NewFetchStrategy(
		fetch.NewFetchResolutionProviderWithParsers(
			content_parsing.NewTrafilaturaParser(),
			content_parsing.NewFitzPDFParser(),
		),
	)
	worker := tasks.NewRetrieveSourceWorker(nil, strategy, registry, systemQueries, nil, nil, nil, nil, nil, nil)

	driver := riverpgxv5.New(env.DB)
	workers := river.NewWorkers()
	river.AddWorker(workers, worker)
	testWorker := rivertest.NewWorker(t, driver, &river.Config{
		Queues:  map[string]river.QueueConfig{tasks.QueueRetrieveSource: {MaxWorkers: 1}},
		Workers: workers,
	}, worker)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	tx, err := env.DB.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer tx.Rollback(context.Background())

	url := contentServer.URL + "/two_page.pdf"
	if _, err := testWorker.Work(ctx, t, tx, tasks.RetrieveSourceArgs{
		URL:          url,
		RepositoryID: repoID,
	}, &river.InsertOpts{Queue: tasks.QueueRetrieveSource}); err != nil {
		t.Fatalf("worker.Work: %v", err)
	}

	// Source row: parse_status ok, parsed_text spans both
	// pages (form-feed separated), parsed_title from the
	// /Info dict. parsed_markdown mirrors parsed_text on PDF
	// sources (no inline structure to convert), and the
	// decomposition worker accepts either column, so the
	// test asserts both are populated to keep the
	// Markdown-first fallback honest.
	var (
		parsedTitle    *string
		parsedText     *string
		parseStatus    *string
		parsedMarkdown *string
	)
	row := env.DB.QueryRow(ctx, `
		SELECT parsed_title, parsed_text, parse_status, parsed_markdown
		FROM okt_repository.sources
		WHERE repository_id = $1 AND url = $2
	`, repoID, url)
	if err := row.Scan(&parsedTitle, &parsedText, &parseStatus, &parsedMarkdown); err != nil {
		t.Fatalf("querying source row: %v", err)
	}
	if parseStatus == nil || *parseStatus != "ok" {
		t.Fatalf("parse_status = %v, want \"ok\"", parseStatus)
	}
	if parsedTitle == nil || *parsedTitle != "OpenKT PDF Test" {
		t.Errorf("parsed_title = %v, want \"OpenKT PDF Test\"", parsedTitle)
	}
	if parsedText == nil {
		t.Fatal("parsed_text = nil, want non-nil")
	}
	if !contains([]byte(*parsedText), "first paragraph of the test document") {
		t.Errorf("parsed_text missing page 1: %q", *parsedText)
	}
	if !contains([]byte(*parsedText), "second page of the test document") {
		t.Errorf("parsed_text missing page 2: %q", *parsedText)
	}
	if parsedMarkdown == nil || *parsedMarkdown == "" {
		t.Fatalf("parsed_markdown = %v, want non-empty (mirrors parsed_text for PDFs)", parsedMarkdown)
	}
	if *parsedMarkdown != *parsedText {
		t.Errorf("parsed_markdown should mirror parsed_text for PDFs; got len(md)=%d len(text)=%d", len(*parsedMarkdown), len(*parsedText))
	}

	// source_images: one row per page, kind=page, page_number
	// 1 and 2, width/height filled (we don't pin the exact
	// numbers — they drift with the renderer — but the
	// columns must be non-null and > 0).
	rows, err := env.DB.Query(ctx, `
		SELECT kind, page_number, width, height, bytes
		FROM okt_repository.source_images
		WHERE source_id = (
			SELECT id FROM okt_repository.sources
			WHERE repository_id = $1 AND url = $2
		)
		ORDER BY page_number
	`, repoID, url)
	if err != nil {
		t.Fatalf("querying source_images: %v", err)
	}
	defer rows.Close()

	var pages []int32
	for rows.Next() {
		var (
			kind    string
			pageNo  *int32
			width   *int32
			height  *int32
			bytesN  *int32
		)
		if err := rows.Scan(&kind, &pageNo, &width, &height, &bytesN); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if kind != "page" {
			t.Errorf("kind = %q, want page", kind)
		}
		if pageNo == nil {
			t.Errorf("page_number = nil for page render")
		} else {
			pages = append(pages, *pageNo)
		}
		if width == nil || *width <= 0 {
			t.Errorf("width = %v, want > 0", width)
		}
		if height == nil || *height <= 0 {
			t.Errorf("height = %v, want > 0", height)
		}
		if bytesN == nil || *bytesN <= 0 {
			t.Errorf("bytes = %v, want > 0", bytesN)
		}
	}
	if len(pages) != 2 {
		t.Fatalf("expected 2 page renders, got %d", len(pages))
	}
	if pages[0] != 1 || pages[1] != 2 {
		t.Errorf("page_number order = %v, want [1 2]", pages)
	}
}

// TestRetrieveSourceWorkerMarksParseFailed covers the
// failure path: when the fetch itself fails, the worker
// sets parse_status='failed' on the source row so the UI
// can hide the parsed view (or show a "could not parse"
// placeholder). The parsed_* columns must remain NULL.
func TestRetrieveSourceWorkerMarksParseFailed(t *testing.T) {
	env := testutil.NewTestEnv(t)
	defer env.Server.Close()
	ensureRiverSchema(t, env.DB)

	admin := bootstrapSysAdmin(t, env, "pfail_admin@example.com")
	repoResp, repoBody, repoID := createRepositoryWithDB(t, admin, "PFail Repo", "pfail-repo", "desc", "")
	if repoResp.StatusCode != http.StatusCreated {
		t.Fatalf("create repo: %d %s", repoResp.StatusCode, repoBody)
	}

	contentServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("boom"))
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
		Queues:  map[string]river.QueueConfig{tasks.QueueRetrieveSource: {MaxWorkers: 1}},
		Workers: workers,
	}, worker)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	tx, err := env.DB.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer tx.Rollback(context.Background())

	url := contentServer.URL + "/will-fail"
	if _, err := testWorker.Work(ctx, t, tx, tasks.RetrieveSourceArgs{
		URL:          url,
		RepositoryID: repoID,
	}, &river.InsertOpts{Queue: tasks.QueueRetrieveSource}); err != nil {
		t.Fatalf("worker.Work: %v", err)
	}

	var (
		parsedTitle *string
		parseStatus *string
	)
	row := env.DB.QueryRow(ctx, `
		SELECT parsed_title, parse_status
		FROM okt_repository.sources
		WHERE repository_id = $1 AND url = $2
	`, repoID, url)
	if err := row.Scan(&parsedTitle, &parseStatus); err != nil {
		t.Fatalf("querying source row: %v", err)
	}
	if parseStatus == nil || *parseStatus != "failed" {
		t.Errorf("parse_status = %v, want \"failed\"", parseStatus)
	}
	if parsedTitle != nil {
		t.Errorf("parsed_title = %q, want NULL on failure", *parsedTitle)
	}
}

// loadTwoPagePDF returns the committed two_page.pdf
// fixture, resolved relative to this test source file
// (not the working directory, which is the package
// directory when `go test` is invoked).
func loadTwoPagePDF(t *testing.T) []byte {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	repoRoot := filepath.Join(filepath.Dir(thisFile), "..")
	path := filepath.Join(repoRoot, "internal", "providers", "content_parsing", "testdata", "two_page.pdf")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read pdf fixture %s: %v", path, err)
	}
	return b
}

// publishedAtFromRow is a small helper that reads the
// published_at column as a *time.Time. Postgres DATE
// scans into time.Time at midnight UTC; pgx gives us a
// zero time.Time for SQL NULL.
func publishedAtFromRow(t *testing.T, env *testutil.TestEnv, repoID string, url string) *time.Time {
	t.Helper()
	var published *time.Time
	row := env.DB.QueryRow(context.Background(), `
		SELECT published_at FROM okt_repository.sources
		WHERE repository_id = $1 AND url = $2
	`, repoID, url)
	if err := row.Scan(&published); err != nil {
		t.Fatalf("querying published_at: %v", err)
	}
	return published
}

// TestRetrieveSourceWorkerPersistsPublishedAtFromParser covers
// the parsing path: when the HTML page exposes a publication
// date (Open Graph article:published_time, JSON-LD
// datePublished, or a visible date htmldate can extract), the
// worker writes it to okt_repository.sources.published_at. The
// test exercises the article:published_time meta tag because
// it is the cleanest fixture: deterministic, day-precision,
// and what Open Graph readers canonically use.
func TestRetrieveSourceWorkerPersistsPublishedAtFromParser(t *testing.T) {
	env := testutil.NewTestEnv(t)
	defer env.Server.Close()
	ensureRiverSchema(t, env.DB)

	admin := bootstrapSysAdmin(t, env, "pubat_admin@example.com")
	repoResp, repoBody, repoID := createRepositoryWithDB(t, admin, "PubAt Repo", "pubat-repo", "desc", "")
	if repoResp.StatusCode != http.StatusCreated {
		t.Fatalf("create repo: %d %s", repoResp.StatusCode, repoBody)
	}

	const page = `<!doctype html>
<html lang="en">
  <head>
    <meta charset="utf-8" />
    <title>Article With Publication Date</title>
    <meta property="article:published_time" content="2021-04-23T08:00:00+00:00" />
    <meta name="author" content="PubAt Author" />
  </head>
  <body>
    <article>
      <h1>Article With Publication Date</h1>
      <p>
        The first paragraph of the article body that introduces the topic and sets up the main argument for the rest of the piece. Knowledge is a familiar concept that has been studied for centuries, and modern accounts tend to treat it as a justified true belief held by an epistemic agent about some proposition or state of affairs in the world.
      </p>
      <p>
        A second paragraph that continues the body and gives the parser enough context to be confident about the article. Real article pages are not two sentences long, and the fixture should match that reality so the assertion reflects how the parser behaves on real-world input rather than on toy data.
      </p>
      <p>
        A third paragraph to firmly establish this as a content-rich page so the parser is confident about the body. A fourth paragraph exists for the same reason: a real article has enough prose to anchor the extraction.
      </p>
    </article>
  </body>
</html>`

	contentServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(page))
	}))
	defer contentServer.Close()

	registry := testutil.NewForTestPool(env.DB)
	systemQueries := store.New(env.DB)
	strategy := fetch.NewFetchStrategy(
		fetch.NewFetchResolutionProviderWithParsers(
			content_parsing.NewTrafilaturaParser(),
		),
	)
	worker := tasks.NewRetrieveSourceWorker(nil, strategy, registry, systemQueries, nil, nil, nil, nil, nil, nil)

	driver := riverpgxv5.New(env.DB)
	workers := river.NewWorkers()
	river.AddWorker(workers, worker)
	testWorker := rivertest.NewWorker(t, driver, &river.Config{
		Queues:  map[string]river.QueueConfig{tasks.QueueRetrieveSource: {MaxWorkers: 1}},
		Workers: workers,
	}, worker)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	tx, err := env.DB.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer tx.Rollback(context.Background())

	url := contentServer.URL + "/dated"
	if _, err := testWorker.Work(ctx, t, tx, tasks.RetrieveSourceArgs{
		URL:          url,
		RepositoryID: repoID,
	}, &river.InsertOpts{Queue: tasks.QueueRetrieveSource}); err != nil {
		t.Fatalf("worker.Work: %v", err)
	}

	published := publishedAtFromRow(t, env, repoID, url)
	if published == nil {
		t.Fatal("published_at = NULL, want 2021-04-23")
	}
	if published.Year() != 2021 || published.Month() != time.April || published.Day() != 23 {
		t.Errorf("published_at = %v, want 2021-04-23", published.Format("2006-01-02"))
	}
}

// TestRetrieveSourceWorkerPublishedAtNullWhenAbsent covers the
// negative case: when neither the parser nor the caller
// surfaced a date, the column must remain NULL. The HTML
// fixture deliberately omits every meta tag htmldate looks
// at (no article:published_time, no dc.date, no JSON-LD
// datePublished) and the test passes no PublishedAt in
// RetrieveSourceArgs, so the worker has no source of truth
// and the column should stay empty.
func TestRetrieveSourceWorkerPublishedAtNullWhenAbsent(t *testing.T) {
	env := testutil.NewTestEnv(t)
	defer env.Server.Close()
	ensureRiverSchema(t, env.DB)

	admin := bootstrapSysAdmin(t, env, "pubatnull_admin@example.com")
	repoResp, repoBody, repoID := createRepositoryWithDB(t, admin, "PubAtNull Repo", "pubatnull-repo", "desc", "")
	if repoResp.StatusCode != http.StatusCreated {
		t.Fatalf("create repo: %d %s", repoResp.StatusCode, repoBody)
	}

	const page = `<!doctype html>
<html lang="en">
  <head>
    <meta charset="utf-8" />
    <title>Article Without Date</title>
    <meta name="author" content="No Date Author" />
  </head>
  <body>
    <article>
      <h1>Article Without Date</h1>
      <p>
        The first paragraph of the article body that introduces the topic and sets up the main argument for the rest of the piece. Knowledge is a familiar concept that has been studied for centuries, and modern accounts tend to treat it as a justified true belief held by an epistemic agent about some proposition or state of affairs in the world.
      </p>
      <p>
        A second paragraph that continues the body and gives the parser enough context to be confident about the article. Real article pages are not two sentences long, and the fixture should match that reality so the assertion reflects how the parser behaves on real-world input rather than on toy data.
      </p>
      <p>
        A third paragraph to firmly establish this as a content-rich page so the parser is confident about the body. A fourth paragraph exists for the same reason: a real article has enough prose to anchor the extraction.
      </p>
    </article>
  </body>
</html>`

	contentServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(page))
	}))
	defer contentServer.Close()

	registry := testutil.NewForTestPool(env.DB)
	systemQueries := store.New(env.DB)
	strategy := fetch.NewFetchStrategy(
		fetch.NewFetchResolutionProviderWithParsers(
			content_parsing.NewTrafilaturaParser(),
		),
	)
	worker := tasks.NewRetrieveSourceWorker(nil, strategy, registry, systemQueries, nil, nil, nil, nil, nil, nil)

	driver := riverpgxv5.New(env.DB)
	workers := river.NewWorkers()
	river.AddWorker(workers, worker)
	testWorker := rivertest.NewWorker(t, driver, &river.Config{
		Queues:  map[string]river.QueueConfig{tasks.QueueRetrieveSource: {MaxWorkers: 1}},
		Workers: workers,
	}, worker)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	tx, err := env.DB.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer tx.Rollback(context.Background())

	url := contentServer.URL + "/undated"
	if _, err := testWorker.Work(ctx, t, tx, tasks.RetrieveSourceArgs{
		URL:          url,
		RepositoryID: repoID,
	}, &river.InsertOpts{Queue: tasks.QueueRetrieveSource}); err != nil {
		t.Fatalf("worker.Work: %v", err)
	}

	published := publishedAtFromRow(t, env, repoID, url)
	if published != nil {
		t.Errorf("published_at = %v, want NULL", published.Format("2006-01-02"))
	}
}

// TestRetrieveSourceWorkerPublishedAtFromCaller covers the
// search-result click-through path: the caller passes a
// PublishedAt in RetrieveSourceArgs (the shape an OpenAlex
// result would produce), the parser fails to recover one
// from the page itself, and the worker still writes the
// caller-supplied date. The "earliest known date wins"
// semantics is what keeps the column useful even when
// trafilatura/htmldate gives up on a noisy page.
func TestRetrieveSourceWorkerPublishedAtFromCaller(t *testing.T) {
	env := testutil.NewTestEnv(t)
	defer env.Server.Close()
	ensureRiverSchema(t, env.DB)

	admin := bootstrapSysAdmin(t, env, "pubatcaller_admin@example.com")
	repoResp, repoBody, repoID := createRepositoryWithDB(t, admin, "PubAtCaller Repo", "pubatcaller-repo", "desc", "")
	if repoResp.StatusCode != http.StatusCreated {
		t.Fatalf("create repo: %d %s", repoResp.StatusCode, repoBody)
	}

	// Same HTML as the absent-date test, so we know
	// the parser side will not surface a date on its
	// own. The test is asserting the caller path
	// independently of the parser path.
	const page = `<!doctype html>
<html lang="en">
  <head>
    <meta charset="utf-8" />
    <title>Article Without Date</title>
  </head>
  <body>
    <article>
      <h1>Article Without Date</h1>
      <p>
        The first paragraph of the article body that introduces the topic and sets up the main argument for the rest of the piece. Knowledge is a familiar concept that has been studied for centuries, and modern accounts tend to treat it as a justified true belief held by an epistemic agent about some proposition or state of affairs in the world.
      </p>
      <p>
        A second paragraph that continues the body and gives the parser enough context to be confident about the article. Real article pages are not two sentences long, and the fixture should match that reality so the assertion reflects how the parser behaves on real-world input rather than on toy data.
      </p>
      <p>
        A third paragraph to firmly establish this as a content-rich page so the parser is confident about the body. A fourth paragraph exists for the same reason: a real article has enough prose to anchor the extraction.
      </p>
    </article>
  </body>
</html>`

	contentServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(page))
	}))
	defer contentServer.Close()

	registry := testutil.NewForTestPool(env.DB)
	systemQueries := store.New(env.DB)
	strategy := fetch.NewFetchStrategy(
		fetch.NewFetchResolutionProviderWithParsers(
			content_parsing.NewTrafilaturaParser(),
		),
	)
	worker := tasks.NewRetrieveSourceWorker(nil, strategy, registry, systemQueries, nil, nil, nil, nil, nil, nil)

	driver := riverpgxv5.New(env.DB)
	workers := river.NewWorkers()
	river.AddWorker(workers, worker)
	testWorker := rivertest.NewWorker(t, driver, &river.Config{
		Queues:  map[string]river.QueueConfig{tasks.QueueRetrieveSource: {MaxWorkers: 1}},
		Workers: workers,
	}, worker)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	tx, err := env.DB.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer tx.Rollback(context.Background())

	// Use a time-of-day component to confirm the
	// column drops the time (Postgres DATE) on
	// persist. The test is robust to that behavior
	// because it asserts on date components only.
	callerDate := time.Date(2019, time.November, 15, 13, 45, 0, 0, time.UTC)
	url := contentServer.URL + "/caller-supplied"
	if _, err := testWorker.Work(ctx, t, tx, tasks.RetrieveSourceArgs{
		URL:          url,
		RepositoryID: repoID,
		PublishedAt:  &callerDate,
	}, &river.InsertOpts{Queue: tasks.QueueRetrieveSource}); err != nil {
		t.Fatalf("worker.Work: %v", err)
	}

	published := publishedAtFromRow(t, env, repoID, url)
	if published == nil {
		t.Fatal("published_at = NULL, want 2019-11-15 (caller-supplied)")
	}
	if published.Year() != 2019 || published.Month() != time.November || published.Day() != 15 {
		t.Errorf("published_at = %v, want 2019-11-15", published.Format("2006-01-02"))
	}
}
