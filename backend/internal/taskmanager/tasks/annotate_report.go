package tasks

import (
	"context"
	"fmt"
	"log"
	"math"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/openktree/open-knowledge-tree/backend/internal/config"
	"github.com/openktree/open-knowledge-tree/backend/internal/dbpool"
	"github.com/openktree/open-knowledge-tree/backend/internal/providers/ai"
	"github.com/openktree/open-knowledge-tree/backend/internal/providers/claims"
	"github.com/openktree/open-knowledge-tree/backend/internal/providers/decomposition"
	"github.com/openktree/open-knowledge-tree/backend/internal/providers/posture"
	"github.com/openktree/open-knowledge-tree/backend/internal/qdrantstore"
	"github.com/openktree/open-knowledge-tree/backend/internal/store"
	"github.com/riverqueue/river"
)

const QueueAnnotateReport = "annotate_report"

// AnnotateReportArgs triggers the autofact-annotation pass for a
// single report. The worker chunks the report's body_md into
// sentences, embeds each with the configured ai.EmbeddingProvider,
// searches the okt_facts Qdrant collection for similar facts above
// reports.similarity_threshold (or the per-repo override), and
// persists the matches into report_annotations so the UI can render
// the autocitation view.
//
// When a posture.Classifier is configured (and the repo hasn't
// disabled it via repository_report_settings.posture_classifier_enabled),
// the worker additionally sends each batch of (sentence, candidate
// fact) pairs to the LLM, labels them related/supports/contradicts/
// irrelevant, and drops the irrelevant ones before persistence. The
// surviving rows carry the posture label so the UI can badge each
// citation. When the classifier is not configured the worker keeps
// all Qdrant hits with posture = NULL (the legacy behavior).
type AnnotateReportArgs struct {
	ReportID     string `json:"report_id"`
	RepositoryID string `json:"repository_id"`
}

func (AnnotateReportArgs) Kind() string { return "annotate_report" }

func (AnnotateReportArgs) InsertOpts() river.InsertOpts { return river.InsertOpts{} }

// AnnotateReportResult is recorded on the job row so the River UI
// shows what the pass did. Sentences is the count of sentences the
// chunker produced (after the min-rune filter); Annotated is the
// count of sentences that had at least one match above threshold;
// Annotations is the total number of (sentence, fact) rows written;
// Dropped is the count of (sentence, fact) pairs the classifier
// judged irrelevant and dropped. PostureEnabled reports whether the
// classifier ran for this pass; PostureModel is the model id used
// (empty when the classifier didn't run).
type AnnotateReportResult struct {
	ReportID      string  `json:"report_id"`
	RepositoryID  string  `json:"repository_id"`
	Sentences     int     `json:"sentences"`
	Annotated     int     `json:"annotated"`
	Annotations   int     `json:"annotations"`
	Dropped       int     `json:"dropped"`
	PostureEnabled bool   `json:"posture_enabled"`
	PostureModel  string  `json:"posture_model"`
	Model         string  `json:"model"`
	Threshold     float64 `json:"threshold"`
}

type AnnotateReportWorker struct {
	river.WorkerDefaults[AnnotateReportArgs]

	embeddingProvider  ai.EmbeddingProvider
	embeddingCfg       config.EmbeddingConfig
	reportsCfg         config.ReportsConfig
	postureClassifier  posture.Classifier
	claimExtractor     claims.Extractor
	qdrant             *qdrantstore.Store
	registry           *dbpool.Registry
	systemQueries      *store.Queries
	modelResolver      *ModelResolver
	promptsetResolver  *PromptsetResolver
}

func NewAnnotateReportWorker(
	embeddingProvider ai.EmbeddingProvider,
	embeddingCfg config.EmbeddingConfig,
	reportsCfg config.ReportsConfig,
	postureClassifier posture.Classifier,
	claimExtractor claims.Extractor,
	qdrant *qdrantstore.Store,
	registry *dbpool.Registry,
	systemQueries *store.Queries,
	modelResolver *ModelResolver,
	promptsetResolver *PromptsetResolver,
) *AnnotateReportWorker {
	return &AnnotateReportWorker{
		embeddingProvider: embeddingProvider,
		embeddingCfg:      embeddingCfg,
		reportsCfg:        reportsCfg,
		postureClassifier: postureClassifier,
		claimExtractor:    claimExtractor,
		qdrant:            qdrant,
		registry:          registry,
		systemQueries:     systemQueries,
		modelResolver:     modelResolver,
		promptsetResolver: promptsetResolver,
	}
}

