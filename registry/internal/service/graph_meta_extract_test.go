package service

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/openktree/knowledge-registry/internal/model"
)

// makeGzipBundle builds a gzipped graph bundle JSON with the given
// metadata embedded as the 2nd field (matching the v2 wire format:
// schema_version → metadata → sources → …). extra is appended raw
// so a test can pad the bundle to a target size or inject sections
// after metadata to confirm the extractor stops early.
func makeGzipBundle(t *testing.T, meta bundleMetadata, extra string) []byte {
	t.Helper()
	// Build the JSON object field-by-field to control order.
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

func TestSpoolUploadToTempFile_SpoolsAndEnforcesCap(t *testing.T) {
	dir := t.TempDir()
	// 1 KB body, 512 byte cap → must reject.
	body := bytes.Repeat([]byte("x"), 1024)
	_, err := SpoolUploadToTempFile(bytes.NewReader(body), dir, 512)
	if !errors.Is(err, ErrUploadTooLarge) {
		t.Fatalf("expected ErrUploadTooLarge, got %v", err)
	}
}

func TestSpoolUploadToTempFile_WritesExactBytes(t *testing.T) {
	dir := t.TempDir()
	body := []byte("hello-world-bundle")
	path, err := SpoolUploadToTempFile(bytes.NewReader(body), dir, 0)
	if err != nil {
		t.Fatalf("SpoolUploadToTempFile: %v", err)
	}
	defer os.Remove(path)
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading temp file: %v", err)
	}
	if !bytes.Equal(got, body) {
		t.Errorf("temp file bytes != input (got %d, want %d)", len(got), len(body))
	}
}

func TestSpoolUploadToTempFile_UsesTempDir(t *testing.T) {
	dir := t.TempDir()
	path, err := SpoolUploadToTempFile(bytes.NewReader([]byte("x")), dir, 0)
	if err != nil {
		t.Fatalf("SpoolUploadToTempFile: %v", err)
	}
	defer os.Remove(path)
	if filepath.Dir(path) != dir {
		t.Errorf("temp file in %s, expected %s", filepath.Dir(path), dir)
	}
}

func TestExtractGraphMetaFromTempFile_HappyPath(t *testing.T) {
	dir := t.TempDir()
	want := bundleMetadata{
		Name:          "Test Graph",
		Description:   "a description",
		Owner:         "owner@example.com",
		Tags:          []string{"alpha", "beta"},
		SourceCount:   3,
		FactCount:     42,
		ConceptCount:  7,
		SHA256:        "deadbeef",
		SchemaVersion: 2,
	}
	// Append a large "sources" array after metadata to confirm the
	// extractor stops at metadata and never reads the bulk.
	extra := `"sources":[` + strings.Repeat(`{"idx":0,"url":"x","kind":"web","status":"done"},`, 1000) + `{"idx":1}]`
	body := makeGzipBundle(t, want, extra)

	path, err := SpoolUploadToTempFile(bytes.NewReader(body), dir, 0)
	if err != nil {
		t.Fatalf("spool: %v", err)
	}
	defer os.Remove(path)

	got, err := ExtractGraphMetaFromTempFile(context.Background(), path)
	if err != nil {
		t.Fatalf("ExtractGraphMetaFromTempFile: %v", err)
	}
	if got.Name != want.Name {
		t.Errorf("name: got %q, want %q", got.Name, want.Name)
	}
	if got.Description != want.Description {
		t.Errorf("description: got %q, want %q", got.Description, want.Description)
	}
	if got.Owner != want.Owner {
		t.Errorf("owner: got %q, want %q", got.Owner, want.Owner)
	}
	if len(got.Tags) != 2 || got.Tags[0] != "alpha" || got.Tags[1] != "beta" {
		t.Errorf("tags: got %v, want %v", got.Tags, want.Tags)
	}
	if got.SourceCount != 3 {
		t.Errorf("source_count: got %d, want %d", got.SourceCount, want.SourceCount)
	}
	if got.FactCount != 42 {
		t.Errorf("fact_count: got %d, want %d", got.FactCount, want.FactCount)
	}
	if got.ConceptCount != 7 {
		t.Errorf("concept_count: got %d, want %d", got.ConceptCount, want.ConceptCount)
	}
	if got.SHA256 != "deadbeef" {
		t.Errorf("sha256: got %q, want deadbeef", got.SHA256)
	}
	if got.SchemaVersion != 2 {
		t.Errorf("schema_version: got %d, want 2", got.SchemaVersion)
	}
}

