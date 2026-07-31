package fetch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/openktree/open-knowledge-tree/backend/internal/config"
	"github.com/openktree/open-knowledge-tree/backend/internal/providers/content_parsing"
)

// Unpaywall API base. The endpoint pattern is
// https://api.unpaywall.org/v2/<doi>?email=<email>; the
// response describes OA locations for the work. The email
// query param is required by Unpaywall's TOS — they use it
// as a polite-contact identifier and to throttle abusive
// callers. The email is also the de-facto API key, so we
// follow the same env-var pattern as the OpenAlex search
// provider.
const unpaywallAPIBase = "https://api.unpaywall.org/v2"

// unpaywallRetryAttempts is the number of extra attempts the
// provider makes when Unpaywall returns 429 (Too Many
// Requests) or a 5xx. The reference Python implementation
// had no retry; this is a small, bounded retry (1 extra
// attempt with a 1s backoff) so a transient rate-limit does
// not collapse the OA path to the publisher landing page.
const unpaywallRetryAttempts = 1

// unpaywallRetryBackoff is the wait between retry attempts.
// Short enough that the per-provider timeout budget still
// leaves room for the OA-location fetch.
const unpaywallRetryBackoff = 1 * time.Second

// ErrUnpaywallNotOpenAccess is returned by Resolve when
// Unpaywall has a record for the DOI but the work has no
// open-access location. The strategy treats this as a
// "skip this provider and try the next one" signal, which
// is the right behavior: the caller (typically a
// RetrieveSource worker that was given a DOI) should fall
// back to the plain HTTP fetch and land on the publisher
// landing page rather than fail the job.
//
// We expose it as a sentinel so callers and tests can
// distinguish "the API said it's closed" from "the network
// call failed" — the former is a normal control-flow case
// and the latter is a real error that should be retried by
// River.
var ErrUnpaywallNotOpenAccess = errors.New("unpaywall: no open-access location for DOI")

// UnpaywallResolutionProvider implements ResolutionProvider
// against the Unpaywall v2 API. It only supports SourceDOI:
// the input shape is the bare DOI string and the provider
// looks up the work's OA locations.
//
// The "content" it returns is the body fetched from the OA
// location's URL (the first one Unpaywall considers
// authoritative — see selectOALocation for the preference
// order). The body is parsed with the wired content
// parsers (Trafilatura for HTML, MuPDF for PDF) so the
// returned ResolvedContent.Parsed is populated the same way
// the plain FetchResolutionProvider populates it. This
// closes the previous gap where OA HTML fetched via
// Unpaywall was never run through Trafilatura and the row
// ended up with parse_status='unsupported'.
//
// An empty email disables the provider at construction
// time: the constructor returns nil and the wiring layer
// skips registration. This mirrors the "missing API key"
// pattern in the search providers and keeps the worker
// code path-agnostic about whether Unpaywall is enabled.
type UnpaywallResolutionProvider struct {
	email      string
	userAgent  string
	httpClient *http.Client
	parsers    []content_parsing.Parser
	// retryCfg governs the OA-location fetch only (the
	// publisher/repo URL the provider follows after the
	// Unpaywall API lookup). The API lookup itself keeps
	// its own inline 1-retry/1s loop (unpaywallRetry*
	// below); the two paths have different failure modes
	// (API 429 vs OA-host 403/timeout) and different
	// budgets. Zero-value = no retry (NoRetryConfig); the
	// production wiring passes defaultRetryConfig so the
	// OA fetch gets the same 3-attempt/2-15s-backoff
	// budget with retry_403_max_attempts=2 as the fetch
	// and tls tiers.
	retryCfg RetryConfig
}

// NewUnpaywallResolutionProvider builds the provider. When
// email is empty, the constructor returns nil so the
// caller can use a single conditional without a separate
// "is configured" check; this is the same convention the
// search providers follow (see cmd/app/api.go for
// examples). The provider wires the default parser
// (Trafilatura for HTML) so OA landing-page HTML is parsed;
// pass additional parsers via NewUnpaywallResolutionProviderWithParsers
// to also handle OA PDFs.
func NewUnpaywallResolutionProvider(email string) *UnpaywallResolutionProvider {
	return NewUnpaywallResolutionProviderWithParsers(email, content_parsing.NewTrafilaturaParser())
}

