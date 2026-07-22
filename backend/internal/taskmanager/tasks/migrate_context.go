package tasks

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/openktree/open-knowledge-tree/backend/internal/concepts"
	"github.com/openktree/open-knowledge-tree/backend/internal/dbpool"
	"github.com/openktree/open-knowledge-tree/backend/internal/store"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
)

const QueueMigrateContext = "migrate_context"

// MigrateContextArgs re-assigns every concept under OldContext to
// NewContext in the named repository, then removes OldContext from
// repository_contexts. The worker uses merge semantics: for each
// old-context concept, if a (canonical_name, NewContext) concept
// already exists, its fact_concepts + aliases are merged into the
// target and the old row is deleted; otherwise the old row's
// context is plain-UPDATEd. Affected targets are re-embedded and the
// relations matview is refreshed via the chain enqueuer.
//
// (RepositoryID, OldContext, NewContext) is the river unique key so a
// double-click or a re-POST coalesces into one job. ByState excludes
// completed/discarded so a finished migration frees the slot for a
// future one (same rationale as refresh_concept_relations).
type MigrateContextArgs struct {
	RepositoryID string `json:"repository_id" river:"unique"`
	OldContext   string `json:"old_context"   river:"unique"`
	NewContext   string `json:"new_context"    river:"unique"`
}

func (MigrateContextArgs) Kind() string { return "migrate_context" }

func (MigrateContextArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{
		Queue:       QueueMigrateContext,
		MaxAttempts: 3,
		UniqueOpts: river.UniqueOpts{
			ByArgs: true,
			ByQueue: true,
			ByState: []rivertype.JobState{
				rivertype.JobStateAvailable,
				rivertype.JobStatePending,
				rivertype.JobStateRunning,
				rivertype.JobStateScheduled,
				rivertype.JobStateRetryable,
			},
		},
	}
}

// MigrateContextResult is recorded on the job row for observability.
type MigrateContextResult struct {
	RepositoryID string `json:"repository_id"`
	OldContext   string `json:"old_context"`
	NewContext    string `json:"new_context"`
	Reassigned   int    `json:"reassigned"`
	Merged       int    `json:"merged"`
}

// MigrateContextChainEnqueuer is the minimal contract the worker
// needs to fan out post-migration re-processing. The Manager
// implements it so the worker doesn't import the Manager type
// directly (avoids the import cycle the enqueuerAdapter exists to
// break). Nil in unit tests; the worker logs and skips the fan-out.
type MigrateContextChainEnqueuer interface {
	EnqueueEmbedConceptsForRepo(ctx context.Context, repositoryID string) error
	EnqueueRefreshConceptRelationsForRepo(ctx context.Context, repositoryID string) error
}

// MigrateContextWorker needs the registry (to resolve the per-repo
// pool) and the system queries (to look up the repo's database_name
// and to delete the repository_contexts row after the merge).
type MigrateContextWorker struct {
	river.WorkerDefaults[MigrateContextArgs]

	registry      *dbpool.Registry
	systemQueries *store.Queries
	chainEnqueuer MigrateContextChainEnqueuer
}

func NewMigrateContextWorker(registry *dbpool.Registry, systemQueries *store.Queries, chain MigrateContextChainEnqueuer) *MigrateContextWorker {
	return &MigrateContextWorker{registry: registry, systemQueries: systemQueries, chainEnqueuer: chain}
}

