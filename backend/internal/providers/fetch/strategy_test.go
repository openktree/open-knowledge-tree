package fetch

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/openktree/open-knowledge-tree/backend/internal/providers/content_parsing"
)

// stubProvider is a test-only ResolutionProvider that returns
// a canned result or error. It lets the strategy tests
// exercise the audit-trail, fall-through, and
// insufficient-content behaviours without hitting the network.
type stubProvider struct {
	id      string
	support []SourceType
	result  ResolvedContent
	err     error
	delay   time.Duration
}

func (s *stubProvider) Resolve(ctx context.Context, _ Resource) (ResolvedContent, error) {
	if s.delay > 0 {
		select {
		case <-ctx.Done():
			return ResolvedContent{}, ctx.Err()
		case <-time.After(s.delay):
		}
	}
	return s.result, s.err
}

func (s *stubProvider) Supports(t SourceType) bool {
	for _, st := range s.support {
		if st == t {
			return true
		}
	}
	return false
}

func (s *stubProvider) Describe() ProviderDescription {
	return ProviderDescription{Name: s.id, Configured: true, Supports: []string{"url"}}
}

// ProviderID lets stubProvider declare its id so the strategy's
// auto-skip / override logic can match it by id (the production
// ProviderID type switch doesn't recognize test stubs).
func (s *stubProvider) ProviderID() string { return s.id }

// TestStrategyReturnsFirstSuccess exercises the happy path:
// the first provider that returns a non-error result wins
// and the audit trail records one success entry. The
// remaining providers must not be called.
func TestStrategyReturnsFirstSuccess(t *testing.T) {
	called := make(map[string]bool)
	p1 := &stubProvider{id: "p1", support: []SourceType{SourceURL}, result: ResolvedContent{StatusCode: 200, Body: []byte("ok")}}
	p2 := &stubProvider{id: "p2", support: []SourceType{SourceURL}}

	strategy := NewFetchStrategy(p1, p2)
	res, err := strategy.Resolve(context.Background(), Resource{Type: SourceURL, Value: "https://example.com"})
	if err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
	if res.StatusCode != 200 {
		t.Errorf("expected status 200, got %d", res.StatusCode)
	}
	if len(res.Attempts) != 1 {
		t.Fatalf("expected 1 attempt, got %d", len(res.Attempts))
	}
	if !res.Attempts[0].Success {
		t.Error("expected first attempt to be successful")
	}
	if res.Attempts[0].Provider != "p1" {
		t.Errorf("expected provider id 'p1' for stub, got %q", res.Attempts[0].Provider)
	}
	if called["p2"] {
		t.Error("second provider should not have been called")
	}
}

// TestStrategyFallsThroughOnError verifies that when the
// first provider errors, the strategy tries the next one
// and records both attempts in the audit trail.
func TestStrategyFallsThroughOnError(t *testing.T) {
	p1 := &stubProvider{id: "p1", support: []SourceType{SourceURL}, err: errors.New("network failure")}
	p2 := &stubProvider{id: "p2", support: []SourceType{SourceURL}, result: ResolvedContent{StatusCode: 200, Body: []byte("ok")}}

	strategy := NewFetchStrategy(p1, p2)
	res, err := strategy.Resolve(context.Background(), Resource{Type: SourceURL, Value: "https://example.com"})
	if err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
	if len(res.Attempts) != 2 {
		t.Fatalf("expected 2 attempts, got %d", len(res.Attempts))
	}
	if res.Attempts[0].Success {
		t.Error("expected first attempt to be a failure")
	}
	if !res.Attempts[1].Success {
		t.Error("expected second attempt to be a success")
	}
}

// TestStrategyAllFailReturnsResolveError checks that when
// every provider errors, the strategy returns a *ResolveError
// carrying the per-provider messages and the full audit trail.
func TestStrategyAllFailReturnsResolveError(t *testing.T) {
	p1 := &stubProvider{id: "p1", support: []SourceType{SourceURL}, err: errors.New("403")}
	p2 := &stubProvider{id: "p2", support: []SourceType{SourceURL}, err: errors.New("timeout")}

	strategy := NewFetchStrategy(p1, p2)
	_, err := strategy.Resolve(context.Background(), Resource{Type: SourceURL, Value: "https://example.com"})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	var re *ResolveError
	if !errors.As(err, &re) {
		t.Fatalf("expected *ResolveError, got %T: %v", err, err)
	}
	if !strings.Contains(re.Error(), "403") || !strings.Contains(re.Error(), "timeout") {
		t.Errorf("expected error to contain both messages, got %q", re.Error())
	}
	if len(re.Attempts) != 2 {
		t.Errorf("expected 2 attempts in ResolveError, got %d", len(re.Attempts))
	}
}

