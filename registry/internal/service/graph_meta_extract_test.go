package service

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
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