// Work resolves the per-repo pool, loads the report, chunks its
// body_md into sentences, embeds each sentence, searches Qdrant for
// similar facts above the configured threshold, optionally classifies
// each (sentence, fact) pair with the posture LLM, and persists the
// matches into report_annotations. When the embedding provider or
// Qdrant is not configured (or reports.enabled is false) the worker
// logs and returns nil (a missing provider is a deployment choice,
// not a retryable error — River would otherwise spin forever). The
// report transitions pending → processing → annotated (or failed on
// error so the UI can surface the failure).
func (w *AnnotateReportWorker) Work(ctx context.Context, job *river.Job[AnnotateReportArgs]) error {
	args := job.Args
	if args.RepositoryID == "" || args.ReportID == "" {
		return fmt.Errorf("annotate_report: repository_id and report_id are required")
	}

	if !w.reportsCfg.Enabled || w.embeddingProvider == nil || w.qdrant == nil {
		log.Printf("annotate_report: reports disabled or embedding/qdrant not configured, skipping report %s", args.ReportID)
		return river.RecordOutput(ctx, &AnnotateReportResult{ReportID: args.ReportID, RepositoryID: args.RepositoryID})
	}

	repoID := pgtype.UUID{}
	if err := repoID.Scan(args.RepositoryID); err != nil {
		return fmt.Errorf("annotate_report: invalid repository_id: %w", err)
	}
	reportID := pgtype.UUID{}
	if err := reportID.Scan(args.ReportID); err != nil {
		return fmt.Errorf("annotate_report: invalid report_id: %w", err)
	}

	dbName, err := w.systemQueries.GetRepositoryDatabaseName(ctx, repoID)
	if err != nil {
		return fmt.Errorf("annotate_report: resolving repository database: %w", err)
	}
	pool := w.registry.Get(dbName)
	queries := store.New(pool.Pool)

	report, err := queries.GetReportByID(ctx, reportID)
	if err != nil {
		log.Printf("annotate_report: report %s not found (likely deleted): %v", args.ReportID, err)
		return river.RecordOutput(ctx, &AnnotateReportResult{ReportID: args.ReportID, RepositoryID: args.RepositoryID})
	}

	// Resolve the per-repo similarity threshold + posture flag. The
	// repository_report_settings row is optional; absence = inherit
	// the global config defaults (threshold 0.84, classifier on).
	threshold := w.reportsCfg.SimilarityThresholdOr(0.84)
	lexicalFloor := w.reportsCfg.LexicalSimilarityFloorOr(0.6)
	postureEnabled := w.reportsCfg.PostureClassifier.Enabled
	maxFacts := w.reportsCfg.MaxFactsPerSentenceOr(5)
	ctxBefore := w.reportsCfg.PostureClassifier.ContextWindowBeforeOr(2)
	ctxAfter := w.reportsCfg.PostureClassifier.ContextWindowAfterOr(2)
	if setting, err := w.systemQueries.GetRepositoryReportSettings(ctx, repoID); err == nil {
		if setting.SimilarityThreshold != nil && *setting.SimilarityThreshold > 0 {
			threshold = *setting.SimilarityThreshold
		}
		// A per-repo row overrides the global posture flag. The
		// global flag is the default; the per-repo flag wins when
		// the row exists (so an operator can turn the LLM step off
		// for a single repo without touching the global config).
		postureEnabled = setting.PostureClassifierEnabled
		// Per-repo max_facts_per_sentence override. NULL (or out of
		// [1,50]) inherits the global default. Operators raise this
		// for value-heavy repos where the top-5 semantic hits crowd
		// out exact-numeric-match facts the hybrid lexical fallback
		// would otherwise surface.
		if setting.MaxFactsPerSentence != nil && *setting.MaxFactsPerSentence >= 1 && *setting.MaxFactsPerSentence <= 50 {
			maxFacts = int(*setting.MaxFactsPerSentence)
		}
		// Per-repo lexical_similarity_floor override. NULL (or out
		// of [0,1]) inherits the global default (0.6). This is the
		// semantic-distance gate the hybrid lexical fallback applies
		// to its tsvector hits: a fact the lexical pass surfaced is
		// re-checked against the sentence embedding and dropped if
		// cosine similarity is below this floor, preventing
		// apples-to-oranges matches (e.g. "0.9 kg weight gain"
		// surfacing "0.9 kg CO2 emissions"). 0.0 disables the gate.
		if setting.LexicalSimilarityFloor != nil && *setting.LexicalSimilarityFloor >= 0 && *setting.LexicalSimilarityFloor <= 1 {
			lexicalFloor = *setting.LexicalSimilarityFloor
		}
		// Per-repo context_window_before / context_window_after
		// override. NULL (or out of [0,10]) inherits the global
		// default (2/2). 0 on either side disables context for that
		// side (the worker emits empty ContextBefore/ContextAfter
		// slices for every sentence). The window is clamped to the
		// available sentence range at batch-build time, so a
		// boundary sentence yields fewer than N context entries
		// instead of synthesized padding.
		if setting.ContextWindowBefore != nil && *setting.ContextWindowBefore >= 0 && *setting.ContextWindowBefore <= 10 {
			ctxBefore = int(*setting.ContextWindowBefore)
		}
		if setting.ContextWindowAfter != nil && *setting.ContextWindowAfter >= 0 && *setting.ContextWindowAfter <= 10 {
			ctxAfter = int(*setting.ContextWindowAfter)
		}
	}

	// Resolve per-repo model override for the posture classifier.
	// When the resolver returns a provider, use its ModelID for the
	// classifier call (overriding the global default model). The
	// classifier itself is still the global posture.Classifier
	// instance (or nil); only the model id is overridden here. The
	// classifier's promptset is swapped to the repo's effective
	// philosophy via WithPromptset so the posture phase runs under
	// the same philosophy as the facts it is classifying. The
	// ThinkingLevel from PostureClassifierConfig (default "low")
	// is applied to both the global and per-repo classifier so
	// DeepSeek V4 Flash runs with minimal reasoning effort — the
	// posture task is a tight JSON-emission task where extended
	// reasoning chains waste tokens and increase latency.
	var postureModelOverride string
	postureThinkingLevel := w.reportsCfg.PostureClassifier.ThinkingLevelOr("low")
	postureClassifier := w.postureClassifier
	if w.promptsetResolver != nil {
		ps := w.promptsetResolver.Effective(ctx, repoID)
		w.promptsetResolver.LogEffective(ctx, repoID, "annotate_report")
		if c, ok := postureClassifier.(*posture.AIClassifier); ok {
			postureClassifier = c.WithPromptset(ps).WithThinkingLevel(postureThinkingLevel)
		}
	}
	if w.modelResolver != nil {
		if r := w.modelResolver.Resolve(ctx, repoID, TaskKindReportAnnotation); r.Provider != nil {
			postureModelOverride = r.ModelID
			if w.promptsetResolver != nil {
				ps := w.promptsetResolver.Effective(ctx, repoID)
				postureClassifier = posture.NewAIClassifier(r.Provider, r.ModelID).WithPromptset(ps).WithThinkingLevel(postureThinkingLevel)
			} else {
				postureClassifier = posture.NewAIClassifier(r.Provider, r.ModelID).WithThinkingLevel(postureThinkingLevel)
			}
		}
	}

	minRunes := w.reportsCfg.MinSentenceRunesOr(40)

	jobIDStr := strconv.FormatInt(job.ID, 10)

	if err := queries.MarkReportStatus(ctx, store.MarkReportStatusParams{
		ID:     reportID,
		Status: "processing",
	}); err != nil {
		return fmt.Errorf("annotate_report: marking report processing: %w", err)
	}

	// Chunk the report body into sentences. SegmentSentences is the
	// same deterministic chunker the source pipeline uses, so the
	// sentence_index keys are stable across re-runs.
	sentences := decomposition.SegmentSentences(report.BodyMd)
	candidates := make([]decomposition.Chunk, 0, len(sentences))
	for _, s := range sentences {
		if len([]rune(s.Text)) >= minRunes {
			candidates = append(candidates, s)
		}
	}
	if len(candidates) == 0 {
		if err := queries.ClearReportAnnotations(ctx, reportID); err != nil {
			return fmt.Errorf("annotate_report: clearing annotations: %w", err)
		}
		scnt := int32(len(sentences))
		if err := queries.MarkReportStatus(ctx, store.MarkReportStatusParams{
			ID:            reportID,
			Status:        "annotated",
			SentenceCount: &scnt,
		}); err != nil {
			return fmt.Errorf("annotate_report: marking report annotated: %w", err)
		}
		return river.RecordOutput(ctx, &AnnotateReportResult{
			ReportID: args.ReportID, RepositoryID: args.RepositoryID, Sentences: len(sentences),
		})
	}

	inputs := make([]string, len(candidates))
	for i, s := range candidates {
		inputs[i] = s.Text
	}
	resp, err := w.embeddingProvider.Embed(ctx, pool.Pool, ai.EmbeddingRequest{
		Model:  w.embeddingCfg.Model,
		Inputs: inputs,
		TaskID: ptrString(jobIDStr),
		Attribution: ai.Attribution{
			RepositoryID: args.RepositoryID,
			SourceID:     args.ReportID,
			Operation:    "report_annotation",
		},
	})
	if err != nil {
		return w.failReport(ctx, queries, reportID, fmt.Errorf("embedding %d sentences: %w", len(candidates), err))
	}
	if len(resp.Embeddings) != len(candidates) {
		return w.failReport(ctx, queries, reportID, fmt.Errorf("embedding provider returned %d vectors for %d inputs", len(resp.Embeddings), len(candidates)))
	}
	model := resp.Model
	if model == "" {
		model = w.embeddingCfg.Model
	}

	repoUUID, err := uuid.Parse(args.RepositoryID)
	if err != nil {
		return w.failReport(ctx, queries, reportID, fmt.Errorf("parsing repository_id as uuid: %w", err))
	}

	if err := queries.ClearReportAnnotations(ctx, reportID); err != nil {
		return w.failReport(ctx, queries, reportID, fmt.Errorf("clearing annotations: %w", err))
	}

	// Index the candidate sentence's embedding by sentence_index so
	// the claim-driven retrieval path (which runs per-claim, not
	// per-candidate) can look it up without a linear scan.
	candidateEmbeddingByIndex := make(map[int][]float32, len(candidates))
	for i, c := range candidates {
		candidateEmbeddingByIndex[c.Index] = resp.Embeddings[i]
	}

	// Phase 0: extract direct inline citations from each candidate
	// sentence's text. The synthesis convention is
	//   [text](<fact:uuid>)   (text citation)
	//   ![alt](<fact:uuid>)   (image citation)
	// An author who placed one of these in the report body is
	// explicitly asserting "this fact verifies this sentence" — the
	// worker persists each as an annotation with posture="supports"
	// and score=1.0, OUTSIDE the maxFacts cap and WITHOUT going
	// through the posture classifier (the author's judgment
	// overrides the LLM). Non-existent fact_ids are dropped silently
	// (the author may have referenced a fact that was since
	// deleted).
	directCitations := extractDirectCitations(candidates)
	directCitationIDs := make([]pgtype.UUID, 0)
	for _, ids := range directCitations {
		for _, id := range ids {
			directCitationIDs = append(directCitationIDs, uuidToPg(id))
		}
	}
	directCitationValid := make(map[uuid.UUID]bool, len(directCitationIDs))
	if len(directCitationIDs) > 0 {
		// Batch-validate so we don't issue one query per fact. The
		// result is a set of valid fact_ids; non-valid ids are
		// silently dropped from the directCitations map.
		validRows, vErr := queries.GetFactsByIDs(ctx, directCitationIDs)
		if vErr != nil {
			log.Printf("annotate_report: validating direct citations for report %s failed: %v (will still persist best-effort)", args.ReportID, vErr)
			// Be lenient: if the validation query fails, assume
			// every direct citation is valid so we don't silently
			// drop the author's explicit citations.
			for _, ids := range directCitations {
				for _, id := range ids {
					directCitationValid[id] = true
				}
			}
		} else {
			for _, f := range validRows {
				if f.ID.Valid {
					directCitationValid[f.ID.Bytes] = true
				}
			}
		}
	}
	// Build a per-sentence set of valid direct-citation fact_ids so
	// the retrieval loops below can exclude them from the top-N
	// pool (they're going to be persisted as extras, not as part of
	// the maxFacts budget).
	directCitationSet := make(map[int]map[uuid.UUID]bool, len(directCitations))
	for sidx, ids := range directCitations {
		m := make(map[uuid.UUID]bool, len(ids))
		for _, id := range ids {
			if directCitationValid[id] {
				m[id] = true
			}
		}
		if len(m) > 0 {
			directCitationSet[sidx] = m
		}
	}

	// Phase 1: extract verifiable claims from each candidate sentence.
	// One LLM call per BatchSize sentences; the claims are ephemeral
	// (worker-local, never persisted). When the claim extractor is
	// not configured (or the per-repo model override is unresolvable),
	// sentenceClaims is empty and the worker falls back to
	// embedding-only retrieval for every sentence (the legacy
	// behavior). The claim extractor uses the same model as the
	// posture classifier (DeepSeek V4 Flash by default) but is
	// resolved independently via TaskKindClaimExtraction so an
	// operator can pin a different model per repo.
	var sentenceClaims map[int][]claims.Claim
	claimThinkingLevel := w.reportsCfg.ClaimExtractor.ThinkingLevelOr("low")
	claimExtractor := w.claimExtractor
	// Apply the configured ThinkingLevel to the global extractor
	// (the per-repo override path below applies it to its own
	// instance). "low" is the default because claim extraction is
	// a structural NLP task where extended reasoning chains waste
	// tokens.
	if ce, ok := claimExtractor.(*claims.AIClaimExtractor); ok {
		claimExtractor = ce.WithThinkingLevel(claimThinkingLevel)
	}
	claimModelOverride := ""
	if w.promptsetResolver != nil {
		// Claim extraction is philosophy-neutral (it's a structural
		// NLP pass), so we don't swap the promptset. The LogEffective
		// call is preserved for symmetry with the posture path so
		// the operator's logs show both phases.
		w.promptsetResolver.LogEffective(ctx, repoID, "claim_extraction")
	}
	if w.modelResolver != nil {
		if r := w.modelResolver.Resolve(ctx, repoID, TaskKindClaimExtraction); r.Provider != nil {
			claimModelOverride = r.ModelID
			claimExtractor = claims.NewAIClaimExtractor(r.Provider, r.ModelID).WithThinkingLevel(claimThinkingLevel)
		}
	}
	if claimExtractor != nil && claimExtractor.Configured() {
		sentenceClaims, err = w.extractClaimsForReport(ctx, pool.Pool, candidates, claimExtractor, claimModelOverride, jobIDStr, args)
		if err != nil {
			log.Printf("annotate_report: claim extraction failed for report %s; falling back to embedding-only retrieval: %v", args.ReportID, err)
			sentenceClaims = nil
		}
	}

	// Phase 2: embedding retrieval over every candidate sentence.
	// sentenceHits maps sentence_index -> []qdrantstore.Hit so we
	// can batch-fetch fact text once and join it back. Direct
	// citations are excluded from the top-N pool (they'll be
	// persisted as extras later).
	sentenceHits := make(map[int][]qdrantstore.Hit)
	hitCount := 0
	for i, c := range candidates {
		// Cap maxFacts minus the direct citations already claimed
		// for this sentence, so a sentence with 2 direct citations
		// leaves room for maxFacts-2 auto-retrieved hits (and never
		// exceeds the configured density).
		budget := maxFacts
		if dc, ok := directCitationSet[c.Index]; ok {
			budget -= len(dc)
			if budget < 0 {
				budget = 0
			}
		}
		var hits []qdrantstore.Hit
		if budget > 0 {
			hits, err = w.qdrant.SearchSimilar(ctx, resp.Embeddings[i], repoUUID, uuid.Nil, float32(threshold), budget)
			if err != nil {
				log.Printf("annotate_report: qdrant search for sentence %d of report %s failed: %v", c.Index, args.ReportID, err)
				continue
			}
		}
		// Drop any Qdrant hit whose fact_id is also a direct
		// citation for this sentence (the direct citation wins and
		// is persisted separately as an extra).
		if dc, ok := directCitationSet[c.Index]; ok && len(hits) > 0 {
			filtered := hits[:0]
			for _, h := range hits {
				if dc[h.ID] {
					continue
				}
				filtered = append(filtered, h)
			}
			hits = filtered
		}
		if len(hits) == 0 {
			continue
		}
		sentenceHits[c.Index] = hits
		hitCount += len(hits)
	}

	// Phase 2b: claim-driven retrieval. For each sentence with
	// extracted claims, retrieve additional candidate facts per
	// claim:
	//   - numeric claim  → tsvector search on the claim Term (the
	//     verbatim value + unit, e.g. "0.9 kg", "508 kcal/day"),
	//     reusing the existing SearchFactsByNumericTokens query but
	//     driven by the extracted claim Term instead of the whole
	//     sentence text. This is more precise than the legacy
	//     whole-sentence tsquery because the claim extractor already
	//     picked out the specific value the sentence is asserting.
	//   - prose claim    → embed the claim Term, run a Qdrant
	//     similarity search scoped to the repo, reusing the same
	//     threshold + maxFacts budget. This catches facts whose
	//     phrasing differs from the sentence but matches the claim
	//     term the extractor pulled out (e.g. a "SelfCheckGPT"
	//     claim surfaces a fact about "SelfCheckGPT" even when the
	//     surrounding sentence embedding is dominated by other
	//     prose).
	//
	// Both paths union with the Phase 2 Qdrant hits, dedup by
	// fact_id within a sentence (a fact that hit both ways counts
	// once, keeping the higher score). Direct citations are excluded
	// (already persisted as extras). The apples-to-oranges guard
	// (lexical_similarity_floor) applies to numeric claim hits the
	// same way it applied to the legacy lexical fallback: each
	// tsvector hit is re-checked against the sentence embedding and
	// dropped if cosine similarity is below the floor.
	lexicalHitCount := 0
	lexicalDroppedCount := 0
	proseHitCount := 0
	// Pre-build a per-sentence exclude list (Qdrant hits + direct
	// citations) so the tsvector query doesn't re-surface facts that
	// are already in the candidate pool.
	for _, c := range candidates {
		cs, hasClaims := sentenceClaims[c.Index]
		if !hasClaims || len(cs) == 0 {
			continue
		}
		// Build the exclude set: direct citations + existing Qdrant
		// hits for this sentence.
		excludeSet := make(map[uuid.UUID]bool)
		if dc, ok := directCitationSet[c.Index]; ok {
			for id := range dc {
				excludeSet[id] = true
			}
		}
		for _, h := range sentenceHits[c.Index] {
			excludeSet[h.ID] = true
		}
		excludeIDs := make([]pgtype.UUID, 0, len(excludeSet))
		for id := range excludeSet {
			excludeIDs = append(excludeIDs, uuidToPg(id))
		}
		// Remaining budget for this sentence under maxFacts.
		used := len(sentenceHits[c.Index]) + len(directCitationSet[c.Index])
		budget := maxFacts - used
		if budget < 0 {
			budget = 0
		}
		for _, cl := range cs {
			if budget <= 0 {
				break
			}
			switch cl.Type {
			case claims.ClaimNumeric:
				// tsvector search on the claim Term. Reuses the
				// existing extractNumericTsquery + the lexical-floor
				// gate. The Term is the verbatim value + unit, so
				// the tsquery is tighter than the legacy whole-
				// sentence one.
				tsq := extractNumericTsquery(cl.Term)
				if tsq == "" {
					continue
				}
				lexRows, lerr := queries.SearchFactsByNumericTokens(ctx, store.SearchFactsByNumericTokensParams{
					RepositoryID: repoID,
					Tsquery:      tsq,
					ExcludeIds:   excludeIDs,
					RowLimit:     int32(budget),
				})
				if lerr != nil {
					log.Printf("annotate_report: claim tsvector search for sentence %d of report %s failed: %v", c.Index, args.ReportID, lerr)
					continue
				}
				if len(lexRows) == 0 {
					continue
				}
				// Apples-to-oranges gate. Reuse the sentence
				// embedding from the candidate array.
				sentVec, ok := candidateEmbeddingByIndex[c.Index]
				var factVecs map[uuid.UUID][]float32
				if ok && lexicalFloor > 0 {
					ids := make([]uuid.UUID, 0, len(lexRows))
					for _, f := range lexRows {
						if f.ID.Valid {
							ids = append(ids, f.ID.Bytes)
						}
					}
					fetched, ferr := w.qdrant.GetFactVectorsByIDs(ctx, ids)
					if ferr != nil {
						log.Printf("annotate_report: claim gate vector fetch for sentence %d of report %s failed: %v", c.Index, args.ReportID, ferr)
						continue
					}
					factVecs = make(map[uuid.UUID][]float32, len(fetched))
					for fid, fp := range fetched {
						factVecs[fid] = fp.Vector
					}
				}
				for _, f := range lexRows {
					if !f.ID.Valid {
						continue
					}
					fid := f.ID.Bytes
					if excludeSet[fid] {
						continue
					}
					var score float64
					if lexicalFloor > 0 {
						vec, ok := factVecs[fid]
						if !ok {
							lexicalDroppedCount++
							continue
						}
						cos := cosineSimilarity(sentVec, vec)
						if cos < lexicalFloor {
							lexicalDroppedCount++
							continue
						}
						score = cos
					}
					sentenceHits[c.Index] = append(sentenceHits[c.Index], qdrantstore.Hit{
						ID:    fid,
						Score: float32(score),
					})
					excludeSet[fid] = true
					excludeIDs = append(excludeIDs, uuidToPg(fid))
					lexicalHitCount++
					hitCount++
					budget--
				}
			case claims.ClaimCausal, claims.ClaimComparison, claims.ClaimQuotation, claims.ClaimDefinition, claims.ClaimOther:
				// Prose claim: embed the claim Term, search Qdrant
				// scoped to the repo. Skip when the embedding
				// provider isn't available (defensive — the worker
				// already checked it once, but the claim extractor
				// could run with a different provider).
				if w.embeddingProvider == nil {
					continue
				}
				embedResp, eerr := w.embeddingProvider.Embed(ctx, pool.Pool, ai.EmbeddingRequest{
					Model:  w.embeddingCfg.Model,
					Inputs: []string{cl.Term},
					TaskID: ptrString(jobIDStr),
					Attribution: ai.Attribution{
						RepositoryID: args.RepositoryID,
						SourceID:     args.ReportID,
						Operation:    "claim_embedding",
					},
				})
				if eerr != nil || len(embedResp.Embeddings) == 0 {
					if eerr != nil {
						log.Printf("annotate_report: claim embedding for sentence %d of report %s failed: %v", c.Index, args.ReportID, eerr)
					}
					continue
				}
				claimVec := embedResp.Embeddings[0]
				claimHits, herr := w.qdrant.SearchSimilar(ctx, claimVec, repoUUID, uuid.Nil, float32(threshold), budget)
				if herr != nil {
					log.Printf("annotate_report: claim qdrant search for sentence %d of report %s failed: %v", c.Index, args.ReportID, herr)
					continue
				}
				for _, h := range claimHits {
					if excludeSet[h.ID] {
						continue
					}
					sentenceHits[c.Index] = append(sentenceHits[c.Index], h)
					excludeSet[h.ID] = true
					excludeIDs = append(excludeIDs, uuidToPg(h.ID))
					proseHitCount++
					hitCount++
					budget--
				}
			}
		}
	}
	if lexicalHitCount > 0 || lexicalDroppedCount > 0 || proseHitCount > 0 {
		log.Printf("annotate_report: claim-driven retrieval for report %s — %d numeric hits added, %d numeric dropped by semantic floor %.2f, %d prose hits added",
			args.ReportID, lexicalHitCount, lexicalDroppedCount, lexicalFloor, proseHitCount)
	}

	// Persist direct citations as extras (posture="supports",
	// score=1.0). These bypass the posture classifier (the author's
	// explicit citation overrides the LLM) and the maxFacts cap
	// (they sit alongside the auto-retrieved top-N hits).
	directCitationCount := 0
	for sidx, ids := range directCitations {
		// Find the candidate's text by sentence_index (the
		// chunker's index is the source of truth for both).
		var sentenceText string
		for _, c := range candidates {
			if c.Index == sidx {
				sentenceText = c.Text
				break
			}
		}
		if sentenceText == "" {
			// The direct citation landed on a sentence that wasn't a
			// candidate (e.g. a short heading below min_runes). Skip
			// — we have no candidate sentence_index slot for it.
			// (The chunker splits headings as their own unit, so the
			// fact:uuid would be in that heading's text; persisting
			// it under the heading's sentence_index is the right
			// thing if the heading was a candidate. If it wasn't,
			// the citation is on a too-short unit and we drop it
			// rather than synthesize a candidate slot.)
			continue
		}
		ps := string(posture.Supports)
		for _, id := range ids {
			if !directCitationValid[id] {
				continue
			}
			factID := uuidToPg(id)
			if !factID.Valid {
				continue
			}
			if err := queries.AddReportAnnotation(ctx, store.AddReportAnnotationParams{
				ReportID:      reportID,
				SentenceIndex: int32(sidx),
				SentenceText:  sentenceText,
				FactID:        factID,
				Score:         1.0,
				Posture:       &ps,
			}); err != nil {
				log.Printf("annotate_report: adding direct citation (report %s, sentence %d, fact %s): %v", args.ReportID, sidx, id, err)
				continue
			}
			directCitationCount++
		}
	}

	// Decide whether the posture classifier runs for this report.
	// It runs only when: (a) the global config has it enabled,
	// (b) the per-repo flag (when present) is true, and
	// (c) a classifier instance is wired and configured with a
	// model + provider. Otherwise we fall back to keep-all with
	// posture = NULL.
	classifierActive := postureEnabled && postureClassifier != nil && postureClassifier.Configured()

	// Persist annotations. The two branches share the same insert
	// call; the difference is whether posture is set and whether
	// irrelevant pairs are dropped.
	annotated := 0
	totalAnnotations := 0
	dropped := 0
	postureModel := ""

	if classifierActive && hitCount > 0 {
		postureModel = postureModelOverride
		if postureModel == "" {
			postureModel = w.reportsCfg.PostureClassifier.Model
		}
		// Batch-fetch fact text for every candidate fact across all
		// sentences so the classifier prompt can include it. Build a
		// deduped id list first (a fact may hit for multiple
		// sentences).
		factIDSet := make(map[uuid.UUID]bool)
		for _, hits := range sentenceHits {
			for _, h := range hits {
				factIDSet[h.ID] = true
			}
		}
		factIDs := make([]pgtype.UUID, 0, len(factIDSet))
		for id := range factIDSet {
			factIDs = append(factIDs, uuidToPg(id))
		}
		factRows, err := queries.GetFactsByIDs(ctx, factIDs)
		if err != nil {
			log.Printf("annotate_report: batch-fetching fact text failed; falling back to keep-all for report %s: %v", args.ReportID, err)
			classifierActive = false
		} else {
			// Build a lookup id -> fact text.
			factText := make(map[uuid.UUID]string, len(factRows))
			for _, f := range factRows {
				if f.ID.Valid {
					factText[f.ID.Bytes] = f.Text
				}
			}
		// Build the SentenceFacts batches the classifier
		// consumes (one batch of BatchSize sentences per LLM
		// call).
		//
		// Context window: each batch entry carries up to
		// ctxBefore/ctxAfter surrounding sentences from the FULL
		// sentence array (not just the candidates that passed the
		// min-runes filter). A heading or short list marker that
		// was too short to be a candidate itself is still useful
		// disambiguation context for the longer sentence that
		// follows it, so context is sourced from `sentences`
		// (every chunk) rather than `candidates` (filtered). The
		// window is clamped to the available range — the first
		// sentence yields fewer than ctxBefore context entries
		// (no synthesized padding at the report boundary).
		sentenceByTextIndex := make(map[int]string, len(sentences))
		for _, s := range sentences {
			sentenceByTextIndex[s.Index] = s.Text
		}
		batchSize := w.reportsCfg.PostureClassifier.BatchSizeOr(8)
		var batches []posture.SentenceFacts
		for _, c := range candidates {
			hits, ok := sentenceHits[c.Index]
			if !ok || len(hits) == 0 {
				continue
			}
			sf := posture.SentenceFacts{
				SentenceIndex: c.Index,
				SentenceText: c.Text,
				ContextBefore: contextWindowBefore(sentenceByTextIndex, c.Index, ctxBefore),
				ContextAfter:  contextWindowAfter(sentenceByTextIndex, c.Index, ctxAfter),
				Claims:        sentenceClaims[c.Index],
			}
			for _, h := range hits {
				text, ok := factText[h.ID]
				if !ok {
					continue
				}
				sf.Facts = append(sf.Facts, posture.FactCandidate{ID: h.ID, Text: text})
			}
			if len(sf.Facts) > 0 {
				batches = appendBatches(batches, sf, batchSize)
			}
		}
			// Run batches with bounded concurrency. Each batch
			// produces one LLM call; we cap in-flight calls at
			// MaxConcurrent. A batch failure logs and falls back to
			// keep-all for that batch (posture = NULL) so one flaky
			// LLM call doesn't fail the whole report.
			maxConc := w.reportsCfg.PostureClassifier.MaxConcurrentOr(4)
			maxTokens := w.reportsCfg.PostureClassifier.MaxTokensOr(4000)

			classifications, dropCount, err := w.classifyBatches(ctx, pool.Pool, batches, postureModel, postureModelOverride, maxConc, maxTokens, jobIDStr, args, postureClassifier)
			if err != nil {
				log.Printf("annotate_report: posture classifier failed; falling back to keep-all for report %s: %v", args.ReportID, err)
				classifierActive = false
			} else {
				dropped = dropCount
				// Build a lookup: (sentence_index, fact_id) -> Posture.
				type key struct {
					sidx int
					fid  uuid.UUID
				}
				clsMap := make(map[key]posture.Posture, len(classifications))
				for _, cl := range classifications {
					clsMap[key{cl.SentenceIndex, cl.FactID}] = cl.Posture
				}
				// Persist: only pairs the classifier kept
				// (related/supports/contradicts); irrelevant pairs
				// are absent from clsMap.
				for _, c := range candidates {
					hits, ok := sentenceHits[c.Index]
					if !ok {
						continue
					}
					hadAny := false
					for _, h := range hits {
						k := key{c.Index, h.ID}
						p, keep := clsMap[k]
						if !keep {
							// Not in the classifier output —
							// either irrelevant (dropped) or
							// hallucinated. Skip.
							continue
						}
						factID := uuidToPg(h.ID)
						if !factID.Valid {
							continue
						}
						ps := string(p)
						if err := queries.AddReportAnnotation(ctx, store.AddReportAnnotationParams{
							ReportID:      reportID,
							SentenceIndex: int32(c.Index),
							SentenceText:  c.Text,
							FactID:        factID,
							Score:         float64(h.Score),
							Posture:       &ps,
						}); err != nil {
							log.Printf("annotate_report: adding annotation (report %s, sentence %d, fact %s): %v", args.ReportID, c.Index, h.ID, err)
							continue
						}
						totalAnnotations++
						hadAny = true
					}
					if hadAny {
						annotated++
					}
				}
			}
		}
	}

	if !classifierActive && hitCount > 0 {
		// Fallback: keep all Qdrant hits with posture = NULL.
		for _, c := range candidates {
			hits, ok := sentenceHits[c.Index]
			if !ok || len(hits) == 0 {
				continue
			}
			hadAny := false
			for _, h := range hits {
				factID := uuidToPg(h.ID)
				if !factID.Valid {
					continue
				}
				if err := queries.AddReportAnnotation(ctx, store.AddReportAnnotationParams{
					ReportID:      reportID,
					SentenceIndex: int32(c.Index),
					SentenceText:  c.Text,
					FactID:        factID,
					Score:         float64(h.Score),
					Posture:       nil,
				}); err != nil {
					log.Printf("annotate_report: adding annotation (report %s, sentence %d, fact %s): %v", args.ReportID, c.Index, h.ID, err)
					continue
				}
				totalAnnotations++
				hadAny = true
			}
			if hadAny {
				annotated++
			}
		}
	}

	scnt := int32(len(sentences))
	thr := threshold
	if err := queries.MarkReportStatus(ctx, store.MarkReportStatusParams{
		ID:                  reportID,
		Status:              "annotated",
		SentenceCount:       &scnt,
		EmbeddedModel:       &model,
		SimilarityThreshold: &thr,
	}); err != nil {
		return fmt.Errorf("annotate_report: marking report annotated: %w", err)
	}

	log.Printf("annotate_report: annotated report %s — %d sentences (%d candidates), %d annotated, %d annotations, %d dropped (model %s, threshold %.2f, posture=%v model=%s)",
		args.ReportID, len(sentences), len(candidates), annotated, totalAnnotations, dropped, model, threshold, classifierActive, postureModel)

	return river.RecordOutput(ctx, &AnnotateReportResult{
		ReportID:       args.ReportID,
		RepositoryID:   args.RepositoryID,
		Sentences:      len(sentences),
		Annotated:      annotated,
		Annotations:    totalAnnotations,
		Dropped:        dropped,
		PostureEnabled: classifierActive,
		PostureModel:   postureModel,
		Model:          model,
		Threshold:      threshold,
	})
}

