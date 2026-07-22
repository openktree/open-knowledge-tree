package tasks

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/url"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/openktree/open-knowledge-tree/backend/internal/config"
	"github.com/openktree/open-knowledge-tree/backend/internal/dbpool"
	"github.com/openktree/open-knowledge-tree/backend/internal/promptset"
	"github.com/openktree/open-knowledge-tree/backend/internal/providers/ai"
	"github.com/openktree/open-knowledge-tree/backend/internal/providers/summarization"
	"github.com/openktree/open-knowledge-tree/backend/internal/store"
	"github.com/riverqueue/river"
)

const QueueSummarizeConcepts = "summarize_concepts"

// SummarizeConceptsArgs triggers a per-repository concept-
// summarization pass. extract_concepts fans out one
// SummarizeConcepts job per MaxConceptsPerRun chunk of touched
// concept_ids; each chunk runs in parallel with embed_concepts and
// with the other chunks (the queue's worker count caps
// concurrency). ConceptIDs is the explicit chunk the worker
// processes — the worker does NOT list concepts itself, it trusts
// the caller's chunk so two chunks never overlap.
type SummarizeConceptsArgs struct {
	RepositoryID string   `json:"repository_id"`
	// SourceID, when non-empty, scopes the touched-concept list to
	// concepts linked (via fact_concepts → fact_sources) to facts
	// from this source. Empty means a repo-wide pass (manual
	// re-enqueue / periodic catch-up). The SourceID is informational
	// only when ConceptIDs is non-empty (the worker iterates the
	// explicit chunk, not the source's touched list); it's carried so
	// ai_usage attribution and the metadata filter still work.
	SourceID   string   `json:"source_id,omitempty"`
	ConceptIDs []string `json:"concept_ids"`
}

func (SummarizeConceptsArgs) Kind() string { return "summarize_concepts" }

func (SummarizeConceptsArgs) InsertOpts() river.InsertOpts { return river.InsertOpts{} }

// SummarizeConceptsResult is recorded on the job row so the River
// UI shows what the pass did. PairsProcessed is the count of
// concept|context pairs the worker actually summarized; PairsSkipped
// counts concepts that had no new facts (uncovered == 0) or were
// locked by another worker; Errors carries per-concept failures so
// the UI can surface partial failures without digging through logs.
type SummarizeConceptsResult struct {
	RepositoryID    string `json:"repository_id"`
	PairsProcessed  int    `json:"pairs_processed"`
	SummariesCreated int   `json:"summaries_created"`
	SummariesUpdated int   `json:"summaries_updated"`
	PairsSkippedNoDelta int `json:"pairs_skipped_no_delta"`
	PairsSkippedLocked  int `json:"pairs_skipped_locked"`
	Errors          int    `json:"errors"`
}

type SummarizeConceptsWorker struct {
	river.WorkerDefaults[SummarizeConceptsArgs]

	summarizer         summarization.SummarizationProvider
	cfg                config.SummarizationConfig
	registry           *dbpool.Registry
	systemQueries      *store.Queries
	modelResolver      *ModelResolver
	promptsetResolver  *PromptsetResolver
	synthesizerEnabled bool
	// llmTimeout is the per-call wall-clock timeout for a summary
	// slice LLM call. Default 20m; set via SetLLMTimeout from
	// cfg.Providers.Summarization.LLMTimeout. See
	// ExtractConceptsWorker.llmTimeout for the rationale.
	llmTimeout time.Duration
}

func NewSummarizeConceptsWorker(
	summarizer summarization.SummarizationProvider,
	cfg config.SummarizationConfig,
	registry *dbpool.Registry,
	systemQueries *store.Queries,
	synthesizerEnabled bool,
	modelResolver *ModelResolver,
	promptsetResolver *PromptsetResolver,
) *SummarizeConceptsWorker {
	return &SummarizeConceptsWorker{
		summarizer:         summarizer,
		cfg:                cfg,
		registry:           registry,
		systemQueries:      systemQueries,
		synthesizerEnabled: synthesizerEnabled,
		modelResolver:      modelResolver,
		promptsetResolver:  promptsetResolver,
		llmTimeout:         20 * time.Minute, // default; overridden via SetLLMTimeout
	}
}

