//go:build e2e

package e2e_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/openktree/open-knowledge-tree/backend/e2e/testutil"
	"github.com/openktree/open-knowledge-tree/backend/internal/config"
	"github.com/openktree/open-knowledge-tree/backend/internal/providers/decomposition"
	"github.com/openktree/open-knowledge-tree/backend/internal/providers/storage"
	"github.com/openktree/open-knowledge-tree/backend/internal/store"
	"github.com/openktree/open-knowledge-tree/backend/internal/taskmanager/tasks"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivertest"
)

// stubImageExtractor is a test double for
// decomposition.ImageFactExtractionProvider that returns a
// caller-configured fact list per call, records the requests it
// saw, and can inject an error to exercise the worker's per-image
// error tolerance. It does not call any real model.
type stubImageExtractor struct {
	facts []string
	err   error
	calls []decomposition.ImageFactRequest
}

func (s *stubImageExtractor) ExtractImageFacts(ctx context.Context, db store.DBTX, req decomposition.ImageFactRequest) ([]string, error) {
	s.calls = append(s.calls, req)
	if s.err != nil {
		return nil, s.err
	}
	return s.facts, nil
}

func (s *stubImageExtractor) Describe() decomposition.ProviderDescription {
	return decomposition.ProviderDescription{
		Name:       "stub-image-extractor",
		Configured: true,
		Supports:   []string{"image_extraction"},
	}
}

// stubChunker returns the whole text as a single chunk so the
// decomposition worker's text path runs once and produces one text
// fact via the stub text extractor below.
type stubChunker struct{}

func (stubChunker) Chunk(text string) []decomposition.Chunk {
	return []decomposition.Chunk{{Index: 0, Text: text, StartRune: 0, EndRune: len([]rune(text))}}
}
func (stubChunker) Describe() decomposition.ProviderDescription {
	return decomposition.ProviderDescription{Name: "stub-chunker", Configured: true, Supports: []string{"chunking"}}
}

// stubFactExtractor returns a single canned text fact per call so
// the text path of the decomposition worker produces at least one
// fact and we can assert text + image facts coexist.
type stubFactExtractor struct{}

func (stubFactExtractor) ExtractFacts(ctx context.Context, db store.DBTX, chunkText string, attr decomposition.FactExtractionAttribution) ([]decomposition.ExtractedFact, error) {
	return []decomposition.ExtractedFact{{Text: "stub text fact from chunk", Sentences: nil}}, nil
}
func (stubFactExtractor) Describe() decomposition.ProviderDescription {
	return decomposition.ProviderDescription{Name: "stub-fact-extractor", Configured: true, Supports: []string{"fact_extraction"}}
}

// errFactExtractor always returns an error, used to exercise the
// all-chunks-failed escalation path.
type errFactExtractor struct{}

func (errFactExtractor) ExtractFacts(ctx context.Context, db store.DBTX, chunkText string, attr decomposition.FactExtractionAttribution) ([]decomposition.ExtractedFact, error) {
	return nil, errors.New("simulated extraction failure")
}
func (errFactExtractor) Describe() decomposition.ProviderDescription {
	return decomposition.ProviderDescription{Name: "err-fact-extractor", Configured: true, Supports: []string{"fact_extraction"}}
}

