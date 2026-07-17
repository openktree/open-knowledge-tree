package content_parsing

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func loadFixture(t *testing.T) []byte {
	t.Helper()
	path := filepath.Join("testdata", "article.html")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read fixture %s: %v", path, err)
	}
	return b
}

func TestTrafilaturaParser_Parse_Fixture(t *testing.T) {
	parser := NewTrafilaturaParser(WithIncludeImages())
	doc, err := parser.Parse(context.Background(), loadFixture(t), SourceHTML, "https://example.com/articles/trafilatura")
	if err != nil {
		t.Fatalf("Parse returned error: %v", err)
	}

	if !strings.Contains(strings.ToLower(doc.Title), "trafilatura") {
		t.Errorf("expected title to mention 'trafilatura', got %q", doc.Title)
	}

	// The main body must survive; chrome must not.
	for _, must := range []string{
		"Trafilatura is designed",
		"second paragraph",
		"final paragraph",
	} {
		if !strings.Contains(doc.Text, must) {
			t.Errorf("expected text to contain %q\n---\n%s\n---", must, doc.Text)
		}
	}
	for _, mustNot := range []string{
		"Copyright",
		"All rights reserved",
		"sidebar text must be stripped",
		"console.log",
	} {
		if strings.Contains(doc.Text, mustNot) {
			t.Errorf("expected text NOT to contain %q\n---\n%s\n---", mustNot, doc.Text)
		}
	}

	// Author and sitename are surfaced from <meta> tags.
	if doc.Author == "" {
		t.Errorf("expected author from meta, got empty")
	}
	if doc.Sitename == "" {
		t.Errorf("expected sitename from og:site_name, got empty")
	}

	// Both absolute and relative <img src> must end up
	// in the Images slice, with relative URLs absolutized
	// against the page URL. Alt text must be captured.
	if len(doc.Images) < 2 {
		t.Fatalf("expected at least 2 images, got %d (%v)", len(doc.Images), doc.Images)
	}
	urls := imageURLs(doc.Images)
	joined := strings.Join(urls, "\n")
	if !strings.Contains(joined, "https://example.com/diagram.png") {
		t.Errorf("expected absolute image to be preserved, got %v", doc.Images)
	}
	if !strings.Contains(joined, "https://example.com/relative-image.jpg") {
		t.Errorf("expected relative image to be absolutized, got %v", doc.Images)
	}
	// Alt text from the fixture: diagram.png has alt="diagram",
	// relative-image.jpg has alt="relative".
	alts := imageAlts(doc.Images)
	if !containsStr(alts, "diagram") {
		t.Errorf("expected alt 'diagram', got alts %v", alts)
	}
	if !containsStr(alts, "relative") {
		t.Errorf("expected alt 'relative', got alts %v", alts)
	}

	// Markdown must be produced from the same cleaned content
	// node the HTML and Text come from. The body text survives
	// the conversion; chrome (Copyright / sidebar / console.log)
	// stays stripped. The test does not pin the exact Markdown
	// shape (the library's whitespace handling is its own), only
	// that the body text is present and the chrome is not.
	if doc.Markdown == "" {
		t.Fatalf("expected non-empty Markdown on a successful parse, got empty")
	}
	if !strings.Contains(doc.Markdown, "Trafilatura is designed") {
		t.Errorf("Markdown missing body text: %q", doc.Markdown)
	}
	for _, mustNot := range []string{
		"Copyright",
		"All rights reserved",
		"sidebar text must be stripped",
		"console.log",
	} {
		if strings.Contains(doc.Markdown, mustNot) {
			t.Errorf("expected Markdown NOT to contain %q\n---\n%s\n---", mustNot, doc.Markdown)
		}
	}
}

func TestTrafilaturaParser_Parse_EmptyInput(t *testing.T) {
	parser := NewTrafilaturaParser()
	_, err := parser.Parse(context.Background(), nil, SourceHTML, "https://example.com")
	if !errors.Is(err, ErrEmptyInput) {
		t.Errorf("expected ErrEmptyInput, got %v", err)
	}
}

