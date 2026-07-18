package tasks

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/openktree/open-knowledge-tree/backend/internal/concepts"
	"github.com/openktree/open-knowledge-tree/backend/internal/config"
	"github.com/openktree/open-knowledge-tree/backend/internal/dbpool"
	"github.com/openktree/open-knowledge-tree/backend/internal/promptset"
	"github.com/openktree/open-knowledge-tree/backend/internal/providers/ai"
	"github.com/openktree/open-knowledge-tree/backend/internal/providers/decomposition"
	"github.com/openktree/open-knowledge-tree/backend/internal/qdrantstore"
	"github.com/openktree/open-knowledge-tree/backend/internal/store"
	"github.com/riverqueue/river"
)

const QueueExtractConcepts = "extract_concepts"

// conceptPlan is one concept extracted from a fact, ready for
// persistence in phase 2. The miss path creates a concept_candidate
// (with the extracted text as concept_text + seed aliases); the
// match path links the fact to the existing concept and merges seed
// aliases. No inline LLM alias generation — that's deferred to the
// refine_concepts task.
type conceptPlan struct {
	c decomposition.ExtractedConcept
}

// factExtract is the phase-1 result for one fact: the extracted concept
// plans (or nil on error). Phase 2 consumes these serially to do the
// DB writes (recordSkip / linkFactToConcept).
type factExtract struct {
	plans []conceptPlan
}

// ExtractConceptsArgs triggers a per-repository concept-extraction
// pass. The working set is the repo's stable facts (post-dedup) that
// have no row yet in fact_concepts and no row in fact_concept_skips.
// Facts are sent to the LLM in batches of FactBatchSize (default 10)
// per call, one parallel wave (Concurrency × FactBatchSize facts) per
// round, so a wave of 40 facts at defaults produces 4 LLM calls
// instead of 40. Duplicate work across concurrent enqueues is
// prevented by the fact_concepts unique index and the NOT EXISTS
// filter in ListStableFactsForConceptExtraction, so no advisory lock
// is taken (a previous version took pg_advisory_xact_lock(hashtext($repo))
// for the entire pass, which blocked deduplicate_facts for the same
// repo for the full duration of the LLM loop — see the comment in
// Work).
type ExtractConceptsArgs struct {
	RepositoryID string `json:"repository_id"`
	// SourceID, when non-empty, narrows the candidate set to stable
	// facts linked to this source (via fact_sources). Empty means a
	// repo-wide pass (manual re-enqueue / periodic catch-up). The
	// NOT EXISTS (fact_concepts) + unique index still guard shared
	// facts when two sources' passes race.
	SourceID string `json:"source_id,omitempty"`
}

func (ExtractConceptsArgs) Kind() string { return "extract_concepts" }

func (ExtractConceptsArgs) InsertOpts() river.InsertOpts { return river.InsertOpts{} }

// ExtractConceptsResult is recorded on the job row so the River UI
// shows what the pass did. FactsProcessed is the count of facts the
// extractor was invoked on; ConceptsNew is the count of newly
// created concepts; ConceptsMatched is the count of (concept,
// context) pairs that text-search-matched an existing concept;
// AliasesMerged is the count of seed aliases merged onto matched
// concepts (free recall boost, no LLM); Errors carries the count of
// per-fact failures so the UI can surface partial failures without
// digging through logs.
type ExtractConceptsResult struct {
	RepositoryID    string `json:"repository_id"`
	FactsProcessed  int    `json:"facts_processed"`
	ConceptsNew     int    `json:"concepts_new"`
	ConceptsMatched int    `json:"concepts_matched"`
	AliasesMerged   int    `json:"aliases_merged"`
	Errors          int    `json:"errors"`
}

type ExtractConceptsWorker struct {
	river.WorkerDefaults[ExtractConceptsArgs]

	conceptExtractor     decomposition.ConceptExtractionProvider
	conceptCfg           config.DecompositionConceptConfig
	summarizationEnabled bool
	refinementEnabled    bool
	registry             *dbpool.Registry
	systemQueries        *store.Queries
	modelResolver        *ModelResolver
	promptsetResolver    *PromptsetResolver
	// embeddingProvider + qdrant + embeddingCfg power the per-fact
	// embedding tie-break for the refinement-DISABLED direct-routing
	// path (concepts.ResolveAliasMatchForFact). When refinement is
	// enabled, extract never routes (refine does), so these are
	// unused on that path. Set via SetEmbeddingDeps; nil is safe
	// (multi-match defers the fact instead of mis-routing).
	embeddingProvider ai.EmbeddingProvider
	embeddingCfg      config.EmbeddingConfig
	qdrant            *qdrantstore.Store
}

func NewExtractConceptsWorker(
	conceptExtractor decomposition.ConceptExtractionProvider,
	conceptCfg config.DecompositionConceptConfig,
	registry *dbpool.Registry,
	systemQueries *store.Queries,
	modelResolver *ModelResolver,
	promptsetResolver *PromptsetResolver,
) *ExtractConceptsWorker {
	return &ExtractConceptsWorker{
		conceptExtractor:  conceptExtractor,
		conceptCfg:        conceptCfg,
		registry:          registry,
		systemQueries:     systemQueries,
		modelResolver:     modelResolver,
		promptsetResolver: promptsetResolver,
	}
}

