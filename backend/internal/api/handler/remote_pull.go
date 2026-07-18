package handler

import (
	"context"
	"log"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/openktree/open-knowledge-tree/backend/internal/providers/registry"
	"github.com/openktree/open-knowledge-tree/backend/internal/store"
)

// RemotePullDeps is the dependency bundle passed to
// PullOneRemoteSource. It bundles the registry service (filter-aware
// decomposition pull), the per-repo store, the repo UUID, the
// inbound context mapper (may be nil when context mapping isn't
// configured — the pull then imports contexts verbatim, the legacy
// behavior), the dedup enqueuer (may be nil to skip the embed→dedup
// chain), and the relevance filter (model whitelist, promptset
// acceptance, sync level, context mapping — the single struct the
// registry core applies).
type RemotePullDeps struct {
	// Service is the filter-aware registry core. When nil the pull
	// falls back to the legacy path (Client + manual filtering),
	// preserved for backward compatibility with callers that
	// haven't been migrated.
	Service *registry.Service
	// Client is the raw registry client, used for the source
	// package pull (PullSource) and as the fallback when Service is
	// nil. Kept on the struct so the batch worker doesn't need a
	// separate resolver.
	Client *registry.Client
	// Filter is the per-repo RelevanceFilter the Service applies
	// when pulling decompositions. Nil when the caller wants the
	// legacy "no filter" behavior (the Service then returns all
	// decompositions unfiltered).
	Filter *registry.RelevanceFilter
	Queries       *store.Queries
	SystemQueries *store.Queries
	RepoID        pgtype.UUID
	// Mapper rewrites registry concept contexts to the repo's local
	// vocabulary on pull. Nil = import verbatim (legacy behavior).
	// Deprecated: pass the mapper via Filter.ContextMapper instead —
	// the Service applies it during the pull so the import loop
	// receives already-translated concepts. Kept for the legacy
	// fallback path.
	Mapper RemoteInboundMapper
	// DedupEnqueuer kicks off embed_facts after a successful pull so
	// the imported 'new' facts go through the standard dedup
	// pipeline. Nil = skip (facts stay 'new' until a periodic sweep).
	DedupEnqueuer RemoteDedupEnqueuer
	// PullFilter strips concept-level fields from each pulled
	// decomposition based on the repo's registry_pull_level. Nil
	// defaults to full "concepts" pull (the legacy behavior). Set by
	// the caller from GetRepositorySyncLevels.
	// Deprecated: pass the filter via Filter.SyncLevel instead — the
	// Service applies it during the pull. Kept for the legacy
	// fallback path.
	PullFilter *registry.SyncLevelFilter
}

// RemoteInboundMapper is the minimal slice of the inbound context
// mapper the pull core needs. The concrete implementation lives in
// the tasks package (InboundContextMapper); this interface lets the
// handler package depend on the shape without importing tasks (which
// would create a cycle).
type RemoteInboundMapper interface {
	// MapContext returns the local context label for a registry
	// context and whether the concept should be imported. When the
	// second return is false, the caller skips the concept (and any
	// link to it). autoAdd is invoked when the policy is auto_add.
	MapContext(registryContext string, autoAdd func(string)) (string, bool)
}