// classifyBatches runs the posture classifier over batches with
// bounded concurrency. It returns the union of all classifications
// (non-irrelevant pairs only — the classifier already drops
// irrelevant ones inside parseClassifications) plus the count of
// dropped (irrelevant) pairs across the whole report.
//
// A batch failure logs and the sentences in that batch fall back to
// keep-all (caller persists their Qdrant hits with posture = NULL).
// This matches the existing per-sentence `continue` on Qdrant
// errors: a flaky upstream doesn't fail the whole report.
func (w *AnnotateReportWorker) classifyBatches(
	ctx context.Context,
	db store.DBTX,
	batches []posture.SentenceFacts,
	modelOverride, _ string, // modelOverride is the resolved per-repo model; _ kept for symmetry
	maxConc, maxTokens int,
	jobIDStr string,
	args AnnotateReportArgs,
	postureClassifier posture.Classifier,
) ([]posture.Classification, int, error) {
	totalPairs := 0
	for _, b := range batches {
		totalPairs += len(b.Facts)
	}

	// semaphore: buffered channel of size maxConc gates the
	// in-flight LLM calls. No new deps.
	sem := make(chan struct{}, maxConc)
	var wg sync.WaitGroup
	var mu sync.Mutex
	var allCls []posture.Classification
	kept := 0
	var firstErr error
	var once sync.Once

	for _, batch := range batches {
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
			return nil, 0, ctx.Err()
		}
		wg.Add(1)
		go func(b posture.SentenceFacts) {
			defer wg.Done()
			defer func() { <-sem }()

			res, err := postureClassifier.Classify(ctx, db, posture.ClassifyRequest{
				Sentences: []posture.SentenceFacts{b},
				Model:     modelOverride,
				MaxTokens: maxTokens,
				TaskID:    jobIDStr,
				Attribution: ai.Attribution{
					RepositoryID: args.RepositoryID,
					SourceID:     args.ReportID,
					Operation:    "report_annotation",
				},
			})
			if err != nil {
				log.Printf("annotate_report: posture classifier batch (sentence %d) failed; keep-all fallback for this batch: %v", b.SentenceIndex, err)
				once.Do(func() { firstErr = err })
				return
			}
			mu.Lock()
			allCls = append(allCls, res...)
			kept += len(res)
			mu.Unlock()
		}(batch)
	}
	wg.Wait()

	dropped := totalPairs - kept
	if dropped < 0 {
		dropped = 0
	}
	// We don't return firstErr: a per-batch failure shouldn't fail
	// the whole pass — the keep-all fallback already handles the
	// affected batch. Log only.
	if firstErr != nil {
		log.Printf("annotate_report: posture classifier had %d batch-level failures; affected sentences fall back to keep-all", countBatchErrs(firstErr))
	}
	return allCls, dropped, nil
}

