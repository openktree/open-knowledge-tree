package tasks

import (
	"context"
	"fmt"
	"io"
	"log"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/openktree/open-knowledge-tree/backend/internal/config"
	"github.com/openktree/open-knowledge-tree/backend/internal/dbpool"
	"github.com/openktree/open-knowledge-tree/backend/internal/promptset"
	"github.com/openktree/open-knowledge-tree/backend/internal/providers/decomposition"
	"github.com/openktree/open-knowledge-tree/backend/internal/providers/storage"
	"github.com/openktree/open-knowledge-tree/backend/internal/store"
	"github.com/riverqueue/river"
)

const QueueSourceDecomposition = "source_decomposition"

type SourceDecompositionArgs struct {
	SourceID     string `json:"source_id"`
	RepositoryID string `json:"repository_id"`
}

func (SourceDecompositionArgs) Kind() string { return "source_decomposition" }

func (SourceDecompositionArgs) InsertOpts() river.InsertOpts { return river.InsertOpts{} }

type SourceDecompositionResult struct {
	SourceID      string       `json:"source_id"`
	Chunks        int          `json:"chunks"`
	Facts         int          `json:"facts"`
	Images        int          `json:"images"`
	ChunkFailures int          `json:"chunk_failures"`
	ImageFailures int          `json:"image_failures"`
	ChunkTraces   []ChunkTrace `json:"chunk_traces,omitempty"`
	ImageTraces   []ImageTrace `json:"image_traces,omitempty"`
	Processed     bool         `json:"processed"`
}

// ChunkTrace is one text chunk's per-chunk troubleshooting record,
// captured during the text-chunk extraction loop and surfaced in the
// job's recorded output so the detailed task view can show which
// chunk was slow or failed. DurationMs is the wall time of the
// extract+persist step for this chunk; Facts is the number of fact
// rows persisted from this chunk; Error is the non-empty extract
// error string when the chunk failed (mutually exclusive with a
// Facts count > 0, but a chunk can produce 0 facts without erroring).
type ChunkTrace struct {
	Type       string `json:"type"`
	Index      int    `json:"index"`
	DurationMs int64  `json:"duration_ms"`
	Facts      int    `json:"facts"`
	Error      string `json:"error,omitempty"`
}

// ImageTrace is the multimodal analogue of ChunkTrace, one entry
// per image attached to the source. Skipped is set (and Error left
// empty) when the image was deliberately skipped before reaching the
// model — e.g. a page render with no storage_key, or a zero-byte
// fetch. Error is set when the extractor was called and returned an
// error. PageNumber is set for PDF page renders; ImageURL for inline
// images.
type ImageTrace struct {
	Type       string `json:"type"`
	Index      int    `json:"index"`
	ImageURL   string `json:"image_url,omitempty"`
	PageNumber *int   `json:"page_number,omitempty"`
	DurationMs int64  `json:"duration_ms"`
	Facts      int    `json:"facts"`
	Error      string `json:"error,omitempty"`
	Skipped    string `json:"skipped,omitempty"`
}

type SourceDecompositionWorker struct {
	river.WorkerDefaults[SourceDecompositionArgs]

	chunkingProvider  decomposition.ChunkingProvider
	factExtractor     decomposition.FactExtractionProvider
	imageExtractor    decomposition.ImageFactExtractionProvider
	factCfg           config.DecompositionFactConfig
	imageCfg          config.DecompositionImageConfig
	registry          *dbpool.Registry
	systemQueries     *store.Queries
	storage           storage.FileStorage
	modelResolver     *ModelResolver
	promptsetResolver *PromptsetResolver
}

func NewSourceDecompositionWorker(
	chunkingProvider decomposition.ChunkingProvider,
	factExtractor decomposition.FactExtractionProvider,
	imageExtractor decomposition.ImageFactExtractionProvider,
	factCfg config.DecompositionFactConfig,
	imageCfg config.DecompositionImageConfig,
	registry *dbpool.Registry,
	systemQueries *store.Queries,
	stor storage.FileStorage,
	modelResolver *ModelResolver,
	promptsetResolver *PromptsetResolver,
) *SourceDecompositionWorker {
	return &SourceDecompositionWorker{
		chunkingProvider:  chunkingProvider,
		factExtractor:     factExtractor,
		imageExtractor:    imageExtractor,
		factCfg:           factCfg,
		imageCfg:          imageCfg,
		registry:          registry,
		systemQueries:     systemQueries,
		storage:           stor,
		modelResolver:     modelResolver,
		promptsetResolver: promptsetResolver,
	}
}

