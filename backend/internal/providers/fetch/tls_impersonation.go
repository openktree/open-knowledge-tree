package fetch

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	fhttp "github.com/bogdanfinn/fhttp"
	tls_client "github.com/bogdanfinn/tls-client"
	"github.com/bogdanfinn/tls-client/profiles"
	"github.com/openktree/open-knowledge-tree/backend/internal/providers/content_parsing"
)

// TLSImpersonationProvider implements ResolutionProvider using
// a TLS-impersonating HTTP client (bogdanfinn/tls-client). It
// defeats the cheapest tier of anti-bot WAFs — Cloudflare,
// Datadome, PerimeterX — that block based on the TLS/JA3
// fingerprint of Go's default crypto/tls handshake. By
// impersonating Chrome's BoringSSL fingerprint the provider
// gets through WAFs that the plain FetchResolutionProvider
// (with its openssl-style handshake) cannot, at zero infra
// cost (no sidecar, pure Go).
//
// The provider mirrors the reference Python implementation's
// curl_cffi tier: it runs after Unpaywall (so OA copies are
// preferred) and before the plain HTTP fetch (so a
// fingerprint-blocked publisher page is retried with a
// browser-shaped handshake before falling through to the
// catch-all). It shares the parser set with the plain fetch
// so the body is parsed (Trafilatura for HTML, MuPDF for PDF)
// and the same insufficient-content guard applies.
//
// The provider self-disables when Impersonate is empty: the
// constructor returns nil and the wiring layer skips
// registration. This mirrors the Unpaywall email convention
// and keeps the chain order stable across configurations.
type TLSImpersonationProvider struct {
	impersonate string
	userAgent   string
	client      tls_client.HttpClient
	parsers     []content_parsing.Parser
	timeout     time.Duration // stored for Describe(); set via WithTimeoutSeconds
	retryCfg    RetryConfig   // zero-value = no retry (MaxAttempts=1)
}

// NewTLSImpersonationProviderWithFullConfig builds the provider
// with explicit timeout and retry config. When impersonate is
// empty, the constructor returns nil. Pass NoRetryConfig to
// disable retry. A zero timeout defaults to 30s.
//
// The timeout is the per-request budget for the TLS-impersonating
// client. The retry config governs transient-failure retries
// before the provider returns an error to the strategy chain.
func NewTLSImpersonationProviderWithFullConfig(
	impersonate, userAgent string,
	timeout time.Duration,
	retCfg RetryConfig,
	parsers ...content_parsing.Parser,
) *TLSImpersonationProvider {
	impersonate = strings.TrimSpace(impersonate)
	if impersonate == "" {
		return nil
	}
	profile, ok := resolveTLSProfile(impersonate)
	if !ok {
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
	ua := userAgent
	if ua == "" {
		ua = defaultUserAgent
	}
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	// Normalise retry config.
	if retCfg.MaxAttempts <= 0 {
		if retCfg.BaseDelay == 0 && retCfg.MaxDelay == 0 {
			retCfg = defaultRetryConfig
		} else {
			retCfg.MaxAttempts = 1
		}
	}
	secs := int(timeout.Seconds())
	if secs < 1 {
		secs = 30
	}
	client, err := tls_client.NewHttpClient(tls_client.NewNoopLogger(), []tls_client.HttpClientOption{
		tls_client.WithTimeoutSeconds(secs),
		tls_client.WithClientProfile(profile),
	}...)
	if err != nil {
		return nil
	}
	return &TLSImpersonationProvider{
		impersonate: impersonate,
		userAgent:   ua,
		client:      client,
		parsers:     cleaned,
		timeout:     timeout,
		retryCfg:    retCfg,
	}
}

// NewTLSImpersonationProvider builds the provider using the
// historical defaults: 30s per-request timeout and no retry.
// When impersonate is empty, the constructor returns nil.
// Use NewTLSImpersonationProviderWithFullConfig for production
// wiring where retry is desired.
func NewTLSImpersonationProvider(impersonate, userAgent string, parsers ...content_parsing.Parser) *TLSImpersonationProvider {
	return NewTLSImpersonationProviderWithFullConfig(impersonate, userAgent, 30*time.Second, NoRetryConfig, parsers...)
}

// resolveTLSProfile maps a human-friendly identifier (e.g.
// "chrome_124") to a tls-client profile. The mapping is
// intentionally narrow: we only expose the latest Chrome
// profiles, which is what the reference's curl_cffi tier
// defaults to. An operator who needs a different browser
// can extend this switch; an unknown identifier returns
// ok=false and the constructor self-disables.
func resolveTLSProfile(name string) (profiles.ClientProfile, bool) {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "chrome_124", "chrome124":
		return profiles.Chrome_124, true
	case "chrome_131", "chrome131":
		return profiles.Chrome_131, true
	case "chrome_133", "chrome133":
		return profiles.Chrome_133, true
	case "firefox_133", "firefox133":
		return profiles.Firefox_133, true
	default:
		return profiles.ClientProfile{}, false
	}
}