// countBatchErrs is a no-op helper kept so the log line above
// compiles without tracking per-batch errors separately. Returns 1
// (at least one batch failed) for the single-error case; the caller
// only logs it for visibility.
func countBatchErrs(_ error) int { return 1 }

// appendBatches appends a SentenceFacts to the batches slice,
// starting a new batch when the current one is full (size = batch).
// The first batch is created lazily so an empty candidates list
// produces an empty batches slice.
func appendBatches(batches []posture.SentenceFacts, sf posture.SentenceFacts, batch int) []posture.SentenceFacts {
	if len(batches) == 0 || len(batches[len(batches)-1].Facts) >= batch {
		// We batch by FACT count (not sentence count) so a batch
		// is roughly batch facts (the prompt-size driver). A
		// sentence's facts stay together in one batch.
		return append(batches, sf)
	}
	// Otherwise append this sentence's facts to the current batch.
	last := &batches[len(batches)-1]
	last.Facts = append(last.Facts, sf.Facts...)
	// Preserve the sentence index of the first sentence in the
	// merged batch so the classifier result can be joined back.
	// (The per-fact result carries its own sentence_index, so this
	// is only cosmetic.)
	return batches
}

// failReport marks the report `failed` with the error message and
// returns the error so River retries. The error is stored on the
// report row so the UI can surface it without digging through logs.
func (w *AnnotateReportWorker) failReport(ctx context.Context, queries *store.Queries, reportID pgtype.UUID, cause error) error {
	errMsg := cause.Error()
	if qerr := queries.MarkReportStatus(ctx, store.MarkReportStatusParams{
		ID:     reportID,
		Status: "failed",
		Error:  &errMsg,
	}); qerr != nil {
		log.Printf("annotate_report: marking report %s failed (cause=%v) failed: %v", reportID, cause, qerr)
	}
	time.Sleep(2 * time.Second)
	return cause
}