// SetLLMTimeout sets the per-call wall-clock timeout for a summary
// slice LLM call. Default 20m when unset.
func (w *SummarizeConceptsWorker) SetLLMTimeout(d time.Duration) {
	if d > 0 {
		w.llmTimeout = d
	}
}

// Work processes the chunk of concept_ids in args.ConceptIDs. For
// each concept:
//  1. Acquire the per-concept summarizing_at lock (skip if held by
//     a fresh worker — staleness reclaims after a crash).
//  2. Load uncovered facts (not in any existing summary's
//     covered_fact_ids). Skip if none.
//  3. If a complete slice already exists and fewer than BatchSize
//     new facts arrived, skip (batch-only mode — no LLM call). This
//     caps cost: the first slice absorbs the trickle-in regeneration
//     while a concept is small; after it freezes, the worker waits
//     for a full batch before spending another call.
//  4. Reconstruct the open slice's covered set + the new facts.
//  5. Produce complete BatchSize slices (frozen, is_complete=TRUE).
//     While no complete slice exists yet, also emit one open
//     remainder (is_complete=FALSE) as the incremental accumulator.
//     Once a complete slice exists, never emit an open remainder.
//  6. One LLM call per slice, outside any transaction (fresh 120s
//     background context, mirroring extract_concepts).
//  7. Short per-slice write tx (CreateSummary or UpdateSummary).
//  8. Release the lock.
//
// The worker is a no-op when summarizer == nil or cfg.Enabled is
// false (deployment choice, not retryable). It is terminal — no
// follow-up chain (it's a parallel sibling of embed_concepts;
// cleanup_facts already chains after embed_concepts).
func (w *SummarizeConceptsWorker) Work(ctx context.Context, job *river.Job[SummarizeConceptsArgs]) error {
	args := job.Args
	if args.RepositoryID == "" {
		return fmt.Errorf("summarize_concepts: repository_id is required")
	}

	if w.summarizer == nil {
		log.Printf("summarize_concepts: summarization provider not configured, skipping repo %s", args.RepositoryID)
		return river.RecordOutput(ctx, &SummarizeConceptsResult{RepositoryID: args.RepositoryID})
	}
	if !w.cfg.Enabled {
		log.Printf("summarize_concepts: summarization not enabled, skipping repo %s", args.RepositoryID)
		return river.RecordOutput(ctx, &SummarizeConceptsResult{RepositoryID: args.RepositoryID})
	}

	repoID := pgtype.UUID{}
	if err := repoID.Scan(args.RepositoryID); err != nil {
		return fmt.Errorf("summarize_concepts: invalid repository_id: %w", err)
	}

	dbName, err := w.systemQueries.GetRepositoryDatabaseName(ctx, repoID)
	if err != nil {
		return fmt.Errorf("summarize_concepts: resolving repository database: %w", err)
	}
	pool := w.registry.Get(dbName)

	// Resolve the repo's effective promptset once at Work() start.
	// The resolved Promptset is threaded into the summarizer via
	// WithPromptset so the repo's philosophy runs, not the built-in
	// default. Summarization reads facts (already tagged with
	// promptset_hash by source_decomposition / extract_concepts);
	// a future step can filter the loaded facts by the active hash
	// to enforce isolation at the summary level too.
	var ps promptset.Promptset
	if w.promptsetResolver != nil {
		ps = w.promptsetResolver.Effective(ctx, repoID)
		w.promptsetResolver.LogEffective(ctx, repoID, "summarize_concepts")
	} else {
		ps = promptset.Default
	}
	summarizer := w.summarizer
	if s, ok := summarizer.(*summarization.AISummarizationProvider); ok {
		summarizer = s.WithPromptset(ps)
	}

	// Resolve per-repo model override for summarization. The
	// summarization wrapper already honors a non-empty req.Model
	// (ai_summarization.go: model := req.Model; if "" { model = p.model }),
	// so we just pass the override model id through and let the
	// wrapper prefer it over the baked-in default.
	var summarizationModelOverride string
	if w.modelResolver != nil {
		if r := w.modelResolver.Resolve(ctx, repoID, TaskKindSummarization); r.Provider != nil {
			summarizationModelOverride = r.ModelID
			summarizer = summarization.NewAISummarizationProvider(r.Provider, r.ModelID).WithPromptset(ps)
		}
	}

	batchSize := w.cfg.BatchSizeOr(20)
	staleness := w.cfg.LockStalenessOr(2 * time.Hour)
	maxTokens := w.cfg.MaxTokensOr(600)
	taskID := fmt.Sprintf("%d", job.ID)
	result := SummarizeConceptsResult{RepositoryID: args.RepositoryID}

	// If the caller passed no ConceptIDs (e.g. a manual re-enqueue
	// without a chunk), fall back to the touched list so a bare
	// enqueue still does useful work. This path is NOT used by the
	// extract_concepts fan-out (which always passes a chunk).
	conceptIDs := args.ConceptIDs
	if len(conceptIDs) == 0 {
		listed, err := w.listTouchedConceptIDs(ctx, pool.Pool, repoID, args.SourceID)
		if err != nil {
			return fmt.Errorf("summarize_concepts: listing touched concepts: %w", err)
		}
		conceptIDs = listed
	}

	for _, cidStr := range conceptIDs {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("summarize_concepts: ctx cancelled: %w", err)
		}
		var conceptID pgtype.UUID
		if err := conceptID.Scan(cidStr); err != nil {
			log.Printf("summarize_concepts: invalid concept_id %q: %v", cidStr, err)
			result.Errors++
			continue
		}
		w.summarizeOneConcept(ctx, pool.Pool, conceptID, batchSize, staleness, maxTokens, args.RepositoryID, args.SourceID, taskID, &result, summarizationModelOverride, summarizer)
	}

	log.Printf("summarize_concepts: repo %s processed %d pairs (%d created, %d updated, %d skipped no-delta, %d skipped locked, %d errors)",
		args.RepositoryID, result.PairsProcessed, result.SummariesCreated, result.SummariesUpdated, result.PairsSkippedNoDelta, result.PairsSkippedLocked, result.Errors)

	return river.RecordOutput(ctx, &result)
}

