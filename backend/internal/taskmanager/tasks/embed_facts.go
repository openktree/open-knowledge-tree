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
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/openktree/open-knowledge-tree/backend/internal/config"
	"github.com/openktree/open-knowledge-tree/backend/internal/dbpool"
	"github.com/openktree/open-knowledge-tree/backend/internal/providers/ai"
	"github.com/openktree/open-knowledge-tree/backend/internal/qdrantstore"
	"github.com/openktree/open-knowledge-tree/backend/internal/store"
	"github.com/riverqueue/river"
)

const QueueEmbedFacts = "embed_facts"

// EmbedFactsArgs triggers an embedding pass for a repository.
// SourceID is carried for traceability (which source's
// decomposition triggered this embed) but the working set is
// cross-source: every `new` fact in the repo with `embedded_at IS
// NULL` is embedded in this pass. RepositoryID scopes the work.
type EmbedFactsArgs struct {
	SourceID     string `json:"source_id"`
	RepositoryID string `json:"repository_id"`
}

func (EmbedFactsArgs) Kind() string { return "embed_facts" }

func (EmbedFactsArgs) InsertOpts() river.InsertOpts { return river.InsertOpts{} }

// EmbedFactsResult is recorded on the job row so the River UI
// shows what the pass did. Embedded is the count of facts
// vectorized this run; it may be 0 when a re-enqueue races with
// a previous pass (the second pass finds no `embedded_at IS NULL`
// rows and no-ops). EmbedErrors carries the count of facts that
// failed to embed (vector upsert or mark-embedded failed) so the
// UI can surface partial failures without digging through logs.
type EmbedFactsResult struct {
	SourceID     string `json:"source_id"`
	RepositoryID string `json:"repository_id"`
	Embedded     int    `json:"embedded"`
	EmbedErrors  int    `json:"embed_errors"`
	Model        string `json:"model"`
}

type EmbedFactsWorker struct {
	river.WorkerDefaults[EmbedFactsArgs]

	embeddingProvider ai.EmbeddingProvider
	embeddingCfg      config.EmbeddingConfig
	qdrant            *qdrantstore.Store
	registry          *dbpool.Registry
	systemQueries     *store.Queries
}

func NewEmbedFactsWorker(
	embeddingProvider ai.EmbeddingProvider,
	embeddingCfg config.EmbeddingConfig,
	qdrant *qdrantstore.Store,
	registry *dbpool.Registry,
	systemQueries *store.Queries,
) *EmbedFactsWorker {
	return &EmbedFactsWorker{
		embeddingProvider: embeddingProvider,
		embeddingCfg:      embeddingCfg,
		qdrant:            qdrant,
		registry:          registry,
		systemQueries:     systemQueries,
	}
}