func (w *SourceDecompositionWorker) Work(ctx context.Context, job *river.Job[SourceDecompositionArgs]) error {
	args := job.Args

	if args.SourceID == "" || args.RepositoryID == "" {
		return fmt.Errorf("source_decomposition: source_id and repository_id are required")
	}

	repoID := pgtype.UUID{}
	if err := repoID.Scan(args.RepositoryID); err != nil {
		return fmt.Errorf("source_decomposition: invalid repository_id: %w", err)
	}

	sourceID := pgtype.UUID{}
	if err := sourceID.Scan(args.SourceID); err != nil {
		return fmt.Errorf("source_decomposition: invalid source_id: %w", err)
	}

	dbName, err := w.systemQueries.GetRepositoryDatabaseName(ctx, repoID)
	if err != nil {
		return fmt.Errorf("source_decomposition: resolving repository database: %w", err)
	}

	pool := w.registry.Get(dbName)
	queries := store.New(pool.Pool)

	// Resolve the repo's effective promptset (philosophy) once at
	// Work() start. The hash tags every fact/reference this job
	// persists so downstream queries (synthesis, registry pull) can
	// filter to a single promptset and decompositions from different
	// promptsets do not mix. The resolved Promptset is threaded into
	// the fact + image extractors via WithPromptset so they run the
	// repo's philosophy, not the built-in default.
	var ps promptset.Promptset
	var psHash string
	if w.promptsetResolver != nil {
		ps = w.promptsetResolver.Effective(ctx, repoID)
		psHash = ps.Hash
		w.promptsetResolver.LogEffective(ctx, repoID, "source_decomposition")
	} else {
		ps = promptset.Default
		psHash = promptset.DefaultHash
	}
	psHashPtr := &psHash

	// Resolve per-repo model overrides for fact + image extraction.
	// When the resolver returns a non-nil provider, build a fresh
	// thin wrapper with the per-repo model; otherwise use the
	// default (baked-in) instance. The wrapper inherits the resolved
	// promptset via WithPromptset so a per-repo model swap does not
	// reset the philosophy.
	factExtractor := w.factExtractor
	if w.modelResolver != nil {
		if r := w.modelResolver.Resolve(ctx, repoID, TaskKindFactExtraction); r.Provider != nil {
			factExtractor = decomposition.NewAIFactExtractionProvider(r.Provider, r.ModelID).WithPromptset(ps)
		}
	}
	if fe, ok := factExtractor.(*decomposition.AIFactExtractionProvider); ok {
		factExtractor = fe.WithPromptset(ps)
	}
	imageExtractor := w.imageExtractor
	if w.modelResolver != nil && imageExtractor != nil {
		if r := w.modelResolver.Resolve(ctx, repoID, TaskKindImageExtraction); r.Provider != nil {
			// Copy the ImageFetcher + MaxImageBytes from the default
			// instance so the fresh wrapper has the same fetch config.
			if def, ok := imageExtractor.(*decomposition.AIImageFactExtractionProvider); ok {
				imageExtractor = decomposition.NewAIImageFactExtractionProvider(
					r.Provider, r.ModelID, def.ImageFetcher, def.MaxImageBytes,
				).WithPromptset(ps)
			}
		}
	}
	if ie, ok := imageExtractor.(*decomposition.AIImageFactExtractionProvider); ok {
		imageExtractor = ie.WithPromptset(ps)
	}

	source, err := queries.GetSourceByID(ctx, sourceID)
	if err != nil {
		return fmt.Errorf("source_decomposition: fetching source: %w", err)
	}

	// Resolve the repository slug once. Image facts whose source
	// image has no remote URL (PDF page renders stored under
	// storage_key) need a service-routable URL written to
	// facts.image_url so the frontend can render the thumbnail.
	// That URL is /api/v1/repositories/{slug}/sources/{sourceID}/
	// images/{imageID} and requires the slug, not the UUID. A
	// lookup failure here is non-fatal: image facts will fall
	// back to image_url=NULL and render text-only, matching the
	// historical behavior.
	repoSlug, slugErr := w.systemQueries.GetRepositorySlug(ctx, repoID)
	if slugErr != nil {
		log.Printf("source_decomposition: resolving repository slug for image URLs (source %s): %v", args.SourceID, slugErr)
	}

	// Image-only sources (e.g. scanned PDFs with no text layer, or
	// HTML pages whose parser returned no body) have no text to feed
	// the chunker, but they may still have page renders / inline
	// images worth extracting image facts from. When image extraction
	// is enabled we therefore skip the text-chunk loop but fall
	// through to the image loop below; when image extraction is also
	// unavailable there is nothing to do and we record a no-op result.
	sourceHasText := source.ParsedText != nil && *source.ParsedText != ""
	if !sourceHasText {
		imageExtractionPossible := imageExtractor != nil && w.imageCfg.Enabled
		if !imageExtractionPossible {
			log.Printf("source_decomposition: source %s has no parsed text and image extraction is not configured; skipping", args.SourceID)
			return river.RecordOutput(ctx, &SourceDecompositionResult{
				SourceID:  args.SourceID,
				Chunks:    0,
				Facts:     0,
				Processed: false,
			})
		}
		log.Printf("source_decomposition: source %s has no parsed text; skipping text-chunk loop and running image extraction only", args.SourceID)
	}

	if sourceHasText && factExtractor == nil {
		log.Printf("source_decomposition: fact extraction provider not configured, marking source %s as processed with 0 facts", args.SourceID)
		if _, err := queries.MarkSourceProcessed(context.Background(), sourceID); err != nil {
			log.Printf("source_decomposition: marking source processed failed: %v", err)
		}
		return river.RecordOutput(ctx, &SourceDecompositionResult{
			SourceID:  args.SourceID,
			Chunks:    0,
			Facts:     0,
			Processed: true,
		})
	}

	// Prefer the Markdown rendering of the cleaned content over
	// the plain text. Markdown carries the inline structure
	// (headings, bold/italic, lists, blockquotes, code, links)
	// that helps the fact extractor understand the document,
	// in a compact form that the model can parse more cheaply than
	// raw HTML. Legacy rows (pre-migration) and PDF sources
	// without inline structure fall back to parsed_text, which
	// is always populated when the parse_status gate above
	// passed.
	chunks := []decomposition.Chunk{}
	// sentences is the deterministic global sentence array the
	// retrieve_source worker persisted at parse time. It is the
	// stable contract that fact_references.sentence_index keys
	// into — independent of the (interchangeable) chunker config.
	// When absent (legacy rows predating migration 0022, or a
	// source with no parseable text), sentence labeling is
	// skipped: facts are still extracted and linked to the source
	// via fact_sources, but no fact_references rows are written.
	var sentences []decomposition.Sentence
	var sourceText string
	if sourceHasText {
		sourceText = *source.ParsedText
		if source.ParsedMarkdown != nil && *source.ParsedMarkdown != "" {
			sourceText = *source.ParsedMarkdown
		}
		chunks = w.chunkingProvider.Chunk(sourceText)
		sentences = sentencesFromOffsets(source.SentenceOffsets)
		log.Printf("source_decomposition: source %s split into %d chunks (%d sentences)",
			args.SourceID, len(chunks), len(sentences))
	}

	totalFacts := 0
	chunkFailures := 0
	chunkTraces := make([]ChunkTrace, 0, len(chunks))

	// ---- Phase 1: parallel AI extraction (no DB writes) ----
	// Fan out ExtractFacts over chunks with bounded concurrency so the
	// LLM-bound part parallelizes. The fact extractor's internal
	// retryWithBackoff handles 429/5xx/net retries per call. Each worker
	// builds its own chunkPromptText (pure string op, no shared state) and
	// returns its facts; persistence is deferred to phase 2 so the DB sees
	// the same serial write pattern as the old loop.
	type chunkExtract struct {
		facts []decomposition.ExtractedFact
	}
	concurrency := w.factCfg.ConcurrencyOr(4)
	chunkResults, chunkErrs := decomposition.ExtractParallel(ctx, concurrency, chunks, func(callCtx context.Context, chunk decomposition.Chunk) (chunkExtract, error) {
		chunkPromptText := chunk.Text
		if len(sentences) > 0 {
			chunkPromptText = buildLabeledChunkText(sentences, sourceText, chunk.StartRune, chunk.EndRune)
		}
		facts, err := factExtractor.ExtractFacts(callCtx, pool.Pool, chunkPromptText, decomposition.FactExtractionAttribution{
			RepositoryID: args.RepositoryID,
			SourceID:     args.SourceID,
			TaskID:       strconv.FormatInt(job.ID, 10),
		})
		if err != nil {
			return chunkExtract{}, err
		}
		return chunkExtract{facts: facts}, nil
	})

	// ---- Phase 2: serial persistence (single goroutine) ----
	// Iterate the ordered results and persist exactly as the old inline
	// loop did. One connection, sequential short operations; the DB
	// connection pressure is unchanged from the serial baseline.
	for i, chunk := range chunks {
		start := time.Now()
		trace := ChunkTrace{Type: "text", Index: chunk.Index}
		if err := chunkErrs[i]; err != nil {
			log.Printf("source_decomposition: chunk %d extraction failed: %v", chunk.Index, err)
			chunkFailures++
			trace.Error = err.Error()
			trace.DurationMs = time.Since(start).Milliseconds()
			chunkTraces = append(chunkTraces, trace)
			continue
		}

		facts := chunkResults[i].facts
		for _, ef := range facts {
			factID := pgtype.UUID{}
			if err := factID.Scan(uuid.New().String()); err != nil {
				log.Printf("source_decomposition: generating fact id failed: %v", err)
				continue
			}
			created, err := queries.CreateFact(ctx, store.CreateFactParams{
				ID:            factID,
				Text:          ef.Text,
				FactKind:      "text",
				PromptsetHash: psHashPtr,
			})
			if err != nil {
				log.Printf("source_decomposition: persisting fact failed: %v", err)
				continue
			}
			// The fact-source link is a junction row. The same
			// fact from a future re-process of this source (or
			// from a dedup merge of another fact) is idempotent
			// via AddFactSource's ON CONFLICT clause.
			if err := queries.AddFactSource(ctx, store.AddFactSourceParams{
				FactID:     created.ID,
				SourceID:   sourceID,
				ChunkIndex: int32(chunk.Index),
			}); err != nil {
				log.Printf("source_decomposition: linking fact to source failed: %v", err)
				continue
			}
			// Sentence-level provenance: one fact_references row
			// per cited global sentence index. Hallucinated
			// indices (out of range) are silently dropped — the
			// fact is still kept, just without a bad reference.
			for _, sIdx := range ef.Sentences {
				if sIdx < 0 || sIdx >= len(sentences) {
					continue
				}
				if err := queries.AddFactReference(ctx, store.AddFactReferenceParams{
					FactID:        created.ID,
					SourceID:      sourceID,
					SentenceIndex: int32(sIdx),
					ChunkIndex:    int32(chunk.Index),
					PromptsetHash: psHashPtr,
				}); err != nil {
					log.Printf("source_decomposition: linking fact reference failed: %v", err)
					continue
				}
			}
			totalFacts++
		}
		trace.Facts = len(facts)
		trace.DurationMs = time.Since(start).Milliseconds()
		chunkTraces = append(chunkTraces, trace)
	}

	log.Printf("source_decomposition: source %s extracted %d facts from %d chunks", args.SourceID, totalFacts, len(chunks))

	// Image fact extraction. Runs after the text-chunk loop in the
	// same job so a single embed_facts chain covers both text and
	// image facts. Skipped when the image extractor is not
	// configured (nil) or disabled in config — the source still
	// produces text facts and is marked processed. Per-image
	// failures (fetch error, model error) are logged and the image
	// is skipped, mirroring the per-chunk text error tolerance.
	imageCount := 0
	imageFailures := 0
	imagesAttempted := 0
	imageTraces := []ImageTrace{}
	if imageExtractor != nil && w.imageCfg.Enabled {
		imageCount, imageFailures, imagesAttempted = w.extractImageFacts(ctx, queries, pool.Pool, source, sourceHasText, repoSlug, args, job, &totalFacts, &imageTraces, imageExtractor, psHashPtr)
	} else if w.imageCfg.Enabled && imageExtractor == nil {
		log.Printf("source_decomposition: image extraction enabled but provider not configured; skipping images for source %s", args.SourceID)
	}

	if _, err := queries.MarkSourceProcessed(context.Background(), sourceID); err != nil {
		return fmt.Errorf("source_decomposition: marking source processed: %w", err)
	}

	// Chain to embed_facts via river.ClientFromContext so the
	// worker stays decoupled from the Manager (no import cycle,
	// no constructor churn for the existing decomposition
	// worker). The embedding job will vectorize the new facts
	// into Qdrant and then chain to deduplicate_facts. A missing
	// client (tests that drive the worker directly without a
	// River client on the context) is logged, not fatal — the
	// facts are persisted and will be picked up by the next
	// embed_facts enqueue (e.g. from another source, or a
	// future catchup path).
	//
	// The chained insert uses a fresh context rather than the
	// worker's ctx: River cancels the worker ctx as the job
	// completes, and the chained insert opens its own
	// transaction internally, so reusing the worker ctx races
	// the cancellation ("context deadline exceeded" on the
	// BEGIN). The chained job is independent of the parent's
	// lifecycle, so a background ctx with a short timeout is
	// correct.
	if totalFacts > 0 {
		if client := river.ClientFromContext[pgx.Tx](ctx); client != nil {
			chainCtx, chainCancel := context.WithTimeout(context.Background(), 15*time.Second)
			if _, err := client.Insert(chainCtx, EmbedFactsArgs{
				SourceID:     args.SourceID,
				RepositoryID: args.RepositoryID,
			}, &river.InsertOpts{
				Queue: QueueEmbedFacts,
				Metadata: MarshalMetadata(JobMetadata{
					RepositoryID: args.RepositoryID,
					SourceID:     args.SourceID,
				}),
			}); err != nil {
				log.Printf("source_decomposition: enqueueing embed_facts for repo %s: %v", args.RepositoryID, err)
			}
			chainCancel()
		} else {
			log.Printf("source_decomposition: no river client on context; embed_facts not enqueued for source %s (facts are persisted)", args.SourceID)
		}
	}

	result := &SourceDecompositionResult{
		SourceID:      args.SourceID,
		Chunks:        len(chunks),
		Facts:         totalFacts,
		Images:        imageCount,
		ChunkFailures: chunkFailures,
		ImageFailures: imageFailures,
		ChunkTraces:   chunkTraces,
		ImageTraces:   imageTraces,
		Processed:     true,
	}
	if err := river.RecordOutput(ctx, result); err != nil {
		return err
	}

	// Surface catastrophic failures to River so the job is NOT
	// marked "completed" when extraction actually broke. Three
	// cases:
	//
	//  1. Every chunk failed AND no facts were produced — the
	//     whole text pass was broken (the timeout scenario). The
	//     source is still marked processed (above) so it won't
	//     loop, but the job row turns red and the errors list
	//     populates so the UI shows something went wrong.
	//  2. Every image failed when images were expected (source had
	//     images, extraction was enabled, and zero images produced
	//     facts due to errors — not because the images were
	//     decorative). Same rationale: surface it, don't hide it.
	//  3. Image-only source (no parsed text) where every image that
	//     reached the extractor errored. Without this the job would
	//     silently complete with 0 facts and the user would see a
	//     "processed" source with nothing in it; the red job row
	//     surfaces the broken pipeline (e.g. vision model
	//     misconfigured, storage down).
	//
	// Partial failures (some chunks ok, some failed) are NOT
	// escalated to a hard error — the job did useful work and the
	// chunk_failures / image_failures counts in the output carry
	// the signal for the UI to render a warning.
	if len(chunks) > 0 && chunkFailures == len(chunks) && totalFacts == 0 {
		return fmt.Errorf("source_decomposition: all %d chunks failed extraction (0 facts produced); see chunk_failures in output", len(chunks))
	}
	if !sourceHasText && imagesAttempted > 0 && imageFailures == imagesAttempted && totalFacts == 0 {
		return fmt.Errorf("source_decomposition: image-only source had %d image(s) attempted and all failed (0 facts produced); see image_failures in output", imagesAttempted)
	}
	return nil
}