// NewUnpaywallResolutionProviderWithParsers lets the wiring
// layer inject the same parser set used by the plain fetch
// provider (typically Trafilatura + FitzPDFParser). The
// OA-location body is parsed by the first parser that
// Supports the detected source type, mirroring the
// FetchResolutionProvider pipeline. Nil parsers are
// skipped; an empty list falls back to Trafilatura.
//
// This constructor keeps the historical behaviour: 60s
// OA-fetch timeout (was 30s; raised to match the
// strategy's perProviderTimeout — see git history for the
// audit-trail evidence) and no retry on the OA fetch. Use
// NewUnpaywallResolutionProviderWithFullConfig for the
// production wiring that enables the 403/timeout retry.
func NewUnpaywallResolutionProviderWithParsers(email string, parsers ...content_parsing.Parser) *UnpaywallResolutionProvider {
	return NewUnpaywallResolutionProviderWithFullConfig(email, 60*time.Second, NoRetryConfig, parsers...)
}

// NewUnpaywallResolutionProviderWithFullConfig is the
// fully-configurable constructor, mirroring
// NewFetchResolutionProviderWithFullConfig. It accepts an
// explicit OA-fetch timeout and retry config (pass
// NoRetryConfig to disable retry) plus the parser set. A
// nil parser, empty email, or zero timeout falls back to
// the defaults (Trafilatura, disabled, 60s). Pass
// defaultRetryConfig (the zero-value normalisation in
// retryWithBackoff applies it) to give the OA fetch the
// same 3-attempt budget as the fetch/tls tiers.
func NewUnpaywallResolutionProviderWithFullConfig(
	email string,
	timeout time.Duration,
	retCfg RetryConfig,
	parsers ...content_parsing.Parser,
) *UnpaywallResolutionProvider {
	if strings.TrimSpace(email) == "" {
		return nil
	}
	cleaned := make([]content_parsing.Parser, 0, len(parsers))
	for _, p := range parsers {
		if p != nil {
			cleaned = append(cleaned, p)
		}
	}
	if len(cleaned) == 0 {
		cleaned = append(cleaned, content_parsing.NewTrafilaturaParser())
	}
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	// Normalise the retry config the same way the fetch
	// provider does: a completely zero RetryConfig means
	// "use defaults" (defaultRetryConfig); a partially-set
	// one with MaxAttempts <= 0 means no retry.
	if retCfg.MaxAttempts <= 0 {
		if retCfg.BaseDelay == 0 && retCfg.MaxDelay == 0 && retCfg.Retry403MaxAttempts == 0 {
			// Caller passed a literal zero-value: treat as
			// "no retry" for the historical constructors
			// (NewUnpaywallResolutionProvider /
			// ...WithParsers pass NoRetryConfig which has
			// MaxAttempts=1, hitting the branch below).
			// The production wiring passes a populated
			// defaultRetryConfig so this branch is not
			// taken.
			retCfg = NoRetryConfig
		} else {
			retCfg.MaxAttempts = 1
		}
	}
	return &UnpaywallResolutionProvider{
		email:     strings.TrimSpace(email),
		userAgent: defaultUserAgent,
		httpClient: &http.Client{
			// Timeout applies to the OA-location fetch
			// (the publisher/repo URL), not the Unpaywall
			// API call (which is fast and rarely hits the
			// ceiling). 60s matches the strategy's
			// perProviderTimeout so a slow-but-legitimate
			// OA host (Europe PMC, arXiv, a publisher with
			// a heavy redirect chain) gets the full tier
			// budget rather than dying at the halfway
			// mark.
			Timeout: timeout,
		},
		parsers:  cleaned,
		retryCfg: retCfg,
	}
}

// NewUnpaywallResolutionProviderFromConfig is the
// config-driven constructor. Email gates the provider on
// (nil-return when empty). Timeout and Retry are threaded
// from config so the OA-location fetch gets the
// configurable 60s budget and the 403/timeout retry
// budget; both default sensibly when zero (60s,
// NoRetryConfig) and the wiring layer (cmd/app/api.go)
// passes the populated defaults from
// configs/config.default.yaml. The Unpaywall API lookup
// keeps its own inline 1-retry/1s loop regardless of this
// config (it's a different failure mode).
func NewUnpaywallResolutionProviderFromConfig(cfg config.UnpaywallProviderConfig) *UnpaywallResolutionProvider {
	retCfg := RetryConfig{
		MaxAttempts:         cfg.Retry.MaxAttempts,
		BaseDelay:           cfg.Retry.BaseDelay,
		MaxDelay:            cfg.Retry.MaxDelay,
		Retry403MaxAttempts: cfg.Retry.Retry403MaxAttempts,
	}
	return NewUnpaywallResolutionProviderWithFullConfig(cfg.Email, cfg.Timeout, retCfg)
}

