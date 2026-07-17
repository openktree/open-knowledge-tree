package fetch

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// perProviderTimeout is the wall-clock budget each provider gets
// inside the strategy. A sub-context wraps every Resolve call so
// a slow tier cannot starve the fallbacks — the previous
// implementation shared one 30s budget across the whole chain.
const perProviderTimeout = 60 * time.Second

// URLValidator is the injectable SSRF guard the strategy runs
// before any provider. The default production wiring uses
// ValidateFetchURL; tests pass nil to disable the check so the
// suite does not depend on DNS.
type URLValidator func(rawURL string) error

// providerID returns a stable slug for a concrete provider
// instance. It mirrors the type-switch the handler uses for
// /sources/providers so the audit trail's "provider" field
// matches the id operators already see in the UI.
func providerID(p ResolutionProvider) string {
	return ProviderID(p)
}

// ProviderID is the exported form of providerID. It returns the
// stable slug for a concrete provider instance (e.g. "fetch",
// "tls", "unpaywall", "flaresolverr"). Used by the
// ProviderRegistry so the per-repository settings feature can
// enumerate the live resolution-provider ids without duplicating
// the type switch.
func ProviderID(p ResolutionProvider) string {
	switch p.(type) {
	case *FetchResolutionProvider:
		return "fetch"
	case *UnpaywallResolutionProvider:
		return "unpaywall"
	case *TLSImpersonationProvider:
		return "tls"
	case *FlareSolverrProvider:
		return "flaresolverr"
	default:
		return "unknown"
	}
}

type FetchStrategy struct {
	providers     []ResolutionProvider
	hostOverrides map[string]string
	urlValidator  URLValidator
}

// NewFetchStrategy builds a strategy with the given providers
// in chain order, no static host overrides, and no SSRF
// validation. Existing call sites that don't need the
// robustness features keep working unchanged.
func NewFetchStrategy(providers ...ResolutionProvider) *FetchStrategy {
	return &FetchStrategy{providers: providers}
}

// NewFetchStrategyWithOverrides builds a strategy with static
// host overrides. The map pins a host to a provider id (e.g.
// "www.cell.com" → "flaresolverr") so the strategy tries that
// provider first for matching hosts, without waiting for any
// learning machinery. Exact host match first, then suffix
// match (so "cell.com" matches "www.cell.com"). Pass nil to
// disable overrides and use the chain order as-is.
func NewFetchStrategyWithOverrides(
	providers []ResolutionProvider,
	hostOverrides map[string]string,
) *FetchStrategy {
	return &FetchStrategy{providers: providers, hostOverrides: hostOverrides}
}

// WithURLValidator returns a copy of the strategy with the
// given SSRF validator wired. The validator runs before any
// provider; an unsafe URL short-circuits the chain with
// ErrUnsafeURL. Pass nil to disable. The method is a builder
// so the production wiring can chain it without exploding
// the constructor signature.
func (s *FetchStrategy) WithURLValidator(v URLValidator) *FetchStrategy {
	cp := *s
	cp.urlValidator = v
	return &cp
}

func (s *FetchStrategy) Providers() []ResolutionProvider {
	return s.providers
}

// staticOverride returns the provider id the strategy should
// try first for the resource's host, or "" when no static
// override matches. Lookup is exact-host first, then suffix
// match so "cell.com" matches "www.cell.com".
func (s *FetchStrategy) staticOverride(resource Resource) string {
	if s.hostOverrides == nil || resource.Type != SourceURL {
		return ""
	}
	host := hostOf(resource.Value)
	if host == "" {
		return ""
	}
	if id, ok := s.hostOverrides[host]; ok && id != "" {
		return id
	}
	for cfgHost, id := range s.hostOverrides {
		if id == "" {
			continue
		}
		if host == cfgHost || strings.HasSuffix(host, "."+cfgHost) {
			return id
		}
	}
	return ""
}