// summarizeOneConcept handles the per-concept claim/load/slice/write/
// release loop. It's split out of Work so the lock release (defer)
// is scoped to one concept without a goto.
func (w *SummarizeConceptsWorker) summarizeOneConcept(
	ctx context.Context,
	pool *pgxpool.Pool,
	conceptID pgtype.UUID,
	batchSize int,
	staleness time.Duration,
	maxTokens int,
	repoIDStr, sourceIDStr, taskID string,
	result *SummarizeConceptsResult,
	modelOverride string,
	summarizer summarization.SummarizationProvider,
) {
	// 1. Claim the lock. Skip if held by a fresh worker.
	claimCtx, claimCancel := context.WithTimeout(context.Background(), 10*time.Second)
	_, claimErr := store.New(pool).ClaimConceptForSummary(claimCtx, store.ClaimConceptForSummaryParams{
		ID:        conceptID,
		Staleness: pgtypeInterval(staleness),
	})
	claimCancel()
	if claimErr != nil {
		if errors.Is(claimErr, pgx.ErrNoRows) {
			result.PairsSkippedLocked++
			return
		}
		log.Printf("summarize_concepts: claiming concept %s: %v", pgUUIDToString(conceptID), claimErr)
		result.Errors++
		return
	}
	// Release on exit so a panic mid-loop still frees the lock.
	defer func() {
		relCtx, relCancel := context.WithTimeout(context.Background(), 10*time.Second)
		if err := store.New(pool).ReleaseConceptSummaryLock(relCtx, conceptID); err != nil {
			log.Printf("summarize_concepts: releasing lock for concept %s: %v", pgUUIDToString(conceptID), err)
		}
		relCancel()
	}()

	queries := store.New(pool)

	// 2. Load uncovered facts + the concept row (for canonical_name + context).
	concept, err := queries.GetConceptByID(ctx, conceptID)
	if err != nil {
		log.Printf("summarize_concepts: loading concept %s: %v", pgUUIDToString(conceptID), err)
		result.Errors++
		return
	}
	uncovered, err := queries.ListUncoveredFactIDsForSummary(ctx, conceptID)
	if err != nil {
		log.Printf("summarize_concepts: listing uncovered facts for concept %s: %v", pgUUIDToString(conceptID), err)
		result.Errors++
		return
	}
	if len(uncovered) == 0 {
		result.PairsSkippedNoDelta++
		return
	}

	// Check whether the concept already has at least one frozen
	// (complete) summary slice. Once it does, the worker switches
	// from the incremental open-accumulator path (first slice only,
	// regenerated as facts trickle in) to the batch-only path:
	// only emit complete BatchSize slices, never an open remainder,
	// and skip the pass entirely when fewer than BatchSize new facts
	// have arrived. This caps LLM cost — the first slice absorbs the
	// per-fact regeneration cost while a concept is still small,
	// then the worker waits for a full batch before spending another
	// call.
	hasComplete, err := queries.ExistsCompleteSummary(ctx, conceptID)
	if err != nil {
		log.Printf("summarize_concepts: checking complete summary for concept %s: %v", pgUUIDToString(conceptID), err)
		result.Errors++
		return
	}

	// Batch-only mode: once a complete slice exists, sub-batch
	// deltas are a no-op. The facts stay uncovered until a full
	// BatchSize batch accumulates.
	if hasComplete && len(uncovered) < batchSize {
		result.PairsSkippedNoDelta++
		return
	}

	// 3. Reconstruct the open slice's covered set + the new facts.
	open, err := queries.GetOpenSummary(ctx, conceptID)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		log.Printf("summarize_concepts: loading open summary for concept %s: %v", pgUUIDToString(conceptID), err)
		result.Errors++
		return
	}
	maxSeq, err := queries.GetMaxSummarySequenceNum(ctx, conceptID)
	if err != nil {
		log.Printf("summarize_concepts: loading max sequence for concept %s: %v", pgUUIDToString(conceptID), err)
		result.Errors++
		return
	}

	// accumulated = open.covered ++ uncovered (the open slice's
	// existing facts plus the new ones). If no open slice exists,
	// accumulated is just the uncovered facts.
	var accumulated []pgtype.UUID
	if open.ID.Valid {
		accumulated = append(accumulated, open.CoveredFactIds...)
	}
	accumulated = append(accumulated, uncovered...)

	// De-dup defensively in case the open slice's covered set and
	// the uncovered list overlap (shouldn't happen because the
	// ListUncoveredFactIDsForSummary NOT EXISTS filter excludes
	// covered facts, but a race is conceivable).
	accumulated = dedupUUIDs(accumulated)

	if len(accumulated) == 0 {
		result.PairsSkippedNoDelta++
		return
	}

	// 4. Slice into BatchSize complete + one open remainder.
	// seq is the sequence_num to use for the NEXT slice we create
	// or update. When an open slice exists, the first slice we
	// produce updates the existing open row (sequence_num =
	// open.SequenceNum); subsequent slices create new rows.
	seq := open.SequenceNum
	if !open.ID.Valid {
		// No open slice: start at maxSeq+1 (maxSeq is 0 when no
		// summaries exist yet, so the first slice is sequence_num=1).
		seq = maxSeq + 1
	}
	openID := open.ID // zero (Invalid) when no open slice exists
	openSeq := open.SequenceNum
	var hadComplete bool // tracks whether a crystallized slice was written this pass

	for len(accumulated) > 0 {
		// Decide the slice size + completion.
		var slice []pgtype.UUID
		var isComplete bool
		if len(accumulated) >= batchSize {
			slice = accumulated[:batchSize]
			isComplete = true
		} else {
			// Sub-batch remainder. The worker only emits an open
			// remainder while no complete slice exists yet (the
			// incremental first-slice phase). Once a complete slice
			// exists — either from a prior pass (hasComplete) or
			// frozen earlier in this same multi-slice pass
			// (hadComplete) — the leftover facts stay uncovered
			// until a full batch gathers. This caps LLM cost.
			if hasComplete || hadComplete {
				break
			}
			slice = accumulated
			isComplete = false
		}

		// 5. Load fact bodies + attribution for this slice.
		facts, err := w.loadFactInputs(ctx, pool, slice)
		if err != nil {
			log.Printf("summarize_concepts: loading facts for concept %s: %v", pgUUIDToString(conceptID), err)
			result.Errors++
			return
		}

		// 6. LLM call outside any transaction.
		// When a per-repo model override is set, pass it via
		// req.Model; the summarization wrapper prefers it over
		// the baked-in default (ai_summarization.go:64-67).
		summarizationModel := w.cfg.Model
		if modelOverride != "" {
			summarizationModel = modelOverride
		}
		llmCtx, llmCancel := context.WithTimeout(context.Background(), w.llmTimeout)
		content, err := summarizer.Summarize(llmCtx, pool, summarization.SummarizationRequest{
			ConceptCanonicalName: concept.CanonicalName,
			Context:              concept.Context,
			Facts:                facts,
			Model:                summarizationModel,
			MaxTokens:            maxTokens,
			TaskID:               taskID,
			Attribution: ai.Attribution{
				RepositoryID: repoIDStr,
				SourceID:     sourceIDStr,
				Operation:    "concept_summarization",
			},
		})
		llmCancel()
		if err != nil {
			log.Printf("summarize_concepts: summarizing concept %s (slice seq %d, %d facts): %v", pgUUIDToString(conceptID), seq, len(slice), err)
			result.Errors++
			return
		}

		// 7. Write in a short transaction.
		writeCtx, writeCancel := context.WithTimeout(context.Background(), 30*time.Second)
		tx, txErr := pool.BeginTx(writeCtx, pgx.TxOptions{})
		if txErr != nil {
			writeCancel()
			log.Printf("summarize_concepts: beginning write tx for concept %s: %v", pgUUIDToString(conceptID), txErr)
			result.Errors++
			return
		}
		txQueries := store.New(tx)
		modelName := w.cfg.Model
		if err := w.upsertSlice(writeCtx, txQueries, openID, openSeq, seq, concept, slice, isComplete, content, modelName, result); err != nil {
			tx.Rollback(writeCtx)
			writeCancel()
			log.Printf("summarize_concepts: upserting slice for concept %s: %v", pgUUIDToString(conceptID), err)
			result.Errors++
			return
		}
		if err := tx.Commit(writeCtx); err != nil {
			writeCancel()
			log.Printf("summarize_concepts: committing write tx for concept %s: %v", pgUUIDToString(conceptID), err)
			result.Errors++
			return
		}
		writeCancel()

		// After the first slice is written, subsequent slices are
		// always NEW rows (the open slice, if it existed, has been
		// frozen by the first slice). Clear openID so the next
		// iteration creates instead of updates.
		openID = pgtype.UUID{}
		seq++

		if isComplete {
			hadComplete = true
		}

		if !isComplete {
			// This was the open remainder; we're done.
			break
		}
		// Complete slice: drop the consumed facts and continue.
		accumulated = accumulated[len(slice):]
	}

	result.PairsProcessed++

	// Chain a synthesize_concept job ONLY when at least one COMPLETE
	// (crystallized) summary slice was written this pass. An open /
	// incomplete remainder slice update means more facts are still
	// arriving; triggering synthesis on every partial update wastes
	// tokens regenerating a definition from incomplete data. The
	// no-delta skip in the synthesis worker (coversAll) already
	// prevents redundant work for identical inputs, but the enqueue
	// itself — and the LLM call it triggers — is wasteful when the
	// only change was the open slice growing by a few facts.
	//
	// Gate: only enqueue when at least one complete slice was written
	// during this loop pass. This means concepts with fewer facts than
	// BatchSize (20) won't trigger synthesis until they accumulate
	// enough facts to crystallize a complete slice. A definition based
	// on fewer than 20 facts would be thin anyway, and the next fact
	// batch will naturally trigger it when the open slice crystallizes.
	if w.synthesizerEnabled && hadComplete {
		w.enqueueSynthesizeConcept(ctx, repoIDStr, sourceIDStr, conceptID)
	}
}

