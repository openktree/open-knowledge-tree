package tasks

import (
	"context"
	"log"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/openktree/open-knowledge-tree/backend/internal/config"
	"github.com/openktree/open-knowledge-tree/backend/internal/providers/ai"
	"github.com/openktree/open-knowledge-tree/backend/internal/store"
	"github.com/riverqueue/river"
)

// ImportStats is the output of a delta-aware registry cache import. It
// records how many facts were genuinely created vs skipped as
// verbatim duplicates, and which embedding models + dimensions the
// imported decompositions actually used (so the reconciler can
// detect a mismatch with the local embedding config).
type ImportStats struct {
	Created           int      // facts actually created (new UUID, new row)
	Skipped           int      // facts skipped (exact text already linked to source)
	ImportedEmbModels []string // distinct embedding models seen in imported embeddings
	// ImportedEmbDims is parallel to ImportedEmbModels: the
	// dimensions each imported decomposition's embeddings used
	// (from EmbeddingData.Dimensions). Used by the reconciler to
	// detect a dimension mismatch (same model name, different
	// vector space) as a defense-in-depth check alongside the
	// model-name comparison. A zero entry means "unknown" and is
	// ignored (the dimensions weren't reported on the import).
	ImportedEmbDims []int
}

// HasDelta reports whether the import produced any new facts. When
// false, the reconciler produces an empty plan (zero downstream
// jobs) — an already-synced source triggers nothing.
func (s ImportStats) HasDelta() bool { return s.Created > 0 }

// EmbModelMismatch reports whether any imported decomposition's
// embedding model differs from the local embedding config. When
// true, the reconciler resets the imported facts'/concepts'
// embedded_at so the embed_facts/embed_concepts workers re-embed
// them with the local model (the imported vectors live in a
// different space and must not be used as-is for dedup/search).
//
// The comparison is provider-agnostic: both the imported model and
// the local config model are normalized via
// ai.NormalizeEmbeddingModel before comparing, so
// "google/gemini-embedding-2:free" (OpenRouter) and
// "gemini-embedding-2" (Ollama) are recognized as the same model and
// do NOT trigger a re-embed. When localDims > 0 and the matching
// imported entry reports a non-zero dimension that differs, the
// decomposition is flagged as a mismatch regardless of the model
// name (same name, different vector space).
func (s ImportStats) EmbModelMismatch(localModel string, localDims int) bool {
	localNorm := ai.NormalizeEmbeddingModel(localModel)
	for i, m := range s.ImportedEmbModels {
		if m == "" {
			continue
		}
		if ai.NormalizeEmbeddingModel(m) != localNorm {
			return true
		}
		// Model names match (after normalization). Defense-in-
		// depth: also compare dimensions when both are known, so a
		// config change to EmbeddingConfig.Dimensions without a
		// model-name change still forces a re-embed.
		if localDims > 0 && i < len(s.ImportedEmbDims) && s.ImportedEmbDims[i] > 0 && s.ImportedEmbDims[i] != localDims {
			return true
		}
	}
	return false
}

// ReconcilePlan is the set of downstream jobs the CacheReconciler
// decided should run after a registry cache import. An empty plan
// (all fields false) means the import was a no-op (already synced)
// and zero jobs are enqueued.
type ReconcilePlan struct {
	// ReembedFacts: reset embedded_at on the imported facts + delete
	// their Qdrant points, then enqueue embed_facts (which chains to
	// dedup naturally). Set when the imported embedding model differs
	// from the local config.
	ReembedFacts bool
	// DedupFacts: enqueue deduplicate_facts directly (the imported
	// vectors are in the local space, no re-embed needed). Dedup
	// chains to extract_concepts → embed_concepts → cleanup + summarize
	// only when it promotes new stable facts (promoted > 0 gate).
	DedupFacts bool
}

// IsEmpty reports whether the plan enqueues zero jobs.
func (p ReconcilePlan) IsEmpty() bool { return !p.ReembedFacts && !p.DedupFacts }

// CacheReconciler computes the post-import task plan for the registry
// cache path. It replaces the hardcoded "dedup + summarize" tail that
// ran unconditionally after every import — the reconciler is
// delta-aware: an already-synced source (all facts skipped) produces
// an empty plan and triggers zero jobs.
//
// The reconciler owns the cache-import path only. The normal fetch
// path keeps its existing hardcoded chain (it always embeds/extracts/
// summarizes from scratch, no reconciliation needed).
type CacheReconciler struct {
	embeddingCfg config.EmbeddingConfig
}

