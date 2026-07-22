package ai

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/openktree/open-knowledge-tree/backend/internal/config"
	"github.com/openktree/open-knowledge-tree/backend/internal/store"
	"golang.org/x/time/rate"
)

// nilDB is a store.DBTX that returns zero values. The stub providers
// ignore the db argument, so this is only needed to satisfy the
// interface signature.
type nilDB struct{}

func (nilDB) Exec(context.Context, string, ...interface{}) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, nil
}
func (nilDB) Query(context.Context, string, ...interface{}) (pgx.Rows, error) {
	return nil, nil
}
func (nilDB) QueryRow(context.Context, string, ...interface{}) pgx.Row {
	return nil
}

var _ store.DBTX = nilDB{}

// stubChatProvider is a minimal AIProvider that counts Chat calls.
type stubChatProvider struct {
	mu    sync.Mutex
	calls []string // model ids per call
}

func (s *stubChatProvider) Chat(_ context.Context, _ store.DBTX, req ChatRequest) (ChatResponse, error) {
	s.mu.Lock()
	s.calls = append(s.calls, req.Model)
	s.mu.Unlock()
	return ChatResponse{Model: req.Model}, nil
}

func (s *stubChatProvider) Describe() ProviderDescription {
	return ProviderDescription{Name: "stub"}
}

func (s *stubChatProvider) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.calls)
}

// stubEmbedProvider satisfies both AIProvider and EmbeddingProvider.
type stubEmbedProvider struct {
	stubChatProvider
	embedCalls atomic.Int32
}

func (s *stubEmbedProvider) Embed(_ context.Context, _ store.DBTX, req EmbeddingRequest) (EmbeddingResponse, error) {
	s.embedCalls.Add(1)
	return EmbeddingResponse{Model: req.Model}, nil
}

// cfgWithModels builds a *config.Config with the given model catalog.
func cfgWithModels(models ...config.AIModelConfig) *config.Config {
	return &config.Config{
		Providers: config.ProvidersConfig{
			AI: config.AIProvidersConfig{
				Models: models,
			},
		},
	}
}

// TestMaybeWrap_ChatOnlyProvider_DoesNotImplementEmbed asserts the
// critical invariant: wrapping a chat-only provider must NOT make it
// satisfy EmbeddingProvider, so the wiring layer's embedding-provider
// type assertion still correctly rejects chat-only providers.
func TestMaybeWrap_ChatOnlyProvider_DoesNotImplementEmbed(t *testing.T) {
	cfg := cfgWithModels(config.AIModelConfig{
		ID: "m1", Provider: "stub", RateLimitRPM: 30,
	})
	inner := &stubChatProvider{}
	wrapped := MaybeWrapRateLimited(inner, "stub", cfg)
	if _, ok := wrapped.(EmbeddingProvider); ok {
		t.Fatal("chat-only wrapper must NOT satisfy EmbeddingProvider; the wiring layer relies on this type assertion to reject chat-only providers as embedding providers")
	}
}

// TestMaybeWrap_EmbedProvider_StillSatisfiesEmbed asserts the
// counterpart: wrapping a chat+embed provider preserves
// EmbeddingProvider satisfaction so the wiring layer still selects it.
func TestMaybeWrap_EmbedProvider_StillSatisfiesEmbed(t *testing.T) {
	cfg := cfgWithModels(config.AIModelConfig{
		ID: "m1", Provider: "stub", RateLimitRPM: 30,
	})
	inner := &stubEmbedProvider{}
	wrapped := MaybeWrapRateLimited(inner, "stub", cfg)
	if _, ok := wrapped.(EmbeddingProvider); !ok {
		t.Fatal("chat+embed wrapper must still satisfy EmbeddingProvider")
	}
}