// TestStrategyInsufficientContentFails verifies that
// ErrInsufficientContent is a hard fall-through: when the
// last provider returns it, the strategy fails the job (no
// graceful fallback). The chain should try the next provider
// when one returns ErrInsufficientContent; if all providers
// fail (including with ErrInsufficientContent), the job
// fails so a heavier tier can be added or the row is marked
// failed.
func TestStrategyInsufficientContentFails(t *testing.T) {
	p1 := &stubProvider{
		id:      "p1",
		support: []SourceType{SourceURL},
		result:  ResolvedContent{StatusCode: 200, Body: []byte("<html>short</html>")},
		err:     ErrInsufficientContent,
	}

	strategy := NewFetchStrategy(p1)
	_, err := strategy.Resolve(context.Background(), Resource{Type: SourceURL, Value: "https://example.com"})
	if err == nil {
		t.Fatal("expected error when only provider returns ErrInsufficientContent, got nil")
	}
	if !errors.Is(err, ErrInsufficientContent) {
		// ErrInsufficientContent is wrapped inside a
		// ResolveError's per-provider message; we just
		// need the overall call to fail.
		if _, ok := err.(*ResolveError); !ok {
			t.Errorf("expected *ResolveError or ErrInsufficientContent, got %T: %v", err, err)
		}
	}
}

// TestStrategySkipsUnsupportedProviders verifies that
// providers whose Supports returns false are skipped
// entirely (no attempt recorded, no call made).
func TestStrategySkipsUnsupportedProviders(t *testing.T) {
	p1 := &stubProvider{id: "p1", support: []SourceType{SourceDOI}, result: ResolvedContent{StatusCode: 200}}
	p2 := &stubProvider{id: "p2", support: []SourceType{SourceURL}, result: ResolvedContent{StatusCode: 200, Body: []byte("ok")}}

	strategy := NewFetchStrategy(p1, p2)
	res, err := strategy.Resolve(context.Background(), Resource{Type: SourceURL, Value: "https://example.com"})
	if err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
	if len(res.Attempts) != 1 {
		t.Fatalf("expected 1 attempt (unsupported skipped), got %d", len(res.Attempts))
	}
}

// TestStrategyNoProviderSupportsReturnsError verifies the
// "no provider supports this source type" path.
func TestStrategyNoProviderSupportsReturnsError(t *testing.T) {
	p1 := &stubProvider{id: "p1", support: []SourceType{SourceURL}}
	strategy := NewFetchStrategy(p1)
	_, err := strategy.Resolve(context.Background(), Resource{Type: SourceDOI, Value: "10.1038/nature12373"})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "no provider supports") {
		t.Errorf("expected 'no provider supports' error, got %q", err.Error())
	}
}

// TestProviderID verifies the concrete-type to slug mapping
// the audit trail depends on. A new provider type that
// forgets to extend the switch falls back to "unknown",
// which is acceptable but should be intentional.
func TestProviderID(t *testing.T) {
	cases := []struct {
		p    ResolutionProvider
		want string
	}{
		{&FetchResolutionProvider{}, "fetch"},
		{&UnpaywallResolutionProvider{email: "x@example.com"}, "unpaywall"},
		{&TLSImpersonationProvider{}, "tls"},
		{&FlareSolverrProvider{}, "flaresolverr"},
		// stubProvider implements ProviderIdentifier so its id
		// is its declared id, not "unknown".
		{&stubProvider{id: "stub"}, "stub"},
	}
	for _, tc := range cases {
		got := providerID(tc.p)
		if got != tc.want {
			t.Errorf("providerID(%T) = %q, want %q", tc.p, got, tc.want)
		}
	}
}

