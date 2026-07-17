package decomposition

import (
	"context"
	"errors"
	"io"
	"net"
	"syscall"
	"testing"
	"time"
)

func TestRetryClassifyError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"ctx cancelled", context.Canceled, false},
		{"ctx deadline", context.DeadlineExceeded, false},
		{"EOF", io.EOF, true},
		{"unexpected EOF", io.ErrUnexpectedEOF, true},
		{"ECONNRESET", syscall.ECONNRESET, true},
		{"EPIPE", syscall.EPIPE, true},
		{"ECONNREFUSED", syscall.ECONNREFUSED, true},
		{"net op error timeout", &net.OpError{Op: "dial", Net: "tcp", Err: &timeoutErr{}}, true},
		{"status 429", errStatus("openrouter: status 429: rate limited"), true},
		{"status 500", errStatus("openrouter: status 500: internal"), true},
		{"status 502", errStatus("openrouter: status 502: bad gateway"), true},
		{"status 503", errStatus("openrouter: status 503: unavailable"), true},
		{"status 504", errStatus("openrouter: status 504: gateway timeout"), true},
		{"status 401", errStatus("openrouter: status 401: unauthorized"), false},
		{"status 403", errStatus("openrouter: status 403: forbidden"), false},
		{"status 400", errStatus("openrouter: status 400: bad request"), false},
		{"timeout in message", errStatus("openrouter: request failed: dial tcp: i/o timeout"), true},
		{"connection in message", errStatus("openrouter: request failed: connection refused"), true},
		{"json parse error", errStatus("fact extraction: failed to parse response as JSON array: unexpected end of JSON"), false},
		{"status 503 substring unrelated", errStatus("model id is google/gemma-503b-it but auth failed"), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, _ := retryClassifyError(c.err)
			if got != c.want {
				t.Errorf("retryClassifyError(%v) = %v, want %v", c.err, got, c.want)
			}
		})
	}
}

type errStatus string

func (e errStatus) Error() string { return string(e) }
func (e errStatus) Unwrap() error { return nil }

type timeoutErr struct{}

func (timeoutErr) Error() string   { return "i/o timeout" }
func (timeoutErr) Timeout() bool   { return true }
func (timeoutErr) Temporary() bool { return true }

func TestRetryWithBackoff_RetriesThenSucceeds(t *testing.T) {
	calls := 0
	cfg := retryConfig{MaxAttempts: 4, BaseDelay: time.Millisecond, MaxDelay: 5 * time.Millisecond, PerCallTO: 0}
	res, err := retryWithBackoff(context.Background(), cfg, "test", func(ctx context.Context) (string, error) {
		calls++
		if calls < 3 {
			return "", errStatus("openrouter: status 429: rate limited")
		}
		return "ok", nil
	})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if res != "ok" {
		t.Errorf("res = %q, want ok", res)
	}
	if calls != 3 {
		t.Errorf("calls = %d, want 3 (2 retries + 1 success)", calls)
	}
}

func TestRetryWithBackoff_PermanentErrorNoRetry(t *testing.T) {
	calls := 0
	cfg := retryConfig{MaxAttempts: 4, BaseDelay: time.Millisecond, MaxDelay: 5 * time.Millisecond, PerCallTO: 0}
	_, err := retryWithBackoff(context.Background(), cfg, "test", func(ctx context.Context) (string, error) {
		calls++
		return "", errStatus("openrouter: status 401: unauthorized")
	})
	if err == nil {
		t.Fatal("expected err, got nil")
	}
	if calls != 1 {
		t.Errorf("calls = %d, want 1 (permanent error must not retry)", calls)
	}
}

func TestRetryWithBackoff_RespectsContextCancellation(t *testing.T) {
	calls := 0
	cfg := retryConfig{MaxAttempts: 4, BaseDelay: 50 * time.Millisecond, MaxDelay: 100 * time.Millisecond, PerCallTO: 0}
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // pre-cancelled
	_, err := retryWithBackoff(ctx, cfg, "test", func(ctx context.Context) (string, error) {
		calls++
		return "", errStatus("openrouter: status 429: rate limited")
	})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
	if calls != 0 {
		t.Errorf("calls = %d, want 0 (pre-cancelled ctx must skip the call entirely)", calls)
	}
}

func TestClassifyHTTPStatus(t *testing.T) {
	if !classifyHTTPStatus(429) {
		t.Error("429 should be retryable")
	}
	if !classifyHTTPStatus(500) {
		t.Error("500 should be retryable")
	}
	if !classifyHTTPStatus(503) {
		t.Error("503 should be retryable")
	}
	if classifyHTTPStatus(401) {
		t.Error("401 should not be retryable")
	}
	if classifyHTTPStatus(400) {
		t.Error("400 should not be retryable")
	}
}