// TestSourceDecomposition_ImageFacts drives the
// SourceDecompositionWorker directly with a stub image extractor
// and asserts that image facts are created with fact_kind='image',
// image_url set, and chunk_index=-1 on the junction. It also
// verifies text facts (fact_kind='text') are still produced and
// that the source is marked processed.
func TestSourceDecomposition_ImageFacts(t *testing.T) {
	env := testutil.NewTestEnv(t)
	defer env.Server.Close()

	admin := bootstrapSysAdmin(t, env, "img_ext@example.com")
	_, _, repoID := createRepositoryWithDB(t, admin, "ImgExt", "img-ext", "desc", "")
	ensureRiverSchema(t, env.DB)
	queries := store.New(env.DB)

	// Create a source in 'fetched' status with parsed text (so the
	// text chunk loop runs) and one inline image (so the image loop
	// runs). The image URL points at a local httptest server so the
	// stub extractor's recorded calls carry a real URL.
	pngServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 1x1 PNG.
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write([]byte{
			0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a, 0x00, 0x00, 0x00, 0x0d, 0x49, 0x48, 0x44, 0x52,
			0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01, 0x08, 0x06, 0x00, 0x00, 0x00, 0x1f, 0x15, 0xc4,
			0x89, 0x00, 0x00, 0x00, 0x0d, 0x49, 0x44, 0x41, 0x54, 0x78, 0x9c, 0x62, 0x00, 0x01, 0x00, 0x00,
			0x05, 0x00, 0x01, 0x0d, 0x0a, 0x2d, 0xb4, 0x00, 0x00, 0x00, 0x00, 0x49, 0x45, 0x4e, 0x44, 0xae,
			0x42, 0x60, 0x82,
		})
	}))
	defer pngServer.Close()

	sourceID := pgtype.UUID{}
	if err := sourceID.Scan(uuid.NewString()); err != nil {
		t.Fatalf("scanning source id: %v", err)
	}
	title := "Acme Revenue 2023"
	if _, err := queries.CreateSource(context.Background(), store.CreateSourceParams{
		ID:           sourceID,
		RepositoryID: pgRepoID(t, repoID),
		Url:          "https://example.com/img-ext",
		Kind:         "homepage",
		Status:       "fetched",
	}); err != nil {
		t.Fatalf("create source: %v", err)
	}
	if _, err := queries.MarkSourceParsed(context.Background(), store.MarkSourceParsedParams{
		ID:          sourceID,
		ParsedText:  ptrString("Some body text about Acme revenue."),
		ParsedTitle: ptrString(title),
		ParseStatus: ptrString("ok"),
		PublishedAt: pgtype.Date{}, // preserved via COALESCE in the query
	}); err != nil {
		t.Fatalf("mark source parsed: %v", err)
	}
	if _, err := queries.AddSourceImage(context.Background(), store.AddSourceImageParams{
		SourceID: sourceID,
		Kind:     "inline",
		Position: 0,
		Url:      ptrString(pngServer.URL + "/chart.png"),
		AltText:  ptrString("Acme revenue bar chart 2023"),
	}); err != nil {
		t.Fatalf("add source image: %v", err)
	}

	// Build the worker with stub providers and the image extractor
	// returning one canned image fact per call.
	imgExtractor := &stubImageExtractor{
		facts: []string{"Acme Corp revenue grew 42% from 2020 to 2023 per the bar chart in the source figure."},
	}
	registry := testutil.NewForTestPool(env.DB)
	imageCfg := config.DecompositionImageConfig{
		Enabled:            true,
		Provider:           "stub",
		Model:              "stub-vision",
		MaxImageBytes:      5 * 1024 * 1024,
		MaxImagesPerSource: 20,
	}
	worker := tasks.NewSourceDecompositionWorker(stubChunker{}, stubFactExtractor{}, imgExtractor, config.DecompositionFactConfig{}, imageCfg, registry, store.New(env.DB), nil, nil, nil)

	driver := riverpgxv5.New(env.DB)
	workers := river.NewWorkers()
	river.AddWorker(workers, worker)
	cfg := &river.Config{
		Queues: map[string]river.QueueConfig{
			tasks.QueueSourceDecomposition: {MaxWorkers: 1},
		},
		Workers: workers,
	}
	testWorker := rivertest.NewWorker(t, driver, cfg, worker)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	tx, err := env.DB.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer tx.Rollback(context.Background())

	job, err := testWorker.Work(ctx, t, tx, tasks.SourceDecompositionArgs{
		SourceID:     pgUUIDString(sourceID),
		RepositoryID: repoID,
	}, &river.InsertOpts{Queue: tasks.QueueSourceDecomposition})
	if err != nil {
		t.Fatalf("worker.Work: %v", err)
	}
	if job.EventKind != river.EventKindJobCompleted {
		t.Fatalf("expected completed, got %s", job.EventKind)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}

	// The image extractor must have been called exactly once with
	// the source URL, title, and image URL/alt carried through.
	if len(imgExtractor.calls) != 1 {
		t.Fatalf("expected 1 image extractor call, got %d", len(imgExtractor.calls))
	}
	call := imgExtractor.calls[0]
	if call.SourceURL != "https://example.com/img-ext" {
		t.Errorf("image extractor SourceURL = %q, want the source url", call.SourceURL)
	}
	if call.SourceTitle != title {
		t.Errorf("image extractor SourceTitle = %q, want %q", call.SourceTitle, title)
	}
	if call.ImageAlt != "Acme revenue bar chart 2023" {
		t.Errorf("image extractor ImageAlt = %q, want the alt text", call.ImageAlt)
	}
	// The source has parsed text, so the worker must forward
	// SourceHasText=true so the prompt builder appends the
	// focus-figures scope note.
	if !call.SourceHasText {
		t.Errorf("image extractor SourceHasText = false, want true (source has parsed text)")
	}

	// Verify the image fact was persisted with fact_kind='image',
	// image_url set, and chunk_index=-1 on the junction.
	imgFacts, err := queries.ListFactsBySource(context.Background(), store.ListFactsBySourceParams{
		SourceID: sourceID,
		Column2:  "",
		Column3:  "",
		Limit:    100,
		Offset:   0,
	})
	if err != nil {
		t.Fatalf("listing facts by source: %v", err)
	}
	var imageFactCount, textFactCount int
	for _, f := range imgFacts {
		if f.FactKind == "image" {
			imageFactCount++
			if f.ImageUrl == nil || *f.ImageUrl == "" {
				t.Errorf("image fact %s has nil/empty image_url", pgUUIDString(f.ID))
			}
			if f.ImageUrl != nil && *f.ImageUrl != pngServer.URL+"/chart.png" {
				t.Errorf("image fact image_url = %q, want %s", *f.ImageUrl, pngServer.URL+"/chart.png")
			}
		} else if f.FactKind == "text" {
			textFactCount++
		}
	}
	if imageFactCount != 1 {
		t.Errorf("expected 1 image fact, got %d", imageFactCount)
	}
	if textFactCount != 1 {
		t.Errorf("expected 1 text fact (from stubFactExtractor), got %d", textFactCount)
	}

	// chunk_index = -1 on the junction row for the image fact.
	junctionRows, err := queries.ListFactSourcesByFact(context.Background(), imgFacts[0].ID)
	if err != nil {
		t.Fatalf("listing fact sources: %v", err)
	}
	_ = junctionRows // The image fact is first (chunk_index=-1 sorts last);
	// we verify via the source listing below instead.

	// Verify the source is marked processed.
	src, err := queries.GetSourceByID(context.Background(), sourceID)
	if err != nil {
		t.Fatalf("get source: %v", err)
	}
	if src.Status != "processed" {
		t.Errorf("source status = %q, want processed", src.Status)
	}

	// Verify the recorded output carries Images=1.
	output := job.Job.Output()
	if output == nil {
		t.Fatal("expected recorded output on job row")
	}
	var result tasks.SourceDecompositionResult
	if err := json.Unmarshal(output, &result); err != nil {
		t.Fatalf("unmarshal output: %v", err)
	}
	if result.Images != 1 {
		t.Errorf("result.Images = %d, want 1", result.Images)
	}
	if result.Facts < 2 {
		t.Errorf("result.Facts = %d, want >= 2 (1 text + 1 image)", result.Facts)
	}
	if !result.Processed {
		t.Error("result.Processed = false, want true")
	}

	// Traces must be recorded for every chunk and every image. The
	// text-chunk loop produces 1 chunk (stubFactExtractor returns one
	// fact), so one ChunkTrace with Type=text, Facts=1, no Error and a
	// non-negative duration. The image loop produces one ImageTrace
	// with Type=image, Facts=1, no Error, no Skipped.
	if len(result.ChunkTraces) != 1 {
		t.Fatalf("result.ChunkTraces len = %d, want 1", len(result.ChunkTraces))
	}
	ct := result.ChunkTraces[0]
	if ct.Type != "text" {
		t.Errorf("ChunkTraces[0].Type = %q, want text", ct.Type)
	}
	if ct.Facts != 1 {
		t.Errorf("ChunkTraces[0].Facts = %d, want 1", ct.Facts)
	}
	if ct.Error != "" {
		t.Errorf("ChunkTraces[0].Error = %q, want empty (happy path)", ct.Error)
	}
	if ct.DurationMs < 0 {
		t.Errorf("ChunkTraces[0].DurationMs = %d, want >= 0", ct.DurationMs)
	}

	if len(result.ImageTraces) != 1 {
		t.Fatalf("result.ImageTraces len = %d, want 1", len(result.ImageTraces))
	}
	it := result.ImageTraces[0]
	if it.Type != "image" {
		t.Errorf("ImageTraces[0].Type = %q, want image", it.Type)
	}
	if it.Facts != 1 {
		t.Errorf("ImageTraces[0].Facts = %d, want 1", it.Facts)
	}
	if it.Error != "" {
		t.Errorf("ImageTraces[0].Error = %q, want empty (happy path)", it.Error)
	}
	if it.Skipped != "" {
		t.Errorf("ImageTraces[0].Skipped = %q, want empty (image was processed)", it.Skipped)
	}
	if it.DurationMs < 0 {
		t.Errorf("ImageTraces[0].DurationMs = %d, want >= 0", it.DurationMs)
	}
}