// HTTPClientTimeout returns the OA-fetch client timeout.
// Exported for the e2e wiring test
// (TestUnpaywallResolutionProvider_RetryBudgetFromConfig)
// so it can assert the production wiring threads the
// configured 60s rather than the old hardcoded 30s without
// reaching into the unexported httpClient field.
func (p *UnpaywallResolutionProvider) HTTPClientTimeout() time.Duration {
	if p == nil || p.httpClient == nil {
		return 0
	}
	return p.httpClient.Timeout
}

// RetryConfig returns a copy of the OA-fetch retry budget.
// Exported for the e2e wiring test so it can assert the
// production wiring threads max_attempts /
// retry_403_max_attempts rather than silently dropping
// them (which would leave the OA fetch with no retry and
// reintroduce the ~396 OA-host 403s the audit trail
// surfaced).
func (p *UnpaywallResolutionProvider) RetryConfig() RetryConfig {
	if p == nil {
		return RetryConfig{}
	}
	return p.retryCfg
}

// unpaywallResponse is the subset of the v2 response we
// need. Unpaywall returns a much richer payload (authors,
// journal, oa_locations with confidence scoring, etc.) but
// for the resolution contract we only care about the
// best_oa_location and the array of oa_locations — both
// expose the URLs the caller would follow to read the
// work. The fields are pointers so an absent best_oa_location
// is distinguishable from an explicitly-null one.
type unpaywallResponse struct {
	BestOALocation *unpaywallLocation  `json:"best_oa_location"`
	OALocations    []unpaywallLocation `json:"oa_locations"`
	// OAStatus is the top-level open-access status
	// Unpaywall assigns to the work: "green", "gold",
	// "bronze", "hybrid", or "closed". We capture it on
	// every call (including when best_oa_location is
	// null / closed access) so the worker can persist it
	// on the source row and the UI can show users why an
	// article might be incomplete.
	OAStatus string `json:"oa_status"`
	IsOA     bool   `json:"is_oa"`
}

// unpaywallLocation is the location object Unpaywall
// returns. url is the human landing page (HTML), url_for_pdf
// is the direct PDF link when the publisher exposes one,
// and the host_type / license / oa_status fields inform
// the preference order in selectOALocation. We keep the
// full set so a future improvement can rank by license
// (CC-BY > preprint) without re-parsing the response.
type unpaywallLocation struct {
	URL       string `json:"url"`
	URLForPDF string `json:"url_for_pdf"`
	HostType  string `json:"host_type"`
	License   string `json:"license"`
	OAStatus  string `json:"oa_status"`
}

// Supports reports whether the provider can act on the
// given source type. Unpaywall is DOI-only by design:
// the API path is `/v2/<doi>` and there is no URL-based
// lookup. Plain URLs go through FetchResolutionProvider.
func (p *UnpaywallResolutionProvider) Supports(sourceType SourceType) bool {
	return sourceType == SourceDOI
}

// Describe returns the static metadata used by the API's
// /sources/providers endpoint and the UI's Fetch Providers
// tab. Unpaywall is enabled by setting the contact email
// (required by Unpaywall's TOS), so Configured reflects
// whether the email is set.
func (p *UnpaywallResolutionProvider) Describe() ProviderDescription {
	return ProviderDescription{
		Name:        "Unpaywall (OA lookup for DOIs)",
		Description: "Looks up a DOI in the Unpaywall v2 API and, when the work has an open-access copy, follows the OA location's URL to fetch the body. Prefers url_for_pdf over the landing-page url so a real PDF is fetched when available; the body is parsed with the wired content parsers (Trafilatura + MuPDF) the same way the plain HTTP fetch parses. The Unpaywall API call retries once on 429/5xx (its own tight loop); the OA-location fetch retries up to max_attempts (default 3) with retry_403_max_attempts (default 2) so a transient publisher-WAF 403 or slow-host timeout gets a bounded retry before the chain falls through to TLS/FlareSolverr. The email query parameter is required by Unpaywall's terms of service; the same address is used to throttle abusive callers.",
		Requires:    "UNPAYWALL_EMAIL",
		Configured:  p.email != "",
		Supports:    []string{"doi"},
		Timeout:     p.httpClient.Timeout.String(),
		Notes:       "Returns ErrUnpaywallNotOpenAccess when the work has no OA location; the strategy treats that as a fall-through to the next provider (HTTP Fetch on the publisher landing page). Returns ErrInsufficientContent when the OA body parses to less than the minimum length so heavier tiers can run. OARedirectURL is carried on every OA-fetch error so the strategy's second pass can retry the direct OA URL with the URL-capable tiers.",
	}
}