// SetEmbeddingDeps wires the embedding provider + Qdrant store +
// embedding config used by the refinement-disabled direct-routing
// path's alias tie-break. Split out as a setter so existing test call
// sites that construct the worker directly keep working without
// changing their constructor call sites; tests that need the
// embedding tie-break call this after construction.
func (w *ExtractConceptsWorker) SetEmbeddingDeps(provider ai.EmbeddingProvider, cfg config.EmbeddingConfig, qdrant *qdrantstore.Store) {
	w.embeddingProvider = provider
	w.embeddingCfg = cfg
	w.qdrant = qdrant
}

// SetSummarizationEnabled lets the wiring layer (taskmanager.New)
// tell the worker whether summarization is configured, so the
// extract_concepts pass can fan out SummarizeConcepts jobs in
// parallel with embed_concepts only when a summarizer is wired.
// Split out as a setter rather than a constructor arg so existing
// callers (tests) keep working without changing their constructor
// call sites.
func (w *ExtractConceptsWorker) SetSummarizationEnabled(enabled bool) {
	w.summarizationEnabled = enabled
}

// SetRefinementEnabled lets the wiring layer tell the worker whether
// refinement is configured, so extract_concepts can fan out
// RefineConcepts jobs only when a refiner is wired.
func (w *ExtractConceptsWorker) SetRefinementEnabled(enabled bool) {
	w.refinementEnabled = enabled
}

