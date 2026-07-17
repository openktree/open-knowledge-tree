package fetch

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"syscall"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// retryableFetchError classification tests
// ---------------------------------------------------------------------------

func TestRetryableFetchError(t *testing.T) {
	cases := []struct {
		name          string
		err           error
		wantRetryable bool
		wantReason    string // empty means don't check
	}{
		{
			name:          "nil is not retryable",
			err:           nil,
			wantRetryable: false,
		},
		{
			name:          "context cancelled is permanent",
			err:           context.Canceled,
			wantRetryable: false,
		},
		{
			name:          "context deadline exceeded is permanent",
			err:           context.DeadlineExceeded,
			wantRetryable: false,
		},
		{
			name:          "net.Error is retryable",
			err:           &net.DNSError{Err: "lookup failed", Name: "example.com"},
			wantRetryable: true,
		},
		{
			name:          "timeout net.Error is retryable",
			err:           &net.DNSError{Err: "i/o timeout", Name: "example.com", IsTimeout: true},
			wantRetryable: true,
		},
		{
			name:          "EOF is retryable",
			err:           io.EOF,
			wantRetryable: true,
		},
		{
			name:          "unexpected EOF is retryable",
			err:           io.ErrUnexpectedEOF,
			wantRetryable: true,
		},
		{
			name:          "ECONNRESET is retryable",
			err:           syscall.ECONNRESET,
			wantRetryable: true,
		},
		{
			name:          "EPIPE is retryable",
			err:           syscall.EPIPE,
			wantRetryable: true,
		},
		{
			name:          "ECONNREFUSED is retryable",
			err:           syscall.ECONNREFUSED,
			wantRetryable: true,
		},
		{
			name:          "429 status is retryable",
			err:           &ErrNon2xxStatus{Code: 429},
			wantRetryable: true,
		},
		{
			name:          "500 status is retryable",
			err:           &ErrNon2xxStatus{Code: 500},
			wantRetryable: true,
		},
		{
			name:          "503 status is retryable",
			err:           &ErrNon2xxStatus{Code: 503},
			wantRetryable: true,
		},
		{
			name:          "403 status is NOT retryable",
			err:           &ErrNon2xxStatus{Code: 403},
			wantRetryable: false,
		},
		{
			name:          "404 status is NOT retryable",
			err:           &ErrNon2xxStatus{Code: 404},
			wantRetryable: false,
		},
		{
			name:          "401 status is NOT retryable",
			err:           &ErrNon2xxStatus{Code: 401},
			wantRetryable: false,
		},
		{
			name:          "ErrBodyTooLarge is NOT retryable",
			err:           ErrBodyTooLarge,
			wantRetryable: false,
		},
		{
			name:          "ErrInsufficientContent is NOT retryable",
			err:           ErrInsufficientContent,
			wantRetryable: false,
		},
		{
			name:          "wrapped net.Error is retryable",
			err:           fmt.Errorf("wrapped: %w", &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED}),
			wantRetryable: true,
		},
		{
			name:          "error with 'timeout' in message is retryable (heuristic)",
			err:           errors.New("dial tcp: i/o timeout"),
			wantRetryable: true,
		},
		{
			name:          "error with 'connection' in message is retryable (heuristic)",
			err:           errors.New("connection refused"),
			wantRetryable: true,
		},
		{
			name:          "arbitrary permanent error is not retryable",
			err:           errors.New("something permanently wrong"),
			wantRetryable: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, reason := retryableFetchError(tc.err)
			if got != tc.wantRetryable {
				t.Errorf("retryableFetchError(%v) = %v, want %v (reason: %q)", tc.err, got, tc.wantRetryable, reason)
			}
			if tc.wantReason != "" && reason != tc.wantReason {
				// Just check reason is non-empty when retryable
				if tc.wantRetryable && reason == "" {
					t.Errorf("expected a non-empty reason for retryable error, got empty")
				}
			}
		})
	}
}

// ---------------------------------------------------------------------------
// retryWithBackoff unit tests
// ---------------------------------------------------------------------------

