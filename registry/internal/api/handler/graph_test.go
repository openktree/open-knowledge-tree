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
// handler's metadata-decode + storage-stream path end to end.
type graphTestEnv struct {
	t        *testing.T
	handler  *GraphHandler
	storage  *recordingStorage
	rec      *httptest.ResponseRecorder
	store    store.MetadataStore
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

// pushBundle issues a POST /api/v1/graphs with the given gzipped bundle
// bytes and returns the recorded response.
func (e *graphTestEnv) pushBundle(body []byte) *httptest.ResponseRecorder {
	e.t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/graphs", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/gzip")
	// Pretend to be an authenticated user with an email so the owner
	// field gets populated (mirrors the real middleware).
	ctx := auth.WithUserEmail(req.Context(), "tester@example.com")
	req = req.WithContext(ctx)
	e.rec = httptest.NewRecorder()
	e.handler.Push(e.rec, req)
	return e.rec
}

// makeGzipBundle builds a gzipped graph bundle with the given metadata
// and an optional trailing payload (simulating the rest of a real
// bundle: sources, facts, …). The metadata.description is the field
// the caller can bloat to force the metadata envelope past a given
// compressed size.
func makeGzipBundle(t *testing.T, metadata graphBundleEnvelope, trailing []byte) []byte {
	t.Helper()
	// Marshal metadata + trailing as a single JSON object: we emit
	// metadata first, then any extra fields the caller wants, so the
	// decoder's read of schema_version + metadata reflects the real
	// wire order.
	var buf bytes.Buffer
	buf.WriteByte('{')
	first := true
	writeField := func(key string, val any) {
		if !first {
			buf.WriteByte(',')
		}
		first = false
		k, _ := json.Marshal(key)
		buf.Write(k)
		buf.WriteByte(':')
		v, _ := json.Marshal(val)
		buf.Write(v)
	}
	if metadata.SchemaVersion != 0 || true {
		writeField("schema_version", metadata.SchemaVersion)
	}
	writeField("metadata", metadata.Metadata)
	// Append the trailing payload as already-marshalled JSON fields
	// (the caller passes e.g. `"sources":[...]`). If empty, nothing.
	if len(bytes.TrimSpace(trailing)) > 0 {
		buf.WriteByte(',')
		// Trim leading/trailing braces if the caller wrapped them.
		trimmed := bytes.TrimSpace(trailing)
		trimmed = bytes.TrimPrefix(trimmed, []byte("{"))
		trimmed = bytes.TrimSuffix(trimmed, []byte("}"))
		buf.Write(bytes.TrimSpace(trimmed))
	}
	buf.WriteByte('}')

	var gzBuf bytes.Buffer
	gz := gzip.NewWriter(&gzBuf)
	if _, err := gz.Write(buf.Bytes()); err != nil {
		t.Fatalf("gzipping bundle: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("closing gzip: %v", err)
	}
	return gzBuf.Bytes()
}

// recordingStorage is a minimal Storage double that records the bytes
// written via StoreStream so a test can assert storage received the
// complete original gzipped stream.
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

// TestGraphPush_SmallBundle verifies the streaming-decode path handles
// a small bundle (metadata fits in the old 256 KB head buffer) and:
//   - returns 201 with a graph_id;
//   - hands storage the *complete* original gzipped bytes (the test
//     re-gunzips the stored bytes and confirms the trailing payload
//     survives the tee + MultiReader replay intact).
func TestGraphPush_SmallBundle(t *testing.T) {
	env := newGraphTestEnv(t)

	bundle := makeGzipBundle(t, graphBundleEnvelope{
		SchemaVersion: 1,
		Metadata: graphMetaSection{
			Name:        "Small Graph",
			Description: "tiny",
			SourceCount: 2,
		},
	}, []byte(`"sources":[{"idx":0,"url":"http://example.com/a"},{"idx":1,"url":"http://example.com/b"}]`))

	rec := env.pushBundle(bundle)
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

	// Storage must have received the full original gzipped bytes.
	stored := env.storage.data
	if !bytes.Equal(stored, bundle) {
		t.Errorf("storage bytes != request bytes (stored=%d, sent=%d)", len(stored), len(bundle))
	}
	// And those bytes must gunzip to valid JSON containing the
	// trailing "sources" field the handler never decoded.
	gz, err := gzip.NewReader(bytes.NewReader(stored))
	if err != nil {
		t.Fatalf("stored bytes are not valid gzip: %v", err)
	}
	jsonBytes, err := io.ReadAll(gz)
	if err != nil {
		t.Fatalf("gunzipping stored bytes: %v", err)
	}
	if !strings.Contains(string(jsonBytes), `"sources"`) || !strings.Contains(string(jsonBytes), "example.com/a") {
		t.Errorf("stored bundle is missing the trailing payload the handler must replay to storage: %s", jsonBytes)
	}
}

// TestGraphPush_MetadataLargerThan256KB is the regression case for the
// bug that caused the original `bufio: buffer full` 400: the metadata
// envelope (here, a deliberately bloated description) decompresses to
// more than the old 256 KB head buffer could hold. The old grow-loop
// called bufio.Peek(>256KB) on a 256KB-buffered reader and died with
// `bufio: buffer full`. The streaming-decode replacement must accept
// this bundle and return 201.
func TestGraphPush_MetadataLargerThan256KB(t *testing.T) {
	env := newGraphTestEnv(t)

	// ~1 MB of description — comfortably past the old 256 KB cap,
	// well under the 16 MB backstop. Decompressed > 1 MB; the gzip
	// reader pulls only as many compressed bytes as json.Decode needs
	// to consume the metadata object, then stops.
	big := strings.Repeat("this is a long description. ", 40_000) // ~1.1 MB
	bundle := makeGzipBundle(t, graphBundleEnvelope{
		SchemaVersion: 1,
		Metadata: graphMetaSection{
			Name:        "Big Metadata Graph",
			Description: big,
			SourceCount: 1,
		},
	}, []byte(`"sources":[{"idx":0,"url":"http://example.com/big"}]`))

	rec := env.pushBundle(bundle)
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201 for >256KB metadata, got %d: %s", rec.Code, rec.Body.String())
	}

	// Storage still gets the full bundle, bloated description included.
	gz, err := gzip.NewReader(bytes.NewReader(env.storage.data))
	if err != nil {
		t.Fatalf("stored bytes are not valid gzip: %v", err)
	}
	jsonBytes, _ := io.ReadAll(gz)
	if !strings.Contains(string(jsonBytes), "this is a long description") {
		t.Errorf("stored bundle lost the bloated description")
	}
	if !strings.Contains(string(jsonBytes), "example.com/big") {
		t.Errorf("stored bundle lost the trailing sources field")
	}
}