// Work runs a concept-extraction pass over a repo's stable facts.
// One round fetches one parallel wave (Concurrency × FactBatchSize
// facts), splits it into Concurrency chunks of FactBatchSize facts
// each, and sends each chunk to the concept-extraction provider in
// one LLM call. The provider returns concepts tagged with a
// fact_index so the worker can map them back to their originating
// fact. For each (concept, context, seed_aliases) triple returned,
// the worker text-search-matches against existing concepts (scoped
// by repository_id + context). A match links the fact to the existing
// concept and merges seed_aliases via ON CONFLICT DO NOTHING (free
// recall boost, no LLM). A miss invokes the alias provider to
// generate the canonical name + expanded aliases, creates the
// concept + aliases, and links the fact. The pass chains to
// embed_concepts on completion.
//
// No advisory lock is taken. The fact_concepts unique index on
// (fact_id, concept_id) and the NOT EXISTS subquery in
// ListStableFactsForConceptExtraction already make duplicate work
// across concurrent enqueues a no-op: a racing pass that picks the
// same fact will see zero candidate rows on its next batch (the
// first pass's fact_concepts insert excludes it). The previous
// implementation held pg_advisory_xact_lock(hashtext($repo)) for
// the entire pass, which blocked deduplicate_facts for the same
// repo for the full duration of the LLM loop — including
// OpenRouter timeouts — and caused the chain to deadlock for
// hours when the upstream provider hung.
//
// Per-fact LLM failures (timeouts, 5xx, parse errors) are recorded
// as a permanent skip row in fact_concept_skips so the next pass
// does not retry the same failing fact forever (there is no
// periodic re-driver; an operator must delete the skip row to
// retry). The skip is written in its own short transaction so a
// failing LLM call cannot poison the batch's main tx.
//
// When the concept extractor, alias provider, or ontology source
// is nil, or concept_extraction.enabled is false, the worker logs
// and returns nil (a missing provider is a deployment choice, not
// a retryable error — River would otherwise spin forever).
func (w *ExtractConceptsWorker) Work(ctx context.Context, job *river.Job[ExtractConceptsArgs]) error {
	args := job.Args
	if args.RepositoryID == "" {
		return fmt.Errorf("extract_concepts: repository_id is required")
	}

	if !w.conceptCfg.Enabled {
		log.Printf("extract_concepts: concept_extraction not enabled, skipping repo %s", args.RepositoryID)
		return river.RecordOutput(ctx, &ExtractConceptsResult{RepositoryID: args.RepositoryID})
	}

	repoID := pgtype.UUID{}
	if err := repoID.Scan(args.RepositoryID); err != nil {
		return fmt.Errorf("extract_concepts: invalid repository_id: %w", err)
	}

	// SourceID is optional: when set, narrow the candidate set to
	// facts linked to that source (avoids re-scanning the whole
	// repo every time any source completes processing). When empty,
	// run the repo-wide pass (manual re-enqueue / catch-up).
	var srcID pgtype.UUID
	sourceScoped := false
	if args.SourceID != "" {
		if err := srcID.Scan(args.SourceID); err != nil {
			return fmt.Errorf("extract_concepts: invalid source_id: %w", err)
		}
		sourceScoped = true
	}

	dbName, err := w.systemQueries.GetRepositoryDatabaseName(ctx, repoID)
	if err != nil {
		return fmt.Errorf("extract_concepts: resolving repository database: %w", err)
	}
	pool := w.registry.Get(dbName)

	// Resolve the repo's effective promptset once at Work() start.
	// The hash tags every concept + fact_concept link this job
	// persists so downstream queries (synthesis, registry pull) can
	// filter to a single promptset and decompositions from different
	// promptsets do not mix.
	var ps promptset.Promptset
	var psHash string
	if w.promptsetResolver != nil {
		ps = w.promptsetResolver.Effective(ctx, repoID)
		psHash = ps.Hash
		w.promptsetResolver.LogEffective(ctx, repoID, "extract_concepts")
	} else {
		ps = promptset.Default
		psHash = promptset.DefaultHash
	}
	psHashPtr := &psHash

	// Resolve per-repo model overrides for concept extraction.
	conceptExtractor := w.conceptExtractor
	if w.modelResolver != nil {
		if r := w.modelResolver.Resolve(ctx, repoID, TaskKindConceptExtraction); r.Provider != nil {
			conceptExtractor = decomposition.NewAIConceptExtractionProvider(r.Provider, r.ModelID).WithPromptset(ps)
		}
	}
	if ce, ok := conceptExtractor.(*decomposition.AIConceptExtractionProvider); ok {
		conceptExtractor = ce.WithPromptset(ps)
	}

	if conceptExtractor == nil {
		log.Printf("extract_concepts: concept extraction provider not configured, skipping repo %s", args.RepositoryID)
		return river.RecordOutput(ctx, &ExtractConceptsResult{RepositoryID: args.RepositoryID})
	}

	// Load the per-repo allowed context list from
	// repository_contexts (the source of truth). Settings are the
	// single source of truth: an empty list means the repo has no
	// allowed contexts configured, so extraction hard-fails (the
	// admin must configure contexts via the repository-settings UI).
	// The backfill migration seeds every legacy repo with the full
	// full context vocabulary, so empty only happens when an admin deliberately
	// clears the list. Each entry carries its description so the
	// prompt can use it as a hint (custom contexts are annotated;
	// standard ones default to empty).
	repoContexts, err := w.systemQueries.ListRepositoryContexts(ctx, repoID)
	if err != nil {
		return fmt.Errorf("extract_concepts: loading repository contexts: %w", err)
	}
	if len(repoContexts) == 0 {
		return fmt.Errorf("extract_concepts: repository %s has no allowed contexts configured; configure contexts in repository settings", args.RepositoryID)
	}
	contextEntries := make([]decomposition.ContextEntry, 0, len(repoContexts))
	for _, c := range repoContexts {
		contextEntries = append(contextEntries, decomposition.ContextEntry{Label: c.Context, Description: c.Description})
	}

	// One round fetches one parallel wave: Concurrency × FactBatchSize
	// facts. That wave splits into exactly Concurrency chunks of
	// FactBatchSize facts each, so every fetched fact is in a chunk
	// that runs immediately — no straggler waits for a semaphore slot.
	// Each call's response objects carry a fact_index (0-based within
	// the chunk) so concepts can be routed back to their fact without
	// relying on output order.
	factBatchSize := w.conceptCfg.FactBatchSizeOr(10)
	conceptConcurrency := w.conceptCfg.ConcurrencyOr(4)
	fetchSize := int32(factBatchSize * conceptConcurrency)
	if fetchSize <= 0 {
		fetchSize = int32(10 * 4)
	}

	taskID := fmt.Sprintf("%d", job.ID)
	result := ExtractConceptsResult{RepositoryID: args.RepositoryID}

	// Loop in batches until no unlinked stable facts remain. The
	// candidate fetch is a read-only query (no tx needed); each
	// fact's LLM call + inserts run in their own short tx so a
	// single slow/failing LLM call cannot hold a transaction open
	// while waiting on the upstream provider. The skip-on-error
	// path uses its own tx so a failing fact is recorded even if
	// the write tx rolls back.
	for {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("extract_concepts: ctx cancelled: %w", err)
		}

		// Read-only candidate fetch. No tx: each query is
		// autonomous and the NOT EXISTS filters make the
		// candidate set stable across concurrent passes.
		var facts []store.ListStableFactsForConceptExtractionRow
		if sourceScoped {
			srcFacts, err := store.New(pool.Pool).ListStableFactsForConceptExtractionBySource(ctx, store.ListStableFactsForConceptExtractionBySourceParams{
				RepositoryID: repoID,
				SourceID:     srcID,
				BatchSize:    fetchSize,
			})
			if err != nil {
				return fmt.Errorf("extract_concepts: listing stable facts by source: %w", err)
			}
			// The source-scoped row type matches the repo-wide one
			// (id, text, created_at); assign into the shared slice
			// so the per-fact loop below is unchanged.
			facts = make([]store.ListStableFactsForConceptExtractionRow, len(srcFacts))
			for i, f := range srcFacts {
				facts[i] = store.ListStableFactsForConceptExtractionRow{ID: f.ID, Text: f.Text, CreatedAt: f.CreatedAt}
			}
		} else {
			facts, err = store.New(pool.Pool).ListStableFactsForConceptExtraction(ctx, store.ListStableFactsForConceptExtractionParams{
				RepositoryID: repoID,
				BatchSize:    fetchSize,
			})
			if err != nil {
				return fmt.Errorf("extract_concepts: listing stable facts: %w", err)
			}
		}
		if len(facts) == 0 {
			break
		}

		// ---- Phase 1: parallel AI extraction (no DB writes) ----
		// Split the wave into chunks of factBatchSize facts. Each
		// chunk is one LLM call that returns concepts tagged with a
		// fact_index (0-based within the chunk). The chunks fan out at
		// conceptConcurrency; each worker uses a fresh background
		// context (120s) so a slow LLM response cannot cancel the
		// job's ctx and poison the subsequent write tx. The provider's
		// internal retryWithBackoff handles 429/5xx/net retries per
		// call.
		//
		// factResults/factErrs are indexed by the fact's position in
		// `facts` (the full wave), so phase 2 can iterate them serially
		// in input order. A chunk-level error marks every fact in that
		// chunk as failed (and skipped), so one bad LLM call does not
		// poison the rest of the wave. With fetchSize = Concurrency ×
		// factBatchSize the wave produces exactly conceptConcurrency
		// chunks, so the semaphore is saturated with no straggler.
		factResults := make([]factExtract, len(facts))
		factErrs := make([]error, len(facts))

		type chunk struct {
			start int
			input []decomposition.FactInput
		}
		var chunks []chunk
		for start := 0; start < len(facts); start += factBatchSize {
			end := start + factBatchSize
			if end > len(facts) {
				end = len(facts)
			}
			inputs := make([]decomposition.FactInput, 0, end-start)
			for i := start; i < end; i++ {
				// Index is 0-based within the chunk so the model's
				// fact_index echo maps directly back to inputs[i].
				inputs = append(inputs, decomposition.FactInput{
					Index: i - start,
					Text:  facts[i].Text,
				})
			}
			chunks = append(chunks, chunk{start: start, input: inputs})
		}

		var wg sync.WaitGroup
		sem := make(chan struct{}, conceptConcurrency)
		for _, ch := range chunks {
			wg.Add(1)
			sem <- struct{}{}
			go func(ch chunk) {
				defer wg.Done()
				defer func() { <-sem }()

				llmCtx, llmCancel := context.WithTimeout(context.Background(), 120*time.Second)
				concepts, err := conceptExtractor.ExtractConcepts(llmCtx, pool.Pool, ch.input, contextEntries, decomposition.ConceptExtractionAttribution{
					RepositoryID: args.RepositoryID,
					TaskID:       taskID,
				})
				llmCancel()
				if err != nil {
					// Mark every fact in this chunk as failed so
					// phase 2 records a skip for each. Other chunks
					// proceed independently.
					for i := range ch.input {
						factErrs[ch.start+i] = err
					}
					return
				}

				// Group the returned concepts by fact_index (0-based
				// within the chunk) and store under the fact's
				// absolute position in `facts`.
				for i := range ch.input {
					factResults[ch.start+i] = factExtract{plans: nil}
				}
				for _, c := range concepts {
					idx := c.FactIndex
					if idx < 0 || idx >= len(ch.input) {
						// Out-of-range index: the model returned a
						// bad fact_index. Drop the concept rather
						// than panic; log so it's visible.
						log.Printf("extract_concepts: dropping concept %q with out-of-range fact_index %d (chunk start %d, chunk len %d)", c.Concept, idx, ch.start, len(ch.input))
						continue
					}
					absIdx := ch.start + idx
					factResults[absIdx].plans = append(factResults[absIdx].plans, conceptPlan{c: c})
				}
			}(ch)
		}
		wg.Wait()

		// ---- Phase 2: serial persistence (single goroutine) ----
		// Iterate the ordered results and persist exactly as the old
		// inline loop did. recordSkip + the per-fact write tx run
		// serially, so the DB sees the same connection pressure as the
		// serial baseline. Because all concept creation now happens
		// in this serial phase, fact #2's FindConceptByAlias sees
		// concepts created by fact #1 — strictly more consistent than
		// the old already-serial-but-inline pattern (no regression).
		for i, fact := range facts {
			if err := ctx.Err(); err != nil {
				return fmt.Errorf("extract_concepts: ctx cancelled: %w", err)
			}
			result.FactsProcessed++

			if err := factErrs[i]; err != nil {
				log.Printf("extract_concepts: extracting concepts for fact %s: %v", pgUUIDToString(fact.ID), err)
				result.Errors++
				if rerr := w.recordSkip(context.Background(), pool.Pool, fact.ID, err); rerr != nil {
					log.Printf("extract_concepts: recording skip for fact %s: %v", pgUUIDToString(fact.ID), rerr)
				}
				continue
			}

			plans := factResults[i].plans
			if len(plans) == 0 {
				continue
			}

			// Per-fact write tx. Short-lived: just the
			// FindConceptByAlias + CreateConcept + alias +
			// fact_concept inserts for this one fact. Uses a
			// fresh background context so the write is not
			// cancelled by a previous LLM call's timeout.
			writeCtx, writeCancel := context.WithTimeout(context.Background(), 30*time.Second)
			tx, err := pool.Pool.BeginTx(writeCtx, pgx.TxOptions{})
			if err != nil {
				writeCancel()
				log.Printf("extract_concepts: beginning write tx for fact %s: %v", pgUUIDToString(fact.ID), err)
				result.Errors++
				continue
			}
			queries := store.New(tx)
			for _, p := range plans {
				if err := w.linkFactToConcept(writeCtx, tx, queries, repoID, fact.ID, p.c, &result, psHashPtr); err != nil {
					log.Printf("extract_concepts: linking fact %s to concept %q: %v", pgUUIDToString(fact.ID), p.c.Concept, err)
					result.Errors++
					continue
				}
			}
			if err := tx.Commit(writeCtx); err != nil {
				log.Printf("extract_concepts: committing write tx for fact %s: %v", pgUUIDToString(fact.ID), err)
				result.Errors++
			}
			writeCancel()
		}
	}

	log.Printf("extract_concepts: repo %s processed %d facts, %d new concepts, %d matched, %d aliases merged, %d errors",
		args.RepositoryID, result.FactsProcessed, result.ConceptsNew, result.ConceptsMatched, result.AliasesMerged, result.Errors)

	// Chain to embed_concepts. Same fresh-background-ctx pattern as
	// embed_facts → deduplicate_facts.
	if client := river.ClientFromContext[pgx.Tx](ctx); client != nil {
		chainCtx, chainCancel := context.WithTimeout(context.Background(), 15*time.Second)
		if _, err := client.Insert(chainCtx, EmbedConceptsArgs{RepositoryID: args.RepositoryID, SourceID: args.SourceID}, &river.InsertOpts{
			Queue: QueueEmbedConcepts,
			Metadata: MarshalMetadata(JobMetadata{
				RepositoryID: args.RepositoryID,
				SourceID:     args.SourceID,
			}),
		}); err != nil {
			log.Printf("extract_concepts: enqueueing embed_concepts for repo %s: %v", args.RepositoryID, err)
		}
		chainCancel()
	} else {
		log.Printf("extract_concepts: no river client on context; embed_concepts not enqueued for repo %s", args.RepositoryID)
	}

	// Fan out RefineConcepts. Runs before summarize_concepts so the
	// summarizer sees the final canonical names. Gated on
	// refinementEnabled so a deployment that hasn't wired a refinement
	// model doesn't enqueue no-op jobs. The unresolved candidate list
	// is queried fresh here (candidates created during this pass).
	// refine_concepts will chain to summarize_concepts on completion.
	if w.refinementEnabled {
		w.enqueueRefineConcepts(ctx, pool.Pool, repoID, args.RepositoryID, args.SourceID)
	}

	// Fan out SummarizeConcepts in parallel with embed_concepts when
	// refinement is NOT enabled. When refinement IS enabled, it
	// chains to summarize_concepts on completion, so we skip the
	// parallel enqueue here to avoid double-enqueueing.
	if w.summarizationEnabled && !w.refinementEnabled {
		w.enqueueSummarizeConcepts(ctx, pool.Pool, repoID, args.RepositoryID, args.SourceID)
	}

	// Chain a concept-relations matview refresh so the relations-list
	// read endpoint reflects this batch's new fact_concepts links. The
	// refresh is deduped per-database by River unique-args, so rapid
	// batches coalesce into a single refresh; it runs CONCURRENTLY so
	// the read side never blocks. Best-effort: a failed enqueue is
	// logged and swallowed (the periodic RefreshAllConceptRelations
	// job covers it within refresh_concept_relations_interval). The
	// database_name was already resolved at the top of Work; reusing
	// it here keeps the lookup to one system-DB query per pass.
	w.enqueueRefreshConceptRelations(ctx, dbName, args.RepositoryID)

	return river.RecordOutput(ctx, &result)
}

