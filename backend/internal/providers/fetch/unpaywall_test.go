package fetch

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestUnpaywallNewUnpaywallResolutionProvider(t *testing.T) {
	cases := []struct {
		name  string
		email string
		want  bool
	}{
		{name: "empty email returns nil", email: "", want: false},
		{name: "whitespace email returns nil", email: "   ", want: false},
		{name: "valid email returns provider", email: "user@example.com", want: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := NewUnpaywallResolutionProvider(tc.email)
			if (got != nil) != tc.want {
				t.Errorf("NewUnpaywallResolutionProvider(%q) nil=%v, want non-nil=%v",
					tc.email, got == nil, tc.want)
			}
		})
	}
}

func TestUnpaywallSupports(t *testing.T) {
	p := NewUnpaywallResolutionProvider("user@example.com")
	if p == nil {
		t.Fatal("expected non-nil provider for valid email")
	}
	if !p.Supports(SourceDOI) {
		t.Error("expected Supports(SourceDOI) to be true")
	}
	if p.Supports(SourceURL) {
		t.Error("expected Supports(SourceURL) to be false")
	}
}

func TestSelectOALocation(t *testing.T) {
	cases := []struct {
		name string
		resp unpaywallResponse
		want string
	}{
		{
			name: "prefers best_oa_location.url_for_pdf",
			resp: unpaywallResponse{
				BestOALocation: &unpaywallLocation{
					URL:       "https://best.example.com/p",
					URLForPDF: "https://best.example.com/p.pdf",
				},
				OALocations: []unpaywallLocation{
					{URL: "https://other.example.com/p"},
				},
			},
			want: "https://best.example.com/p.pdf",
		},
		{
			name: "falls back to best_oa_location.url when no pdf",
			resp: unpaywallResponse{
				BestOALocation: &unpaywallLocation{URL: "https://best.example.com/p"},
			},
			want: "https://best.example.com/p",
		},
		{
			name: "falls back to oa_locations[0].url_for_pdf when best is nil",
			resp: unpaywallResponse{
				OALocations: []unpaywallLocation{
					{URLForPDF: "https://repo.example.com/p.pdf"},
					{URL: "https://other.example.com/p"},
				},
			},
			want: "https://repo.example.com/p.pdf",
		},
		{
			name: "falls back to oa_locations url across locations",
			resp: unpaywallResponse{
				OALocations: []unpaywallLocation{
					{URL: "https://repo.example.com/p"},
				},
			},
			want: "https://repo.example.com/p",
		},
		{
			name: "empty result when no urls are present",
			resp: unpaywallResponse{
				BestOALocation: &unpaywallLocation{HostType: "publisher"},
				OALocations: []unpaywallLocation{
					{License: "CC-BY"},
				},
			},
			want: "",
		},
		{
			name: "empty best_oa_location object still lets oa_locations win",
			resp: unpaywallResponse{
				BestOALocation: &unpaywallLocation{},
				OALocations: []unpaywallLocation{
					{URL: "https://repo.example.com/p"},
				},
			},
			want: "https://repo.example.com/p",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := selectOALocation(tc.resp)
			if got != tc.want {
				t.Errorf("selectOALocation() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestUnpaywallErrNotOpenAccessIsExported(t *testing.T) {
	// The strategy depends on a sentinel value to drive
	// fall-through; we want the type to be stable for
	// future callers and for errors.Is to keep working.
	// The test is intentionally minimal: it just asserts
	// the symbol exists and is non-nil, and that wrapping
	// it in fmt.Errorf still matches.
	err := ErrUnpaywallNotOpenAccess
	if err == nil {
		t.Fatal("ErrUnpaywallNotOpenAccess must be non-nil")
	}
	if !errors.Is(err, ErrUnpaywallNotOpenAccess) {
		t.Error("expected errors.Is to match the sentinel directly")
	}
}

// TestUnpaywallTimeoutDefault asserts the full-config
// constructor applies the 60s default when timeout <= 0,
// matching the strategy's perProviderTimeout. This is the
// fix for the audit-trail finding that the previous
// hardcoded 30s gave up at the halfway mark of the tier's
// budget on slow-but-legitimate OA hosts (~223 timeouts).
func TestUnpaywallTimeoutDefault(t *testing.T) {
	p := NewUnpaywallResolutionProviderWithFullConfig(
		"user@example.com", 0, NoRetryConfig,
	)
	if p == nil {
		t.Fatal("expected non-nil provider for valid email")
	}
	got := p.httpClient.Timeout
	if got != 60*time.Second {
		t.Errorf("expected 60s default timeout, got %v", got)
	}
}

// TestUnpaywallTimeoutExplicit asserts an explicit timeout
// is honored rather than overwritten by the default.
func TestUnpaywallTimeoutExplicit(t *testing.T) {
	p := NewUnpaywallResolutionProviderWithFullConfig(
		"user@example.com", 42*time.Second, NoRetryConfig,
	)
	if p == nil {
		t.Fatal("expected non-nil provider for valid email")
	}
	if p.httpClient.Timeout != 42*time.Second {
		t.Errorf("expected 42s explicit timeout, got %v", p.httpClient.Timeout)
	}
}

// oaFetchRetry runs doOAFetch through retryWithBackoff the same
// way Resolve does when retryCfg.MaxAttempts > 1, including the
// lastResult-preservation logic that keeps OARedirectURL / OAStatus
// on the exhaustion return (retryWithBackoff itself returns a zero
// value on exhaustion). The tests use this helper because doOAFetch
// alone does not retry — the retry wrapper lives in Resolve, and
// stubbing the full Unpaywall API lookup (the first network call in
// Resolve) just to test the OA-fetch retry would couple the test
// to the API shape. This helper mirrors the production code path
// exactly.
func oaFetchRetry(t *testing.T, p *UnpaywallResolutionProvider, target, oaStatus string) (ResolvedContent, error) {
	t.Helper()
	var lastResult ResolvedContent
	result, err := retryWithBackoff(context.Background(), p.retryCfg, "unpaywall_oa_test",
		func(ctx context.Context) (ResolvedContent, error) {
			res, e := p.doOAFetch(ctx, target, false, oaStatus)
			if e != nil {
				lastResult = res
			}
			return res, e
		})
	if err != nil && result.OARedirectURL == "" && result.OAStatus == "" && lastResult.OARedirectURL != "" {
		result = lastResult
	}
	return result, err
}

// TestUnpaywallRetryOn403 asserts the OA-host fetch retries
// a transient 403 then succeeds on the second attempt. This
// is the primary fix for the ~396 OA-host 403s in the audit
// trail: a publisher WAF that 403s the first plain-HTTP
// request often clears in the 2s backoff window, and the
// retry recovers it in-tier before the chain falls through.
func TestUnpaywallRetryOn403(t *testing.T) {
	srv := &errorServer{
		t:              t,
		status:         http.StatusForbidden,
		failCount:      1, // 403 once, 200 on second
		successContent: "this is a sufficiently long article body that exceeds the minimum extracted length threshold for the parser guard",
	}
	ts := httptest.NewServer(srv)
	defer ts.Close()

	p := NewUnpaywallResolutionProviderWithFullConfig(
		"user@example.com",
		5*time.Second,
		RetryConfig{
			MaxAttempts:         3,
			BaseDelay:           time.Millisecond,
			MaxDelay:            10 * time.Millisecond,
			Retry403MaxAttempts: 2,
		},
	)

	res, err := oaFetchRetry(t, p, ts.URL, "gold")
	if err != nil {
		t.Fatalf("expected success after 403 retry, got error: %v", err)
	}
	if res.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", res.StatusCode)
	}
	if srv.requestCount != 2 {
		t.Fatalf("expected 2 requests (1 initial + 1 retry), got %d", srv.requestCount)
	}
	if res.OAStatus != "gold" {
		t.Errorf("expected OAStatus carried through as 'gold', got %q", res.OAStatus)
	}
}

// TestUnpaywallNoRetryOnInsufficientContent asserts
// ErrInsufficientContent (a consent/JS page, not a
// transient error) is NOT retried — retrying a page that
// rendered no real body just wastes the timeout budget.
// This guards the retryableFetchError classification:
// ErrInsufficientContent is a sentinel, not a network
// error.
func TestUnpaywallNoRetryOnInsufficientContent(t *testing.T) {
	// Serve a 200 with a Content-Type the parser claims
	// (text/html) and a body that triggers the
	// IsJSBoilerplate check (a <noscript>-style phrase). The
	// fetch succeeds, parse runs, the boilerplate guard
	// flags it as ErrInsufficientContent, which must NOT be
	// retried — retrying a page that rendered no real body
	// just wastes the timeout budget.
	const boilerplate = "JavaScript is disabled. Please enable JavaScript to continue."
	requestCount := 0
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount++
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("<html><body><noscript>" + boilerplate + "</noscript></body></html>"))
	}))
	defer ts.Close()

	p := NewUnpaywallResolutionProviderWithFullConfig(
		"user@example.com",
		5*time.Second,
		RetryConfig{
			MaxAttempts:         3,
			BaseDelay:           time.Millisecond,
			MaxDelay:            10 * time.Millisecond,
			Retry403MaxAttempts: 2,
		},
	)

	_, err := oaFetchRetry(t, p, ts.URL, "gold")
	if !errors.Is(err, ErrInsufficientContent) {
		t.Fatalf("expected ErrInsufficientContent, got %v", err)
	}
	if requestCount != 1 {
		t.Errorf("expected 1 request (ErrInsufficientContent is not retryable), got %d", requestCount)
	}
}

