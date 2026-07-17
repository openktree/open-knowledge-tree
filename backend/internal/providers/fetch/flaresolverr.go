package fetch

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"mime"
	"net/http"
	"strings"
	"time"

	"github.com/openktree/open-knowledge-tree/backend/internal/providers/content_parsing"
)

// FlareSolverrProvider implements ResolutionProvider against
// a FlareSolverr / Byparr headless-browser sidecar. It is
// the heaviest tier in the chain, reserved for pages that
// cheaper tiers cannot retrieve: JavaScript-rendered pages,
// Cloudflare "Checking your browser" challenges, and other
// bot-protection schemes that require a real browser to
// solve. The sidecar runs an undetected-Chromium instance
// and exposes a simple JSON-over-HTTP protocol.
//
// The provider self-disables when URL is empty: the
// constructor returns nil and the wiring layer skips
// registration. It runs last in the chain so the cheaper
// tiers get a chance first; the host-preference store will
// learn to skip them for hosts where FlareSolverr is the
// only working tier, avoiding repeated failed cheap-tier
// attempts before reaching the heavy tier.
//
// The FlareSolverr protocol returns HTML text only — it
// cannot return raw PDF/image bytes (the headless browser
// decodes everything to text). When the response content
// type is PDF or image, the provider returns an error so
// the chain falls through to a byte-capable tier. This
// matches the reference Python implementation's
// flaresolverr_provider.py behaviour.
type FlareSolverrProvider struct {
	endpoint  string
	timeout   time.Duration
	userAgent string
	parsers   []content_parsing.Parser
	client    *http.Client
}

