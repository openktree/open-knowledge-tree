package tasks

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/openktree/open-knowledge-tree/backend/internal/concepts"
	"github.com/openktree/open-knowledge-tree/backend/internal/config"
	"github.com/openktree/open-knowledge-tree/backend/internal/dbpool"
	"github.com/openktree/open-knowledge-tree/backend/internal/promptset"
	"github.com/openktree/open-knowledge-tree/backend/internal/providers/ai"
	"github.com/openktree/open-knowledge-tree/backend/internal/providers/refinement"
	"github.com/openktree/open-knowledge-tree/backend/internal/qdrantstore"
	"github.com/openktree/open-knowledge-tree/backend/internal/store"
	"github.com/riverqueue/river"
	"golang.org/x/sync/errgroup"
)

const QueueRefineConcepts = "refine_concepts"

// RefineConceptsArgs triggers a refinement pass for a chunk of
// unresolved concept_candidates in a repository. extract_concepts
// fans out one RefineConcepts job per MaxCandidatesPerRun chunk of
// touched candidates; each chunk runs before summarize_concepts so
// the summarizer sees the final canonical names. CandidateIDs is the
// explicit chunk the worker processes — the worker does NOT list
// candidates itself, it trusts the caller's chunk so two chunks
// never overlap. When CandidateIDs is empty, the worker falls back
// to listing unresolved candidates itself (manual re-enqueue /
// periodic catch-up).
type RefineConceptsArgs struct {
	RepositoryID string   `json:"repository_id"`
	SourceID     string   `json:"source_id,omitempty"`
	CandidateIDs []string `json:"candidate_ids"`
}

func (RefineConceptsArgs) Kind() string { return "refine_concepts" }

func (RefineConceptsArgs) InsertOpts() river.InsertOpts { return river.InsertOpts{} }

// RefineConceptsResult is recorded on the job row so the River UI
// shows what the pass did. Counters are int64 so they can be mutated
// concurrently via atomic.AddInt64 when the worker processes
// candidates in parallel (errgroup with bounded concurrency).
type RefineConceptsResult struct {
	RepositoryID         string  `json:"repository_id"`
	CandidatesResolved  int64   `json:"candidates_resolved"`
	ConceptsCreated     int64   `json:"concepts_created"`
	ConceptsMerged      int64   `json:"concepts_merged"`
	AliasesAdded        int64   `json:"aliases_added"`
	AliasesPruned       int64   `json:"aliases_pruned"`
	Errors              int64   `json:"errors"`
}

type RefineConceptsWorker struct {
	river.WorkerDefaults[RefineConceptsArgs]

	refiner           refinement.RefineProvider
	cfg               config.RefinementConfig
	registry          *dbpool.Registry
	systemQueries     *store.Queries
	modelResolver     *ModelResolver
	promptsetResolver *PromptsetResolver
	summarizerEnabled bool
	concurrency       int
	// embeddingProvider + qdrant power the per-fact embedding
	// tie-break when an alias matches multiple concepts (see
	// concepts.ResolveAliasMatchForFact). Optional: when nil, a
	// multi-match defers the fact (no cosine comparison) rather than
	// mis-routing on an arbitrary first row.
	embeddingProvider ai.EmbeddingProvider
	embeddingCfg      config.EmbeddingConfig
	qdrant            *qdrantstore.Store
}

func NewRefineConceptsWorker(
	refiner refinement.RefineProvider,
	cfg config.RefinementConfig,
	registry *dbpool.Registry,
	systemQueries *store.Queries,
	summarizerEnabled bool,
	modelResolver *ModelResolver,
	promptsetResolver *PromptsetResolver,
	embeddingProvider ai.EmbeddingProvider,
	embeddingCfg config.EmbeddingConfig,
	qdrant *qdrantstore.Store,
) *RefineConceptsWorker {
	return &RefineConceptsWorker{
		refiner:           refiner,
		cfg:               cfg,
		registry:          registry,
		systemQueries:     systemQueries,
		summarizerEnabled: summarizerEnabled,
		modelResolver:     modelResolver,
		promptsetResolver: promptsetResolver,
		concurrency:       cfg.MaxConcurrencyOr(5),
		embeddingProvider: embeddingProvider,
		embeddingCfg:      embeddingCfg,
		qdrant:            qdrant,
	}
}

// SetSummarizerEnabled lets the wiring layer gate the summarize
// fan-out without exposing the summarizer instance.
func (w *RefineConceptsWorker) SetSummarizerEnabled(v bool) {
	w.summarizerEnabled = v
}