// numericTokenPattern matches bare numbers and numbers with common
// scientific units in report sentences. Used by the hybrid retrieval
// fallback to extract the tokens whose exact presence in a fact's
// text is strong evidence the fact supports/relates to the sentence
// (e.g. "508 kcal", "0.9 kg", "22.3%", "OR 7.61", "κ = 0.32", "p =
// 0.04"). The pattern is deliberately wide: it captures any number
// (with optional decimal) and the unit word that may follow, then
// also captures standalone numbers so a fact quoting the same number
// in a different unit still matches.
var numericTokenPattern = regexp.MustCompile(
	`(?:\d+(?:[.,]\d+)?%?)` + // a number, optional decimal, optional %
		`(?:\s*(?:kg|kcal|g|mg|ml|l|mmol|µg|ug|ng|lb|in|cm|mm|m|km|h|min|s|ms|kpa|pa)?)`)

// unitAliases maps short unit tokens to their long-form equivalents
// (and vice versa) so the lexical fallback bridges unit spelling
// variants the english tsvector config stems differently. The english
// config stems "kilograms" → "kilogram" but leaves "kg" alone, so a
// sentence quoting "0.9 kg" and a fact stating "0.9 kilograms"
// wouldn't match on the unit token without this normalization. We
// expand each short unit token to its long form(s) and OR them
// together in the tsquery so either spelling matches. The mapping is
// conservative — only units common in scientific/medical prose, and
// only forms the english stemmer treats differently (e.g. "kcal" and
// "calories" both index verbatim as "kcal"/"calori", so they need
// bridging too).
var unitAliases = map[string][]string{
	"kg":       {"kilogram", "kilograms"},
	"g":        {"gram", "grams"},
	"mg":       {"milligram", "milligrams"},
	"µg":       {"microgram", "micrograms"},
	"ug":       {"microgram", "micrograms"},
	"ng":       {"nanogram", "nanograms"},
	"ml":       {"milliliter", "milliliters", "millilitre", "millilitres"},
	"l":        {"liter", "liters", "litre", "litres"},
	"kcal":     {"calorie", "calories", "kilocalorie", "kilocalories"},
	"mmol":     {"millimole", "millimoles"},
	"cm":       {"centimeter", "centimeters", "centimetre", "centimetres"},
	"mm":       {"millimeter", "millimeters", "millimetre", "millimetres"},
	"km":       {"kilometer", "kilometers", "kilometre", "kilometres"},
	"h":        {"hour", "hours"},
	"min":      {"minute", "minutes"},
	"s":        {"second", "seconds"},
	"ms":       {"millisecond", "milliseconds"},
	"lb":       {"pound", "pounds"},
	"in":       {"inch", "inches"},
	"pa":       {"pascal", "pascals"},
	"kpa":      {"kilopascal", "kilopascals"},
}