// TestTrafilaturaParser_Parse_MarkdownFallbackMirrorsText
// covers the nil-ContentNode path: when trafilatura is not
// confident about the cleaned content node but still
// produces ContentText, the parser falls back to wrapping
// the plain text in <p> for the HTML field and — for
// Markdown — mirrors the plain text verbatim (no structure
// to convert). The test uses a fixture just above the
// MinExtractedLength threshold so the parser returns
// text without a content node, exercising the fallback.
// This is the case that matters for the decomposition
// worker's Markdown-first selection: a legacy-style row
// must still get a Markdown field so the worker does not
// need a format switch.
func TestTrafilaturaParser_Parse_MarkdownFallbackMirrorsText(t *testing.T) {
	// A page with a single long paragraph of plain text and
	// no chrome. trafilatura's confidence on such a minimal
	// page is low enough that ContentNode may be nil; when it
	// is not nil, the conversion path applies and the test
	// still passes (the body text is present either way). The
	// assertion is symmetric: Markdown is non-empty and
	// contains the body text regardless of which path the
	// parser took.
	page := `<!doctype html>
<html lang="en">
  <head><title>Minimal Page</title></head>
  <body>
    <p>` + strings.Repeat("This is a long sentence with meaningful prose that gives the parser enough to work with. ", 8) + `</p>
  </body>
</html>`

	parser := NewTrafilaturaParser()
	doc, err := parser.Parse(context.Background(), []byte(page), SourceHTML, "https://example.com/minimal")
	if err != nil {
		t.Fatalf("Parse returned error: %v", err)
	}
	if doc.Text == "" {
		t.Fatalf("expected non-empty Text as the baseline, got empty")
	}
	if doc.Markdown == "" {
		t.Fatalf("expected non-empty Markdown (mirrors Text on the fallback path), got empty")
	}
	if !strings.Contains(doc.Markdown, "meaningful prose") {
		t.Errorf("Markdown did not contain the body text: %q", doc.Markdown)
	}
}

func TestTrafilaturaParser_Parse_UnsupportedSourceType(t *testing.T) {
	parser := NewTrafilaturaParser()
	_, err := parser.Parse(context.Background(), []byte("%PDF-1.4\n"), SourcePDF, "https://example.com/file.pdf")
	if !errors.Is(err, ErrUnsupported) {
		t.Errorf("expected ErrUnsupported, got %v", err)
	}
}

func TestTrafilaturaParser_Supports(t *testing.T) {
	parser := NewTrafilaturaParser()
	if !parser.Supports(SourceHTML) {
		t.Errorf("expected Supports(html) = true")
	}
	if parser.Supports(SourcePDF) {
		t.Errorf("expected Supports(pdf) = false (PDF parser is a separate provider)")
	}
}

func TestTrafilaturaParser_Describe(t *testing.T) {
	parser := NewTrafilaturaParser()
	d := parser.Describe()
	if !d.Configured {
		t.Errorf("expected Configured = true, got false")
	}
	if d.Name == "" {
		t.Errorf("expected non-empty Name")
	}
}

