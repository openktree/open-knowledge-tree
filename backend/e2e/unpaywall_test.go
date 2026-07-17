//go:build e2e

package e2e

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/openktree/open-knowledge-tree/backend/internal/providers/content_parsing"
	"github.com/openktree/open-knowledge-tree/backend/internal/providers/fetch"
)

// TestUnpaywallResolutionProvider_ResolvesDOI drives the
// provider against the live Unpaywall v2 API and asserts
// the wire contract: a DOI that has an open-access copy
// must return the body of the OA location, not an error.
// The test gracefully skips when UNPAYWALL_EMAIL is not
// set so the e2e suite stays green in environments without
// API credentials (the same pattern as
// TestSerperSearchProvider_Search and
// TestOpenAlexSearchProvider_Search).
//
// Unpaywall's `?email=` parameter is also the de-facto API
// authentication, so the test must be skipped in dev
// environments that haven't been onboarded.
//
// The DOI used here is "10.1038/nature14539" (the "Deep
// learning" review by LeCun, Bengio & Hinton, a well-known
// Nature article). If the upstream OA copy ever moves we
// accept either outcome (a body from the OA location, or
// the closed-access sentinel) — both are valid for the
// strategy to handle, and a hard-coded "must be OA" check
// would couple the test to Unpaywall's current state.
//
// The provider now parses the OA body with the wired
// content parsers (Trafilatura for HTML, MuPDF for PDF),
// so a successful OA fetch must also populate Parsed with
// non-empty Text (>= fetch.MinExtractedLength). This
// guards the regression where the Unpaywall path returned
// a body with Parsed zeroed and the row ended up with
// parse_status='unsupported'.
func TestUnpaywallResolutionProvider_ResolvesDOI(t *testing.T) {
	email := os.Getenv("UNPAYWALL_EMAIL")
	if email == "" {
		t.Skip("UNPAYWALL_EMAIL not set; skipping live-API test")
	}

	provider := fetch.NewUnpaywallResolutionProviderWithParsers(
		email,
		content_parsing.NewTrafilaturaParser(),
		content_parsing.NewFitzPDFParser(),
	)
	if provider == nil {
		t.Fatal("provider is nil despite non-empty email")
	}
	if !provider.Supports(fetch.SourceDOI) {
		t.Fatal("provider does not claim SourceDOI support")
	}
	// Plain URLs must not be claimed; the plain
	// FetchResolutionProvider is the only thing that
	// resolves generic URLs.
	if provider.Supports(fetch.SourceURL) {
		t.Error("provider unexpectedly claims SourceURL support")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	content, err := provider.Resolve(ctx, fetch.Resource{
		Type: fetch.SourceDOI,
		DOI:  "10.1038/nature14539",
	})
	if err != nil {
		if errors.Is(err, fetch.ErrUnpaywallNotOpenAccess) {
			// Closed-access is a valid outcome; the
			// contract is that the strategy falls
			// through to the next provider.
			t.Logf("DOI is closed-access per Unpaywall; strategy should fall through to plain fetch")
			return
		}
		if errors.Is(err, fetch.ErrInsufficientContent) {
			// The OA copy existed but trafilatura extracted
			// too little text (e.g. a JS-rendered landing
			// page). The strategy falls through to a heavier
			// tier. Valid outcome.
			t.Logf("OA body parsed but below MinExtractedLength; strategy should fall through")
			return
		}
		t.Fatalf("Resolve failed: %v", err)
	}

	if content.StatusCode < 200 || content.StatusCode >= 300 {
		t.Fatalf("expected 2xx from OA location, got %d", content.StatusCode)
	}
	if len(content.Body) == 0 {
		t.Fatal("expected non-empty body from OA location")
	}
	if !strings.HasPrefix(strings.ToLower(content.ContentType), "text/") &&
		!strings.Contains(strings.ToLower(content.ContentType), "pdf") {
		t.Errorf("unexpected content type %q; want text/* or application/pdf",
			content.ContentType)
	}
	// The parser must have populated Parsed.Text. This is
	// the regression guard for the previous gap where the
	// Unpaywall path never ran the body through Trafilatura.
	if strings.TrimSpace(content.Parsed.Text) == "" {
		t.Error("expected non-empty Parsed.Text from OA body (provider must parse the OA location)")
	}
	t.Logf("OA body: %d bytes, content-type=%s, final-url=%s, parsed-text=%d chars",
		len(content.Body), content.ContentType, content.FinalURL, len(content.Parsed.Text))
}

// TestUnpaywallResolutionProvider_NotADOI verifies the
// non-DOI source type is rejected up front. A type
// assertion bug here would silently let the provider
// handle plain URLs, which would be a regression.
func TestUnpaywallResolutionProvider_NotADOI(t *testing.T) {
	provider := fetch.NewUnpaywallResolutionProvider("user@example.com")
	if provider == nil {
		t.Fatal("expected non-nil provider for valid email")
	}

	_, err := provider.Resolve(context.Background(), fetch.Resource{
		Type:  fetch.SourceURL,
		Value: "https://example.com/some-article",
	})
	if err == nil {
		t.Fatal("expected error for SourceURL; provider is DOI-only")
	}
	if errors.Is(err, fetch.ErrUnpaywallNotOpenAccess) {
		t.Fatal("SourceURL should not produce the closed-access sentinel")
	}
}