// Work runs the merge migration. Steps:
//  1. Resolve the repo's database_name + per-repo pool.
//  2. Load every concept row where context = OldContext.
//  3. For each, look up (canonical_name, NewContext):
//     - exists → re-link fact_concepts onto the target (ON CONFLICT
//       DO NOTHING), copy missing aliases, delete the old row, mark
//       the target for re-embed (embedded_at = NULL).
//     - missing → UPDATE the old row's context to NewContext.
//  4. Delete the OldContext row from repository_contexts.
//  5. Fan out embed_concepts + refresh_concept_relations so vectors
//     and the relations matview reflect the new shape.
//
// All writes for one concept run in one tx so a failure rolls back
// only that concept; the job retries from the top, and the idempotent
// inserts + the ListConceptsByContext NOT EXISTS guard make a retry
// safe.
func (w *MigrateContextWorker) Work(ctx context.Context, job *river.Job[MigrateContextArgs]) error {
	args := job.Args
	if args.RepositoryID == "" || args.OldContext == "" || args.NewContext == "" {
		return fmt.Errorf("migrate_context: repository_id, old_context, and new_context are required")
	}
	if w.registry == nil || w.systemQueries == nil {
		return fmt.Errorf("migrate_context: registry and systemQueries are required")
	}

	repoID := pgtype.UUID{}
	if err := repoID.Scan(args.RepositoryID); err != nil {
		return fmt.Errorf("migrate_context: invalid repository_id: %w", err)
	}

	dbName, err := w.systemQueries.GetRepositoryDatabaseName(ctx, repoID)
	if err != nil {
		return fmt.Errorf("migrate_context: resolving repository database: %w", err)
	}
	pool := w.registry.Get(dbName)
	if pool == nil || pool.Pool == nil {
		return fmt.Errorf("migrate_context: no pool for database %q", dbName)
	}

	result := MigrateContextResult{RepositoryID: args.RepositoryID, OldContext: args.OldContext, NewContext: args.NewContext}
	queries := store.New(pool.Pool)

	oldConcepts, err := queries.ListConceptsByContext(ctx, store.ListConceptsByContextParams{
		RepositoryID: repoID,
		Context:      args.OldContext,
	})
	if err != nil {
		return fmt.Errorf("migrate_context: listing old-context concepts: %w", err)
	}

	// touchedNameKeys collects the lower(canonical_name) groups
	// affected by this migration (merge or plain reassign) so the
	// concept_groups summary can be recomputed once at the end. A
	// name may appear more than once (multiple contexts); the dedup
	// happens in RecomputeTouchedGroups.
	touchedNameKeys := make([]string, 0, len(oldConcepts))
	for _, old := range oldConcepts {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("migrate_context: ctx cancelled: %w", err)
		}
		tx, err := pool.Pool.BeginTx(ctx, pgx.TxOptions{})
		if err != nil {
			return fmt.Errorf("migrate_context: beginning tx: %w", err)
		}
		txQueries := store.New(tx)
		reassigned, merged, mErr := w.migrateOneConcept(ctx, txQueries, repoID, old, args.NewContext)
		if mErr != nil {
			tx.Rollback(context.Background())
			return fmt.Errorf("migrate_context: merging concept %s: %w", old.ID, mErr)
		}
		if err := tx.Commit(ctx); err != nil {
			return fmt.Errorf("migrate_context: committing concept %s: %w", old.ID, err)
		}
		result.Reassigned += reassigned
		result.Merged += merged
		touchedNameKeys = append(touchedNameKeys, strings.ToLower(old.CanonicalName))
	}

	// Recompute the concept_groups summary for the touched name keys
	// so the q="" concept list page reflects the new context counts
	// and any merge-deleted groups immediately. Best-effort.
	if len(touchedNameKeys) > 0 {
		recomputeCtx, recomputeCancel := context.WithTimeout(context.Background(), 30*time.Second)
		if rerr := concepts.RecomputeTouchedGroups(recomputeCtx, queries, repoID, touchedNameKeys); rerr != nil {
			log.Printf("migrate_context: recompute concept_groups for repo %s: %v", args.RepositoryID, rerr)
		}
		recomputeCancel()
	}

	// Delete the old context from repository_contexts now that no
	// concepts reference it. Best-effort: a failure is logged but
	// doesn't fail the job (the merge already succeeded; a lingering
	// empty context row is cosmetic and the admin can delete it via
	// the UI).
	if err := w.systemQueries.DeleteRepositoryContext(ctx, store.DeleteRepositoryContextParams{
		RepositoryID: repoID,
		Context:      args.OldContext,
	}); err != nil {
		log.Printf("migrate_context: deleting old context %q from repository_contexts: %v (merge already complete)", args.OldContext, err)
	}

	// Fan out re-embed + relations refresh so vectors + the matview
	// reflect the new shape. Best-effort: logged on failure.
	if w.chainEnqueuer != nil {
		if err := w.chainEnqueuer.EnqueueEmbedConceptsForRepo(ctx, args.RepositoryID); err != nil {
			log.Printf("migrate_context: enqueueing embed_concepts for repo %s: %v", args.RepositoryID, err)
		}
		if err := w.chainEnqueuer.EnqueueRefreshConceptRelationsForRepo(ctx, args.RepositoryID); err != nil {
			log.Printf("migrate_context: enqueueing refresh_concept_relations for repo %s: %v", args.RepositoryID, err)
		}
	}

	log.Printf("migrate_context: repo %s %q → %q: reassigned=%d merged=%d",
		args.RepositoryID, args.OldContext, args.NewContext, result.Reassigned, result.Merged)
	return river.RecordOutput(ctx, &result)
}