// enqueueRefineConcepts lists the unresolved candidates created by
// this pass (scoped by source when SourceID is set, repo-wide
// otherwise), chunks them by MaxCandidatesPerRun, and enqueues one
// RefineConcepts job per chunk. Failures are logged and swallowed so
// a refinement enqueue problem never fails the extract_concepts job.
// refine_concepts chains to summarize_concepts on completion.
func (w *ExtractConceptsWorker) enqueueRefineConcepts(ctx context.Context, pool *pgxpool.Pool, repoID pgtype.UUID, repoIDStr, sourceIDStr string) {
	client := river.ClientFromContext[pgx.Tx](ctx)
	if client == nil {
		log.Printf("extract_concepts: no river client on context; refine_concepts not enqueued for repo %s", repoIDStr)
		return
	}
	queries := store.New(pool)
	listCtx, listCancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer listCancel()
	var candidateIDs []pgtype.UUID
	if sourceIDStr != "" {
		var srcID pgtype.UUID
		if err := srcID.Scan(sourceIDStr); err != nil {
			log.Printf("extract_concepts: scanning source_id for refine fan-out: %v", err)
			return
		}
		rows, err := queries.ListUnresolvedCandidatesBySource(listCtx, srcID)
		if err != nil {
			log.Printf("extract_concepts: listing unresolved candidates by source for refine fan-out: %v", err)
			return
		}
		for _, r := range rows {
			candidateIDs = append(candidateIDs, r.ID)
		}
	} else {
		rows, err := queries.ListUnresolvedCandidatesByRepo(listCtx, repoID)
		if err != nil {
			log.Printf("extract_concepts: listing unresolved candidates repo-wide for refine fan-out: %v", err)
			return
		}
		for _, r := range rows {
			candidateIDs = append(candidateIDs, r.ID)
		}
	}
	if len(candidateIDs) == 0 {
		return
	}

	maxPerRun := 40
	chunkCtx, chunkCancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer chunkCancel()
	for start := 0; start < len(candidateIDs); start += maxPerRun {
		end := start + maxPerRun
		if end > len(candidateIDs) {
			end = len(candidateIDs)
		}
		chunkIDs := make([]string, 0, end-start)
		for _, id := range candidateIDs[start:end] {
			chunkIDs = append(chunkIDs, pgUUIDToString(id))
		}
		if _, err := client.Insert(chunkCtx, RefineConceptsArgs{
			RepositoryID: repoIDStr,
			SourceID:     sourceIDStr,
			CandidateIDs: chunkIDs,
		}, &river.InsertOpts{
			Queue: QueueRefineConcepts,
			Metadata: MarshalMetadata(JobMetadata{
				RepositoryID: repoIDStr,
				SourceID:     sourceIDStr,
			}),
		}); err != nil {
			log.Printf("extract_concepts: enqueueing refine_concepts chunk for repo %s: %v", repoIDStr, err)
		}
	}
}