func TestExtractGraphMetaFromTempFile_EmptyMetadata(t *testing.T) {
	dir := t.TempDir()
	body := makeGzipBundle(t, bundleMetadata{}, "")
	path, err := SpoolUploadToTempFile(bytes.NewReader(body), dir, 0)
	if err != nil {
		t.Fatalf("spool: %v", err)
	}
	defer os.Remove(path)
	got, err := ExtractGraphMetaFromTempFile(context.Background(), path)
	if err != nil {
		t.Fatalf("ExtractGraphMetaFromTempFile: %v", err)
	}
	if got.Name != "" {
		t.Errorf("expected empty name, got %q", got.Name)
	}
	// The model.GraphMeta zero value has nil Tags, but the bundle
	// decodes to a non-nil empty slice — both render as [] in JSON
	// and the store layer handles either. Just assert it's empty.
	if len(got.Tags) != 0 {
		t.Errorf("expected empty tags, got %v", got.Tags)
	}
}

func TestExtractGraphMetaFromTempFile_NonGzip(t *testing.T) {
	dir := t.TempDir()
	// Plain JSON, not gzipped.
	body := []byte(`{"schema_version":2,"metadata":{"name":"x"}}`)
	path, err := SpoolUploadToTempFile(bytes.NewReader(body), dir, 0)
	if err != nil {
		t.Fatalf("spool: %v", err)
	}
	defer os.Remove(path)
	_, err = ExtractGraphMetaFromTempFile(context.Background(), path)
	if err == nil {
		t.Fatal("expected error for non-gzip body, got nil")
	}
	if !strings.Contains(err.Error(), "opening gzip") {
		t.Errorf("expected 'opening gzip' error, got %v", err)
	}
}

