package registry

import (
	"context"
	"fmt"

	"github.com/openktree/open-knowledge-tree/backend/internal/config"
)

// registryIDKey is the context key the search and cache adapters
// use to carry the active registry id from the HTTP handler (which
// reads it from the repo's registry_id column) down to the
// ServiceMap.Service(ctx) lookup. It is private to this package so
// callers go through WithRegistryID / RegistryIDFromContext.
type registryIDKey struct{}

// WithRegistryID returns a copy of ctx carrying the active registry
// id. The search and cache adapters read it via RegistryIDFromContext
// to pick the right Service when a repo has selected a non-default
// registry. An empty id falls back to "default" at lookup time.
func WithRegistryID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, registryIDKey{}, id)
}

// RegistryIDFromContext returns the registry id stored on ctx, or
// "" when none is present. ServiceMap.Service(ctx) applies the
// "default" fallback so callers can pass the empty string through.
func RegistryIDFromContext(ctx context.Context) string {
	id, _ := ctx.Value(registryIDKey{}).(string)
	return id
}

// Service is the core, filter-aware layer over a single registry
// *Client. It is the "core class" the search and cache adapters
// share: both wrap a Service to reach the registry, so the model
// whitelist, promptset-acceptance, sync-level, and context-mapping
// rules live in one place instead of being duplicated across the
// pull workers.
//
// Service is transport-agnostic (no HTTP, no DB) — it only talks to
// the registry Client and applies RelevanceFilter rules. Callers
// (the pull/contribute workers, the search adapter, the cache
// adapter) own the DB writes.
type Service struct {
	client *Client
	cfg    config.RegistryConfig
}

// NewService builds a Service over an existing *Client. The cfg is
// carried so the service can fall back to the global allowed_models
// when a RelevanceFilter doesn't override it.
func NewService(c *Client, cfg config.RegistryConfig) *Service {
	return &Service{client: c, cfg: cfg}
}

// Client returns the underlying *Client. Exposed so callers that
// need a method the Service doesn't yet wrap (e.g. PushSource) can
// reach it without a cast.
func (s *Service) Client() *Client { return s.client }

// Config returns the registry config this service was built from.
func (s *Service) Config() config.RegistryConfig { return s.cfg }

// IsConfigured reports whether the underlying client points at a
// live registry (non-empty baseURL). A disabled service is a no-op
// for every method.
func (s *Service) IsConfigured() bool {
	return s != nil && s.client != nil && s.client.IsConfigured()
}

// SearchSourcesResult is the page returned by SearchSources, the
// free-text search the RegistrySearchProvider adapter wraps. It
// mirrors the registry's ListSourcesResponse so the adapter can
// map each RemoteSourceMeta to a search.SearchResult without an
// extra round-trip.
type SearchSourcesResult struct {
	Sources []RemoteSourceMeta
	Total   int
}

// SearchSources runs a free-text search over the registry's source
// catalog via GET /api/v1/sources?q=. The query is a LIKE search
// across title/url/doi. Per-page and cursor are offset-based: the
// cursor is a base64-encoded integer offset (empty = first page).
// Used by the RegistrySearchProvider adapter so a keyless deployment
// (no SERPER_API_KEY, no OPENALEX_EMAIL) can still discover sources.
func (s *Service) SearchSources(ctx context.Context, q string, perPage int, cursor string) (*SearchSourcesResult, error) {
	if !s.IsConfigured() {
		return nil, ErrRegistryDisabled
	}
	if perPage <= 0 {
		perPage = registrySearchDefaultPerPage
	}
	if perPage > registrySearchMaxPerPage {
		perPage = registrySearchMaxPerPage
	}
	offset, err := DecodeCursorOffset(cursor)
	if err != nil {
		offset = 0
	}
	resp, err := s.client.ListSources(ctx, perPage, offset, q)
	if err != nil {
		return nil, err
	}
	return &SearchSourcesResult{
		Sources: resp.Sources,
		Total:   resp.Total,
	}, nil
}

