package decomposition

import (
	"context"
	"errors"
	"io"
	"log"
	"math"
	"net"
	"net/http"
	"strings"
	"syscall"
	"time"

	"github.com/openktree/open-knowledge-tree/backend/internal/config"
	"github.com/openktree/open-knowledge-tree/backend/internal/providers/ai"
)

// retryConfig governs the retry-with-backoff helper used by the
// text and image fact extractors. The defaults are tuned for the
// AI-provider Chat call: providers (OpenRouter, Ollama Cloud) return
// 429 on rate limit and 5xx on transient upstream failures, and the
// HTTP client itself surfaces network errors (DNS, connection reset,
// EOF) that are also worth one retry. A 4xx (other than 429) is
// treated as permanent — retrying a 401/403/400 just wastes budget.
type retryConfig struct {
	MaxAttempts int           // total attempts including the first; 1 == no retry
	BaseDelay   time.Duration // backoff for attempt 2; subsequent delays grow exponentially
	MaxDelay    time.Duration // cap on per-attempt backoff
	PerCallTO   time.Duration // per-attempt context timeout; 0 means inherit caller's ctx
}

// defaultRetryConfig is applied when a caller passes a zero
// retryConfig. MaxAttempts=4 means the original call + up to 3
// retries; with BaseDelay=2s and MaxDelay=30s a wedged provider
// spends at most ~2+4+8 = 14s of backoff before the final attempt
// fails — short enough not to dominate a chunk's wall time, long
// enough to ride out a 429 storm or a brief upstream 5xx blip.
//
// PerCallTO defaults to 5m (raised from 180s) so a single LLM call
// can tolerate a slow OpenRouter streaming-decode without firing
// the per-call ctx mid-stream. The worker's LLM timeout (20m for
// extract_concepts via DecompositionConceptConfig.LLMTimeout)
// must exceed MaxAttempts × PerCallTO + backoffs so all 4 attempts
// can actually run; historically the worker's 120s outer ctx
// fired before the first 180s per-call could complete, defeating
// the retry loop. SetRetryDefaults lets the wiring layer override
// these from cfg.Providers.LLMRetry at boot.
var defaultRetryConfig = retryConfig{
	MaxAttempts: 4,
	BaseDelay:   2 * time.Second,
	MaxDelay:    30 * time.Second,
	PerCallTO:   5 * time.Minute,
}

// SetRetryDefaults overrides the package's defaultRetryConfig from
// the shared cfg.Providers.LLMRetry block. Called once at boot by
// the wiring layer (cmd/app/api.go) so the four phase providers
// (fact_extraction, concept_extraction, image_fact_extraction —
// and the sibling summarization/refinement/synthesis packages have
// their own SetRetryDefaults) pick up the operator's retry tuning
// without each provider needing a constructor change. Zero-valued
// fields fall back to the defaults above via the Or methods.
func SetRetryDefaults(cfg config.LLMRetryConfig) {
	defaultRetryConfig = retryConfig{
		MaxAttempts: cfg.MaxAttemptsOr(0),
		BaseDelay:   cfg.BaseDelayOr(0),
		MaxDelay:    cfg.MaxDelayOr(0),
		PerCallTO:   cfg.PerCallTimeoutOr(0),
	}
}

// retryClassifyError inspects an error returned by an AI provider's
// Chat/Embed call and reports whether it is worth retrying. The
// providers wrap non-200 responses as `"<provider>: status %d: %s"`
// and network failures as `"<provider>: request failed: %w"` or
// `"<provider>: decoding response: %w"`. Rather than retrofit a
// typed status error onto every provider, we classify by inspecting
// the wrapped chain: any net.Error, EOF, connection-reset, or broken
// pipe is retryable; any "status 429" or "status 5xx" string is
// retryable; anything else (4xx auth/validation, JSON parse errors
// that aren't transient, context cancellation) is permanent.
//
// Context cancellation is deliberately permanent: if the job's
// timeout fired (River cancelling the worker ctx) or the operator
// cancelled the call, retrying just burns budget against a ctx that
// is already done.
func retryClassifyError(err error) (retryable bool, reason string) {
	return ClassifyLLMError(err)
}