// extractImageFacts runs the multimodal image extractor over every
// image attached to the source and persists the returned facts as
// image facts (fact_kind='image', image_url set). It returns the
// number of images that produced at least one fact, the number
// of images that errored, and the number of images that were
// actually attempted (reached the extractor, not skipped before
// the call); it mutates totalFacts to include the image facts
// so the embed_facts chain fires when any facts (text or image)
// were produced.
//
// sourceHasText is forwarded to each ImageFactRequest so the
// extractor's prompt builder can append the focus-figures scope note
// when the source's text body was already processed (true) or stay
// generic for image-only sources (false).
//
// repoSlug is the repository's slug (resolved once at the top of
// Work). It is used to synthesize a service-routable image_url for
// PDF page renders that have no remote URL — see imageURLFor below.
// An empty slug disables that fallback; such rows keep the
// historical behavior of NULL image_url.
//
// Image bytes come from the DB for page renders (PDF) and from the
// ImageFetcher for inline images. Per-image failures are logged and
// the source is still marked processed. Image facts use
// chunk_index = -1 on the junction so they sort after text facts in
// ListFactsBySource (which orders by chunk_index then first_seen_at)
// without a junction schema change.
func (w *SourceDecompositionWorker) extractImageFacts(
	ctx context.Context,
	queries *store.Queries,
	db store.DBTX,
	source store.OktRepositorySource,
	sourceHasText bool,
	repoSlug string,
	args SourceDecompositionArgs,
	job *river.Job[SourceDecompositionArgs],
	totalFacts *int,
	imageTraces *[]ImageTrace,
	imageExtractor decomposition.ImageFactExtractionProvider,
	psHashPtr *string,
) (int, int, int) {
	images, err := queries.ListSourceImages(ctx, source.ID)
	if err != nil {
		log.Printf("source_decomposition: listing images for source %s failed: %v", args.SourceID, err)
		return 0, 0, 0
	}

	max := w.imageCfg.MaxImagesPerSource
	if max > 0 && len(images) > max {
		log.Printf("source_decomposition: source %s has %d images, capping to %d", args.SourceID, len(images), max)
		images = images[:max]
	}

	sourceTitle := ""
	if source.ParsedTitle != nil {
		sourceTitle = *source.ParsedTitle
	}
	attribution := decomposition.FactExtractionAttribution{
		RepositoryID: args.RepositoryID,
		SourceID:     args.SourceID,
		TaskID:       strconv.FormatInt(job.ID, 10),
	}

	// ---- Phase 1: parallel AI extraction (no DB writes) ----
	// Each worker resolves the imageURL, fetches bytes from storage
	// (read-only, concurrent-safe), and calls ExtractImageFacts. Skips
	// (no storage_key, storage get/read failure) are recorded as a
	// zero-fact result with a Skipped reason so phase 2 can surface
	// them in the trace without a separate path. The extractor's
	// internal retryWithBackoff handles 429/5xx/net retries.
	type imageExtract struct {
		facts   []string
		trace   ImageTrace
		attempt bool // true == reached the extractor (not skipped)
	}
	concurrency := w.imageCfg.ConcurrencyOr(4)
	imgResults, imgErrs := decomposition.ExtractParallel(ctx, concurrency, images, func(callCtx context.Context, img store.OktRepositorySourceImage) (imageExtract, error) {
		start := time.Now()
		trace := ImageTrace{Type: "image", Index: 0}
		if img.PageNumber != nil && *img.PageNumber != 0 {
			pn := int(*img.PageNumber)
			trace.PageNumber = &pn
		}

		imageURL := imageURLFor(img, repoSlug, source.ID)
		trace.ImageURL = imageURL
		altText := ""
		if img.AltText != nil {
			altText = *img.AltText
		}

		// Page renders (PDF) carry their byte *length* in the
		// `bytes` column; the actual payload lives in the storage
		// backend under `storage_key`. When storage is configured
		// and the row has been mirrored, fetch the bytes from
		// storage and feed them to the extractor directly (no
		// remote round-trip — page renders have no URL anyway).
		// When storage is not configured or the row hasn't been
		// mirrored yet, fall back to the historical skip so the
		// source still processes its text facts.
		var imageBytes []byte
		if img.Bytes != nil && *img.Bytes > 0 {
			if w.storage == nil || img.StorageKey == nil || *img.StorageKey == "" {
				log.Printf("source_decomposition: source %s image page=%d has bytes-length but no storage_key; skipping (storage not yet mirrored)", args.SourceID, img.PageNumber)
				trace.Skipped = "no storage_key (not yet mirrored)"
				trace.DurationMs = time.Since(start).Milliseconds()
				return imageExtract{trace: trace, attempt: false}, nil
			}
			file, err := w.storage.Get(callCtx, *img.StorageKey)
			if err != nil {
				log.Printf("source_decomposition: source %s image page=%d storage get failed: %v; skipping", args.SourceID, img.PageNumber, err)
				trace.Skipped = "storage get failed: " + err.Error()
				trace.DurationMs = time.Since(start).Milliseconds()
				return imageExtract{trace: trace, attempt: false}, nil
			}
			body, err := io.ReadAll(file.Body)
			_ = file.Body.Close()
			if err != nil {
				log.Printf("source_decomposition: source %s image page=%d storage read failed: %v; skipping", args.SourceID, img.PageNumber, err)
				trace.Skipped = "storage read failed: " + err.Error()
				trace.DurationMs = time.Since(start).Milliseconds()
				return imageExtract{trace: trace, attempt: false}, nil
			}
			imageBytes = body
		}

		req := decomposition.ImageFactRequest{
			SourceURL:     source.Url,
			SourceTitle:   sourceTitle,
			ImageAlt:      altText,
			ImageURL:      imageURL,
			ImageBytes:    imageBytes,
			SourceHasText: sourceHasText,
			Attribution:   attribution,
		}

		facts, err := imageExtractor.ExtractImageFacts(callCtx, db, req)
		if err != nil {
			trace.Error = err.Error()
			trace.DurationMs = time.Since(start).Milliseconds()
			return imageExtract{trace: trace, attempt: true}, err
		}
		trace.Facts = len(facts)
		trace.DurationMs = time.Since(start).Milliseconds()
		return imageExtract{facts: facts, trace: trace, attempt: true}, nil
	})

	// ---- Phase 2: serial persistence (single goroutine) ----
	imagesWithFacts := 0
	imageFailures := 0
	imagesAttempted := 0
	for i := range images {
		res := imgResults[i]
		err := imgErrs[i]
		trace := res.trace
		trace.Index = i

		if res.trace.Skipped != "" {
			// Storage-side skip before the extractor was reached.
			*imageTraces = append(*imageTraces, trace)
			continue
		}
		// The image reached the extractor (was not storage-skipped),
		// so it counts as attempted regardless of whether the model
		// call then errored or returned zero facts.
		imagesAttempted++
		if err != nil {
			log.Printf("source_decomposition: image %s extraction failed for source %s: %v", res.trace.ImageURL, args.SourceID, err)
			imageFailures++
			*imageTraces = append(*imageTraces, trace)
			continue
		}

		facts := res.facts
		if len(facts) == 0 {
			*imageTraces = append(*imageTraces, trace)
			continue
		}
		imagesWithFacts++
		imageURL := res.trace.ImageURL
		for _, factText := range facts {
			factID := pgtype.UUID{}
			if err := factID.Scan(uuid.New().String()); err != nil {
				log.Printf("source_decomposition: generating image fact id failed: %v", err)
				continue
			}
			imgURLPtr := imageURLPtr(imageURL)
		created, err := queries.CreateFact(ctx, store.CreateFactParams{
			ID:            factID,
			Text:          factText,
			FactKind:      "image",
			ImageUrl:      imgURLPtr,
			PromptsetHash: psHashPtr,
		})
			if err != nil {
				log.Printf("source_decomposition: persisting image fact failed: %v", err)
				continue
			}
			// chunk_index = -1 marks image facts so they sort
			// after text facts in ListFactsBySource without a
			// junction schema change.
			if err := queries.AddFactSource(ctx, store.AddFactSourceParams{
				FactID:     created.ID,
				SourceID:   source.ID,
				ChunkIndex: -1,
			}); err != nil {
				log.Printf("source_decomposition: linking image fact to source failed: %v", err)
				continue
			}
			*totalFacts++
		}
		*imageTraces = append(*imageTraces, trace)
	}

	log.Printf("source_decomposition: source %s extracted image facts from %d/%d images (%d failures)", args.SourceID, imagesWithFacts, len(images), imageFailures)
	return imagesWithFacts, imageFailures, imagesAttempted
}