// Work resolves the per-repo pool, fetches ALL unembedded 'new'
// facts for the triggering source, bulk-embeds them via the
// configured embedding provider, upserts the vectors into Qdrant
// (payload `{repository_id, status}`), marks each fact
// `embedded_at` + `embedded_model`, and chains to
// `deduplicate_facts`.
//
// Embedding is strictly source-bounded: a job only embeds its own
// source's facts. This is what bounds per-job work and keeps 50
// concurrent workers from re-embedding the same full-repo set
// (which caused runaway embedding API spend). The repo-wide
// "everything gets processed eventually" guarantee comes from the
// callers enqueuing one embed_facts job PER source (source_decomposition
// already does this; the registry reconciler was fixed to do the
// same), not from a single repo-wide embed job.
//
// SourceID is required. When the provider or Qdrant is not
// configured the worker logs and returns nil (a missing provider
// is a deployment choice, not a retryable error — River would
// otherwise spin forever). When there are no facts to embed the
// worker returns early without enqueuing dedup, so a re-enqueue
// from a racing source doesn't trigger an empty dedup pass.
func (w *EmbedFactsWorker) Work(ctx context.Context, job *river.Job[EmbedFactsArgs]) error {
	args := job.Args
	if args.RepositoryID == "" {
		return fmt.Errorf("embed_facts: repository_id is required")
	}
	if args.SourceID == "" {
		return fmt.Errorf("embed_facts: source_id is required")
	}

	if w.embeddingProvider == nil {
		log.Printf("embed_facts: embedding provider not configured, skipping repo %s source %s", args.RepositoryID, args.SourceID)
		return river.RecordOutput(ctx, &EmbedFactsResult{RepositoryID: args.RepositoryID, SourceID: args.SourceID})
	}
	if w.qdrant == nil {
		log.Printf("embed_facts: qdrant store not configured, skipping repo %s source %s", args.RepositoryID, args.SourceID)
		return river.RecordOutput(ctx, &EmbedFactsResult{RepositoryID: args.RepositoryID, SourceID: args.SourceID})
	}

	repoID := pgtype.UUID{}
	if err := repoID.Scan(args.RepositoryID); err != nil {
		return fmt.Errorf("embed_facts: invalid repository_id: %w", err)
	}
	sourceID := pgtype.UUID{}
	if err := sourceID.Scan(args.SourceID); err != nil {
		return fmt.Errorf("embed_facts: invalid source_id: %w", err)
	}

	dbName, err := w.systemQueries.GetRepositoryDatabaseName(ctx, repoID)
	if err != nil {
		return fmt.Errorf("embed_facts: resolving repository database: %w", err)
	}
	pool := w.registry.Get(dbName)
	queries := store.New(pool.Pool)

	facts, err := queries.ListNewFactsForSourceEmbedding(ctx, store.ListNewFactsForSourceEmbeddingParams{
		RepositoryID: repoID,
		SourceID:     sourceID,
	})
	if err != nil {
		return fmt.Errorf("embed_facts: listing source facts: %w", err)
	}
	if len(facts) == 0 {
		log.Printf("embed_facts: no facts to embed for repo %s source %s", args.RepositoryID, args.SourceID)
		return river.RecordOutput(ctx, &EmbedFactsResult{RepositoryID: args.RepositoryID, SourceID: args.SourceID})
	}

	totalEmbedded, totalErrors, model, err := w.embedFacts(ctx, queries, pool.Pool, job, args, facts)
	if err != nil {
		return err
	}

	log.Printf("embed_facts: embedded %d facts for repo %s source %s (model %s, %d errors)", totalEmbedded, args.RepositoryID, args.SourceID, model, totalErrors)

	// Chain to deduplicate_facts. Same client-from-context pattern
	// as source_decomposition → embed_facts. A fresh background ctx
	// is used (not the worker ctx) because River cancels the
	// worker ctx as the job completes and the chained Insert
	// opens its own transaction — reusing the worker ctx races
	// the cancellation.
	if client := river.ClientFromContext[pgx.Tx](ctx); client != nil {
		chainCtx, chainCancel := context.WithTimeout(context.Background(), 15*time.Second)
		if _, err := client.Insert(chainCtx, DeduplicateFactsArgs{RepositoryID: args.RepositoryID, SourceID: args.SourceID}, &river.InsertOpts{
			Queue: QueueDeduplicateFacts,
			Metadata: MarshalMetadata(JobMetadata{
				RepositoryID: args.RepositoryID,
				SourceID:     args.SourceID,
			}),
		}); err != nil {
			log.Printf("embed_facts: enqueueing deduplicate_facts for repo %s: %v", args.RepositoryID, err)
		}
		chainCancel()
	} else {
		log.Printf("embed_facts: no river client on context; deduplicate_facts not enqueued for repo %s", args.RepositoryID)
	}

	return river.RecordOutput(ctx, &EmbedFactsResult{
		SourceID:     args.SourceID,
		RepositoryID: args.RepositoryID,
		Embedded:     totalEmbedded,
		EmbedErrors:  totalErrors,
		Model:        model,
	})
}

