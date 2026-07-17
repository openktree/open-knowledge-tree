package ai

import (
	"context"
	"log"
	"sync"
	"time"

	"github.com/openktree/open-knowledge-tree/backend/internal/config"
	"github.com/openktree/open-knowledge-tree/backend/internal/store"
	"golang.org/x/time/rate"
)

// Rate limiting decorator.
//
// A provider serving N models gets N *rate.Limiter buckets (one per
// model id); the right bucket is selected from req.Model at call
// time. Models with rate_limit_rpm: 0 in the catalog are unlimited;
// the default (DefaultModelRateLimitRPM, 30) is applied when the
// field is unset.
//
// The limiter sits OUTSIDE the per-task retryWithBackoff wrappers
// (the providers hold a *rateLimited[Chat|Embed] as their aiProvider),
// so when the rate limit is reached the call blocks rather than
// retrying. The worker's LLM context (120s in summarize_concepts,
// etc.) covers the wait; if the wait exceeds the context deadline,
// Wait returns ctx.Err() which classifyError treats as permanent
// (non-retryable) — the slice fails gracefully, the per-concept
// advisory lock releases, and the next pass retries the uncovered
// facts. No data is lost; the limiter just paces the provider.
//
// Burst sizing: rate.NewLimiter(rate.Every(minute/rpm), burst).
// A burst of max(1, rpm/20) lets a short spike through (providers
// tolerate brief bursts) while smoothing sustained load.
//
// Two concrete wrapper types exist (rateLimitedChat and
// rateLimitedEmbed) so the EmbeddingProvider interface is only
// satisfied when the inner provider actually implements it — the
// embedding-provider selection in cmd/app/api.go type-asserts on
// the (possibly wrapped) provider, and a chat-only provider must
// NOT be selectable as an embedding provider just because it was
// wrapped.

// rateLimitedChat wraps a chat-only AIProvider with per-model rate
// limiting. It does NOT implement EmbeddingProvider.
type rateLimitedChat struct {
	inner    AIProvider
	limiters *limiterMap
}

func (p *rateLimitedChat) Chat(ctx context.Context, db store.DBTX, req ChatRequest) (ChatResponse, error) {
	if lim := p.limiters.limiterFor(req.Model); lim != nil {
		if err := lim.Wait(ctx); err != nil {
			return ChatResponse{}, waitErr(ctx, err)
		}
	}
	return p.inner.Chat(ctx, db, req)
}

func (p *rateLimitedChat) Describe() ProviderDescription { return p.inner.Describe() }

// rateLimitedEmbed wraps a chat+embed AIProvider with per-model rate
// limiting. It implements EmbeddingProvider by delegating to inner.
type rateLimitedEmbed struct {
	inner    AIProvider
	embed    EmbeddingProvider // == inner, type-asserted once at construction
	limiters *limiterMap
}

func (p *rateLimitedEmbed) Chat(ctx context.Context, db store.DBTX, req ChatRequest) (ChatResponse, error) {
	if lim := p.limiters.limiterFor(req.Model); lim != nil {
		if err := lim.Wait(ctx); err != nil {
			return ChatResponse{}, waitErr(ctx, err)
		}
	}
	return p.inner.Chat(ctx, db, req)
}

func (p *rateLimitedEmbed) Embed(ctx context.Context, db store.DBTX, req EmbeddingRequest) (EmbeddingResponse, error) {
	if lim := p.limiters.limiterFor(req.Model); lim != nil {
		if err := lim.Wait(ctx); err != nil {
			return EmbeddingResponse{}, waitErr(ctx, err)
		}
	}
	return p.embed.Embed(ctx, db, req)
}

// waitErr normalizes the error from rate.Limiter.Wait. When the
// context has already fired (the common case during a long block),
// ctx.Err() returns context.DeadlineExceeded/Canceled which
// classifyError recognizes as permanent (non-retryable). When the
// context hasn't fired yet but the reservation can't fit before the
// deadline (rate's "would exceed context deadline" variant), ctx.Err()
// is nil — return the original rate error so the message still
// surfaces; classifyError treats it as permanent via the
// "deadline"/"timeout" heuristic, which is the correct behavior
// (don't retry a rate-limited call).
func waitErr(ctx context.Context, waitErr error) error {
	if cerr := ctx.Err(); cerr != nil {
		return cerr
	}
	return waitErr
}

func (p *rateLimitedEmbed) Describe() ProviderDescription { return p.inner.Describe() }

// limiterMap holds the per-model rate limiters for one provider
// instance. Safe for concurrent use; limiters are created lazily for
// model ids not pre-seeded from the catalog (e.g. per-repo model
// overrides).
type limiterMap struct {
	mu       sync.Mutex
	limiters map[string]*rate.Limiter // nil value = "no limit" (cached)
	rpmFor   func(modelID string) int // resolves effective RPM (with default)
}

