package tasks

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/openktree/open-knowledge-tree/backend/internal/dbpool"
	"github.com/openktree/open-knowledge-tree/backend/internal/providers/ai"
	"github.com/openktree/open-knowledge-tree/backend/internal/providers/registry"
	"github.com/openktree/open-knowledge-tree/backend/internal/qdrantstore"
	"github.com/openktree/open-knowledge-tree/backend/internal/store"
	"github.com/riverqueue/river"
)

const QueueContributeSource = "contribute_source"

// ContributeSourceArgs triggers an upload of a source's decomposition
// data (facts, concepts, embeddings) to the registry. Chained after
// cleanup_facts so only stable, deduplicated facts are contributed.
type ContributeSourceArgs struct {
	RepositoryID string `json:"repository_id"`
	SourceID     string `json:"source_id"`
}

func (ContributeSourceArgs) Kind() string { return "contribute_source" }

func (ContributeSourceArgs) InsertOpts() river.InsertOpts { return river.InsertOpts{} }

type ContributeSourceResult struct {
	RepositoryID string `json:"repository_id"`
	SourceID     string `json:"source_id"`
	Pushed       bool   `json:"pushed"`
}

type ContributeSourceWorker struct {
	river.WorkerDefaults[ContributeSourceArgs]

	registryClients      *registry.ClientMap
	qdrant               *qdrantstore.Store
	registry             *dbpool.Registry
	systemQueries        *store.Queries
	modelResolver        *ModelResolver
	promptsetResolver    *PromptsetResolver
	defaultFactModel     string
}

func NewContributeSourceWorker(
	registryClients *registry.ClientMap,
	qdrant *qdrantstore.Store,
	poolRegistry *dbpool.Registry,
	systemQueries *store.Queries,
	modelResolver *ModelResolver,
	promptsetResolver *PromptsetResolver,
	defaultFactModel string,
) *ContributeSourceWorker {
	return &ContributeSourceWorker{
		registryClients:   registryClients,
		qdrant:            qdrant,
		registry:          poolRegistry,
		systemQueries:     systemQueries,
		modelResolver:     modelResolver,
		promptsetResolver: promptsetResolver,
		defaultFactModel:  defaultFactModel,
	}
}