// Work processes the chunk of CandidateIDs in args.CandidateIDs. For
// each unresolved candidate:
//  1. Pre-LLM routing: exact canonical match, alias match, seed-alias
//     match — all same-context DB lookups, no LLM. If any hits,
//     merge the candidate into the existing concept.
//  2. If pre-LLM routing misses, one LLM call proposes canonical_name
//     + aliases_to_add + aliases_to_prune.
//  3. Post-LLM routing: AI canonical match, AI alias match — same-
//     context DB lookups reusing the LLM output. If any hits, merge.
//  4. If all miss, create a new concept with the AI canonical name +
//     AI aliases + seed aliases.
//  5. Resolve: move fact_candidates → fact_concepts, set
//     resolved_concept_id on the candidate (cache entry).
//  6. Apply pruning (delete aliases_to_prune).
//  7. Chain to summarize_concepts for resolved concept_ids.
//
// The worker is a no-op when refiner == nil or cfg.Enabled is false
// (deployment choice, not retryable).
func (w *RefineConceptsWorker) Work(ctx context.Context, job *river.Job[RefineConceptsArgs]) error {
	args := job.Args
	if args.RepositoryID == "" {
		return fmt.Errorf("refine_concepts: repository_id is required")
	}

	if w.refiner == nil {
		log.Printf("refine_concepts: refinement provider not configured, skipping repo %s", args.RepositoryID)
		return river.RecordOutput(ctx, &RefineConceptsResult{RepositoryID: args.RepositoryID})
	}
	if !w.cfg.Enabled {
		log.Printf("refine_concepts: refinement not enabled, skipping repo %s", args.RepositoryID)
		return river.RecordOutput(ctx, &RefineConceptsResult{RepositoryID: args.RepositoryID})
	}

	repoID := pgtype.UUID{}
	if err := repoID.Scan(args.RepositoryID); err != nil {
		return fmt.Errorf("refine_concepts: invalid repository_id: %w", err)
	}

	dbName, err := w.systemQueries.GetRepositoryDatabaseName(ctx, repoID)
	if err != nil {
		return fmt.Errorf("refine_concepts: resolving repository database: %w", err)
	}
	pool := w.registry.Get(dbName)
	if pool == nil || pool.Pool == nil {
		return fmt.Errorf("refine_concepts: no pool for database %q", dbName)
	}

	// Resolve the repo's effective promptset once at Work() start.
	// The hash tags every concept + fact_concept link this job
	// persists so decompositions from different promptsets do not
	// mix. The resolved Promptset is threaded into the refiner via
	// WithPromptset.
	var ps promptset.Promptset
	var psHash string
	if w.promptsetResolver != nil {
		ps = w.promptsetResolver.Effective(ctx, repoID)
		psHash = ps.Hash
		w.promptsetResolver.LogEffective(ctx, repoID, "refine_concepts")
	} else {
		ps = promptset.Default
		psHash = promptset.DefaultHash
	}
	psHashPtr := &psHash

	// Resolve per-repo model override for refinement.
	var refinementModelOverride string
	refiner := w.refiner
	if w.modelResolver != nil {
		if r := w.modelResolver.Resolve(ctx, repoID, TaskKindRefinement); r.Provider != nil {
			refinementModelOverride = r.ModelID
			refiner = refinement.NewAIRefineProvider(r.Provider, r.ModelID).WithPromptset(ps)
		}
	}
	if rf, ok := refiner.(*refinement.AIRefineProvider); ok {
		refiner = rf.WithPromptset(ps)
	}

	maxTokens := w.cfg.MaxTokensOr(400)
	taskID := fmt.Sprintf("%d", job.ID)
	result := RefineConceptsResult{RepositoryID: args.RepositoryID}

	// If the caller passed no CandidateIDs (manual re-enqueue), fall
	// back to listing unresolved candidates for this source (or
	// repo-wide when SourceID is empty).
	candidateIDs := args.CandidateIDs
	if len(candidateIDs) == 0 {
		queries := store.New(pool.Pool)
		listCtx, listCancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer listCancel()
		var rows []store.OktRepositoryConceptCandidate
		if args.SourceID != "" {
			var srcID pgtype.UUID
			if err := srcID.Scan(args.SourceID); err != nil {
				return fmt.Errorf("refine_concepts: invalid source_id: %w", err)
			}
			rows, err = queries.ListUnresolvedCandidatesBySource(listCtx, srcID)
		} else {
			rows, err = queries.ListUnresolvedCandidatesByRepo(listCtx, repoID)
		}
		listCancel()
		if err != nil {
			return fmt.Errorf("refine_concepts: listing unresolved candidates: %w", err)
		}
		for _, r := range rows {
			candidateIDs = append(candidateIDs, pgUUIDToString(r.ID))
		}
	}

	// Track resolved concept_ids for the summarize chain.
	resolvedConceptIDs := make([]pgtype.UUID, 0, len(candidateIDs))
	var idsMu sync.Mutex

	// Process candidates concurrently. Each candidate is independent
	// (the per-canonical advisory lock inside refineOneCandidate
	// prevents two goroutines from racing on the same AI-proposed
	// canonical). Concurrency is bounded by w.concurrency so a
	// 40-candidate chunk with ~30 LLM calls drains in ~6 concurrent
	// waves instead of 30 serial calls. Per-candidate errors are
	// best-effort (log + atomic increment, don't fail the group) so
	// one bad candidate never aborts the chunk.
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(w.concurrency)
	for _, cidStr := range candidateIDs {
		cidStr := cidStr
		g.Go(func() error {
			if err := gctx.Err(); err != nil {
				return err
			}
			var candidateID pgtype.UUID
			if err := candidateID.Scan(cidStr); err != nil {
				atomic.AddInt64(&result.Errors, 1)
				log.Printf("refine_concepts: invalid candidate_id %q: %v", cidStr, err)
				return nil
			}
			resolvedConceptID, rerr := w.refineOneCandidate(gctx, pool.Pool, repoID, candidateID, args.RepositoryID, args.SourceID, taskID, maxTokens, refinementModelOverride, &result, refiner, psHashPtr)
			if rerr != nil {
				atomic.AddInt64(&result.Errors, 1)
				log.Printf("refine_concepts: refining candidate %s: %v", cidStr, rerr)
				return nil
			}
			if resolvedConceptID.Valid {
				idsMu.Lock()
				resolvedConceptIDs = append(resolvedConceptIDs, resolvedConceptID)
				idsMu.Unlock()
			}
			return nil
		})
	}
	_ = g.Wait()

	log.Printf("refine_concepts: repo %s resolved %d candidates (%d created, %d merged, %d aliases added, %d pruned, %d errors)",
		args.RepositoryID, result.CandidatesResolved, result.ConceptsCreated, result.ConceptsMerged, result.AliasesAdded, result.AliasesPruned, result.Errors)

	// Chain to summarize_concepts for the resolved concept_ids.
	if w.summarizerEnabled {
		w.enqueueSummarizeConcepts(ctx, args.RepositoryID, args.SourceID, resolvedConceptIDs)
	}

	return river.RecordOutput(ctx, &result)
}