// TestRateLimit_PacesBurstOverflow asserts that with a configured
// RPM, concurrent calls beyond the burst are paced (delayed) rather
// than all firing at once.
func TestRateLimit_PacesBurstOverflow(t *testing.T) {
	cfg := cfgWithModels(config.AIModelConfig{
		ID: "m1", Provider: "stub", RateLimitRPM: 600, // burst=30, interval=100ms
	})
	inner := &stubChatProvider{}
	wrapped := MaybeWrapRateLimited(inner, "stub", cfg).(*rateLimitedChat)

	const N = 35 // burst=30, so 5 must be paced
	var wg sync.WaitGroup
	start := time.Now()
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = wrapped.Chat(context.Background(), nilDB{}, ChatRequest{Model: "m1"})
		}()
	}
	wg.Wait()
	elapsed := time.Since(start)
	// 30 burst immediately, 5 paced at 100ms each → ~400ms+.
	if elapsed < 300*time.Millisecond {
		t.Errorf("expected pacing to delay the burst-overflow calls; elapsed=%s (N=%d, burst=30)", elapsed, N)
	}
	if got := inner.callCount(); got != N {
		t.Errorf("inner call count = %d, want %d", got, N)
	}
}

// TestRateLimit_ContextCancelAbortsWait asserts that when the limiter
// is exhausted and the context is cancelled, Wait returns ctx.Err()
// and the inner provider is NOT called.
func TestRateLimit_ContextCancelAbortsWait(t *testing.T) {
	cfg := cfgWithModels(config.AIModelConfig{
		ID: "m1", Provider: "stub", RateLimitRPM: 60, // burst=3
	})
	inner := &stubChatProvider{}
	wrapped := MaybeWrapRateLimited(inner, "stub", cfg).(*rateLimitedChat)

	// Drain the burst (3 calls).
	for i := 0; i < 3; i++ {
		_, err := wrapped.Chat(context.Background(), nilDB{}, ChatRequest{Model: "m1"})
		if err != nil {
			t.Fatalf("drain call %d: %v", i, err)
		}
	}
	before := inner.callCount()

	// Now the limiter is exhausted. Under the decoupled-wait design,
	// the wait runs on a background ctx with the wrapper's
	// waitTimeout; shrink it so the test doesn't block for 1h. The
	// wrapper returns ErrRateLimitWaitTimeout when the wait ctx
	// fires, and does NOT call the inner provider.
	wrapped.waitTimeout = 50 * time.Millisecond
	_, err := wrapped.Chat(context.Background(), nilDB{}, ChatRequest{Model: "m1"})
	if !errors.Is(err, ErrRateLimitWaitTimeout) {
		t.Errorf("expected ErrRateLimitWaitTimeout, got %v", err)
	}
	if got := inner.callCount(); got != before {
		t.Errorf("inner provider was called despite wait timeout; count went from %d to %d", before, got)
	}
}

// TestRateLimit_PerModelIndependent asserts that two models get
// independent limiters: saturating model A does not block model B.
func TestRateLimit_PerModelIndependent(t *testing.T) {
	cfg := cfgWithModels(
		config.AIModelConfig{ID: "a", Provider: "stub", RateLimitRPM: 60}, // burst=3
		config.AIModelConfig{ID: "b", Provider: "stub", RateLimitRPM: 60}, // burst=3
	)
	inner := &stubChatProvider{}
	wrapped := MaybeWrapRateLimited(inner, "stub", cfg).(*rateLimitedChat)

	// Drain model A's burst.
	for i := 0; i < 3; i++ {
		_, err := wrapped.Chat(context.Background(), nilDB{}, ChatRequest{Model: "a"})
		if err != nil {
			t.Fatalf("drain a[%d]: %v", i, err)
		}
	}

	// Model A is now exhausted; under the decoupled-wait design the
	// wait runs on a background ctx with waitTimeout. Shrink it so
	// the test doesn't block for 1h; the wrapper returns
	// ErrRateLimitWaitTimeout when the wait ctx fires.
	wrapped.waitTimeout = 50 * time.Millisecond
	if _, err := wrapped.Chat(context.Background(), nilDB{}, ChatRequest{Model: "a"}); !errors.Is(err, ErrRateLimitWaitTimeout) {
		t.Errorf("model a: expected ErrRateLimitWaitTimeout after draining, got %v", err)
	}

	// Model B should still be immediately available (separate bucket).
	// Restore the waitTimeout so B isn't artificially throttled.
	wrapped.waitTimeout = time.Hour
	ctxB, cancelB := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancelB()
	if _, err := wrapped.Chat(ctxB, nilDB{}, ChatRequest{Model: "b"}); err != nil {
		t.Errorf("model b: expected immediate success (independent bucket), got %v", err)
	}
}