// NextSearchCursor returns the opaque cursor the caller should pass
// to fetch the next page, or "" when the current page is the last.
// It encodes the next offset as base64 so the cursor stays an
// implementation detail of the registry provider (the same way
// Serper uses a page number and OpenAlex uses the upstream cursor).
func NextSearchCursor(currentOffset, pageSize, total int) string {
	next := currentOffset + pageSize
	if total > 0 && next >= total {
		return ""
	}
	if pageSize <= 0 {
		return ""
	}
	return encodeCursorOffset(next)
}

// LookupCacheSource checks the registry for a source matching the
// given URL or DOI and, on a hit, pulls the full source package
// (with its Decompositions list). This is the cache-hit shortcut
// the RegistryCacheProvider adapter wraps — it replaces the inline
// SearchSource + PullSource block the retrieve_source / pull
// workers used to inline.
//
// Returns (pkg, true, nil) on a hit, (nil, false, nil) on a miss,
// and (nil, false, err) on a registry error.
func (s *Service) LookupCacheSource(ctx context.Context, sourceURL, doi string) (*SourcePackage, bool, error) {
	if !s.IsConfigured() {
		return nil, false, nil
	}
	sr, err := s.client.SearchSource(ctx, sourceURL, doi)
	if err != nil {
		return nil, false, err
	}
	if sr == nil || !sr.Found {
		return nil, false, nil
	}
	pkg, err := s.client.PullSource(ctx, sr.SourceID)
	if err != nil {
		return nil, false, err
	}
	return pkg, true, nil
}

// ListRelevantDecompositions returns the DecompRefs from a source
// package that pass the model whitelist and promptset-acceptance
// rules. Used by the cache adapter to decide which decompositions
// to pull, and by the pull workers to filter the DecompRef list
// before iterating. The actual promptset_hash lives on the
// DecompositionPackage (returned by PullDecomposition), so when
// DecompRef does not carry a promptset_hash the filter is
// permissive (accept) — the per-decomposition check in
// PullRelevantDecomposition is the authoritative guard.
func (s *Service) ListRelevantDecompositions(pkg *SourcePackage, f *RelevanceFilter) []DecompRef {
	if pkg == nil {
		return nil
	}
	out := make([]DecompRef, 0, len(pkg.Decompositions))
	for _, dr := range pkg.Decompositions {
		if !f.AllowsModel(dr.ModelID) {
			continue
		}
		// DecompRef may carry a promptset_hash on newer registries;
		// when absent the per-decomposition check is authoritative.
		if dr.PromptsetHash != "" && !f.AllowsPromptset(dr.PromptsetHash) {
			continue
		}
		out = append(out, dr)
	}
	return out
}

// PullRelevantDecomposition pulls one decomposition and applies the
// full RelevanceFilter: model + promptset guards skip disallowed
// decompositions; SyncLevelFilter strips concept-level fields when
// the repo's pull level is "facts"; the context mapper rewrites
// concept/link contexts (or drops them per the unmapped policy).
//
// Returns (decomp, true, nil) when the decomposition passes the
// filters and is pulled; (nil, false, nil) when it's filtered out
// (model not allowed, promptset not accepted, or 404 from the
// registry); (nil, false, err) on a registry error.
//
// The returned decomposition is already filtered — callers do not
// re-apply the SyncLevelFilter or context mapper. This is the
// single place the rules live, so the four pull paths
// (retrieve_source, pull_all, pull_remote_batch, the cache adapter)
// stay in lockstep.
func (s *Service) PullRelevantDecomposition(ctx context.Context, sourceID, modelID string, f *RelevanceFilter) (*DecompositionPackage, bool, error) {
	if !s.IsConfigured() {
		return nil, false, nil
	}
	if !f.AllowsModel(modelID) {
		return nil, false, nil
	}
	decomp, err := s.client.PullDecomposition(ctx, sourceID, modelID)
	if err != nil {
		return nil, false, err
	}
	if !f.AllowsPromptset(decomp.PromptsetHash) {
		return nil, false, nil
	}
	if f.SyncLevel != nil {
		decomp = f.SyncLevel.FilterForPull(decomp)
	}
	decomp = applyContextFilter(decomp, f)
	return decomp, true, nil
}