// refineOneCandidate handles the full pre-LLM → LLM → post-LLM →
// resolve cycle for one candidate. Returns the resolved concept_id
// (so the caller can chain summarize) and updates the result counters.
func (w *RefineConceptsWorker) refineOneCandidate(
	ctx context.Context,
	pool *pgxpool.Pool,
	repoID pgtype.UUID,
	candidateID pgtype.UUID,
	repoIDStr, sourceIDStr, taskID string,
	maxTokens int,
	modelOverride string,
	result *RefineConceptsResult,
	refiner refinement.RefineProvider,
	psHashPtr *string,
) (pgtype.UUID, error) {
	queries := store.New(pool)

	candidate, err := queries.GetConceptCandidateByID(ctx, candidateID)
	if err != nil {
		return pgtype.UUID{}, fmt.Errorf("loading candidate: %w", err)
	}
	if candidate.ResolvedConceptID.Valid {
		// Already resolved (e.g. a prior pass raced us). Nothing to do.
		return candidate.ResolvedConceptID, nil
	}

	// ---- Pre-LLM routing (all DB, no LLM) ----
	// Step 1: exact canonical match (same context).
	if target, hit := w.findConceptByCanonical(ctx, queries, repoID, candidate.Context, candidate.ConceptText); hit {
		merged, err := w.mergeCandidateIntoConcept(ctx, pool, queries, candidate, target, nil, result)
		if err != nil {
			return pgtype.UUID{}, err
		}
		return merged, nil
	}

	// Step 2: existing alias match (same context). When the alias is
	// shared by multiple concepts, fork into per-fact routing (each
	// fact goes to its cosine-closest concept); otherwise the legacy
	// single-target merge.
	if matches := w.findConceptsByAlias(ctx, queries, repoID, candidate.Context, candidate.ConceptText); len(matches) == 1 {
		merged, err := w.mergeCandidateIntoConcept(ctx, pool, queries, candidate, matches[0], nil, result)
		if err != nil {
			return pgtype.UUID{}, err
		}
		return merged, nil
	} else if len(matches) > 1 {
		resolved, handled, err := w.tryRouteAliasAmbiguous(ctx, pool, queries, repoID, candidate, matches, nil, result, psHashPtr)
		if err != nil {
			return pgtype.UUID{}, err
		}
		if handled {
			return resolved, nil
		}
	}

	// Step 3: seed alias match (same context).
	for _, seedAlias := range candidate.SeedAliases {
		if seedAlias == "" {
			continue
		}
		if target, hit := w.findConceptByCanonical(ctx, queries, repoID, candidate.Context, seedAlias); hit {
			merged, err := w.mergeCandidateIntoConcept(ctx, pool, queries, candidate, target, nil, result)
			if err != nil {
				return pgtype.UUID{}, err
			}
			return merged, nil
		}
		if matches := w.findConceptsByAlias(ctx, queries, repoID, candidate.Context, seedAlias); len(matches) == 1 {
			merged, err := w.mergeCandidateIntoConcept(ctx, pool, queries, candidate, matches[0], nil, result)
			if err != nil {
				return pgtype.UUID{}, err
			}
			return merged, nil
		} else if len(matches) > 1 {
			resolved, handled, err := w.tryRouteAliasAmbiguous(ctx, pool, queries, repoID, candidate, matches, nil, result, psHashPtr)
			if err != nil {
				return pgtype.UUID{}, err
			}
			if handled {
				return resolved, nil
			}
		}
	}

	// ---- LLM call (only if pre-LLM routing all misses) ----
	llmCtx, llmCancel := context.WithTimeout(context.Background(), 120*time.Second)
	refineModel := modelOverride
	if refineModel == "" {
		refineModel = w.cfg.Model
	}
	refineResult, rerr := refiner.Refine(llmCtx, pool, refinement.RefinementRequest{
		Concept:         candidate.ConceptText,
		Context:         candidate.Context,
		ExistingAliases: nil, // new candidate has no aliases yet
		SeedAliases:     candidate.SeedAliases,
		Model:           refineModel,
		MaxTokens:       maxTokens,
		TaskID:          taskID,
		Attribution: ai.Attribution{
			RepositoryID: repoIDStr,
			SourceID:     sourceIDStr,
			Operation:    "alias_generation",
		},
	})
	llmCancel()
	if rerr != nil {
		return pgtype.UUID{}, fmt.Errorf("refinement LLM call: %w", rerr)
	}

	// ---- Post-LLM routing (all DB, reuses LLM output) ----
	// Acquire advisory lock on (repo, lower(ai_canonical)) to prevent
	// a parallel refinement from racing on the same canonical.
	lockKey := repoIDStr + ":" + strings.ToLower(refineResult.CanonicalName)
	lockCtx, lockCancel := context.WithTimeout(context.Background(), 10*time.Second)
	locked, lerr := queries.TryAdvisoryLockForSynthesis(lockCtx, lockKey)
	lockCancel()
	if lerr != nil {
		return pgtype.UUID{}, fmt.Errorf("acquiring advisory lock: %w", lerr)
	}
	if !locked {
		// Another worker holds the lock; skip this candidate for now.
		// The next pass will retry.
		log.Printf("refine_concepts: advisory lock held for %s; skipping candidate %s", lockKey, pgUUIDToString(candidateID))
		return pgtype.UUID{}, nil
	}
	defer func() {
		relCtx, relCancel := context.WithTimeout(context.Background(), 10*time.Second)
		if err := store.New(pool).ReleaseAdvisoryLockForSynthesis(relCtx, lockKey); err != nil {
			log.Printf("refine_concepts: releasing advisory lock for %s: %v", lockKey, err)
		}
		relCancel()
	}()

	// Step 4: AI canonical match (same context).
	if target, hit := w.findConceptByCanonical(ctx, queries, repoID, candidate.Context, refineResult.CanonicalName); hit {
		merged, err := w.mergeCandidateIntoConcept(ctx, pool, queries, candidate, target, refineResult.AliasesToAdd, result)
		if err != nil {
			return pgtype.UUID{}, err
		}
		w.applyPruning(ctx, queries, target.ID, refineResult.AliasesToPrune, result)
		return merged, nil
	}

	// Step 5: AI alias match (same context).
	for _, aiAlias := range refineResult.AliasesToAdd {
		if aiAlias == "" {
			continue
		}
		if target, hit := w.findConceptByCanonical(ctx, queries, repoID, candidate.Context, aiAlias); hit {
			merged, err := w.mergeCandidateIntoConcept(ctx, pool, queries, candidate, target, refineResult.AliasesToAdd, result)
			if err != nil {
				return pgtype.UUID{}, err
			}
			w.applyPruning(ctx, queries, target.ID, refineResult.AliasesToPrune, result)
			return merged, nil
		}
		if matches := w.findConceptsByAlias(ctx, queries, repoID, candidate.Context, aiAlias); len(matches) == 1 {
			merged, err := w.mergeCandidateIntoConcept(ctx, pool, queries, candidate, matches[0], refineResult.AliasesToAdd, result)
			if err != nil {
				return pgtype.UUID{}, err
			}
			w.applyPruning(ctx, queries, matches[0].ID, refineResult.AliasesToPrune, result)
			return merged, nil
		} else if len(matches) > 1 {
			resolved, handled, err := w.tryRouteAliasAmbiguous(ctx, pool, queries, repoID, candidate, matches, refineResult.AliasesToAdd, result, psHashPtr)
			if err != nil {
				return pgtype.UUID{}, err
			}
			if handled {
				// Pruning applies to all winning concepts.
				for _, m := range matches {
					w.applyPruning(ctx, queries, m.ID, refineResult.AliasesToPrune, result)
				}
				return resolved, nil
			}
		}
	}

	// Step 6: genuinely new concept — create it.
	created, err := queries.CreateConcept(ctx, store.CreateConceptParams{
		RepositoryID:  repoID,
		CanonicalName: refineResult.CanonicalName,
		Context:       candidate.Context,
		PromptsetHash: psHashPtr,
	})
	if err != nil {
		// ON CONFLICT DO NOTHING may return ErrNoRows if a racing insert
		// created the same (repo, canonical, context). Re-find and merge.
		if errors.Is(err, pgx.ErrNoRows) {
			target, found := w.findConceptByCanonical(ctx, queries, repoID, candidate.Context, refineResult.CanonicalName)
			if !found || !target.ID.Valid {
				return pgtype.UUID{}, fmt.Errorf("create concept conflict + re-find failed: %w", err)
			}
			merged, merr := w.mergeCandidateIntoConcept(ctx, pool, queries, candidate, target, refineResult.AliasesToAdd, result)
			if merr != nil {
				return pgtype.UUID{}, merr
			}
			w.applyPruning(ctx, queries, target.ID, refineResult.AliasesToPrune, result)
			return merged, nil
		}
		return pgtype.UUID{}, fmt.Errorf("create concept: %w", err)
	}
	atomic.AddInt64(&result.ConceptsCreated, 1)

	// Insert AI aliases + seed aliases + canonical name + concept text.
	aliasSet := make(map[string]struct{})
	aliasSet[refineResult.CanonicalName] = struct{}{}
	aliasSet[candidate.ConceptText] = struct{}{}
	for _, a := range refineResult.AliasesToAdd {
		if a != "" {
			aliasSet[a] = struct{}{}
		}
	}
	for _, a := range candidate.SeedAliases {
		if a != "" {
			aliasSet[a] = struct{}{}
		}
	}
	for alias := range aliasSet {
		if _, err := queries.AddConceptAlias(ctx, store.AddConceptAliasParams{
			ConceptID: created.ID,
			AliasText: alias,
		}); err != nil && !errors.Is(err, pgx.ErrNoRows) {
			log.Printf("refine_concepts: inserting alias %q for concept %s: %v", alias, pgUUIDToString(created.ID), err)
		}
		atomic.AddInt64(&result.AliasesAdded, 1)
	}
	if err := queries.SetConceptRefinedAt(ctx, created.ID); err != nil {
		log.Printf("refine_concepts: setting refined_at for concept %s: %v", pgUUIDToString(created.ID), err)
	}

	// Apply pruning (first refinement has nothing to prune, but the
	// call is harmless).
	w.applyPruning(ctx, queries, created.ID, refineResult.AliasesToPrune, result)

	// Re-link fact_candidates → fact_concepts for the new concept.
	if err := queries.ReassignFactCandidatesToConcept(ctx, store.ReassignFactCandidatesToConceptParams{
		OldCandidateID: candidateID,
		NewConceptID:   created.ID,
	}); err != nil {
		return pgtype.UUID{}, fmt.Errorf("reassigning fact_candidates to new concept: %w", err)
	}

	// Resolve: clear fact_candidates, set resolved_concept_id (cache).
	if err := w.resolveCandidate(ctx, pool, queries, candidateID, created.ID); err != nil {
		return pgtype.UUID{}, fmt.Errorf("resolving candidate: %w", err)
	}
	atomic.AddInt64(&result.CandidatesResolved, 1)

	return created.ID, nil
}