// extractNumericTsquery extracts the numeric/unit tokens from a
// sentence and joins them into a Postgres tsquery string with ` & `
// (AND semantics) so the lexical fallback returns only facts that
// share every token. Returns "" when the sentence has fewer than one
// numeric token (no lexical fallback to run). Tokens are lowercased,
// stripped of trailing punctuation, and quoted with double-quotes so
// tsquery treats them as plain phrases — this is critical because
// numeric tokens like "0.9" contain a `.` which is a tsquery
// weight/position separator when unquoted, producing a syntax error.
// Duplicates are removed.
//
// Unit tokens are expanded with their aliases (see unitAliases) and
// OR'd together so "0.9 kg" matches a fact that says "0.9 kilograms"
// — the english tsvector config stems "kilograms" → "kilogram" but
// leaves "kg" alone, so without this expansion the unit token would
// fail to match across spellings even though the number itself
// matches. A pure-number token (no unit) emits as a single quoted
// token; a number+unit pair emits as `(num & (unit | alias1 | ...))`
// so the fact must contain the number AND one of the unit spellings.
//
// Example: "The trial produced 0.9 kg weight gain"
// -> '"0.9" & ( "kg" | "kilogram" | "kilograms" )'
//
// The `plainto_tsquery` helper would also work and is safer, but it
// ORs tokens together (not ANDs), which would over-match for short
// numeric tokens that appear in many facts. We want AND on the
// number, OR on the unit aliases.
func extractNumericTsquery(sentence string) string {
	matches := numericTokenPattern.FindAllString(sentence, -1)
	if len(matches) == 0 {
		return ""
	}
	seen := make(map[string]bool, len(matches))
	var tokens []string
	for _, m := range matches {
		t := strings.ToLower(strings.TrimSpace(m))
		if t == "" || seen[t] {
			continue
		}
		seen[t] = true
		// Split the match into a leading numeric part and a trailing
		// unit part. The regex captures them together; we split on
		// the first whitespace. If there's no unit, emit the number
		// alone. If there's a unit, expand it via unitAliases.
		num, unit, hasUnit := splitNumberUnit(t)
		escapedNum := strings.ReplaceAll(num, `"`, `""`)
		if !hasUnit || unit == "" {
			tokens = append(tokens, `"`+escapedNum+`"`)
			continue
		}
		aliases, ok := unitAliases[unit]
		if !ok {
			// Unknown unit — emit number AND the literal unit token.
			escapedUnit := strings.ReplaceAll(unit, `"`, `""`)
			tokens = append(tokens, `"`+escapedNum+`" & "`+escapedUnit+`"`)
			continue
		}
		// Build ( "unit" | "alias1" | "alias2" | ... )
		var unitAlternatives []string
		unitAlternatives = append(unitAlternatives, `"`+strings.ReplaceAll(unit, `"`, `""`)+`"`)
		for _, a := range aliases {
			unitAlternatives = append(unitAlternatives, `"`+strings.ReplaceAll(a, `"`, `""`)+`"`)
		}
		unitGroup := "( " + strings.Join(unitAlternatives, " | ") + " )"
		tokens = append(tokens, `"`+escapedNum+`" & `+unitGroup)
	}
	if len(tokens) == 0 {
		return ""
	}
	return strings.Join(tokens, " & ")
}