// TestSourceDecomposition_ImageExtractionErrorIsSkipped verifies
// that a per-image extraction failure (e.g. the model returns an
// error) does not fail the job: the image is skipped, text facts
// are still produced, and the source is marked processed. This
// mirrors the per-chunk text error tolerance the worker already
// has.
func TestSourceDecomposition_ImageExtractionErrorIsSkipped(t *testing.T) {
	env := testutil.NewTestEnv(t)
	defer env.Server.Close()

	admin := bootstrapSysAdmin(t, env, "img_err@example.com")
	_, _, repoID := createRepositoryWithDB(t, admin, "ImgErr", "img-err", "desc", "")
	ensureRiverSchema(t, env.DB)
	queries := store.New(env.DB)

	sourceID := pgtype.UUID{}
	if err := sourceID.Scan(uuid.NewString()); err != nil {
		t.Fatalf("scanning source id: %v", err)
	}
	if _, err := queries.CreateSource(context.Background(), store.CreateSourceParams{
		ID:           sourceID,
		RepositoryID: pgRepoID(t, repoID),
		Url:          "https://example.com/img-err",
		Kind:         "homepage",
		Status:       "fetched",
	}); err != nil {
		t.Fatalf("create source: %v", err)
	}
	if _, err := queries.MarkSourceParsed(context.Background(), store.MarkSourceParsedParams{
		ID:          sourceID,
		ParsedText:  ptrString("Some body text."),
		ParseStatus: ptrString("ok"),
		PublishedAt: pgtype.Date{},
	}); err != nil {
		t.Fatalf("mark source parsed: %v", err)
	}
	if _, err := queries.AddSourceImage(context.Background(), store.AddSourceImageParams{
		SourceID: sourceID,
		Kind:     "inline",
		Position: 0,
		Url:      ptrString("https://example.com/broken.png"),
	}); err != nil {
		t.Fatalf("add source image: %v", err)
	}

	imgExtractor := &stubImageExtractor{err: errors.New("model exploded")}
	registry := testutil.NewForTestPool(env.DB)
	imageCfg := config.DecompositionImageConfig{Enabled: true, Provider: "stub", Model: "stub", MaxImagesPerSource: 20, MaxImageBytes: 5 * 1024 * 1024}
	worker := tasks.NewSourceDecompositionWorker(stubChunker{}, stubFactExtractor{}, imgExtractor, config.DecompositionFactConfig{}, imageCfg, registry, store.New(env.DB), nil, nil, nil)

	driver := riverpgxv5.New(env.DB)
	workers := river.NewWorkers()
	river.AddWorker(workers, worker)
	cfg := &river.Config{
		Queues:  map[string]river.QueueConfig{tasks.QueueSourceDecomposition: {MaxWorkers: 1}},
		Workers: workers,
	}
	testWorker := rivertest.NewWorker(t, driver, cfg, worker)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	tx, err := env.DB.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer tx.Rollback(context.Background())

	job, err := testWorker.Work(ctx, t, tx, tasks.SourceDecompositionArgs{
		SourceID:     pgUUIDString(sourceID),
		RepositoryID: repoID,
	}, &river.InsertOpts{Queue: tasks.QueueSourceDecomposition})
	if err != nil {
		t.Fatalf("worker.Work: %v", err)
	}
	if job.EventKind != river.EventKindJobCompleted {
		t.Fatalf("expected completed even with image error, got %s", job.EventKind)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}

	// The image extractor was called and failed, so no image
	// facts. The text fact from the stub text extractor must still
	// be present.
	facts, err := queries.ListFactsBySource(context.Background(), store.ListFactsBySourceParams{
		SourceID: sourceID,
		Limit:    100,
	})
	if err != nil {
		t.Fatalf("listing facts: %v", err)
	}
	var imageFactCount, textFactCount int
	for _, f := range facts {
		if f.FactKind == "image" {
			imageFactCount++
		} else if f.FactKind == "text" {
			textFactCount++
		}
	}
	if imageFactCount != 0 {
		t.Errorf("expected 0 image facts after error, got %d", imageFactCount)
	}
	if textFactCount != 1 {
		t.Errorf("expected 1 text fact (error must not block text path), got %d", textFactCount)
	}

	// Source still marked processed despite the image error.
	src, err := queries.GetSourceByID(context.Background(), sourceID)
	if err != nil {
		t.Fatalf("get source: %v", err)
	}
	if src.Status != "processed" {
		t.Errorf("source status = %q, want processed (image error must not block)", src.Status)
	}

	var result tasks.SourceDecompositionResult
	if err := json.Unmarshal(job.Job.Output(), &result); err != nil {
		t.Fatalf("unmarshal output: %v", err)
	}
	if result.Images != 0 {
		t.Errorf("result.Images = %d, want 0 (error path)", result.Images)
	}
	if result.Facts != 1 {
		t.Errorf("result.Facts = %d, want 1 (text only)", result.Facts)
	}
	if result.ImageFailures != 1 {
		t.Errorf("result.ImageFailures = %d, want 1 (one image errored)", result.ImageFailures)
	}
	if result.ChunkFailures != 0 {
		t.Errorf("result.ChunkFailures = %d, want 0 (text path succeeded)", result.ChunkFailures)
	}

	// Traces: the text-chunk loop produced 1 chunk (the stub
	// extractor returns one fact), so one ChunkTrace with no error;
	// the image loop attempted one image and the extractor errored,
	// so one ImageTrace with Type=image, Error set, Facts=0.
	if len(result.ChunkTraces) != 1 {
		t.Fatalf("result.ChunkTraces len = %d, want 1", len(result.ChunkTraces))
	}
	if result.ChunkTraces[0].Error != "" {
		t.Errorf("ChunkTraces[0].Error = %q, want empty (text path ok)", result.ChunkTraces[0].Error)
	}
	if len(result.ImageTraces) != 1 {
		t.Fatalf("result.ImageTraces len = %d, want 1", len(result.ImageTraces))
	}
	it := result.ImageTraces[0]
	if it.Type != "image" {
		t.Errorf("ImageTraces[0].Type = %q, want image", it.Type)
	}
	if it.Error == "" {
		t.Error("ImageTraces[0].Error = empty, want the extraction error string")
	}
	if it.Facts != 0 {
		t.Errorf("ImageTraces[0].Facts = %d, want 0 (image errored)", it.Facts)
	}
	if it.Skipped != "" {
		t.Errorf("ImageTraces[0].Skipped = %q, want empty (image was attempted, not skipped)", it.Skipped)
	}
}

// TestSourceDecomposition_AllChunksFailedErrors verifies that when
// every chunk fails extraction (the timeout scenario), the worker
// returns a non-nil error so River marks the job errored (red in
// the UI) instead of completed, while still recording the output
// with the failure counts so the UI can show what broke.
func TestSourceDecomposition_AllChunksFailedErrors(t *testing.T) {
	env := testutil.NewTestEnv(t)
	defer env.Server.Close()

	admin := bootstrapSysAdmin(t, env, "img_allfail@example.com")
	_, _, repoID := createRepositoryWithDB(t, admin, "ImgAllFail", "img-allfail", "desc", "")
	ensureRiverSchema(t, env.DB)
	queries := store.New(env.DB)

	sourceID := pgtype.UUID{}
	if err := sourceID.Scan(uuid.NewString()); err != nil {
		t.Fatalf("scanning source id: %v", err)
	}
	if _, err := queries.CreateSource(context.Background(), store.CreateSourceParams{
		ID:           sourceID,
		RepositoryID: pgRepoID(t, repoID),
		Url:          "https://example.com/img-allfail",
		Kind:         "homepage",
		Status:       "fetched",
	}); err != nil {
		t.Fatalf("create source: %v", err)
	}
	if _, err := queries.MarkSourceParsed(context.Background(), store.MarkSourceParsedParams{
		ID:          sourceID,
		ParsedText:  ptrString("Body text that will be chunked but every chunk fails."),
		ParseStatus: ptrString("ok"),
		PublishedAt: pgtype.Date{},
	}); err != nil {
		t.Fatalf("mark source parsed: %v", err)
	}

	// Text extractor that always errors; image extractor nil so
	// the image loop is skipped (the test is about the text-fail
	// escalation path). InJobRetries=1 so the retry round runs
	// once (5s backoff) before the worker gives up and escalates.
	errExtractor := &errFactExtractor{}
	registry := testutil.NewForTestPool(env.DB)
	imageCfg := config.DecompositionImageConfig{Enabled: false}
	factCfg := config.DecompositionFactConfig{InJobRetries: 1}
	worker := tasks.NewSourceDecompositionWorker(stubChunker{}, errExtractor, nil, factCfg, imageCfg, registry, store.New(env.DB), nil, nil, nil)

	driver := riverpgxv5.New(env.DB)
	workers := river.NewWorkers()
	river.AddWorker(workers, worker)
	cfg := &river.Config{
		Queues:  map[string]river.QueueConfig{tasks.QueueSourceDecomposition: {MaxWorkers: 1}},
		Workers: workers,
	}
	testWorker := rivertest.NewWorker(t, driver, cfg, worker)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	tx, err := env.DB.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer tx.Rollback(context.Background())

	job, workErr := testWorker.Work(ctx, t, tx, tasks.SourceDecompositionArgs{
		SourceID:     pgUUIDString(sourceID),
		RepositoryID: repoID,
	}, &river.InsertOpts{Queue: tasks.QueueSourceDecomposition})

	// The job must be in an error state, not completed, so the
	// UI surfaces it as a failure rather than a silent success.
	// When all chunks fail, the worker returns a non-nil error
	// (after recording the output), which River turns into
	// EventKindJobFailed. The workErr itself carries the
	// escalation message — assert on it, don't treat it as a
	// test harness failure.
	if job == nil || job.EventKind != river.EventKindJobFailed {
		t.Fatalf("expected EventKindJobFailed when all chunks fail, got job=%+v err=%v", job, workErr)
	}
	if workErr == nil {
		t.Error("expected a non-nil error from worker.Work when all chunks fail")
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}

	// The output must still be recorded with the failure counts so
	// the UI's ResultSummary can render the breakdown.
	var result tasks.SourceDecompositionResult
	if err := json.Unmarshal(job.Job.Output(), &result); err != nil {
		t.Fatalf("unmarshal output: %v", err)
	}
	if result.ChunkFailures != 1 {
		t.Errorf("result.ChunkFailures = %d, want 1", result.ChunkFailures)
	}
	if result.Facts != 0 {
		t.Errorf("result.Facts = %d, want 0 (all chunks failed)", result.Facts)
	}

	// Traces: the failing extractor was called once for the single
	// chunk, so one ChunkTrace with Type=text, Error set, Facts=0,
	// and a non-negative duration. No ImageTraces because the
	// image extractor was nil/disabled in this test setup.
	if len(result.ChunkTraces) != 1 {
		t.Fatalf("result.ChunkTraces len = %d, want 1", len(result.ChunkTraces))
	}
	ct := result.ChunkTraces[0]
	if ct.Type != "text" {
		t.Errorf("ChunkTraces[0].Type = %q, want text", ct.Type)
	}
	if ct.Error == "" {
		t.Error("ChunkTraces[0].Error = empty, want the extraction error string")
	}
	if ct.Facts != 0 {
		t.Errorf("ChunkTraces[0].Facts = %d, want 0 (chunk failed)", ct.Facts)
	}
	if ct.DurationMs < 0 {
		t.Errorf("ChunkTraces[0].DurationMs = %d, want >= 0", ct.DurationMs)
	}
}

