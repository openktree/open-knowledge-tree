package handler

import (
	"bytes"
	"context"
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/openktree/knowledge-registry/internal/auth"
	"github.com/openktree/knowledge-registry/internal/config"
	"github.com/openktree/knowledge-registry/internal/mailer"
	"github.com/openktree/knowledge-registry/internal/model"
	"github.com/openktree/knowledge-registry/internal/service"
	"github.com/openktree/knowledge-registry/internal/store"
)

// newUIHandler builds a UIHandler against an in-memory sqlite store
// + the given storage, the minimum needed to exercise the /ui/graphs/upload
// POST path. Mirrors newUploadTestEnv / newGraphTestEnv.
func newUIHandler(t *testing.T, storage service.Storage) (*UIHandler, store.MetadataStore) {
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
	reg := service.New(s, storage, 3600, 0, 0, false)
	h := NewUIHandler(s, reg, &config.AuthConfig{JWTSecret: "test"}, &config.EmailValidationConfig{}, &mailer.NoopMailer{})
	h.SetUploadConfig(&config.GraphUploadConfig{
		MaxSizeBytes: 1 << 20,
		TempDir:      t.TempDir(),
	})
	return h, s
}

// failingStorage is a Storage double whose StoreStream always returns
// storeErr, simulating an R2/upload failure mid-push. Used to assert
// the upload handler's error-status contract: a storage failure must
// re-render the form with a 4xx/5xx (never an implicit 200 that the
// client-side progress-bar JS would mistake for success).
type failingStorage struct{ storeErr error }

func (f *failingStorage) Store(_ context.Context, _ string, _ []byte, _ string) error {
	return f.storeErr
}
func (f *failingStorage) StoreStream(_ context.Context, _ string, _ io.Reader, _ string) (int64, error) {
	return 0, f.storeErr
}
func (f *failingStorage) StoreJSON(_ context.Context, _ string, _ []byte) error { return f.storeErr }
func (f *failingStorage) Delete(_ context.Context, _ string) error              { return nil }
func (f *failingStorage) ReadAll(_ context.Context, _ string) ([]byte, string, error) {
	return nil, "", f.storeErr
}
func (f *failingStorage) PresignedURL(_ context.Context, _ string, _ time.Duration) (string, error) {
	return "", nil
}
func (f *failingStorage) PresignedPUTURL(_ context.Context, _ string, _ time.Duration) (string, error) {
	return "", nil
}

var _ service.Storage = (*failingStorage)(nil)

// uiUploadRequest builds a multipart/form-data POST for
// /ui/graphs/upload with a "bundle" file part holding the given bytes,
// authenticated as an admin (so the admin-only guard passes and the
// owner field is populated).
func uiUploadRequest(t *testing.T, bundle []byte) *http.Request {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
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
	req := httptest.NewRequest(http.MethodPost, "/ui/graphs/upload", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	ctx := auth.WithUserEmail(req.Context(), "admin@example.com")
	ctx = auth.WithUser(ctx, "user-1", "admin")
	return req.WithContext(ctx)
}

// makeBundleGzipUI returns a gzipped graph bundle with a metadata
// section, matching the v2 wire format. Wraps makeBundleGzip with
// the happy-path metadata so UI tests don't repeat the boilerplate.
func makeBundleGzipUI(t *testing.T) []byte {
	t.Helper()
	return makeBundleGzip(t, bundleMetaJSON{
		Name:   "UI Upload Graph",
		Tags:   []string{"ui"},
		SHA256: "ui-sha",
	}, `"sources":[]`)
}

// TestGraphUploadPage_StorageFailureReturnsNon2xx is the regression
// test for the false-success bug: when PushGraphFromFile fails (here
// because StoreStream returns an error), the handler must respond
// with a non-2xx status so the client-side progress-bar JS keys off
// xhr.status and shows the re-rendered error form instead of
// navigating to /ui/graphs?uploaded=1. Before the fix the handler
// called h.render (implicit 200) on every error branch, so the JS
// treated the error form as a success redirect.
func TestGraphUploadPage_StorageFailureReturnsNon2xx(t *testing.T) {
	h, _ := newUIHandler(t, &failingStorage{storeErr: errors.New("simulated R2 outage")})
	bundle := makeBundleGzipUI(t)
	req := uiUploadRequest(t, bundle)
	rec := httptest.NewRecorder()
	h.GraphUploadPage(rec, req)

	if rec.Code < 400 {
		t.Fatalf("expected >=400 status on storage failure so the JS error branch fires, got %d (body=%q)", rec.Code, rec.Body.String())
	}
	// The re-rendered form must surface the error message so the
	// user (and the no-JS fallback) sees what went wrong.
	if !strings.Contains(rec.Body.String(), "Failed to upload graph") {
		t.Errorf("expected error message in re-rendered form, got: %s", rec.Body.String())
	}
}

// TestGraphUploadPage_MissingBundleReturns4xx covers the early
// validation branch (no bundle file part) — must be 4xx, not 200.
func TestGraphUploadPage_MissingBundleReturns4xx(t *testing.T) {
	h, _ := newUIHandler(t, &recordingStorage{})
	// Multipart form with no file part.
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	if err := mw.Close(); err != nil {
		t.Fatalf("closing multipart: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/ui/graphs/upload", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	ctx := auth.WithUser(req.Context(), "user-1", "admin")
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()
	h.GraphUploadPage(rec, req)
	if rec.Code < 400 {
		t.Fatalf("expected >=400 for missing bundle, got %d", rec.Code)
	}
}

// TestGraphUploadPage_HappyPathRedirects asserts the success path is
// unchanged: a valid bundle + a working storage returns 302 to
// /ui/graphs?uploaded=1. This guards against the fix accidentally
// breaking the happy path.
func TestGraphUploadPage_HappyPathRedirects(t *testing.T) {
	h, _ := newUIHandler(t, &recordingStorage{})
	bundle := makeBundleGzipUI(t)
	req := uiUploadRequest(t, bundle)
	rec := httptest.NewRecorder()
	h.GraphUploadPage(rec, req)
	if rec.Code != http.StatusFound {
		t.Fatalf("expected 302 redirect on success, got %d: %s", rec.Code, rec.Body.String())
	}
	if loc := rec.Header().Get("Location"); loc != "/ui/graphs?uploaded=1" {
		t.Errorf("expected redirect to /ui/graphs?uploaded=1, got %q", loc)
	}
}

// TestGraphUploadPage_NonGzipReturns4xx covers the metadata-extraction
// branch: a non-gzip bundle must surface a 4xx (the previous code
// returned an implicit 200, hiding the "reading bundle metadata" error).
func TestGraphUploadPage_NonGzipReturns4xx(t *testing.T) {
	h, _ := newUIHandler(t, &recordingStorage{})
	bundle := []byte(`{"schema_version":2,"metadata":{"name":"x"},"sources":[]}`) // not gzipped
	req := uiUploadRequest(t, bundle)
	rec := httptest.NewRecorder()
	h.GraphUploadPage(rec, req)
	if rec.Code < 400 {
		t.Fatalf("expected >=400 for non-gzip bundle, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "reading bundle metadata") {
		t.Errorf("expected 'reading bundle metadata' error in body, got: %s", rec.Body.String())
	}
}
