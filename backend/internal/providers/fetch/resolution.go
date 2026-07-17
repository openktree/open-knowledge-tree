// Package fetch contains the resolution-provider interface plus
// concrete HTTP fetch implementations and the strategy that composes
// them. The interface lives here because it is the contract the
// strategy depends on when selecting a provider for a given source.
package fetch

import (
	"context"
	"errors"
	"strings"

	"github.com/openktree/open-knowledge-tree/backend/internal/providers/content_parsing"
)

type SourceType string

const (
	SourceURL SourceType = "url"
	SourceDOI SourceType = "doi"
)

// Sentinels the strategy treats as "skip this provider and try
// the next one" rather than a hard failure. Returning one of
// these from Resolve keeps the per-provider error message
// informative in the audit trail while still letting the chain
// fall through.
var (
	// ErrInsufficientContent is returned when a provider fetched
	// a body and parsed it but the extracted text is shorter than
	// MinExtractedLength. The canonical case is a JavaScript-only
	// page whose <noscript> fallback has no real article body; the
	// strategy should fall through to a heavier tier (e.g. a
	// headless browser) instead of treating the empty parse as a
	// successful fetch.
	ErrInsufficientContent = errors.New("fetch: extracted content below minimum length")

	// ErrBodyTooLarge is returned when the response body exceeds
	// the provider's configured MaxBodyBytes cap. Bounded memory
	// use in the worker is worth more than a single huge document;
	// the strategy falls through in case a lighter provider can
	// serve the same URL with a smaller payload.
	ErrBodyTooLarge = errors.New("fetch: response body exceeds max bytes")
)

// MinExtractedLength is the minimum length of Parsed.Text below
// which a fetch is considered insufficient. The value matches the
// reference Python implementation's MIN_EXTRACTED_LENGTH and is
// what lets the chain fall through on JS-bot-Challenge pages that
// return 200 with no real article body.
const MinExtractedLength = 200

// jsBoilerplatePhrases are substrings that indicate the extracted
// text is a <noscript> fallback, a JS-required challenge page, a
// WAF "Validate User" captcha interstitial, a cookies-disabled
// publisher gate, or any other boilerplate that proves the
// response was not the real article. When any of these appear in
// Parsed.Text the fetch is treated as ErrInsufficientContent
// regardless of text length, so the chain falls through to a
// heavier tier (FlareSolverr) that can render the JavaScript —
// or, when the heavier tier also fails, the row is marked failed
// instead of silently persisting the boilerplate as if it were
// the article body. The check is case-insensitive.
//
// The list is grown from the empirical "silent failure" set
// surfaced by scripts/diagnose-sources against a real corpus.
// Each phrase was chosen to be:
//   - Specific enough not to fire on a legitimate article (e.g.
//     "validate user" is the exact OUP title; a paper would not
//     use that phrase in its body).
//   - Stable across the publisher's variation of the interstitial
//     (e.g. "could not validate captcha" is the body OUP renders
//     on every retry, not a one-off).
// Keep the bar high; a false positive drops a good source into
// the failed bucket and forces a re-fetch.
var jsBoilerplatePhrases = []string{
	// <noscript> fallbacks (the original set).
	"javascript is disabled",
	"please enable javascript",
	"enable it to continue",
	"doesn't work properly without javascript",
	"you need to enable javascript",
	// OUP "Validate User" captcha interstitial. Title is
	// "Validate User"; body starts with "We are sorry, but we
	// are experiencing unusual traffic… Could not validate
	// captcha." Either phrase is unique to the interstitial.
	"validate user",
	"could not validate captcha",
	"experiencing unusual traffic",
	// Wiley "Cookies disabled" gate. Body starts with
	// "Cookies disabled Cookies are disabled for this browser.
	// Wiley Online Library requires cookies for authentication…"
	"cookies are disabled",
	"requires cookies for authentication",
	// Generic JS-bot-challenge phrases observed in the corpus.
	"making sure you're not a bot",
	"please verify you are a human",
	"site protection: verifying your request",
	// "Dear visitor" captcha landing (CyberPurify / similar).
	// Paired with "fight cybercrime" to avoid a false positive
	// on a legitimate letter-to-the-editor that opens with
	// "Dear visitor".
	"dear visitor",
	"fight cybercrime",
	// Cloudflare 5xx landing page ("Connection timed out Error
	// code 522"). The origin server is down; the row should be
	// failed, not stored as the article.
	"connection timed out error code",
	// Rails / Django "The page isn't redirecting properly"
	// landing. Surfaces when a publisher's redirect chain is
	// broken; the body is the framework error page, not the
	// article.
	"the page isn't redirecting properly",
}

