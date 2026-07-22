package summarization

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

	"github.com/openktree/open-knowledge-tree/backend/internal/config"
	"github.com/openktree/open-knowledge-tree/backend/internal/providers/ai"
)

// retryConfig governs the summarizer's retry-with-backoff loop. The
// defaults mirror decomposition.defaultRetryConfig so the
// summarization provider behaves the same as the other AI-backed
// providers (concept extraction, fact extraction, alias generation)
// against OpenRouter / Ollama Cloud: 429 + 5xx + network errors are
// retried; 4xx (other than 429) is permanent.
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
	PerCallTO:   5 * time.Minute,
}

// SetRetryDefaults overrides the package's defaultRetryConfig from
// the shared cfg.Providers.LLMRetry block. Called once at boot by
// the wiring layer. See decomposition.SetRetryDefaults for the
// rationale.
func SetRetryDefaults(cfg config.LLMRetryConfig) {
	defaultRetryConfig = retryConfig{
		MaxAttempts: cfg.MaxAttemptsOr(0),
		BaseDelay:   cfg.BaseDelayOr(0),
		MaxDelay:    cfg.MaxDelayOr(0),
		PerCallTO:   cfg.PerCallTimeoutOr(0),
	}
}

// classifyError reports whether an AI provider error is worth
// retrying. Mirrors decomposition.retryClassifyError so the
// summarizer rides out the same transient failures the other
// providers do.
func classifyError(err error) (retryable bool, reason string) {
	if err == nil {
		return false, ""
	}
	// Rate-limit-wait-timeout from the decorator is transient.
	if errors.Is(err, ai.ErrRateLimitWaitTimeout) {
		return true, "rate limit wait timeout"
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

// trimMarkdownFences strips a leading ``` fenced-code wrapper the
// model may add around its markdown output despite the prompt asking
// for plain markdown. The summarizer treats the response as the
// summary body, so any wrapper must be removed before persisting.
func trimMarkdownFences(s string) string {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "```") {
		return s
	}
	// Drop the opening fence (and any language tag on the same line).
	if idx := strings.IndexByte(s, '\n'); idx != -1 {
		s = s[idx+1:]
	}
	s = strings.TrimSpace(s)
	// Drop a trailing ``` fence.
	if strings.HasSuffix(s, "```") {
		s = strings.TrimSuffix(s, "```")
	}
	return strings.TrimSpace(s)
}

// _ = fmt.Sprintf guard so the import stays used even if the helper
// above is later refactored away.
var _ = fmt.Sprintf