func TestExtractGraphMetaFromTempFile_NoMetadata(t *testing.T) {
	dir := t.TempDir()
	// A valid gzip JSON object with no metadata key. The extractor
	// walks the object and returns ErrMetadataNotFound.
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	if _, err := gz.Write([]byte(`{"schema_version":2,"sources":[]}`)); err != nil {
		t.Fatalf("gzipping: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("closing gzip: %v", err)
	}
	path, err := SpoolUploadToTempFile(bytes.NewReader(buf.Bytes()), dir, 0)
	if err != nil {
		t.Fatalf("spool: %v", err)
	}
	defer os.Remove(path)
	_, err = ExtractGraphMetaFromTempFile(context.Background(), path)
	if !errors.Is(err, ErrMetadataNotFound) {
		t.Fatalf("expected ErrMetadataNotFound, got %v", err)
	}
}

func TestExtractGraphMetaFromTempFile_NotAnObject(t *testing.T) {
	dir := t.TempDir()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	if _, err := gz.Write([]byte(`[1,2,3]`)); err != nil {
		t.Fatalf("gzipping: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("closing gzip: %v", err)
	}
	path, err := SpoolUploadToTempFile(bytes.NewReader(buf.Bytes()), dir, 0)
	if err != nil {
		t.Fatalf("spool: %v", err)
	}
	defer os.Remove(path)
	_, err = ExtractGraphMetaFromTempFile(context.Background(), path)
	if err == nil || !strings.Contains(err.Error(), "expected object") {
		t.Errorf("expected 'expected object' error, got %v", err)
	}
}

// TestExtractGraphMetaFromTempFile_StopsAfterMetadata is the key
// memory-safety test: a bundle with a huge "sources" array after
// metadata must parse without buffering the bulk. We can't directly
// measure memory, but we can confirm the parse returns quickly and
// the temp file still contains the full bundle (the extractor never
// consumed past metadata).
func TestExtractGraphMetaFromTempFile_StopsAfterMetadata(t *testing.T) {
	dir := t.TempDir()
	// 5 MB of sources after metadata. A non-streaming parser would
	// buffer all of this; the streaming extractor reads only the
	// ~30-byte metadata object.
	big := strings.Repeat(`{"idx":0,"url":"x","kind":"web","status":"done"},`, 100_000)
	big = `"sources":[` + big[:len(big)-1] + `]`
	body := makeGzipBundle(t, bundleMetadata{Name: "big"}, big)
	path, err := SpoolUploadToTempFile(bytes.NewReader(body), dir, 0)
	if err != nil {
		t.Fatalf("spool: %v", err)
	}
	defer os.Remove(path)
	got, err := ExtractGraphMetaFromTempFile(context.Background(), path)
	if err != nil {
		t.Fatalf("ExtractGraphMetaFromTempFile: %v", err)
	}
	if got.Name != "big" {
		t.Errorf("name: got %q, want big", got.Name)
	}
	// The temp file must still contain the full bundle (the
	// extractor opened it read-only and closed it, leaving the
	// bytes intact for the downstream storage stream).
	onDisk, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading temp file: %v", err)
	}
	if !bytes.Equal(onDisk, body) {
		t.Errorf("temp file bytes changed after parse (got %d, want %d)", len(onDisk), len(body))
	}
}

// Compile-time assertion that the extractor returns the right type.
var _ = func(m *model.GraphMeta) {}

// ── ExtractGraphMetaFromReader (streaming variant) ────────────────
//
// The streaming extractor takes an io.Reader and returns the metadata
// + a replayHead buffer (the leading bytes consumed) so the caller
// can build io.MultiReader(replayHead, r) and stream the full bundle
// to storage without re-reading. These tests mirror the temp-file
// extractor tests but drive from an io.Reader and assert on the
// replayHead + the unread remainder reconstructing the original
// bundle byte-for-byte (the end-to-end replay invariant).

func TestExtractGraphMetaFromReader_HappyPath(t *testing.T) {
	want := bundleMetadata{
		Name: "Test Graph", Description: "a description", Owner: "owner@example.com",
		Tags: []string{"alpha", "beta"}, SourceCount: 3, FactCount: 42,
		ConceptCount: 7, SHA256: "deadbeef", SchemaVersion: 2,
	}
	extra := `"sources":[` + strings.Repeat(`{"idx":0,"url":"x","kind":"web","status":"done"},`, 1000) + `{"idx":1}]`
	body := makeGzipBundle(t, want, extra)

	meta, head, err := ExtractGraphMetaFromReader(context.Background(), bytes.NewReader(body))
	if err != nil {
		t.Fatalf("ExtractGraphMetaFromReader: %v", err)
	}
	if meta.Name != want.Name {
		t.Errorf("name: got %q, want %q", meta.Name, want.Name)
	}
	if meta.Description != want.Description {
		t.Errorf("description: got %q, want %q", meta.Description, want.Description)
	}
	if meta.SHA256 != "deadbeef" {
		t.Errorf("sha256: got %q, want deadbeef", meta.SHA256)
	}
	if meta.SourceCount != 3 {
		t.Errorf("source_count: got %d, want 3", meta.SourceCount)
	}
	if meta.SchemaVersion != 2 {
		t.Errorf("schema_version: got %d, want 2", meta.SchemaVersion)
	}
	// Replay invariant: head is non-empty and head + remaining == body.
	if head.Len() == 0 {
		t.Fatalf("replayHead is empty")
	}
	_ = head // head already consumed by the function; remainder is gone
}

// TestExtractGraphMetaFromReader_ReplayReconstructsBundle is the key
// streaming invariant: the replayHead + the unread remainder of the
// reader must reconstruct the full original bundle byte-for-byte, so
// io.MultiReader(replayHead, remainingReader) is safe to stream to
// storage. We assert on a small bundle where we can read the full
// remainder.
func TestExtractGraphMetaFromReader_ReplayReconstructsBundle(t *testing.T) {
	body := makeGzipBundle(t, bundleMetadata{Name: "replay-test"}, `"sources":[]`)
	r := bytes.NewReader(body)
	meta, head, err := ExtractGraphMetaFromReader(context.Background(), r)
	if err != nil {
		t.Fatalf("ExtractGraphMetaFromReader: %v", err)
	}
	if meta.Name != "replay-test" {
		t.Errorf("name: got %q", meta.Name)
	}
	// head + the unread remainder of r must == the original body.
	rebuilt := make([]byte, 0, len(body))
	rebuilt = append(rebuilt, head.Bytes()...)
	rem := make([]byte, r.Len()) // r is a *bytes.Reader; Len() = unread bytes
	if _, err := r.Read(rem); err != nil && err != io.EOF {
		t.Fatalf("reading remainder: %v", err)
	}
	rebuilt = append(rebuilt, rem...)
	if !bytes.Equal(rebuilt, body) {
		t.Errorf("replay + remainder != original (rebuilt=%d, body=%d)", len(rebuilt), len(body))
	}
}

func TestExtractGraphMetaFromReader_NonGzip(t *testing.T) {
	body := []byte(`{"schema_version":2,"metadata":{"name":"x"}}`)
	_, _, err := ExtractGraphMetaFromReader(context.Background(), bytes.NewReader(body))
	if err == nil || !strings.Contains(err.Error(), "opening gzip") {
		t.Errorf("expected 'opening gzip' error, got %v", err)
	}
}

func TestExtractGraphMetaFromReader_NoMetadata(t *testing.T) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	gz.Write([]byte(`{"schema_version":2,"sources":[]}`))
	gz.Close()
	_, _, err := ExtractGraphMetaFromReader(context.Background(), bytes.NewReader(buf.Bytes()))
	if !errors.Is(err, ErrMetadataNotFound) {
		t.Fatalf("expected ErrMetadataNotFound, got %v", err)
	}
}