// applyContextFilter rewrites the concept_context of each concept
// and link via the inbound context mapper, dropping concepts (and
// their links) that the mapper rejects. This keeps the returned
// decomposition consistent: a caller iterating Concepts/Links sees
// only concepts that survived the unmapped_context_policy. When the
// filter has no mapper, the package is returned unchanged (the
// legacy "import verbatim" behavior).
func applyContextFilter(decomp *DecompositionPackage, f *RelevanceFilter) *DecompositionPackage {
	if f == nil || f.ContextMapper == nil {
		return decomp
	}
	if decomp == nil {
		return decomp
	}
	skipped := map[string]bool{}
	out := make([]ConceptData, 0, len(decomp.Concepts))
	for _, c := range decomp.Concepts {
		localContext, ok := f.MapContext(c.Context)
		if !ok {
			skipped[lowerJoin(c.CanonicalName, c.Context)] = true
			continue
		}
		c.Context = localContext
		out = append(out, c)
	}
	decomp.Concepts = out
	links := make([]FactConceptLink, 0, len(decomp.Links))
	for _, l := range decomp.Links {
		if skipped[lowerJoin(l.ConceptName, l.ConceptContext)] {
			continue
		}
		localContext, ok := f.MapContext(l.ConceptContext)
		if !ok {
			continue
		}
		l.ConceptContext = localContext
		links = append(links, l)
	}
	decomp.Links = links
	return decomp
}

// OutboundContextMapper is the minimal slice of the outbound context
// mapper the Service needs for the contribute (push) path. The
// concrete implementation lives in the tasks package; this interface
// lets the registry package depend on the shape without importing
// tasks (which would create a cycle, since tasks imports registry).
type OutboundContextMapper interface {
	// MapContext returns the registry-side context label for a
	// local one and whether the concept should be contributed.
	// When the second return is false, the caller skips the
	// concept (and any link to it).
	MapContext(localContext string) (string, bool)
}

// ContributeDecomposition pushes a source's decomposition to the
// registry, applying the outbound context mapper first so concepts
// whose local context is unmapped (and absent from the registry
// vocab) are dropped, and the survivors' contexts are rewritten to
// the registry's canonical labels. This is the contribute-side
// counterpart to PullRelevantDecomposition: the rules live in the
// Service so the contribute_source worker doesn't inline them.
//
// The caller still owns building the DecompositionPackage (loading
// facts/concepts/links/embeddings from the local DB + Qdrant) and
// the SyncLevelFilter (stripping concept-level fields when the
// repo's push level is "facts"). This method only applies the
// outbound context mapping and pushes — it does not load or filter
// by model/promptset (those are pull-side concerns).
func (s *Service) ContributeDecomposition(
	ctx context.Context,
	sourceID, modelID string,
	decomp *DecompositionPackage,
	outbound OutboundContextMapper,
) (string, error) {
	if !s.IsConfigured() {
		return "", ErrRegistryDisabled
	}
	if decomp == nil {
		return "", fmt.Errorf("registry: nil decomposition")
	}
	if outbound != nil {
		decomp = applyOutboundContextFilter(decomp, outbound)
	}
	return s.client.PushDecomposition(ctx, sourceID, modelID, decomp)
}

// applyOutboundContextFilter rewrites the context field of each
// concept via the outbound mapper, dropping concepts whose local
// context is unmapped-and-absent. Links to dropped concepts are
// dropped too so the registry doesn't receive dangling
// fact_concept links. The caller passes the outbound mapper from
// the tasks package (OutboundContextMapper); the Service applies it
// so the contribute worker doesn't inline the logic.
func applyOutboundContextFilter(decomp *DecompositionPackage, mapper OutboundContextMapper) *DecompositionPackage {
	if decomp == nil || mapper == nil {
		return decomp
	}
	skipped := map[string]bool{}
	out := make([]ConceptData, 0, len(decomp.Concepts))
	for _, c := range decomp.Concepts {
		mapped, ok := mapper.MapContext(c.Context)
		if !ok {
			skipped[lowerJoin(c.CanonicalName, c.Context)] = true
			continue
		}
		c.Context = mapped
		out = append(out, c)
	}
	decomp.Concepts = out
	links := make([]FactConceptLink, 0, len(decomp.Links))
	for _, l := range decomp.Links {
		if skipped[lowerJoin(l.ConceptName, l.ConceptContext)] {
			continue
		}
		mapped, ok := mapper.MapContext(l.ConceptContext)
		if !ok {
			continue
		}
		l.ConceptContext = mapped
		links = append(links, l)
	}
	decomp.Links = links
	return decomp
}

