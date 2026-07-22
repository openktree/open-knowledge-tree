package tasks

import (
	"context"
	"errors"
	"fmt"
	"log"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/openktree/open-knowledge-tree/backend/internal/config"
	"github.com/openktree/open-knowledge-tree/backend/internal/dbpool"
	"github.com/openktree/open-knowledge-tree/backend/internal/promptset"
	"github.com/openktree/open-knowledge-tree/backend/internal/providers/ai"
	"github.com/openktree/open-knowledge-tree/backend/internal/providers/synthesis"
	"github.com/openktree/open-knowledge-tree/backend/internal/store"
	"github.com/riverqueue/river"
)

const QueueSynthesizeConcept = "synthesize_concept"

// SynthesizeConceptArgs triggers a synthesis regeneration for one
// concept_id. The worker resolves the concept_id to its canonical-name
// group (repository_id + lower(canonical_name)), loads ALL summary
// slices across the group, optionally picks up to MaxImages image
// facts via a separate LLM call, runs the synthesis LLM call, and
// upserts the single concept_syntheses row for the group.
//
// summarize_concepts enqueues one SynthesizeConcept job per
// concept_id after a successful slice write. Because the group spans
// multiple concept_ids, several enqueues may target the same group;
// the worker's advisory lock + no-delta skip (covered_summary_ids
// already ⊇ the slices) coalesce concurrent jobs into one regeneration.
type SynthesizeConceptArgs struct {
	RepositoryID string `json:"repository_id"`
	ConceptID    string `json:"concept_id"`
	// SourceID is informational only — carried so ai_usage attribution
	// still reflects the source that triggered the pipeline.
	SourceID string `json:"source_id,omitempty"`
}

func (SynthesizeConceptArgs) Kind() string { return "synthesize_concept" }

func (SynthesizeConceptArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{
		Queue: QueueSynthesizeConcept,
		// Retry budget: a synthesis failure is usually a transient LLM
		// provider error (rate limit, timeout, empty response) or a
		// transient DB error (connection blip, lock conflict). River's
		// exponential backoff resolves it without operator intervention.
		// 5 attempts matches the retry budget used by other LLM-bound
		// repair jobs (refresh_concept_relations, recompute_concept_groups).
		MaxAttempts: 5,
	}
}

// SynthesizeConceptResult is recorded on the job row so the River UI
// shows what the pass did.
type SynthesizeConceptResult struct {
	RepositoryID    string `json:"repository_id"`
	CanonicalName   string `json:"canonical_name"`
	Created         int    `json:"created"`
	Updated         int    `json:"updated"`
	SkippedNoDelta  int    `json:"skipped_no_delta"`
	SkippedLocked   int    `json:"skipped_locked"`
	ImagesPicked     int   `json:"images_picked"`
	Errors          int    `json:"errors"`
}

type SynthesizeConceptsWorker struct {
	river.WorkerDefaults[SynthesizeConceptArgs]

	synthesizer     synthesis.SynthesisProvider
	cfg             config.SynthesisConfig
	registry        *dbpool.Registry
	systemQueries   *store.Queries
	modelResolver   *ModelResolver
	promptsetResolver *PromptsetResolver
	// pickerTimeout is the per-call wall-clock timeout for the
	// image-picker LLM call. Default 20m; set via SetPickerTimeout
	// from cfg.Providers.Synthesis.PickerTimeout.
	pickerTimeout time.Duration
	// llmTimeout is the per-call wall-clock timeout for the
	// synthesis LLM call. Default 25m (synthesis produces longer
	// output than the picker); set via SetLLMTimeout from
	// cfg.Providers.Synthesis.LLMTimeout.
	llmTimeout time.Duration
}

func NewSynthesizeConceptsWorker(
	synthesizer synthesis.SynthesisProvider,
	cfg config.SynthesisConfig,
	registry *dbpool.Registry,
	systemQueries *store.Queries,
	modelResolver *ModelResolver,
	promptsetResolver *PromptsetResolver,
) *SynthesizeConceptsWorker {
	return &SynthesizeConceptsWorker{
		synthesizer:       synthesizer,
		cfg:               cfg,
		registry:          registry,
		systemQueries:     systemQueries,
		modelResolver:     modelResolver,
		promptsetResolver: promptsetResolver,
		pickerTimeout:     20 * time.Minute, // default; overridden via SetPickerTimeout
		llmTimeout:        25 * time.Minute, // default; overridden via SetLLMTimeout
	}
}