// mergeCandidateIntoConcept moves the candidate's facts onto the
// target concept, copies seed + AI aliases, resolves the candidate
// (cache entry), resets the target's embedding, and deletes any
// stale synthesis row under the candidate's concept_text. Returns
// the target concept_id.
func (w *RefineConceptsWorker) mergeCandidateIntoConcept(
	ctx context.Context,
	pool *pgxpool.Pool,
	queries *store.Queries,
	candidate store.OktRepositoryConceptCandidate,
	target store.OktRepositoryConcept,
	aiAliases []string,
	result *RefineConceptsResult,
) (pgtype.UUID, error) {
	// Re-link fact_candidates → fact_concepts.
	if err := queries.ReassignFactCandidatesToConcept(ctx, store.ReassignFactCandidatesToConceptParams{
		OldCandidateID: candidate.ID,
		NewConceptID:   target.ID,
	}); err != nil {
		return pgtype.UUID{}, fmt.Errorf("reassigning fact_candidates: %w", err)
	}

	// Copy seed aliases onto the target.
	for _, alias := range candidate.SeedAliases {
		if alias == "" {
			continue
		}
		if _, err := queries.AddConceptAlias(ctx, store.AddConceptAliasParams{
			ConceptID: target.ID,
			AliasText: alias,
		}); err != nil && !errors.Is(err, pgx.ErrNoRows) {
			log.Printf("refine_concepts: copying seed alias %q onto target %s: %v", alias, pgUUIDToString(target.ID), err)
		}
		atomic.AddInt64(&result.AliasesAdded, 1)
	}

	// Copy AI aliases onto the target.
	for _, alias := range aiAliases {
		if alias == "" {
			continue
		}
		if _, err := queries.AddConceptAlias(ctx, store.AddConceptAliasParams{
			ConceptID: target.ID,
			AliasText: alias,
		}); err != nil && !errors.Is(err, pgx.ErrNoRows) {
			log.Printf("refine_concepts: copying AI alias %q onto target %s: %v", alias, pgUUIDToString(target.ID), err)
		}
		atomic.AddInt64(&result.AliasesAdded, 1)
	}

	// Also add the concept_text itself as an alias (it's the surface
	// form future extractions will match on).
	if _, err := queries.AddConceptAlias(ctx, store.AddConceptAliasParams{
		ConceptID: target.ID,
		AliasText: candidate.ConceptText,
	}); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		log.Printf("refine_concepts: copying concept_text %q onto target %s: %v", candidate.ConceptText, pgUUIDToString(target.ID), err)
	}

	// Reset the target's embedding (its fact set changed).
	if err := queries.ResetConceptEmbedding(ctx, target.ID); err != nil {
		log.Printf("refine_concepts: resetting embedding for target %s: %v", pgUUIDToString(target.ID), err)
	}

	// Resolve: clear fact_candidates, set resolved_concept_id (cache).
	if err := w.resolveCandidate(ctx, pool, queries, candidate.ID, target.ID); err != nil {
		return pgtype.UUID{}, fmt.Errorf("resolving candidate: %w", err)
	}

	atomic.AddInt64(&result.ConceptsMerged, 1)
	atomic.AddInt64(&result.CandidatesResolved, 1)
	return target.ID, nil
}

