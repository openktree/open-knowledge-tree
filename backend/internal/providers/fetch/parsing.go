package fetch

import (
	"bytes"
	"mime"
	"strings"

	"github.com/openktree/open-knowledge-tree/backend/internal/providers/content_parsing"
)

// detectOASourceType maps a Content-Type header (and the body,
// as a fallback for servers that omit the header or send
// text/plain) to a content_parsing.SourceType. The bool return
// is false when the type is not understood, in which case the
// parser is skipped and ResolvedContent.Parsed stays empty.
// Shared by FetchResolutionProvider, UnpaywallResolutionProvider,
// TLSImpersonationProvider, and FlareSolverrProvider so every
// tier picks the parser the same way.
func detectOASourceType(contentType string, body []byte) (content_parsing.SourceType, bool) {
	mt, _, err := mime.ParseMediaType(contentType)
	if err != nil || mt == "" {
		mt = strings.ToLower(strings.TrimSpace(strings.SplitN(contentType, ";", 2)[0]))
	}
	switch mt {
	case "text/html", "application/xhtml+xml", "application/xml", "text/xml":
		return content_parsing.SourceHTML, true
	case "application/pdf":
		return content_parsing.SourcePDF, true
	}
	if len(body) > 0 {
		head := body
		if len(head) > 512 {
			head = head[:512]
		}
		trimmed := bytes.TrimLeft(head, " \t\r\n\ufeff")
		if len(trimmed) > 0 {
			c := trimmed[0]
			if c == '<' {
				lower := bytes.ToLower(trimmed)
				if bytes.HasPrefix(lower, []byte("<!doctype html")) ||
					bytes.HasPrefix(lower, []byte("<html")) {
					return content_parsing.SourceHTML, true
				}
			}
		}
	}
	return "", false
}

// pickParser returns the first parser that Supports the given
// source type. Shared by every resolution provider so the OA
// path and the plain fetch path pick the same parser for a
// given Content-Type. Order matters: pass the most specific
// parser first.
func pickParser(parsers []content_parsing.Parser, sourceType content_parsing.SourceType) content_parsing.Parser {
	for _, parser := range parsers {
		if parser.Supports(sourceType) {
			return parser
		}
	}
	return nil
}