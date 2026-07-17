package fetch

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/openktree/open-knowledge-tree/backend/internal/providers/content_parsing"
)

const defaultUserAgent = "Mozilla/5.0 (compatible; OpenKT/1.0; +https://github.com/openktree/open-knowledge-tree)"

// ErrNon2xxStatus is returned by Resolve when the upstream
// returns a non-2xx HTTP status. It wraps the status code so
// the strategy's audit trail records the real reason (403,
// 404, 500, ...) and the chain falls through to the next
// provider instead of short-circuiting on the paywalled /
// blocked body. Callers that want the original status code
// can type-assert or inspect the message.
type ErrNon2xxStatus struct {
	Code int
}

func (e *ErrNon2xxStatus) Error() string {
	return fmt.Sprintf("upstream returned status %d", e.Code)
}

// FetchResolutionProvider is the catch-all HTTP resolver.
// It performs a plain GET, follows redirects, and — when
// configured with one or more content_parsing.Parser
// implementations — also extracts the main readable
// content from the response body so the caller does not
// have to deal with raw HTML or rasterized PDF pages.
//
// The parser is an interface, not a concrete type, so tests
// can inject a stub and the strategy can swap the default
// (Trafilatura) for another implementation (a PDF parser
// for non-HTML responses, a future readability-only
// parser, etc.) without touching this file.
type FetchResolutionProvider struct {
	httpClient *http.Client
	userAgent  string
	parsers    []content_parsing.Parser
	retryCfg   RetryConfig // zero-value = no retry (MaxAttempts=1)
}

// NewFetchResolutionProvider returns a resolver wired with
// the default parser set: Trafilatura for HTML. Existing
// call sites that don't care about the parser keep
// working unchanged. Add the PDF parser through
// NewFetchResolutionProviderWithParsers when PDF inputs
// are expected.
func NewFetchResolutionProvider() *FetchResolutionProvider {
	return NewFetchResolutionProviderWithParsers(content_parsing.NewTrafilaturaParser())
}

// NewFetchResolutionProviderWithConfig is the historical
// constructor that lets callers override the User-Agent.
// It is kept for backwards compatibility and now also
// wires the default parser. Use
// NewFetchResolutionProviderWithParser when you need to
// inject a non-default parser.
func NewFetchResolutionProviderWithConfig(userAgent string) *FetchResolutionProvider {
	return NewFetchResolutionProviderWithParsersAndUserAgent(
		[]content_parsing.Parser{content_parsing.NewTrafilaturaParser()}, userAgent,
	)
}

// NewFetchResolutionProviderWithParser lets callers inject
// a single custom content_parsing.Parser. The parser is
// invoked in Resolve on every response whose Content-Type
// is understood by the parser. Use
// NewFetchResolutionProviderWithParsers to wire multiple
// parsers (e.g. Trafilatura + PDF).
func NewFetchResolutionProviderWithParser(parser content_parsing.Parser) *FetchResolutionProvider {
	return NewFetchResolutionProviderWithParsers(parser)
}

// NewFetchResolutionProviderWithParsers wires one or more
// parsers. The resolver picks the first parser that
// Supports the response's source type. Nil entries are
// skipped. An empty list falls back to the default
// (Trafilatura) so a misconfigured call site still works.
func NewFetchResolutionProviderWithParsers(parsers ...content_parsing.Parser) *FetchResolutionProvider {
	return NewFetchResolutionProviderWithParsersAndUserAgent(parsers, defaultUserAgent)
}

// NewFetchResolutionProviderWithFullConfig is the fully-configurable
// constructor. It accepts an explicit per-request timeout, a retry
// config (pass NoRetryConfig to disable retry), a User-Agent string,
// and the parser set. A nil parser or empty User-Agent falls back to
// the defaults. Pass zero-value RetryConfig to use defaultRetryConfig
// (3 attempts, 2s base, 15s cap). Pass NoRetryConfig to disable retry.
func NewFetchResolutionProviderWithFullConfig(
	timeout time.Duration,
	retCfg RetryConfig,
	userAgent string,
	parsers ...content_parsing.Parser,
) *FetchResolutionProvider {
	cleaned := make([]content_parsing.Parser, 0, len(parsers))
	for _, p := range parsers {
		if p != nil {
			cleaned = append(cleaned, p)
		}
	}
	if len(cleaned) == 0 {
		cleaned = append(cleaned, content_parsing.NewTrafilaturaParser())
	}
	ua := userAgent
	if ua == "" {
		ua = defaultUserAgent
	}
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	// Normalise the retry config so the Resolve method can check
	// retCfg.MaxAttempts == 1 to skip the retry wrapper.
	if retCfg.MaxAttempts <= 0 {
		if retCfg.BaseDelay == 0 && retCfg.MaxDelay == 0 {
			// Completely zero => caller wants defaults.
			retCfg = defaultRetryConfig
		} else {
			// Partially set but MaxAttempts is zero => no retry.
			retCfg.MaxAttempts = 1
		}
	}
	return &FetchResolutionProvider{
		httpClient: &http.Client{
			Timeout: timeout,
		},
		userAgent: ua,
		parsers:   cleaned,
		retryCfg:  retCfg,
	}
}

