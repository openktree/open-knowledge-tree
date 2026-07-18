package handler

import (
	"sort"

	"github.com/openktree/open-knowledge-tree/backend/internal/providers/fetch"
	"github.com/openktree/open-knowledge-tree/backend/internal/providers/search"
)

// ProviderKind enumerates the provider categories the per-repository
// settings feature distinguishes. The string values are the
// `provider_kind` column values in repository_provider_settings and
// the keys the UI groups by.
const (
	ProviderKindSearch     = "search"
	ProviderKindResolution = "resolution"
)

// ContentKind enumerates the source content types the per-repository
// allowed_content_types gate distinguishes (migration 0049). The
// string values are the only accepted members of the column's CHECK
// constraint. "document" = uploaded files (UploadSource), "url" =
// web URLs (CreateSource / EnqueueRetrieveSource with a URL),
// "doi" = DOIs (EnqueueRetrieveSource with a DOI). A repo with a
// non-NULL allowed_content_types array 403-rejects any source whose
// classified type is not in the list; NULL = allow all (the default,
// backward compatible for existing repos).
const (
	ContentKindDocument = "document"
	ContentKindURL      = "url"
	ContentKindDOI      = "doi"
)

// ValidContentKinds is the set of accepted content-type strings. Used
// by the SetContentTypes handler to validate PUT .../settings/content-types
// input before persisting. Mirrors the column's CHECK constraint.
var ValidContentKinds = map[string]bool{
	ContentKindDocument: true,
	ContentKindURL:      true,
	ContentKindDOI:      true,
}

// LiveProvider is one entry in the live provider catalog. ID is the
// stable slug (e.g. "serper", "openalex", "fetch", "tls"); Kind is
// "search" or "resolution"; Name is the human label the UI shows.
// The Name mirrors what GET /sources/providers already returns so
// the settings UI can render the same labels without a second
// source of truth.
type LiveProvider struct {
	Kind string
	ID   string
	Name string
}

// ProviderRegistry is the runtime catalog of the providers the
// server is currently wired with. It is the single source of truth
// for which provider ids exist at this deployment: the
// CreateRepository seeding iterates it, the gate intersects stored
// settings with it, and the UI lists it. Nothing about the live set
// is hardcoded — a deployment that doesn't configure Serper simply
// won't have "serper" in the registry, and the settings UI won't
// offer it.
//
// The registry is constructed once at wiring time (cmd/app/api.go)
// from the same maps passed to the Source handler, so the catalog
// the settings feature sees is exactly the catalog the /sources
// endpoints see. Stored rows whose provider_id is not in the live
// registry are silently ignored at enforcement time (the gate
// intersects with LiveProviderIDs); the UI renders them as
// "unavailable" so an admin sees the drift.
type ProviderRegistry struct {
	search     map[string]search.SearchProvider
	strategy   *fetch.FetchStrategy
	searchMeta map[string]string // id -> human name
}

// NewProviderRegistry builds a registry from the live provider maps.
// searchMeta optionally overrides the human name for a search id;
// when nil the registry falls back to the same hard-coded labels the
// /sources/providers endpoint uses (serper → "Serper (Google
// Search)", openalex → "OpenAlex (Academic Works)"). Unknown ids
// get the id itself as the name.
func NewProviderRegistry(searchProviders map[string]search.SearchProvider, strategy *fetch.FetchStrategy) *ProviderRegistry {
	return &ProviderRegistry{
		search: searchProviders,
		strategy: strategy,
		searchMeta: map[string]string{
			"serper":   "Serper (Google Search)",
			"openalex": "OpenAlex (Academic Works)",
			"registry": "OKT Knowledge Registry",
		},
	}
}

// LiveProviders returns the full live catalog, sorted by kind then
// id for a stable UI order. Resolution provider names come from
// Describe(); search provider names come from searchMeta (or the id
// when no metadata is registered for the id).
func (r *ProviderRegistry) LiveProviders() []LiveProvider {
	out := make([]LiveProvider, 0, 16)
	for id := range r.search {
		name := id
		if n, ok := r.searchMeta[id]; ok && n != "" {
			name = n
		}
		out = append(out, LiveProvider{Kind: ProviderKindSearch, ID: id, Name: name})
	}
	if r.strategy != nil {
		for _, p := range r.strategy.Providers() {
			id := fetch.ProviderID(p)
			d := p.Describe()
			name := id
			if d.Name != "" {
				name = d.Name
			}
			out = append(out, LiveProvider{Kind: ProviderKindResolution, ID: id, Name: name})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Kind != out[j].Kind {
			return out[i].Kind < out[j].Kind
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// LiveProviderIDs returns the set of live (kind, id) pairs as a
// map keyed by [2]string{kind, id} for fast membership tests. The
// gate uses this to filter stored settings down to the live set.
func (r *ProviderRegistry) LiveProviderIDs() map[[2]string]bool {
	live := r.LiveProviders()
	m := make(map[[2]string]bool, len(live))
	for _, p := range live {
		m[[2]string{p.Kind, p.ID}] = true
	}
	return m
}

// HasSearchProvider reports whether a given search provider id is
// live. Used by the TestSearch gate.
func (r *ProviderRegistry) HasSearchProvider(id string) bool {
	_, ok := r.search[id]
	return ok
}

// HasResolutionProvider reports whether a given resolution provider
// id is live. Used by the EnqueueRetrieveSource gate.
func (r *ProviderRegistry) HasResolutionProvider(id string) bool {
	if r.strategy == nil {
		return false
	}
	for _, p := range r.strategy.Providers() {
		if fetch.ProviderID(p) == id {
			return true
		}
	}
	return false
}