// embedFacts bulk-embeds a slice of facts, upserts the vectors into
// Qdrant, and marks each fact embedded_at + embedded_model. It
// returns (embedded count, error count, model, error). The model
// string is the one the provider actually returned (resp.Model),
// falling back to the configured model when empty so a provider-
// side alias is recorded honestly. Per-fact upsert/mark failures
// are logged and counted, not fatal — a single bad fact doesn't
// kill the pass (the rest of the slice still gets embedded).
func (w *EmbedFactsWorker) embedFacts(
	ctx context.Context,
	queries *store.Queries,
	pool *pgxpool.Pool,
	job *river.Job[EmbedFactsArgs],
	args EmbedFactsArgs,
	facts []store.OktRepositoryFact,
) (embedded, embedErrors int, model string, err error) {
	inputs := make([]string, len(facts))
	for i, f := range facts {
		inputs[i] = f.Text
	}
	resp, err := w.embeddingProvider.Embed(ctx, pool, ai.EmbeddingRequest{
		Model:  w.embeddingCfg.Model,
		Inputs: inputs,
		TaskID: ptrString(strconv.FormatInt(job.ID, 10)),
		Attribution: ai.Attribution{
			RepositoryID: args.RepositoryID,
			SourceID:     args.SourceID,
			Operation:    "embedding",
		},
	})
	if err != nil {
		return 0, 0, "", fmt.Errorf("embed_facts: embedding %d facts for repo %s: %w", len(facts), args.RepositoryID, err)
	}
	if len(resp.Embeddings) != len(facts) {
		return 0, 0, "", fmt.Errorf("embed_facts: embedding provider returned %d vectors for %d inputs", len(resp.Embeddings), len(facts))
	}

	// Upsert into Qdrant. Point id = fact UUID; payload =
	// {repository_id, status}. The repository_id filter on
	// future searches is what keeps the shared collection per-repo.
	repoUUID, err := uuid.Parse(args.RepositoryID)
	if err != nil {
		return 0, 0, "", fmt.Errorf("embed_facts: parsing repository_id as uuid: %w", err)
	}
	points := make([]qdrantstore.FactPoint, len(facts))
	for i, f := range facts {
		factID, err := uuid.Parse(pgUUIDToString(f.ID))
		if err != nil {
			log.Printf("embed_facts: parsing fact id failed: %v", err)
			continue
		}
		points[i] = qdrantstore.FactPoint{
			ID:           factID,
			Vector:       resp.Embeddings[i],
			RepositoryID: repoUUID,
			Status:       f.Status,
		}
	}
	if err := w.qdrant.UpsertFactVectors(ctx, points); err != nil {
		return 0, 0, "", fmt.Errorf("embed_facts: upserting vectors to qdrant: %w", err)
	}

	model = resp.Model
	if model == "" {
		model = w.embeddingCfg.Model
	}
	// Normalize to the bare model name so the stored embedded_model
	// is provider-routing-agnostic and matches the registry cache
	// reconciler's compare on pull.
	model = ai.NormalizeEmbeddingModel(model)
	embedErrors = 0
	for _, f := range facts {
		if _, err := queries.MarkFactEmbedded(ctx, store.MarkFactEmbeddedParams{
			ID:            f.ID,
			EmbeddedModel: &model,
		}); err != nil {
			log.Printf("embed_facts: marking fact %s embedded failed: %v", pgUUIDToString(f.ID), err)
			embedErrors++
			continue
		}
	}
	return len(facts) - embedErrors, embedErrors, model, nil
}

// pgUUIDToString formats a pgtype.UUID as a canonical lowercase
// UUID string. Returns "" when the UUID is invalid (the caller
// logs and skips).
func pgUUIDToString(id pgtype.UUID) string {
	if !id.Valid {
		return ""
	}
	b := id.Bytes
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// uuidToPg converts a google/uuid.UUID into a pgtype.UUID for
// store query params. The scan path is the most reliable round-
// trip (it validates the canonical string form), so we use it
// rather than touching id.Bytes directly.
func uuidToPg(id uuid.UUID) pgtype.UUID {
	var p pgtype.UUID
	if err := p.Scan(id.String()); err != nil {
		return pgtype.UUID{}
	}
	return p
}

// ptrString returns a pointer to s. Used to build the *string
// TaskID field of EmbeddingRequest from the River job id (which
// is an int64 formatted to a string) without a helper at every
// call site.
func ptrString(s string) *string {
	return &s
}