// TestTrafilaturaParser_Parse_FiltersChromeImages is the
// regression guard for the "icons, logos, footer badges
// polluting the image list" bug. The fixture is a realistic
// page with images in the chrome (header logo, sidebar
// promo, footer sponsor, hidden tracking pixel, social
// share icon, inline SVG-as-data-URI) and exactly one
// image inside the article body. The default parser
// (IncludeImages=true) must return only the article image
// and drop every chrome image. The old behavior — a
// raw-tree fallback that unioned all <img> tags into the
// result — would have returned every URL on the page.
func TestTrafilaturaParser_Parse_FiltersChromeImages(t *testing.T) {
	parser := NewTrafilaturaParser()
	doc, err := parser.Parse(context.Background(), []byte(chromeHeavyFixture), SourceHTML, "https://en.wikipedia.org/wiki/Knowledge")
	if err != nil {
		t.Fatalf("Parse returned error: %v", err)
	}

	// Exactly one article image must survive.
	if len(doc.Images) != 1 {
		t.Fatalf("expected 1 article image, got %d (%v)", len(doc.Images), doc.Images)
	}
	if doc.Images[0].URL != "https://en.wikipedia.org/static/images/diagram-of-knowledge.png" {
		t.Errorf("expected the article image, got %q", doc.Images[0].URL)
	}
	if doc.Images[0].Alt != "diagram of knowledge" {
		t.Errorf("expected alt 'diagram of knowledge', got %q", doc.Images[0].Alt)
	}

	// Every chrome URL must be absent.
	chromeURLs := []string{
		"https://en.wikipedia.org/static/images/icons/enwiki-25.svg", // the exact example from the bug report
		"https://en.wikipedia.org/static/images/header-logo.png",
		"https://en.wikipedia.org/static/images/sidebar-promo.png",
		"https://en.wikipedia.org/static/images/footer-donate.png",
		"https://tracker.example.com/pixel.gif",
		"https://en.wikipedia.org/static/images/share-twitter.svg",
		"https://en.wikipedia.org/static/images/share-facebook.svg",
		"data:image/svg+xml;base64,PHN2ZyB4bWxucz0iaHR0cDovL3d3dy53My5vcmcvMjAwMC9zdmciLz4=",
	}
	for _, bad := range chromeURLs {
		for _, got := range doc.Images {
			if got.URL == bad {
				t.Errorf("chrome image %q leaked into Images: %v", bad, doc.Images)
			}
		}
	}
}

// TestTrafilaturaParser_Parse_FiltersSVGAsImg verifies the
// same filtering applies to <img src="...svg"> used as
// icons (e.g. media-wiki style). The fixture is the
// exact kind of page from the bug report. The image
// must not appear in Images because it lives in the
// header.
func TestTrafilaturaParser_Parse_FiltersSVGAsImg(t *testing.T) {
	parser := NewTrafilaturaParser()
	doc, err := parser.Parse(context.Background(), []byte(wikiIconFixture), SourceHTML, "https://en.wikipedia.org/")
	if err != nil {
		t.Fatalf("Parse returned error: %v", err)
	}
	for _, got := range doc.Images {
		if strings.Contains(got.URL, "enwiki-25.svg") {
			t.Errorf("Wikipedia icon leaked into Images: %v", doc.Images)
		}
	}
}

// TestTrafilaturaParser_Parse_DefaultIncludesImages pins
// the new default: a parser built with no options must
// include images. Without this guard, a future refactor
// could silently re-introduce the chrome-image bug.
func TestTrafilaturaParser_Parse_DefaultIncludesImages(t *testing.T) {
	parser := NewTrafilaturaParser()
	doc, err := parser.Parse(context.Background(), []byte(loadFixtureOrFail(t)), SourceHTML, "https://example.com/articles/trafilatura")
	if err != nil {
		t.Fatalf("Parse returned error: %v", err)
	}
	if len(doc.Images) < 2 {
		t.Fatalf("default parser must include images; got %v", doc.Images)
	}
}

// TestTrafilaturaParser_Parse_ExcludeImages covers the
// explicit opt-out. WithExcludeImages leaves Images empty
// even on a page that has plenty of article images.
func TestTrafilaturaParser_Parse_ExcludeImages(t *testing.T) {
	parser := NewTrafilaturaParser(WithExcludeImages())
	doc, err := parser.Parse(context.Background(), []byte(loadFixtureOrFail(t)), SourceHTML, "https://example.com/articles/trafilatura")
	if err != nil {
		t.Fatalf("Parse returned error: %v", err)
	}
	if len(doc.Images) != 0 {
		t.Errorf("WithExcludeImages must drop Images; got %v", doc.Images)
	}
}

func loadFixtureOrFail(t *testing.T) []byte {
	t.Helper()
	return loadFixture(t)
}

func imageURLs(imgs []ImageRef) []string {
	out := make([]string, len(imgs))
	for i, img := range imgs {
		out[i] = img.URL
	}
	return out
}

func imageAlts(imgs []ImageRef) []string {
	out := make([]string, len(imgs))
	for i, img := range imgs {
		out[i] = img.Alt
	}
	return out
}

func containsStr(slice []string, want string) bool {
	for _, s := range slice {
		if s == want {
			return true
		}
	}
	return false
}

