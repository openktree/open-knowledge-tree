package content_parsing

import (
	"bytes"
	"context"
	"fmt"
	nurl "net/url"
	"strings"

	md "github.com/JohannesKaufmann/html-to-markdown"
	"github.com/JohannesKaufmann/html-to-markdown/plugin"
	"github.com/go-shiori/dom"
	"golang.org/x/net/html"
	trafilatura "github.com/markusmobius/go-trafilatura"
)

// Compile-time check that TrafilaturaParser satisfies Parser.
var _ Parser = (*TrafilaturaParser)(nil)

// TrafilaturaParser extracts the main readable content from
// raw HTML using the go-trafilatura port of Python's
// trafilatura. It is the default Parser wired into the HTTP
// fetch resolver. The parser is stateless after
// construction and safe for concurrent use.
type TrafilaturaParser struct {
	opts trafilatura.Options
}

// Option configures a TrafilaturaParser. Use it to opt in
// to experimental features (images, links, fallback) or to
// pin language / metadata requirements.
type Option func(*TrafilaturaParser)

// WithIncludeImages keeps <img> tags in the cleaned
// content node. It is enabled by default in
// NewTrafilaturaParser because the alternative (walking
// the raw HTML) drags in chrome images — header logos,
// sidebar icons, footer badges, hidden tracking
// pixels — that have nothing to do with the article.
// Trafilatura's own DOM pass already excludes those
// (nav, header, footer, aside, [hidden], role=img on
// chrome-only containers, …), so the right image list
// is the one that survives the extraction, not the
// union with the raw tree.
//
// Pass this option off only when a caller explicitly
// wants the parser to skip <img> from the result.
func WithIncludeImages() Option {
	return func(p *TrafilaturaParser) {
		p.opts.IncludeImages = true
	}
}

// WithExcludeImages drops <img> from the cleaned
// content node and leaves the Images slice empty.
// Useful for callers that want a text-only result
// (e.g. a search indexer, a feed reader, an LLM that
// should not look at images).
func WithExcludeImages() Option {
	return func(p *TrafilaturaParser) {
		p.opts.IncludeImages = false
	}
}

// WithEnableFallback makes trafilatura fall back to
// readability / dom-distiller when its own extractor is
// not confident. Slower but more accurate on pages with
// unusual layouts.
func WithEnableFallback() Option {
	return func(p *TrafilaturaParser) {
		p.opts.EnableFallback = true
	}
}

// WithTargetLanguage filters out pages not written in the
// given ISO 639-1 code (e.g. "en"). Useful for repositories
// that only catalogue a single language.
func WithTargetLanguage(lang string) Option {
	return func(p *TrafilaturaParser) {
		p.opts.TargetLanguage = lang
	}
}

// NewTrafilaturaParser builds a parser with the default
// options. The defaults match the upstream library for
// everything except IncludeImages, which we flip on
// (see WithIncludeImages for the reasoning). The
// IncludeImages=true default is what guarantees the
// Images slice only carries article images and not
// chrome: trafilatura's cleaned node tree already
// excludes nav, header, footer, aside, [hidden] and
// the other low-signal containers, so the <img>
// elements that survive into ContentNode are, by
// construction, the ones a reader would consider part
// of the article body.
func NewTrafilaturaParser(opts ...Option) *TrafilaturaParser {
	p := &TrafilaturaParser{
		opts: trafilatura.Options{
			// Deduplicate matches the upstream default and
			// keeps recurring nav blocks out of the body.
			Deduplicate: true,
			// See WithIncludeImages. The raw-tree
			// fallback that previous versions used to
			// fill Images produced long lists of logo /
			// icon / footer images that the cleaned
			// text correctly stripped; we trust the
			// cleaned node instead.
			IncludeImages: true,
		},
	}
	for _, opt := range opts {
		opt(p)
	}
	return p
}

// Supports reports whether the parser handles the given
// source type. PDF is intentionally false here — the PDF
// parser is a separate provider that will register its
// own Supports answer.
func (p *TrafilaturaParser) Supports(sourceType SourceType) bool {
	return sourceType == SourceHTML
}