// enqueueSynthesizeConcept inserts one synthesize_concept job for the
// given concept_id. Failures are logged and swallowed so a synthesis
// enqueue problem never fails the summarize_concepts job (the summary
// pipeline still completes; the next summarize_concepts pass or a
// manual re-enqueue will retry synthesis).
func (w *SummarizeConceptsWorker) enqueueSynthesizeConcept(ctx context.Context, repoIDStr, sourceIDStr string, conceptID pgtype.UUID) {
	client := river.ClientFromContext[pgx.Tx](ctx)
	if client == nil {
		log.Printf("summarize_concepts: no river client on context; synthesize_concept not enqueued for concept %s", pgUUIDToString(conceptID))
		return
	}
	insertCtx, insertCancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer insertCancel()
	if _, err := client.Insert(insertCtx, SynthesizeConceptArgs{
		RepositoryID: repoIDStr,
		ConceptID:    pgUUIDToString(conceptID),
		SourceID:     sourceIDStr,
	}, &river.InsertOpts{
		Queue: QueueSynthesizeConcept,
		Metadata: MarshalMetadata(JobMetadata{
			RepositoryID: repoIDStr,
			SourceID:     sourceIDStr,
		}),
	}); err != nil {
		log.Printf("summarize_concepts: enqueueing synthesize_concept for concept %s: %v", pgUUIDToString(conceptID), err)
	}
}