// migrateOneConcept handles one old-context concept: merge into an
// existing (canonical_name, new_context) target if one exists, else
// plain UPDATE the old row's context. Returns (reassigned, merged)
// flags for the result counter.
func (w *MigrateContextWorker) migrateOneConcept(ctx context.Context, q *store.Queries, repoID pgtype.UUID, old store.OktRepositoryConcept, newContext string) (int, int, error) {
	target, err := q.GetConceptByNameContext(ctx, store.GetConceptByNameContextParams{
		RepositoryID:  repoID,
		CanonicalName: old.CanonicalName,
		Context:       newContext,
	})
	if err == nil {
		// Merge: re-link fact_concepts onto the target, copy missing
		// aliases, delete the old row, mark the target for re-embed.
		if err := q.ReassignFactConceptsToConcept(ctx, store.ReassignFactConceptsToConceptParams{
			OldConceptID: old.ID,
			NewConceptID: target.ID,
		}); err != nil {
			return 0, 0, fmt.Errorf("re-linking fact_concepts: %w", err)
		}
		// Copy old row's aliases onto the target before deleting the
		// old row (concept_aliases cascade-delete with the concept,
		// so copy first).
		oldAliases, aerr := q.ListConceptAliasesByConcept(ctx, old.ID)
		if aerr != nil {
			return 0, 0, fmt.Errorf("listing old aliases: %w", aerr)
		}
		for _, a := range oldAliases {
			if _, err := q.AddConceptAlias(ctx, store.AddConceptAliasParams{
				ConceptID: target.ID,
				AliasText: a.AliasText,
			}); err != nil && !errors.Is(err, pgx.ErrNoRows) {
				log.Printf("migrate_context: copying alias %q onto target: %v", a.AliasText, err)
			}
		}
		if err := q.DeleteConceptByID(ctx, old.ID); err != nil {
			return 0, 0, fmt.Errorf("deleting old concept row: %w", err)
		}
		// Force the target to re-embed (its fact set + alias set
		// changed, so its vector is stale).
		if err := q.ResetConceptEmbedding(ctx, target.ID); err != nil {
			return 0, 0, fmt.Errorf("resetting target embedding: %w", err)
		}
		return 0, 1, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return 0, 0, fmt.Errorf("looking up target (name, new_context): %w", err)
	}
	// No target: plain UPDATE the old row's context. Safe because
	// the unique index on (repo, lower(name), lower(context))
	// guarantees no (name, new_context) row exists (we just checked).
	if err := q.UpdateConceptContext(ctx, store.UpdateConceptContextParams{
		ID:      old.ID,
		Context: newContext,
	}); err != nil {
		return 0, 0, fmt.Errorf("updating concept context: %w", err)
	}
	return 1, 0, nil
}

var _ river.Worker[MigrateContextArgs] = (*MigrateContextWorker)(nil)