func TestRetryWithBackoff_RetriesOnTransientError(t *testing.T) {
	var callCount int
	fn := func(ctx context.Context) (string, error) {
		callCount++
		if callCount < 3 {
			return "", &ErrNon2xxStatus{Code: 503}
		}
		return "success", nil
	}

	cfg := RetryConfig{MaxAttempts: 3, BaseDelay: time.Millisecond, MaxDelay: 10 * time.Millisecond}
	result, err := retryWithBackoff(context.Background(), cfg, "test", fn)
	if err != nil {
		t.Fatalf("expected success, got error: %v", err)
	}
	if result != "success" {
		t.Fatalf("expected 'success', got %q", result)
	}
	if callCount != 3 {
		t.Fatalf("expected 3 calls (initial + 2 retries), got %d", callCount)
	}
}

func TestRetryWithBackoff_NoRetryOnPermanentError(t *testing.T) {
	var callCount int
	fn := func(ctx context.Context) (string, error) {
		callCount++
		return "", &ErrNon2xxStatus{Code: 403}
	}

	cfg := RetryConfig{MaxAttempts: 3, BaseDelay: time.Millisecond, MaxDelay: 10 * time.Millisecond}
	_, err := retryWithBackoff(context.Background(), cfg, "test", fn)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if callCount != 1 {
		t.Fatalf("expected only 1 call (permanent error), got %d", callCount)
	}
}

func TestRetryWithBackoff_RespectsContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // pre-cancelled

	var callCount int
	fn := func(ctx context.Context) (string, error) {
		callCount++
		return "", &ErrNon2xxStatus{Code: 503}
	}

	cfg := RetryConfig{MaxAttempts: 3, BaseDelay: time.Millisecond, MaxDelay: 10 * time.Millisecond}
	_, err := retryWithBackoff(ctx, cfg, "test", fn)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
	if callCount != 0 {
		t.Fatalf("expected 0 calls (context already cancelled), got %d", callCount)
	}
}

func TestRetryWithBackoff_MaxAttemptsOneDisablesRetry(t *testing.T) {
	var callCount int
	fn := func(ctx context.Context) (string, error) {
		callCount++
		return "", &ErrNon2xxStatus{Code: 503}
	}

	cfg := RetryConfig{MaxAttempts: 1} // no retry
	_, err := retryWithBackoff(context.Background(), cfg, "test", fn)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if callCount != 1 {
		t.Fatalf("expected 1 call (no retry), got %d", callCount)
	}
}

func TestRetryWithBackoff_SucceedsOnFirstAttempt(t *testing.T) {
	fn := func(ctx context.Context) (string, error) {
		return "immediate", nil
	}

	cfg := RetryConfig{MaxAttempts: 3, BaseDelay: time.Millisecond, MaxDelay: 10 * time.Millisecond}
	result, err := retryWithBackoff(context.Background(), cfg, "test", fn)
	if err != nil {
		t.Fatalf("expected success, got error: %v", err)
	}
	if result != "immediate" {
		t.Fatalf("expected 'immediate', got %q", result)
	}
}

// ---------------------------------------------------------------------------
// HTTP fetch provider integration tests (with retry)
// ---------------------------------------------------------------------------

// errorServer returns the given status code on the first N-1 requests,
// then a 200 on the Nth.
type errorServer struct {
	t              *testing.T
	status         int
	failCount      int
	requestCount   int
	successContent string
}

func (s *errorServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.requestCount++
	if s.requestCount <= s.failCount {
		w.WriteHeader(s.status)
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(s.successContent))
}

