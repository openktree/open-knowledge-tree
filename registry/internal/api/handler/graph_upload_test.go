package handler

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/openktree/knowledge-registry/internal/auth"
	"github.com/openktree/knowledge-registry/internal/config"
	"github.com/openktree/knowledge-registry/internal/model"
	"github.com/openktree/knowledge-registry/internal/service"
	"github.com/openktree/knowledge-registry/internal/store"
)

// uploadTestEnv wires a GraphHandler with upload config against the
// in-memory sqlite + recordingStorage pattern from graph_test.go.
type uploadTestEnv struct {
	t       *testing.T
	handler *GraphHandler
	storage *recordingStorage
	store   store.MetadataStore
}

func newUploadTestEnv(t *testing.T) *uploadTestEnv {
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
	reg := service.New(s, rs, 3600, 0, 0, false)
	h := NewGraphHandler(reg)
	h.SetUploadConfig(&config.GraphUploadConfig{
		MaxSizeBytes: 1 << 20, // 1 MB cap for tests
		TempDir:      t.TempDir(),
	})
	return &uploadTestEnv{t: t, handler: h, storage: rs, store: s}
}

// bundleMetaJSON is the wire shape of the bundle's metadata section,
// mirrored here so the handler test can construct test bundles without
// importing the (unexported) service.bundleMetadata struct. Kept in
// sync with service.bundleMetadata + model.GraphMeta fields.
type bundleMetaJSON struct {
	Name          string   `json:"name"`
	Description   string   `json:"description,omitempty"`
	Owner         string   `json:"owner,omitempty"`
	Tags          []string `json:"tags,omitempty"`
	SourceCount   int      `json:"source_count"`
	FactCount     int      `json:"fact_count"`
	ConceptCount  int      `json:"concept_count"`
	SHA256        string   `json:"sha256,omitempty"`
	SchemaVersion int      `json:"schema_version,omitempty"`
}