// NewFetchResolutionProviderWithParsersAndUserAgent is the
// historical fully-configured constructor. It uses a 30s
// per-request timeout and disables retry, preserving the
// existing behaviour for callers that don't need retry
// (mostly tests). Use NewFetchResolutionProviderWithFullConfig
// for production wiring.
func NewFetchResolutionProviderWithParsersAndUserAgent(parsers []content_parsing.Parser, userAgent string) *FetchResolutionProvider {
	return NewFetchResolutionProviderWithFullConfig(30*time.Second, NoRetryConfig, userAgent, parsers...)
}

func (p *FetchResolutionProvider) Resolve(ctx context.Context, resource Resource) (ResolvedContent, error) {
	fetchURL, err := p.resolveURL(resource)
	if err != nil {
		return ResolvedContent{}, err
	}

	// When retry is enabled, wrap the fetch+parse in retryWithBackoff
	// so transient network errors and 5xx/429 status codes are retried
	// before falling through to the next provider. When retry is
	// disabled (MaxAttempts == 1) we call the inner function directly
	// to keep the path simple and avoid log noise.
	if p.retryCfg.MaxAttempts > 1 {
		return retryWithBackoff(ctx, p.retryCfg, "http_fetch",
			func(retryCtx context.Context) (ResolvedContent, error) {
				return p.doFetchAndParse(retryCtx, fetchURL)
			})
	}
	return p.doFetchAndParse(ctx, fetchURL)
}

// doFetchAndParse performs a single HTTP GET against fetchURL and
// parses the response. This is separated from Resolve so the retry
// wrapper can call it multiple times without duplicating the URL
// resolution step.
func (p *FetchResolutionProvider) doFetchAndParse(ctx context.Context, fetchURL string) (ResolvedContent, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fetchURL, nil)
	if err != nil {
		return ResolvedContent{}, fmt.Errorf("creating fetch request: %w", err)
	}

	// Full browser-style header suite. A bare User-Agent
	// passes naive bot-checks but fails WAFs that inspect
	// the whole Sec-Fetch-* set; sending the full suite
	// costs nothing and defeats the cheapest tier of
	// anti-bot heuristics (mirrors the reference's
	// httpx_provider._browser_headers).
	p.setBrowserHeaders(req)

	resp, err := p.httpClient.Do(req)
	if err != nil {
		return ResolvedContent{}, fmt.Errorf("fetch request failed: %w", err)
	}
	defer resp.Body.Close()

	// Non-2xx is an error so the strategy falls through to
	// the next provider instead of short-circuiting on a
	// 403/404/500 body. The previous behaviour returned a
	// nil error for any HTTP status, which meant a paywalled
	// publisher page became the "successful" result and no
	// fallback ran. The audit trail records the status code
	// in the error message.
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return ResolvedContent{
			StatusCode:  resp.StatusCode,
			FinalURL:    resp.Request.URL.String(),
			ContentType: resp.Header.Get("Content-Type"),
		}, &ErrNon2xxStatus{Code: resp.StatusCode}
	}

	// Bound the read so a multi-megabyte response can't
	// blow worker memory. FetchImageBytes already caps; the
	// source path now does too. One byte above the cap lets
	// us detect overflow without a separate Content-Length
	// path.
	if resp.ContentLength > MaxBodyBytes {
		return ResolvedContent{StatusCode: resp.StatusCode, FinalURL: resp.Request.URL.String()}, ErrBodyTooLarge
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, MaxBodyBytes+1))
	if err != nil {
		return ResolvedContent{}, fmt.Errorf("reading response body: %w", err)
	}
	if int64(len(body)) > MaxBodyBytes {
		return ResolvedContent{StatusCode: resp.StatusCode, FinalURL: resp.Request.URL.String()}, ErrBodyTooLarge
	}

	finalURL := resp.Request.URL.String()
	contentType := resp.Header.Get("Content-Type")

	resolved := ResolvedContent{
		Body:        body,
		ContentType: contentType,
		StatusCode:  resp.StatusCode,
		FinalURL:    finalURL,
	}

	// Parse the body when we have at least one parser and
	// the Content-Type is one any of them understands. On a
	// successful parse we apply the insufficient-content
	// guard: when the extracted Text is shorter than
	// MinExtractedLength the fetch is treated as a
	// fall-through (ErrInsufficientContent) so heavier tiers
	// (TLS impersonation, headless browser) get a chance.
	// This is what defeats "Please enable JavaScript" pages
	// whose <noscript> fallback trafilatura extracts as
	// near-empty text.
	if len(p.parsers) > 0 && len(body) > 0 {
		sourceType, ok := p.detectSourceType(contentType, body)
		if ok {
			if parser := p.pickParser(sourceType); parser != nil {
				parsed, parseErr := parser.Parse(ctx, body, sourceType, finalURL)
				if parseErr == nil {
					resolved.Parsed = parsed
					text := strings.TrimSpace(parsed.Text)
					if len(text) < MinExtractedLength || IsJSBoilerplate(text) || IsHTMLLeakBoilerplate(text) {
						return resolved, ErrInsufficientContent
					}
				} else {
					// Parse failure does not fail the
					// resolution — the raw body is
					// still useful. The error is
					// swallowed; future iterations
					// can add a structured error
					// channel here.
					resolved.Parsed = content_parsing.ParsedDoc{}
					_ = parseErr
				}
			}
		}
	}

	return resolved, nil
}

