package tasks

import (
	"context"
	"fmt"
	"log"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/openktree/open-knowledge-tree/backend/internal/config"
	"github.com/openktree/open-knowledge-tree/backend/internal/dbpool"
	"github.com/openktree/open-knowledge-tree/backend/internal/providers/ai"
	"github.com/openktree/open-knowledge-tree/backend/internal/qdrantstore"
	"github.com/openktree/open-knowledge-tree/backend/internal/store"
	"github.com/riverqueue/river"
)

const QueueEmbedConcepts = "embed_concepts"

// EmbedConceptsArgs triggers an embedding pass for a repository's
// newly created concepts (those with embedded_at IS NULL). The
// pass embeds canonical_name + " " + context for each concept,
// upserts the vectors into the okt_concepts Qdrant collection,
// marks each concept embedded_at + embedded_model, and chains to
// cleanup_facts (the terminal step of the pipeline, moved here
// from deduplicate_facts so the serial chain is dedup →
// extract_concepts → embed_concepts → cleanup).
type EmbedConceptsArgs struct {
	RepositoryID string `json:"repository_id"`
	// SourceID, when non-empty, narrows the candidate set to concepts
	// linked (via fact_concepts → fact_sources) to facts from this
	// source. Empty means a repo-wide pass. MarkConceptEmbedded is
	// per-concept and idempotent, so a concept linked to facts from
	// multiple sources is embedded once; the second source's pass
	// finds embedded_at set and the IS NULL filter excludes it.
	SourceID string `json:"source_id,omitempty"`
}

func (EmbedConceptsArgs) Kind() string { return "embed_concepts" }

func (EmbedConceptsArgs) InsertOpts() river.InsertOpts { return river.InsertOpts{} }

// EmbedConceptsResult is recorded on the job row so the River UI
// shows what the pass did. Embedded is the count of concepts
// vectorized this run; EmbedErrors carries the count of concepts
// that failed to embed (vector upsert or mark-embedded failed) so
// the UI can surface partial failures.
type EmbedConceptsResult struct {
	RepositoryID string `json:"repository_id"`
	Embedded     int    `json:"embedded"`
	EmbedErrors  int    `json:"embed_errors"`
	Model        string `json:"model"`
}

type EmbedConceptsWorker struct {
	river.WorkerDefaults[EmbedConceptsArgs]

	embeddingProvider ai.EmbeddingProvider
	embeddingCfg      config.EmbeddingConfig
	qdrant            *qdrantstore.Store
	registry          *dbpool.Registry
	systemQueries     *store.Queries
}

func NewEmbedConceptsWorker(
	embeddingProvider ai.EmbeddingProvider,
	embeddingCfg config.EmbeddingConfig,
	qdrant *qdrantstore.Store,
	registry *dbpool.Registry,
	systemQueries *store.Queries,
) *EmbedConceptsWorker {
	return &EmbedConceptsWorker{
		embeddingProvider: embeddingProvider,
		embeddingCfg:      embeddingCfg,
		qdrant:            qdrant,
		registry:          registry,
		systemQueries:     systemQueries,
	}
}

