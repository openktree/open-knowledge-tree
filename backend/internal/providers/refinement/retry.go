package refinement

import (
	"context"
	"errors"
	"io"
	"log"
	"math"
	"net"
	"strings"
	"syscall"
	"time"
)

// retryConfig governs the refiner's retry-with-backoff loop. The
// defaults mirror decomposition.defaultRetryConfig so the refinement
// provider behaves the same as the other AI-backed providers against
// OpenRouter / Ollama Cloud: 429 + 5xx + network errors are retried;
// 4xx (other than 429) is permanent.
type retryConfig struct {
	MaxAttempts int
	BaseDelay   time.Duration
	MaxDelay    time.Duration
	PerCallTO   time.Duration
}

var defaultRetryConfig = retryConfig{
	MaxAttempts: 4,
	BaseDelay:   2 * time.Second,
	MaxDelay:    30 * time.Second,
	PerCallTO:   180 * time.Second,
}

// classifyError reports whether an AI provider error is worth
// retrying. Mirrors summarization.classifyError.
func classifyError(err error) (retryable bool, reason string) {
	if err == nil {
		return false, ""
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false, "context cancelled/deadline"
	}
	var netErr net.Error
	if errors.As(err, &netErr) {
		return true, "net.Error"
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true, "EOF/unexpected EOF"
	}
	if errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.EPIPE) || errors.Is(err, syscall.ECONNREFUSED) {
		return true, "connection reset/pipe/refused"
	}
	msg := err.Error()
	if strings.Contains(msg, "status 429") {
		return true, "429 rate limited"
	}
	if strings.Contains(msg, "status 500") || strings.Contains(msg, "status 502") ||
		strings.Contains(msg, "status 503") || strings.Contains(msg, "status 504") {
		return true, "5xx server error"
	}
	lower := strings.ToLower(msg)
	if strings.Contains(lower, "timeout") || strings.Contains(lower, "connection") {
		return true, "network/timeout heuristic"
	}
	return false, "permanent"
}

// retryWithBackoff calls fn up to cfg.MaxAttempts times with
// exponential backoff. Between attempts it sleeps BaseDelay *
// 2^(attempt-1), capped at MaxDelay. A fresh per-call context of
// PerCallTO is derived from the caller's ctx so one wedged LLM call
// can't eat the whole job budget. Permanent errors return
// immediately.
func retryWithBackoff[T any](
	ctx context.Context,
	cfg retryConfig,
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
	for attempt := 1; attempt <= cfg.MaxAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return zero, err
		}
		callCtx := ctx
		var cancel context.CancelFunc
		if cfg.PerCallTO > 0 {
			callCtx, cancel = context.WithTimeout(ctx, cfg.PerCallTO)
		}
		result, err := fn(callCtx)
		if cancel != nil {
			cancel()
		}
		if err == nil {
			return result, nil
		}
		lastErr = err
		retryable, reason := classifyError(err)
		if !retryable || attempt == cfg.MaxAttempts {
			if !retryable {
				log.Printf("%s: attempt %d/%d failed (%s, permanent): %v", label, attempt, cfg.MaxAttempts, reason, err)
			} else {
				log.Printf("%s: attempt %d/%d failed (%s) — out of retries: %v", label, attempt, cfg.MaxAttempts, reason, err)
			}
			return zero, lastErr
		}
		delay := time.Duration(float64(cfg.BaseDelay) * math.Pow(2, float64(attempt-1)))
		if delay > cfg.MaxDelay {
			delay = cfg.MaxDelay
		}
		log.Printf("%s: attempt %d/%d failed (%s); retrying in %s: %v", label, attempt, cfg.MaxAttempts, reason, delay, err)
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return zero, ctx.Err()
		}
	}
	return zero, lastErr
}

// cleanJSONObject extracts the first {...} balanced block from raw,
// tolerating prose around it (e.g. "Here is the result: {...}").
// Returns "" when no balanced object is found. Mirrors the
// decomposition alias provider's JSON extraction.
func cleanJSONObject(raw string) string {
	raw = strings.TrimSpace(raw)
	raw = strings.TrimPrefix(raw, "```json")
	raw = strings.TrimPrefix(raw, "```")
	raw = strings.TrimSuffix(raw, "```")
	raw = strings.TrimSpace(raw)
	start := strings.Index(raw, "{")
	end := strings.LastIndex(raw, "}")
	if start == -1 || end == -1 || end <= start {
		return ""
	}
	return raw[start : end+1]
}