// setBrowserHeaders applies the full Sec-Fetch-* suite plus
// a browser-like Accept / Accept-Language /
// Upgrade-Insecure-Requests set. Cheap defence against
// WAFs that inspect more than the User-Agent.
//
// Accept-Encoding is intentionally NOT set: Go's default
// http.Transport auto-sets "Accept-Encoding: gzip" and
// transparently decompresses gzip responses when the caller
// does not set the header manually. Setting it ourselves
// disables that auto-decompression, so brotli/gzip responses
// would be stored as raw compressed bytes — the root cause
// of the "garbled content" bug where parsed_text was binary
// noise. By leaving the header off we get gzip for free and
// avoid advertising brotli (which Go can't auto-decompress).
func (p *FetchResolutionProvider) setBrowserHeaders(req *http.Request) {
	req.Header.Set("User-Agent", p.userAgent)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,image/apng,*/*;q=0.8")
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")
	req.Header.Set("Sec-Fetch-Dest", "document")
	req.Header.Set("Sec-Fetch-Mode", "navigate")
	req.Header.Set("Sec-Fetch-Site", "none")
	req.Header.Set("Sec-Fetch-User", "?1")
	req.Header.Set("Upgrade-Insecure-Requests", "1")
}

// pickParser returns the first parser that Supports the
// given source type. Order matters: pass the most specific
// parser first. The default ctor wires Trafilatura only,
// so HTML is always handled; add NewFitzPDFParser to
// handle PDF responses. Delegates to the package-level
// pickParser so the Unpaywall provider reuses the same
// selection logic.
func (p *FetchResolutionProvider) pickParser(sourceType content_parsing.SourceType) content_parsing.Parser {
	return pickParser(p.parsers, sourceType)
}

func (p *FetchResolutionProvider) Supports(sourceType SourceType) bool {
	return sourceType == SourceURL || sourceType == SourceDOI
}

// Describe returns the static metadata used by the API's
// /sources/providers endpoint and the UI's Fetch Providers
// tab. The plain HTTP fetch provider is always available (the
// UserAgent is configured at construction time, with a default
// in place when none is set), so Configured is true.
func (p *FetchResolutionProvider) Describe() ProviderDescription {
	return ProviderDescription{
		Name:        "HTTP Fetch",
		Description: "Generic HTTP GET used as the catch-all resolver. Sends a full browser-style header suite (Sec-Fetch-*, Upgrade-Insecure-Requests) and a browser-like User-Agent, follows the URL directly for http(s) links, and rewrites bare DOIs to https://doi.org/ before fetching. Responses are parsed with the wired content parsers (Trafilatura for HTML, MuPDF for PDF); when the extracted text is below the minimum length the resolver returns ErrInsufficientContent so the strategy falls through to a heavier tier. Non-2xx responses are treated as errors so a 403/404 paywalled page does not short-circuit the chain.",
		Requires:    "",
		Configured:  true,
		Supports:    []string{"url", "doi"},
		Timeout:     p.httpClient.Timeout.String(),
		Notes:       "Runs after Unpaywall so OA copies are preferred over the publisher landing page. Non-2xx and insufficient-content outcomes fall through to the next provider.",
	}
}

func (p *FetchResolutionProvider) resolveURL(resource Resource) (string, error) {
	switch resource.Type {
	case SourceURL:
		return resource.Value, nil
	case SourceDOI:
		return "https://doi.org/" + resource.Value, nil
	default:
		return "", fmt.Errorf("unsupported source type: %s", resource.Type)
	}
}

// detectSourceType maps a Content-Type header (and the body,
// as a fallback for servers that omit the header or send
// text/plain) to a content_parsing.SourceType. The bool
// return is false when the type is not understood, in which
// case the parser is skipped and ResolvedContent.Parsed
// stays empty. Delegates to the package-level detectOASourceType
// so the Unpaywall provider reuses the same detection logic.
func (p *FetchResolutionProvider) detectSourceType(contentType string, body []byte) (content_parsing.SourceType, bool) {
	return detectOASourceType(contentType, body)
}