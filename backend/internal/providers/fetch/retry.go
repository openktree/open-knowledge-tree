package fetch

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net"
	"strings"
	"syscall"
	"time"
)

// RetryConfig governs the retry-with-backoff helper used by the
// HTTP fetch and TLS impersonation providers. A MaxAttempts of 1
// disables retry (just one attempt, no backoff). The zero value
// is safe: NewFetchResolutionProviderWithFullConfig normalises it
// to defaultRetryConfig.
type RetryConfig struct {
	// MaxAttempts is the total number of attempts including the
	// first. 1 means no retry (run once and done). 3 means the
	// initial attempt plus two retries.
	MaxAttempts int

	// BaseDelay is the backoff duration for the first retry
	// (attempt 2). Subsequent delays double: BaseDelay,
	// BaseDelay*2, BaseDelay*4, ... capped at MaxDelay.
	BaseDelay time.Duration

	// MaxDelay caps the per-attempt backoff so retries don't
	// accumulate unbounded delay.
	MaxDelay time.Duration

	// Retry403MaxAttempts caps the number of retries for HTTP 403
	// responses specifically. Many WAFs (Reddit, Cloudflare) emit
	// 403 under burst but return 200/429 in quiet windows, so a
	// transient 403 is worth a short retry. A large retry budget
	// on 403 would waste time on a truly-blocked host, so this is
	// capped separately from MaxAttempts. 0 disables 403 retry
	// (403 is treated as permanent, the historical behaviour).
	// The cap counts 403 attempts; other retryable errors
	// (429/5xx/network) continue to use MaxAttempts.
	Retry403MaxAttempts int
}

// defaultRetryConfig is used when the caller passes a zero-value
// config with MaxAttempts <= 0. It gives three total attempts
// (initial + 2 retries) with exponential backoff from 2s to 15s.
var defaultRetryConfig = RetryConfig{
	MaxAttempts:         3,
	BaseDelay:           2 * time.Second,
	MaxDelay:            15 * time.Second,
	Retry403MaxAttempts: 2,
}

// NoRetryConfig disables retry entirely. Used by the simple
// constructors so existing callers (mostly tests) keep their
// current behaviour.
var NoRetryConfig = RetryConfig{
	MaxAttempts: 1,
}

// retryableFetchError inspects an error returned by an HTTP fetch
// or TLS-impersonation attempt and reports whether it is worth
// retrying. It returns the classification reason for logging. The
// reason for an HTTP 403 is prefixed "403" so retryWithBackoff can
// apply the separate Retry403MaxAttempts cap.
func retryableFetchError(err error) (retryable bool, reason string) {
	if err == nil {
		return false, ""
	}

	// Context cancellation is terminal — the caller gave up.
	if errors.Is(err, context.Canceled) {
		return false, "context cancelled"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return false, "context deadline exceeded"
	}

	// Network-level errors are retryable (DNS, TCP, TLS, etc.).
	var netErr net.Error
	if errors.As(err, &netErr) {
		return true, "network error"
	}

	// I/O errors like unexpected EOF are often transient.
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true, "unexpected EOF"
	}

	// Socket-level errors from syscall.
	if errors.Is(err, syscall.ECONNRESET) ||
		errors.Is(err, syscall.EPIPE) ||
		errors.Is(err, syscall.ECONNREFUSED) {
		return true, "socket error"
	}

	// Unwrap once to check for wrapped net.Error.
	if u := errors.Unwrap(err); u != nil {
		if errors.As(u, &netErr) {
			return true, "wrapped network error"
		}
		msg := u.Error()
		if strings.Contains(msg, "status 429") {
			return true, "429 rate limited"
		}
		if strings.Contains(msg, "status 403") {
			return true, "403 forbidden"
		}
		if is5xxStatusMessage(msg) {
			return true, "5xx server error"
		}
	}

	msg := err.Error()
	if strings.Contains(msg, "status 429") {
		return true, "429 rate limited"
	}
	if strings.Contains(msg, "status 403") {
		return true, "403 forbidden"
	}
	if is5xxStatusMessage(msg) {
		return true, "5xx server error"
	}

	// Heuristic: timeout / connection keywords in the message.
	lower := strings.ToLower(msg)
	if strings.Contains(lower, "timeout") || strings.Contains(lower, "connection") {
		return true, "network/timeout heuristic"
	}

	// Non-retryable sentinel errors from the fetch package.
	if errors.Is(err, ErrBodyTooLarge) {
		return false, "body too large"
	}
	if errors.Is(err, ErrInsufficientContent) {
		return false, "insufficient content"
	}

	// ErrNon2xxStatus: 429/5xx are fully retryable (subject to
	// MaxAttempts); 403 is retryable subject to the separate
	// Retry403MaxAttempts cap; other 4xx are permanent.
	var non2xx *ErrNon2xxStatus
	if errors.As(err, &non2xx) {
		if non2xx.Code == 429 || (non2xx.Code >= 500 && non2xx.Code <= 599) {
			return true, fmt.Sprintf("status %d", non2xx.Code)
		}
		if non2xx.Code == 403 {
			return true, "403 forbidden"
		}
		return false, fmt.Sprintf("non-retryable status %d", non2xx.Code)
	}

	return false, "unknown/permanent"
}

