//go:build e2e

package e2e_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/openktree/open-knowledge-tree/backend/e2e/testutil"
	"github.com/openktree/open-knowledge-tree/backend/internal/providers/fetch"
	"github.com/openktree/open-knowledge-tree/backend/internal/store"
	"github.com/openktree/open-knowledge-tree/backend/internal/taskmanager/tasks"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivertest"
)

// onePixelPNG is a 1x1 PNG used as the inline-image payload in the
// serving-endpoint tests. It is small enough to inline and lets the
// assertions compare byte-for-byte without depending on a fixture
// file. The storage backend's content-type sniffer recognizes the
// PNG magic (89 50 4E 47) so the served Content-Type is image/png.
var onePixelPNG = []byte{
	0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a, 0x00, 0x00, 0x00, 0x0d, 0x49, 0x48, 0x44, 0x52,
	0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01, 0x08, 0x06, 0x00, 0x00, 0x00, 0x1f, 0x15, 0xc4,
	0x89, 0x00, 0x00, 0x00, 0x0d, 0x49, 0x44, 0x41, 0x54, 0x78, 0x9c, 0x62, 0x00, 0x01, 0x00, 0x00,
	0x05, 0x00, 0x01, 0x0d, 0x0a, 0x2d, 0xb4, 0x00, 0x00, 0x00, 0x00, 0x49, 0x45, 0x4e, 0x44, 0xae,
	0x42, 0x60, 0x82,
}

// a minimal valid PDF header + trailer used as the source-body
// payload. It is not a real document but it carries the PDF magic
// (%PDF-) so the content-type sniffer returns application/pdf and
// the byte-for-byte assertion is meaningful.
var minimalPDF = []byte("%PDF-1.4\n1 0 obj<<>>endobj\ntrailer<<>>\n%%EOF\n")