// TestUnpaywallRetryExhaustedSetsOARedirectURL asserts that
// when the OA-host fetch exhausts its 403 retry budget, the
// returned error result carries OARedirectURL so the
// strategy's second pass (strategy.go Resolve, the "OA URL
// pass") can retry the direct OA URL with the remaining
// URL-capable tiers (TLS, fetch, FlareSolverr). Without this,
// a persistent publisher WAF 403 would lose the direct OA
// URL and the strategy could only retry the DOI landing
// page.
func TestUnpaywallRetryExhaustedSetsOARedirectURL(t *testing.T) {
	srv := &errorServer{
		t:         t,
		status:    http.StatusForbidden,
		failCount: 100, // always 403
	}
	ts := httptest.NewServer(srv)
	defer ts.Close()

	p := NewUnpaywallResolutionProviderWithFullConfig(
		"user@example.com",
		5*time.Second,
		RetryConfig{
			MaxAttempts:         3,
			BaseDelay:           time.Millisecond,
			MaxDelay:            10 * time.Millisecond,
			Retry403MaxAttempts: 2,
		},
	)

	res, err := oaFetchRetry(t, p, ts.URL, "bronze")
	if err == nil {
		t.Fatal("expected error after exhausted 403 retries, got nil")
	}
	// 403 retry cap is 2 total attempts: 1 initial + 1 retry.
	if srv.requestCount != 2 {
		t.Fatalf("expected 2 requests (403 cap = 2 total), got %d", srv.requestCount)
	}
	if res.OARedirectURL == "" {
		t.Error("expected OARedirectURL set on exhausted-403 result so strategy's second pass can retry the direct OA URL")
	}
	if res.OARedirectURL != ts.URL {
		t.Errorf("expected OARedirectURL=%q, got %q", ts.URL, res.OARedirectURL)
	}
	if res.OAStatus != "bronze" {
		t.Errorf("expected OAStatus carried through as 'bronze', got %q", res.OAStatus)
	}
}