// SetPickerTimeout sets the per-call wall-clock timeout for the
// image-picker LLM call. Default 20m when unset.
func (w *SynthesizeConceptsWorker) SetPickerTimeout(d time.Duration) {
	if d > 0 {
		w.pickerTimeout = d
	}
}

// SetLLMTimeout sets the per-call wall-clock timeout for the
// synthesis LLM call. Default 25m when unset.
func (w *SynthesizeConceptsWorker) SetLLMTimeout(d time.Duration) {
	if d > 0 {
		w.llmTimeout = d
	}
}

// Work resolves the concept_id to its group, acquires a group-keyed
// advisory lock, loads the group's slices + image candidates, runs
// the picker (when needed) and the synthesis LLM call, and upserts
// the single synthesis row.
//
// The worker is a no-op when synthesizer == nil or cfg.Enabled is
// false (deployment choice, not retryable). It is terminal — no
// follow-up chain.
func (w *SynthesizeConceptsWorker) Work(ctx context.Context, job *river.Job[SynthesizeConceptArgs]) error {
	args := job.Args
	if args.RepositoryID == "" || args.ConceptID == "" {
		return fmt.Errorf("synthesize_concept: repository_id and concept_id are required")
	}

	if w.synthesizer == nil {
		log.Printf("synthesize_concept: synthesis provider not configured, skipping concept %s", args.ConceptID)
		return river.RecordOutput(ctx, &SynthesizeConceptResult{RepositoryID: args.RepositoryID})
	}
	if !w.cfg.Enabled {
		log.Printf("synthesize_concept: synthesis not enabled, skipping concept %s", args.ConceptID)
		return river.RecordOutput(ctx, &SynthesizeConceptResult{RepositoryID: args.RepositoryID})
	}

	repoID := pgtype.UUID{}
	if err := repoID.Scan(args.RepositoryID); err != nil {
		return fmt.Errorf("synthesize_concept: invalid repository_id: %w", err)
	}
	conceptID := pgtype.UUID{}
	if err := conceptID.Scan(args.ConceptID); err != nil {
		return fmt.Errorf("synthesize_concept: invalid concept_id: %w", err)
	}

	dbName, err := w.systemQueries.GetRepositoryDatabaseName(ctx, repoID)
	if err != nil {
		return fmt.Errorf("synthesize_concept: resolving repository database: %w", err)
	}
	pool := w.registry.Get(dbName)

	// Resolve the repo's effective promptset once at Work() start.
	// The resolved Promptset is threaded into the synthesizer (and
	// its image-picker) via WithPromptset so the repo's philosophy
	// runs, not the built-in default.
	var ps promptset.Promptset
	if w.promptsetResolver != nil {
		ps = w.promptsetResolver.Effective(ctx, repoID)
		w.promptsetResolver.LogEffective(ctx, repoID, "synthesize_concept")
	} else {
		ps = promptset.Default
	}
	synthesizer := w.synthesizer
	if s, ok := synthesizer.(*synthesis.AISynthesisProvider); ok {
		synthesizer = s.WithPromptset(ps)
	}

	// Resolve per-repo model override for synthesis. The synthesis
	// wrapper already honors a non-empty req.Model (ai_synthesis.go:
	// model := req.Model; if "" { model = p.model }), so we pass the
	// override model id through.
	var synthesisModelOverride string
	if w.modelResolver != nil {
		if r := w.modelResolver.Resolve(ctx, repoID, TaskKindSynthesis); r.Provider != nil {
			synthesisModelOverride = r.ModelID
		}
	}

	maxTokens := w.cfg.MaxTokensOr(1200)
	maxImages := w.cfg.MaxImagesOr(10)
	maxCands := w.cfg.MaxImageCandidatesOr(50)
	// N1: top related concept names + per-context counts (graph block).
	// N2: of those N1, how many also carry their synthesis text.
	// Load() clamps N2 <= N1; cfg.MaxRelatedConcepts/MaxRelatedSyntheses
	// are populated there from the *Or defaults.
	maxRelated := w.cfg.MaxRelatedConcepts
	maxRelatedSynth := w.cfg.MaxRelatedSyntheses
	var thinkingLevel *string
	if w.cfg.ThinkingLevel != "" {
		tl := w.cfg.ThinkingLevel
		thinkingLevel = &tl
	}
	taskID := fmt.Sprintf("%d", job.ID)
	result := SynthesizeConceptResult{RepositoryID: args.RepositoryID}

	err = w.synthesizeOneGroup(ctx, pool.Pool, conceptID, repoID, args.RepositoryID, args.SourceID,
		maxTokens, maxImages, maxCands, maxRelated, maxRelatedSynth, thinkingLevel, taskID, &result, synthesisModelOverride, synthesizer)

	log.Printf("synthesize_concept: repo %s concept %s -> %s (created=%d updated=%d skipped-no-delta=%d skipped-locked=%d images=%d errors=%d)",
		args.RepositoryID, args.ConceptID, result.CanonicalName,
		result.Created, result.Updated, result.SkippedNoDelta, result.SkippedLocked, result.ImagesPicked, result.Errors)

	// Record the partial result so the River UI shows the attempt's
	// counts even when the job will be retried. A non-nil error makes
	// River retry the job up to MaxAttempts (5) with exponential backoff;
	// a nil error completes the job.
	_ = river.RecordOutput(ctx, &result)
	return err
}