// enqueueSummarizeConcepts lists the concept_ids touched by this
// pass (scoped by source when SourceID is set, repo-wide otherwise),
// chunks them by MaxConceptsPerRun, and enqueues one
// SummarizeConcepts job per chunk. Failures are logged and swallowed
// so a summarization enqueue problem never fails the
// extract_concepts job (the fact pipeline still completes; the next
// extract_concepts pass or the periodic catch-up will retry
// summarization).
func (w *ExtractConceptsWorker) enqueueSummarizeConcepts(ctx context.Context, pool *pgxpool.Pool, repoID pgtype.UUID, repoIDStr, sourceIDStr string) {
	client := river.ClientFromContext[pgx.Tx](ctx)
	if client == nil {
		log.Printf("extract_concepts: no river client on context; summarize_concepts not enqueued for repo %s", repoIDStr)
		return
	}
	queries := store.New(pool)
	listCtx, listCancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer listCancel()
	var conceptIDs []pgtype.UUID
	if sourceIDStr != "" {
		var srcID pgtype.UUID
		if err := srcID.Scan(sourceIDStr); err != nil {
			log.Printf("extract_concepts: scanning source_id for summarize fan-out: %v", err)
			return
		}
		rows, err := queries.ListTouchedConceptsForSummary(listCtx, store.ListTouchedConceptsForSummaryParams{
			RepositoryID: repoID, SourceID: srcID,
		})
		if err != nil {
			log.Printf("extract_concepts: listing touched concepts by source for summarize fan-out: %v", err)
			return
		}
		for _, r := range rows {
			conceptIDs = append(conceptIDs, r.ID)
		}
	} else {
		rows, err := queries.ListTouchedConceptsForSummaryByRepo(listCtx, repoID)
		if err != nil {
			log.Printf("extract_concepts: listing touched concepts repo-wide for summarize fan-out: %v", err)
			return
		}
		for _, r := range rows {
			conceptIDs = append(conceptIDs, r.ID)
		}
	}
	if len(conceptIDs) == 0 {
		return
	}

	maxPerRun := 40
	chunkCtx, chunkCancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer chunkCancel()
	for start := 0; start < len(conceptIDs); start += maxPerRun {
		end := start + maxPerRun
		if end > len(conceptIDs) {
			end = len(conceptIDs)
		}
		chunkIDs := make([]string, 0, end-start)
		for _, id := range conceptIDs[start:end] {
			chunkIDs = append(chunkIDs, pgUUIDToString(id))
		}
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
			log.Printf("extract_concepts: enqueueing summarize_concepts chunk for repo %s: %v", repoIDStr, err)
		}
	}
}