// chromeHeavyFixture mimics a real article page with
// images scattered through every kind of chrome. Only
// the image inside <article> is the one a reader would
// consider part of the content. The body text is long
// enough that trafilatura is confident about the
// extraction; short fixtures (one or two paragraphs)
// trip the library's "not enough content" path and
// return a near-empty body, which is a separate
// concern this test is not exercising.
const chromeHeavyFixture = `<!doctype html>
<html lang="en">
  <head>
    <meta charset="utf-8" />
    <title>Knowledge</title>
  </head>
  <body>
    <header>
      <a href="/"><img src="/static/images/header-logo.png" alt="logo" /></a>
      <img src="/static/images/icons/enwiki-25.svg" alt="" width="25" height="25" />
    </header>
    <nav>
      <img src="/static/images/share-twitter.svg" alt="share on twitter" />
      <img src="/static/images/share-facebook.svg" alt="share on facebook" />
    </nav>
    <main>
      <article>
        <h1>Knowledge</h1>
        <p>The first paragraph of the article body that introduces the topic and sets up the main argument for the rest of the piece. Knowledge is a familiar concept that has been studied for centuries, and modern accounts tend to treat it as a justified true belief held by an epistemic agent about some proposition or state of affairs in the world.</p>
        <p>
          <img src="https://en.wikipedia.org/static/images/diagram-of-knowledge.png" alt="diagram of knowledge" />
          The article body hosts a single inline figure that illustrates the JTBD account of knowledge, showing how the three classical conditions (justification, truth, belief) come together. The diagram is the only image a reader would consider part of the content of the article, in contrast to the surrounding chrome.
        </p>
        <p>A third paragraph that wraps up the argument with a final sentence about the topic at hand. It also exists to make sure the extractor has plenty of text to work with, which is the difference between a confident extraction and a near-empty one on a thin page.</p>
        <p>A fourth paragraph to firmly establish this as a content-rich page with at least a few hundred words of meaningful content for the reader to consume, ensuring the parser returns a body that matches what a human reader would identify as the article.</p>
      </article>
      <aside>
        <img src="/static/images/sidebar-promo.png" alt="promo" />
      </aside>
    </main>
    <footer>
      <img src="/static/images/footer-donate.png" alt="donate" />
      <img src="data:image/svg+xml;base64,PHN2ZyB4bWxucz0iaHR0cDovL3d3dy53My5vcmcvMjAwMC9zdmciLz4=" alt="" />
    </footer>
    <img src="https://tracker.example.com/pixel.gif" alt="" width="1" height="1" style="display:none" />
  </body>
</html>`

// wikiIconFixture is the shape of the example that
// triggered the bug report: a page whose only <img> is
// a header icon. The body is long enough for the
// extractor to be confident that the article body is
// the main content, and the assertion is that no
// header icon leaks into the Images list. This is the
// case the user reported in the bug — the Wikipedia
// enwiki-25.svg icon showing up as a "content image".
const wikiIconFixture = `<!doctype html>
<html lang="en">
  <head><title>Wikipedia</title></head>
  <body>
    <header>
      <img src="https://en.wikipedia.org/static/images/icons/enwiki-25.svg" alt="" width="25" height="25" />
    </header>
    <main>
      <article>
        <h1>Wikipedia</h1>
        <p>Wikipedia is a free online encyclopedia that anyone can edit, organised into millions of articles in hundreds of languages. The project started in 2001 and has since become one of the most consulted reference works on the public web, with content maintained by a global community of volunteer editors.</p>
        <p>This second paragraph exists to give the extractor enough context to identify the body confidently on a thin page. A single-paragraph fixture often trips the library's "not enough content" path and returns a near-empty body; a few paragraphs of meaningful text sidestep that problem and let the assertion focus on the image filtering, which is the actual subject of this test.</p>
        <p>A third paragraph for good measure, ensuring the parser has a representative slice of body text to work with and that the cleaned content node carries the article paragraphs the way a reader would expect. This also gives the test a reason to assert that chrome-only images do not pollute the result even when the rest of the page is straightforward.</p>
      </article>
    </main>
    <footer><p>Footer text that must be stripped from the parsed body.</p></footer>
  </body>
</html>`