// upsertSlice writes one summary slice. When the slice reuses an
// existing open summary row (openID.Valid AND openSeq == seq), the
// slice updates that row (preserving its identity). Otherwise it
// creates a new row. is_complete toggles the open/frozen state.
func (w *SummarizeConceptsWorker) upsertSlice(
	ctx context.Context,
	queries *store.Queries,
	openID pgtype.UUID,
	openSeq, seq int32,
	concept store.OktRepositoryConcept,
	slice []pgtype.UUID,
	isComplete bool,
	content, modelName string,
	result *SummarizeConceptsResult,
) error {
	// Update the existing open row when the caller says so (openID
	// Valid AND the sequence matches). The open row's sequence_num
	// is preserved; only content / covered / fact_count / is_complete
	// change. When is_complete flips TRUE the row freezes.
	if openID.Valid && openSeq == seq {
		if _, err := queries.UpdateSummary(ctx, store.UpdateSummaryParams{
			ID:             openID,
			Content:        content,
			CoveredFactIds: slice,
			FactCount:      int32(len(slice)),
			IsComplete:     isComplete,
			Model:          &modelName,
		}); err != nil {
			return fmt.Errorf("update open summary: %w", err)
		}
		result.SummariesUpdated++
		return nil
	}
	if _, err := queries.CreateSummary(ctx, store.CreateSummaryParams{
		ConceptID:      concept.ID,
		RepositoryID:   concept.RepositoryID,
		Context:        concept.Context,
		SequenceNum:    seq,
		IsComplete:     isComplete,
		FactCount:      int32(len(slice)),
		Content:        content,
		CoveredFactIds: slice,
		Model:          &modelName,
	}); err != nil {
		return fmt.Errorf("create summary: %w", err)
	}
	result.SummariesCreated++
	return nil
}