// resolveCandidate clears the candidate's fact_candidates rows and
// marks the candidate as resolved (cache entry for future
// extractions). Runs in a short tx so the two writes are atomic.
func (w *RefineConceptsWorker) resolveCandidate(ctx context.Context, pool *pgxpool.Pool, queries *store.Queries, candidateID, conceptID pgtype.UUID) error {
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("beginning resolve tx: %w", err)
	}
	defer tx.Rollback(context.Background())
	txQueries := store.New(tx)
	if err := txQueries.DeleteFactCandidatesByCandidate(ctx, candidateID); err != nil {
		return fmt.Errorf("deleting fact_candidates: %w", err)
	}
	if err := txQueries.ResolveCandidate(ctx, store.ResolveCandidateParams{
		ID:                candidateID,
		ResolvedConceptID: conceptID,
	}); err != nil {
		return fmt.Errorf("resolving candidate: %w", err)
	}
	return tx.Commit(ctx)
}

// applyPruning deletes each alias in pruneList from the concept.
func (w *RefineConceptsWorker) applyPruning(ctx context.Context, queries *store.Queries, conceptID pgtype.UUID, pruneList []string, result *RefineConceptsResult) {
	for _, alias := range pruneList {
		if alias == "" {
			continue
		}
		if err := queries.DeleteConceptAliasByText(ctx, store.DeleteConceptAliasByTextParams{
			ConceptID: conceptID,
			AliasText: alias,
		}); err != nil {
			log.Printf("refine_concepts: pruning alias %q from concept %s: %v", alias, pgUUIDToString(conceptID), err)
			continue
		}
		atomic.AddInt64(&result.AliasesPruned, 1)
	}
}

