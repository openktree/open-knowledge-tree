package registry

import (
	"context"
)

// CacheHit is the outcome of a successful cache lookup: the source
// package (with parsed content + the list of available
// decompositions) and the already-filtered decompositions the repo
// is allowed to import. Callers iterate Decompositions to persist
// facts/concepts/links/embeddings — the model whitelist, promptset
// acceptance, sync-level, and context-mapping rules have already
// been applied by Service.PullRelevantDecomposition, so the import
// loops don't re-filter.
type CacheHit struct {
	Source         *SourcePackage
	Decompositions []DecompositionPackage
}

// RegistryCacheProvider is the cache-hit adapter backed by the
// registry. It is NOT a fetch.ResolutionProvider — it doesn't join
// the http_fetch / unpaywall / tls / flaresolverr chain. It is the
// same cache-hit shortcut the retrieve_source / pull workers used
// to inline (SearchSource → PullSource → PullDecomposition + filter),
// lifted into a named type so the workers can call one method
// instead of duplicating the logic.
//
// The provider is the only cache provider that exists today. It is
// active whenever the repo's registry_enabled flag is true — the
// per-repo gate the workers already check before calling
// LookupAndPull. There is no per-repo toggle in
// repository_provider_settings because this is not a resolution
// provider; the registry_enabled flag is the gate.
type RegistryCacheProvider struct {
	services *ServiceMap
}

// NewRegistryCacheProvider builds a cache provider over a ServiceMap.
// The ServiceMap is the same one constructed in cmd/app/api.go from
// the resolved registries config, so the cache provider sees the
// same clients the pull/contribute workers do.
func NewRegistryCacheProvider(services *ServiceMap) *RegistryCacheProvider {
	return &RegistryCacheProvider{services: services}
}

// IsConfigured reports whether the underlying ServiceMap has any
// configured registry. The nil receiver returns false. Used by the
// pull workers to short-circuit the cache lookup when the
// deployment hasn't wired a registry.
func (p *RegistryCacheProvider) IsConfigured() bool {
	return p != nil && p.services != nil && p.services.IsConfigured()
}

// LookupAndPull checks the registry cache for a source matching the
// given URL or DOI. On a hit, it pulls the source package, filters
// the available decompositions by the RelevanceFilter, and pulls
// each relevant decomposition (already filtered). On a miss, it
// returns (nil, false, nil) so the caller falls through to the
// normal fetch + extract pipeline.
//
// The pull is best-effort per decomposition: a registry error on
// one model is logged and skipped (the caller still gets the
// decompositions that succeeded), mirroring the existing workers'
// posture. A filtered-out decomposition (model not allowed,
// promptset not accepted) is silently skipped — it's not an error.
//
// The returned CacheHit.Decompositions are already filtered by the
// RelevanceFilter (model, promptset, sync-level, context mapping).
// Callers do not re-apply any filter — they persist the facts,
// concepts, links, and embeddings directly.
func (p *RegistryCacheProvider) LookupAndPull(ctx context.Context, sourceURL, doi string, f *RelevanceFilter) (*CacheHit, bool, error) {
	if p == nil || p.services == nil || !p.services.IsConfigured() {
		return nil, false, nil
	}
	svc := p.services.Service(ctx)
	pkg, found, err := svc.LookupCacheSource(ctx, sourceURL, doi)
	if err != nil {
		return nil, false, err
	}
	if !found {
		return nil, false, nil
	}
	relevant := svc.ListRelevantDecompositions(pkg, f)
	decs := make([]DecompositionPackage, 0, len(relevant))
	for _, dr := range relevant {
		decomp, ok, err := svc.PullRelevantDecomposition(ctx, pkg.Source.ID, dr.ModelID, f)
		if err != nil {
			// Best-effort: log + skip, matching the existing workers.
			continue
		}
		if !ok {
			continue
		}
		decs = append(decs, *decomp)
	}
	return &CacheHit{Source: pkg, Decompositions: decs}, true, nil
}