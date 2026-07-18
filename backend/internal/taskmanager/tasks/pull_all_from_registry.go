package tasks

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/openktree/open-knowledge-tree/backend/internal/api/handler"
	"github.com/openktree/open-knowledge-tree/backend/internal/dbpool"
	"github.com/openktree/open-knowledge-tree/backend/internal/providers/registry"
	"github.com/openktree/open-knowledge-tree/backend/internal/qdrantstore"
	"github.com/openktree/open-knowledge-tree/backend/internal/store"
	"github.com/riverqueue/river"
)

const QueuePullAllFromRegistry = "pull_all_from_registry"

type PullAllFromRegistryArgs struct {
	RepositoryID string `json:"repository_id"`
}

func (PullAllFromRegistryArgs) Kind() string { return "pull_all_from_registry" }

func (PullAllFromRegistryArgs) InsertOpts() river.InsertOpts { return river.InsertOpts{} }

type PullAllFromRegistryResult struct {
	RepositoryID string `json:"repository_id"`
	Checked      int    `json:"checked"`
	Imported     int    `json:"imported"`
}

type PullAllFromRegistryWorker struct {
	river.WorkerDefaults[PullAllFromRegistryArgs]

	registryClients   *registry.ClientMap
	registryCache     *registry.RegistryCacheProvider
	registry          *dbpool.Registry
	systemQueries     *store.Queries
	qdrant            *qdrantstore.Store
	reconciler        *CacheReconciler
	dedupEnqueuer     handler.RemoteDedupEnqueuer
	promptsetResolver *PromptsetResolver
}

func NewPullAllFromRegistryWorker(
	registryClients *registry.ClientMap,
	registryCache *registry.RegistryCacheProvider,
	poolRegistry *dbpool.Registry,
	systemQueries *store.Queries,
	qdrant *qdrantstore.Store,
	reconciler *CacheReconciler,
	dedupEnqueuer handler.RemoteDedupEnqueuer,
	promptsetResolver *PromptsetResolver,
) *PullAllFromRegistryWorker {
	return &PullAllFromRegistryWorker{
		registryClients:   registryClients,
		registryCache:     registryCache,
		registry:          poolRegistry,
		systemQueries:     systemQueries,
		qdrant:            qdrant,
		reconciler:        reconciler,
		dedupEnqueuer:     dedupEnqueuer,
		promptsetResolver: promptsetResolver,
	}
}

