//go:build e2e

package e2e_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/openktree/open-knowledge-tree/backend/internal/providers/content_parsing"
	"github.com/openktree/open-knowledge-tree/backend/internal/providers/fetch"
)

// TestContentParsing_FetchPipeline wires the trafilatura
// parser into the HTTP fetch resolver and asserts that
// resolving a real-shape HTML document yields a non-empty
// text body and at least one image. The page is served by
// an httptest server so the test has no external network
// dependency and is deterministic.
func TestContentParsing_FetchPipeline(t *testing.T) {
	page := `<!doctype html>
<html lang="en">
  <head>
    <meta charset="utf-8" />
    <title>Pipeline Test Article</title>
    <meta name="author" content="Pipeline Author" />
    <meta property="og:site_name" content="Pipeline Site" />
  </head>
  <body>
    <nav><a href="/">Home</a></nav>
    <article>
      <h1>Pipeline Test Article</h1>
      <p>
        The trafilatura parser should keep this
        paragraph because it is the main content of
        the article. The body needs to be long enough
        for the extractor to be confident that what
        comes after this opener is genuinely the
        article and not just a sidebar; a few
        sentences of meaningful prose is the
        difference between a confident extraction
        and a near-empty one.
      </p>
      <p>
        <img src="/diagram.png" alt="diagram" />
        A second paragraph to give the extractor
        enough context to identify the body
        confidently, and to host the inline figure
        that the assertion at the end of the test
        looks for. The figure is the only image on
        the page and it lives inside the article,
        so the parser's cleaned content node should
        carry it through to the Images slice in the
        parsed result.
      </p>
      <p>
        A third paragraph to firmly establish this
        as a content-rich page with enough body text
        that the extractor is sure about the body. A
        fourth paragraph exists for the same reason
        — real article pages are not two sentences
        long, and the fixture should match that
        reality so the assertion reflects how the
        parser behaves on real-world input rather
        than on toy data.
      </p>
    </article>
    <footer>Copyright 2025 Pipeline Site</footer>
  </body>
</html>`

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(page))
	}))
	defer srv.Close()

	parser := content_parsing.NewTrafilaturaParser(content_parsing.WithIncludeImages())
	resolver := fetch.NewFetchResolutionProviderWithParser(parser)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	resolved, err := resolver.Resolve(ctx, fetch.Resource{
		Type:  fetch.SourceURL,
		Value: srv.URL + "/article",
	})
	if err != nil {
		t.Fatalf("Resolve failed: %v", err)
	}

	if resolved.StatusCode != http.StatusOK {
		t.Errorf("expected status 200, got %d", resolved.StatusCode)
	}
	if len(resolved.Body) == 0 {
		t.Errorf("expected raw body to be preserved, got empty")
	}
	if resolved.ContentType == "" {
		t.Errorf("expected non-empty ContentType")
	}

	doc := resolved.Parsed
	if !strings.Contains(strings.ToLower(doc.Title), "pipeline") {
		t.Errorf("expected title to mention 'pipeline', got %q", doc.Title)
	}
	if !strings.Contains(doc.Text, "main content of the article") {
		t.Errorf("expected text to contain article body, got %q", doc.Text)
	}
	if strings.Contains(doc.Text, "Copyright 2025") {
		t.Errorf("expected footer to be stripped, got %q", doc.Text)
	}

	if len(doc.Images) == 0 {
		t.Errorf("expected at least one image, got 0")
	}
	if len(doc.Images) > 0 && !strings.HasPrefix(doc.Images[0].URL, srv.URL) {
		t.Errorf("expected image to be absolutized against %q, got %q", srv.URL, doc.Images[0].URL)
	}
	if len(doc.Images) > 0 && doc.Images[0].Alt != "diagram" {
		t.Errorf("expected alt 'diagram', got %q", doc.Images[0].Alt)
	}
}