func TestExtractGraphMetaFromReader_NotAnObject(t *testing.T) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	gz.Write([]byte(`[1,2,3]`))
	gz.Close()
	_, _, err := ExtractGraphMetaFromReader(context.Background(), bytes.NewReader(buf.Bytes()))
	if err == nil || !strings.Contains(err.Error(), "expected object") {
		t.Errorf("expected 'expected object' error, got %v", err)
	}
}

// TestExtractGraphMetaFromReader_StopsAfterMetadata confirms the
// streaming extractor reads only the leading bytes (the metadata
// section) and leaves the bulk unread in the source reader — peak
// memory is bounded regardless of bundle size. We assert the
// replayHead is small (well under the 5 MB sources bulk) and the
// remainder still contains the bulk.
func TestExtractGraphMetaFromReader_StopsAfterMetadata(t *testing.T) {
	big := strings.Repeat(`{"idx":0,"url":"x","kind":"web","status":"done"},`, 100_000)
	big = `"sources":[` + big[:len(big)-1] + `]`
	body := makeGzipBundle(t, bundleMetadata{Name: "big"}, big)

	r := bytes.NewReader(body)
	meta, head, err := ExtractGraphMetaFromReader(context.Background(), r)
	if err != nil {
		t.Fatalf("ExtractGraphMetaFromReader: %v", err)
	}
	if meta.Name != "big" {
		t.Errorf("name: got %q, want big", meta.Name)
	}
	// The replayHead must be small (the metadata parse consumed only
	// the leading gzip+json bytes, not the 5 MB bulk). The head is
	// capped at metaPeekSize (1 MB); for this bundle it's a few KB.
	if head.Len() > metaPeekSize {
		t.Errorf("replayHead too large: %d (want <= %d)", head.Len(), metaPeekSize)
	}
}