// IsJSBoilerplate reports whether text contains any of the
// JS-required boilerplate phrases, indicating the page is a
// <noscript> fallback rather than real article content. Used
// by the providers alongside the MinExtractedLength check so
// a 514-char noscript block doesn't pass the length guard.
func IsJSBoilerplate(text string) bool {
	if text == "" {
		return false
	}
	lower := strings.ToLower(text)
	for _, phrase := range jsBoilerplatePhrases {
		if strings.Contains(lower, phrase) {
			return true
		}
	}
	return false
}

// htmlLeakPrefixes are byte-sequences that, when found at the
// start of Parsed.Text, indicate trafilatura returned raw HTML
// verbatim instead of extracting text. The canonical case is a
// PerimeterX / "Google Tag Manager" challenge page (seen on
// jstor.org, sciencedirect.com): the response body is a JS
// challenge whose <noscript> fallback is an <iframe> the
// parser can't strip, so Parsed.Text starts with
// `<iframe title="Google Tag Manager"…`. A real article's
// extracted text never starts with an HTML tag — trafilatura
// strips all markup — so this is a safe signal.
//
// The check is byte-based (no ToLower allocation) because it
// runs on the hot path of every fetch; the prefixes are
// already lowercased.
var htmlLeakPrefixes = [][]byte{
	[]byte(`<iframe title="google tag manager"`),
	[]byte(`<iframe title='google tag manager'`),
}

// IsHTMLLeakBoilerplate reports whether text begins with a raw
// HTML tag trafilatura failed to strip — the signature of a
// PerimeterX / GTM challenge page that leaked through the
// parser. The providers call it alongside IsJSBoilerplate so
// the chain falls through to FlareSolverr instead of storing
// the challenge iframe as the article body.
//
// The check only inspects the first 200 bytes: a real article
// whose body happens to contain an <iframe> halfway through is
// not flagged, because the prefix is what distinguishes a
// challenge page (the iframe is the entire <noscript> body)
// from a legitimate article (the iframe is inline content
// surrounded by real text).
func IsHTMLLeakBoilerplate(text string) bool {
	if text == "" {
		return false
	}
	head := text
	if len(head) > 200 {
		head = head[:200]
	}
	lower := strings.ToLower(head)
	for _, prefix := range htmlLeakPrefixes {
		if strings.HasPrefix(lower, string(prefix)) {
			return true
		}
	}
	return false
}

// MaxBodyBytes caps the raw response body a provider will read
// into memory. The value is generous enough for academic articles
// (HTML or PDF) while bounding worker memory under concurrent
// fetches. Matches the reference's implicit cap.
const MaxBodyBytes int64 = 15 * 1024 * 1024

// Resource is the result of classifying a user-supplied identifier
// (URL, DOI string, or any of its common forms). The Value/Type
// pair is the minimum information the fetch strategy needs to
// act. The DOI enrichment field is populated on a best-effort
// basis whenever the input shape makes it derivable (a doi.org
// URL extracts the DOI from the path; a bare "10.…" string is
// already the DOI). Callers and providers that have additional
// context (e.g. a search result that already knows the DOI) can
// also fill it in; the worker persists it on the source row.
type Resource struct {
	Value string
	Type  SourceType
	DOI   string // bare DOI (e.g. "10.1038/nature12373") when known
}