// findConceptByCanonical is a thin wrapper around
// FindConceptByCanonical that converts pgx.ErrNoRows to (zero, false).
func (w *RefineConceptsWorker) findConceptByCanonical(ctx context.Context, queries *store.Queries, repoID pgtype.UUID, context, name string) (store.OktRepositoryConcept, bool) {
	c, err := queries.FindConceptByCanonical(ctx, store.FindConceptByCanonicalParams{
		RepositoryID: repoID,
		Context:      context,
		Name:         name,
	})
	if err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			log.Printf("refine_concepts: FindConceptByCanonical: %v", err)
		}
		return store.OktRepositoryConcept{}, false
	}
	return c, true
}

// findConceptByAlias is a thin wrapper around FindConceptByAlias
// (same-context) that converts pgx.ErrNoRows to (zero, false).
//
// NOTE: this returns the LIMIT-1 first match. It is only used by
// refine_concepts for the single-target merge path. When an alias
// may be shared by multiple concepts, use findConceptsByAlias + the
// per-fact disambiguation branch (routeAliasAmbiguous) instead.
func (w *RefineConceptsWorker) findConceptByAlias(ctx context.Context, queries *store.Queries, repoID pgtype.UUID, context, name string) (store.OktRepositoryConcept, bool) {
	c, err := queries.FindConceptByAlias(ctx, store.FindConceptByAliasParams{
		RepositoryID: repoID,
		Context:      context,
		Name:         name,
	})
	if err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			log.Printf("refine_concepts: FindConceptByAlias: %v", err)
		}
		return store.OktRepositoryConcept{}, false
	}
	return c, true
}

// findConceptsByAlias wraps FindConceptsByAlias (:many). Returns the
// full match set so the caller can detect ambiguity (>1 match) and
// fork into per-fact routing. 0 or 1 match means no ambiguity and
// the caller takes the legacy single-target merge path.
func (w *RefineConceptsWorker) findConceptsByAlias(ctx context.Context, queries *store.Queries, repoID pgtype.UUID, context, name string) []store.OktRepositoryConcept {
	matches, err := queries.FindConceptsByAlias(ctx, store.FindConceptsByAliasParams{
		RepositoryID: repoID,
		Context:      context,
		Name:         name,
	})
	if err != nil {
		log.Printf("refine_concepts: FindConceptsByAlias: %v", err)
		return nil
	}
	return matches
}

