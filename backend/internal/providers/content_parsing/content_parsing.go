// Package content_parsing defines the contract for turning a
// raw resolved document (HTML, PDF, ...) into a uniform
// structured form: a clean text body, the cleaned HTML, a
// list of image URLs, and a small metadata bag.
//
// The package follows the same conventions as the other
// provider trees in this repository (search, fetch): one
// interface, multiple implementations registered in the
// composition root. The interface is HTTP-agnostic so the
// resolution providers (and any future transport, e.g. a
// CLI ingestion tool) can compose a Parser without taking
// on a dependency on net/http or the API layer.
package content_parsing

import (
	"context"
	"errors"
	"time"
)

// SourceType identifies the format of the raw bytes a Parser
// is being asked to extract from. New formats (PDF, EPUB,
// ...) are added here as new providers come online.
type SourceType string

const (
	// SourceHTML covers text/html and application/xhtml+xml.
	SourceHTML SourceType = "html"

	// SourcePDF is reserved for the future PDF parser. Returning
	// ErrUnsupported for this type today is the correct behaviour:
	// callers fail loud instead of silently treating a PDF as
	// an empty document.
	SourcePDF SourceType = "pdf"
)

// Errors callers can match against. Providers should wrap
// these with %w so the API layer can distinguish a "we
// don't speak this format" condition from a transient
// failure (network, parse, etc.).
var (
	// ErrUnsupported is returned when the requested source
	// type is not handled by this provider. Callers that
	// register multiple parsers can use Supports to avoid
	// even making the call.
	ErrUnsupported = errors.New("content_parsing: source type not supported by this parser")

	// ErrEmptyInput is returned when the raw byte slice has
	// no usable content. Surfacing this as a distinct
	// sentinel lets handlers respond with 422 instead of 500.
	ErrEmptyInput = errors.New("content_parsing: empty input")
)

// ParsedDoc is the structured result of parsing raw bytes
// into useful content. Every field is best-effort: an empty
// Title on a noisy page is still a successful parse. Callers
// should never rely on a specific field being set — they
// should branch on length, not presence.
type ParsedDoc struct {
	// Title is the document title (from <title>, og:title,
	// JSON-LD, or the first <h1> in that order).
	Title string

	// Text is the readable plain-text body with all
	// chrome / nav / footer / aside content removed by
	// the parser. Whitespace is normalized to single
	// spaces within a block and newlines between blocks.
	Text string

	// HTML is the cleaned article HTML — the same content
	// as Text but with the original inline structure
	// (paragraphs, lists, blockquotes, code blocks) kept
	// intact. Useful for re-rendering with custom CSS.
	HTML string

	// Markdown is the Markdown rendering of the same
	// cleaned article content HTML carries. It preserves
	// the inline structure (headings, bold/italic emphasis,
	// lists, blockquotes, code blocks, links) in a compact
	// form that is cheaper for an LLM to parse than raw HTML.
	// The decomposition worker prefers Markdown over Text
	// when feeding the fact extractor; parsed_text stays
	// the FTS source. Empty when the parser produced no
	// ContentNode (the HTML fallback path applies here too)
	// or for sources with no inline structure (PDF text is
	// set to plain Text and Markdown mirrors it verbatim).
	Markdown string

	// Images is the list of images that belong to the
	// article body. Chrome images (header logos, footer
	// badges, sidebar icons, hidden tracking pixels,
	// social-share buttons) are filtered out by the
	// parser's DOM pass; the list is collected from the
	// cleaned content node, not from a walk of the raw
	// HTML. Empty when the page has no images, the
	// parser stripped them, or the parser could not
	// absolutize them.
	//
	// For PDF sources this list is populated by the
	// resolver after persisting the per-page renders
	// returned in PageImages; the parser itself does
	// not touch the filesystem.
	Images []ImageRef

	// PageImages is the list of in-memory page renders
	// produced by the parser, one per page of the
	// document. The resolver persists them and fills
	// Images with the resulting paths. Empty for HTML
	// sources.
	//
	// The format is PNG by default; Format is set to
	// "png" for consumers that want to switch on the
	// encoder without sniffing the magic bytes.
	PageImages []PageImage

	// Excerpt is a short summary / description, when the
	// parser can derive one (og:description, meta
	// description, or the first sentence of the body).
	Excerpt string

	// Sitename is the publisher / site name (og:site_name).
	Sitename string

	// Author is the byline, when present.
	Author string

	// Language is the ISO 639-1 code (e.g. "en"), when
	// the parser detected one.
	Language string

	// PublishedAt is the publication date of the
	// underlying document, when the parser can recover
	// one. Day-precision (the time component is dropped
	// on parse) because every upstream we read it from
	// (trafilatura / htmldate) is day-precision and the
	// database column is Postgres DATE. Nil when the
	// parser could not surface a date (HTML pages
	// without an article:published_time meta tag and
	// without a date htmldate could extract, PDF
	// documents that don't carry a /CreationDate in
	// their Info dict, etc.). Callers should compare
	// against nil, not against time.Time{}.
	PublishedAt *time.Time
}

// ImageRef is one image found in the article body. It
// carries the absolute URL and the alt text from the
// <img> tag when present. The alt text is empty for
// PDF page renders (which have no HTML source).
type ImageRef struct {
	URL string
	Alt string
}

// PageImage is one rendered page of a document, kept in
// memory so the parser stays transport-agnostic. The
// resolver persists it to disk (or an object store) and
// stores the resulting path/URL in ParsedDoc.Images.
type PageImage struct {
	// Page is the 1-indexed page number.
	Page int
	// Bytes is the encoded image (PNG by default).
	Bytes []byte
	// Format is the image format ("png", "jpg", ...).
	Format string
}

// Parser turns raw bytes into a ParsedDoc. Implementations
// are expected to be safe for concurrent use after
// construction — the strategy layer may invoke one
// provider from many goroutines.
type Parser interface {
	// Parse extracts the main content from raw. The
	// finalURL argument is the URL the bytes were
	// fetched from, used to resolve relative image
	// URLs and to populate metadata fields like
	// Sitename. It may be empty for in-memory inputs.
	Parse(ctx context.Context, raw []byte, sourceType SourceType, finalURL string) (ParsedDoc, error)

	// Supports reports whether the parser can handle the
	// given source type. The strategy uses this to pick
	// the right provider without trial-and-error.
	Supports(sourceType SourceType) bool

	// Describe returns the static metadata surfaced
	// through the API's /providers endpoint and the
	// admin UI. A nil or zero-value description is
	// treated as "skip" by the handler.
	Describe() ProviderDescription
}

// ProviderDescription is the static metadata a parser
// exposes to operators. It mirrors the shape used by
// the search and resolution trees so a single UI card
// component can render any of them.
type ProviderDescription struct {
	Name        string   // human-friendly label, e.g. "Trafilatura (article extractor)"
	Description string   // one-paragraph summary of what the parser does
	Requires    string   // env var / config key needed ("" when always on)
	Configured  bool     // true when the parser is currently usable
	Supports    []string // source types handled, as strings ("html", "pdf")
	Notes       string   // free-form follow-up
}