// loadFactInputs fetches the fact bodies and source attribution for
// a slice of fact_ids, returning the FactInput list the
// SummarizationProvider expects. Two queries (one for bodies, one
// for sources) so there's no N+1.
func (w *SummarizeConceptsWorker) loadFactInputs(ctx context.Context, pool *pgxpool.Pool, factIDs []pgtype.UUID) ([]summarization.FactInput, error) {
	if len(factIDs) == 0 {
		return nil, nil
	}
	queries := store.New(pool)
	factRows, err := queries.GetFactsForSummary(ctx, factIDs)
	if err != nil {
		return nil, fmt.Errorf("loading fact bodies: %w", err)
	}
	byID := make(map[pgtype.UUID]string, len(factRows))
	for _, f := range factRows {
		byID[f.ID] = f.Text
	}
	srcRows, err := queries.ListFactSourcesByFactIDs(ctx, factIDs)
	if err != nil {
		return nil, fmt.Errorf("loading fact sources: %w", err)
	}
	// Build the attribution string per fact: "(title1; title2)" or
	// "" when the fact has no sources. Prefer parsed_title; fall back
	// to the source URL's host so an untitled source still gets a
	// recognizable name.
	attrPerFact := make(map[pgtype.UUID][]string)
	for _, s := range srcRows {
		name := ""
		if s.ParsedTitle != nil && *s.ParsedTitle != "" {
			name = *s.ParsedTitle
		} else {
			name = hostFromURL(s.Url)
		}
		if name == "" {
			continue
		}
		attrPerFact[s.FactID] = append(attrPerFact[s.FactID], name)
	}

	out := make([]summarization.FactInput, 0, len(factIDs))
	for _, fid := range factIDs {
		text, ok := byID[fid]
		if !ok {
			continue
		}
		attr := ""
		if names := attrPerFact[fid]; len(names) > 0 {
			attr = "(" + strings.Join(names, "; ") + ")"
		}
		out = append(out, summarization.FactInput{
			ID:          pgUUIDToString(fid),
			Text:        text,
			Attribution: attr,
		})
	}
	return out, nil
}