// TestGraphPush_NotGzip verifies the handler rejects a non-gzip body
// with 400 (preserves the old error posture for malformed requests).
func TestGraphPush_NotGzip(t *testing.T) {
	env := newGraphTestEnv(t)
	rec := env.pushBundle([]byte(`{"schema_version":1,"metadata":{"name":"x"}}`))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for non-gzip body, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestGraphPush_MissingName verifies the handler rejects a bundle
// whose metadata.name is empty (the registry indexes by name).
func TestGraphPush_MissingName(t *testing.T) {
	env := newGraphTestEnv(t)
	bundle := makeGzipBundle(t, graphBundleEnvelope{
		SchemaVersion: 1,
		Metadata:      graphMetaSection{Name: ""},
	}, nil)
	rec := env.pushBundle(bundle)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for missing metadata.name, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestGraphPush_OwnerFromAuth verifies the handler populates the
// indexed graph's owner from the authenticated user's email rather
// than the bundle's declared owner.
func TestGraphPush_OwnerFromAuth(t *testing.T) {
	env := newGraphTestEnv(t)
	bundle := makeGzipBundle(t, graphBundleEnvelope{
		SchemaVersion: 1,
		Metadata: graphMetaSection{
			Name:  "Owner Test",
			Owner: "bundle-declared@example.com",
		},
	}, nil)
	rec := env.pushBundle(bundle)
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