// Resolve looks up the DOI, picks the best OA location,
// and fetches that location's body. It returns
// ErrUnpaywallNotOpenAccess when the work has no OA
// location, which the fetch strategy treats as a "try
// the next provider" signal.
//
// When the OA location is fetched the ContentType /
// FinalURL / StatusCode fields reflect the OA location
// response (not the Unpaywall API response) so the
// worker / UI see the document the user would have
// read.
func (p *UnpaywallResolutionProvider) Resolve(ctx context.Context, resource Resource) (ResolvedContent, error) {
	if resource.Type != SourceDOI {
		return ResolvedContent{}, fmt.Errorf("unpaywall: source type %q is not supported", resource.Type)
	}
	if resource.DOI == "" {
		// The classifier should have populated this for
		// a SourceDOI; the safety net is a 400-style
		// error rather than a panic.
		return ResolvedContent{}, fmt.Errorf("unpaywall: resource has no DOI")
	}

	// Unpaywall's v2 API uses the bare DOI (not the
	// doi.org URL form) as the path segment, with the
	// email as a required query parameter. The DOI's "/"
	// is a valid path character and must NOT be percent-
	// encoded: url.PathEscape encodes "/" as "%2F" and
	// url.URL.String() re-encodes "%" as "%25", producing
	// a double-encoded path that Unpaywall 404s on. We
	// append the DOI verbatim and rely on the DOI character
	// set (registrant code + suffix, both ASCII without
	// characters that are unsafe in a URL path segment
	// beyond "/") to keep the URL well-formed.
	apiURL, err := url.Parse(unpaywallAPIBase)
	if err != nil {
		return ResolvedContent{}, fmt.Errorf("unpaywall: parsing base URL: %w", err)
	}
	apiURL.Path = apiURL.Path + "/" + resource.DOI
	q := apiURL.Query()
	q.Set("email", p.email)
	apiURL.RawQuery = q.Encode()

	// Retry the API call once on 429 / 5xx. A transient
	// rate-limit should not collapse the OA path to the
	// publisher landing page; the bounded retry keeps the
	// per-provider timeout budget intact while giving
	// Unpaywall a second chance.
	var lookup unpaywallResponse
	for attempt := 0; attempt <= unpaywallRetryAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return ResolvedContent{}, fmt.Errorf("unpaywall: cancelled: %w", err)
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL.String(), nil)
		if err != nil {
			return ResolvedContent{}, fmt.Errorf("unpaywall: creating request: %w", err)
		}
		req.Header.Set("Accept", "application/json")

		resp, err := p.httpClient.Do(req)
		if err != nil {
			return ResolvedContent{}, fmt.Errorf("unpaywall: request failed: %w", err)
		}

		// 404 means Unpaywall has no record for this DOI —
		// distinct from "I know this work but it's closed
		// access" (which is 200 with best_oa_location=null).
		// Either way, the strategy should fall through to
		// the next provider, so we collapse both cases to
		// the sentinel error. We still set OAStatus="closed"
		// on the error result so the strategy can carry it
		// to the source row even when falling through.
		if resp.StatusCode == http.StatusNotFound {
			resp.Body.Close()
			return ResolvedContent{OAStatus: "closed"}, ErrUnpaywallNotOpenAccess
		}
		if resp.StatusCode == http.StatusOK {
			err = json.NewDecoder(resp.Body).Decode(&lookup)
			resp.Body.Close()
			if err != nil {
				return ResolvedContent{}, fmt.Errorf("unpaywall: decoding response: %w", err)
			}
			break
		}
		// 429 / 5xx: retry once after a short backoff if
		// we have attempts left; otherwise hard-fail.
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if attempt < unpaywallRetryAttempts && (resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500) {
			select {
			case <-ctx.Done():
				return ResolvedContent{}, fmt.Errorf("unpaywall: cancelled during retry backoff: %w", ctx.Err())
			case <-time.After(unpaywallRetryBackoff):
			}
			continue
		}
		return ResolvedContent{}, fmt.Errorf("unpaywall: returned status %d: %s", resp.StatusCode, string(b))
	}

	target := selectOALocation(lookup)
	if target == "" {
		// No OA location but we have the oa_status from
		// the API response. Carry it on the error result
		// so the strategy can persist it on the source
		// row. When oa_status is "closed" the UI can show
		// the user why the article is incomplete.
		return ResolvedContent{OAStatus: lookup.OAStatus}, ErrUnpaywallNotOpenAccess
	}

	// Track whether the selected OA URL is a direct PDF link.
	// When it is but the fetch returns HTML (not PDF), the
	// response is likely a redirect/consent/cookie page rather
	// than the actual PDF — we treat it as insufficient so the
	// strategy's second pass can retry the URL with a different
	// tier (TLS impersonation, FlareSolverr) that might get
	// the real PDF.
	isPDFURL := strings.Contains(strings.ToLower(target), ".pdf") ||
		strings.Contains(strings.ToLower(target), "/pdf/")

	// SSRF-validate the OA URL before fetching. Unpaywall
	// aggregates OA locations from many sources (repositories,
	// publisher APIs, web crawls), and the URLs are
	// user-influenceable (a hostile deposit could point at an
	// internal address). The same ValidateFetchURL the
	// strategy uses for SourceURL applies here. An unsafe URL
	// is treated as "no OA location" so the chain falls
	// through to the next provider rather than failing the
	// job — the closed-access sentinel is the right behaviour
	// for "we can't safely fetch what Unpaywall pointed us at".
	if err := ValidateFetchURL(target); err != nil {
		return ResolvedContent{OAStatus: lookup.OAStatus}, fmt.Errorf("unpaywall: OA location rejected by SSRF guard: %w", err)
	}

	// Follow the OA location URL to actually retrieve
	// the content. The body is what the worker persists
	// to okt_repository.sources; the FinalURL is what
	// gets displayed in the UI as the canonical link.
	//
	// The OA fetch is wrapped in retryWithBackoff when
	// retryCfg.MaxAttempts > 1 so a transient OA-host 403
	// (publisher WAF under burst) or a slow-host timeout
	// gets a bounded retry before the chain falls through
	// to TLS/FlareSolverr. This mirrors the fetch/tls
	// tiers. When retry is disabled (the historical
	// constructors / tests) we call doOAFetch directly to
	// keep the path simple and avoid log noise. The
	// per-provider context (set by the strategy) caps the
	// whole loop at perProviderTimeout regardless.
	//
	// retryWithBackoff returns the zero-value ResolvedContent
	// on exhaustion (it only preserves lastErr). We capture
	// the last non-zero result via a closure so the
	// OARedirectURL / OAStatus doOAFetch sets on every error
	// path survive the retry-exhaustion return — without
	// this, the strategy's second pass (the "OA URL pass")
	// would lose the direct OA URL it needs to retry with
	// the remaining URL-capable tiers.
	if p.retryCfg.MaxAttempts > 1 {
		var lastResult ResolvedContent
		result, err := retryWithBackoff(ctx, p.retryCfg, "unpaywall_oa",
			func(retryCtx context.Context) (ResolvedContent, error) {
				res, e := p.doOAFetch(retryCtx, target, isPDFURL, lookup.OAStatus)
				if e != nil {
					// Preserve the most recent error-result so
					// it can be returned on exhaustion.
					lastResult = res
				}
				return res, e
			})
		if err != nil {
			// On exhaustion retryWithBackoff returns a zero
			// ResolvedContent; substitute the last non-zero
			// error result doOAFetch produced so the strategy
			// still gets OARedirectURL + OAStatus.
			if result.OARedirectURL == "" && result.OAStatus == "" && lastResult.OARedirectURL != "" {
				result = lastResult
			}
			return result, err
		}
		return result, nil
	}
	return p.doOAFetch(ctx, target, isPDFURL, lookup.OAStatus)
}