// synthesizeOneGroup handles the per-concept claim/load/pick/write/
// release loop. Split out of Work so the lock release (defer) is
// scoped to one group. Returns a non-nil error for retryable
// failures (LLM call, DB writes, slice/concept loads) so River
// retries the job up to MaxAttempts. Returns nil for terminal
// no-ops (concept not found, lock held, no slices, no-delta skip,
// non-fatal image failures) so River does not waste retry budget.
func (w *SynthesizeConceptsWorker) synthesizeOneGroup(
	ctx context.Context,
	pool *pgxpool.Pool,
	conceptID, repoID pgtype.UUID,
	repoIDStr, sourceIDStr string,
	maxTokens, maxImages, maxCands, maxRelated, maxRelatedSynth int,
	thinkingLevel *string,
	taskID string,
	result *SynthesizeConceptResult,
	synthesisModelOverride string,
	synthesizer synthesis.SynthesisProvider,
) error {
	queries := store.New(pool)

	// 1. Resolve the concept_id -> canonical_name (group key).
	concept, err := queries.GetConceptByID(ctx, conceptID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			log.Printf("synthesize_concept: concept %s not found", pgUUIDToString(conceptID))
			result.Errors++
			return nil
		}
		log.Printf("synthesize_concept: loading concept %s: %v", pgUUIDToString(conceptID), err)
		result.Errors++
		return nil
	}
	result.CanonicalName = concept.CanonicalName
	groupKey := repoIDStr + ":" + strings.ToLower(concept.CanonicalName)

	// 2. Acquire the group-keyed advisory lock. Skip if held.
	lockCtx, lockCancel := context.WithTimeout(context.Background(), 10*time.Second)
	ok, err := queries.TryAdvisoryLockForSynthesis(lockCtx, groupKey)
	lockCancel()
	if err != nil {
		log.Printf("synthesize_concept: acquiring lock for group %s: %v", groupKey, err)
		result.Errors++
		return nil
	}
	if !ok {
		result.SkippedLocked++
		return nil
	}
	defer func() {
		relCtx, relCancel := context.WithTimeout(context.Background(), 10*time.Second)
		if err := store.New(pool).ReleaseAdvisoryLockForSynthesis(relCtx, groupKey); err != nil {
			log.Printf("synthesize_concept: releasing lock for group %s: %v", groupKey, err)
		}
		relCancel()
	}()

	// 3. Load the group's summary slices + concept_ids.
	slices, err := queries.ListSummariesByCanonicalNameGroup(ctx, store.ListSummariesByCanonicalNameGroupParams{
		RepositoryID:  repoID,
		CanonicalName: concept.CanonicalName,
	})
	if err != nil {
		log.Printf("synthesize_concept: listing slices for group %s: %v", groupKey, err)
		result.Errors++
		return fmt.Errorf("synthesize_concept: listing slices for group %s: %w", groupKey, err)
	}
	if len(slices) == 0 {
		result.SkippedNoDelta++
		return nil
	}

	groupConceptIDs, err := queries.ListGroupConceptIDs(ctx, store.ListGroupConceptIDsParams{
		RepositoryID:  repoID,
		CanonicalName: concept.CanonicalName,
	})
	if err != nil {
		log.Printf("synthesize_concept: listing group concept_ids for %s: %v", groupKey, err)
		result.Errors++
		return fmt.Errorf("synthesize_concept: listing group concept_ids for %s: %w", groupKey, err)
	}

	// 4. No-delta skip: if a synthesis exists and its covered_summary_ids
	//    already contains every slice id, there is nothing new to fold.
	existing, err := queries.GetSynthesisByGroup(ctx, store.GetSynthesisByGroupParams{
		RepositoryID:  repoID,
		CanonicalName: concept.CanonicalName,
	})
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		log.Printf("synthesize_concept: loading existing synthesis for %s: %v", groupKey, err)
		result.Errors++
		return fmt.Errorf("synthesize_concept: loading existing synthesis for %s: %w", groupKey, err)
	}
	if existing.ID.Valid && coversAll(existing.CoveredSummaryIds, slices) {
		result.SkippedNoDelta++
		return nil
	}

	// 5. Build SliceInput list for the synthesis prompt.
	sliceInputs := make([]synthesis.SliceInput, 0, len(slices))
	sliceIDs := make([]pgtype.UUID, 0, len(slices))
	for _, s := range slices {
		sliceInputs = append(sliceInputs, synthesis.SliceInput{
			ID:          pgUUIDToString(s.ID),
			ConceptID:   pgUUIDToString(s.ConceptID),
			SequenceNum: s.SequenceNum,
			FactCount:   s.FactCount,
			Content:     s.Content,
		})
		sliceIDs = append(sliceIDs, s.ID)
	}
	// Ensure non-nil for the NOT NULL DEFAULT '{}' columns.
	if sliceIDs == nil {
		sliceIDs = []pgtype.UUID{}
	}

	// groupConceptIDs is guaranteed non-empty (the concept we
	// resolved is in the group), but guard defensively.
	groupCIDs := groupConceptIDs
	if groupCIDs == nil {
		groupCIDs = []pgtype.UUID{}
	}

	// 5b. Related concepts (Graph-Aware Reasoning context): the top
	//     N1 related concept names + per-context shared_fact_counts
	//     (drives the graph-structure block of the prompt), of which
	//     the top N2 by shared_fact_count also carry their existing
	//     concept_syntheses.content verbatim. Loaded from the
	//     concept_relations matview + ListConceptRelationDetailsByName
	//     + GetSynthesisByGroup. Any error is non-fatal: the synthesis
	//     proceeds without relations (the prompt announces the
	//     absence). maxRelated==0 disables the block entirely.
	var relatedConcepts []synthesis.RelatedConceptInput
	if maxRelated > 0 {
		relatedConcepts = w.loadRelatedConcepts(ctx, queries, repoID, concept.CanonicalName, maxRelated, maxRelatedSynth, groupKey)
	}

	// 6. Image candidates: load the group's image facts (capped).
	//    - 0 candidates  -> no images, no picker call.
	//    - <= maxImages  -> skip picker, pass all directly.
	//    - >  maxImages  -> picker call; on failure, proceed with no images.
	var candidateImages []synthesis.ImageInput
	if maxImages > 0 {
		imgRows, err := queries.ListGroupImageFacts(ctx, store.ListGroupImageFactsParams{
			RepositoryID:  repoID,
			CanonicalName: concept.CanonicalName,
			Cap:           int32(maxCands),
		})
		if err != nil {
			log.Printf("synthesize_concept: listing image candidates for %s: %v", groupKey, err)
			// Non-fatal: proceed with no images.
		} else {
			candidateImages = imageRowsToInputs(imgRows)
		}
	}

	var chosenImages []synthesis.ImageInput
	if len(candidateImages) > 0 {
		if len(candidateImages) <= maxImages {
			chosenImages = candidateImages
		} else {
			pickCtx, pickCancel := context.WithTimeout(context.Background(), w.pickerTimeout)
			picked, perr := synthesizer.PickImages(pickCtx, pool, synthesis.ImagePickRequest{
				CanonicalName: concept.CanonicalName,
				Context:       concept.Context,
				Candidates:    candidateImages,
				MaxImages:     maxImages,
				Model:         pickModelOverride(synthesisModelOverride, w.cfg),
				MaxTokens:     300,
				TaskID:        taskID,
				Attribution: ai.Attribution{
					RepositoryID: repoIDStr,
					SourceID:     sourceIDStr,
					Operation:    "concept_image_picking",
				},
			})
			pickCancel()
			if perr != nil {
				log.Printf("synthesize_concept: image picker failed for %s (proceeding without images): %v", groupKey, perr)
				chosenImages = nil
			} else {
				chosenImages = resolvePickedImages(picked, candidateImages, maxImages)
			}
		}
	}
	result.ImagesPicked = len(chosenImages)

	// 7. Synthesis LLM call (outside any transaction, 180s background ctx).
	synthCtx, synthCancel := context.WithTimeout(context.Background(), w.llmTimeout)
	content, err := synthesizer.Synthesize(synthCtx, pool, synthesis.SynthesisRequest{
		CanonicalName:   concept.CanonicalName,
		Context:         concept.Context,
		Slices:          sliceInputs,
		CandidateImages: chosenImages,
		MaxImages:       maxImages,
		RelatedConcepts: relatedConcepts,
		Model:           synthesisModel(synthesisModelOverride, w.cfg.Model),
		MaxTokens:       maxTokens,
		ThinkingLevel:   thinkingLevel,
		TaskID:          taskID,
		Attribution: ai.Attribution{
			RepositoryID: repoIDStr,
			SourceID:     sourceIDStr,
			Operation:    "concept_synthesis",
		},
	})
	synthCancel()
	if err != nil {
		log.Printf("synthesize_concept: synthesizing group %s: %v", groupKey, err)
		result.Errors++
		return fmt.Errorf("synthesize_concept: synthesizing group %s: %w", groupKey, err)
	}
	if content == "" {
		log.Printf("synthesize_concept: empty synthesis content for group %s", groupKey)
		result.Errors++
		return fmt.Errorf("synthesize_concept: empty synthesis content for group %s", groupKey)
	}

	// 8. Extract embedded image fact_ids from the markdown so the read
	//    path can eager-load those facts' image_url without parsing
	//    markdown on every GET. Ensure a non-nil slice so the
	//    NOT NULL DEFAULT '{}' column doesn't receive a SQL NULL
	//    (pgx encodes a nil []pgtype.UUID as NULL).
	embeddedImageIDs := extractEmbeddedImageIDs(content)
	if embeddedImageIDs == nil {
		embeddedImageIDs = []pgtype.UUID{}
	}

	// 9. Short write tx: upsert the synthesis row.
	writeCtx, writeCancel := context.WithTimeout(context.Background(), 30*time.Second)
	tx, txErr := pool.BeginTx(writeCtx, pgx.TxOptions{})
	if txErr != nil {
		writeCancel()
		log.Printf("synthesize_concept: beginning write tx for %s: %v", groupKey, txErr)
		result.Errors++
		return fmt.Errorf("synthesize_concept: beginning write tx for %s: %w", groupKey, txErr)
	}
	modelName := w.cfg.Model
	row, err := store.New(tx).UpsertSynthesis(writeCtx, store.UpsertSynthesisParams{
		RepositoryID:      repoID,
		CanonicalName:     concept.CanonicalName,
		Content:           content,
		CoveredSummaryIds: sliceIDs,
		CoveredConceptIds: groupCIDs,
		EmbeddedImageIds:  embeddedImageIDs,
		Model:             &modelName,
	})
	if err != nil {
		tx.Rollback(writeCtx)
		writeCancel()
		log.Printf("synthesize_concept: upserting synthesis for %s: %v", groupKey, err)
		result.Errors++
		return fmt.Errorf("synthesize_concept: upserting synthesis for %s: %w", groupKey, err)
	}
	if err := tx.Commit(writeCtx); err != nil {
		writeCancel()
		log.Printf("synthesize_concept: committing write tx for %s: %v", groupKey, err)
		result.Errors++
		return fmt.Errorf("synthesize_concept: committing write tx for %s: %w", groupKey, err)
	}
	writeCancel()

	if existing.ID.Valid && existing.ID.Bytes == row.ID.Bytes {
		result.Updated++
	} else {
		result.Created++
	}
	return nil
}