// splitNumberUnit splits a numeric-token match like "0.9 kg" or
// "508kcal" into its leading numeric part and trailing unit part.
// Returns (number, unit, hasUnit). hasUnit is false when the match
// was a bare number with no unit word. The unit is returned without
// surrounding whitespace.
func splitNumberUnit(token string) (number, unit string, hasUnit bool) {
	// Find the first whitespace in the token. The regex produces
	// matches where the unit (if any) is separated from the number
	// by whitespace ("0.9 kg"), or the token is a bare number
	// ("0.9"), or the token is a number with a trailing % ("22.3%").
	idx := strings.IndexAny(token, " \t")
	if idx < 0 {
		return token, "", false
	}
	return token[:idx], strings.TrimSpace(token[idx:]), true
}

// cosineSimilarity computes the cosine similarity between two
// equal-length float32 vectors. Returns 0 when either vector is empty
// or zero, or when lengths differ (defensive — should never happen
// because the embedding provider guarantees a fixed dimension).
// Used by the hybrid lexical retrieval gate to filter tsvector hits
// whose only overlap with the sentence is a bare numeric token.
func cosineSimilarity(a, b []float32) float64 {
	if len(a) == 0 || len(b) == 0 || len(a) != len(b) {
		return 0
	}
	var dot, na, nb float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		na += float64(a[i]) * float64(a[i])
		nb += float64(b[i]) * float64(b[i])
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}