type ResolvedContent struct {
	// Body is the raw bytes returned by the resolver. For
	// HTML responses this is the original document; for
	// the PDF resolver (future) it will be the raw PDF.
	// The fetch provider keeps it for now so callers
	// (debug endpoints, cache layers) can fall back to it
	// when the parser fails. A future change will drop
	// it from the API surface once the parser is
	// reliable enough to be canonical.
	Body []byte

	// ContentType is the response Content-Type header,
	// lowercased. Used to pick a content_parsing.Parser
	// and to surface a 415 when no parser supports the
	// declared type.
	ContentType string

	// StatusCode is the HTTP status from the resolver.
	StatusCode int

	// FinalURL is the URL the resolver ended up at after
	// any redirects. May differ from the requested URL
	// (publisher landing pages, DOI redirects, etc.).
	FinalURL string

	// Parsed is the structured content extracted by the
	// content_parsing.Parser wired into the resolver. It
	// is the canonical view of the document for API
	// consumers — the raw Body is kept only as a
	// fallback / debug aid.
	Parsed content_parsing.ParsedDoc

	// OAStatus is the open-access status of the work, as
	// reported by Unpaywall when the DOI was looked up.
	// Values: "green", "gold", "bronze", "hybrid",
	// "closed", or "" when Unpaywall was not consulted
	// (no DOI, or Unpaywall not configured). The worker
	// persists this on the source row so the UI can show
	// users why an article might be incomplete — a
	// "closed" status explains that the full text is
	// paywalled and only the abstract/landing page was
	// retrieved.
	OAStatus string

	// OARedirectURL is the direct OA URL Unpaywall discovered
	// (e.g. a publisher PDF link like
	// "https://dl.acm.org/doi/pdf/10.1145/882262.882269").
	// The Unpaywall provider sets it on the error result when
	// it found an OA location but couldn't fetch the body
	// (403, network error, etc.). The strategy uses it to
	// retry the remaining URL-capable providers (TLS, fetch)
	// with this specific URL instead of the DOI redirect —
	// a different TLS fingerprint might get through where
	// Unpaywall's plain HTTP client got 403. Empty when
	// Unpaywall didn't run, found no OA location, or
	// successfully fetched the body itself.
	OARedirectURL string

	// Attempts is the audit trail of every provider the
	// strategy tried for this resource, in chain order.
	// The strategy populates it regardless of success so
	// the worker / UI can show which tier fetched the
	// content (or which tiers failed and why). Empty
	// when a single provider is called directly (outside
	// the strategy).
	Attempts []FetchAttempt
}

// FetchAttempt records one provider's attempt to resolve a
// resource. The strategy appends one entry per provider it
// tries (in chain order) and returns the full list on the
// ResolvedContent so the worker can persist it on the source
// row. The audit trail makes "why did this fetch fail / which
// tier won" answerable without grepping logs.
type FetchAttempt struct {
	// Provider is the provider id (e.g. "fetch",
	// "unpaywall", "tls", "flaresolverr"). Matches the id
	// surfaced by /sources/providers.
	Provider string `json:"provider"`

	// Success is true when the provider returned a
	// non-error ResolvedContent.
	Success bool `json:"success"`

	// Error is the provider's error message when Success
	// is false, empty otherwise.
	Error string `json:"error,omitempty"`

	// ElapsedMs is the wall-clock time the provider took,
	// rounded to milliseconds.
	ElapsedMs int64 `json:"elapsed_ms"`

	// OAStatus is the open-access status the Unpaywall
	// provider discovered for this DOI, even when it fell
	// through (no OA location found). Other providers
	// leave it empty. Values: "green", "gold", "bronze",
	// "hybrid", "closed", or "". The strategy copies the
	// first non-empty OAStatus from any attempt onto the
	// final ResolvedContent so the worker can persist it
	// on the source row regardless of which tier won.
	OAStatus string `json:"oa_status,omitempty"`
}

type ResolutionProvider interface {
	Resolve(ctx context.Context, resource Resource) (ResolvedContent, error)
	Supports(sourceType SourceType) bool
	Describe() ProviderDescription
}

// ProviderDescription is the static metadata the API surfaces
// for a resolution provider. The fields are kept narrow: just
// what the UI needs to render an informational card. The
// "Configured" / "Requires" pair tells the operator why a
// provider might be present in the strategy with no obvious
// toggle — e.g. Unpaywall is in the list whenever the env
// var is set, and "Requires: UNPAYWALL_EMAIL" explains how
// to enable it. Providers that have nothing useful to
// advertise beyond their name can return a zero-value
// description; the handler treats a nil Description the
// same way (skip).
type ProviderDescription struct {
	Name        string   // human-friendly label, e.g. "Unpaywall (OA lookup for DOIs)"
	Description string   // one-paragraph summary of what the provider does
	Requires    string   // env var / config key needed to enable the provider ("" when always on)
	Configured  bool     // true when the provider is currently usable
	Supports    []string // source types the provider handles, as strings ("url", "doi")
	Timeout     string   // request timeout as a human-readable string, e.g. "30s"
	Notes       string   // free-form follow-up, e.g. "Falls through to the next provider when no OA location is found"
}