// enqueueRefreshConceptRelations enqueues a refresh of the
// okt_repository.concept_relations matview for the database this
// repo lives in. The matview is per-database (two repos sharing a
// database share one view), so the unique key is databaseName, not
// repositoryID — River's unique-by-args dedup makes a second enqueue
// for the same database a no-op while one is queued/running. The
// enqueue is best-effort: a failure is logged and swallowed so the
// relations refresh never fails the extract_concepts job; the
// periodic RefreshAllConceptRelations worker covers it within
// refresh_concept_relations_interval. Uses a fresh background context
// (same pattern as the embed_concepts chain) so a ctx cancellation
// after the work loop doesn't drop the refresh enqueue.
func (w *ExtractConceptsWorker) enqueueRefreshConceptRelations(ctx context.Context, databaseName, repositoryIDStr string) {
	client := river.ClientFromContext[pgx.Tx](ctx)
	if client == nil {
		log.Printf("extract_concepts: no river client on context; refresh_concept_relations not enqueued for repo %s", repositoryIDStr)
		return
	}
	refreshCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	res, err := client.Insert(refreshCtx, RefreshConceptRelationsArgs{
		DatabaseName: databaseName,
		RepositoryID: repositoryIDStr,
	}, &river.InsertOpts{
		Queue: QueueRefreshConceptRelations,
		Metadata: MarshalMetadata(JobMetadata{
			RepositoryID: repositoryIDStr,
		}),
	})
	if err != nil {
		log.Printf("extract_concepts: enqueueing refresh_concept_relations for repo %s (db %s): %v", repositoryIDStr, databaseName, err)
		return
	}
	if res != nil && res.UniqueSkippedAsDuplicate {
		// A refresh for this database is already queued/running — the
		// unique opts coalesced this batch's request onto it. Expected
		// under bursty extraction; not a failure.
		log.Printf("extract_concepts: refresh_concept_relations for repo %s (db %s) deduped (one already active)",
			repositoryIDStr, databaseName)
		return
	}
	log.Printf("extract_concepts: enqueued refresh_concept_relations for repo %s (db %s)", repositoryIDStr, databaseName)
}