// tryRouteAliasAmbiguous handles the case where a (name, context)
// lookup matches MORE than one existing concept (an alias shared by
// multiple concepts, e.g. "N" on both Nitrogen and Neutron). It
// routes each of the candidate's facts individually to the concept
// whose Qdrant vector is cosine-closest to that fact's vector (via
// concepts.ResolveAliasMatchForFact), so facts about nitrogen go to
// Nitrogen and facts about a neutron go to Neutron.
//
// Returns (resolvedConceptID, handled, error):
//   - handled=false when matches is 0 or 1 (no ambiguity; the caller
//     takes the legacy single-target path).
//   - handled=true  when the multi-match branch ran. resolvedConceptID
//     is Valid only when a single concept won ALL facts (the candidate
//     is resolved-cached to it); when facts split or any fact deferred,
//     the candidate is deleted (facts split) or kept unresolved (deferred),
//     and resolvedConceptID is invalid.
//
// Facts whose Qdrant vector is unavailable are deferred: they stay on
// the candidate (fact_candidates row intact) and the candidate is kept
// unresolved so the next refine pass retries them once embedded.
//
// aiAliases + candidate.SeedAliases + candidate.ConceptText are merged
// onto every winning concept (same alias-copying mergeCandidateIntoConcept
// does), and each winning concept's embedding is reset (its fact set
// changed) so embed_concepts re-embeds it.
func (w *RefineConceptsWorker) tryRouteAliasAmbiguous(
	ctx context.Context,
	pool *pgxpool.Pool,
	queries *store.Queries,
	repoID pgtype.UUID,
	candidate store.OktRepositoryConceptCandidate,
	matches []store.OktRepositoryConcept,
	aiAliases []string,
	result *RefineConceptsResult,
	psHashPtr *string,
) (resolvedConceptID pgtype.UUID, handled bool, err error) {
	if len(matches) <= 1 {
		return pgtype.UUID{}, false, nil
	}

	// List the candidate's facts to route each individually.
	factIDs, err := queries.ListFactIDsByCandidate(ctx, candidate.ID)
	if err != nil {
		return pgtype.UUID{}, false, fmt.Errorf("listing facts for candidate %s: %w", pgUUIDToString(candidate.ID), err)
	}

	embeddingModel := w.embeddingCfg.Model
	winners := make(map[pgtype.UUID]struct{}) // concept IDs that won >=1 fact
	deferredCount := 0
	routedCount := 0

	for _, factID := range factIDs {
		winner, ok := concepts.ResolveAliasMatchForFact(ctx, queries, w.qdrant, w.embeddingProvider, embeddingModel, repoID, candidate.Context, candidate.ConceptText, factID)
		if !ok {
			// Defer: leave the fact on the candidate. ResolveAliasMatchForFact
			// returns false on 0 matches (shouldn't happen here — matches is
			// >1) or when the fact's vector is unavailable.
			deferredCount++
			continue
		}
		// Link this fact to its winner (idempotent).
		if _, err := queries.AddFactConcept(ctx, store.AddFactConceptParams{
			FactID:        factID,
			ConceptID:     winner.ID,
			PromptsetHash: psHashPtr,
		}); err != nil && !errors.Is(err, pgx.ErrNoRows) {
			log.Printf("refine_concepts: per-fact AddFactConcept (fact %s -> concept %s): %v",
				pgUUIDToString(factID), pgUUIDToString(winner.ID), err)
			deferredCount++
			continue
		}
		// Remove the fact from the candidate so deferred-vs-routed is
		// tracked in the junction, not just in-memory.
		if err := queries.DeleteFactCandidate(ctx, store.DeleteFactCandidateParams{
			FactID:      factID,
			CandidateID: candidate.ID,
		}); err != nil {
			log.Printf("refine_concepts: deleting fact_candidate for fact %s: %v", pgUUIDToString(factID), err)
		}
		winners[winner.ID] = struct{}{}
		routedCount++
	}

	// Merge aliases onto every winning concept + reset its embedding.
	for _, m := range matches {
		if _, won := winners[m.ID]; !won {
			continue
		}
		w.mergeAliasesOntoConcept(ctx, queries, m, candidate, aiAliases, result)
		if err := queries.ResetConceptEmbedding(ctx, m.ID); err != nil {
			log.Printf("refine_concepts: resetting embedding for winner %s: %v", pgUUIDToString(m.ID), err)
		}
	}

	atomic.AddInt64(&result.ConceptsMerged, int64(len(winners)))

	// Candidate fate: if all facts routed, delete the candidate. If any
	// fact deferred, keep the candidate unresolved so the next pass
	// retries the deferred facts once they're embedded.
	if deferredCount == 0 {
		// All facts routed. Delete the candidate (cascade clears any
		// leftover fact_candidates). No cache entry — the alias was
		// ambiguous, so caching a single resolved_concept_id would
		// mislead future extractions.
		if err := queries.DeleteCandidate(ctx, candidate.ID); err != nil {
			return pgtype.UUID{}, true, fmt.Errorf("deleting resolved candidate %s: %w", pgUUIDToString(candidate.ID), err)
		}
		atomic.AddInt64(&result.CandidatesResolved, 1)
		// If exactly one concept won all facts, return it so the caller
		// can chain summarize for it. When facts split, there's no
		// single concept to chain; summarize is fanned out for all
		// resolved concepts by the caller's existing summarize enqueue
		// (which lists touched concepts). Return the sole winner if
		// any; otherwise invalid.
		if len(winners) == 1 {
			for id := range winners {
				return id, true, nil
			}
		}
		return pgtype.UUID{}, true, nil
	}

	// Some facts deferred. Keep the candidate unresolved.
	log.Printf("refine_concepts: candidate %s routed %d facts, deferred %d (kept unresolved)",
		pgUUIDToString(candidate.ID), routedCount, deferredCount)
	// Still count as partially resolved for metrics, but do not set
	// resolved_concept_id (no single target).
	if routedCount > 0 {
		atomic.AddInt64(&result.CandidatesResolved, 1)
	}
	return pgtype.UUID{}, true, nil
}