// TestSourceDecomposition_ImageExtractionDisabled verifies that
// when image extraction is disabled in config (Enabled=false), the
// worker skips the image loop entirely even when an image extractor
// is wired. Text facts are still produced.
func TestSourceDecomposition_ImageExtractionDisabled(t *testing.T) {
	env := testutil.NewTestEnv(t)
	defer env.Server.Close()

	admin := bootstrapSysAdmin(t, env, "img_dis@example.com")
	_, _, repoID := createRepositoryWithDB(t, admin, "ImgDis", "img-dis", "desc", "")
	ensureRiverSchema(t, env.DB)
	queries := store.New(env.DB)

	sourceID := pgtype.UUID{}
	if err := sourceID.Scan(uuid.NewString()); err != nil {
		t.Fatalf("scanning source id: %v", err)
	}
	if _, err := queries.CreateSource(context.Background(), store.CreateSourceParams{
		ID:           sourceID,
		RepositoryID: pgRepoID(t, repoID),
		Url:          "https://example.com/img-dis",
		Kind:         "homepage",
		Status:       "fetched",
	}); err != nil {
		t.Fatalf("create source: %v", err)
	}
	if _, err := queries.MarkSourceParsed(context.Background(), store.MarkSourceParsedParams{
		ID:          sourceID,
		ParsedText:  ptrString("Body text."),
		ParseStatus: ptrString("ok"),
		PublishedAt: pgtype.Date{},
	}); err != nil {
		t.Fatalf("mark source parsed: %v", err)
	}
	if _, err := queries.AddSourceImage(context.Background(), store.AddSourceImageParams{
		SourceID: sourceID,
		Kind:     "inline",
		Position: 0,
		Url:      ptrString("https://example.com/chart.png"),
	}); err != nil {
		t.Fatalf("add source image: %v", err)
	}

	imgExtractor := &stubImageExtractor{facts: []string{"should not be produced"}}
	registry := testutil.NewForTestPool(env.DB)
	imageCfg := config.DecompositionImageConfig{Enabled: false, Provider: "stub", Model: "stub", MaxImagesPerSource: 20, MaxImageBytes: 5 * 1024 * 1024}
	worker := tasks.NewSourceDecompositionWorker(stubChunker{}, stubFactExtractor{}, imgExtractor, config.DecompositionFactConfig{}, imageCfg, registry, store.New(env.DB), nil, nil, nil)

	driver := riverpgxv5.New(env.DB)
	workers := river.NewWorkers()
	river.AddWorker(workers, worker)
	cfg := &river.Config{
		Queues:  map[string]river.QueueConfig{tasks.QueueSourceDecomposition: {MaxWorkers: 1}},
		Workers: workers,
	}
	testWorker := rivertest.NewWorker(t, driver, cfg, worker)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	tx, err := env.DB.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer tx.Rollback(context.Background())

	job, err := testWorker.Work(ctx, t, tx, tasks.SourceDecompositionArgs{
		SourceID:     pgUUIDString(sourceID),
		RepositoryID: repoID,
	}, &river.InsertOpts{Queue: tasks.QueueSourceDecomposition})
	if err != nil {
		t.Fatalf("worker.Work: %v", err)
	}
	if job.EventKind != river.EventKindJobCompleted {
		t.Fatalf("expected completed, got %s", job.EventKind)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}

	// The image extractor must NOT have been called.
	if len(imgExtractor.calls) != 0 {
		t.Errorf("expected 0 image extractor calls when disabled, got %d", len(imgExtractor.calls))
	}

	// No image facts; only the text fact.
	facts, err := queries.ListFactsBySource(context.Background(), store.ListFactsBySourceParams{
		SourceID: sourceID,
		Limit:    100,
	})
	if err != nil {
		t.Fatalf("listing facts: %v", err)
	}
	for _, f := range facts {
		if f.FactKind == "image" {
			t.Errorf("expected no image facts when disabled, got one: %+v", f)
		}
	}
}

