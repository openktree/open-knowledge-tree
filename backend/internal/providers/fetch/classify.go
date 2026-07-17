package fetch

import (
	"net/url"
	"strings"
)

// ClassifyURL inspects a raw identifier (URL, DOI string, or any of
// its common forms) and returns a normalized Resource with a known
// SourceType. It does not perform any network I/O; the result is
// always derivable from the input string alone. When the input
// shape permits, the returned Resource is enriched with a
// pre-populated DOI (extracted from a doi.org URL path or a bare
// "10.…" string) so the rest of the pipeline can act on the bare
// DOI without re-parsing the input.
func ClassifyURL(raw string) Resource {
	trimmed := strings.TrimSpace(raw)

	if isDOI(trimmed) {
		doi := extractDOI(trimmed)
		return Resource{Value: doi, Type: SourceDOI, DOI: doi}
	}

	parsed, err := url.Parse(trimmed)
	if err == nil && (parsed.Scheme == "http" || parsed.Scheme == "https") {
		return Resource{Value: trimmed, Type: SourceURL}
	}

	return Resource{Value: trimmed, Type: SourceURL}
}

// isDOI returns true when the input string is recognizably a DOI
// in one of its accepted shapes: a bare registrant/publisher
// prefix starting with "10.", or any of the doi.org / dx.doi.org
// URL forms. Cheap string check; no URL parsing.
func isDOI(s string) bool {
	if strings.HasPrefix(s, "10.") {
		return true
	}
	return strings.Contains(s, "doi.org/")
}

func extractDOI(s string) string {
	doi := s
	for _, prefix := range []string{
		"https://doi.org/",
		"http://doi.org/",
		"https://dx.doi.org/",
		"http://dx.doi.org/",
	} {
		doi = strings.TrimPrefix(doi, prefix)
	}
	return strings.TrimSpace(doi)
}