// TestRateLimit_UnlimitedModel_Passthrough asserts that a model with
// a very high rate_limit_rpm (opt-out) never blocks.
func TestRateLimit_UnlimitedModel_Passthrough(t *testing.T) {
	cfg := cfgWithModels(config.AIModelConfig{
		ID: "m1", Provider: "stub", RateLimitRPM: 100000, // opt out; burst=5000
	})
	inner := &stubChatProvider{}
	wrapped := MaybeWrapRateLimited(inner, "stub", cfg).(*rateLimitedChat)

	const N = 200
	var wg sync.WaitGroup
	start := time.Now()
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = wrapped.Chat(context.Background(), nilDB{}, ChatRequest{Model: "m1"})
		}()
	}
	wg.Wait()
	elapsed := time.Since(start)
	// burst=5000 absorbs all 200 immediately.
	if elapsed > 100*time.Millisecond {
		t.Errorf("unlimited model should not pace 200 calls; elapsed=%s", elapsed)
	}
	if got := inner.callCount(); got != N {
		t.Errorf("inner call count = %d, want %d", got, N)
	}
}

// TestRateLimit_EmbedProxiedThroughLimiter asserts that Embed calls
// on the chat+embed wrapper are also rate-limited per model. Under
// the decoupled-wait design, the wait runs on a background ctx with
// the wrapper's waitTimeout; when the wait exceeds waitTimeout the
// wrapper returns ErrRateLimitWaitTimeout (transient). The caller's
// ctx no longer fires the wait — it only bounds the inner HTTP call.
func TestRateLimit_EmbedProxiedThroughLimiter(t *testing.T) {
	cfg := cfgWithModels(config.AIModelConfig{
		ID: "emb-1", Provider: "stub", RateLimitRPM: 60, // burst=3
	})
	inner := &stubEmbedProvider{}
	wrapped := MaybeWrapRateLimited(inner, "stub", cfg).(*rateLimitedEmbed)
	// Shrink the wait timeout so the test doesn't block for 1h.
	wrapped.waitTimeout = 50 * time.Millisecond

	// Drain the burst.
	for i := 0; i < 3; i++ {
		_, err := wrapped.Embed(context.Background(), nilDB{}, EmbeddingRequest{Model: "emb-1"})
		if err != nil {
			t.Fatalf("drain embed[%d]: %v", i, err)
		}
	}

	// Next call should block on the rate limiter; the background
	// wait ctx fires after waitTimeout (50ms) and the wrapper
	// returns ErrRateLimitWaitTimeout.
	_, err := wrapped.Embed(context.Background(), nilDB{}, EmbeddingRequest{Model: "emb-1"})
	if !errors.Is(err, ErrRateLimitWaitTimeout) {
		t.Errorf("embed: expected ErrRateLimitWaitTimeout after draining, got %v", err)
	}
}

// TestEffectiveRPM asserts the default-application logic.
func TestEffectiveRPM(t *testing.T) {
	if got := effectiveRPM(0); got != config.DefaultModelRateLimitRPM {
		t.Errorf("effectiveRPM(0) = %d, want default %d", got, config.DefaultModelRateLimitRPM)
	}
	if got := effectiveRPM(100); got != 100 {
		t.Errorf("effectiveRPM(100) = %d, want 100", got)
	}
}

// TestNewRPMLimiter_BurstSizing asserts the burst formula max(1, rpm/20)
// and the limit (tokens/sec = rpm/60).
func TestNewRPMLimiter_BurstSizing(t *testing.T) {
	cases := []struct {
		rpm       int
		wantBurst int
	}{
		{rpm: 30, wantBurst: 1},  // 30/20=1.5→1
		{rpm: 60, wantBurst: 3},  // 60/20=3
		{rpm: 600, wantBurst: 30},
		{rpm: 1, wantBurst: 1},   // 1/20=0→1
	}
	for _, c := range cases {
		lim := newRPMlimiter(c.rpm)
		if got := lim.Burst(); got != c.wantBurst {
			t.Errorf("rpm=%d: burst=%d, want %d", c.rpm, got, c.wantBurst)
		}
		wantLimit := rate.Limit(float64(c.rpm) / 60.0)
		if got := lim.Limit(); got != wantLimit {
			t.Errorf("rpm=%d: limit=%v, want %v", c.rpm, got, wantLimit)
		}
	}
}