// NewCacheReconciler builds a reconciler from the local embedding
// config. The embedding model is the one the local workers use; the
// reconciler compares it against the imported decompositions' actual
// embedding models to decide whether re-embedding is needed.
func NewCacheReconciler(embeddingCfg config.EmbeddingConfig) *CacheReconciler {
	return &CacheReconciler{embeddingCfg: embeddingCfg}
}

// Plan computes the ReconcilePlan from the import stats.
//
//   - created == 0 → empty plan (already synced, no delta)
//   - created > 0 + embedding mismatch → ReembedFacts (reset + embed,
//     which chains to dedup naturally)
//   - created > 0 + no mismatch → DedupFacts (dedup chains to extract
//     only when promoted > 0; summarize is transitive via extract, not
//     enqueued directly)
func (r *CacheReconciler) Plan(stats ImportStats) ReconcilePlan {
	if !stats.HasDelta() {
		return ReconcilePlan{}
	}
	if stats.EmbModelMismatch(r.embeddingCfg.Model, r.embeddingCfg.Dimensions) {
		return ReconcilePlan{ReembedFacts: true}
	}
	return ReconcilePlan{DedupFacts: true}
}

// ResetForReembed clears embedded_at + embedded_model on the source's
// imported facts and concepts so the embed_facts/embed_concepts
// workers (which filter embedded_at IS NULL) re-embed them with the
// local model. The Qdrant points for these facts/concepts are deleted
// via the qdrant store (now keyed by local UUIDs). Called by the
// worker before enqueuing embed_facts when the plan says ReembedFacts.
func (r *CacheReconciler) ResetForReembed(ctx context.Context, queries *store.Queries, repoID pgtype.UUID) {
	// Reset facts: imported facts for this repo that have an
	// embedded_at (i.e. were stamped by the import). We reset only
	// 'new' facts so stable facts from a prior pass are untouched.
	facts, err := queries.ListFactsForDedup(ctx, repoID)
	if err != nil {
		log.Printf("reconcile: listing facts for re-embed reset: %v", err)
		return
	}
	var factIDs []pgtype.UUID
	for _, f := range facts {
		if f.Status == "new" && f.EmbeddedAt.Valid {
			factIDs = append(factIDs, f.ID)
		}
	}
	if len(factIDs) > 0 {
		if err := queries.ResetFactEmbeddingForReembed(ctx, factIDs); err != nil {
			log.Printf("reconcile: resetting fact embeddings: %v", err)
		}
	}
}

// EnqueuePlan enqueues the plan's jobs with the given River client,
// respecting ordering: re-embed (which chains to dedup) before a
// direct dedup enqueue. A fresh background ctx is used for the
// Insert so a worker cancellation doesn't cancel the chained enqueue.
//
// sourceIDs is the list of source IDs whose facts were imported.
// ReembedFacts enqueues one embed_facts job PER source (embed_facts
// is source-bounded now — a job only embeds its own source's facts —
// so a repo-wide re-embed must fan out per source). DedupFacts
// enqueues a single repo-wide deduplicate_facts (dedup is repo-wide
// by design). When sourceIDs is empty the re-embed plan is a no-op
// (nothing to embed without a source).
func EnqueuePlan(
	ctx context.Context,
	plan ReconcilePlan,
	repoIDStr string,
	sourceIDs []string,
) {
	if plan.IsEmpty() {
		return
	}
	client := river.ClientFromContext[pgx.Tx](ctx)
	if client == nil || repoIDStr == "" {
		return
	}
	chainCtx, chainCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer chainCancel()

	if plan.ReembedFacts {
		for _, sid := range sourceIDs {
			if _, err := client.Insert(chainCtx, EmbedFactsArgs{
				RepositoryID: repoIDStr,
				SourceID:     sid,
			}, &river.InsertOpts{
				Queue: QueueEmbedFacts,
				Metadata: MarshalMetadata(JobMetadata{
					RepositoryID: repoIDStr,
					SourceID:     sid,
				}),
			}); err != nil {
				log.Printf("reconcile: enqueueing embed_facts for re-embed (source %s): %v", sid, err)
			}
		}
		return
	}

	if plan.DedupFacts {
		if _, err := client.Insert(chainCtx, DeduplicateFactsArgs{
			RepositoryID: repoIDStr,
		}, &river.InsertOpts{
			Queue: QueueDeduplicateFacts,
			Metadata: MarshalMetadata(JobMetadata{
				RepositoryID: repoIDStr,
			}),
		}); err != nil {
			log.Printf("reconcile: enqueueing deduplicate_facts: %v", err)
		}
	}
}