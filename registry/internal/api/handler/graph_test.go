package handler

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/openktree/knowledge-registry/internal/auth"
	"github.com/openktree/knowledge-registry/internal/model"
	"github.com/openktree/knowledge-registry/internal/service"
	"github.com/openktree/knowledge-registry/internal/store"
)

// graphTestEnv wires a GraphHandler against an in-memory sqlite store
// + a recording storage mock, the minimum needed to exercise the Push
// handler's header-based metadata + storage-stream path end to end.
type graphTestEnv struct {
	t       *testing.T
	handler *GraphHandler
	storage *recordingStorage
	rec     *httptest.ResponseRecorder
	store   store.MetadataStore
}

func newGraphTestEnv(t *testing.T) *graphTestEnv {
	t.Helper()
	s, err := store.NewSQLiteStore("file::memory:?cache=shared&_pragma=busy_timeout=5000")
	if err != nil {
		t.Fatalf("creating sqlite: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	if err := s.CreateRepository(context.Background(), &model.Repository{
		ID:        "default",
		Name:      "Test",
		CreatedAt: time.Now().UTC(),
		UpdatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("creating default repo: %v", err)
	}
	rs := &recordingStorage{}
	reg := service.New(s, rs, 3600, 0, 0)
	return &graphTestEnv{
		t:       t,
		handler: NewGraphHandler(reg),
		storage: rs,
		store:   s,
	}
}

// pushBundle issues a POST /api/v1/graphs with the given headers + body
// bytes and returns the recorded response. headers may be nil.
func (e *graphTestEnv) pushBundle(headers map[string]string, body []byte) *httptest.ResponseRecorder {
	e.t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/graphs", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/gzip")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	// Pretend to be an authenticated user with an email so the owner
	// field gets populated (mirrors the real middleware).
	ctx := auth.WithUserEmail(req.Context(), "tester@example.com")
	req = req.WithContext(ctx)
	e.rec = httptest.NewRecorder()
	e.handler.Push(e.rec, req)
	return e.rec
}

