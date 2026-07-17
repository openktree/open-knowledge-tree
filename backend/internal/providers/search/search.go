// Package search defines the SearchProvider interface and shared
// types used by all search-provider implementations. Concrete
// implementations live in sibling subpackages (e.g. search/serper,
// search/openalex).
package search

import (
	"context"
	"time"
)

// SearchResult is a single hit returned by a search provider.
//
// The handler layer may tag results with AlreadyExists /
// ExistingStatus after the provider returns them: those two fields
// are populated by the HTTP TestSearch handler (which has the
// repository context the provider does not), never by the provider
// itself. Providers leave them at their zero values.
type SearchResult struct {
	Title   string `json:"title"`
	URL     string `json:"url"`
	Snippet string `json:"snippet"`

	// Enrichment fields populated on a best-effort basis by
	// providers that have the data (e.g. OpenAlex returns
	// `doi` and the canonical `id` URL on every work record).
	// Zero when the upstream did not include them. Callers
	// that want to fetch a DOI directly should prefer
	// Result.DOI when it is non-empty.
	DOI        string     `json:"doi,omitempty"`
	OpenAlexID string     `json:"openalex_id,omitempty"`

	// PublishedAt is the publication date of the underlying
	// resource, when the provider has one. Day-precision
	// (the time component is dropped on parse) because
	// every upstream we read it from (OpenAlex
	// `publication_date`, future Crossref `published-print`)
	// is day-precision. Nil for providers that don't ship
	// a date (Serper) or for records where the upstream
	// returned no date. Marshals as an RFC 3339 timestamp
	// with the time component zeroed (`"2018-02-13T00:00:00Z"`);
	// a frontend that only wants the day string can read
	// the leading 10 characters.
	PublishedAt *time.Time `json:"published_at,omitempty"`

	// AlreadyExists is set by the TestSearch HTTP handler when
	// the result's URL or DOI matches an existing source row in
	// the active repository. Providers never set it. The flag is
	// `omitempty`-clean (default false) so a legacy caller that
	// ignores it sees no change in the wire shape.
	AlreadyExists bool `json:"already_exists,omitempty"`

	// ExistingStatus carries the status ("pending", "fetching",
	// "fetched", "failed") of the matched source row when
	// AlreadyExists is true. It is nil when the result did not
	// match an existing row.
	ExistingStatus *string `json:"existing_status,omitempty"`
}

// SearchOptions carries pagination controls into a Search call.
// PerPage is the page size (the provider applies its own default
// and cap when zero or out of range); Cursor is an opaque,
// provider-specific pagination token — empty means "first page".
// The provider returns the next page's cursor in SearchResponse.
type SearchOptions struct {
	PerPage int
	Cursor  string
}

// SearchResponse wraps the result slice with pagination metadata.
// Total is the upstream's count of results matching the query, or
// 0 when the upstream does not surface a count (Serper). NextCursor
// is the opaque token the caller should pass as SearchOptions.Cursor
// to fetch the next page; empty means there are no more pages.
type SearchResponse struct {
	Results    []SearchResult
	Total      int64
	NextCursor string
}

type SearchProvider interface {
	Search(ctx context.Context, query string, opts SearchOptions) (SearchResponse, error)
}