// imageRowsToInputs converts the sqlc ListGroupImageFactsRow slice
// into the synthesis.ImageInput list the provider expects. Alt is
// derived from the image description (a future extractor may supply
// a dedicated alt field).
func imageRowsToInputs(rows []store.ListGroupImageFactsRow) []synthesis.ImageInput {
	out := make([]synthesis.ImageInput, 0, len(rows))
	for _, r := range rows {
		alt := r.Text
		if len(alt) > 80 {
			alt = alt[:80] + "..."
		}
		out = append(out, synthesis.ImageInput{
			FactID: pgUUIDToString(r.ID),
			Text:   r.Text,
			Alt:    alt,
		})
	}
	return out
}

// resolvePickedImages filters the picker's returned ids to those
// actually present in the candidate set (defensive against
// hallucinated ids), resolves them back to ImageInputs, and caps at
// maxImages.
func resolvePickedImages(picked []string, candidates []synthesis.ImageInput, maxImages int) []synthesis.ImageInput {
	if len(picked) == 0 {
		return nil
	}
	byID := make(map[string]synthesis.ImageInput, len(candidates))
	for _, c := range candidates {
		byID[c.FactID] = c
	}
	out := make([]synthesis.ImageInput, 0, len(picked))
	seen := make(map[string]bool, len(picked))
	for _, id := range picked {
		if seen[id] {
			continue
		}
		seen[id] = true
		if c, ok := byID[id]; ok {
			out = append(out, c)
			if maxImages > 0 && len(out) >= maxImages {
				break
			}
		}
	}
	return out
}