// orderedProviders returns the chain reordered so the
// overridden provider (if any and registered) runs first,
// followed by the rest in original order.
func (s *FetchStrategy) orderedProviders(t SourceType, preferred string) []ResolutionProvider {
	if preferred == "" {
		var out []ResolutionProvider
		for _, p := range s.providers {
			if p.Supports(t) {
				out = append(out, p)
			}
		}
		return out
	}
	var (
		out      []ResolutionProvider
		prefP    ResolutionProvider
		prefSeen bool
	)
	for _, p := range s.providers {
		if providerID(p) == preferred && p.Supports(t) {
			prefP = p
			prefSeen = true
			continue
		}
		if p.Supports(t) {
			out = append(out, p)
		}
	}
	if prefSeen {
		return append([]ResolutionProvider{prefP}, out...)
	}
	return out
}

// hostOf extracts the lowercased host from a URL. Used by
// staticOverride to key host → provider overrides.
func hostOf(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil || u.Host == "" {
		return strings.ToLower(strings.TrimSpace(rawURL))
	}
	return strings.ToLower(u.Host)
}

// Resolve runs the configured providers in order, skipping
// any that don't Support the resource's source type. Each
// provider runs under its own perProviderTimeout sub-context
// so a slow tier cannot starve the fallbacks. On success the
// returned ResolvedContent carries the full Attempts audit
// trail; on all-failed the *ResolveError carries the
// per-provider failure messages and the audit trail.
//
// A static host override (when configured) tries the pinned
// provider first, then falls through the rest of the chain
// in order. This is the KISS replacement for the learned
// host-preference machinery: operators pin known-bad hosts
// in YAML, the chain does the rest.
//
// The SSRF validator (when wired) runs before any provider
// for SourceURL resources, short-circuiting unsafe URLs.
//
// When Unpaywall discovers a direct OA URL (e.g. a publisher
// PDF link) but can't fetch it (403, network error), the
// strategy retries the remaining URL-capable providers (TLS,
// fetch, FlareSolverr) with that specific URL instead of the
// DOI redirect. A different TLS fingerprint or the plain
// HTTP client might get through where Unpaywall's client got
// 403 — this is the standard fallback path for WAF-blocked OA
// copies.
func (s *FetchStrategy) Resolve(ctx context.Context, resource Resource) (ResolvedContent, error) {
	// SSRF gate. Runs before any provider so a hostile or
	// malformed URL never reaches net/http.
	//
	// ErrDNSLookupFailed is a *transient* failure (the resolver
	// was unreachable, not the URL is unsafe). It is recorded
	// as a url_safety attempt in the audit trail but does NOT
	// short-circuit the chain: the fetch providers run their
	// own DNS resolution and will fail the same way if the host
	// is truly unreachable, while a transient docker-DNS hiccup
	// gets a second chance. This is the fix for the
	// "K_ssrf_dns_fail" failure mode (~11 rows in the corpus
	// where a 127.0.0.11:53 read timeout turned into a hard
	// reject).
	if s.urlValidator != nil && resource.Type == SourceURL {
		if err := s.urlValidator(resource.Value); err != nil {
			if errors.Is(err, ErrDNSLookupFailed) {
				// Record the transient DNS failure in the audit
				// trail so the UI shows why the first tier
				// didn't run, then fall through to the chain.
				// The chain providers will retry DNS themselves.
				dnsAttempt := FetchAttempt{
					Provider: "url_safety",
					Success:  false,
					Error:    err.Error(),
				}
				result, attempts, oaStatus, _, err2 := s.runChain(ctx, resource, "")
				attempts = append([]FetchAttempt{dnsAttempt}, attempts...)
				if err2 == nil {
					result.Attempts = attempts
					result.OAStatus = oaStatus
					return result, nil
				}
				return ResolvedContent{Attempts: attempts, OAStatus: oaStatus}, &ResolveError{
					Resource: string(resource.Type),
					Errors:   []string{err.Error(), err2.Error()},
					Attempts: attempts,
				}
			}
			return ResolvedContent{Attempts: []FetchAttempt{{
				Provider: "url_safety",
				Success:  false,
				Error:    err.Error(),
			}}}, err
		}
	}

	result, attempts, oaStatus, oaRedirectURL, err := s.runChain(ctx, resource, "")
	if err == nil {
		// First pass succeeded. But if Unpaywall discovered
		// a direct OA URL (e.g. a PDF link) that it couldn't
		// fetch (got a consent page instead), try the second
		// pass — a different tier might get the actual PDF.
		// Only do this when the first-pass result has
		// OARedirectURL set (Unpaywall returned
		// ErrInsufficientContent but a later tier succeeded
		// with the DOI landing page).
		if oaRedirectURL == "" {
			return result, nil
		}
		// The first pass gave us content, but it might be
		// just the landing page. Try the OA URL; if the
		// second pass succeeds, prefer its result (it
		// should be the actual PDF/article). If the second
		// pass fails, keep the first-pass result.
	} else {
		// Second pass: if Unpaywall discovered a direct OA URL
		// but couldn't fetch it, retry the URL-capable providers
		// with that specific URL. The first pass ran the chain
		// against the DOI (which redirects to the landing page);
		// the second pass runs against the direct PDF URL, which
		// a different TLS fingerprint might reach.
		if oaRedirectURL == "" {
			if len(attempts) == 0 {
				return ResolvedContent{Attempts: attempts, OAStatus: oaStatus}, fmt.Errorf("no provider supports source type %q", resource.Type)
			}
			return ResolvedContent{Attempts: attempts, OAStatus: oaStatus}, &ResolveError{
				Resource: string(resource.Type),
				Errors:   []string{err.Error()},
				Attempts: attempts,
			}
		}
	}

	// SSRF-validate the OA redirect URL before the second
	// pass. Unpaywall URLs are user-influenceable.
	//
	// As with the initial gate, ErrDNSLookupFailed is treated
	// as a transient fall-through: the OA-URL pass runs the
	// URL-capable providers, which retry DNS themselves. A
	// hard ErrUnsafeURL still rejects.
	if s.urlValidator != nil {
		if err := s.urlValidator(oaRedirectURL); err != nil {
			// If the first pass succeeded, keep its result
			// rather than failing — some content is better
			// than none.
			if err == nil {
				result.Attempts = attempts
				result.OAStatus = oaStatus
				return result, nil
			}
			if !errors.Is(err, ErrDNSLookupFailed) {
				// Hard reject. The OA URL is unsafe (private
				// IP, forbidden scheme, etc.); don't run the
				// second pass against it.
				return ResolvedContent{Attempts: attempts, OAStatus: oaStatus}, &ResolveError{
					Resource: string(resource.Type),
					Errors:   []string{fmt.Sprintf("OA redirect URL rejected by SSRF guard: %v", err)},
					Attempts: attempts,
				}
			}
			// ErrDNSLookupFailed: fall through to the OA-URL
			// pass. The chain providers will retry DNS
			// themselves; if the host is truly unreachable
			// they fail the same way, but a transient
			// docker-DNS hiccup gets a second chance.
		}
	}

	// Run the URL-capable providers against the OA URL.
	// The resource type changes to SourceURL so the
	// URL-capable providers (TLS, fetch, FlareSolverr) claim
	// it. Unpaywall (DOI-only) is naturally skipped.
	oaResource := Resource{Type: SourceURL, Value: oaRedirectURL, DOI: resource.DOI}
	result2, attempts2, oaStatus2, _, err2 := s.runChain(ctx, oaResource, oaRedirectURL)
	// Merge the second-pass attempts into the first-pass
	// audit trail so the UI shows the full picture.
	attempts = append(attempts, attempts2...)
	if oaStatus == "" && oaStatus2 != "" {
		oaStatus = oaStatus2
	}
	if err2 == nil {
		result2.Attempts = attempts
		result2.OAStatus = oaStatus
		return result2, nil
	}

	// Second pass failed. If the first pass succeeded, keep
	// its result (landing-page content is better than nothing).
	if err == nil {
		result.Attempts = attempts
		result.OAStatus = oaStatus
		return result, nil
	}

	return ResolvedContent{Attempts: attempts, OAStatus: oaStatus}, &ResolveError{
		Resource: string(resource.Type),
		Errors:   []string{err.Error(), fmt.Sprintf("OA URL pass: %s", err2.Error())},
		Attempts: attempts,
	}
}