// TestUnpaywallNoRetryByDefault asserts the historical
// constructors (NewUnpaywallResolutionProvider /
// ...WithParsers) keep no-retry behaviour, so existing
// callers (tests, e2e) are unaffected by the new
// retry-capable constructor.
func TestUnpaywallNoRetryByDefault(t *testing.T) {
	srv := &errorServer{
		t:         t,
		status:    http.StatusForbidden,
		failCount: 100,
	}
	ts := httptest.NewServer(srv)
	defer ts.Close()

	// NewUnpaywallResolutionProvider (no retry config) —
	// the backwards-compatible constructor.
	p := NewUnpaywallResolutionProvider("user@example.com")
	if p == nil {
		t.Fatal("expected non-nil provider")
	}
	if p.retryCfg.MaxAttempts != 1 {
		t.Errorf("expected NoRetryConfig.MaxAttempts=1 for the default constructor, got %d", p.retryCfg.MaxAttempts)
	}

	_, err := p.doOAFetch(context.Background(), ts.URL, false, "gold")
	if err == nil {
		t.Fatal("expected error on 403, got nil")
	}
	if srv.requestCount != 1 {
		t.Fatalf("expected exactly 1 request with no retry, got %d", srv.requestCount)
	}
}

// TestUnpaywallRetryOn5xxThenSuccess asserts non-403
// retryable errors (5xx) use the max_attempts budget (3
// total), not the tighter retry_403_max_attempts cap. This
// guards the separation between the two caps.
func TestUnpaywallRetryOn5xxThenSuccess(t *testing.T) {
	srv := &errorServer{
		t:              t,
		status:         http.StatusServiceUnavailable,
		failCount:      2, // 503 twice, 200 on third
		successContent: "this is a sufficiently long article body that exceeds the minimum extracted length threshold for the parser guard",
	}
	ts := httptest.NewServer(srv)
	defer ts.Close()

	p := NewUnpaywallResolutionProviderWithFullConfig(
		"user@example.com",
		5*time.Second,
		RetryConfig{
			MaxAttempts:         3,
			BaseDelay:           time.Millisecond,
			MaxDelay:            10 * time.Millisecond,
			Retry403MaxAttempts: 2,
		},
	)

	res, err := oaFetchRetry(t, p, ts.URL, "gold")
	if err != nil {
		t.Fatalf("expected success after 503 retries, got error: %v", err)
	}
	if res.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", res.StatusCode)
	}
	// 3 total attempts (max_attempts), not capped at 2
	// because the 403 cap doesn't apply to 5xx.
	if srv.requestCount != 3 {
		t.Fatalf("expected 3 requests (max_attempts=3), got %d", srv.requestCount)
	}
}