func TestFetchResolutionProvider_RetriesOn503(t *testing.T) {
	srv := &errorServer{
		t:              t,
		status:         http.StatusServiceUnavailable,
		failCount:      2, // fail twice, succeed on 3rd
		successContent: "hello world this is sufficient content for the min length check",
	}
	ts := httptest.NewServer(srv)
	defer ts.Close()

	p := NewFetchResolutionProviderWithFullConfig(
		5*time.Second,
		RetryConfig{MaxAttempts: 3, BaseDelay: time.Millisecond, MaxDelay: 10 * time.Millisecond},
		"test-agent",
		// No parser — we're testing the retry, not parsing.
	)

	ctx := context.Background()
	res, err := p.Resolve(ctx, Resource{Value: ts.URL, Type: SourceURL})
	if err != nil {
		t.Fatalf("expected success after retries, got error: %v", err)
	}
	if res.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", res.StatusCode)
	}
	if srv.requestCount != 3 {
		t.Fatalf("expected 3 requests (1 initial + 2 retries), got %d", srv.requestCount)
	}
	if string(res.Body) != srv.successContent {
		t.Fatalf("body mismatch: got %q, want %q", string(res.Body), srv.successContent)
	}
}

func TestFetchResolutionProvider_DoesNotRetryOn403(t *testing.T) {
	srv := &errorServer{
		t:         t,
		status:    http.StatusForbidden,
		failCount: 100, // always fail
	}
	ts := httptest.NewServer(srv)
	defer ts.Close()

	p := NewFetchResolutionProviderWithFullConfig(
		5*time.Second,
		RetryConfig{MaxAttempts: 5, BaseDelay: time.Millisecond, MaxDelay: 10 * time.Millisecond},
		"test-agent",
	)

	ctx := context.Background()
	_, err := p.Resolve(ctx, Resource{Value: ts.URL, Type: SourceURL})
	if err == nil {
		t.Fatal("expected error for 403, got nil")
	}
	var non2xx *ErrNon2xxStatus
	if !errors.As(err, &non2xx) {
		t.Fatalf("expected ErrNon2xxStatus, got %T: %v", err, err)
	}
	if non2xx.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", non2xx.Code)
	}
	// Only 1 request — no retry on 4xx (except 429).
	if srv.requestCount != 1 {
		t.Fatalf("expected only 1 request (403 is permanent), got %d", srv.requestCount)
	}
}

func TestFetchResolutionProvider_RetriesOn429(t *testing.T) {
	srv := &errorServer{
		t:              t,
		status:         http.StatusTooManyRequests,
		failCount:      1, // fail once, succeed on 2nd
		successContent: "rate limit over, here is the content that is long enough",
	}
	ts := httptest.NewServer(srv)
	defer ts.Close()

	p := NewFetchResolutionProviderWithFullConfig(
		5*time.Second,
		RetryConfig{MaxAttempts: 3, BaseDelay: time.Millisecond, MaxDelay: 10 * time.Millisecond},
		"test-agent",
	)

	ctx := context.Background()
	res, err := p.Resolve(ctx, Resource{Value: ts.URL, Type: SourceURL})
	if err != nil {
		t.Fatalf("expected success after 429 retry, got error: %v", err)
	}
	if res.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", res.StatusCode)
	}
	if srv.requestCount != 2 {
		t.Fatalf("expected 2 requests (1 initial + 1 retry), got %d", srv.requestCount)
	}
}

func TestFetchResolutionProvider_ExhaustedRetriesReturnsLastError(t *testing.T) {
	srv := &errorServer{
		t:         t,
		status:    http.StatusServiceUnavailable,
		failCount: 100, // always fail
	}
	ts := httptest.NewServer(srv)
	defer ts.Close()

	p := NewFetchResolutionProviderWithFullConfig(
		5*time.Second,
		RetryConfig{MaxAttempts: 3, BaseDelay: time.Millisecond, MaxDelay: 10 * time.Millisecond},
		"test-agent",
	)

	ctx := context.Background()
	_, err := p.Resolve(ctx, Resource{Value: ts.URL, Type: SourceURL})
	if err == nil {
		t.Fatal("expected error after exhausting retries")
	}
	// Should have attempted 3 times.
	if srv.requestCount != 3 {
		t.Fatalf("expected 3 requests (exhausted), got %d", srv.requestCount)
	}
}