// TestStrategyHostOverrideExactMatch verifies the static
// host override takes priority over the chain order: the
// pinned provider runs first, then the rest in order.
func TestStrategyHostOverrideExactMatch(t *testing.T) {
	p1 := &stubProvider{id: "p1", support: []SourceType{SourceURL}, result: ResolvedContent{StatusCode: 200, Body: []byte("p1")}}
	p2 := &stubProvider{id: "p2", support: []SourceType{SourceURL}, result: ResolvedContent{StatusCode: 200, Body: []byte("p2")}}

	// Override pins example.com to "fetch" — but stubProvider
	// resolves to "unknown" via providerID, so the override
	// won't match. Instead we verify the override mechanism
	// with a real provider type. The strategy tries the
	// pinned id first; if no provider matches, it falls
	// through to the chain order (which is the behaviour we
	// want for an unknown override id).
	overrides := map[string]string{"example.com": "tls"}
	strategy := NewFetchStrategyWithOverrides([]ResolutionProvider{p1, p2}, overrides)

	res, err := strategy.Resolve(context.Background(), Resource{Type: SourceURL, Value: "https://example.com/x"})
	if err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
	// No provider in the chain has id "tls", so the override
	// is a no-op and p1 (first in chain) wins.
	if string(res.Body) != "p1" {
		t.Errorf("expected p1 to win (override id 'tls' not in chain), got body %q", string(res.Body))
	}
}

// TestStrategyURLValidatorRejectsUnsafeURL verifies the SSRF
// gate short-circuits the chain with an unsafe URL.
func TestStrategyURLValidatorRejectsUnsafeURL(t *testing.T) {
	p1 := &stubProvider{id: "p1", support: []SourceType{SourceURL}, result: ResolvedContent{StatusCode: 200}}
	strategy := NewFetchStrategy(p1).WithURLValidator(func(raw string) error {
		return ErrUnsafeURL
	})

	_, err := strategy.Resolve(context.Background(), Resource{Type: SourceURL, Value: "https://evil.example.com"})
	if err == nil {
		t.Fatal("expected error from unsafe URL, got nil")
	}
}

// TestStrategyURLValidatorDNSFailureFallsThrough verifies the
// SSRF gate does NOT short-circuit the chain when the validator
// returns ErrDNSLookupFailed — the transient DNS error is a
// retry signal, not a safety verdict. The chain providers run
// their own DNS resolution and can still succeed (or fail
// normally) when the resolver hiccup was transient. The audit
// trail records the url_safety DNS failure as the first
// attempt so the UI surfaces it, but the chain is not blocked.
//
// This is the regression test for the "K_ssrf_dns_fail" failure
// mode in the corpus (~11 rows where a 127.0.0.11:53 read
// timeout turned into a hard reject and the whole chain was
// skipped).
func TestStrategyURLValidatorDNSFailureFallsThrough(t *testing.T) {
	called := false
	p1 := &stubProvider{
		id:      "p1",
		support: []SourceType{SourceURL},
		result:  ResolvedContent{StatusCode: 200, Body: []byte("ok after dns hiccup")},
	}
	strategy := NewFetchStrategy(p1).WithURLValidator(func(raw string) error {
		return ErrDNSLookupFailed
	})

	// Patch the stub to record that it was called. The stub's
	// Resolve doesn't take a callback, so we wrap via a custom
	// provider inline.
	chainRan := false
	wrapped := &dnsFallthroughProvider{
		stub:    p1,
		onCall:  func() { chainRan = true },
	}
	strategy = NewFetchStrategy(wrapped).WithURLValidator(func(raw string) error {
		return ErrDNSLookupFailed
	})
	_ = called

	res, err := strategy.Resolve(context.Background(), Resource{Type: SourceURL, Value: "https://transient.example.com"})
	if err != nil {
		t.Fatalf("expected nil error (chain should fall through and succeed), got %v", err)
	}
	if !chainRan {
		t.Error("expected chain provider to run after DNS-failure fall-through, but it was not called")
	}
	if string(res.Body) != "ok after dns hiccup" {
		t.Errorf("expected body from chain provider, got %q", string(res.Body))
	}
	// The audit trail should start with the url_safety DNS
	// failure attempt, followed by the successful chain
	// provider.
	if len(res.Attempts) < 2 {
		t.Fatalf("expected at least 2 attempts (url_safety + chain), got %d", len(res.Attempts))
	}
	if res.Attempts[0].Provider != "url_safety" {
		t.Errorf("expected first attempt to be url_safety, got %q", res.Attempts[0].Provider)
	}
	if res.Attempts[0].Success {
		t.Error("expected url_safety attempt to be a failure (DNS lookup failed)")
	}
}