// TestContentParsing_PDFPipeline wires both the
// Trafilatura and the PDF parsers into the HTTP fetch
// resolver and asserts that a PDF served over HTTP flows
// end-to-end: the resolver picks the PDF parser based on
// Content-Type, text is extracted from every page, and
// one PNG render per page is returned in PageImages.
//
// The fixture is read from the same testdata directory
// the unit tests use, so the e2e test has no external
// network dependency and stays deterministic.
func TestContentParsing_PDFPipeline(t *testing.T) {
	raw := loadContentParsingPDF(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/pdf")
		_, _ = w.Write(raw)
	}))
	defer srv.Close()

	resolver := fetch.NewFetchResolutionProviderWithParsers(
		content_parsing.NewTrafilaturaParser(),
		content_parsing.NewFitzPDFParser(),
	)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	resolved, err := resolver.Resolve(ctx, fetch.Resource{
		Type:  fetch.SourceURL,
		Value: srv.URL + "/two_page.pdf",
	})
	if err != nil {
		t.Fatalf("Resolve failed: %v", err)
	}
	if resolved.StatusCode != http.StatusOK {
		t.Errorf("expected status 200, got %d", resolved.StatusCode)
	}
	if !strings.Contains(strings.ToLower(resolved.Parsed.Title), "openkt pdf test") {
		t.Errorf("expected parsed title, got %q", resolved.Parsed.Title)
	}
	if !strings.Contains(resolved.Parsed.Text, "first paragraph of the test document") {
		t.Errorf("expected page 1 text, got %q", resolved.Parsed.Text)
	}
	if !strings.Contains(resolved.Parsed.Text, "second page of the test document") {
		t.Errorf("expected page 2 text, got %q", resolved.Parsed.Text)
	}
	if len(resolved.Parsed.PageImages) != 2 {
		t.Errorf("expected 2 page renders, got %d", len(resolved.Parsed.PageImages))
	}
}

// TestContentParsing_PDFNotParsedWithoutParser verifies
// that a PDF response, when the resolver is wired with
// only an HTML parser, leaves Parsed empty and the raw
// body preserved. This is the regression guard for the
// pickParser logic: a misconfigured call site must
// return the raw PDF bytes, not crash.
func TestContentParsing_PDFNotParsedWithoutParser(t *testing.T) {
	raw := []byte("%PDF-1.4\n%placeholder for fixture-less path\n")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/pdf")
		_, _ = w.Write(raw)
	}))
	defer srv.Close()

	resolver := fetch.NewFetchResolutionProviderWithParsers(
		content_parsing.NewTrafilaturaParser(),
	)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	resolved, err := resolver.Resolve(ctx, fetch.Resource{
		Type:  fetch.SourceURL,
		Value: srv.URL,
	})
	if err != nil {
		t.Fatalf("Resolve failed: %v", err)
	}
	if len(resolved.Body) == 0 {
		t.Errorf("expected raw body preserved, got empty")
	}
	if resolved.Parsed.Text != "" {
		t.Errorf("expected empty Parsed when no PDF parser is wired, got %q", resolved.Parsed.Text)
	}
}

// loadContentParsingPDF returns the committed two_page.pdf
// fixture. The path is resolved relative to this test
// source file (not the working directory, which is the
// package directory when `go test` is invoked) so the
// test is independent of cwd.
func loadContentParsingPDF(t *testing.T) []byte {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	// thisFile is e2e/content_parsing_test.go; the
	// fixture lives under
	// internal/providers/content_parsing/testdata
	// inside the backend/ module.
	repoRoot := filepath.Join(filepath.Dir(thisFile), "..")
	path := filepath.Join(repoRoot, "internal", "providers", "content_parsing", "testdata", "two_page.pdf")
	return mustReadFile(t, path)
}

func mustReadFile(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return b
}

// TestContentParsing_UnsupportedContentType verifies that a
// resolver wired with a parser does not return a parsed
// document when the response is a non-HTML, non-PDF body
// (here, plain text). The raw body is still returned.
func TestContentParsing_UnsupportedContentType(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte("just some plain text"))
	}))
	defer srv.Close()

	parser := content_parsing.NewTrafilaturaParser()
	resolver := fetch.NewFetchResolutionProviderWithParser(parser)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	resolved, err := resolver.Resolve(ctx, fetch.Resource{
		Type:  fetch.SourceURL,
		Value: srv.URL,
	})
	if err != nil {
		t.Fatalf("Resolve failed: %v", err)
	}
	if string(resolved.Body) != "just some plain text" {
		t.Errorf("expected raw body preserved, got %q", string(resolved.Body))
	}
	// No parser claims text/plain, so Parsed should be
	// empty — the resolver does not synthesize a result.
	if resolved.Parsed.Text != "" || resolved.Parsed.Title != "" {
		t.Errorf("expected empty Parsed for text/plain, got %+v", resolved.Parsed)
	}
}