// ClassifyLLMError is the exported form of retryClassifyError, used
// by worker-level code (e.g. extract_concepts' decision to record a
// soft-skip vs. leave the fact for retry) to decide whether an LLM
// failure is transient (timeout, rate-limit wait, network blip,
// 5xx, 429) or permanent (parse failure, empty result, auth error).
// Transient failures stay in the candidate set so the next pass
// retries them; permanent failures record a soft-skip row that
// counts against the retry budget.
//
// The rate-limiter decorator returns ErrRateLimitWaitTimeout when
// its waitTimeout budget is exhausted; this is classified as
// transient so the fact is retried on the next pass instead of
// being permanently skipped (the historical failure mode that
// severed 13,485 facts — see the 0024 skip table's
// "rate: Wait(n=1) would exceed context deadline" rows).
func ClassifyLLMError(err error) (retryable bool, reason string) {
	if err == nil {
		return false, ""
	}
	// Rate-limit-wait-timeout from the decorator is transient.
	if errors.Is(err, ai.ErrRateLimitWaitTimeout) {
		return true, "rate limit wait timeout"
	}
	// Context cancellation is terminal — never retry.
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false, "context cancelled/deadline"
	}
	// Network-class errors are transient.
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
	// Provider-wrapped HTTP status. We string-match because the
	// providers' error format is stable (`status %d:`) and adding a
	// typed error to every provider is a larger refactor than this
	// helper needs.
	msg := err.Error()
	if strings.Contains(msg, "status 429") {
		return true, "429 rate limited"
	}
	if strings.Contains(msg, "status 500") || strings.Contains(msg, "status 502") ||
		strings.Contains(msg, "status 503") || strings.Contains(msg, "status 504") ||
		strings.Contains(msg, "status 5") && looksLike5xx(msg) {
		return true, "5xx server error"
	}
	// Fallback: any error whose message explicitly says "timeout" or
	// "connection" is treated as transient. This catches wrappers
	// like "openrouter: request failed: dial tcp: i/o timeout" that
	// don't unwrap to a recognised sentinel.
	lower := strings.ToLower(msg)
	if strings.Contains(lower, "timeout") || strings.Contains(lower, "connection") {
		return true, "network/timeout heuristic"
	}
	return false, "permanent"
}

// looksLike5xx is a last-resort guard so that a `status 5` substring
// in an unrelated error message (e.g. a 503-byte body, or a model id
// that happens to contain "5") doesn't get misclassified as a 5xx.
// It checks for the canonical `status 5xx:` shape the providers emit.
func looksLike5xx(msg string) bool {
	idx := strings.Index(msg, "status 5")
	if idx == -1 {
		return false
	}
	rest := msg[idx+len("status 5"):]
	if len(rest) < 2 {
		return false
	}
	// Two digits then ':' (the providers format as %d).
	return (rest[0] >= '0' && rest[0] <= '9') && (rest[1] >= '0' && rest[1] <= '9') && (len(rest) > 2 && rest[2] == ':')
}

// retryWithBackoff calls fn up to cfg.MaxAttempts times. Between
// attempts it sleeps for an exponentially-growing delay capped at
// cfg.MaxDelay. The delay is skipped after the final attempt. fn is
// invoked with a fresh context timeout of cfg.PerCallTO (when > 0)
// derived from the caller's ctx, so one wedged call can't eat the
// entire job budget — only its own per-call slice.
//
// fn is expected to be the AI provider Chat/Embed call plus any
// provider-specific marshalling. It must respect the ctx passed to
// it so the per-call timeout is enforced.
//
// On a permanent error (4xx auth, JSON parse failure, ctx cancelled)
// retryWithBackoff returns immediately without retrying. On a
// retryable error it logs the attempt and sleeps. The last error is
// returned verbatim so callers can wrap it with their own context.
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
		// Honour the parent ctx between attempts too — if the job
		// is cancelled while we're sleeping in backoff, bail out
		// rather than wake up and retry against a dead ctx.
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

		retryable, reason := retryClassifyError(err)
		if !retryable || attempt == cfg.MaxAttempts {
			if !retryable {
				log.Printf("%s: attempt %d/%d failed (%s, permanent): %v", label, attempt, cfg.MaxAttempts, reason, err)
			} else {
				log.Printf("%s: attempt %d/%d failed (%s) — out of retries: %v", label, attempt, cfg.MaxAttempts, reason, err)
			}
			return zero, lastErr
		}

		// Exponential backoff with cap: 2s, 4s, 8s, ... up to MaxDelay.
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

// classifyHTTPStatus is a thin helper for tests / future typed-error
// migration: it returns true for status codes that are worth
// retrying. Exported so provider implementations can eventually use
// it directly when they wrap errors with a typed status.
func classifyHTTPStatus(code int) bool {
	return code == http.StatusTooManyRequests || code >= 500 && code <= 599
}