// recordSkip writes a permanent fact_concept_skips row in its own
// short transaction. A fresh context is used because the caller's
// batch tx may already be poisoned by the failing LLM call; we
// still want the skip recorded so the next pass doesn't retry the
// same failing fact forever.
func (w *ExtractConceptsWorker) recordSkip(ctx context.Context, pool *pgxpool.Pool, factID pgtype.UUID, cause error) error {
	skipCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	tx, err := pool.BeginTx(skipCtx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("beginning skip tx: %w", err)
	}
	defer tx.Rollback(context.Background())
	queries := store.New(tx)
	if _, err := queries.RecordFactConceptSkip(skipCtx, store.RecordFactConceptSkipParams{
		FactID:    factID,
		LastError: truncateForLog(cause.Error(), 500),
	}); err != nil {
		return fmt.Errorf("recording skip: %w", err)
	}
	return tx.Commit(skipCtx)
}

// truncateForLog caps a string to n bytes so the skip row's
// last_error column doesn't grow unbounded on noisy upstream
// errors.
func truncateForLog(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// linkFactToConcept records one (concept, context) pair extracted
// from a fact. The behavior splits on whether refinement is enabled:
//
//   - Refinement ENABLED (the normal case): extract does NOT route.
//     It creates (or reuses) a concept_candidate and links the fact
//     via fact_candidates. refine_concepts later routes each fact
//     individually to its cosine-winning concept. This keeps exactly
//     one routing call site (refine) and lets a stuck canonicalization
//     LLM stall only refine, never the fact pipeline.
//   - Refinement DISABLED: refine is a no-op, so extract must route
//     directly. It uses the shared concepts.ResolveAliasMatchForFact
//     helper (alias match with per-fact embedding tie-break) and
//     creates the concept inline on a miss. This is the legacy path,
//     kept so refinement-disabled deployments still link facts to
//     concepts.
//
// The resolved-candidate cache applies in both branches: when a
// candidate for this (concept_text, context) was already resolved by
// a prior refine pass, the fact links directly to the resolved
// concept — a memo lookup, not a routing decision (the routing
// decision was made by the refine pass that set resolved_concept_id).
func (w *ExtractConceptsWorker) linkFactToConcept(
	ctx context.Context,
	db store.DBTX,
	queries *store.Queries,
	repoID pgtype.UUID,
	factID pgtype.UUID,
	c decomposition.ExtractedConcept,
	result *ExtractConceptsResult,
	psHashPtr *string,
) error {
	if c.Concept == "" || c.Context == "" {
		return nil
	}

	// Resolved-candidate cache (applies in both branches): a
	// candidate for this (concept_text, context) was already
	// resolved, so the routing decision is memoized. Link the fact
	// directly to the resolved concept and merge seed aliases.
	resolved, err := queries.FindResolvedCandidate(ctx, store.FindResolvedCandidateParams{
		RepositoryID: repoID,
		ConceptText:  c.Concept,
		Context:      c.Context,
	})
	if err == nil && resolved.ResolvedConceptID.Valid {
		result.ConceptsMatched++
		if _, err := queries.AddFactConcept(ctx, store.AddFactConceptParams{
			FactID:        factID,
			ConceptID:     resolved.ResolvedConceptID,
			PromptsetHash: psHashPtr,
		}); err != nil {
			if !errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("add fact_concept (cache hit): %w", err)
			}
		}
		for _, alias := range c.SeedAliases {
			if alias == "" {
				continue
			}
			if _, err := queries.AddConceptAlias(ctx, store.AddConceptAliasParams{
				ConceptID: resolved.ResolvedConceptID,
				AliasText: alias,
			}); err != nil && !errors.Is(err, pgx.ErrNoRows) {
				log.Printf("extract_concepts: merging seed alias %q onto cached concept %s: %v", alias, pgUUIDToString(resolved.ResolvedConceptID), err)
			}
			result.AliasesMerged++
		}
		return nil
	}

	// Refinement ENABLED: emit a candidate (no routing).
	if w.refinementEnabled {
		return w.emitCandidate(ctx, queries, repoID, factID, c, result, psHashPtr)
	}

	// Refinement DISABLED: route directly via the shared helper.
	return w.routeDirect(ctx, queries, repoID, factID, c, result, psHashPtr)
}

