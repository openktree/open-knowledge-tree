package content_parsing

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/gen2brain/go-fitz"
)

// Compile-time check that FitzPDFParser satisfies Parser.
var _ Parser = (*FitzPDFParser)(nil)

// DefaultPDFDPI is the resolution used when rendering
// pages to PNG. 150 DPI is a reasonable compromise between
// legibility of embedded text and file size (~80–200 KB
// per A4 page); it matches what most academic-paper
// viewers default to.
const DefaultPDFDPI = 150.0

// FitzPDFParser extracts text and per-page raster images
// from PDF documents using the purego build of go-fitz
// (vendored MuPDF). The parser is stateless apart from
// its configuration, but go-fitz's Document is not safe
// for concurrent use — the constructor accepts an
// OutputDir so the resolver can persist renders, and the
// parser serializes Document access through an internal
// mutex.
//
// The parser keeps the rendered pages in memory
// (PageImages) so the caller — typically the HTTP fetch
// resolver — can decide whether to write them to disk,
// push them to an object store, or stream them straight
// to a client. The parser itself never touches the
// filesystem.
type FitzPDFParser struct {
	dpi    float64
	mu     sync.Mutex
}

// Option configures a FitzPDFParser.
type PDFOption func(*FitzPDFParser)

// WithDPI overrides the per-page render DPI. Values
// outside [72, 600] are clamped to avoid absurd memory
// use or unusable output.
func WithDPI(dpi float64) PDFOption {
	return func(p *FitzPDFParser) {
		if dpi < 72 {
			dpi = 72
		}
		if dpi > 600 {
			dpi = 600
		}
		p.dpi = dpi
	}
}

// NewFitzPDFParser builds a parser using the default DPI.
// go-fitz is licensed under AGPL; if your project license
// is incompatible, gate the import on a build tag and
// provide a stub that returns ErrUnsupported.
func NewFitzPDFParser(opts ...PDFOption) *FitzPDFParser {
	p := &FitzPDFParser{dpi: DefaultPDFDPI}
	for _, opt := range opts {
		opt(p)
	}
	return p
}

// Supports reports whether the parser handles the given
// source type. PDF only — HTML is trafilatura's job.
func (p *FitzPDFParser) Supports(sourceType SourceType) bool {
	return sourceType == SourcePDF
}

// Parse extracts text from every page and renders each
// page to a PNG at the configured DPI. The returned
// ParsedDoc has Text set to the concatenation of all
// pages (separated by a form-feed so callers that care
// about page boundaries can split on "\f"), PageImages
// populated with one entry per page, and Title set to
// the document's metadata title when present.
//
// The parser does not write anything to disk. Callers
// persist PageImages themselves.
func (p *FitzPDFParser) Parse(ctx context.Context, raw []byte, sourceType SourceType, finalURL string) (ParsedDoc, error) {
	if err := ctx.Err(); err != nil {
		return ParsedDoc{}, err
	}
	if !p.Supports(sourceType) {
		return ParsedDoc{}, fmt.Errorf("fitz pdf: %w: %s", ErrUnsupported, sourceType)
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return ParsedDoc{}, ErrEmptyInput
	}

	// go-fitz's Document is not safe for concurrent use
	// across goroutines, but Parse is called per-request
	// by the resolver. We still take a mutex so a single
	// shared parser cannot have two goroutines inside
	// the same Document at once.
	p.mu.Lock()
	defer p.mu.Unlock()

	doc, err := fitz.NewFromMemory(raw)
	if err != nil {
		return ParsedDoc{}, fmt.Errorf("fitz open: %w", err)
	}
	defer doc.Close()

	pageCount := doc.NumPage()
	if pageCount <= 0 {
		return ParsedDoc{}, fmt.Errorf("fitz: document has no pages")
	}

	out := ParsedDoc{}
	if meta := doc.Metadata(); meta != nil {
		// go-fitz returns metadata in a 256-byte
		// fixed-size buffer, so absent fields come
		// back as NUL-filled strings. Trim them
		// before falling back to the URL basename.
		out.Title = trimNULs(firstNonEmpty(meta["title"], meta["Title"]))
		out.Author = trimNULs(firstNonEmpty(meta["author"], meta["Author"]))
	}
	// Fall back to the filename in the metadata if no
	// title was found. Useful when the PDF has no
	// /Title entry.
	if out.Title == "" && finalURL != "" {
		out.Title = basename(finalURL)
	}

	texts := make([]string, 0, pageCount)
	pages := make([]PageImage, 0, pageCount)
	for i := 0; i < pageCount; i++ {
		// Page numbers in go-fitz are 0-indexed, but
		// the PageImage struct uses 1-indexed numbers
		// to match the convention readers expect.
		pageNum := i + 1

		t, err := doc.Text(i)
		if err != nil {
			return ParsedDoc{}, fmt.Errorf("fitz text page %d: %w", pageNum, err)
		}
		texts = append(texts, t)

		png, err := doc.ImagePNG(i, p.dpi)
		if err != nil {
			return ParsedDoc{}, fmt.Errorf("fitz render page %d: %w", pageNum, err)
		}
		pages = append(pages, PageImage{
			Page:   pageNum,
			Bytes:  png,
			Format: "png",
		})
	}

	out.Text = strings.Join(texts, "\f")
	// PDFs extracted by MuPDF have no inline HTML structure to
	// convert, so Markdown mirrors the plain text. This keeps
	// the column uniformly populated so the decomposition worker
	// does not need a format switch on the source kind.
	out.Markdown = out.Text
	out.PageImages = pages
	return out, nil
}

// Describe returns the static metadata surfaced through
// the API. The PDF parser is always available when its
// dependency is linked in; Configured is true unconditionally.
func (p *FitzPDFParser) Describe() ProviderDescription {
	return ProviderDescription{
		Name:        "MuPDF (PDF)",
		Description: "Go binding for MuPDF. Extracts text from every page and renders each page to a PNG at the configured DPI (default 150). Embedded images are not extracted individually; pages are returned as full-page PNGs that the UI can display or crop.",
		Requires:    "github.com/gen2brain/go-fitz (vendored mupdf, AGPL)",
		Configured:  true,
		Supports:    []string{"pdf"},
		Notes:       "Set WithDPI to trade off legibility vs. storage. Page renders are kept in memory by the parser; the resolver persists them to disk or object storage.",
	}
}

// basename returns the last path segment, stripped of the
// .pdf extension, as a best-effort fallback title.
func basename(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if idx := strings.LastIndexAny(raw, "/?#"); idx >= 0 {
		raw = raw[idx+1:]
	}
	raw = strings.TrimSuffix(raw, ".pdf")
	raw = strings.TrimSuffix(raw, ".PDF")
	return strings.TrimSpace(raw)
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// trimNULs strips trailing NULs and surrounding
// whitespace. go-fitz's Metadata() lookup fills the
// destination buffer with a fixed 256-byte array, so
// absent fields come back as long runs of \x00.
func trimNULs(s string) string {
	s = strings.TrimRight(s, "\x00")
	return strings.TrimSpace(s)
}