// makeGzipBody returns a gzipped body of the given byte size (random-ish
// repeated content), used to simulate a graph bundle payload without
// needing a real bundle. The registry treats the body as opaque.
func makeGzipBody(t *testing.T, size int) []byte {
	t.Helper()
	payload := bytes.Repeat([]byte("graph-bundle-payload-"), size/20+1)
	payload = payload[:size]
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	if _, err := gz.Write(payload); err != nil {
		t.Fatalf("gzipping body: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("closing gzip: %v", err)
	}
	return buf.Bytes()
}

// recordingStorage is a minimal Storage double that records the bytes
// written via StoreStream so a test can assert storage received the
// complete body.
type recordingStorage struct {
	mu       sync.Mutex
	key      string
	data     []byte
	mimeType string
}

func (r *recordingStorage) Store(_ context.Context, key string, data []byte, contentType string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.key = key
	r.data = append(r.data[:0], data...)
	r.mimeType = contentType
	return nil
}

func (r *recordingStorage) StoreStream(_ context.Context, key string, body io.Reader, contentType string) (int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.key = key
	r.mimeType = contentType
	n, err := io.ReadAll(body)
	r.data = append(r.data[:0], n...)
	return int64(len(n)), err
}

func (r *recordingStorage) Delete(_ context.Context, key string) error { return nil }
func (r *recordingStorage) StoreJSON(_ context.Context, key string, data []byte) error {
	return r.Store(context.Background(), key, data, "application/json")
}
func (r *recordingStorage) ReadAll(_ context.Context, key string) ([]byte, string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.key == key {
		return r.data, r.mimeType, nil
	}
	return nil, "", nil
}
func (r *recordingStorage) PresignedURL(_ context.Context, key string, _ time.Duration) (string, error) {
	return "http://mock/" + key, nil
}
func (r *recordingStorage) PresignedPUTURL(_ context.Context, key string, _ time.Duration) (string, error) {
	return "http://mock/" + key, nil
}

var _ service.Storage = (*recordingStorage)(nil)

// TestGraphPush_SmallBundle verifies the header-based metadata path:
// X-Graph-Name + a small gzip body → 201, storage receives the body
// bytes verbatim.
func TestGraphPush_SmallBundle(t *testing.T) {
	env := newGraphTestEnv(t)
	body := makeGzipBody(t, 1<<10) // 1 KB
	rec := env.pushBundle(map[string]string{
		"X-Graph-Name":        "Small Graph",
		"X-Graph-Description": "tiny",
		"X-Graph-Source-Count": "2",
	}, body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
	var result model.GraphPushResult
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if result.GraphID == "" {
		t.Errorf("expected non-empty graph_id, got %q", result.GraphID)
	}
	// Storage must have received the exact body bytes.
	if !bytes.Equal(env.storage.data, body) {
		t.Errorf("storage bytes != request bytes (stored=%d, sent=%d)", len(env.storage.data), len(body))
	}
	// The indexed metadata should reflect the headers.
	meta, err := env.store.GetGraph(context.Background(), result.GraphID)
	if err != nil {
		t.Fatalf("GetGraph: %v", err)
	}
	if meta.Name != "Small Graph" {
		t.Errorf("expected name %q, got %q", "Small Graph", meta.Name)
	}
	if meta.Description != "tiny" {
		t.Errorf("expected description %q, got %q", "tiny", meta.Description)
	}
	if meta.SourceCount != 2 {
		t.Errorf("expected source_count 2, got %d", meta.SourceCount)
	}
}

// TestGraphPush_BodyStreamedUnmodifiedToStorage is the core invariant
// test: a large body (well past the old 16 MB backstop) with a small
// metadata header set must succeed, and storage must receive the body
// bytes byte-for-byte. This is the prod case that the previous
// body-decode path failed on.
func TestGraphPush_BodyStreamedUnmodifiedToStorage(t *testing.T) {
	env := newGraphTestEnv(t)
	body := makeGzipBody(t, 20<<20) // 20 MB
	rec := env.pushBundle(map[string]string{
		"X-Graph-Name": "Big Bundle",
	}, body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201 for big bundle, got %d: %s", rec.Code, rec.Body.String())
	}
	if !bytes.Equal(env.storage.data, body) {
		t.Errorf("storage bytes != request bytes (stored=%d, sent=%d)", len(env.storage.data), len(body))
	}
}

// TestGraphPush_MissingNameHeader verifies the handler rejects a
// request without X-Graph-Name (the required header).
func TestGraphPush_MissingNameHeader(t *testing.T) {
	env := newGraphTestEnv(t)
	body := makeGzipBody(t, 64)
	rec := env.pushBundle(nil, body)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for missing X-Graph-Name, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestGraphPush_TagsHeader verifies X-Graph-Tags is parsed as a JSON
// array and indexed.
func TestGraphPush_TagsHeader(t *testing.T) {
	env := newGraphTestEnv(t)
	body := makeGzipBody(t, 64)
	rec := env.pushBundle(map[string]string{
		"X-Graph-Name": "Tagged Graph",
		"X-Graph-Tags": `["alpha","beta"]`,
	}, body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
	var result model.GraphPushResult
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	meta, err := env.store.GetGraph(context.Background(), result.GraphID)
	if err != nil {
		t.Fatalf("GetGraph: %v", err)
	}
	if len(meta.Tags) != 2 || meta.Tags[0] != "alpha" || meta.Tags[1] != "beta" {
		t.Errorf("expected tags [alpha beta], got %v", meta.Tags)
	}
}

// TestGraphPush_TagsHeaderInvalid verifies a malformed X-Graph-Tags
// header is rejected with 400.
func TestGraphPush_TagsHeaderInvalid(t *testing.T) {
	env := newGraphTestEnv(t)
	body := makeGzipBody(t, 64)
	rec := env.pushBundle(map[string]string{
		"X-Graph-Name": "Bad Tags",
		"X-Graph-Tags": `not-json`,
	}, body)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for invalid X-Graph-Tags, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestGraphPush_OwnerFromAuth verifies the handler populates the
// indexed graph's owner from the authenticated user's email rather
// than the X-Graph-Owner header.
func TestGraphPush_OwnerFromAuth(t *testing.T) {
	env := newGraphTestEnv(t)
	body := makeGzipBody(t, 64)
	rec := env.pushBundle(map[string]string{
		"X-Graph-Name":  "Owner Test",
		"X-Graph-Owner": "bundle-declared@example.com",
	}, body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
	var result model.GraphPushResult
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	meta, err := env.store.GetGraph(context.Background(), result.GraphID)
	if err != nil {
		t.Fatalf("GetGraph: %v", err)
	}
	if meta.Owner != "tester@example.com" {
		t.Errorf("expected owner from auth email, got %q", meta.Owner)
	}
}

// TestGraphPush_SHA256AndSchemaVersion verifies the optional numeric
// + string headers flow through to the indexed metadata.
func TestGraphPush_SHA256AndSchemaVersion(t *testing.T) {
	env := newGraphTestEnv(t)
	body := makeGzipBody(t, 64)
	rec := env.pushBundle(map[string]string{
		"X-Graph-Name":            "SHA Test",
		"X-Graph-SHA256":          "deadbeef",
		"X-Graph-Schema-Version":  "1",
		"X-Graph-Fact-Count":      "42",
		"X-Graph-Concept-Count":   "7",
	}, body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
	var result model.GraphPushResult
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	meta, err := env.store.GetGraph(context.Background(), result.GraphID)
	if err != nil {
		t.Fatalf("GetGraph: %v", err)
	}
	if meta.SHA256 != "deadbeef" {
		t.Errorf("expected sha256 deadbeef, got %q", meta.SHA256)
	}
	if meta.SchemaVersion != 1 {
		t.Errorf("expected schema_version 1, got %d", meta.SchemaVersion)
	}
	if meta.FactCount != 42 {
		t.Errorf("expected fact_count 42, got %d", meta.FactCount)
	}
	if meta.ConceptCount != 7 {
		t.Errorf("expected concept_count 7, got %d", meta.ConceptCount)
	}
}

// TestGraphPush_BigDescription verifies a large X-Graph-Description
// header (near the 2 MB MaxHeaderBytes budget) is accepted. The
// previous body-decode path tripped on a >256 KB description; the
// header path has no such limit below MaxHeaderBytes.
func TestGraphPush_BigDescription(t *testing.T) {
	env := newGraphTestEnv(t)
	body := makeGzipBody(t, 64)
	big := strings.Repeat("this is a long description. ", 40_000) // ~1.1 MB
	rec := env.pushBundle(map[string]string{
		"X-Graph-Name":        "Big Description",
		"X-Graph-Description": big,
	}, body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201 for big description, got %d: %s", rec.Code, rec.Body.String())
	}
	var result model.GraphPushResult
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	meta, err := env.store.GetGraph(context.Background(), result.GraphID)
	if err != nil {
		t.Fatalf("GetGraph: %v", err)
	}
	if len(meta.Description) != len(big) {
		t.Errorf("expected description length %d, got %d", len(big), len(meta.Description))
	}
}