// doOAFetch performs a single HTTP GET against the OA
// location URL, applies the body-size cap, and parses the
// body with the wired parsers. It is separated from Resolve
// so retryWithBackoff can call it multiple times without
// duplicating the SSRF-validation or OA-location-selection
// steps (which are not retryable — a different OA URL would
// require a fresh Unpaywall API lookup, which is outside
// this loop's scope).
//
// oaStatus is threaded onto every returned ResolvedContent
// (success and error) so the strategy can persist it on the
// source row regardless of which tier eventually wins.
// OARedirectURL is set on every error path so the strategy's
// second pass (strategy.go Resolve, the "OA URL pass") can
// retry the direct OA URL with the remaining URL-capable
// tiers (TLS, fetch, FlareSolverr) when this tier exhausts
// its retry budget — a different TLS fingerprint might get
// through where this plain HTTP client got 403.
//
// Non-2xx responses are wrapped in ErrNon2xxStatus so
// retryableFetchError (retry.go) classifies them: 403 is
// retryable up to retry_403_max_attempts, 429/5xx are
// retryable up to max_attempts, other 4xx are permanent.
// Network and timeout errors are classified by retryableFetchError's
// net.Error / heuristic paths. ErrInsufficientContent and
// ErrBodyTooLarge are non-retryable sentinels (retry.go).
func (p *UnpaywallResolutionProvider) doOAFetch(
	ctx context.Context,
	target string,
	isPDFURL bool,
	oaStatus string,
) (ResolvedContent, error) {
	contentReq, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return ResolvedContent{OAStatus: oaStatus, OARedirectURL: target}, fmt.Errorf("unpaywall: creating content request: %w", err)
	}
	// Browser-like headers on the OA-location fetch too.
	// Some OA hosts (PMC, arXiv) are fine without a UA,
	// but a non-trivial subset behaves differently for
	// empty User-Agents. Reusing the default UA costs
	// nothing and avoids a class of silent 403s.
	// Accept-Encoding is intentionally not set so Go's
	// transport auto-decompresses gzip (see
	// FetchResolutionProvider.setBrowserHeaders for the
	// full rationale).
	contentReq.Header.Set("User-Agent", p.userAgent)
	contentReq.Header.Set("Accept", "text/html,application/xhtml+xml,application/pdf;q=0.9,text/plain;q=0.8,*/*;q=0.5")
	contentReq.Header.Set("Accept-Language", "en-US,en;q=0.9")

	contentResp, err := p.httpClient.Do(contentReq)
	if err != nil {
		return ResolvedContent{OAStatus: oaStatus, OARedirectURL: target}, fmt.Errorf("unpaywall: fetching OA location failed: %w", err)
	}
	defer contentResp.Body.Close()

	// Non-2xx on the OA location is an error (not the
	// sentinel ErrUnpaywallNotOpenAccess): the OA location
	// Unpaywall pointed us at is broken or blocked, and the
	// strategy should fall through to the next provider
	// (plain fetch on the publisher landing page) rather
	// than pretend the broken OA copy is the content.
	//
	// Wrapped in ErrNon2xxStatus so retryableFetchError
	// classifies 403/429/5xx as retryable. The audit trail
	// error message keeps the original "OA location
	// returned status N" phrasing via the wrapped message.
	if contentResp.StatusCode < 200 || contentResp.StatusCode >= 300 {
		return ResolvedContent{
				StatusCode:    contentResp.StatusCode,
				FinalURL:      contentResp.Request.URL.String(),
				ContentType:   contentResp.Header.Get("Content-Type"),
				OAStatus:      oaStatus,
				OARedirectURL: target,
			}, fmt.Errorf("unpaywall: OA location returned status %d: %w",
				contentResp.StatusCode, &ErrNon2xxStatus{Code: contentResp.StatusCode})
	}

	// Cap the body read the same way the plain fetch does.
	if contentResp.ContentLength > MaxBodyBytes {
		return ResolvedContent{StatusCode: contentResp.StatusCode, FinalURL: contentResp.Request.URL.String(), OAStatus: oaStatus, OARedirectURL: target}, ErrBodyTooLarge
	}
	body, err := io.ReadAll(io.LimitReader(contentResp.Body, MaxBodyBytes+1))
	if err != nil {
		return ResolvedContent{OAStatus: oaStatus, OARedirectURL: target}, fmt.Errorf("unpaywall: reading OA body: %w", err)
	}
	if int64(len(body)) > MaxBodyBytes {
		return ResolvedContent{StatusCode: contentResp.StatusCode, FinalURL: contentResp.Request.URL.String(), OAStatus: oaStatus, OARedirectURL: target}, ErrBodyTooLarge
	}

	contentType := contentResp.Header.Get("Content-Type")
	finalURL := contentResp.Request.URL.String()
	resolved := ResolvedContent{
		Body:        body,
		ContentType: contentType,
		StatusCode:  contentResp.StatusCode,
		FinalURL:    finalURL,
		OAStatus:    oaStatus,
	}

	// When the OA URL was a direct PDF link but the response
	// is HTML (not PDF), the server redirected to a
	// consent/cookie/landing page instead of serving the
	// actual PDF. Set OARedirectURL and return
	// ErrInsufficientContent so the strategy's second pass
	// retries the direct PDF URL with the remaining tiers
	// (TLS impersonation, FlareSolverr) which might handle
	// the redirect differently.
	if isPDFURL && !isPDFContentType(contentType) {
		resolved.OARedirectURL = target
		return resolved, ErrInsufficientContent
	}

	// Parse the OA body the same way FetchResolutionProvider
	// does. This closes the previous gap where OA HTML
	// fetched via Unpaywall was never run through
	// Trafilatura and the row ended up with
	// parse_status='unsupported'.
	if len(p.parsers) > 0 && len(body) > 0 {
		sourceType, ok := detectOASourceType(contentType, body)
		if ok {
			if parser := pickParser(p.parsers, sourceType); parser != nil {
				parsed, parseErr := parser.Parse(ctx, body, sourceType, finalURL)
				if parseErr == nil {
					resolved.Parsed = parsed
					text := strings.TrimSpace(parsed.Text)
					if len(text) < MinExtractedLength || IsJSBoilerplate(text) || IsHTMLLeakBoilerplate(text) {
						return resolved, ErrInsufficientContent
					}
				} else {
					resolved.Parsed = content_parsing.ParsedDoc{}
					_ = parseErr
				}
			}
		}
	}

	return resolved, nil
}