// PullOneRemoteSource imports a single source (with its facts and
// concepts) from the remote registry into the local repository. It's
// the shared core between the synchronous POST /remote/{id}/pull
// handler and the async pull_remote_batch worker. Returns the local
// source ID + import counts so callers can report progress.
//
// The pull is idempotent: re-pulling a source whose URL/DOI already
// exists locally reuses the existing source row and appends facts
// (CreateFact is idempotent on content hash, and AddFactSource is
// idempotent on (fact_id, source_id)). Re-pulls are how the UI's
// "Re-sync" button refreshes a source with new decompositions pushed
// to the registry since the last pull.
func PullOneRemoteSource(ctx context.Context, deps RemotePullDeps, remoteID string) (PullResult, error) {
	pkg, err := deps.Client.PullSource(ctx, remoteID)
	if err != nil {
		return PullResult{}, err
	}

	displayURL := pkg.Source.URL
	if displayURL == "" && pkg.Source.DOI != "" {
		displayURL = "https://doi.org/" + pkg.Source.DOI
	}
	urlVal := displayURL
	if urlVal == "" {
		urlVal = pkg.Source.URL
	}
	var doi *string
	if pkg.Source.DOI != "" {
		d := pkg.Source.DOI
		doi = &d
	}
	title := pkg.Source.Title

	srcID := pgtype.UUID{}
	if err := srcID.Scan(uuid.New().String()); err != nil {
		return PullResult{}, err
	}
	_, insertErr := deps.Queries.CreateSource(ctx, store.CreateSourceParams{
		ID:           srcID,
		RepositoryID: deps.RepoID,
		Url:          urlVal,
		Kind:         "url",
		Status:       "fetching",
		Doi:          doi,
	})
	if insertErr != nil {
		existing, listErr := deps.Queries.GetSourceByRepoAndURL(ctx, store.GetSourceByRepoAndURLParams{
			RepositoryID: deps.RepoID,
			Url:          urlVal,
		})
		if listErr != nil {
			return PullResult{}, insertErr
		}
		srcID = existing.ID
	}

	// Import the parsed content from the registry so the local
	// source has the extracted text + markdown without needing to
	// re-fetch the URL. The registry stores these at push time
	// (contribute_source worker sends parsed_text + parsed_markdown).
	var parsedText, parsedMarkdown *string
	if pkg.Content.Text != "" {
		t := pkg.Content.Text
		parsedText = &t
	}
	if pkg.Content.Markdown != "" {
		m := pkg.Content.Markdown
		parsedMarkdown = &m
	}

	if _, err := deps.Queries.MarkSourceFetched(ctx, store.MarkSourceFetchedParams{
		ID:      srcID,
		Content: nil,
	}); err != nil {
		return PullResult{}, err
	}
	if _, parseErr := deps.Queries.MarkSourceParsed(ctx, store.MarkSourceParsedParams{
		ID:             srcID,
		ParsedTitle:    &title,
		ParsedText:     parsedText,
		ParsedMarkdown: parsedMarkdown,
		ParseStatus:    strPtr("ok"),
	}); parseErr != nil {
		log.Printf("remote: marking source parsed: %v", parseErr)
	}

	importedFacts := 0
	importedConcepts := 0

	// autoAdd seeds a repository_contexts row for the auto_add
	// policy. It needs the system pool (repository_contexts lives in
	// okt_system), so it's only wired when SystemQueries is set.
	// When the Service is wired, the filter's AutoAdd callback is
	// preferred (set by the caller); this one is the fallback for
	// the legacy path.
	autoAdd := func(registryLabel string) {
		if deps.SystemQueries == nil {
			return
		}
		if _, err := deps.SystemQueries.SeedRepositoryContext(ctx, store.SeedRepositoryContextParams{
			RepositoryID: deps.RepoID,
			Context:      registryLabel,
			IsCustom:     true,
			Description:  "",
		}); err != nil {
			log.Printf("remote: auto-adding context %q: %v", registryLabel, err)
		}
	}

	// Build the decomposition list to pull. When a Service + Filter
	// is wired, use the filter-aware path (the Service applies the
	// model whitelist, promptset acceptance, sync level, and
	// context mapping). Otherwise fall back to the legacy path
	// (Client.IsAllowedModel + manual filtering) so callers that
	// haven't been migrated keep working.
	var decompRefs []registry.DecompRef
	if deps.Service != nil && deps.Filter != nil {
		decompRefs = deps.Service.ListRelevantDecompositions(pkg, deps.Filter)
	} else {
		for _, dr := range pkg.Decompositions {
			if deps.Client != nil && !deps.Client.IsAllowedModel(dr.ModelID) {
				continue
			}
			decompRefs = append(decompRefs, dr)
		}
	}

	for _, dr := range decompRefs {
		var decomp *registry.DecompositionPackage
		if deps.Service != nil && deps.Filter != nil {
			d, ok, err := deps.Service.PullRelevantDecomposition(ctx, pkg.Source.ID, dr.ModelID, deps.Filter)
			if err != nil {
				log.Printf("remote: pulling decomposition %s: %v", dr.ModelID, err)
				continue
			}
			if !ok {
				continue
			}
			decomp = d
		} else {
			d, err := deps.Client.PullDecomposition(ctx, pkg.Source.ID, dr.ModelID)
			if err != nil {
				log.Printf("remote: pulling decomposition %s: %v", dr.ModelID, err)
				continue
			}
			if deps.PullFilter != nil {
				d = deps.PullFilter.FilterForPull(d)
			}
			decomp = d
		}

		// Track fact content_hash → local fact_id for link resolution.
		factIDByHash := make(map[string]pgtype.UUID, len(decomp.Facts))
		// Track skipped (name, registryContext) pairs so the link
		// loop drops links to skipped concepts. Only populated on
		// the legacy path; the Service path already dropped them.
		skippedConcepts := map[string]bool{}

		for _, f := range decomp.Facts {
			factID := pgtype.UUID{}
			if err := factID.Scan(uuid.New().String()); err != nil {
				continue
			}
			factKind := "text"
			if f.ImageURL != "" {
				factKind = "image"
			}
			if _, err := deps.Queries.CreateFact(ctx, store.CreateFactParams{
				ID:       factID,
				Text:     f.Content,
				FactKind: factKind,
				ImageUrl: strPtrOrNil(f.ImageURL),
			}); err != nil {
				continue
			}
			if f.ContentHash != "" {
				factIDByHash[f.ContentHash] = factID
			}
			if err := deps.Queries.AddFactSource(ctx, store.AddFactSourceParams{
				FactID:     factID,
				SourceID:   srcID,
				ChunkIndex: int32(f.SentenceIdx),
			}); err != nil {
				continue
			}
			importedFacts++
		}

		for _, c := range decomp.Concepts {
			if c.CanonicalName == "" {
				continue
			}
			// When the Service is wired, c.Context is already the
			// local context (the filter translated it). Otherwise
			// apply the legacy mapper.
			localContext := c.Context
			if deps.Service == nil || deps.Filter == nil || deps.Filter.ContextMapper == nil {
				var ok bool
				localContext, ok = applyInboundContext(deps.Mapper, c.Context, autoAdd)
				if !ok {
					skippedConcepts[lowerJoin(c.CanonicalName, c.Context)] = true
					continue
				}
			}
			desc := strPtrOrNil(localContext)
			if _, err := deps.Queries.CreateConcept(ctx, store.CreateConceptParams{
				RepositoryID:  deps.RepoID,
				CanonicalName: c.CanonicalName,
				Context:       localContext,
				Description:   desc,
			}); err != nil {
				continue
			}
			importedConcepts++
		}

		// Import fact_concept links. The link's concept_context is
		// translated via the inbound mapper; links to skipped
		// concepts are dropped. On the Service path the filter
		// already translated + dropped, so this loop persists
		// directly.
		for _, link := range decomp.Links {
			factID, ok := factIDByHash[link.FactContentHash]
			if !ok {
				continue
			}
			localContext := link.ConceptContext
			if deps.Service == nil || deps.Filter == nil || deps.Filter.ContextMapper == nil {
				if skippedConcepts[lowerJoin(link.ConceptName, link.ConceptContext)] {
					continue
				}
				var ok2 bool
				localContext, ok2 = applyInboundContext(deps.Mapper, link.ConceptContext, autoAdd)
				if !ok2 {
					continue
				}
			}
			concept, err := deps.Queries.GetConceptByNameContext(ctx, store.GetConceptByNameContextParams{
				RepositoryID:  deps.RepoID,
				CanonicalName: link.ConceptName,
				Context:       localContext,
			})
			if err != nil {
				continue
			}
			if _, err := deps.Queries.AddFactConcept(ctx, store.AddFactConceptParams{
				FactID:    factID,
				ConceptID: concept.ID,
			}); err != nil {
				continue
			}
		}
	}

	if importedFacts > 0 && deps.DedupEnqueuer != nil {
		if err := deps.DedupEnqueuer.EnqueueEmbedFacts(ctx, uuidFromPgtype(deps.RepoID), uuidFromPgtype(srcID)); err != nil {
			log.Printf("remote: enqueueing embed_facts for pulled source: %v", err)
		}
	}

	return PullResult{
		SourceID:         uuidFromPgtype(srcID),
		Title:            title,
		URL:              urlVal,
		DOI:              doi,
		ImportedFacts:    importedFacts,
		ImportedConcepts: importedConcepts,
	}, nil
}

// PullResult is the outcome of a single remote-source pull, returned
// to both the HTTP handler (which JSON-encodes it to the client) and
// the batch worker (which aggregates across the batch).
type PullResult struct {
	SourceID         string  `json:"source_id"`
	Title            string  `json:"title"`
	URL              string  `json:"url"`
	DOI              *string `json:"doi"`
	ImportedFacts    int     `json:"imported_facts"`
	ImportedConcepts int     `json:"imported_concepts"`
}

// applyInboundContext routes a registry context through the mapper
// when one is configured; a nil mapper imports verbatim (the legacy
// behavior before context mapping shipped).
func applyInboundContext(mapper RemoteInboundMapper, registryContext string, autoAdd func(string)) (string, bool) {
	if mapper == nil {
		return registryContext, true
	}
	return mapper.MapContext(registryContext, autoAdd)
}

// lowerJoin is the skipped-concept key: lower(name\x00context).
// Matches the key used by the pull_all_from_registry worker so the
// two paths stay consistent.
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