// Work resolves the per-repo pool, fetches the repo's concepts with
// embedded_at IS NULL, bulk-embeds them via the configured embedding
// provider, upserts the vectors into the okt_concepts Qdrant
// collection (payload {repository_id}), marks each concept
// embedded_at + embedded_model, and chains to cleanup_facts. When
// the provider or Qdrant is not configured the worker logs and
// returns nil (a missing provider is a deployment choice, not a
// retryable error). When there are no concepts to embed the worker
// returns early but still chains to cleanup_facts so the pipeline
// doesn't stall.
func (w *EmbedConceptsWorker) Work(ctx context.Context, job *river.Job[EmbedConceptsArgs]) error {
	args := job.Args
	if args.RepositoryID == "" {
		return fmt.Errorf("embed_concepts: repository_id is required")
	}

	repoID := pgTypeUUID(args.RepositoryID)
	if !repoID.Valid {
		return fmt.Errorf("embed_concepts: invalid repository_id")
	}

	// SourceID is optional: when set, narrow to concepts linked to
	// facts from this source; when empty, repo-wide pass.
	var srcID pgtype.UUID
	sourceScoped := false
	if args.SourceID != "" {
		srcID = pgTypeUUID(args.SourceID)
		if !srcID.Valid {
			return fmt.Errorf("embed_concepts: invalid source_id")
		}
		sourceScoped = true
	}

	dbName, err := w.systemQueries.GetRepositoryDatabaseName(ctx, repoID)
	if err != nil {
		return fmt.Errorf("embed_concepts: resolving repository database: %w", err)
	}
	pool := w.registry.Get(dbName)
	queries := store.New(pool.Pool)

	// Unify the source-scoped and repo-wide row types into the
	// repo-wide row type (same fields: id, canonical_name, context)
	// so the embedding loop below is unchanged.
	var concepts []store.ListNewConceptsForEmbeddingRow
	if sourceScoped {
		srcConcepts, err := queries.ListNewConceptsForEmbeddingBySource(ctx, store.ListNewConceptsForEmbeddingBySourceParams{
			RepositoryID: repoID,
			SourceID:     srcID,
		})
		if err != nil {
			return fmt.Errorf("embed_concepts: listing new concepts by source: %w", err)
		}
		concepts = make([]store.ListNewConceptsForEmbeddingRow, len(srcConcepts))
		for i, c := range srcConcepts {
			concepts[i] = store.ListNewConceptsForEmbeddingRow{ID: c.ID, CanonicalName: c.CanonicalName, Context: c.Context}
		}
	} else {
		concepts, err = queries.ListNewConceptsForEmbedding(ctx, repoID)
		if err != nil {
			return fmt.Errorf("embed_concepts: listing new concepts: %w", err)
		}
	}

	result := EmbedConceptsResult{RepositoryID: args.RepositoryID}

	if len(concepts) == 0 {
		log.Printf("embed_concepts: no concepts to embed for repo %s", args.RepositoryID)
	} else if w.embeddingProvider == nil {
		log.Printf("embed_concepts: embedding provider not configured, skipping %d concepts for repo %s", len(concepts), args.RepositoryID)
	} else if w.qdrant == nil {
		log.Printf("embed_concepts: qdrant store not configured, skipping %d concepts for repo %s", len(concepts), args.RepositoryID)
	} else {
		// Build inputs: canonical_name + " " + context.
		inputs := make([]string, len(concepts))
		for i, c := range concepts {
			inputs[i] = c.CanonicalName + " " + c.Context
		}
		resp, err := w.embeddingProvider.Embed(ctx, pool.Pool, ai.EmbeddingRequest{
			Model:  w.embeddingCfg.Model,
			Inputs: inputs,
			TaskID: ptrString(strconv.FormatInt(job.ID, 10)),
			Attribution: ai.Attribution{
				RepositoryID: args.RepositoryID,
				Operation:    "concept_embedding",
			},
		})
		if err != nil {
			return fmt.Errorf("embed_concepts: embedding %d concepts for repo %s: %w", len(concepts), args.RepositoryID, err)
		}
		if len(resp.Embeddings) != len(concepts) {
			return fmt.Errorf("embed_concepts: embedding provider returned %d vectors for %d inputs", len(resp.Embeddings), len(concepts))
		}

		repoUUID, err := uuid.Parse(args.RepositoryID)
		if err != nil {
			return fmt.Errorf("embed_concepts: parsing repository_id as uuid: %w", err)
		}
		points := make([]qdrantstore.ConceptPoint, len(concepts))
		for i, c := range concepts {
			conceptID, err := uuid.Parse(pgUUIDToString(c.ID))
			if err != nil {
				log.Printf("embed_concepts: parsing concept id failed: %v", err)
				continue
			}
			points[i] = qdrantstore.ConceptPoint{
				ID:           conceptID,
				Vector:       resp.Embeddings[i],
				RepositoryID: repoUUID,
			}
		}
		if err := w.qdrant.UpsertConceptVectors(ctx, points); err != nil {
			return fmt.Errorf("embed_concepts: upserting concept vectors to qdrant: %w", err)
		}

		model := resp.Model
		if model == "" {
			model = w.embeddingCfg.Model
		}
		// Normalize to the bare model name so the stored
		// embedded_model is provider-routing-agnostic and matches
		// the registry cache reconciler's compare on pull.
		model = ai.NormalizeEmbeddingModel(model)
		result.Model = model
		for _, c := range concepts {
			if _, err := queries.MarkConceptEmbedded(ctx, store.MarkConceptEmbeddedParams{
				ID:            c.ID,
				EmbeddedModel: &model,
			}); err != nil {
				log.Printf("embed_concepts: marking concept %s embedded failed: %v", pgUUIDToString(c.ID), err)
				result.EmbedErrors++
				continue
			}
			result.Embedded++
		}
		log.Printf("embed_concepts: embedded %d concepts for repo %s (model %s, %d errors)", result.Embedded, args.RepositoryID, model, result.EmbedErrors)
	}

	// Chain to cleanup_facts (the terminal step, moved here from
	// deduplicate_facts). Fresh background ctx — River cancels the
	// worker ctx as the job completes and the chained Insert opens
	// its own transaction.
	if client := river.ClientFromContext[pgx.Tx](ctx); client != nil {
		chainCtx, chainCancel := context.WithTimeout(context.Background(), 15*time.Second)
		if _, err := client.Insert(chainCtx, CleanupFactsArgs{RepositoryID: args.RepositoryID, SourceID: args.SourceID}, &river.InsertOpts{
			Queue: QueueCleanupFacts,
			Metadata: MarshalMetadata(JobMetadata{
				RepositoryID: args.RepositoryID,
				SourceID:     args.SourceID,
			}),
		}); err != nil {
			log.Printf("embed_concepts: enqueueing cleanup_facts for repo %s: %v", args.RepositoryID, err)
		}
		chainCancel()
	} else {
		log.Printf("embed_concepts: no river client on context; cleanup_facts not enqueued for repo %s", args.RepositoryID)
	}

	return river.RecordOutput(ctx, &result)
}

// pgTypeUUID scans a UUID string into a pgtype.UUID. Returns the
// zero value (Valid=false) when the string is not a valid UUID so
// the caller can branch on validity without a separate error.
func pgTypeUUID(s string) pgtype.UUID {
	var u pgtype.UUID
	if err := u.Scan(s); err != nil {
		return pgtype.UUID{}
	}
	return u
}