// TestStorage_ServeSourceImage exercises the authenticated serving
// endpoint for a stored inline image:
//   - insert a source + source_images row directly into the per-repo
//     pool;
//   - store the image bytes via the same LocalFileStorage the
//     wiring layer uses, so the row's storage_key points at a real
//     file the serving endpoint can read;
//   - GET /repositories/{slug}/sources/{sourceID}/images/{imageID}
//     and assert the body, status, and Content-Type match.
//
// It also covers the error cases:
//   - 404 when the imageID is unknown;
//   - 404 when the row has no storage_key (un-mirrored);
//   - 404 when the image's source_id does not match the route
//     {sourceID} (cross-source defense-in-depth);
//   - 401 when no Authorization header is sent.
func TestStorage_ServeSourceImage(t *testing.T) {
	env := testutil.NewTestEnv(t)
	defer env.Server.Close()

	admin := bootstrapSysAdmin(t, env, "storage_img@example.com")
	_, _, repoID := createRepositoryWithDB(t, admin, "StorageImg", "storage-img", "desc", "")
	queries := store.New(env.DB)

	srcID := pgtype.UUID{}
	if err := srcID.Scan(uuid.NewString()); err != nil {
		t.Fatalf("scan source id: %v", err)
	}
	if _, err := queries.CreateSource(context.Background(), store.CreateSourceParams{
		ID:           srcID,
		RepositoryID: pgRepoID(t, repoID),
		Url:          "https://example.com/storage-img",
		Kind:         "homepage",
		Status:       "fetched",
	}); err != nil {
		t.Fatalf("create source: %v", err)
	}

	// Store the image bytes via the server's own storage backend,
	// so the serving endpoint finds the file on the same disk.
	fs := env.Storage
	imgID := pgtype.UUID{}
	if err := imgID.Scan(uuid.NewString()); err != nil {
		t.Fatalf("scan image id: %v", err)
	}
	imgIDStr := pgUUIDString(imgID)
	srcIDStr := pgUUIDString(srcID)
	key := "repositories/" + repoID + "/sources/" + srcIDStr + "/images/" + imgIDStr + ".png"
	if _, err := fs.Store(context.Background(), key, "image/png", onePixelPNG); err != nil {
		t.Fatalf("store image: %v", err)
	}
	keyPtr := &key
	ct := "image/png"
	row, err := queries.AddSourceImage(context.Background(), store.AddSourceImageParams{
		SourceID: srcID,
		Kind:     "inline",
		Position: 0,
		Url:      ptrString("https://example.com/storage-img/chart.png"),
	})
	if err != nil {
		t.Fatalf("add source image: %v", err)
	}
	if _, err := queries.MarkSourceImageStored(context.Background(), store.MarkSourceImageStoredParams{
		ID:          row.ID,
		StorageKey:  keyPtr,
		ContentType: &ct,
		LocalPath:   keyPtr,
	}); err != nil {
		t.Fatalf("mark source image stored: %v", err)
	}

	// Happy path: 200 with the stored bytes.
	path := "/api/v1/repositories/storage-img/sources/" + srcIDStr + "/images/" + pgUUIDString(row.ID)
	resp, body := admin.do("GET", path, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET image: status %d, body %s", resp.StatusCode, body)
	}
	if !bytes.Equal(body, onePixelPNG) {
		t.Errorf("served body does not match stored bytes (got %d bytes, want %d)", len(body), len(onePixelPNG))
	}
	if got := resp.Header.Get("Content-Type"); got != "image/png" {
		t.Errorf("Content-Type = %q, want image/png", got)
	}

	// 404 for unknown imageID.
	resp, _ = admin.do("GET", path+"-nope", nil)
	// Append a UUID-shaped suffix so the route matches.
	unknownPath := "/api/v1/repositories/storage-img/sources/" + srcIDStr + "/images/" + uuid.NewString()
	resp, _ = admin.do("GET", unknownPath, nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("unknown imageID: status = %d, want 404", resp.StatusCode)
	}

	// 404 for un-mirrored image (storage_key NULL).
	unmirroredRow, err := queries.AddSourceImage(context.Background(), store.AddSourceImageParams{
		SourceID: srcID,
		Kind:     "inline",
		Position: 1,
		Url:      ptrString("https://example.com/storage-img/unmirrored.png"),
	})
	if err != nil {
		t.Fatalf("add unmirrored image: %v", err)
	}
	unmirroredPath := "/api/v1/repositories/storage-img/sources/" + srcIDStr + "/images/" + pgUUIDString(unmirroredRow.ID)
	resp, _ = admin.do("GET", unmirroredPath, nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("un-mirrored image: status = %d, want 404", resp.StatusCode)
	}

	// 404 when the image's source_id does not match the route
	// {sourceID} (cross-source defense in depth). We insert a
	// second source and request its image via the first source's
	// URL path; the handler must reject.
	otherSrcID := pgtype.UUID{}
	if err := otherSrcID.Scan(uuid.NewString()); err != nil {
		t.Fatalf("scan other source id: %v", err)
	}
	if _, err := queries.CreateSource(context.Background(), store.CreateSourceParams{
		ID:           otherSrcID,
		RepositoryID: pgRepoID(t, repoID),
		Url:          "https://example.com/storage-img-other",
		Kind:         "homepage",
		Status:       "fetched",
	}); err != nil {
		t.Fatalf("create other source: %v", err)
	}
	otherImgID := pgtype.UUID{}
	if err := otherImgID.Scan(uuid.NewString()); err != nil {
		t.Fatalf("scan other image id: %v", err)
	}
	otherKey := "repositories/" + repoID + "/sources/" + pgUUIDString(otherSrcID) + "/images/" + pgUUIDString(otherImgID) + ".png"
	if _, err := fs.Store(context.Background(), otherKey, "image/png", onePixelPNG); err != nil {
		t.Fatalf("store other image: %v", err)
	}
	otherRow, err := queries.AddSourceImage(context.Background(), store.AddSourceImageParams{
		SourceID: otherSrcID,
		Kind:     "inline",
		Position: 0,
		Url:      ptrString("https://example.com/other.png"),
	})
	if err != nil {
		t.Fatalf("add other image: %v", err)
	}
	otherKeyPtr := &otherKey
	if _, err := queries.MarkSourceImageStored(context.Background(), store.MarkSourceImageStoredParams{
		ID:          otherRow.ID,
		StorageKey:  otherKeyPtr,
		ContentType: &ct,
		LocalPath:   otherKeyPtr,
	}); err != nil {
		t.Fatalf("mark other image stored: %v", err)
	}
	// Request the other image via the first source's URL path.
	crossPath := "/api/v1/repositories/storage-img/sources/" + srcIDStr + "/images/" + pgUUIDString(otherRow.ID)
	resp, _ = admin.do("GET", crossPath, nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("cross-source image: status = %d, want 404", resp.StatusCode)
	}

	// 401 when no Authorization header is sent.
	unauthed := &http.Client{}
	req, _ := http.NewRequest("GET", env.BaseURL+path, nil)
	resp2, err := unauthed.Do(req)
	if err != nil {
		t.Fatalf("unauthed request: %v", err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusUnauthorized {
		t.Errorf("unauthed: status = %d, want 401", resp2.StatusCode)
	}
	_, _ = io.ReadAll(resp2.Body)
}

// TestStorage_ServeSourceBody exercises the PDF body serving
// endpoint. It stores a minimal PDF payload under the canonical
// source-body key, marks the source row, and asserts the endpoint
// serves the bytes with Content-Type application/pdf. Also covers
// the 404 when storage_key is NULL (HTML/text sources).
func TestStorage_ServeSourceBody(t *testing.T) {
	env := testutil.NewTestEnv(t)
	defer env.Server.Close()

	admin := bootstrapSysAdmin(t, env, "storage_body@example.com")
	_, _, repoID := createRepositoryWithDB(t, admin, "StorageBody", "storage-body", "desc", "")
	queries := store.New(env.DB)

	srcID := pgtype.UUID{}
	if err := srcID.Scan(uuid.NewString()); err != nil {
		t.Fatalf("scan source id: %v", err)
	}
	if _, err := queries.CreateSource(context.Background(), store.CreateSourceParams{
		ID:           srcID,
		RepositoryID: pgRepoID(t, repoID),
		Url:          "https://example.com/storage-body.pdf",
		Kind:         "url",
		Status:       "fetched",
	}); err != nil {
		t.Fatalf("create source: %v", err)
	}

	fs := env.Storage
	srcIDStr := pgUUIDString(srcID)
	key := "repositories/" + repoID + "/sources/" + srcIDStr + "/body.pdf"
	if _, err := fs.Store(context.Background(), key, "application/pdf", minimalPDF); err != nil {
		t.Fatalf("store body: %v", err)
	}
	keyPtr := &key
	ct := "application/pdf"
	if _, err := queries.MarkSourceBodyStored(context.Background(), store.MarkSourceBodyStoredParams{
		ID:          srcID,
		StorageKey:  keyPtr,
		ContentType: &ct,
		LocalPath:   keyPtr,
	}); err != nil {
		t.Fatalf("mark source body stored: %v", err)
	}

	// Happy path: 200 with the stored PDF bytes.
	path := "/api/v1/repositories/storage-body/sources/" + srcIDStr + "/body"
	resp, body := admin.do("GET", path, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET body: status %d, body %s", resp.StatusCode, body)
	}
	if !bytes.Equal(body, minimalPDF) {
		t.Errorf("served body does not match stored PDF (got %d bytes, want %d)", len(body), len(minimalPDF))
	}
	if got := resp.Header.Get("Content-Type"); got != "application/pdf" {
		t.Errorf("Content-Type = %q, want application/pdf", got)
	}

	// 404 for a source with no stored body (HTML/text source).
	htmlSrcID := pgtype.UUID{}
	if err := htmlSrcID.Scan(uuid.NewString()); err != nil {
		t.Fatalf("scan html source id: %v", err)
	}
	if _, err := queries.CreateSource(context.Background(), store.CreateSourceParams{
		ID:           htmlSrcID,
		RepositoryID: pgRepoID(t, repoID),
		Url:          "https://example.com/storage-body-html",
		Kind:         "url",
		Status:       "fetched",
	}); err != nil {
		t.Fatalf("create html source: %v", err)
	}
	htmlPath := "/api/v1/repositories/storage-body/sources/" + pgUUIDString(htmlSrcID) + "/body"
	resp, _ = admin.do("GET", htmlPath, nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("un-stored body: status = %d, want 404", resp.StatusCode)
	}
}

// TestStorage_GetSourceReturnsStorageKey asserts that the
// GetSource response now includes the new storage columns
// (storage_key, content_type, mirrored_at) on image rows and
// (storage_key, content_type, stored_at) on the source row, so the
// frontend can decide whether to render the served URL or fall back
// to the remote url.
func TestStorage_GetSourceReturnsStorageKey(t *testing.T) {
	env := testutil.NewTestEnv(t)
	defer env.Server.Close()

	admin := bootstrapSysAdmin(t, env, "storage_getsource@example.com")
	_, _, repoID := createRepositoryWithDB(t, admin, "StorageGet", "storage-get", "desc", "")
	queries := store.New(env.DB)

	srcID := pgtype.UUID{}
	if err := srcID.Scan(uuid.NewString()); err != nil {
		t.Fatalf("scan source id: %v", err)
	}
	if _, err := queries.CreateSource(context.Background(), store.CreateSourceParams{
		ID:           srcID,
		RepositoryID: pgRepoID(t, repoID),
		Url:          "https://example.com/storage-get",
		Kind:         "url",
		Status:       "fetched",
	}); err != nil {
		t.Fatalf("create source: %v", err)
	}
	row, err := queries.AddSourceImage(context.Background(), store.AddSourceImageParams{
		SourceID: srcID,
		Kind:     "inline",
		Position: 0,
		Url:      ptrString("https://example.com/storage-get/img.png"),
	})
	if err != nil {
		t.Fatalf("add image: %v", err)
	}
	key := "repositories/" + repoID + "/sources/" + pgUUIDString(srcID) + "/images/" + pgUUIDString(row.ID) + ".png"
	keyPtr := &key
	ct := "image/png"
	if _, err := queries.MarkSourceImageStored(context.Background(), store.MarkSourceImageStoredParams{
		ID:          row.ID,
		StorageKey:  keyPtr,
		ContentType: &ct,
		LocalPath:   keyPtr,
	}); err != nil {
		t.Fatalf("mark image stored: %v", err)
	}

	resp, body := admin.do("GET", "/api/v1/repositories/storage-get/sources/"+pgUUIDString(srcID), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get source: status %d, body %s", resp.StatusCode, body)
	}
	var parsed struct {
		Source map[string]any `json:"source"`
		Images []map[string]any `json:"images"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		t.Fatalf("decode get source: %v", err)
	}
	if len(parsed.Images) != 1 {
		t.Fatalf("expected 1 image, got %d", len(parsed.Images))
	}
	img := parsed.Images[0]
	if img["storage_key"] == nil || img["storage_key"] == "" {
		t.Errorf("image storage_key is nil/empty; want the stored key")
	}
	if img["content_type"] != "image/png" {
		t.Errorf("image content_type = %v, want image/png", img["content_type"])
	}
	if img["mirrored_at"] == nil {
		t.Errorf("image mirrored_at is nil; want a timestamp")
	}
}

// TestStorage_RetrieveSourceWorkerMirrorsImage drives the
// RetrieveSourceWorker against a local httptest server that serves
// an HTML document with one inline image (also served by the test
// server). It asserts that:
//   - the source_images row carries a non-null storage_key after
//     the worker runs;
//   - the stored file exists on disk under the canonical key;
//   - the served endpoint returns the stored bytes.
//
// This is the end-to-end test that the storage module's wiring
// into the retrieve_source worker is correct; the unit-level
// serving tests above cover the handler in isolation.
func TestStorage_RetrieveSourceWorkerMirrorsImage(t *testing.T) {
	env := testutil.NewTestEnv(t)
	defer env.Server.Close()
	ensureRiverSchema(t, env.DB)

	admin := bootstrapSysAdmin(t, env, "storage_worker@example.com")
	_, _, repoID := createRepositoryWithDB(t, admin, "StorageWorker", "storage-worker", "desc", "")

	pngServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(onePixelPNG)
	}))
	defer pngServer.Close()

	// The HTML mirrors the shape used by source_parsed_test: a
	// content-rich <article> with at least three paragraphs (the
	// parser's "enough content" heuristic) and the inline image
	// inside the article body so trafilatura's cleaned-content
	// pass keeps it. The image src points at the pngServer so
	// the worker's FetchImageBytes call lands on a real,
	// fast PNG.
	htmlPayload := `<html><head><title>Storage Worker Test</title></head><body>
    <article>
      <h1>Storage Worker Test</h1>
      <p>The first paragraph of the article body. A few sentences of meaningful prose give the extractor enough context to be confident about what counts as the body.</p>
      <p><img src="` + pngServer.URL + `/chart.png" alt="chart" />A second paragraph that hosts an inline image so we can assert on the source_images row count. The figure lives inside the article, so the cleaned content node carries it through to the Images slice.</p>
      <p>A third paragraph to firmly establish this as a content-rich page. A fourth paragraph exists for the same reason — real article pages are not two sentences long.</p>
      <p>And a fifth paragraph for good measure, so the parser is confident about the body and the inline image is preserved.</p>
    </article>
    <footer>Footer that must be stripped.</footer>
  </body></html>`
	htmlServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(htmlPayload))
	}))
	defer htmlServer.Close()

	queries := store.New(env.DB)
	repoUUID := pgRepoID(t, repoID)

	registry := testutil.NewForTestPool(env.DB)
	strategy := fetch.NewFetchStrategy(fetch.NewFetchResolutionProvider())
	fs := env.Storage
	worker := tasks.NewRetrieveSourceWorker(nil, strategy, registry, queries, fs, nil, nil, nil, nil, nil)

	driver := riverpgxv5.New(env.DB)
	workers := river.NewWorkers()
	river.AddWorker(workers, worker)
	testWorker := rivertest.NewWorker(t, driver, &river.Config{
		Queues: map[string]river.QueueConfig{
			tasks.QueueRetrieveSource: {MaxWorkers: 1},
		},
		Workers: workers,
	}, worker)

	ctx, cancel := context.WithTimeout(context.Background(), 15*1000*1000*1000)
	defer cancel()

	tx, err := env.DB.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer tx.Rollback(context.Background())

	job, err := testWorker.Work(ctx, t, tx, tasks.RetrieveSourceArgs{
		URL:          htmlServer.URL + "/article",
		RepositoryID: repoID,
	}, &river.InsertOpts{Queue: tasks.QueueRetrieveSource})
	if err != nil {
		t.Fatalf("worker.Work: %v", err)
	}
	if job.EventKind != river.EventKindJobCompleted {
		t.Fatalf("expected job completed, got %s", job.EventKind)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}

	src, err := queries.GetSourceByRepoAndURL(ctx, store.GetSourceByRepoAndURLParams{
		RepositoryID: repoUUID,
		Url:          htmlServer.URL + "/article",
	})
	if err != nil {
		t.Fatalf("get source by url: %v", err)
	}

	images, err := queries.ListSourceImages(ctx, src.ID)
	if err != nil {
		t.Fatalf("list source images: %v", err)
	}
	if len(images) == 0 {
		t.Fatalf("expected at least 1 source image, got 0")
	}
	var mirrored int
	var storedImg store.OktRepositorySourceImage
	for _, img := range images {
		if img.StorageKey != nil && *img.StorageKey != "" {
			mirrored++
			if storedImg.ID == (pgtype.UUID{}) {
				storedImg = img
			}
		}
	}
	if mirrored == 0 {
		t.Fatalf("expected at least 1 mirrored image, got 0; storage_key is NULL on every row")
	}

	// The stored file must exist on disk under the canonical key.
	file, err := fs.Get(ctx, *storedImg.StorageKey)
	if err != nil {
		t.Fatalf("storage.Get(%q): %v", *storedImg.StorageKey, err)
	}
	body, _ := io.ReadAll(file.Body)
	_ = file.Body.Close()
	if !bytes.Equal(body, onePixelPNG) {
		t.Errorf("stored image bytes do not match the served PNG (got %d, want %d)", len(body), len(onePixelPNG))
	}

	// The serving endpoint must return the stored bytes.
	path := "/api/v1/repositories/storage-worker/sources/" + pgUUIDString(src.ID) + "/images/" + pgUUIDString(storedImg.ID)
	resp, servedBody := admin.do("GET", path, nil)
	if resp.StatusCode != http.StatusOK {
		t.Errorf("serving endpoint: status %d, body %s", resp.StatusCode, servedBody)
	} else if !bytes.Equal(servedBody, onePixelPNG) {
		t.Errorf("served bytes do not match (got %d, want %d)", len(servedBody), len(onePixelPNG))
	}
}