// listTouchedConceptIDs is the fallback path used when the worker
// is enqueued without an explicit ConceptIDs chunk (manual
// re-enqueue / periodic catch-up). It lists distinct concept_ids
// linked to facts from the given source (or repo-wide when sourceID
// is empty), capped at MaxConceptsPerRun.
func (w *SummarizeConceptsWorker) listTouchedConceptIDs(ctx context.Context, pool *pgxpool.Pool, repoID pgtype.UUID, sourceID string) ([]string, error) {
	queries := store.New(pool)
	var rows []pgtype.UUID
	if sourceID != "" {
		var srcID pgtype.UUID
		if err := srcID.Scan(sourceID); err != nil {
			return nil, fmt.Errorf("invalid source_id: %w", err)
		}
		listed, err := queries.ListTouchedConceptsForSummary(ctx, store.ListTouchedConceptsForSummaryParams{
			RepositoryID: repoID, SourceID: srcID,
		})
		if err != nil {
			return nil, err
		}
		for _, r := range listed {
			rows = append(rows, r.ID)
		}
	} else {
		listed, err := queries.ListTouchedConceptsForSummaryByRepo(ctx, repoID)
		if err != nil {
			return nil, err
		}
		for _, r := range listed {
			rows = append(rows, r.ID)
		}
	}
	out := make([]string, 0, len(rows))
	maxPerRun := w.cfg.MaxConceptsPerRunOr(40)
	for i, id := range rows {
		if i >= maxPerRun {
			break
		}
		out = append(out, pgUUIDToString(id))
	}
	return out, nil
}

// hostFromURL extracts the host portion of a URL for attribution
// fallback. Returns "" on a parse failure so the caller can skip the
// attribution entry.
func hostFromURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return ""
	}
	return u.Host
}

// pgtypeInterval converts a Go time.Duration into a pgtype.Interval
// the sqlc-generated ClaimConceptForSummary expects.
func pgtypeInterval(d time.Duration) pgtype.Interval {
	return pgtype.Interval{Microseconds: int64(d / time.Microsecond), Valid: true}
}

// dedupUUIDs returns the input slice with duplicates removed,
// preserving order. Used defensively on the accumulated fact list
// (open.covered ++ uncovered should already be disjoint, but a race
// between the NOT EXISTS filter and the open slice's covered array
// is conceivable).
func dedupUUIDs(in []pgtype.UUID) []pgtype.UUID {
	if len(in) <= 1 {
		return in
	}
	seen := make(map[[16]byte]bool, len(in))
	out := in[:0]
	for _, u := range in {
		var key [16]byte
		copy(key[:], u.Bytes[:])
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, u)
	}
	return out
}