// contextWindowBefore returns up to `n` sentences immediately before
// the candidate at sentenceIndex, ordered oldest → newest (so the
// classifier reads them in narrative order). The window is clamped
// to the available range — indices < 0 are skipped (no synthesized
// padding at the report boundary). Returns an empty slice when n is
// 0 or the candidate is the first sentence. Sourced from the FULL
// sentence array (every chunk, including short ones the min-runes
// filter dropped from the candidate set) so a heading or list
// marker can still disambiguate the longer sentence that follows.
func contextWindowBefore(sentences map[int]string, sentenceIndex, n int) []string {
	if n <= 0 {
		return nil
	}
	out := make([]string, 0, n)
	for i := sentenceIndex - n; i < sentenceIndex; i++ {
		if i < 0 {
			continue
		}
		if t, ok := sentences[i]; ok {
			out = append(out, t)
		}
	}
	return out
}

// contextWindowAfter returns up to `n` sentences immediately after
// the candidate at sentenceIndex, in narrative order. The window is
// clamped to the available range — indices past the last sentence
// are skipped. Returns an empty slice when n is 0 or the candidate
// is the last sentence. Sourced from the FULL sentence array so a
// short heading following the candidate can still serve as context.
func contextWindowAfter(sentences map[int]string, sentenceIndex, n int) []string {
	if n <= 0 {
		return nil
	}
	out := make([]string, 0, n)
	for i := sentenceIndex + 1; i <= sentenceIndex+n; i++ {
		if t, ok := sentences[i]; ok {
			out = append(out, t)
		}
	}
	return out
}

// directCitationPattern matches the synthesis convention for
// inline fact citations:
//   [text](<fact:uuid>)   (text citation)
//   ![alt](<fact:uuid>)   (image citation)
// The link target is the literal string "fact:" followed by a
// canonical UUID. The angle brackets are part of the markdown link
// target delimiters (the synthesizer emits them so a UUID
// containing no special markdown chars still parses as one link
// target). The pattern is permissive about the surrounding
// markdown link syntax so it catches both the canonical form and
// minor variants (no angle brackets, extra whitespace); the UUID
// is the load-bearing part.
var directCitationPattern = regexp.MustCompile(`fact:([0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12})`)

// extractDirectCitations scans each candidate sentence's text for
// inline fact:uuid citations and returns a map sentence_index ->
// []fact_id. A sentence may carry multiple direct citations (the
// synthesis convention allows several [text](<fact:uuid>) links in
// one sentence). Duplicate fact_ids within a sentence are deduped
// (the same fact cited twice in one sentence is one annotation).
// The ids are returned in the order they appear in the text so the
// persistence step writes them in a stable, reader-facing order.
//
// The caller is responsible for validating that each id refers to
// an existing fact in the repository (the author may have
// referenced a fact that was since deleted); invalid ids are
// dropped at persistence time.
func extractDirectCitations(candidates []decomposition.Chunk) map[int][]uuid.UUID {
	out := make(map[int][]uuid.UUID)
	for _, c := range candidates {
		matches := directCitationPattern.FindAllStringSubmatch(c.Text, -1)
		if len(matches) == 0 {
			continue
		}
		seen := make(map[uuid.UUID]bool, len(matches))
		var ids []uuid.UUID
		for _, m := range matches {
			id, err := uuid.Parse(m[1])
			if err != nil {
				continue
			}
			if seen[id] {
				continue
			}
			seen[id] = true
			ids = append(ids, id)
		}
		if len(ids) > 0 {
			out[c.Index] = ids
		}
	}
	return out
}

// extractClaimsForReport runs the claim extractor over the report's
// candidate sentences in batches of BatchSize, with bounded
// concurrency. Returns a map sentence_index -> []Claim (sentences
// with no claims are absent from the map). A batch failure logs and
// the sentences in that batch get no claims (the worker falls back
// to embedding-only retrieval for them); the report still annotates.
// Mirrors the classifyBatches concurrency pattern.
func (w *AnnotateReportWorker) extractClaimsForReport(
	ctx context.Context,
	db store.DBTX,
	candidates []decomposition.Chunk,
	extractor claims.Extractor,
	modelOverride string,
	jobIDStr string,
	args AnnotateReportArgs,
) (map[int][]claims.Claim, error) {
	batchSize := w.reportsCfg.ClaimExtractor.BatchSizeOr(8)
	// Build batches of sentences. Each batch is a slice of
	// SentenceInput. Unlike the posture batches (which are keyed
	// by fact count), claim batches are keyed by sentence count
	// (one LLM call per BatchSize sentences).
	var batches [][]claims.SentenceInput
	cur := make([]claims.SentenceInput, 0, batchSize)
	for _, c := range candidates {
		cur = append(cur, claims.SentenceInput{Index: c.Index, Text: c.Text})
		if len(cur) >= batchSize {
			batches = append(batches, cur)
			cur = make([]claims.SentenceInput, 0, batchSize)
		}
	}
	if len(cur) > 0 {
		batches = append(batches, cur)
	}

	maxConc := w.reportsCfg.ClaimExtractor.MaxConcurrentOr(6)
	maxTokens := w.reportsCfg.ClaimExtractor.MaxTokensOr(8000)

	sem := make(chan struct{}, maxConc)
	var wg sync.WaitGroup
	var mu sync.Mutex
	out := make(map[int][]claims.Claim)
	var firstErr error
	var once sync.Once

	for _, batch := range batches {
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		wg.Add(1)
		go func(b []claims.SentenceInput) {
			defer wg.Done()
			defer func() { <-sem }()

			res, err := extractor.Extract(ctx, db, claims.ExtractRequest{
				Sentences: b,
				Model:     modelOverride,
				MaxTokens: maxTokens,
				TaskID:    jobIDStr,
				Attribution: ai.Attribution{
					RepositoryID: args.RepositoryID,
					SourceID:     args.ReportID,
					Operation:    "claim_extraction",
				},
			})
			if err != nil {
				log.Printf("annotate_report: claim extraction batch failed (sentences %d-%d); falling back to embedding-only for these: %v",
					b[0].Index, b[len(b)-1].Index, err)
				once.Do(func() { firstErr = err })
				return
			}
			mu.Lock()
			for _, sc := range res {
				out[sc.SentenceIndex] = sc.Claims
			}
			mu.Unlock()
		}(batch)
	}
	wg.Wait()

	if firstErr != nil {
		log.Printf("annotate_report: claim extraction had %d batch-level failures; affected sentences fall back to embedding-only", countBatchErrs(firstErr))
	}
	return out, nil
}