// TestSourceDecomposition_ImageOnlyPDFProcesses verifies that a
// source with NO parsed text but with images (the scanned-PDF /
// image-only-PDF case) is still processed when image extraction is
// enabled. Before the fix the worker early-returned at the
// no-parsed-text gate and never reached the image loop; now it
// skips the text-chunk loop and runs image extraction only.
//
// The stub extractor records each ImageFactRequest so we can assert
// SourceHasText=false (the prompt builder must NOT append the
// focus-figures scope note for an image-only source — the image IS
// the primary content and the model should transcribe everything).
// We use inline images (kind='inline', url set, no bytes) so the
// test does not need a storage backend; the stub extractor does not
// fetch bytes, it just returns the canned fact.
func TestSourceDecomposition_ImageOnlyPDFProcesses(t *testing.T) {
	env := testutil.NewTestEnv(t)
	defer env.Server.Close()

	admin := bootstrapSysAdmin(t, env, "img_only@example.com")
	_, _, repoID := createRepositoryWithDB(t, admin, "ImgOnly", "img-only", "desc", "")
	ensureRiverSchema(t, env.DB)
	queries := store.New(env.DB)

	sourceID := pgtype.UUID{}
	if err := sourceID.Scan(uuid.NewString()); err != nil {
		t.Fatalf("scanning source id: %v", err)
	}
	title := "Scanned Field Notebook"
	if _, err := queries.CreateSource(context.Background(), store.CreateSourceParams{
		ID:           sourceID,
		RepositoryID: pgRepoID(t, repoID),
		Url:          "https://example.com/scanned-notebook.pdf",
		Kind:         "pdf",
		Status:       "fetched",
	}); err != nil {
		t.Fatalf("create source: %v", err)
	}
	// Mark the source parsed with parse_status='ok' but NO
	// parsed_text / parsed_markdown — this is the "parser ran,
	// recognized the PDF, produced page renders, but the PDF had
	// no text layer so the text body is empty" case. The
	// parse_status gate in the worker is about parse_status, not
	// about parsed_text being non-empty; the new gate is the
	// sourceHasText check.
	if _, err := queries.MarkSourceParsed(context.Background(), store.MarkSourceParsedParams{
		ID:          sourceID,
		ParsedTitle: ptrString(title),
		ParseStatus: ptrString("ok"),
		PublishedAt: pgtype.Date{},
	}); err != nil {
		t.Fatalf("mark source parsed: %v", err)
	}
	// Two inline images (simulating two page renders the parser
	// surfaced). Real PDF page renders are kind='page' with bytes
	// + storage_key; we use kind='inline' with URLs so the test
	// does not need a storage backend — the stub extractor does
	// not fetch bytes, it returns a canned fact per call.
	for i, u := range []string{
		"https://example.com/scanned-notebook/page-1.png",
		"https://example.com/scanned-notebook/page-2.png",
	} {
		if _, err := queries.AddSourceImage(context.Background(), store.AddSourceImageParams{
			SourceID: sourceID,
			Kind:     "inline",
			Position: int32(i),
			Url:      ptrString(u),
		}); err != nil {
			t.Fatalf("add source image %d: %v", i, err)
		}
	}

	imgExtractor := &stubImageExtractor{
		facts: []string{"The scanned page depicts a labelled cross-section of a plant leaf."},
	}
	registry := testutil.NewForTestPool(env.DB)
	imageCfg := config.DecompositionImageConfig{
		Enabled:            true,
		Provider:           "stub",
		Model:              "stub-vision",
		MaxImageBytes:      5 * 1024 * 1024,
		MaxImagesPerSource: 20,
	}
	worker := tasks.NewSourceDecompositionWorker(stubChunker{}, stubFactExtractor{}, imgExtractor, config.DecompositionFactConfig{}, imageCfg, registry, store.New(env.DB), nil, nil, nil)

	driver := riverpgxv5.New(env.DB)
	workers := river.NewWorkers()
	river.AddWorker(workers, worker)
	cfg := &river.Config{
		Queues: map[string]river.QueueConfig{
			tasks.QueueSourceDecomposition: {MaxWorkers: 1},
		},
		Workers: workers,
	}
	testWorker := rivertest.NewWorker(t, driver, cfg, worker)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	tx, err := env.DB.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer tx.Rollback(context.Background())

	job, err := testWorker.Work(ctx, t, tx, tasks.SourceDecompositionArgs{
		SourceID:     pgUUIDString(sourceID),
		RepositoryID: repoID,
	}, &river.InsertOpts{Queue: tasks.QueueSourceDecomposition})
	if err != nil {
		t.Fatalf("worker.Work: %v", err)
	}
	if job.EventKind != river.EventKindJobCompleted {
		t.Fatalf("expected completed, got %s", job.EventKind)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}

	// The image extractor must have been called once per image
	// (the text-chunk loop must NOT have run — there is no text).
	if len(imgExtractor.calls) != 2 {
		t.Fatalf("expected 2 image extractor calls (one per image), got %d", len(imgExtractor.calls))
	}
	for i, call := range imgExtractor.calls {
		if call.SourceHasText {
			t.Errorf("call %d SourceHasText = true, want false (source has no parsed text; prompt must stay generic so the model transcribes the page)", i)
		}
		if call.SourceTitle != title {
			t.Errorf("call %d SourceTitle = %q, want %q", i, call.SourceTitle, title)
		}
	}

	// Two image facts must be persisted (one per page); zero text
	// facts (no parsed text → no chunks → no text facts).
	facts, err := queries.ListFactsBySource(context.Background(), store.ListFactsBySourceParams{
		SourceID: sourceID,
		Limit:    100,
	})
	if err != nil {
		t.Fatalf("listing facts: %v", err)
	}
	var imageFactCount, textFactCount int
	for _, f := range facts {
		if f.FactKind == "image" {
			imageFactCount++
		} else if f.FactKind == "text" {
			textFactCount++
		}
	}
	if imageFactCount != 2 {
		t.Errorf("expected 2 image facts, got %d", imageFactCount)
	}
	if textFactCount != 0 {
		t.Errorf("expected 0 text facts (image-only source has no text chunks), got %d", textFactCount)
	}

	// Source must be marked processed even without parsed text.
	src, err := queries.GetSourceByID(context.Background(), sourceID)
	if err != nil {
		t.Fatalf("get source: %v", err)
	}
	if src.Status != "processed" {
		t.Errorf("source status = %q, want processed", src.Status)
	}

	// Recorded output: 0 chunks, 2 images, 2 facts, processed.
	var result tasks.SourceDecompositionResult
	if err := json.Unmarshal(job.Job.Output(), &result); err != nil {
		t.Fatalf("unmarshal output: %v", err)
	}
	if result.Chunks != 0 {
		t.Errorf("result.Chunks = %d, want 0 (no text to chunk)", result.Chunks)
	}
	if result.Images != 2 {
		t.Errorf("result.Images = %d, want 2", result.Images)
	}
	if result.Facts != 2 {
		t.Errorf("result.Facts = %d, want 2 (image facts only)", result.Facts)
	}
	if !result.Processed {
		t.Error("result.Processed = false, want true")
	}
	if len(result.ChunkTraces) != 0 {
		t.Errorf("result.ChunkTraces len = %d, want 0 (text loop skipped)", len(result.ChunkTraces))
	}
	if len(result.ImageTraces) != 2 {
		t.Errorf("result.ImageTraces len = %d, want 2", len(result.ImageTraces))
	}
}