// emitCandidate creates (or reuses an unresolved) concept_candidate
// and links the fact via fact_candidates. refine_concepts will route
// this fact — and every other fact on the same candidate —
// individually to its cosine-winning concept.
func (w *ExtractConceptsWorker) emitCandidate(
	ctx context.Context,
	queries *store.Queries,
	repoID pgtype.UUID,
	factID pgtype.UUID,
	c decomposition.ExtractedConcept,
	result *ExtractConceptsResult,
	psHashPtr *string,
) error {
	candidate, err := queries.CreateCandidate(ctx, store.CreateCandidateParams{
		RepositoryID: repoID,
		ConceptText:  c.Concept,
		Context:      c.Context,
		SeedAliases:  c.SeedAliases,
	})
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("create candidate: %w", err)
	}
	if errors.Is(err, pgx.ErrNoRows) {
		// ON CONFLICT DO NOTHING: a candidate for this
		// (concept_text, context) already exists. Re-fetch it. If
		// it's resolved (race with a concurrent refine), the
		// resolved-cache lookup at the top of linkFactToConcept
		// missed; re-check here and treat as a cache hit.
		existing, ferr := queries.FindUnresolvedCandidate(ctx, store.FindUnresolvedCandidateParams{
			RepositoryID: repoID,
			ConceptText:  c.Concept,
			Context:      c.Context,
		})
		if ferr != nil {
			resolved, rerr := queries.FindResolvedCandidate(ctx, store.FindResolvedCandidateParams{
				RepositoryID: repoID,
				ConceptText:  c.Concept,
				Context:      c.Context,
			})
			if rerr == nil && resolved.ResolvedConceptID.Valid {
				result.ConceptsMatched++
				if _, lerr := queries.AddFactConcept(ctx, store.AddFactConceptParams{
					FactID: factID, ConceptID: resolved.ResolvedConceptID, PromptsetHash: psHashPtr,
				}); lerr != nil && !errors.Is(lerr, pgx.ErrNoRows) {
					return fmt.Errorf("add fact_concept (race cache hit): %w", lerr)
				}
				return nil
			}
			return fmt.Errorf("re-find unresolved candidate after ON CONFLICT: %w", ferr)
		}
		candidate = existing
	}
	result.ConceptsNew++

	if _, err := queries.AddFactCandidate(ctx, store.AddFactCandidateParams{
		FactID:      factID,
		CandidateID: candidate.ID,
	}); err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("add fact_candidate: %w", err)
		}
	}
	return nil
}

// routeDirect is the refinement-DISABLED direct-routing path. It
// uses the shared concepts.ResolveAliasMatchForFact helper so the
// alias tie-break (embedding distance on a shared alias) is the same
// logic refine uses. On a match it links the fact + merges seed
// aliases; on a miss it creates the concept inline + aliases + links
// the fact. This path exists so refinement-disabled deployments
// still link facts to concepts (refine is a no-op when disabled).
func (w *ExtractConceptsWorker) routeDirect(
	ctx context.Context,
	queries *store.Queries,
	repoID pgtype.UUID,
	factID pgtype.UUID,
	c decomposition.ExtractedConcept,
	result *ExtractConceptsResult,
	psHashPtr *string,
) error {
	winner, ok := concepts.ResolveAliasMatchForFact(ctx, queries, w.qdrant, w.embeddingProvider, w.embeddingCfg.Model, repoID, c.Context, c.Concept, factID)
	if ok {
		// Match: link fact to the winning concept + merge seed aliases.
		result.ConceptsMatched++
		if _, err := queries.AddFactConcept(ctx, store.AddFactConceptParams{
			FactID:        factID,
			ConceptID:     winner.ID,
			PromptsetHash: psHashPtr,
		}); err != nil {
			if !errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("add fact_concept (direct match): %w", err)
			}
		}
		for _, alias := range c.SeedAliases {
			if alias == "" {
				continue
			}
			if _, err := queries.AddConceptAlias(ctx, store.AddConceptAliasParams{
				ConceptID: winner.ID,
				AliasText: alias,
			}); err != nil {
				if !errors.Is(err, pgx.ErrNoRows) {
					log.Printf("extract_concepts: merging seed alias %q onto concept %s: %v", alias, pgUUIDToString(winner.ID), err)
				}
				continue
			}
			result.AliasesMerged++
		}
		return nil
	}
	// Miss: ok is false on 0 matches, OR on >1 matches when the
	// fact's vector is unavailable (the helper defers). In both cases
	// the refinement-disabled path falls back to creating the concept
	// inline so the fact is still linked (refine is not coming).
	created, err := queries.CreateConcept(ctx, store.CreateConceptParams{
		RepositoryID:  repoID,
		CanonicalName: c.Concept,
		Context:       c.Context,
		PromptsetHash: psHashPtr,
	})
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("create concept (direct miss): %w", err)
	}
	if errors.Is(err, pgx.ErrNoRows) {
		// ON CONFLICT: a racing pass created the same concept. Re-find.
		existing, ferr := queries.FindConceptByCanonical(ctx, store.FindConceptByCanonicalParams{
			RepositoryID: repoID,
			Context:      c.Context,
			Name:         c.Concept,
		})
		if ferr != nil {
			return fmt.Errorf("re-find concept after ON CONFLICT: %w", ferr)
		}
		created = existing
	}
	result.ConceptsNew++

	// Add the concept_text as an alias + seed aliases.
	if _, err := queries.AddConceptAlias(ctx, store.AddConceptAliasParams{
		ConceptID: created.ID,
		AliasText: c.Concept,
	}); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		log.Printf("extract_concepts: adding concept_text alias %q: %v", c.Concept, err)
	}
	for _, alias := range c.SeedAliases {
		if alias == "" {
			continue
		}
		if _, err := queries.AddConceptAlias(ctx, store.AddConceptAliasParams{
			ConceptID: created.ID,
			AliasText: alias,
		}); err != nil && !errors.Is(err, pgx.ErrNoRows) {
			log.Printf("extract_concepts: adding seed alias %q: %v", alias, err)
		}
		result.AliasesMerged++
	}

	if _, err := queries.AddFactConcept(ctx, store.AddFactConceptParams{
		FactID:        factID,
		ConceptID:     created.ID,
		PromptsetHash: psHashPtr,
	}); err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("add fact_concept (direct miss): %w", err)
		}
	}
	return nil
}