// coversAll reports whether every slice id is present in covered.
// Used for the no-delta skip: when the existing synthesis already
// folds every current slice, regeneration would produce the same
// covered set (the LLM output may differ but the inputs are
// unchanged), so we skip.
func coversAll(covered []pgtype.UUID, slices []store.OktRepositoryConceptSummary) bool {
	if len(slices) == 0 {
		return true
	}
	set := make(map[[16]byte]bool, len(covered))
	for _, c := range covered {
		var k [16]byte
		copy(k[:], c.Bytes[:])
		set[k] = true
	}
	for _, s := range slices {
		var k [16]byte
		copy(k[:], s.ID.Bytes[:])
		if !set[k] {
			return false
		}
	}
	return true
}

// embeddedImageIDRe matches ![alt](<fact:uuid>) markdown image
// citations the synthesis is prompted to emit, and tolerates the
// legacy bare-<uuid> form produced by older summaries before the
// "fact:" kind prefix was introduced. The uuid is captured so the
// worker can store embedded_image_ids for the read path.
var embeddedImageIDRe = regexp.MustCompile(`!\[[^\]]*\]\(<(?:fact:)?([0-9a-fA-F-]{36})>\)`)

// extractEmbeddedImageIDs returns the deduplicated list of image
// fact_ids the synthesis embeds via ![alt](<fact:fact_id>). The worker
// stores this on concept_syntheses.embedded_image_ids so the GET
// /definition endpoint can eager-load those facts' image_url without
// parsing markdown on every read.
func extractEmbeddedImageIDs(markdown string) []pgtype.UUID {
	matches := embeddedImageIDRe.FindAllStringSubmatch(markdown, -1)
	if len(matches) == 0 {
		return nil
	}
	seen := make(map[[16]byte]bool, len(matches))
	out := make([]pgtype.UUID, 0, len(matches))
	for _, m := range matches {
		var u pgtype.UUID
		if err := u.Scan(m[1]); err != nil {
			continue
		}
		var k [16]byte
		copy(k[:], u.Bytes[:])
		if seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, u)
	}
	return out
}