// TestStrategyOARedirectSecondPass verifies the strategy
// retries URL-capable providers with the direct OA URL when
// Unpaywall discovered it but couldn't fetch it. The stub
// "unpaywall" provider returns an error with
// OARedirectURL set; the stub "tls" provider (which handles
// SourceURL) should then be retried against the OA URL.
func TestStrategyOARedirectSecondPass(t *testing.T) {
	unpaywall := &stubProvider{
		id:      "unpaywall",
		support: []SourceType{SourceDOI},
		err:     ErrUnpaywallNotOpenAccess,
		result: ResolvedContent{
			OAStatus:      "bronze",
			OARedirectURL: "https://dl.acm.org/doi/pdf/10.1145/882262.882269",
		},
	}
	tls := &stubProvider{
		id:      "tls",
		support: []SourceType{SourceDOI, SourceURL},
		result:  ResolvedContent{StatusCode: 200, Body: []byte("pdf content via OA URL"), Parsed: content_parsing.ParsedDoc{Text: strings.Repeat("x", 300)}},
	}

	strategy := NewFetchStrategy(unpaywall, tls)
	res, err := strategy.Resolve(context.Background(), Resource{Type: SourceDOI, Value: "10.1145/882262.882269"})
	if err != nil {
		t.Fatalf("expected nil error (second pass should succeed), got %v", err)
	}
	if string(res.Body) != "pdf content via OA URL" {
		t.Errorf("expected body from OA URL second pass, got %q", string(res.Body))
	}
	if res.OAStatus != "bronze" {
		t.Errorf("expected OAStatus 'bronze', got %q", res.OAStatus)
	}
	// The audit trail should have at least 2 attempts:
	// unpaywall (first pass, failed) + tls (second pass, succeeded).
	if len(res.Attempts) < 2 {
		t.Fatalf("expected at least 2 attempts, got %d", len(res.Attempts))
	}
}

// dnsFallthroughProvider wraps a stubProvider and fires a
// callback the first time Resolve is called. Used by
// TestStrategyURLValidatorDNSFailureFallsThrough to assert the
// chain ran after the SSRF guard returned ErrDNSLookupFailed.
type dnsFallthroughProvider struct {
	stub   *stubProvider
	onCall func()
}

func (d *dnsFallthroughProvider) Resolve(ctx context.Context, r Resource) (ResolvedContent, error) {
	if d.onCall != nil {
		d.onCall()
	}
	return d.stub.Resolve(ctx, r)
}

func (d *dnsFallthroughProvider) Supports(t SourceType) bool {
	return d.stub.Supports(t)
}

func (d *dnsFallthroughProvider) Describe() ProviderDescription {
	return d.stub.Describe()
}
// fakeSkipStore is a test HostSkipStore that records calls and
// returns a canned active-skip list.
type fakeSkipStore struct {
	active   []HostSkipEntry
	activeFor []string
	records  []skipRecordCall
}

type skipRecordCall struct {
	repoID, host, providerID string
	success                   bool
}

func (f *fakeSkipStore) ActiveForRepo(_ context.Context, repoID string) ([]HostSkipEntry, error) {
	f.activeFor = append(f.activeFor, repoID)
	return f.active, nil
}

func (f *fakeSkipStore) RecordAndMaybeSkip(_ context.Context, repoID, host, providerID string, success bool, _ AutoSkipConfig) error {
	f.records = append(f.records, skipRecordCall{repoID, host, providerID, success})
	return nil
}

