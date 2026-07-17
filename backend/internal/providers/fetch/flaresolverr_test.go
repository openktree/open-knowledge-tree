package fetch

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/openktree/open-knowledge-tree/backend/internal/providers/content_parsing"
)

// TestFlareSolverrProviderPool_SelfDisable covers the
// self-disable contract: an empty endpoints list returns nil
// so the wiring layer can skip registration with a single
// conditional. This is the same contract the single-endpoint
// constructor has always had; the pool constructor must
// preserve it.
func TestFlareSolverrProviderPool_SelfDisable(t *testing.T) {
	if p := NewFlareSolverrProviderPool(nil, 0, "", 0); p != nil {
		t.Fatal("expected nil provider for empty endpoints")
	}
	if p := NewFlareSolverrProviderPool([]string{"", "  "}, 0, "", 0); p != nil {
		t.Fatal("expected nil provider for all-whitespace endpoints")
	}
}

// TestFlareSolverrProviderPool_SingleEndpoint verifies the
// pool constructor accepts a single endpoint and normalizes
// it to the /v1 path, matching the legacy
// NewFlareSolverrProvider behaviour.
func TestFlareSolverrProviderPool_SingleEndpoint(t *testing.T) {
	p := NewFlareSolverrProviderPool([]string{"http://flaresolverr:8191"}, 0, "", 0)
	if p == nil {
		t.Fatal("expected non-nil provider for one endpoint")
	}
	if len(p.endpoints) != 1 {
		t.Fatalf("expected 1 endpoint, got %d", len(p.endpoints))
	}
	if got := p.endpoints[0]; got != "http://flaresolverr:8191/v1" {
		t.Errorf("expected /v1 normalization, got %q", got)
	}
	if p.sem != nil {
		t.Error("expected nil semaphore for maxConcurrency=0 (no cap)")
	}
}

// TestFlareSolverrProviderPool_RoundRobin verifies the
// round-robin cursor distributes requests across all
// configured endpoints. This is the core scaling
// guarantee: a single Byparr container saturates under
// burst load, so the pool must spread requests across N
// containers.
func TestFlareSolverrProviderPool_RoundRobin(t *testing.T) {
	endpoints := []string{"http://e1:8191", "http://e2:8191", "http://e3:8191"}
	p := NewFlareSolverrProviderPool(endpoints, 60*time.Second, "", 0)
	if p == nil {
		t.Fatal("expected non-nil provider")
	}
	seen := make(map[string]int)
	for i := 0; i < 9; i++ {
		ep := p.nextEndpoint()
		seen[ep]++
	}
	if len(seen) != 3 {
		t.Fatalf("expected requests spread across 3 endpoints, got %d: %v", len(seen), seen)
	}
	for ep, n := range seen {
		if n != 3 {
			t.Errorf("expected 3 requests per endpoint, got %d for %s", n, ep)
		}
	}
}

// TestFlareSolverrProviderPool_ConcurrencyCap verifies the
// semaphore caps in-flight Resolve calls at MaxConcurrency.
// A single Byparr container queues concurrent requests
// internally and every queued request burns its 60s timeout,
// so the cap is what prevents the timeout storm under burst
// load. We stand up a slow sidecar that blocks each request
// for 200ms and confirm that with MaxConcurrency=2, no more
// than 2 requests are ever in flight simultaneously.
func TestFlareSolverrProviderPool_ConcurrencyCap(t *testing.T) {
	var inFlight, maxInFlight int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cur := atomic.AddInt32(&inFlight, 1)
		for {
			old := atomic.LoadInt32(&maxInFlight)
			if cur <= old || atomic.CompareAndSwapInt32(&maxInFlight, old, cur) {
				break
			}
		}
		time.Sleep(100 * time.Millisecond)
		atomic.AddInt32(&inFlight, -1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok","solution":{"response":"<html><body>enough text to pass the minimum length guard so the provider returns nil and the semaphore is released</body></html>","headers":{"content-type":"text/html"},"url":"http://example.com"}}`))
	}))
	defer srv.Close()

	p := NewFlareSolverrProviderPool([]string{srv.URL}, 5*time.Second, "", 2,
		content_parsing.NewTrafilaturaParser(),
	)
	if p == nil {
		t.Fatal("expected non-nil provider")
	}
	if p.sem == nil {
		t.Fatal("expected non-nil semaphore for maxConcurrency=2")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	const numRequests = 6
	var wg sync.WaitGroup
	errs := make([]error, numRequests)
	for i := 0; i < numRequests; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = p.Resolve(ctx, Resource{Type: SourceURL, Value: "http://example.com"})
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Logf("request %d error (acceptable if ErrInsufficientContent): %v", i, err)
		}
	}
	if got := atomic.LoadInt32(&maxInFlight); got > 2 {
		t.Errorf("expected max in-flight <= 2 (MaxConcurrency), got %d", got)
	}
}

// TestFlareSolverrProviderPool_ContextCancelOnSemaphoreWait
// verifies that when the semaphore is saturated and the
// context is cancelled, acquireSem returns the context
// error instead of stranding the worker forever. This is
// the safety valve: a backed-up pool must not hold a
// worker past its budget.
func TestFlareSolverrProviderPool_ContextCancelOnSemaphoreWait(t *testing.T) {
	p := NewFlareSolverrProviderPool([]string{"http://never-reached:8191"}, 5*time.Second, "", 1)
	if p == nil {
		t.Fatal("expected non-nil provider")
	}
	// Fill the single slot and hold it.
	p.sem <- struct{}{}
	defer func() { <-p.sem }()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	_, err := p.Resolve(ctx, Resource{Type: SourceURL, Value: "http://example.com"})
	if err == nil {
		t.Fatal("expected error when context cancelled while waiting on semaphore")
	}
}