// makeBundleGzip returns a gzipped graph bundle with the given
// metadata, matching the v2 wire format (schema_version → metadata →
// sources → …). extra is appended raw after metadata.
func makeBundleGzip(t *testing.T, meta bundleMetaJSON, extra string) []byte {
	t.Helper()
	var b strings.Builder
	b.WriteString(`{"schema_version":2,"metadata":`)
	metaJSON, err := json.Marshal(meta)
	if err != nil {
		t.Fatalf("marshaling meta: %v", err)
	}
	b.Write(metaJSON)
	if extra != "" {
		b.WriteString(",")
		b.WriteString(extra)
	}
	b.WriteString("}")
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	if _, err := gz.Write([]byte(b.String())); err != nil {
		t.Fatalf("gzipping: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("closing gzip: %v", err)
	}
	return buf.Bytes()
}

// uploadRequest builds a multipart/form-data request with a "bundle"
// file part + optional text fields.
func uploadRequest(t *testing.T, bundle []byte, fields map[string]string) *http.Request {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for k, v := range fields {
		if err := mw.WriteField(k, v); err != nil {
			t.Fatalf("writing field %s: %v", k, err)
		}
	}
	part, err := mw.CreateFormFile("bundle", "graph.json.gz")
	if err != nil {
		t.Fatalf("creating bundle part: %v", err)
	}
	if _, err := part.Write(bundle); err != nil {
		t.Fatalf("writing bundle: %v", err)
	}
	if err := mw.Close(); err != nil {
		t.Fatalf("closing multipart: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/graphs/upload", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	// Pretend to be an authenticated admin so the owner field gets
	// populated (mirrors the real middleware).
	ctx := auth.WithUserEmail(req.Context(), "admin@example.com")
	ctx = auth.WithUser(ctx, "user-1", "admin")
	req = req.WithContext(ctx)
	return req
}

func TestGraphUpload_HappyPath(t *testing.T) {
	env := newUploadTestEnv(t)
	bundle := makeBundleGzip(t, bundleMetaJSON{
		Name:         "Uploaded Graph",
		Description:  "from a file",
		Tags:         []string{"x", "y"},
		SourceCount:  5,
		FactCount:    30,
		ConceptCount: 4,
		SHA256:       "abc123",
	}, `"sources":[]`)
	req := uploadRequest(t, bundle, nil)
	rec := httptest.NewRecorder()
	env.handler.UploadFromFile(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
	var result model.GraphPushResult
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if result.GraphID == "" {
		t.Fatalf("empty graph_id")
	}
	// Storage received the bundle bytes.
	if !bytes.Equal(env.storage.data, bundle) {
		t.Errorf("storage bytes != bundle (stored=%d, sent=%d)", len(env.storage.data), len(bundle))
	}
	// Indexed metadata reflects the bundle.
	meta, err := env.store.GetGraph(context.Background(), result.GraphID)
	if err != nil {
		t.Fatalf("GetGraph: %v", err)
	}
	if meta.Name != "Uploaded Graph" {
		t.Errorf("name: got %q, want Uploaded Graph", meta.Name)
	}
	if meta.SourceCount != 5 {
		t.Errorf("source_count: got %d, want 5", meta.SourceCount)
	}
	if meta.SHA256 != "abc123" {
		t.Errorf("sha256: got %q, want abc123", meta.SHA256)
	}
	// Owner comes from the authenticated admin's email.
	if meta.Owner != "admin@example.com" {
		t.Errorf("owner: got %q, want admin@example.com", meta.Owner)
	}
}

func TestGraphUpload_NameOverride(t *testing.T) {
	env := newUploadTestEnv(t)
	bundle := makeBundleGzip(t, bundleMetaJSON{
		Name: "Bundle Name",
	}, `"sources":[]`)
	req := uploadRequest(t, bundle, map[string]string{
		"name": "Overridden Name",
	})
	rec := httptest.NewRecorder()
	env.handler.UploadFromFile(rec, req)
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
	if meta.Name != "Overridden Name" {
		t.Errorf("name: got %q, want Overridden Name", meta.Name)
	}
}

func TestGraphUpload_TagsOverride(t *testing.T) {
	env := newUploadTestEnv(t)
	bundle := makeBundleGzip(t, bundleMetaJSON{
		Name: "Tags Test",
		Tags: []string{"bundle-tag"},
	}, `"sources":[]`)
	req := uploadRequest(t, bundle, map[string]string{
		"tags": `["form-a","form-b"]`,
	})
	rec := httptest.NewRecorder()
	env.handler.UploadFromFile(rec, req)
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
	if len(meta.Tags) != 2 || meta.Tags[0] != "form-a" || meta.Tags[1] != "form-b" {
		t.Errorf("tags: got %v, want [form-a form-b]", meta.Tags)
	}
}

func TestGraphUpload_OversizedRejected(t *testing.T) {
	env := newUploadTestEnv(t)
	// Build a gzipped bundle whose compressed size exceeds a 512 B
	// cap. base64 of high-entropy bytes is JSON-safe and defeats
	// gzip's repetition-based compression, so the gzipped bundle
	// stays close to the raw size (~990 B compressed). The streaming
	// path caps the compressed bytes read from the part body, so the
	// cap must be below the compressed size to trigger 413.
	entropy := make([]byte, 4_000)
	for i := range entropy {
		entropy[i] = byte(i*7 + 3) // low repetition → poor gzip ratio
	}
	enc := base64.StdEncoding.EncodeToString(entropy) // ~5.3 KB, JSON-safe
	big := `"sources":[{"idx":0,"url":"` + enc + `","kind":"web","status":"done"}]`
	bundle := makeBundleGzip(t, bundleMetaJSON{Name: "Big"}, big)
	env.handler.uploadCfg.MaxSizeBytes = 1 << 9 // 512 B cap → below the ~990 B bundle → 413
	req := uploadRequest(t, bundle, nil)
	rec := httptest.NewRecorder()
	env.handler.UploadFromFile(rec, req)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("expected 413, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestGraphUpload_MissingBundle(t *testing.T) {
	env := newUploadTestEnv(t)
	// Empty body, no file part.
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	mw.Close()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/graphs/upload", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	rec := httptest.NewRecorder()
	env.handler.UploadFromFile(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for missing bundle, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestGraphUpload_NonGzip(t *testing.T) {
	env := newUploadTestEnv(t)
	// Plain JSON, not gzipped.
	bundle := []byte(`{"schema_version":2,"metadata":{"name":"x"},"sources":[]}`)
	req := uploadRequest(t, bundle, nil)
	rec := httptest.NewRecorder()
	env.handler.UploadFromFile(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for non-gzip, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestGraphUpload_NoMetadata(t *testing.T) {
	env := newUploadTestEnv(t)
	// A valid gzip JSON object with no metadata key.
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	gz.Write([]byte(`{"schema_version":2,"sources":[]}`))
	gz.Close()
	req := uploadRequest(t, buf.Bytes(), nil)
	rec := httptest.NewRecorder()
	env.handler.UploadFromFile(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for missing metadata, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestGraphUpload_UploadConfigNil_503(t *testing.T) {
	s, err := store.NewSQLiteStore("file::memory:?cache=shared&_pragma=busy_timeout=5000")
	if err != nil {
		t.Fatalf("creating sqlite: %v", err)
	}
	defer s.Close()
	if err := s.CreateRepository(context.Background(), &model.Repository{
		ID:        "default",
		Name:      "Test",
		CreatedAt: time.Now().UTC(),
		UpdatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("creating default repo: %v", err)
	}
	rs := &recordingStorage{}
	reg := service.New(s, rs, 3600, 0, 0, false)
	h := NewGraphHandler(reg) // no SetUploadConfig
	req := uploadRequest(t, []byte("x"), nil)
	rec := httptest.NewRecorder()
	h.UploadFromFile(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 when upload config nil, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestGraphUpload_StorageReceivesFullBundle is the memory-safety
// invariant: a bundle with a large sources section must arrive at
// storage byte-for-byte (the spool → storage stream never buffers
// the whole thing in the registry).
func TestGraphUpload_StorageReceivesFullBundle(t *testing.T) {
	env := newUploadTestEnv(t)
	// Bump the cap so the 2 MB bundle fits.
	env.handler.uploadCfg.MaxSizeBytes = 10 << 20
	big := strings.Repeat(`{"idx":0,"url":"x","kind":"web","status":"done"},`, 50_000)
	big = `"sources":[` + big[:len(big)-1] + `]`
	bundle := makeBundleGzip(t, bundleMetaJSON{Name: "big"}, big)
	req := uploadRequest(t, bundle, nil)
	rec := httptest.NewRecorder()
	env.handler.UploadFromFile(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
	if !bytes.Equal(env.storage.data, bundle) {
		t.Errorf("storage bytes != bundle (stored=%d, sent=%d)", len(env.storage.data), len(bundle))
	}
}

// Ensure the recordingStorage still satisfies the Storage interface.
var _ = func() service.Storage { return &recordingStorage{} }