// is5xxStatusMessage checks if the message contains a 5xx HTTP
// status code string like "status 500" or "status 503". This
// catches errors from providers that embed the status in the
// error message (e.g. upstream returned status 500).
func is5xxStatusMessage(msg string) bool {
	for code := 500; code <= 599; code++ {
		if strings.Contains(msg, fmt.Sprintf("status %d", code)) {
			return true
		}
	}
	return false
}

// retryWithBackoff calls fn up to cfg.MaxAttempts times.
// Between attempts it sleeps for an exponentially-growing delay
// (BaseDelay * 2^(attempt-1)) capped at cfg.MaxDelay. The
// function respects context cancellation — if ctx is done while
// sleeping, it returns immediately with ctx.Err().
//
// The label is used in log messages so operators can distinguish
// retries from different providers in the logs.
func retryWithBackoff[T any](
	ctx context.Context,
	cfg RetryConfig,
	label string,
	fn func(ctx context.Context) (T, error),
) (T, error) {
	var zero T

	if cfg.MaxAttempts <= 0 {
		cfg = defaultRetryConfig
	}
	if cfg.BaseDelay <= 0 {
		cfg.BaseDelay = defaultRetryConfig.BaseDelay
	}
	if cfg.MaxDelay <= 0 {
		cfg.MaxDelay = defaultRetryConfig.MaxDelay
	}

	var lastErr error
	// count403 tracks how many attempts have returned a 403-classified
	// error so we can apply the separate Retry403MaxAttempts cap.
	// cap403 <= 0 means 403 retry is disabled: 403 is treated as
	// permanent (the historical behaviour). cap403 > 0 means a 403
	// is retried up to that many total attempts.
	count403 := 0
	cap403 := cfg.Retry403MaxAttempts
	for attempt := 1; attempt <= cfg.MaxAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return zero, err
		}

		result, err := fn(ctx)
		if err == nil {
			return result, nil
		}
		lastErr = err

		retryable, reason := retryableFetchError(err)
		is403 := reason == "403 forbidden"
		if is403 {
			count403++
		}
		// 403 retry disabled (cap403 <= 0): a 403 is permanent.
		// 403 retry enabled: stop once count403 reaches the cap.
		over403Cap := is403 && (cap403 <= 0 || count403 >= cap403)
		if !retryable || over403Cap || attempt == cfg.MaxAttempts {
			if !retryable || over403Cap {
				log.Printf("%s: attempt %d/%d failed (%s, permanent): %v",
					label, attempt, cfg.MaxAttempts, reason, err)
			} else {
				log.Printf("%s: attempt %d/%d failed (%s) — out of retries: %v",
					label, attempt, cfg.MaxAttempts, reason, err)
			}
			return zero, lastErr
		}

		// Exponential backoff: BaseDelay * 2^(attempt-1).
		// attempt=1 is the first attempt (succeeds or fails
		// immediately, no backoff). The first backoff happens
		// before attempt 2.
		delay := time.Duration(float64(cfg.BaseDelay) * math.Pow(2, float64(attempt-1)))
		if delay > cfg.MaxDelay {
			delay = cfg.MaxDelay
		}

		log.Printf("%s: attempt %d/%d failed (%s); retrying in %s: %v",
			label, attempt, cfg.MaxAttempts, reason, delay, err)

		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return zero, ctx.Err()
		}
	}

	return zero, lastErr
}