// NewFlareSolverrProvider builds the provider. When endpoint
// is empty, the constructor returns nil so the caller can use
// a single conditional. The endpoint is the FlareSolverr /
// Byparr HTTP URL (e.g. "http://flaresolverr:8191"). The
// timeout is the per-request budget for the headless
// browser; it defaults to 60s when zero. The parser set is
// used to parse the HTML the sidecar returns and to apply
// the same insufficient-content guard as the other tiers.
func NewFlareSolverrProvider(endpoint string, timeout time.Duration, userAgent string, parsers ...content_parsing.Parser) *FlareSolverrProvider {
	endpoint = strings.TrimSpace(endpoint)
	if endpoint == "" {
		return nil
	}
	// The FlareSolverr / Byparr protocol lives at /v1. The
	// operator-supplied URL is the base (e.g.
	// "http://flaresolverr:8191"); we append the path so the
	// POST lands on the right handler. A root POST returns
	// 405 (Method Not Allowed) which is the symptom of
	// forgetting this.
	if !strings.HasSuffix(endpoint, "/v1") {
		endpoint = strings.TrimRight(endpoint, "/") + "/v1"
	}
	if timeout <= 0 {
		timeout = 60 * time.Second
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
	return &FlareSolverrProvider{
		endpoint:  endpoint,
		timeout:   timeout,
		userAgent: ua,
		parsers:   cleaned,
		client: &http.Client{
			Timeout: timeout,
		},
	}
}

func (p *FlareSolverrProvider) Supports(sourceType SourceType) bool {
	return sourceType == SourceURL || sourceType == SourceDOI
}

func (p *FlareSolverrProvider) Describe() ProviderDescription {
	return ProviderDescription{
		Name:        "FlareSolverr (headless Chromium)",
		Description: "Drives a FlareSolverr / Byparr headless-browser sidecar to solve JavaScript challenges (Cloudflare, Datadome, PerimeterX) that no amount of TLS fingerprinting or header spoofing can bypass. Returns HTML text only; PDF/image responses fall through to a byte-capable tier. The heaviest tier in the chain — config-gated and last-resort.",
		Requires:    "FLARESOLVERR_URL",
		Configured:  p.endpoint != "",
		Supports:    []string{"url", "doi"},
		Timeout:     p.timeout.String(),
		Notes:       "Self-disables when FLARESOLVERR_URL is empty. Runs last in the chain so cheaper tiers get a chance first. Static host_overrides can pin a known-bad host to this tier in YAML.",
	}
}

// flaresolverrRequest is the JSON body sent to the sidecar's
// /v1 endpoint. The cmd "request.get" asks the browser to
// navigate to the URL and return the rendered HTML.
type flaresolverrRequest struct {
	Cmd        string `json:"cmd"`
	URL        string `json:"url"`
	MaxTimeout int    `json:"maxTimeout"`
}

// flaresolverrResponse is the subset of the sidecar's
// response we need. The solution.response field carries the
// rendered HTML; solution.headers carries the content-type
// the browser observed.
type flaresolverrResponse struct {
	Status   string `json:"status"`
	Message  string `json:"message"`
	Solution struct {
		Response string            `json:"response"`
		Headers  map[string]string `json:"headers"`
		URL      string            `json:"url"`
	} `json:"solution"`
}

func (p *FlareSolverrProvider) Resolve(ctx context.Context, resource Resource) (ResolvedContent, error) {
	fetchURL, err := resolveFlareURL(resource)
	if err != nil {
		return ResolvedContent{}, err
	}

	// Build the FlareSolverr request body. maxTimeout is in
	// milliseconds and is the budget the headless browser
	// gets to navigate + solve any challenge + render.
	body, err := json.Marshal(flaresolverrRequest{
		Cmd:        "request.get",
		URL:        fetchURL,
		MaxTimeout: int(p.timeout.Milliseconds()),
	})
	if err != nil {
		return ResolvedContent{}, fmt.Errorf("flaresolverr: marshalling request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.endpoint, bytes.NewReader(body))
	if err != nil {
		return ResolvedContent{}, fmt.Errorf("flaresolverr: creating request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := p.client.Do(req)
	if err != nil {
		return ResolvedContent{}, fmt.Errorf("flaresolverr: request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return ResolvedContent{}, fmt.Errorf("flaresolverr: sidecar returned status %d", resp.StatusCode)
	}

	var out flaresolverrResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return ResolvedContent{}, fmt.Errorf("flaresolverr: decoding response: %w", err)
	}
	if out.Status != "ok" {
		return ResolvedContent{}, fmt.Errorf("flaresolverr: sidecar reported %q: %s", out.Status, out.Message)
	}

	htmlBody := []byte(out.Solution.Response)
	if len(htmlBody) == 0 {
		return ResolvedContent{}, ErrInsufficientContent
	}

	contentType := out.Solution.Headers["content-type"]
	if contentType == "" {
		contentType = "text/html"
	}
	// FlareSolverr cannot return raw PDF/image bytes; if the
	// content type is not text/html, fall through to a
	// byte-capable tier.
	mt, _, _ := mime.ParseMediaType(contentType)
	if mt != "text/html" && mt != "application/xhtml+xml" {
		return ResolvedContent{}, fmt.Errorf("flaresolverr: non-HTML content type %q; falling through to byte-capable tier", contentType)
	}

	finalURL := out.Solution.URL
	if finalURL == "" {
		finalURL = fetchURL
	}
	resolved := ResolvedContent{
		Body:        htmlBody,
		ContentType: contentType,
		StatusCode:  200,
		FinalURL:    finalURL,
	}

	// Parse the HTML the headless browser rendered. The
	// same insufficient-content guard applies so a
	// challenge page that still didn't render real content
	// falls through (or fails when this is the last tier).
	if len(p.parsers) > 0 {
		if parser := pickParser(p.parsers, content_parsing.SourceHTML); parser != nil {
			parsed, parseErr := parser.Parse(ctx, htmlBody, content_parsing.SourceHTML, finalURL)
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

	return resolved, nil
}

func resolveFlareURL(resource Resource) (string, error) {
	switch resource.Type {
	case SourceURL:
		return resource.Value, nil
	case SourceDOI:
		return "https://doi.org/" + resource.Value, nil
	default:
		return "", fmt.Errorf("flaresolverr: unsupported source type: %s", resource.Type)
	}
}