// lowerJoin is the skipped-concept key: lower(name\x00context).
// Matches the key used by the existing pull workers so the filter
// stays consistent with the legacy code paths.
func lowerJoin(name, context string) string {
	return lowerString(name) + "\x00" + lowerString(context)
}

func lowerString(s string) string {
	out := make([]byte, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		out[i] = c
	}
	return string(out)
}

// ServiceMap is the id → Service map, the search/cache adapter's
// entry point. It wraps a ClientMap and builds Service instances
// lazily, sharing the underlying *Client with the existing workers
// (which still take a *Client directly). A nil ServiceMap is safe
// to call — Service(ctx) returns a disabled Service.
type ServiceMap struct {
	clients *ClientMap
}

// NewServiceMap builds a ServiceMap over an existing ClientMap. The
// ClientMap is the same one constructed in cmd/app/api.go from the
// resolved registries config, so the search/cache adapters see the
// same clients the pull/contribute workers do.
func NewServiceMap(clients *ClientMap) *ServiceMap {
	return &ServiceMap{clients: clients}
}

// Service returns the Service for the registry id carried on ctx
// (via WithRegistryID), falling back to "default" when ctx carries
// no id. Returns a disabled Service (empty baseURL) when no such
// registry is configured, so callers can call IsConfigured() to
// decide whether to proceed.
func (m *ServiceMap) Service(ctx context.Context) *Service {
	if m == nil || m.clients == nil {
		return NewService(nil, config.RegistryConfig{})
	}
	id := RegistryIDFromContext(ctx)
	if id == "" {
		id = "default"
	}
	c, cfg, ok := m.clients.Client(id)
	if !ok {
		return NewService(nil, config.RegistryConfig{})
	}
	return NewService(c, cfg)
}

// IsConfigured reports whether any registry is configured. The nil
// receiver returns false. Used by the search/cache adapters to
// short-circuit when the deployment hasn't wired a registry.
func (m *ServiceMap) IsConfigured() bool {
	return m != nil && m.clients != nil && m.clients.IsConfigured()
}

// registrySearchDefaultPerPage / registrySearchMaxPerPage cap the
// page size the registry search provider forwards to ListSources.
// The registry's SQLite store caps limit at 100; the postgres store
// has no cap. 20 matches OpenAlex's default; 100 is the registry's
// hard cap.
const (
	registrySearchDefaultPerPage = 20
	registrySearchMaxPerPage     = 100
)

// decodeCursorOffset parses a base64-encoded integer offset cursor.
// An empty cursor returns (0, nil). A non-base64 or non-integer
// cursor returns (0, err) so the caller can fall back to the first
// page (friendlier than erroring the whole search).
//
// Exported as DecodeCursorOffset so the search provider can recover
// the current offset to compute the next cursor.
func DecodeCursorOffset(cursor string) (int, error) {
	if cursor == "" {
		return 0, nil
	}
	n, err := decodeBase64Int(cursor)
	if err != nil {
		return 0, err
	}
	if n < 0 {
		return 0, fmt.Errorf("negative offset")
	}
	return n, nil
}

// decodeCursorOffset is the unexported alias kept for internal use.
func decodeCursorOffset(cursor string) (int, error) {
	return DecodeCursorOffset(cursor)
}

// encodeCursorOffset is the inverse of decodeCursorOffset.
func encodeCursorOffset(offset int) string {
	return encodeBase64Int(offset)
}