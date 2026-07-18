package search

import (
	"context"
	"fmt"
	"time"

	"github.com/openktree/open-knowledge-tree/backend/internal/config"
	"github.com/openktree/open-knowledge-tree/backend/internal/providers/registry"
)

// RegistrySearchProvider implements SearchProvider against the OKT
// Knowledge Registry's source catalog (GET /api/v1/sources?q=). It
// is the keyless default: a deployment with no SERPER_API_KEY and
// no OPENALEX_EMAIL still gets a working search provider so agents
// can discover sources other OKT instances have contributed.
//
// The provider wraps a *registry.ServiceMap and resolves the active
// registry from the request context (set by the handler from the
// repo's registry_id column via registry.WithRegistryID). When no
// registry id is on the context it falls back to "default".
//
// Pagination is offset-based: the cursor is a base64-encoded
// integer offset (empty = first page). NextCursor is computed from
// the offset + page size + total so the caller knows when to stop.
type RegistrySearchProvider struct {
	services *registry.ServiceMap
	perPage  int
	timeout  time.Duration
}

// NewRegistrySearchProvider builds a provider over a ServiceMap.
// perPage is the page size used when the caller doesn't supply one
// (clamped to registry.SearchSources' bounds); timeout caps each
// search call. Zero perPage/timeout fall back to the package
// defaults (20 / 15s) — same shape as the Serper/OpenAlex providers.
func NewRegistrySearchProvider(services *registry.ServiceMap, cfg config.SearchRegistryProviderConfig) *RegistrySearchProvider {
	perPage := cfg.PerPage
	if perPage <= 0 {
		perPage = registrySearchDefaultPerPage
	}
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	return &RegistrySearchProvider{services: services, perPage: perPage, timeout: timeout}
}

// Search implements SearchProvider. It resolves the active registry
// from ctx, calls Service.SearchSources, and maps each
// RemoteSourceMeta to a SearchResult. Title, URL, and DOI (bare)
// are populated from the registry meta; Snippet is the truncated
// title (the registry's ListSources response carries no abstract);
// OpenAlexID and PublishedAt are nil (the registry is neutral on
// both). Total and NextCursor come from the registry response.
func (p *RegistrySearchProvider) Search(ctx context.Context, query string, opts SearchOptions) (SearchResponse, error) {
	if p.services == nil || !p.services.IsConfigured() {
		return SearchResponse{}, fmt.Errorf("registry: not configured")
	}
	perPage := opts.PerPage
	if perPage <= 0 {
		perPage = p.perPage
	}
	cursor := opts.Cursor

	sctx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()
	svc := p.services.Service(ctx)
	res, err := svc.SearchSources(sctx, query, perPage, cursor)
	if err != nil {
		return SearchResponse{}, err
	}

	results := make([]SearchResult, 0, len(res.Sources))
	for _, rs := range res.Sources {
		results = append(results, SearchResult{
			Title:   rs.Title,
			URL:     rs.URL,
			Snippet: truncateSnippet(rs.Title, 300),
			DOI:     rs.DOI,
		})
	}

	// Decode the current cursor to recover the offset so we can
	// compute the next one. A malformed cursor is treated as 0
	// (the first page) — same posture as Serper's page fallback.
	offset, _ := registry.DecodeCursorOffset(cursor)

	return SearchResponse{
		Results:    results,
		Total:      int64(res.Total),
		NextCursor: registry.NextSearchCursor(offset, perPage, res.Total),
	}, nil
}

// registrySearchDefaultPerPage mirrors the constant in the registry
// package so this provider has a sensible default without importing
// it (the constant is unexported there). 20 matches OpenAlex's
// default; the registry enforces its own cap (100) server-side.
const registrySearchDefaultPerPage = 20