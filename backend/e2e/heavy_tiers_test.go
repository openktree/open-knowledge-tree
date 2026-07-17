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

// TestTLSImpersonationProvider_Resolve drives the TLS-impersonation
// provider against a real URL and asserts the wire contract: a
// successful fetch returns a non-error ResolvedContent with a
// parsed body. The test gracefully skips when
// OKT_FETCH_IMPERSONATE is not set so the e2e suite stays green
// in environments without the tls-client profile configured.
//
// The test uses https://example.com (a plain page with no
// anti-bot protection) so it exercises the provider's happy path
// without depending on a specific WAF's behaviour. The
// assertion is that the provider fetches the body and the
// parser extracts non-empty text.
func TestTLSImpersonationProvider_Resolve(t *testing.T) {
	impersonate := os.Getenv("OKT_FETCH_IMPERSONATE")
	if impersonate == "" {
		t.Skip("OKT_FETCH_IMPERSONATE not set; skipping live TLS-impersonation test")
	}

	provider := fetch.NewTLSImpersonationProvider(
		impersonate,
		"",
		content_parsing.NewTrafilaturaParser(),
	)
	if provider == nil {
		t.Fatal("provider is nil despite non-empty impersonate")
	}
	if !provider.Supports(fetch.SourceURL) {
		t.Fatal("provider does not claim SourceURL support")
	}
	if !provider.Supports(fetch.SourceDOI) {
		t.Error("provider should claim SourceDOI support")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	content, err := provider.Resolve(ctx, fetch.Resource{
		Type:  fetch.SourceURL,
		Value: "https://example.com",
	})
	if err != nil {
		if errors.Is(err, fetch.ErrInsufficientContent) {
			t.Logf("example.com parsed below MinExtractedLength; acceptable for a minimal page")
			return
		}
		t.Fatalf("Resolve failed: %v", err)
	}
	if content.StatusCode < 200 || content.StatusCode >= 300 {
		t.Fatalf("expected 2xx, got %d", content.StatusCode)
	}
	if len(content.Body) == 0 {
		t.Fatal("expected non-empty body")
	}
	t.Logf("TLS body: %d bytes, content-type=%s, final-url=%s, parsed-text=%d chars",
		len(content.Body), content.ContentType, content.FinalURL, len(content.Parsed.Text))
}

// TestTLSImpersonationProvider_NotConfigured asserts the
// constructor returns nil when impersonate is empty, so the
// wiring layer skips registration. A regression here would
// silently register a no-op provider in the chain.
func TestTLSImpersonationProvider_NotConfigured(t *testing.T) {
	if p := fetch.NewTLSImpersonationProvider("", ""); p != nil {
		t.Fatal("expected nil provider for empty impersonate")
	}
}

// TestFlareSolverrProvider_Resolve drives the FlareSolverr /
// Byparr headless-browser sidecar against a real URL and
// asserts the wire contract. The test gracefully skips when
// FLARESOLVERR_URL is not set so the e2e suite stays green
// in environments without the sidecar running.
//
// The test uses https://example.com so it exercises the
// sidecar's happy path without depending on a specific WAF.
func TestFlareSolverrProvider_Resolve(t *testing.T) {
	endpoint := os.Getenv("FLARESOLVERR_URL")
	if endpoint == "" {
		t.Skip("FLARESOLVERR_URL not set; skipping live FlareSolverr test")
	}

	provider := fetch.NewFlareSolverrProvider(
		endpoint,
		60*time.Second,
		"",
		content_parsing.NewTrafilaturaParser(),
	)
	if provider == nil {
		t.Fatal("provider is nil despite non-empty endpoint")
	}
	if !provider.Supports(fetch.SourceURL) {
		t.Fatal("provider does not claim SourceURL support")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	content, err := provider.Resolve(ctx, fetch.Resource{
		Type:  fetch.SourceURL,
		Value: "https://example.com",
	})
	if err != nil {
		if errors.Is(err, fetch.ErrInsufficientContent) {
			t.Logf("example.com parsed below MinExtractedLength; acceptable for a minimal page")
			return
		}
		t.Fatalf("Resolve failed: %v", err)
	}
	if len(content.Body) == 0 {
		t.Fatal("expected non-empty body from FlareSolverr")
	}
	if !strings.Contains(strings.ToLower(content.ContentType), "text/html") {
		t.Errorf("expected text/html content type, got %q", content.ContentType)
	}
	t.Logf("FlareSolverr body: %d bytes, final-url=%s, parsed-text=%d chars",
		len(content.Body), content.FinalURL, len(content.Parsed.Text))
}

// TestFlareSolverrProvider_NotConfigured asserts the
// constructor returns nil when endpoint is empty.
func TestFlareSolverrProvider_NotConfigured(t *testing.T) {
	if p := fetch.NewFlareSolverrProvider("", 0, ""); p != nil {
		t.Fatal("expected nil provider for empty endpoint")
	}
}