// Parse extracts the main article from raw HTML bytes. The
// finalURL is used to absolutize <img src> values and to
// seed metadata fields. It may be empty.
func (p *TrafilaturaParser) Parse(ctx context.Context, raw []byte, sourceType SourceType, finalURL string) (ParsedDoc, error) {
	if err := ctx.Err(); err != nil {
		return ParsedDoc{}, err
	}

	if !p.Supports(sourceType) {
		return ParsedDoc{}, fmt.Errorf("trafilatura: %w: %s", ErrUnsupported, sourceType)
	}

	if len(bytes.TrimSpace(raw)) == 0 {
		return ParsedDoc{}, ErrEmptyInput
	}

	parsed, err := nurl.Parse(finalURL)
	if err != nil {
		parsed = nil
	}
	opts := p.opts
	opts.OriginalURL = parsed

	result, err := trafilatura.Extract(bytes.NewReader(raw), opts)
	if err != nil {
		// trafilatura returns an error for a few different
		// conditions: language mismatch, missing essential
		// metadata, or a tree larger than MaxTreeSize. We
		// surface them as-is — handlers can map them to
		// 422 / 415 as appropriate.
		return ParsedDoc{}, fmt.Errorf("trafilatura extract: %w", err)
	}

	doc := ParsedDoc{
		Title:    result.Metadata.Title,
		Author:   result.Metadata.Author,
		Sitename: result.Metadata.Sitename,
		Excerpt:  result.Metadata.Description,
		Language: result.Metadata.Language,
		Text:     strings.TrimSpace(result.ContentText),
	}

	// Surface the publication date the library
	// recovered. Trafilatura populates Metadata.Date
	// from htmldate when it can find a date anywhere
	// in the page (meta tags, JSON-LD, visible text,
	// URL); a zero time.Time means "no date was
	// recoverable". We forward a *time.Time so the
	// caller can distinguish "not present" from
	// "present and zero" — the same distinction
	// Postgres NULL makes on the published_at column.
	if !result.Metadata.Date.IsZero() {
		d := result.Metadata.Date
		doc.PublishedAt = &d
	}

	// Render the cleaned content node back to HTML when we
	// have one. Some pages (very small / parser-unsure
	// documents) come back with a nil ContentNode; in that
	// case we fall back to wrapping the plain text in a
	// single <p> so the HTML field is never silently empty
	// in a way callers might mistake for a successful
	// empty parse.
	if result.ContentNode != nil {
		var buf bytes.Buffer
		if err := html.Render(&buf, result.ContentNode); err != nil {
			return ParsedDoc{}, fmt.Errorf("render content node: %w", err)
		}
		doc.HTML = buf.String()

		// Convert the cleaned content node's HTML to Markdown.
		// The Markdown carries the same inline structure the
		// HTML does (headings, bold/italic, lists, blockquotes,
		// code, links) in a compact form the fact extractor can
		// parse more cheaply than raw HTML. html-to-markdown is
		// configured with the GitHub-flavored plugin so fenced
		// code blocks and tables render the way an LLM expects.
		// A conversion failure is non-fatal: the worker falls
		// back to parsed_text, so we drop the error and leave
		// Markdown empty.
		if md, mdErr := htmlToMarkdown(doc.HTML); mdErr == nil {
			doc.Markdown = md
		}

		// Collect <img> elements that survived into the
		// cleaned content node. The node already has nav,
		// header, footer, aside, [hidden] and similar
		// low-signal containers removed, so the surviving
		// <img> elements are article images by
		// construction. We capture both the absolutized
		// src URL and the alt text so the UI can render
		// accessible <img> tags.
		for _, img := range dom.GetElementsByTagName(result.ContentNode, "img") {
			src := dom.GetAttribute(img, "src")
			if src == "" {
				continue
			}
			doc.Images = append(doc.Images, ImageRef{
				URL: absolutize(src, finalURL),
				Alt: strings.TrimSpace(dom.GetAttribute(img, "alt")),
			})
		}
		doc.Images = dedupeImageRefs(doc.Images)
	} else if doc.Text != "" {
		doc.HTML = "<p>" + html.EscapeString(doc.Text) + "</p>"
		// No structural node to convert; mirror the plain text
		// so consumers that prefer Markdown still get the body.
		doc.Markdown = doc.Text
	}

	return doc, nil
}

// Describe is the static metadata surfaced through the
// API. Trafilatura is always available — it has no
// external dependencies and no API key.
func (p *TrafilaturaParser) Describe() ProviderDescription {
	return ProviderDescription{
		Name:        "Trafilatura",
		Description: "Go port of Python's trafilatura. Strips chrome (nav, footer, sidebars) and returns the main article body, title, author, sitename and image list. Always available — no API key required.",
		Requires:    "",
		Configured:  true,
		Supports:    []string{"html"},
		Notes:       "Images are extracted from the cleaned content node by default (WithIncludeImages is on). The returned list contains only article images — header logos, footer badges, sidebar icons, hidden tracking pixels and other chrome are filtered out by the same DOM pass that strips the text. Pass WithExcludeImages to skip image extraction entirely.",
	}
}

// absolutize resolves a possibly-relative image URL against
// the page URL. Absolute URLs (with a scheme) pass through
// unchanged. Protocol-relative URLs get https.
func absolutize(src, base string) string {
	src = strings.TrimSpace(src)
	if src == "" {
		return ""
	}
	if strings.HasPrefix(src, "//") {
		return "https:" + src
	}
	if base == "" {
		return src
	}
	parsed, err := nurl.Parse(src)
	if err != nil || parsed.IsAbs() {
		return src
	}
	baseURL, err := nurl.Parse(base)
	if err != nil {
		return src
	}
	return baseURL.ResolveReference(parsed).String()
}

// dedupeImageRefs preserves order and removes duplicates
// by URL. When two <img> tags share the same src, the
// first alt text wins.
func dedupeImageRefs(in []ImageRef) []ImageRef {
	seen := make(map[string]struct{}, len(in))
	out := make([]ImageRef, 0, len(in))
	for _, r := range in {
		if _, ok := seen[r.URL]; ok {
			continue
		}
		seen[r.URL] = struct{}{}
		out = append(out, r)
	}
	return out
}

// mdConverter is the package-level html-to-markdown converter.
// It is constructed once and reused: the library's internals
// are safe for concurrent use after construction, and the
// GitHub-flavored plugin set (fenced code blocks, tables,
// strikethrough) matches what the fact extractor's LLMs are
// trained on. The converter is constructed with an empty
// domain because the trafilatura ContentNode HTML has already
// been absolutized against the page URL; passing a domain
// here would re-resolve relative URLs against the wrong base.
var mdConverter = func() *md.Converter {
	c := md.NewConverter("", true, nil)
	c.Use(plugin.GitHubFlavored())
	return c
}()

// htmlToMarkdown converts a cleaned-content HTML fragment to
// Markdown. It is the single conversion point for the package:
// the Trafilatura parser calls it on the rendered ContentNode
// and the PDF parser reuses it (when it has structural HTML to
// convert). Errors from the converter are surfaced so callers
// can decide whether to fall back to plain text; the
// Trafilatura parser treats an error as non-fatal and leaves
// Markdown empty, falling through to the parsed_text path.
func htmlToMarkdown(htmlStr string) (string, error) {
	return mdConverter.ConvertString(htmlStr)
}