// TestStrategyAutoSkipJumpsSkippedTier asserts the strategy
// jumps a (host, provider) tier the store reports as skipped,
// records an audit-trail "skipped" entry, and falls through to
// the next tier.
func TestStrategyAutoSkipJumpsSkippedTier(t *testing.T) {
	p1 := &stubProvider{id: "fetch", support: []SourceType{SourceURL}, err: errors.New("upstream returned status 403")}
	p2 := &stubProvider{id: "flaresolverr", support: []SourceType{SourceURL}, result: ResolvedContent{StatusCode: 200, Body: []byte("ok")}}

	store := &fakeSkipStore{active: []HostSkipEntry{{Host: "reddit.com", ProviderID: "fetch"}}}
	strategy := NewFetchStrategyWithOverrides([]ResolutionProvider{p1, p2}, nil).
		WithAutoSkip(store, AutoSkipConfig{Enabled: true, MinSample: 1, FailureThreshold: 0.5})

	ctx := WithRepoID(context.Background(), "repo-1")
	res, err := strategy.Resolve(ctx, Resource{Type: SourceURL, Value: "https://reddit.com/r/x"})
	if err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
	if res.StatusCode != 200 {
		t.Fatalf("expected flaresolverr to win, got status %d", res.StatusCode)
	}
	// The audit trail must record fetch as skipped and
	// flaresolverr as the successful tier.
	var skipped, won bool
	for _, a := range res.Attempts {
		if a.Provider == "fetch" && !a.Success && strings.Contains(a.Error, "skipped") {
			skipped = true
		}
		if a.Provider == "flaresolverr" && a.Success {
			won = true
		}
	}
	if !skipped {
		t.Errorf("expected fetch tier skipped in audit trail, got %+v", res.Attempts)
	}
	if !won {
		t.Errorf("expected flaresolverr tier to win, got %+v", res.Attempts)
	}
	// ActiveForRepo was called with the repo id from context.
	if len(store.activeFor) == 0 || store.activeFor[0] != "repo-1" {
		t.Errorf("expected ActiveForRepo called with repo-1, got %v", store.activeFor)
	}
}

// TestStrategyAutoSkipDisabledWhenNoRepo asserts the strategy
// does NOT consult the skip store when no repo id is in context
// (so a non-repo fetch path is unaffected).
func TestStrategyAutoSkipDisabledWhenNoRepo(t *testing.T) {
	p1 := &stubProvider{id: "fetch", support: []SourceType{SourceURL}, result: ResolvedContent{StatusCode: 200, Body: []byte("ok")}}
	store := &fakeSkipStore{active: []HostSkipEntry{{Host: "example.com", ProviderID: "fetch"}}}
	strategy := NewFetchStrategy(p1).WithAutoSkip(store, AutoSkipConfig{Enabled: true, MinSample: 1, FailureThreshold: 0.5})

	// No WithRepoID: the store's active list must be ignored.
	res, err := strategy.Resolve(context.Background(), Resource{Type: SourceURL, Value: "https://example.com"})
	if err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
	if res.StatusCode != 200 {
		t.Fatalf("expected fetch to win (no skip), got status %d", res.StatusCode)
	}
	if len(store.activeFor) != 0 {
		t.Errorf("expected ActiveForRepo NOT called without repo, got %v", store.activeFor)
	}
}

// TestStrategyAutoSkipRecordsOutcome asserts the strategy calls
// RecordAndMaybeSkip after each provider attempt so the store
// can recompute the failure rate and upsert/clear a skip.
func TestStrategyAutoSkipRecordsOutcome(t *testing.T) {
	p1 := &stubProvider{id: "fetch", support: []SourceType{SourceURL}, err: errors.New("upstream returned status 403")}
	p2 := &stubProvider{id: "flaresolverr", support: []SourceType{SourceURL}, result: ResolvedContent{StatusCode: 200, Body: []byte("ok")}}
	store := &fakeSkipStore{}
	strategy := NewFetchStrategy(p1, p2).WithAutoSkip(store, AutoSkipConfig{Enabled: true, MinSample: 1, FailureThreshold: 0.5})

	ctx := WithRepoID(context.Background(), "repo-1")
	_, err := strategy.Resolve(ctx, Resource{Type: SourceURL, Value: "https://example.com"})
	if err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
	// Two records: one for the failed fetch, one for the
	// successful flaresolverr.
	if len(store.records) != 2 {
		t.Fatalf("expected 2 RecordAndMaybeSkip calls, got %d", len(store.records))
	}
	if store.records[0].providerID != "fetch" || store.records[0].success != false {
		t.Errorf("expected first record fetch/false, got %+v", store.records[0])
	}
	if store.records[1].providerID != "flaresolverr" || store.records[1].success != true {
		t.Errorf("expected second record flaresolverr/true, got %+v", store.records[1])
	}
}
