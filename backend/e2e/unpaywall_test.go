//go:build e2e

package e2e

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/openktree/open-knowledge-tree/backend/internal/config"
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
//
// The OA-host retry budget (403/timeout retry with
// retry_403_max_attempts=2) is exercised in the unit tests
// (TestUnpaywallRetryOn403, TestUnpaywallRetryOn5xxThenSuccess,
// TestUnpaywallRetryExhaustedSetsOARedirectURL,
// TestUnpaywallNoRetryOnInsufficientContent) since driving
// a transient 403 against the live Unpaywall API is not
// deterministic. This e2e test guards the happy path and
// the constructor wiring; the unit tests guard the retry
// classification and the OARedirectURL-preservation logic.
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

// TestUnpaywallResolutionProvider_RetryBudgetFromConfig
// guards the production wiring: the config-driven
// constructor must produce a provider whose OA-fetch retry
// budget matches the configured values, so a transient
// OA-host 403 actually retries instead of hard-failing.
// This is a structural test (no live API call) that
// complements the unit-test retry-classification coverage
// (TestUnpaywallRetryOn403 et al.) by asserting the
// cmd/app/api.go wiring threads the config fields into the
// provider rather than silently dropping them. It does
// not need UNPAYWALL_EMAIL because it checks the provider
// construction shape, not a live resolution.
func TestUnpaywallResolutionProvider_RetryBudgetFromConfig(t *testing.T) {
	// Mirror the production wiring in cmd/app/api.go: build
	// the RetryConfig from a config.UnpaywallProviderConfig
	// and pass it through the full-config constructor.
	cfg := config.UnpaywallProviderConfig{
		Email:   "user@example.com",
		Timeout: 60 * time.Second,
		Retry: config.FetchRetryConfig{
			MaxAttempts:         3,
			BaseDelay:           2 * time.Second,
			MaxDelay:            15 * time.Second,
			Retry403MaxAttempts: 2,
		},
	}
	retCfg := fetch.RetryConfig{
		MaxAttempts:         cfg.Retry.MaxAttempts,
		BaseDelay:           cfg.Retry.BaseDelay,
		MaxDelay:            cfg.Retry.MaxDelay,
		Retry403MaxAttempts: cfg.Retry.Retry403MaxAttempts,
	}
	provider := fetch.NewUnpaywallResolutionProviderWithFullConfig(
		cfg.Email, cfg.Timeout, retCfg,
		content_parsing.NewTrafilaturaParser(),
	)
	if provider == nil {
		t.Fatal("expected non-nil provider for valid config")
	}
	// The OA-fetch timeout must match the configured 60s,
	// not the old hardcoded 30s.
	if got := provider.HTTPClientTimeout(); got != 60*time.Second {
		t.Errorf("expected 60s OA-fetch timeout, got %v", got)
	}
	// The retry budget must be threaded: max_attempts=3
	// (enables retry), retry_403_max_attempts=2 (one 403
	// retry). A zero-value MaxAttempts would mean the
	// wiring dropped the config and the OA fetch never
	// retries — the exact regression this test guards.
	if got := provider.RetryConfig(); got.MaxAttempts != 3 {
		t.Errorf("expected MaxAttempts=3, got %d", got.MaxAttempts)
	}
	if got := provider.RetryConfig(); got.Retry403MaxAttempts != 2 {
		t.Errorf("expected Retry403MaxAttempts=2, got %d", got.Retry403MaxAttempts)
	}
}