// sentencesFromOffsets reconstructs the global sentence array from
// the flat [start0, end0, start1, end1, ...] rune-offset array
// persisted by the retrieve_source worker. The Text field is left
// empty — the worker only needs the indices and offsets to label
// chunk text and to bound fact_references; it never reslices the
// source text here. An odd-length or empty array yields no
// sentences (defensive: the migration guarantees pairs).
func sentencesFromOffsets(offsets []int32) []decomposition.Sentence {
	if len(offsets) == 0 || len(offsets)%2 != 0 {
		return nil
	}
	sents := make([]decomposition.Sentence, 0, len(offsets)/2)
	for i := 0; i+1 < len(offsets); i += 2 {
		sents = append(sents, decomposition.Sentence{
			Index:     len(sents),
			StartRune: int(offsets[i]),
			EndRune:   int(offsets[i+1]),
		})
	}
	return sents
}

// imageURLFor returns the URL to persist on facts.image_url for
// the given source image. Inline images (kind='inline') already
// carry a remote URL; that one is returned unchanged. PDF page
// renders (kind='page') have no remote URL — their bytes live in
// the storage backend under storage_key — so we synthesize a
// service-routable URL that streams the bytes via the
// ServeSourceImage handler. The frontend detects this same-origin
// storage URL and fetches it through the authenticated
// getSourceImage helper (the <img> element cannot send the
// Authorization header itself). An empty slug or storage_key
// yields an empty string, which is persisted as NULL image_url
// (the historical behavior).
func imageURLFor(img store.OktRepositorySourceImage, repoSlug string, sourceID pgtype.UUID) string {
	if img.Url != nil && *img.Url != "" {
		return *img.Url
	}
	if repoSlug == "" {
		return ""
	}
	if img.StorageKey == nil || *img.StorageKey == "" {
		return ""
	}
	return fmt.Sprintf("/api/v1/repositories/%s/sources/%s/images/%s",
		repoSlug, uuid.UUID(sourceID.Bytes), uuid.UUID(img.ID.Bytes))
}