// isPDFContentType returns true when the Content-Type header
// indicates a PDF response. Used to detect when a direct PDF
// URL returned HTML instead (a redirect to a consent/cookie
// page rather than the actual PDF).
func isPDFContentType(ct string) bool {
	return strings.Contains(strings.ToLower(ct), "application/pdf")
}

// selectOALocation picks the URL the resolver should
// follow. The preference order is:
//
//  1. best_oa_location.url_for_pdf — the direct PDF link,
//     when Unpaywall's top-ranked OA location exposes one.
//     We now prefer the PDF over the landing-page URL
//     because the wired parser set includes a PDF parser
//     (MuPDF), so a PDF response is fully extractable
//     instead of becoming a 32KB binary preview. This
//     matches the reference Python implementation's
//     preference (doi_enricher._fetch_unpaywall_oa).
//  2. best_oa_location.url — Unpaywall's own ranking when
//     no direct PDF link is available (e.g. a repository
//     that only exposes an HTML landing page).
//  3. The first oa_locations entry's url_for_pdf — when
//     best_oa_location is null but other locations exist
//     with PDF links.
//  4. The first oa_locations entry's url — last-resort
//     fallback when the work only exposes landing-page
//     URLs without a clean PDF link.
//
// The function is a pure helper so the unit tests can
// exercise the preference logic without hitting the
// network.
func selectOALocation(r unpaywallResponse) string {
	if r.BestOALocation != nil {
		if u := strings.TrimSpace(r.BestOALocation.URLForPDF); u != "" {
			return u
		}
		if u := strings.TrimSpace(r.BestOALocation.URL); u != "" {
			return u
		}
	}
	for _, loc := range r.OALocations {
		if u := strings.TrimSpace(loc.URLForPDF); u != "" {
			return u
		}
	}
	for _, loc := range r.OALocations {
		if u := strings.TrimSpace(loc.URL); u != "" {
			return u
		}
	}
	return ""
}