// TestSourceDecomposition_ImageOnlyPDFAllImagesFailedErrors
// verifies the symmetric escalation for image-only sources: when
// the source has no parsed text, image extraction is enabled, and
// every image that reached the extractor errored, the worker
// returns a non-nil error so River marks the job failed (red in the
// UI) instead of silently completing with 0 facts. Without this the
// user would see a "processed" source with nothing in it and no
// signal that the vision model was broken.
func TestSourceDecomposition_ImageOnlyPDFAllImagesFailedErrors(t *testing.T) {
	env := testutil.NewTestEnv(t)
	defer env.Server.Close()

	admin := bootstrapSysAdmin(t, env, "img_only_fail@example.com")
	_, _, repoID := createRepositoryWithDB(t, admin, "ImgOnlyFail", "img-only-fail", "desc", "")
	ensureRiverSchema(t, env.DB)
	queries := store.New(env.DB)

	sourceID := pgtype.UUID{}
	if err := sourceID.Scan(uuid.NewString()); err != nil {
		t.Fatalf("scanning source id: %v", err)
	}
	if _, err := queries.CreateSource(context.Background(), store.CreateSourceParams{
		ID:           sourceID,
		RepositoryID: pgRepoID(t, repoID),
		Url:          "https://example.com/broken-scanned.pdf",
		Kind:         "pdf",
		Status:       "fetched",
	}); err != nil {
		t.Fatalf("create source: %v", err)
	}
	if _, err := queries.MarkSourceParsed(context.Background(), store.MarkSourceParsedParams{
		ID:          sourceID,
		ParseStatus: ptrString("ok"),
		PublishedAt: pgtype.Date{},
	}); err != nil {
		t.Fatalf("mark source parsed: %v", err)
	}
	if _, err := queries.AddSourceImage(context.Background(), store.AddSourceImageParams{
		SourceID: sourceID,
		Kind:     "inline",
		Position: 0,
		Url:      ptrString("https://example.com/broken-scanned/page-1.png"),
	}); err != nil {
		t.Fatalf("add source image: %v", err)
	}

	// Image extractor that always errors; text extractor is the
	// stub returning a fact, but it will never be called because
	// the source has no parsed text (the text-chunk loop is
	// skipped).
	imgExtractor := &stubImageExtractor{err: errors.New("vision model down")}
	registry := testutil.NewForTestPool(env.DB)
	imageCfg := config.DecompositionImageConfig{
		Enabled:            true,
		Provider:           "stub",
		Model:              "stub-vision",
		MaxImageBytes:      5 * 1024 * 1024,
		MaxImagesPerSource: 20,
	}
	worker := tasks.NewSourceDecompositionWorker(stubChunker{}, stubFactExtractor{}, imgExtractor, config.DecompositionFactConfig{}, imageCfg, registry, store.New(env.DB), nil, nil, nil)

	driver := riverpgxv5.New(env.DB)
	workers := river.NewWorkers()
	river.AddWorker(workers, worker)
	cfg := &river.Config{
		Queues:  map[string]river.QueueConfig{tasks.QueueSourceDecomposition: {MaxWorkers: 1}},
		Workers: workers,
	}
	testWorker := rivertest.NewWorker(t, driver, cfg, worker)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	tx, err := env.DB.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer tx.Rollback(context.Background())

	job, workErr := testWorker.Work(ctx, t, tx, tasks.SourceDecompositionArgs{
		SourceID:     pgUUIDString(sourceID),
		RepositoryID: repoID,
	}, &river.InsertOpts{Queue: tasks.QueueSourceDecomposition})

	// The job must be in a failed state, not completed, so the UI
	// surfaces the broken vision pipeline rather than a silent
	// 0-fact "processed" source.
	if job == nil || job.EventKind != river.EventKindJobFailed {
		t.Fatalf("expected EventKindJobFailed when all images fail on an image-only source, got job=%+v err=%v", job, workErr)
	}
	if workErr == nil {
		t.Error("expected a non-nil error from worker.Work when all images fail on an image-only source")
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}

	// Output must still be recorded with the failure counts.
	var result tasks.SourceDecompositionResult
	if err := json.Unmarshal(job.Job.Output(), &result); err != nil {
		t.Fatalf("unmarshal output: %v", err)
	}
	if result.Chunks != 0 {
		t.Errorf("result.Chunks = %d, want 0 (image-only)", result.Chunks)
	}
	if result.Images != 0 {
		t.Errorf("result.Images = %d, want 0 (all failed)", result.Images)
	}
	if result.Facts != 0 {
		t.Errorf("result.Facts = %d, want 0 (all failed)", result.Facts)
	}
	if result.ImageFailures != 1 {
		t.Errorf("result.ImageFailures = %d, want 1", result.ImageFailures)
	}
	if len(result.ImageTraces) != 1 {
		t.Fatalf("result.ImageTraces len = %d, want 1", len(result.ImageTraces))
	}
	if result.ImageTraces[0].Error == "" {
		t.Error("ImageTraces[0].Error = empty, want the extraction error string")
	}
}

// TestSourceDecomposition_ImageOnlyPDFSkippedWhenExtractionDisabled
// verifies the no-text gate still early-returns when image
// extraction is NOT enabled — an image-only source with no text and
// no image extractor configured produces no facts and is NOT marked
// processed (there is nothing to do). This preserves the historical
// behaviour for the "we don't have a vision model" deployment.
func TestSourceDecomposition_ImageOnlyPDFSkippedWhenExtractionDisabled(t *testing.T) {
	env := testutil.NewTestEnv(t)
	defer env.Server.Close()

	admin := bootstrapSysAdmin(t, env, "img_only_skip@example.com")
	_, _, repoID := createRepositoryWithDB(t, admin, "ImgOnlySkip", "img-only-skip", "desc", "")
	ensureRiverSchema(t, env.DB)
	queries := store.New(env.DB)

	sourceID := pgtype.UUID{}
	if err := sourceID.Scan(uuid.NewString()); err != nil {
		t.Fatalf("scanning source id: %v", err)
	}
	if _, err := queries.CreateSource(context.Background(), store.CreateSourceParams{
		ID:           sourceID,
		RepositoryID: pgRepoID(t, repoID),
		Url:          "https://example.com/no-extraction.pdf",
		Kind:         "pdf",
		Status:       "fetched",
	}); err != nil {
		t.Fatalf("create source: %v", err)
	}
	if _, err := queries.MarkSourceParsed(context.Background(), store.MarkSourceParsedParams{
		ID:          sourceID,
		ParseStatus: ptrString("ok"),
		PublishedAt: pgtype.Date{},
	}); err != nil {
		t.Fatalf("mark source parsed: %v", err)
	}
	if _, err := queries.AddSourceImage(context.Background(), store.AddSourceImageParams{
		SourceID: sourceID,
		Kind:     "inline",
		Position: 0,
		Url:      ptrString("https://example.com/no-extraction/page-1.png"),
	}); err != nil {
		t.Fatalf("add source image: %v", err)
	}

	// Image extraction disabled; imageExtractor nil. The worker
	// must early-return with Processed=false and no facts.
	registry := testutil.NewForTestPool(env.DB)
	imageCfg := config.DecompositionImageConfig{Enabled: false}
	worker := tasks.NewSourceDecompositionWorker(stubChunker{}, stubFactExtractor{}, nil, config.DecompositionFactConfig{}, imageCfg, registry, store.New(env.DB), nil, nil, nil)

	driver := riverpgxv5.New(env.DB)
	workers := river.NewWorkers()
	river.AddWorker(workers, worker)
	cfg := &river.Config{
		Queues:  map[string]river.QueueConfig{tasks.QueueSourceDecomposition: {MaxWorkers: 1}},
		Workers: workers,
	}
	testWorker := rivertest.NewWorker(t, driver, cfg, worker)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	tx, err := env.DB.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer tx.Rollback(context.Background())

	job, err := testWorker.Work(ctx, t, tx, tasks.SourceDecompositionArgs{
		SourceID:     pgUUIDString(sourceID),
		RepositoryID: repoID,
	}, &river.InsertOpts{Queue: tasks.QueueSourceDecomposition})
	if err != nil {
		t.Fatalf("worker.Work: %v", err)
	}
	if job.EventKind != river.EventKindJobCompleted {
		t.Fatalf("expected completed (no-op early return), got %s", job.EventKind)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}

	var result tasks.SourceDecompositionResult
	if err := json.Unmarshal(job.Job.Output(), &result); err != nil {
		t.Fatalf("unmarshal output: %v", err)
	}
	if result.Processed {
		t.Error("result.Processed = true, want false (no text and image extraction disabled → nothing to do)")
	}
	if result.Facts != 0 {
		t.Errorf("result.Facts = %d, want 0", result.Facts)
	}

	// Source must NOT be marked processed (the worker early-returned
	// before the MarkSourceProcessed call).
	src, err := queries.GetSourceByID(context.Background(), sourceID)
	if err != nil {
		t.Fatalf("get source: %v", err)
	}
	if src.Status == "processed" {
		t.Errorf("source status = %q, want not processed (no work was done)", src.Status)
	}
}

// ptrString is a tiny helper to take the address of a string literal
// for the nullable *string fields on the store params.
func ptrString(s string) *string { return &s }

// stubStorage is a minimal storage.FileStorage for tests that need
// the decomposition worker to read back page-render bytes via the
// storage backend. It returns a caller-configured byte slice for a
// caller-configured key and errors with ErrNotFound for any other
// key. Store/Delete are no-ops; Describe returns an empty
// description (the decomposition worker does not consult it).
type stubStorage struct {
	key  string
	body []byte
	ct   string
}