func (w *ContributeSourceWorker) Work(ctx context.Context, job *river.Job[ContributeSourceArgs]) error {
	args := job.Args
	if args.RepositoryID == "" || args.SourceID == "" {
		return fmt.Errorf("contribute_source: repository_id and source_id are required")
	}

	repoID := pgtype.UUID{}
	if err := repoID.Scan(args.RepositoryID); err != nil {
		return fmt.Errorf("contribute_source: invalid repository_id: %w", err)
	}
	srcID := pgtype.UUID{}
	if err := srcID.Scan(args.SourceID); err != nil {
		return fmt.Errorf("contribute_source: invalid source_id: %w", err)
	}

	dbName, err := w.systemQueries.GetRepositoryDatabaseName(ctx, repoID)
	if err != nil {
		return fmt.Errorf("contribute_source: resolving repository database: %w", err)
	}
	dbPool := w.registry.Get(dbName)

	// Per-repo gate: the repo may have turned the registry
	// integration off, or the configured registry may no longer
	// exist. Skip the push in either case (defense-in-depth — the
	// HTTP gate already rejects enqueuing when disabled).
	_, rc, err := resolveRepoRegistryClient(ctx, w.systemQueries, w.registryClients, repoID)
	if err != nil {
		log.Printf("contribute_source: skipping push for repo %s: %v", args.RepositoryID, err)
		return nil
	}

	source, err := store.New(dbPool).GetSourceByID(ctx, srcID)
	if err != nil {
		return fmt.Errorf("contribute_source: getting source: %w", err)
	}

	// Per-repo push level (migration 0044). Controls whether the
	// contribution includes concepts/links/concept-embeddings or
	// only sources + facts + fact embeddings. Defaults to "concepts"
	// (full push). The SyncLevelFilter is the single source of truth
	// for what each level includes.
	syncLevels, err := w.systemQueries.GetRepositorySyncLevels(ctx, repoID)
	if err != nil {
		return fmt.Errorf("contribute_source: reading sync levels: %w", err)
	}
	pushFilter := registry.NewSyncLevelFilter(registry.ParseSyncLevel(syncLevels.RegistryPushLevel))

	sourceURL := source.Url
	var sourceDOI string
	if source.Doi != nil {
		sourceDOI = *source.Doi
	}
	var sourceTitle string
	if source.ParsedTitle != nil {
		sourceTitle = *source.ParsedTitle
	}
	var parsedText, parsedMarkdown string
	if source.ParsedText != nil {
		parsedText = *source.ParsedText
	}
	if source.ParsedMarkdown != nil {
		parsedMarkdown = *source.ParsedMarkdown
	}

	regSourceID, err := rc.PushSource(ctx, sourceURL, sourceDOI, "", sourceTitle, parsedText, parsedMarkdown)
	if err != nil {
		return fmt.Errorf("contribute_source: pushing source: %w", err)
	}

	var embeddingModel string
	facts, factUUIDs, err := w.loadFacts(ctx, dbPool, srcID, &embeddingModel)
	if err != nil {
		return fmt.Errorf("contribute_source: loading facts: %w", err)
	}
	if len(facts) == 0 {
		log.Printf("contribute_source: repo %s source %s has no stable facts; skipping decomposition push",
			args.RepositoryID, args.SourceID)
		return river.RecordOutput(ctx, &ContributeSourceResult{
			RepositoryID: args.RepositoryID,
			SourceID:     args.SourceID,
			Pushed:       true,
		})
	}

	// Concepts, links, and concept embeddings are only loaded and
	// pushed when the repo's push level is "concepts". At "facts"
	// level we skip the DB + Qdrant reads entirely (no wasted work)
	// and the decomposition is pushed with Concepts/Links nil and
	// only fact embeddings. The SyncLevelFilter is the single source
	// of truth for what each level includes.
	var concepts []registry.ConceptData
	var conceptIDs []uuid.UUID
	var links []registry.FactConceptLink
	if pushFilter.IncludeConcepts() {
		concepts, conceptIDs, err = w.loadConcepts(ctx, dbPool, srcID)
		if err != nil {
			return fmt.Errorf("contribute_source: loading concepts: %w", err)
		}

		// Build the outbound context mapper before filtering so both
		// concepts and links see the same translation. Concepts whose
		// local context is unmapped (and absent from the registry vocab)
		// are dropped from the push; their concept IDs are dropped from
		// the vector lookup; links to dropped concepts are dropped too
		// so the registry doesn't receive dangling fact_concept links.
		mapper, err := NewOutboundContextMapper(ctx, w.systemQueries, rc, repoID)
		if err != nil {
			return fmt.Errorf("contribute_source: building context mapper: %w", err)
		}
		skippedConceptNames := map[string]bool{}
		concepts, conceptIDs = ApplyOutboundConcepts(concepts, conceptIDs, mapper, skippedConceptNames)
		if len(skippedConceptNames) > 0 {
			log.Printf("contribute_source: repo %s source %s: %d concepts skipped (unmapped local context absent from registry vocab)",
				args.RepositoryID, args.SourceID, len(skippedConceptNames))
		}

		links, err = w.loadFactConceptLinks(ctx, dbPool, srcID)
		if err != nil {
			return fmt.Errorf("contribute_source: loading fact-concept links: %w", err)
		}
		links = FilterLinksByContext(links, mapper, skippedConceptNames)
	}

	// The decomposition's ModelID is the EXTRACTION model (e.g.
	// "google/gemma-4-31b-it"), not the embedding model. This is
	// the key the registry uses to store and retrieve
	// decompositions — a source can have multiple decompositions
	// from different extraction models. The embedding model is
	// recorded separately on the EmbeddingData so pulling repos
	// know which model produced the vectors.
	extractionModel := w.defaultFactModel
	if w.modelResolver != nil {
		if r := w.modelResolver.Resolve(ctx, repoID, TaskKindFactExtraction); r.ModelID != "" {
			extractionModel = r.ModelID
		}
	}
	if extractionModel == "" {
		extractionModel = "default"
	}

	// Resolve the repo's effective promptset hash so the registry
	// can tag the decomposition with the philosophy that produced
	// it. Pulling repos filter on this hash (via their
	// accepted_promptset_hashes) so decompositions from different
	// promptsets do not mix.
	var psHash string
	if w.promptsetResolver != nil {
		psHash = w.promptsetResolver.EffectiveHash(ctx, repoID)
	}

	decomp := &registry.DecompositionPackage{
		ModelID:       extractionModel,
		PromptsetHash: psHash,
		Facts:         facts,
		Concepts:      concepts,
		Links:         links,
	}

	// Build the EmbeddingData in the registry's expected shape:
	// a single object with a vectors map keyed by "fact:<uuid>"
	// / "concept:<uuid>". All vectors in one decomposition share
	// the same embedding model (the one stored on facts.embedded_model).
	if embeddingModel != "" {
		factVecs, err := w.qdrant.GetFactVectorsByIDs(ctx, factUUIDs)
		if err != nil {
			log.Printf("contribute_source: loading fact vectors (non-fatal): %v", err)
		}
		vectors := make(map[string][]float64)
		var dims int
		for i := range facts {
			if i >= len(factUUIDs) || factUUIDs[i] == uuid.Nil {
				continue
			}
			u := factUUIDs[i]
			if vec, ok := factVecs[u]; ok {
				// Key by content_hash so the pull path can match
				// embeddings to facts via factIDByHash without
				// needing the remote UUID (which differs from
				// the local UUID after import).
				key := "fact:" + facts[i].ContentHash
				vectors[key] = float32sTo64s(vec.Vector)
				if dims == 0 {
					dims = len(vec.Vector)
				}
			}
		}
		if pushFilter.IncludeConcepts() {
			conceptVecs, err := w.qdrant.GetConceptVectorsByIDs(ctx, conceptIDs)
			if err != nil {
				log.Printf("contribute_source: loading concept vectors (non-fatal): %v", err)
			}
			for _, u := range conceptIDs {
				if u == uuid.Nil {
					continue
				}
				if vec, ok := conceptVecs[u]; ok {
					key := "concept:" + u.String()
					vectors[key] = float32sTo64s(vec.Vector)
					if dims == 0 {
						dims = len(vec.Vector)
					}
				}
			}
		}
		if len(vectors) > 0 {
			decomp.Embeddings = &registry.EmbeddingData{
				Model:      embeddingModel,
				Dimensions: dims,
				Vectors:    vectors,
			}
		}
	}

	if _, err := rc.PushDecomposition(ctx, regSourceID, extractionModel, decomp); err != nil {
		return fmt.Errorf("contribute_source: pushing decomposition: %w", err)
	}

	log.Printf("contribute_source: repo %s source %s pushed %d facts, %d concepts, %d links (model=%s, emb_model=%s, emb_vectors=%d)",
		args.RepositoryID, args.SourceID, len(decomp.Facts), len(decomp.Concepts), len(decomp.Links),
		extractionModel, embeddingModel, embVecCount(decomp.Embeddings))
	return river.RecordOutput(ctx, &ContributeSourceResult{
		RepositoryID: args.RepositoryID,
		SourceID:     args.SourceID,
		Pushed:       true,
	})
}