// maxRelatedContexts caps how many per-context breakdown rows are
// rendered per related concept, to keep the synthesis prompt bounded
// even for well-connected concepts with many contexts.
const maxRelatedContexts = 5

// loadRelatedConcepts fetches the top N1 related concepts (by
// shared_fact_count) for the given concept group, with their
// per-context shared_fact_count breakdowns, and — for the top N2 of
// those — attaches the related concept's existing synthesis text
// verbatim. It is best-effort enrichment: ANY error (matview not
// refreshed, query failure, synthesis lookup failure) is logged and
// returns nil so the synthesis proceeds without relations (the prompt
// announces the absence). Callers MUST pass n2 <= n1 (Load() clamps
// the config).
//
// The queries used:
//   - ListConceptRelationsByConceptName: matview-backed, returns
//     other_name, representative canonical_name/concept_id (interface{}
//     from MAX/MIN over mixed types), and aggregate shared_fact_count,
//     ordered by shared_fact_count DESC. Limited to n1.
//   - ListConceptRelationDetailsByConceptName: live fact_concepts
//     query, one row per context of A with shared_fact_count aggregated
//     across all of B's contexts. Called once per related concept.
//   - GetSynthesisByGroup: returns the related concept's synthesis row
//     if one exists; pgx.ErrNoRows means "no synthesis yet" (the
//     RelatedConceptInput.Synthesis stays "").
func (w *SynthesizeConceptsWorker) loadRelatedConcepts(
	ctx context.Context,
	queries *store.Queries,
	repoID pgtype.UUID,
	canonicalName string,
	n1, n2 int,
	groupKey string,
) []synthesis.RelatedConceptInput {
	if n1 <= 0 {
		return nil
	}
	if n2 > n1 {
		n2 = n1
	}

	rows, err := queries.ListConceptRelationsByConceptName(ctx, store.ListConceptRelationsByConceptNameParams{
		RepositoryID: repoID,
		Lower:        canonicalName,
		Limit:        int32(n1),
		Offset:       0,
	})
	if err != nil {
		log.Printf("synthesize_concept: listing related concepts for %s: %v (proceeding without)", groupKey, err)
		return nil
	}
	if len(rows) == 0 {
		return nil
	}

	out := make([]synthesis.RelatedConceptInput, 0, len(rows))
	for i, row := range rows {
		name, _ := row.CanonicalName.(string)
		if name == "" {
			name = row.OtherName
		}
		rc := synthesis.RelatedConceptInput{
			CanonicalName:   name,
			ConceptID:       pgUUIDInterfaceToString(row.ConceptID),
			SharedFactCount: int32(row.SharedFactCount),
		}

		// Per-context breakdown. Errors here are non-fatal: we
		// keep the concept with empty Contexts rather than dropping
		// it entirely.
		details, derr := queries.ListConceptRelationDetailsByConceptName(ctx, store.ListConceptRelationDetailsByConceptNameParams{
			RepositoryID: repoID,
			Lower:        canonicalName,
			Lower_2:      row.OtherName,
		})
		if derr != nil {
			log.Printf("synthesize_concept: loading relation details for %s <-> %s: %v (keeping name+count only)",
				groupKey, row.OtherName, derr)
		} else {
			capped := details
			if len(capped) > maxRelatedContexts {
				capped = capped[:maxRelatedContexts]
			}
			rc.Contexts = make([]synthesis.RelatedContext, 0, len(capped))
			for _, d := range capped {
				rc.Contexts = append(rc.Contexts, synthesis.RelatedContext{
					Context:         d.Context,
					SharedFactCount: int32(d.SharedFactCount),
				})
			}
		}

		// Synthesis text: only for the top n2 by rank (rows are
		// already ordered by shared_fact_count DESC). Missing
		// synthesis is NOT a failure — the related concept may not
		// have been synthesized yet; Synthesis stays "".
		if i < n2 {
			synth, serr := queries.GetSynthesisByGroup(ctx, store.GetSynthesisByGroupParams{
				RepositoryID:  repoID,
				CanonicalName: row.OtherName,
			})
			if serr != nil && !errors.Is(serr, pgx.ErrNoRows) {
				log.Printf("synthesize_concept: loading related synthesis for %s <-> %s: %v (no synthesis attached)",
					groupKey, row.OtherName, serr)
			} else if serr == nil {
				rc.Synthesis = synth.Content
			}
		}

		out = append(out, rc)
	}
	return out
}