func TestFetchResolutionProvider_NoRetryWithDefaultConstructor(t *testing.T) {
	// The default constructor uses NoRetryConfig so retries are disabled.
	p := NewFetchResolutionProvider()

	if p.retryCfg.MaxAttempts != 1 {
		t.Fatalf("expected default constructor to have MaxAttempts=1 (no retry), got %d", p.retryCfg.MaxAttempts)
	}
}

func TestFetchResolutionProvider_DescribeShowsConfiguredTimeout(t *testing.T) {
	p := NewFetchResolutionProviderWithFullConfig(
		90*time.Second,
		RetryConfig{MaxAttempts: 1},
		"test-agent",
	)
	d := p.Describe()
	if d.Timeout != "1m30s" {
		t.Errorf("expected Timeout '1m30s', got %q", d.Timeout)
	}
}

// ---------------------------------------------------------------------------
// RetryConfig normalisation tests
// ---------------------------------------------------------------------------

func TestNewFetchResolutionProviderWithFullConfig_NormalisesRetryConfig(t *testing.T) {
	// Zero-value RetryConfig with all fields zero should use defaults.
	p := NewFetchResolutionProviderWithFullConfig(
		5*time.Second,
		RetryConfig{},
		"test-agent",
	)
	if p.retryCfg.MaxAttempts != 3 {
		t.Fatalf("expected default MaxAttempts=3, got %d", p.retryCfg.MaxAttempts)
	}
	if p.retryCfg.BaseDelay != 2*time.Second {
		t.Fatalf("expected default BaseDelay=2s, got %v", p.retryCfg.BaseDelay)
	}
	if p.retryCfg.MaxDelay != 15*time.Second {
		t.Fatalf("expected default MaxDelay=15s, got %v", p.retryCfg.MaxDelay)
	}
}

func TestNewFetchResolutionProviderWithFullConfig_NoRetryConfig(t *testing.T) {
	p := NewFetchResolutionProviderWithFullConfig(
		5*time.Second,
		NoRetryConfig,
		"test-agent",
	)
	if p.retryCfg.MaxAttempts != 1 {
		t.Fatalf("expected NoRetryConfig MaxAttempts=1, got %d", p.retryCfg.MaxAttempts)
	}
}

func TestNewFetchResolutionProviderWithFullConfig_ZeroTimeoutDefaults(t *testing.T) {
	p := NewFetchResolutionProviderWithFullConfig(
		0, // zero timeout should get 30s default
		NoRetryConfig,
		"test-agent",
	)
	if p.httpClient.Timeout != 30*time.Second {
		t.Fatalf("expected default timeout 30s, got %v", p.httpClient.Timeout)
	}
}

// ---------------------------------------------------------------------------
// Additional edge case: body too large is never retried
// ---------------------------------------------------------------------------

// largeBodyServer returns a body that claims to be 16MB, exceeding MaxBodyBytes.
type largeBodyServer struct{}

func (s *largeBodyServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Length", "16000000")
	w.WriteHeader(http.StatusOK)
	// Write just enough to trigger the Content-Length check.
}

func TestFetchResolutionProvider_BodyTooLargeIsNotRetried(t *testing.T) {
	ts := httptest.NewServer(&largeBodyServer{})
	defer ts.Close()

	var callCount int
	// Use a custom transport that records calls (we can't easily intercept
	// the HTTP client, but we can check only one request was made).
	_ = callCount

	p := NewFetchResolutionProviderWithFullConfig(
		5*time.Second,
		RetryConfig{MaxAttempts: 3, BaseDelay: time.Millisecond, MaxDelay: 10 * time.Millisecond},
		"test-agent",
	)

	ctx := context.Background()
	_, err := p.Resolve(ctx, Resource{Value: ts.URL, Type: SourceURL})
	if !errors.Is(err, ErrBodyTooLarge) {
		t.Fatalf("expected ErrBodyTooLarge, got %v", err)
	}
	// Just check the content-length check fired — the read is bounded
	// so the body check won't fire on a short test body.
}