func embVecCount(ed *registry.EmbeddingData) int {
	if ed == nil {
		return 0
	}
	return len(ed.Vectors)
}

func (w *ContributeSourceWorker) loadFacts(ctx context.Context, pool *dbpool.Pool, sourceID pgtype.UUID, embeddingModel *string) ([]registry.FactData, []uuid.UUID, error) {
	rows, err := pool.Query(ctx, `
		SELECT f.id, f.text, f.fact_kind, COALESCE(f.image_url, ''), fs.chunk_index, f.embedded_model
		FROM okt_repository.facts f
		JOIN okt_repository.fact_sources fs ON fs.fact_id = f.id
		WHERE fs.source_id = $1
		  AND f.status = 'stable'
		ORDER BY fs.chunk_index`, sourceID)
	if err != nil {
		return nil, nil, fmt.Errorf("querying facts: %w", err)
	}
	defer rows.Close()

	var facts []registry.FactData
	var factUUIDs []uuid.UUID
	for rows.Next() {
		var id pgtype.UUID
		var text, factKind, imageURL string
		var chunkIndex int32
		var embModel *string
		if err := rows.Scan(&id, &text, &factKind, &imageURL, &chunkIndex, &embModel); err != nil {
			return nil, nil, fmt.Errorf("scanning fact row: %w", err)
		}
		if embModel != nil && *embeddingModel == "" {
			// Normalize to the bare model name so the registry
			// stores a provider-routing-agnostic identifier and
			// the pull-side reconciler compares on model identity.
			*embeddingModel = ai.NormalizeEmbeddingModel(*embModel)
		}
		facts = append(facts, registry.FactData{
			Content:     text,
			ContentHash: factContentHash(text),
			Confidence:  1.0,
			SentenceIdx: int(chunkIndex),
			ImageURL:    imageURL,
		})
		factUUIDs = append(factUUIDs, idFromString(pgUUIDToString(id)))
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	return facts, factUUIDs, nil
}

func (w *ContributeSourceWorker) loadConcepts(ctx context.Context, pool *dbpool.Pool, sourceID pgtype.UUID) ([]registry.ConceptData, []uuid.UUID, error) {
	rows, err := pool.Query(ctx, `
		SELECT DISTINCT c.id, c.canonical_name, c.context
		FROM okt_repository.concepts c
		JOIN okt_repository.fact_concepts fc ON fc.concept_id = c.id
		JOIN okt_repository.fact_sources fs ON fs.fact_id = fc.fact_id
		WHERE fs.source_id = $1
		ORDER BY c.canonical_name`, sourceID)
	if err != nil {
		return nil, nil, fmt.Errorf("querying concepts: %w", err)
	}
	defer rows.Close()

	type conceptRow struct {
		id            pgtype.UUID
		canonicalName string
		context       string
	}
	var conceptRows []conceptRow
	var conceptPgIDs []pgtype.UUID

	for rows.Next() {
		var id pgtype.UUID
		var canonicalName, context string
		if err := rows.Scan(&id, &canonicalName, &context); err != nil {
			return nil, nil, fmt.Errorf("scanning concept row: %w", err)
		}
		conceptRows = append(conceptRows, conceptRow{id, canonicalName, context})
		conceptPgIDs = append(conceptPgIDs, id)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	if len(conceptRows) == 0 {
		return nil, nil, nil
	}

	aliasRows, err := pool.Query(ctx, `
		SELECT concept_id, alias_text
		FROM okt_repository.concept_aliases
		WHERE concept_id = ANY($1)
		ORDER BY concept_id, alias_text`, conceptPgIDs)
	if err != nil {
		return nil, nil, fmt.Errorf("querying concept aliases: %w", err)
	}
	defer aliasRows.Close()

	aliasMap := make(map[pgtype.UUID][]string)
	for aliasRows.Next() {
		var conceptID pgtype.UUID
		var alias string
		if err := aliasRows.Scan(&conceptID, &alias); err != nil {
			return nil, nil, fmt.Errorf("scanning alias row: %w", err)
		}
		aliasMap[conceptID] = append(aliasMap[conceptID], alias)
	}
	if err := aliasRows.Err(); err != nil {
		return nil, nil, err
	}

	conceptIDs := make([]uuid.UUID, 0, len(conceptRows))
	result := make([]registry.ConceptData, 0, len(conceptRows))
	for _, cr := range conceptRows {
		u := idFromString(pgUUIDToString(cr.id))
		if u == uuid.Nil {
			continue
		}
		conceptIDs = append(conceptIDs, u)
		result = append(result, registry.ConceptData{
			CanonicalName: cr.canonicalName,
			Context:       cr.context,
			Aliases:       aliasMap[cr.id],
		})
	}
	return result, conceptIDs, nil
}

func (w *ContributeSourceWorker) loadFactConceptLinks(ctx context.Context, pool *dbpool.Pool, sourceID pgtype.UUID) ([]registry.FactConceptLink, error) {
	rows, err := pool.Query(ctx, `
		SELECT f.text AS fact_text, c.canonical_name, c.context
		FROM okt_repository.fact_concepts fc
		JOIN okt_repository.facts f ON f.id = fc.fact_id
		JOIN okt_repository.concepts c ON c.id = fc.concept_id
		JOIN okt_repository.fact_sources fs ON fs.fact_id = fc.fact_id
		WHERE fs.source_id = $1 AND f.status = 'stable'
		ORDER BY f.id, c.canonical_name`, sourceID)
	if err != nil {
		return nil, fmt.Errorf("querying fact-concept links: %w", err)
	}
	defer rows.Close()

	var links []registry.FactConceptLink
	for rows.Next() {
		var factText, canonicalName, context string
		if err := rows.Scan(&factText, &canonicalName, &context); err != nil {
			return nil, fmt.Errorf("scanning fact-concept link row: %w", err)
		}
		links = append(links, registry.FactConceptLink{
			FactContentHash: factContentHash(factText),
			ConceptName:     canonicalName,
			ConceptContext:  context,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return links, nil
}

func float32sTo64s(in []float32) []float64 {
	out := make([]float64, len(in))
	for i, v := range in {
		out[i] = float64(v)
	}
	return out
}

func idFromString(s string) uuid.UUID {
	if s == "" {
		return uuid.Nil
	}
	u, err := uuid.Parse(s)
	if err != nil {
		return uuid.Nil
	}
	return u
}

func factContentHash(text string) string {
	h := sha256.Sum256([]byte(text))
	return hex.EncodeToString(h[:])
}