// mergeAliasesOntoConcept copies seed + AI aliases + the candidate's
// concept_text alias onto a concept. Shared between the single-target
// mergeCandidateIntoConcept and the per-fact multi-match fork.
func (w *RefineConceptsWorker) mergeAliasesOntoConcept(
	ctx context.Context,
	queries *store.Queries,
	target store.OktRepositoryConcept,
	candidate store.OktRepositoryConceptCandidate,
	aiAliases []string,
	result *RefineConceptsResult,
) {
	for _, alias := range candidate.SeedAliases {
		if alias == "" {
			continue
		}
		if _, err := queries.AddConceptAlias(ctx, store.AddConceptAliasParams{
			ConceptID: target.ID,
			AliasText: alias,
		}); err != nil && !errors.Is(err, pgx.ErrNoRows) {
			log.Printf("refine_concepts: copying seed alias %q onto target %s: %v", alias, pgUUIDToString(target.ID), err)
		}
		atomic.AddInt64(&result.AliasesAdded, 1)
	}
	for _, alias := range aiAliases {
		if alias == "" {
			continue
		}
		if _, err := queries.AddConceptAlias(ctx, store.AddConceptAliasParams{
			ConceptID: target.ID,
			AliasText: alias,
		}); err != nil && !errors.Is(err, pgx.ErrNoRows) {
			log.Printf("refine_concepts: copying AI alias %q onto target %s: %v", alias, pgUUIDToString(target.ID), err)
		}
		atomic.AddInt64(&result.AliasesAdded, 1)
	}
	if _, err := queries.AddConceptAlias(ctx, store.AddConceptAliasParams{
		ConceptID: target.ID,
		AliasText: candidate.ConceptText,
	}); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		log.Printf("refine_concepts: copying concept_text %q onto target %s: %v", candidate.ConceptText, pgUUIDToString(target.ID), err)
	}
}

// enqueueSummarizeConcepts enqueues one summarize_concepts job per
// resolved concept_id. Failures are logged and swallowed so a
// summarize enqueue problem never fails the refine_concepts job.
func (w *RefineConceptsWorker) enqueueSummarizeConcepts(ctx context.Context, repoIDStr, sourceIDStr string, conceptIDs []pgtype.UUID) {
	client := river.ClientFromContext[pgx.Tx](ctx)
	if client == nil {
		log.Printf("refine_concepts: no river client on context; summarize_concepts not enqueued for repo %s", repoIDStr)
		return
	}

	maxPerRun := 40
	for start := 0; start < len(conceptIDs); start += maxPerRun {
		end := start + maxPerRun
		if end > len(conceptIDs) {
			end = len(conceptIDs)
		}
		chunkIDs := make([]string, 0, end-start)
		for _, id := range conceptIDs[start:end] {
			chunkIDs = append(chunkIDs, pgUUIDToString(id))
		}
		chunkCtx, chunkCancel := context.WithTimeout(context.Background(), 15*time.Second)
		if _, err := client.Insert(chunkCtx, SummarizeConceptsArgs{
			RepositoryID: repoIDStr,
			SourceID:     sourceIDStr,
			ConceptIDs:   chunkIDs,
		}, &river.InsertOpts{
			Queue: QueueSummarizeConcepts,
			Metadata: MarshalMetadata(JobMetadata{
				RepositoryID: repoIDStr,
				SourceID:     sourceIDStr,
			}),
		}); err != nil {
			log.Printf("refine_concepts: enqueueing summarize_concepts chunk for repo %s: %v", repoIDStr, err)
		}
		chunkCancel()
	}
}

var _ river.Worker[RefineConceptsArgs] = (*RefineConceptsWorker)(nil)