// imageURLPtr returns a *string suitable for the nullable
// facts.image_url column: nil when url is empty (NULL in the DB),
// otherwise a pointer to url.
func imageURLPtr(url string) *string {
	if url == "" {
		return nil
	}
	return &url
}

// buildLabeledChunkText constructs the AI-facing input for one chunk
// by emitting only the global sentences that overlap the chunk's
// rune range, each prefixed with its global [Sn] index. This lets
// the model return the global sentence indices it derived each fact
// from, which the worker writes directly to fact_references without
// any chunk-relative translation. Sentences are emitted in index
// order; the chunker is interchangeable and its boundaries only
// decide which sentences the model sees together, never how they are
// numbered. sourceText must be the same string the retrieve_source
// worker segmented (parsed_markdown preferred, parsed_text fallback)
// so the offsets align.
func buildLabeledChunkText(sentences []decomposition.Sentence, sourceText string, chunkStart, chunkEnd int) string {
	runes := []rune(sourceText)
	var b strings.Builder
	for _, s := range sentences {
		if s.EndRune <= chunkStart || s.StartRune >= chunkEnd {
			continue
		}
		if s.StartRune < 0 || s.EndRune > len(runes) || s.StartRune >= s.EndRune {
			continue
		}
		fmt.Fprintf(&b, "[S%d] %s ", s.Index, string(runes[s.StartRune:s.EndRune]))
	}
	return strings.TrimSpace(b.String())
}