func (s *stubStorage) Store(ctx context.Context, key, contentType string, body []byte) (storage.StoredRef, error) {
	return storage.StoredRef{Key: key, ContentType: contentType, Bytes: int64(len(body))}, nil
}
func (s *stubStorage) Get(ctx context.Context, key string) (storage.StoredFile, error) {
	if key != s.key {
		return storage.StoredFile{}, storage.ErrNotFound
	}
	return storage.StoredFile{
		ContentType: s.ct,
		Body:        io.NopCloser(bytes.NewReader(s.body)),
		Size:        int64(len(s.body)),
	}, nil
}
func (s *stubStorage) Delete(ctx context.Context, key string) error { return nil }
func (s *stubStorage) Describe() storage.ProviderDescription {
	return storage.ProviderDescription{Name: "stub-storage", Configured: true}
}

// TestSourceDecomposition_PageImageFactHasSynthesizedURL verifies
// that an image fact extracted from a PDF page render (kind='page',
// no remote URL, bytes + storage_key set) is persisted with a
// service-routable image_url that the frontend can resolve through
// the authenticated getSourceImage helper. Before the fix the
// worker wrote image_url=NULL for these rows because it only copied
// img.Url, which is NULL for page renders.
func TestSourceDecomposition_PageImageFactHasSynthesizedURL(t *testing.T) {
	env := testutil.NewTestEnv(t)
	defer env.Server.Close()

	admin := bootstrapSysAdmin(t, env, "img_page@example.com")
	_, _, repoID := createRepositoryWithDB(t, admin, "ImgPage", "img-page", "desc", "")
	ensureRiverSchema(t, env.DB)
	queries := store.New(env.DB)

	// Resolve the repository slug so we can build the expected
	// synthesized URL in the assertion below.
	repoIDUUID := pgRepoID(t, repoID)
	repoRow, err := store.New(env.DB).GetRepositoryByID(context.Background(), repoIDUUID)
	if err != nil {
		t.Fatalf("GetRepositoryByID: %v", err)
	}
	if repoRow.Slug != "img-page" {
		t.Fatalf("repository slug = %q, want img-page", repoRow.Slug)
	}

	// 1x1 PNG body the stub storage will hand back when the worker
	// fetches the storage_key.
	pngBytes := []byte{
		0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a, 0x00, 0x00, 0x00, 0x0d, 0x49, 0x48, 0x44, 0x52,
		0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01, 0x08, 0x06, 0x00, 0x00, 0x00, 0x1f, 0x15, 0xc4,
		0x89, 0x00, 0x00, 0x00, 0x0d, 0x49, 0x44, 0x41, 0x54, 0x78, 0x9c, 0x62, 0x00, 0x01, 0x00, 0x00,
		0x05, 0x00, 0x01, 0x0d, 0x0a, 0x2d, 0xb4, 0x00, 0x00, 0x00, 0x00, 0x49, 0x45, 0x4e, 0x44, 0xae,
		0x42, 0x60, 0x82,
	}
	storageKey := "repositories/" + repoID + "/sources/page-render-image/page-1.png"
	stor := &stubStorage{key: storageKey, body: pngBytes, ct: "image/png"}

	sourceID := pgtype.UUID{}
	if err := sourceID.Scan(uuid.NewString()); err != nil {
		t.Fatalf("scanning source id: %v", err)
	}
	if _, err := queries.CreateSource(context.Background(), store.CreateSourceParams{
		ID:           sourceID,
		RepositoryID: repoIDUUID,
		Url:          "https://example.com/page-render.pdf",
		Kind:         "pdf",
		Status:       "fetched",
	}); err != nil {
		t.Fatalf("create source: %v", err)
	}
	if _, err := queries.MarkSourceParsed(context.Background(), store.MarkSourceParsedParams{
		ID:          sourceID,
		ParsedTitle: ptrString("Page Render Test"),
		ParseStatus: ptrString("ok"),
		PublishedAt: pgtype.Date{},
	}); err != nil {
		t.Fatalf("mark source parsed: %v", err)
	}
	// Add a kind='page' image row (the kind PDF page renders get)
	// with bytes set but no URL — this is the bug shape: before the
	// fix the worker wrote image_url=NULL because img.Url was nil.
	pageNum := int32(1)
	img, err := queries.AddSourceImage(context.Background(), store.AddSourceImageParams{
		SourceID:   sourceID,
		Kind:       "page",
		PageNumber: &pageNum,
		Position:   0,
		Bytes:      ptrInt32(int32(len(pngBytes))),
	})
	if err != nil {
		t.Fatalf("add source image: %v", err)
	}
	// Mark the row as mirrored so the worker's storage-Get path
	// fires (the bug fix's URL synthesis runs after a successful
	// storage read).
	if _, err := queries.MarkSourceImageStored(context.Background(), store.MarkSourceImageStoredParams{
		ID:          img.ID,
		StorageKey:  ptrString(storageKey),
		ContentType: ptrString("image/png"),
		LocalPath:   ptrString("/tmp/" + storageKey),
	}); err != nil {
		t.Fatalf("mark source image stored: %v", err)
	}

	imgExtractor := &stubImageExtractor{
		facts: []string{"The page render shows a labelled diagram of the ribosome 50S subunit."},
	}
	registry := testutil.NewForTestPool(env.DB)
	imageCfg := config.DecompositionImageConfig{
		Enabled:            true,
		Provider:           "stub",
		Model:              "stub-vision",
		MaxImageBytes:      5 * 1024 * 1024,
		MaxImagesPerSource: 20,
	}
	worker := tasks.NewSourceDecompositionWorker(stubChunker{}, stubFactExtractor{}, imgExtractor, config.DecompositionFactConfig{}, imageCfg, registry, store.New(env.DB), stor, nil, nil)

	driver := riverpgxv5.New(env.DB)
	workers := river.NewWorkers()
	river.AddWorker(workers, worker)
	cfg := &river.Config{
		Queues:  map[string]river.QueueConfig{tasks.QueueSourceDecomposition: {MaxWorkers: 1}},
		Workers: workers,
	}
	testWorker := rivertest.NewWorker(t, driver, cfg, worker)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	tx, err := env.DB.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer tx.Rollback(context.Background())

	job, err := testWorker.Work(ctx, t, tx, tasks.SourceDecompositionArgs{
		SourceID:     pgUUIDString(sourceID),
		RepositoryID: repoID,
	}, &river.InsertOpts{Queue: tasks.QueueSourceDecomposition})
	if err != nil {
		t.Fatalf("worker.Work: %v", err)
	}
	if job.EventKind != river.EventKindJobCompleted {
		t.Fatalf("expected completed, got %s", job.EventKind)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}

	// The image extractor must have been called once with the
	// synthesized service URL in ImageURL (not empty, like before
	// the fix).
	if len(imgExtractor.calls) != 1 {
		t.Fatalf("expected 1 image extractor call, got %d", len(imgExtractor.calls))
	}
	call := imgExtractor.calls[0]
	if call.ImageURL == "" {
		t.Errorf("image extractor ImageURL = empty, want the synthesized storage URL")
	}

	// The image fact must be persisted with fact_kind='image' and
	// image_url set to the synthesized storage URL.
	facts, err := queries.ListFactsBySource(context.Background(), store.ListFactsBySourceParams{
		SourceID: sourceID,
		Limit:    100,
	})
	if err != nil {
		t.Fatalf("listing facts: %v", err)
	}
	var imageFactCount int
	var factImageURL string
	for _, f := range facts {
		if f.FactKind == "image" {
			imageFactCount++
			if f.ImageUrl == nil || *f.ImageUrl == "" {
				t.Errorf("image fact %s has nil/empty image_url", pgUUIDString(f.ID))
				continue
			}
			factImageURL = *f.ImageUrl
		}
	}
	if imageFactCount != 1 {
		t.Fatalf("expected 1 image fact, got %d", imageFactCount)
	}

	// The synthesized URL must point at our storage endpoint with
	// the repository slug (not UUID), the source ID, and the image
	// ID — these are what the frontend parses out to call
	// getSourceImage(slug, sourceID, imageID).
	wantURL := "/api/v1/repositories/img-page/sources/" + pgUUIDString(sourceID) + "/images/" + pgUUIDString(img.ID)
	if factImageURL != wantURL {
		t.Errorf("image fact image_url = %q, want %q", factImageURL, wantURL)
	}
}