func (p *TLSImpersonationProvider) Supports(sourceType SourceType) bool {
	return sourceType == SourceURL || sourceType == SourceDOI
}

func (p *TLSImpersonationProvider) Describe() ProviderDescription {
	timeoutStr := "30s"
	if p.timeout > 0 {
		timeoutStr = p.timeout.String()
	}
	return ProviderDescription{
		Name:        "TLS Impersonation (Chrome fingerprint)",
		Description: "HTTP GET via a TLS-impersonating client (bogdanfinn/tls-client) that mimics Chrome's BoringSSL JA3 fingerprint. Defeats WAFs that block Go's default crypto/tls handshake (Cloudflare, Datadome, PerimeterX) at zero infra cost. Shares the parser set with the plain HTTP fetch so the body is parsed the same way.",
		Requires:    "OKT_FETCH_IMPERSONATE",
		Configured:  p.impersonate != "",
		Supports:    []string{"url", "doi"},
		Timeout:     timeoutStr,
		Notes:       "Self-disables when OKT_FETCH_IMPERSONATE is empty. Runs after Unpaywall and before the plain HTTP fetch so a fingerprint-blocked publisher page is retried with a browser-shaped handshake.",
	}
}

func (p *TLSImpersonationProvider) Resolve(ctx context.Context, resource Resource) (ResolvedContent, error) {
	fetchURL, err := resolveTLSURL(resource)
	if err != nil {
		return ResolvedContent{}, err
	}

	if p.retryCfg.MaxAttempts > 1 {
		return retryWithBackoff(ctx, p.retryCfg, "tls_impersonation",
			func(retryCtx context.Context) (ResolvedContent, error) {
				return p.doResolveTLS(retryCtx, fetchURL)
			})
	}
	return p.doResolveTLS(ctx, fetchURL)
}

// doResolveTLS performs a single TLS-impersonated HTTP GET and
// parses the response. Separated from Resolve so the retry wrapper
// can call it multiple times.
func (p *TLSImpersonationProvider) doResolveTLS(ctx context.Context, fetchURL string) (ResolvedContent, error) {
	req, err := fhttp.NewRequestWithContext(ctx, fhttp.MethodGet, fetchURL, nil)
	if err != nil {
		return ResolvedContent{}, fmt.Errorf("tls: creating request: %w", err)
	}
	p.setBrowserHeaders(req)

	resp, err := p.client.Do(req)
	if err != nil {
		return ResolvedContent{}, fmt.Errorf("tls: request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return ResolvedContent{
			StatusCode:  resp.StatusCode,
			FinalURL:    resp.Request.URL.String(),
			ContentType: resp.Header.Get("Content-Type"),
		}, &ErrNon2xxStatus{Code: resp.StatusCode}
	}

	if resp.ContentLength > MaxBodyBytes {
		return ResolvedContent{StatusCode: resp.StatusCode, FinalURL: resp.Request.URL.String()}, ErrBodyTooLarge
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, MaxBodyBytes+1))
	if err != nil {
		return ResolvedContent{}, fmt.Errorf("tls: reading body: %w", err)
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

	if len(p.parsers) > 0 && len(body) > 0 {
		sourceType, ok := detectOASourceType(contentType, body)
		if ok {
			if parser := pickParser(p.parsers, sourceType); parser != nil {
				parsed, parseErr := parser.Parse(ctx, body, sourceType, finalURL)
				if parseErr == nil {
					resolved.Parsed = parsed
					text := strings.TrimSpace(parsed.Text)
					if len(text) < MinExtractedLength || IsJSBoilerplate(text) {
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

func (p *TLSImpersonationProvider) setBrowserHeaders(req *fhttp.Request) {
	req.Header.Set("User-Agent", p.userAgent)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,image/apng,*/*;q=0.8")
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")
	req.Header.Set("Sec-Fetch-Dest", "document")
	req.Header.Set("Sec-Fetch-Mode", "navigate")
	req.Header.Set("Sec-Fetch-Site", "none")
	req.Header.Set("Sec-Fetch-User", "?1")
	req.Header.Set("Upgrade-Insecure-Requests", "1")
}

func resolveTLSURL(resource Resource) (string, error) {
	switch resource.Type {
	case SourceURL:
		return resource.Value, nil
	case SourceDOI:
		return "https://doi.org/" + resource.Value, nil
	default:
		return "", fmt.Errorf("tls: unsupported source type: %s", resource.Type)
	}
}