func (w *PullAllFromRegistryWorker) Work(ctx context.Context, job *river.Job[PullAllFromRegistryArgs]) error {
	args := job.Args
	if args.RepositoryID == "" {
		return fmt.Errorf("pull_all_from_registry: repository_id is required")
	}

	repoID := pgtype.UUID{}
	if err := repoID.Scan(args.RepositoryID); err != nil {
		return fmt.Errorf("pull_all_from_registry: invalid repository_id: %w", err)
	}

	// Per-repo gate: no-op when the integration is off or the
	// configured registry is gone. The HTTP gate already rejects
	// the enqueue; this is defense-in-depth for a job enqueued
	// before a toggle-off.
	regID, rc, err := resolveRepoRegistryClient(ctx, w.systemQueries, w.registryClients, repoID)
	if err != nil {
		logSkip("pull_all_from_registry", args.RepositoryID, err.Error())
		return river.RecordOutput(ctx, &PullAllFromRegistryResult{
			RepositoryID: args.RepositoryID,
		})
	}

	dbName, err := w.systemQueries.GetRepositoryDatabaseName(ctx, repoID)
	if err != nil {
		return fmt.Errorf("pull_all_from_registry: resolving repository database: %w", err)
	}
	pool := w.registry.Get(dbName)
	if pool == nil || pool.Pool == nil {
		return fmt.Errorf("pull_all_from_registry: no pool for database %q", dbName)
	}
	queries := store.New(pool.Pool)

	// Resolve the repo's effective promptset hash. Imported facts
	// and concepts are tagged with this hash so they join the repo's
	// active philosophy. (A future step can tag with the registry
	// decomposition's own promptset_hash once the wire format
	// carries it; for now the pull is gated on accepted hashes, so
	// the imported decomposition is by definition from an accepted
	// philosophy.)
	var psHashPtr *string
	var acceptedHashes []string
	if w.promptsetResolver != nil {
		h := w.promptsetResolver.EffectiveHash(ctx, repoID)
		psHashPtr = &h
		acceptedHashes = w.promptsetResolver.AcceptedHashes(ctx, repoID)
	}

	// Build the inbound context mapper once per repo. The mapper
	// translates registry concept contexts to the repo's local
	// vocabulary and applies the unmapped_context_policy (skip |
	// auto_add | catch_all). tryPullOne threads it through the
	// concept + link import loops.
	mapper, err := NewInboundContextMapper(ctx, w.systemQueries, repoID)
	if err != nil {
		return fmt.Errorf("pull_all_from_registry: building context mapper: %w", err)
	}

	// Per-repo pull level (migration 0044). Controls whether the
	// import includes concepts/links/concept-embedments or only
	// sources + facts + fact embeddings. Defaults to "concepts"
	// (full pull). The SyncLevelFilter strips concept-level fields
	// from each pulled decomposition so the import loops no-op them.
	syncLevels, err := w.systemQueries.GetRepositorySyncLevels(ctx, repoID)
	if err != nil {
		return fmt.Errorf("pull_all_from_registry: reading sync levels: %w", err)
	}
	pullFilter := registry.NewSyncLevelFilter(registry.ParseSyncLevel(syncLevels.RegistryPullLevel))

	sources, err := w.listAllSources(ctx, pool.Pool, repoID)
	if err != nil {
		return fmt.Errorf("pull_all_from_registry: listing sources: %w", err)
	}

	// Build a local lookup set by URL and DOI so we can skip
	// registry sources that already exist in this repo.
	localURLs := make(map[string]bool, len(sources))
	localDOIs := make(map[string]bool, len(sources))
	for _, src := range sources {
		if src.Url != "" {
			localURLs[src.Url] = true
		}
		if src.Doi != nil && *src.Doi != "" {
			localDOIs[*src.Doi] = true
		}
	}

	checked := 0
	imported := 0
	aggregateStats := ImportStats{}
	// Collect the source IDs that produced facts so the reconciler
	// can fan out one embed_facts job per source on a re-embed plan
	// (embed_facts is source-bounded now).
	var importedSourceIDs []string

	// Phase 1: Import registry sources that don't exist locally.
	// Paginate through every source in the registry and pull each
	// one that's not already in the repo. This lets a freshly-reset
	// repo bootstrap from the registry without needing the Remote
	// Sources UI to pull each source manually.
	// The registry's SQLite store caps limit at 100; the postgres
	// store has no cap. Use 100 so we paginate efficiently without
	// triggering the SQLite fallback-to-20 behavior.
	const listBatchSize = 100
	for offset := 0; ; {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("pull_all_from_registry: ctx cancelled: %w", err)
		}
		listCtx, listCancel := context.WithTimeout(ctx, 60*time.Second)
		listResp, err := rc.ListSources(listCtx, listBatchSize, offset, "")
		listCancel()
		if err != nil {
			log.Printf("pull_all_from_registry: listing registry sources at offset %d: %v", offset, err)
			break
		}
		if len(listResp.Sources) == 0 {
			break
		}
		for _, rs := range listResp.Sources {
			if err := ctx.Err(); err != nil {
				return fmt.Errorf("pull_all_from_registry: ctx cancelled: %w", err)
			}
			// Skip sources that already exist locally.
			if rs.URL != "" && localURLs[rs.URL] {
				continue
			}
			if rs.DOI != "" && localDOIs[rs.DOI] {
				continue
			}
			checked++
			pr, err := handler.PullOneRemoteSource(ctx, handler.RemotePullDeps{
				Client:        rc,
				Queries:       queries,
				SystemQueries: w.systemQueries,
				RepoID:        repoID,
				Mapper:        mapper,
				DedupEnqueuer: w.dedupEnqueuer,
				PullFilter:    pullFilter,
			}, rs.ID)
			if err != nil {
				log.Printf("pull_all_from_registry: importing source %s (url=%q): %v", rs.ID, rs.URL, err)
				continue
			}
			imported++
			aggregateStats.Created += pr.ImportedFacts
			if pr.ImportedFacts > 0 && pr.SourceID != "" {
				importedSourceIDs = append(importedSourceIDs, pr.SourceID)
			}
		}
		offset += len(listResp.Sources)
		if len(listResp.Sources) < listBatchSize {
			break
		}
	}

	// Phase 2: Delta-check existing local sources against the
	// registry for new decompositions pushed since the last pull.
	for _, src := range sources {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("pull_all_from_registry: ctx cancelled: %w", err)
		}
		sourceURL := src.Url
		var sourceDOI string
		if src.Doi != nil {
			sourceDOI = *src.Doi
		}

		if sourceURL == "" && sourceDOI == "" {
			continue
		}
		checked++

		srcFull, err := w.loadFullSource(ctx, queries, src.ID)
		if err != nil {
			log.Printf("pull_all_from_registry: loading source %s: %v", uuidFromPgtype(src.ID), err)
			continue
		}
		srcStats, err := w.tryPullOne(ctx, queries, *srcFull, args.RepositoryID, regID, rc, mapper, pullFilter, psHashPtr, acceptedHashes)
		if err != nil {
			log.Printf("pull_all_from_registry: pulling source %s (url=%q): %v", uuidFromPgtype(src.ID), sourceURL, err)
			continue
		}
		aggregateStats.Created += srcStats.Created
		aggregateStats.Skipped += srcStats.Skipped
		aggregateStats.ImportedEmbModels = append(aggregateStats.ImportedEmbModels, srcStats.ImportedEmbModels...)
		aggregateStats.ImportedEmbDims = append(aggregateStats.ImportedEmbDims, srcStats.ImportedEmbDims...)
		if srcStats.Created > 0 {
			importedSourceIDs = append(importedSourceIDs, uuidFromPgtype(src.ID))
		}
	}

	// Delta-aware reconciliation: the CacheReconciler decides
	// whether to enqueue downstream jobs. An already-synced repo
	// (all facts skipped) produces an empty plan and triggers
	// zero jobs. Summarize is NOT enqueued directly — it's
	// transitive via dedup → extract_concepts → summarize.
	plan := w.reconciler.Plan(aggregateStats)
	if plan.ReembedFacts {
		w.reconciler.ResetForReembed(ctx, queries, repoID)
	}
	// pull_all is repo-wide; fan out per-source embeds using the
	// collected source IDs (embed_facts is source-bounded now).
	EnqueuePlan(ctx, plan, args.RepositoryID, importedSourceIDs)

	log.Printf("pull_all_from_registry: repo %s imported=%d new sources, checked=%d existing, created=%d facts, skipped=%d facts",
		args.RepositoryID, imported, checked, aggregateStats.Created, aggregateStats.Skipped)
	return river.RecordOutput(ctx, &PullAllFromRegistryResult{
		RepositoryID: args.RepositoryID,
		Checked:      checked,
		Imported:     aggregateStats.Created,
	})
}