// pgUUIDInterfaceToString coerces the interface{} value sqlc generates
// for MIN(c.id::text) over a UNION subquery into a canonical UUID
// string. pgx returns either a pgtype.UUID or a string; both are
// handled. Mirrors handler.pgUUIDInterfaceToString (kept local to
// avoid importing the api/handler package from a task worker).
func pgUUIDInterfaceToString(v interface{}) string {
	switch id := v.(type) {
	case pgtype.UUID:
		if id.Valid {
			return pgUUIDToString(id)
		}
	case string:
		return id
	case []byte:
		return string(id)
	}
	return ""
}

// synthesisModel returns the model id to use for the synthesis LLM
// call. When a per-repo override is set, it takes precedence;
// otherwise the global config default is used.
func synthesisModel(override, defaultModel string) string {
	if override != "" {
		return override
	}
	return defaultModel
}

// pickModelOverride returns the model id to use for the image-picker
// LLM call. The per-repo synthesis override does NOT apply to the
// picker (it has its own config: ImagePickerModel). When no override
// is set, fall back to the config's ImagePickerModelOr(Model).
func pickModelOverride(override string, cfg config.SynthesisConfig) string {
	if override != "" {
		return cfg.ImagePickerModelOr(override)
	}
	return cfg.ImagePickerModelOr(cfg.Model)
}