// runChain executes the provider chain for the given resource.
// When skipURL is non-empty, providers that already ran for
// that URL in a previous pass are identified by the Unpaywall
// provider (which only handles SourceDOI, so it's naturally
// skipped when the resource is SourceURL). The method returns
// the result (if successful), the audit trail, the OA status,
// the OA redirect URL (if Unpaywall discovered one), and an
// error (if all providers failed).
func (s *FetchStrategy) runChain(ctx context.Context, resource Resource, _ string) (ResolvedContent, []FetchAttempt, string, string, error) {
	ordered := s.orderedProviders(resource.Type, s.staticOverride(resource))

	var (
		errs          []string
		attempts      []FetchAttempt
		oaStatus      string
		oaRedirectURL string
	)

	for _, p := range ordered {
		pid := providerID(p)

		pctx, cancel := context.WithTimeout(ctx, perProviderTimeout)
		start := time.Now()
		result, err := p.Resolve(pctx, resource)
		elapsed := time.Since(start).Milliseconds()
		cancel()

		if err != nil {
			attempts = append(attempts, FetchAttempt{
				Provider:  pid,
				Success:   false,
				Error:     err.Error(),
				ElapsedMs: elapsed,
				OAStatus:  result.OAStatus,
			})
			if oaStatus == "" && result.OAStatus != "" {
				oaStatus = result.OAStatus
			}
			// Capture the OA redirect URL Unpaywall
			// discovered, so the strategy can retry
			// with it on a second pass.
			if oaRedirectURL == "" && result.OARedirectURL != "" {
				oaRedirectURL = result.OARedirectURL
			}
			errs = append(errs, fmt.Sprintf("%s: %s", pid, err.Error()))
			continue
		}

		attempts = append(attempts, FetchAttempt{
			Provider:  pid,
			Success:   true,
			ElapsedMs: elapsed,
			OAStatus:  result.OAStatus,
		})
		if oaStatus == "" && result.OAStatus != "" {
			oaStatus = result.OAStatus
		}
		result.Attempts = attempts
		result.OAStatus = oaStatus
		return result, attempts, oaStatus, oaRedirectURL, nil
	}

	if len(errs) == 0 {
		return ResolvedContent{}, attempts, oaStatus, oaRedirectURL, fmt.Errorf("no provider supports source type %q", resource.Type)
	}

	return ResolvedContent{}, attempts, oaStatus, oaRedirectURL, &ResolveError{
		Resource: string(resource.Type),
		Errors:   errs,
		Attempts: attempts,
	}
}

// ResolveError is the structured all-providers-failed error
// returned by Resolve. It carries the per-provider failure
// messages and the full Attempts audit trail so callers (the
// worker, tests) can surface "which tier failed and why"
// without re-running the chain.
type ResolveError struct {
	Resource string
	Errors   []string
	Attempts []FetchAttempt
}

func (e *ResolveError) Error() string {
	return fmt.Sprintf("all providers failed for %q: %s", e.Resource, strings.Join(e.Errors, "; "))
}

func (e *ResolveError) Unwrap() []error {
	out := make([]error, len(e.Errors))
	for i, msg := range e.Errors {
		out[i] = errors.New(msg)
	}
	return out
}