func (w *PullAllFromRegistryWorker) tryPullOne(
	ctx context.Context,
	queries *store.Queries,
	src store.OktRepositorySource,
	repoIDStr string,
	regID string,
	rc *registry.Client,
	mapper *InboundContextMapper,
	pullFilter *registry.SyncLevelFilter,
	psHashPtr *string,
	acceptedHashes []string,
) (ImportStats, error) {
	var stats ImportStats
	sourceURL := src.Url
	var sourceDOI string
	if src.Doi != nil {
		sourceDOI = *src.Doi
	}

	var repoID pgtype.UUID
	if err := repoID.Scan(repoIDStr); err != nil {
		return stats, fmt.Errorf("invalid repository id: %w", err)
	}

	// Build the per-repo RelevanceFilter the cache provider applies.
	// The context mapper translates registry concept contexts to
	// the repo's local vocabulary; the Service's applyContextFilter
	// rewrites concept/link contexts (or drops them per the
	// unmapped policy) so the import loop below uses c.Context
	// directly — no re-mapping.
	autoAdd := func(registryLabel string) {
		if _, err := w.systemQueries.SeedRepositoryContext(ctx, store.SeedRepositoryContextParams{
			RepositoryID: repoID,
			Context:      registryLabel,
			IsCustom:     true,
			Description:  "",
		}); err != nil {
			log.Printf("pull_all_from_registry: auto-adding context %q: %v", registryLabel, err)
		}
	}
	filter := &registry.RelevanceFilter{
		AllowedModels:      resolveAllowedModels(ctx, w.systemQueries, repoID, rc.AllowedModels()),
		AcceptedPromptsets: acceptedHashes,
		SyncLevel:          pullFilter,
		ContextMapper:      mapper,
		AutoAdd:            autoAdd,
	}

	// Inject the active registry id into the context so the
	// ServiceMap resolves the right client.
	cacheCtx := registry.WithRegistryID(ctx, regID)
	hit, found, err := w.registryCache.LookupAndPull(cacheCtx, sourceURL, sourceDOI, filter)
	if err != nil {
		return stats, fmt.Errorf("registry cache lookup: %w", err)
	}
	if !found || hit == nil {
		return stats, nil
	}

	// The decompositions in the hit are already filtered (model
	// whitelist, promptset acceptance, sync level, context
	// mapping). Iterate and persist — no re-filtering here.
	for i := range hit.Decompositions {
		decomp := &hit.Decompositions[i]

		// Track fact content_hash → local fact_id for link resolution
		// and Qdrant point mapping (local UUIDs, not remote).
		factIDByHash := make(map[string]pgtype.UUID, len(decomp.Facts))
		conceptIDByKey := make(map[string]pgtype.UUID, len(decomp.Concepts))
		localUUIDByEmbKey := make(map[string]pgtype.UUID)
		var decompEmbModel string
		var decompEmbDims int
		if decomp.Embeddings != nil {
			decompEmbModel = decomp.Embeddings.Model
			decompEmbDims = decomp.Embeddings.Dimensions
		}

		// Import facts + link to source. Delta-aware: skip facts
		// whose exact text is already linked to this source.
		for _, f := range decomp.Facts {
			existing, err := queries.GetFactByTextAndSource(ctx, store.GetFactByTextAndSourceParams{
				Text:     f.Content,
				SourceID: src.ID,
			})
			if err == nil {
				if f.ContentHash != "" {
					factIDByHash[f.ContentHash] = existing.ID
				}
				stats.Skipped++
				continue
			}

			factID := pgtype.UUID{}
			if err := factID.Scan(uuid.New().String()); err != nil {
				log.Printf("pull_all_from_registry: generating fact id: %v", err)
				continue
			}
			factKind := "text"
			if f.ImageURL != "" {
				factKind = "image"
			}
			if _, err := queries.CreateFact(ctx, store.CreateFactParams{
				ID:            factID,
				Text:          f.Content,
				FactKind:      factKind,
				ImageUrl:      strPtrOrNil(f.ImageURL),
				PromptsetHash: psHashPtr,
			}); err != nil {
				log.Printf("pull_all_from_registry: creating fact from registry: %v", err)
				continue
			}
			if f.ContentHash != "" {
				factIDByHash[f.ContentHash] = factID
			}
			if err := queries.AddFactSource(ctx, store.AddFactSourceParams{
				FactID:     factID,
				SourceID:   src.ID,
				ChunkIndex: int32(f.SentenceIdx),
			}); err != nil {
				log.Printf("pull_all_from_registry: linking fact to source: %v", err)
			}
			stats.Created++
		}

		// Import concepts + aliases. The Service's
		// applyContextFilter already translated c.Context to the
		// repo's local vocabulary (and dropped concepts whose
		// context maps to skip), so this loop persists directly —
		// no per-concept mapper call.
		for _, c := range decomp.Concepts {
			if c.CanonicalName == "" {
				continue
			}
			// c.Context is already the local context (the filter
			// translated it). auto_add was already invoked inside
			// the filter for unmapped-but-accepted contexts, so the
			// repository_contexts row exists by now.
			localContext := c.Context
			desc := strPtrOrNil(localContext)
			if _, err := queries.CreateConcept(ctx, store.CreateConceptParams{
				RepositoryID:  repoID,
				CanonicalName: c.CanonicalName,
				Context:       localContext,
				Description:   desc,
				PromptsetHash: psHashPtr,
			}); err != nil {
				log.Printf("pull_all_from_registry: creating concept from registry: %v", err)
				continue
			}
			concept, err := queries.GetConceptByNameContext(ctx, store.GetConceptByNameContextParams{
				RepositoryID:  repoID,
				CanonicalName: c.CanonicalName,
				Context:       localContext,
			})
			if err != nil {
				log.Printf("pull_all_from_registry: resolving concept %q/%q: %v", c.CanonicalName, localContext, err)
				continue
			}
			conceptKey := c.CanonicalName + "\x00" + localContext
			conceptIDByKey[conceptKey] = concept.ID
			// Registry-imported concepts come pre-refined. Mark them
			// so refine_concepts skips them.
			if err := queries.SetConceptRefinedAt(ctx, concept.ID); err != nil {
				log.Printf("pull_all_from_registry: setting refined_at for concept %s: %v", pgUUIDToString(concept.ID), err)
			}
			for _, alias := range c.Aliases {
				if alias == "" {
					continue
				}
				if _, err := queries.AddConceptAlias(ctx, store.AddConceptAliasParams{
					ConceptID: concept.ID,
					AliasText: alias,
				}); err != nil && !errors.Is(err, pgx.ErrNoRows) {
					log.Printf("pull_all_from_registry: adding alias %q for concept %s: %v", alias, pgUUIDToString(concept.ID), err)
				}
			}
		}

		// Import fact_concept links. The Service's
		// applyContextFilter already translated link.ConceptContext
		// to the local vocabulary and dropped links to skipped
		// concepts, so this loop persists directly.
		for _, link := range decomp.Links {
			factID, ok := factIDByHash[link.FactContentHash]
			if !ok {
				continue
			}
			concept, err := queries.GetConceptByNameContext(ctx, store.GetConceptByNameContextParams{
				RepositoryID:  repoID,
				CanonicalName: link.ConceptName,
				Context:       link.ConceptContext,
			})
			if err != nil {
				log.Printf("pull_all_from_registry: resolving concept for link %q/%q: %v", link.ConceptName, link.ConceptContext, err)
				continue
			}
			if _, err := queries.AddFactConcept(ctx, store.AddFactConceptParams{
				FactID:        factID,
				ConceptID:     concept.ID,
				PromptsetHash: psHashPtr,
			}); err != nil && !errors.Is(err, pgx.ErrNoRows) {
				log.Printf("pull_all_from_registry: adding fact_concept link: %v", err)
			}
		}

		// Resolve embedding keys to local UUIDs. The push path
		// keys fact embeddings by "fact:<content_hash>" so we can
		// match via factIDByHash. Concept embeddings are keyed by
		// "concept:<uuid>" and matched best-effort.
		if decomp.Embeddings != nil {
			for embKey := range decomp.Embeddings.Vectors {
				parts := strings.SplitN(embKey, ":", 2)
				if len(parts) == 2 && parts[0] == "fact" {
					if fID, ok := factIDByHash[parts[1]]; ok {
						localUUIDByEmbKey[embKey] = fID
					}
				}
			}
		}

		// Import embeddings into Qdrant using LOCAL UUIDs.
		if w.qdrant != nil && decomp.Embeddings != nil {
			var factPoints []qdrantstore.FactPoint
			var conceptPoints []qdrantstore.ConceptPoint
			for embKey, values := range decomp.Embeddings.Vectors {
				localID, ok := localUUIDByEmbKey[embKey]
				if !ok {
					continue
				}
				localUUID, err := uuid.Parse(pgUUIDToString(localID))
				if err != nil {
					continue
				}
				vec := make([]float32, len(values))
				for i, v := range values {
					vec[i] = float32(v)
				}
				parts := strings.SplitN(embKey, ":", 2)
				switch parts[0] {
				case "fact":
					factPoints = append(factPoints, qdrantstore.FactPoint{
						ID:           localUUID,
						Vector:       vec,
						RepositoryID: pgtypeToUUID(repoID),
						Status:       "new",
					})
				case "concept":
					conceptPoints = append(conceptPoints, qdrantstore.ConceptPoint{
						ID:           localUUID,
						Vector:       vec,
						RepositoryID: pgtypeToUUID(repoID),
					})
				}
			}
			if len(factPoints) > 0 {
				if err := w.qdrant.UpsertFactVectors(ctx, factPoints); err != nil {
					log.Printf("pull_all_from_registry: upserting fact vectors: %v", err)
				}
			}
			if len(conceptPoints) > 0 {
				if err := w.qdrant.UpsertConceptVectors(ctx, conceptPoints); err != nil {
					log.Printf("pull_all_from_registry: upserting concept vectors: %v", err)
				}
			}
			// Mark facts and concepts as embedded. Use the actual
			// embedding model (emb.Model), not the generation model.
			embModelPtr := strPtrOrNil(decompEmbModel)
			for _, f := range decomp.Facts {
				if fID, ok := factIDByHash[f.ContentHash]; ok {
					if _, err := queries.MarkFactEmbedded(ctx, store.MarkFactEmbeddedParams{
						ID:            fID,
						EmbeddedModel: embModelPtr,
					}); err != nil {
						log.Printf("pull_all_from_registry: marking fact embedded: %v", err)
					}
				}
			}
			for _, c := range decomp.Concepts {
				if c.CanonicalName == "" {
					continue
				}
				conceptKey := c.CanonicalName + "\x00" + c.Context
				conceptID, ok := conceptIDByKey[conceptKey]
				if !ok {
					continue
				}
				if _, err := queries.MarkConceptEmbedded(ctx, store.MarkConceptEmbeddedParams{
					ID:            conceptID,
					EmbeddedModel: embModelPtr,
				}); err != nil {
					log.Printf("pull_all_from_registry: marking concept embedded: %v", err)
				}
			}
		}
		if decompEmbModel != "" {
			stats.ImportedEmbModels = append(stats.ImportedEmbModels, decompEmbModel)
			stats.ImportedEmbDims = append(stats.ImportedEmbDims, decompEmbDims)
		}
	}
	return stats, nil
}

func (w *PullAllFromRegistryWorker) listAllSources(ctx context.Context, db pgxpoolLike, repoID pgtype.UUID) ([]sourceRow, error) {
	rows, err := db.Query(ctx, `
		SELECT id, repository_id, url, doi
		FROM okt_repository.sources
		WHERE repository_id = $1
		ORDER BY created_at DESC`, repoID)
	if err != nil {
		return nil, fmt.Errorf("querying sources: %w", err)
	}
	defer rows.Close()

	var sources []sourceRow
	for rows.Next() {
		var s sourceRow
		if err := rows.Scan(&s.ID, &s.RepositoryID, &s.Url, &s.Doi); err != nil {
			return nil, fmt.Errorf("scanning source row: %w", err)
		}
		sources = append(sources, s)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return sources, nil
}

type sourceRow struct {
	ID           pgtype.UUID
	RepositoryID pgtype.UUID
	Url          string
	Doi          *string
}

func (w *PullAllFromRegistryWorker) loadFullSource(ctx context.Context, queries *store.Queries, id pgtype.UUID) (*store.OktRepositorySource, error) {
	row, err := queries.GetSourceByID(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("querying source by id: %w", err)
	}
	return &row, nil
}