// limiterFor returns the *rate.Limiter for the given model id, or
// nil when the model has no configured limit (unlimited). nil is
// the sentinel for "no throttling"; callers skip the Wait.
func (l *limiterMap) limiterFor(modelID string) *rate.Limiter {
	if modelID == "" || l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if lim, ok := l.limiters[modelID]; ok {
		return lim
	}
	rpm := 0
	if l.rpmFor != nil {
		rpm = l.rpmFor(modelID)
	}
	if rpm <= 0 {
		// Cache the "no limit" decision as a nil entry so repeated
		// calls for the same unknown model don't re-walk the catalog.
		l.limiters[modelID] = nil
		return nil
	}
	lim := newRPMlimiter(rpm)
	l.limiters[modelID] = lim
	return lim
}

// newRPMlimiter builds a *rate.Limiter for the given
// requests-per-minute. Burst is max(1, rpm/20): a small burst
// tolerates short spikes while smoothing sustained load.
func newRPMlimiter(rpm int) *rate.Limiter {
	burst := rpm / 20
	if burst < 1 {
		burst = 1
	}
	return rate.NewLimiter(rate.Every(time.Minute/time.Duration(rpm)), burst)
}

// MaybeWrapRateLimited inspects the model catalog and wraps inner in
// a per-model rate-limited decorator. The decorator keys on
// req.Model at call time, so a provider serving many models gets
// one bucket per model. When the inner provider also implements
// EmbeddingProvider, the wrapper proxies Embed through the same
// limiter map (selected by the embedding model id) — otherwise the
// wrapper is chat-only and does NOT satisfy EmbeddingProvider, so
// the wiring layer's embedding-provider type assertion still
// correctly rejects chat-only providers.
//
// `providerID` is the key the wiring layer used to register the
// provider (e.g. "openrouter", "ollama_cloud"); it's matched
// against AIModelConfig.Provider to scope which models this
// provider serves.
//
// When the model catalog has no entries for this provider AND the
// default RPM is 0 (operator opted out globally), the inner
// provider is returned unchanged — zero overhead, zero allocations.
func MaybeWrapRateLimited(inner AIProvider, providerID string, cfg *config.Config) AIProvider {
	if inner == nil || cfg == nil {
		return inner
	}

	// Walk the catalog and seed limiters for every model this
	// provider serves that has an effective RPM > 0. effectiveRPM
	// applies DefaultModelRateLimitRPM when the entry omits
	// rate_limit_rpm (the common case — operators get a sane
	// ceiling without configuring it per model).
	limiters := make(map[string]*rate.Limiter)
	anyLimited := false
	for _, m := range cfg.Providers.AI.Models {
		if m.Provider != providerID {
			continue
		}
		rpm := effectiveRPM(m.RateLimitRPM)
		if rpm <= 0 {
			continue
		}
		limiters[m.ID] = newRPMlimiter(rpm)
		anyLimited = true
	}

	defaultRPM := effectiveRPM(0)
	// When no model for this provider has a limit and the default
	// is 0 (operator opted out globally), skip wrapping entirely.
	if !anyLimited && defaultRPM <= 0 {
		return inner
	}

	// rpmFor resolves the effective RPM for a model id not
	// pre-seeded (e.g. a per-repo override). Looks up the catalog
	// entry; falls back to the default when the model isn't listed.
	rpmFor := func(modelID string) int {
		for _, m := range cfg.Providers.AI.Models {
			if m.ID == modelID {
				return effectiveRPM(m.RateLimitRPM)
			}
		}
		// Unknown model id (not in catalog): apply the default so a
		// per-repo override still gets a sane ceiling. Operators who
		// want unlimited for an override model can add it to the
		// catalog with rate_limit_rpm set to a very large number.
		return defaultRPM
	}
	lm := &limiterMap{limiters: limiters, rpmFor: rpmFor}

	// Choose the wrapper type so EmbeddingProvider satisfaction
	// matches the inner provider's actual capabilities.
	if ep, ok := inner.(EmbeddingProvider); ok {
		log.Printf("ai: rate-limited provider %q (chat+embed): %d model(s) with explicit limiter(s)", providerID, len(limiters))
		return &rateLimitedEmbed{inner: inner, embed: ep, limiters: lm}
	}
	log.Printf("ai: rate-limited provider %q (chat-only): %d model(s) with explicit limiter(s)", providerID, len(limiters))
	return &rateLimitedChat{inner: inner, limiters: lm}
}

// effectiveRPM returns the RPM to enforce for a model entry,
// applying DefaultModelRateLimitRPM when the operator left
// rate_limit_rpm unset (0). An operator who wants unlimited for a
// specific model should set its rate_limit_rpm to a very large
// number (e.g. 100000) rather than 0, since 0 means "use the
// default". This keeps the default-on behavior the common path and
// avoids the "set 0 to opt out" footgun (Viper parses unset and
// explicit-0 identically, so the two can't be distinguished).
func effectiveRPM(configured int) int {
	if configured > 0 {
		return configured
	}
	return config.DefaultModelRateLimitRPM
}