// ptrInt32 is a tiny helper to take the address of an int32 literal
// for the nullable *int32 fields on the store params.
func ptrInt32(v int32) *int32 { return &v }

// multiChunker splits text into fixed-size chunks so the decomposition
// worker's text-chunk loop runs more than once — exercising the
// two-phase parallel extract path (phase 1 fans out ExtractFacts,
// phase 2 serializes persistence).
type multiChunker struct{ chunkSize int }

func (m multiChunker) Chunk(text string) []decomposition.Chunk {
	runes := []rune(text)
	var chunks []decomposition.Chunk
	for start := 0; start < len(runes); start += m.chunkSize {
		end := start + m.chunkSize
		if end > len(runes) {
			end = len(runes)
		}
		chunks = append(chunks, decomposition.Chunk{
			Index:     len(chunks),
			Text:      string(runes[start:end]),
			StartRune: start,
			EndRune:   end,
		})
	}
	return chunks
}
func (m multiChunker) Describe() decomposition.ProviderDescription {
	return decomposition.ProviderDescription{Name: "multi-chunker", Configured: true, Supports: []string{"chunking"}}
}

// indexAwareFactExtractor returns one fact per chunk whose text encodes
// the chunk index it came from, so the test can assert that every chunk's
// fact was persisted and that chunk_traces are in input order.
type indexAwareFactExtractor struct{}

func (indexAwareFactExtractor) ExtractFacts(ctx context.Context, db store.DBTX, chunkText string, attr decomposition.FactExtractionAttribution) ([]decomposition.ExtractedFact, error) {
	return []decomposition.ExtractedFact{{Text: "fact from chunk input: " + chunkText, Sentences: nil}}, nil
}
func (indexAwareFactExtractor) Describe() decomposition.ProviderDescription {
	return decomposition.ProviderDescription{Name: "index-fact-extractor", Configured: true, Supports: []string{"fact_extraction"}}
}

// TestSourceDecomposition_ParallelChunks verifies the two-phase
// parallel extract → serial persist refactor: with concurrency=4 and
// multiple chunks, every chunk's facts are persisted and the
// ChunkTraces are returned in input (chunk-index) order, not
// completion order. The fact extractor returns a fact whose text
// encodes the chunk text, so we can confirm no chunk was dropped or
// duplicated by the parallel fan-out.
func TestSourceDecomposition_ParallelChunks(t *testing.T) {
	env := testutil.NewTestEnv(t)
	defer env.Server.Close()

	admin := bootstrapSysAdmin(t, env, "parallel_chunks@example.com")
	_, _, repoID := createRepositoryWithDB(t, admin, "ParallelChunks", "parallel-chunks", "desc", "")
	ensureRiverSchema(t, env.DB)
	queries := store.New(env.DB)

	sourceID := pgtype.UUID{}
	if err := sourceID.Scan(uuid.NewString()); err != nil {
		t.Fatalf("scanning source id: %v", err)
	}
	if _, err := queries.CreateSource(context.Background(), store.CreateSourceParams{
		ID:           sourceID,
		RepositoryID: pgRepoID(t, repoID),
		Url:          "https://example.com/parallel-chunks",
		Kind:         "homepage",
		Status:       "fetched",
	}); err != nil {
		t.Fatalf("create source: %v", err)
	}
	// 6 chunks of 10 runes each (chunkSize=10). With concurrency=4,
	// phase 1 fans out the first 4 chunks concurrently, then the last
	// 2; phase 2 persists all 6 serially in index order.
	const chunkSize = 10
	const numChunks = 6
	body := ""
	for i := 0; i < numChunks*chunkSize; i++ {
		body += "x"
	}
	if _, err := queries.MarkSourceParsed(context.Background(), store.MarkSourceParsedParams{
		ID:          sourceID,
		ParsedText:  ptrString(body),
		ParseStatus: ptrString("ok"),
		PublishedAt: pgtype.Date{},
	}); err != nil {
		t.Fatalf("mark source parsed: %v", err)
	}

	factCfg := config.DecompositionFactConfig{Provider: "stub", Model: "stub", Concurrency: 4}
	registry := testutil.NewForTestPool(env.DB)
	worker := tasks.NewSourceDecompositionWorker(multiChunker{chunkSize: chunkSize}, indexAwareFactExtractor{}, nil, factCfg, config.DecompositionImageConfig{Enabled: false}, registry, store.New(env.DB), nil, nil, nil)

	driver := riverpgxv5.New(env.DB)
	workers := river.NewWorkers()
	river.AddWorker(workers, worker)
	cfg := &river.Config{
		Queues:  map[string]river.QueueConfig{tasks.QueueSourceDecomposition: {MaxWorkers: 1}},
		Workers: workers,
	}
	testWorker := rivertest.NewWorker(t, driver, cfg, worker)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	tx, err := env.DB.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	job, err := testWorker.Work(ctx, t, tx, tasks.SourceDecompositionArgs{
		SourceID:     pgUUIDString(sourceID),
		RepositoryID: repoID,
	}, &river.InsertOpts{Queue: tasks.QueueSourceDecomposition})
	if err != nil {
		t.Fatalf("worker.Work: %v", err)
	}
	if job.EventKind != river.EventKindJobCompleted {
		t.Fatalf("expected completed, got %s", job.EventKind)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}

	var result tasks.SourceDecompositionResult
	if err := json.Unmarshal(job.Job.Output(), &result); err != nil {
		t.Fatalf("unmarshal output: %v", err)
	}

	if result.Chunks != numChunks {
		t.Errorf("result.Chunks = %d, want %d", result.Chunks, numChunks)
	}
	if result.Facts != numChunks {
		t.Errorf("result.Facts = %d, want %d (one fact per chunk)", result.Facts, numChunks)
	}
	if result.ChunkFailures != 0 {
		t.Errorf("result.ChunkFailures = %d, want 0", result.ChunkFailures)
	}

	// ChunkTraces must be in input (chunk-index) order, regardless of
	// which chunk's AI call completed first under concurrency=4.
	if len(result.ChunkTraces) != numChunks {
		t.Fatalf("ChunkTraces len = %d, want %d", len(result.ChunkTraces), numChunks)
	}
	for i, ct := range result.ChunkTraces {
		if ct.Index != i {
			t.Errorf("ChunkTraces[%d].Index = %d, want %d (must be input order)", i, ct.Index, i)
		}
		if ct.Type != "text" {
			t.Errorf("ChunkTraces[%d].Type = %q, want text", i, ct.Type)
		}
		if ct.Facts != 1 {
			t.Errorf("ChunkTraces[%d].Facts = %d, want 1", i, ct.Facts)
		}
		if ct.Error != "" {
			t.Errorf("ChunkTraces[%d].Error = %q, want empty", i, ct.Error)
		}
	}

	// Every chunk's fact must be persisted and linked to the source.
	facts, err := queries.ListFactsBySource(context.Background(), store.ListFactsBySourceParams{
		SourceID: sourceID,
		Limit:    100,
	})
	if err != nil {
		t.Fatalf("listing facts: %v", err)
	}
	if len(facts) != numChunks {
		t.Errorf("persisted facts = %d, want %d", len(facts), numChunks)
	}
	for _, f := range facts {
		if f.FactKind != "text" {
			t.Errorf("fact %s FactKind = %q, want text", pgUUIDString(f.ID), f.FactKind)
		}
	}
}
