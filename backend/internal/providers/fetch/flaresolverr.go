package fetch

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"mime"
	"net/http"
	"strings"
	"sync/atomic"
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
// The provider self-disables when no endpoint is configured:
// the constructor returns nil and the wiring layer skips
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
//
// Horizontal scaling: a single Byparr container drives one
// headless Chromium and queues concurrent requests
// internally, so a burst of 50 retrieve_source workers all
// needing the heavy tier saturates one container and every
// queued request burns its 60s timeout waiting. The
// provider therefore supports a pool of endpoints
// (round-robin) and an optional global concurrency cap
// (MaxConcurrency). With N containers and
// MaxConcurrency=N, at most N Resolve calls are in flight
// at once; the rest block on the semaphore (cheap, no
// timeout burn) until a slot frees. This keeps each
// container's queue short enough that requests complete
// within the per-request timeout.
type FlareSolverrProvider struct {
	endpoints []string
	timeout   time.Duration
	userAgent string
	parsers   []content_parsing.Parser
	client    *http.Client
	// robin is the round-robin cursor across endpoints. We
	// use atomic so concurrent Resolve calls pick distinct
	// endpoints without a mutex on the hot path.
	robin uint32
	// sem caps the number of in-flight Resolve calls across
	// the whole pool. nil means no cap (the sidecar's own
	// HTTP server is the only limit). The semaphore is
	// buffered with MaxConcurrency slots; each Resolve
	// acquires one on entry and releases on exit.
	sem chan struct{}
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
	return NewFlareSolverrProviderPool([]string{endpoint}, timeout, userAgent, 0, parsers...)
}

// NewFlareSolverrProviderPool builds a multi-endpoint
// FlareSolverr provider. All non-empty entries in endpoints
// are normalized to their /v1 URL and added to the
// round-robin pool. When the resulting pool is empty the
// constructor returns nil (self-disable). maxConcurrency
// caps the number of in-flight Resolve calls across the
// whole pool; 0 means no application-level cap. A typical
// production setting is maxConcurrency = len(endpoints)
// (one in-flight call per container) for challenge-heavy
// workloads, or 2*len(endpoints) for lighter pages.
func NewFlareSolverrProviderPool(endpoints []string, timeout time.Duration, userAgent string, maxConcurrency int, parsers ...content_parsing.Parser) *FlareSolverrProvider {
	cleaned := make([]string, 0, len(endpoints))
	for _, ep := range endpoints {
		ep = strings.TrimSpace(ep)
		if ep == "" {
			continue
		}
		// The FlareSolverr / Byparr protocol lives at /v1.
		// The operator-supplied URL is the base (e.g.
		// "http://flaresolverr:8191"); we append the path
		// so the POST lands on the right handler. A root
		// POST returns 405 (Method Not Allowed) which is
		// the symptom of forgetting this.
		if !strings.HasSuffix(ep, "/v1") {
			ep = strings.TrimRight(ep, "/") + "/v1"
		}
		cleaned = append(cleaned, ep)
	}
	if len(cleaned) == 0 {
		return nil
	}
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	parsedCleaned := make([]content_parsing.Parser, 0, len(parsers))
	for _, p := range parsers {
		if p != nil {
			parsedCleaned = append(parsedCleaned, p)
		}
	}
	if len(parsedCleaned) == 0 {
		parsedCleaned = append(parsedCleaned, content_parsing.NewTrafilaturaParser())
	}
	ua := userAgent
	if ua == "" {
		ua = defaultUserAgent
	}
	p := &FlareSolverrProvider{
		endpoints: cleaned,
		timeout:   timeout,
		userAgent: ua,
		parsers:   parsedCleaned,
		client: &http.Client{
			Timeout: timeout,
		},
	}
	if maxConcurrency > 0 {
		p.sem = make(chan struct{}, maxConcurrency)
	}
	return p
}

// acquireSem blocks until a pool-wide concurrency slot is
// available, or returns immediately when no cap is
// configured. The caller must release the slot via
// defer release(). When the context is cancelled while
// waiting, acquireSem returns the context error without
// acquiring a slot, so a backed-up pool doesn't strand a
// worker past its budget.
func (p *FlareSolverrProvider) acquireSem(ctx context.Context) error {
	if p.sem == nil {
		return nil
	}
	select {
	case p.sem <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (p *FlareSolverrProvider) release() {
	if p.sem == nil {
		return
	}
	select {
	case <-p.sem:
	default:
	}
}

// nextEndpoint returns the next endpoint in round-robin
// order. The atomic add wraps modulo 2^32, and we then
// index modulo the slice length — the rare wrap is
// harmless because the index is always reduced to a valid
// range.
func (p *FlareSolverrProvider) nextEndpoint() string {
	idx := atomic.AddUint32(&p.robin, 1)
	return p.endpoints[int(idx-1)%len(p.endpoints)]
}

func (p *FlareSolverrProvider) Supports(sourceType SourceType) bool {
	return sourceType == SourceURL || sourceType == SourceDOI
}

func (p *FlareSolverrProvider) Describe() ProviderDescription {
	return ProviderDescription{
		Name:        "FlareSolverr (headless Chromium)",
		Description: "Drives a FlareSolverr / Byparr headless-browser sidecar to solve JavaScript challenges (Cloudflare, Datadome, PerimeterX) that no amount of TLS fingerprinting or header spoofing can bypass. Returns HTML text only; PDF/image responses fall through to a byte-capable tier. The heaviest tier in the chain — config-gated and last-resort. Supports a round-robin pool of endpoints and an optional global concurrency cap so a single Byparr container is not saturated under burst load.",
		Requires:    "FLARESOLVERR_URL",
		Configured:  len(p.endpoints) > 0,
		Supports:    []string{"url", "doi"},
		Timeout:     p.timeout.String(),
		Notes:       "Self-disables when no endpoint is configured. Runs last in the chain so cheaper tiers get a chance first. Static host_overrides can pin a known-bad host to this tier in YAML. When multiple endpoints are configured, requests are round-robined across them; max_concurrency caps in-flight calls to avoid saturating a single container's internal queue.",
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

	// Acquire a pool-wide concurrency slot before building
	// the request. When the pool is saturated this blocks
	// (respecting ctx) instead of firing a request that
	// would just queue inside the sidecar and burn the
	// per-request timeout. A worker that blocks here is
	// cheap — it holds no sidecar resources — so capping
	// in-flight calls at the number of containers keeps
	// each container's internal queue short.
	if err := p.acquireSem(ctx); err != nil {
		return ResolvedContent{}, fmt.Errorf("flaresolverr: acquiring concurrency slot: %w", err)
	}
	defer p.release()

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

	endpoint := p.nextEndpoint()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
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
				if len(text) < MinExtractedLength || IsJSBoilerplate(text) || IsHTMLLeakBoilerplate(text) {
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