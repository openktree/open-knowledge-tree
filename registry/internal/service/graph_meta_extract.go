package service

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/openktree/knowledge-registry/internal/model"
)

// bundleMetadata is the small slice of the OKT graph bundle's metadata
// section the registry needs to index an uploaded bundle. It mirrors
// the fields of graph.BundleMetadata on the OKT side (see
// backend/internal/providers/graph/bundle.go). The registry treats the
// bundle body as opaque; this struct is the only part it parses, and
// only because a file upload from the UI has no caller to set the
// X-Graph-* headers the export-task push path uses.
//
// The wire format is stable (gated by schema_version); if OKT adds a
// field the registry doesn't index, json.Decode silently ignores it.
// Adding a field OKT removed is a no-op (it stays zero-valued).
type bundleMetadata struct {
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

// ErrUploadTooLarge is returned by SpoolUploadToTempFile when the
// incoming body exceeds the configured max size. The HTTP handler maps
// it to 413 Request Entity Too Large so the client gets a clear signal
// rather than a generic 500.
var ErrUploadTooLarge = fmt.Errorf("upload exceeds configured max size")

// ErrMetadataNotFound is returned by ExtractGraphMetaFromTempFile when
// the bundle's JSON object has no "metadata" key before it closes. The
// handler maps it to 400 — the uploaded file is not a valid graph
// bundle (or is a v0 bundle the registry can't index).
var ErrMetadataNotFound = fmt.Errorf("metadata section not found in bundle")

// SpoolUploadToTempFile copies r to a new temp file in tempDir, rejecting
// the upload once it exceeds maxBytes (when > 0; 0 = unlimited). Returns
// the temp file's path. The caller owns the file: close + os.Remove it
// when done (typically via defer). On error the temp file is already
// cleaned up and the returned path is "".
//
// This is the single spool for the upload path: the multipart handler
// streams the file part straight here (via multipart.Reader, NOT
// ParseMultipartForm, which would buffer the file in the parser's own
// temp file and force a second copy). Peak memory is one io.Copy chunk
// (32 KB); the 100s-of-GB bulk lives only on disk.
func SpoolUploadToTempFile(r io.Reader, tempDir string, maxBytes int64) (string, error) {
	tmp, err := os.CreateTemp(tempDir, "okt-upload-*.json.gz")
	if err != nil {
		return "", fmt.Errorf("creating upload temp file: %w", err)
	}
	tmpName := tmp.Name()
	success := false
	defer func() {
		if !success {
			_ = tmp.Close()
			_ = os.Remove(tmpName)
		}
	}()

	// Manual copy with a byte cap (not io.Copy + io.LimitReader, which
	// would silently truncate an oversized upload at exactly maxBytes
	// and lose the tail). A 32 KB buffer keeps peak memory tiny
	// regardless of bundle size.
	const copyBuf = 32 << 10 // 32 KB
	buf := make([]byte, copyBuf)
	var total int64
	for {
		n, readErr := r.Read(buf)
		if n > 0 {
			if maxBytes > 0 && total+int64(n) > maxBytes {
				return "", ErrUploadTooLarge
			}
			if _, werr := tmp.Write(buf[:n]); werr != nil {
				return "", fmt.Errorf("writing upload temp file: %w", werr)
			}
			total += int64(n)
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return "", fmt.Errorf("reading upload body: %w", readErr)
		}
	}
	if err := tmp.Close(); err != nil {
		return "", fmt.Errorf("closing upload temp file: %w", err)
	}
	success = true
	return tmpName, nil
}

// ExtractGraphMetaFromTempFile opens the spooled temp file at tmpPath,
// streams it through gzip + json.Decoder, and reads just the bundle's
// metadata section — the rest of the bundle (sources, facts,
// embeddings, bodies — the 100s-of-GB bulk) is never touched. Returns
// the extracted GraphMeta. The temp file is left in place for the
// caller to stream to storage (and remove when done).
//
// Peak memory during the parse is the small metadata struct (~1 KB)
// plus the gzip reader's internal buffer; the bulk lives only on disk.
//
// Errors:
//   - non-gzip body        → wrap "opening gzip"
//   - not a JSON object    → "expected object"
//   - no "metadata" key    → ErrMetadataNotFound
func ExtractGraphMetaFromTempFile(ctx context.Context, tmpPath string) (*model.GraphMeta, error) {
	f, err := os.Open(tmpPath)
	if err != nil {
		return nil, fmt.Errorf("opening upload temp file: %w", err)
	}
	defer f.Close()

	gz, err := gzip.NewReader(f)
	if err != nil {
		return nil, fmt.Errorf("opening gzip: %w", err)
	}
	defer gz.Close()

	dec := json.NewDecoder(gz)
	// Read the opening '{'.
	t, err := dec.Token()
	if err != nil {
		return nil, fmt.Errorf("reading open token: %w", err)
	}
	if d, ok := t.(json.Delim); !ok || d != '{' {
		return nil, fmt.Errorf("expected object, got %v", t)
	}

	// Walk top-level keys until we hit "metadata". The v2 field
	// order is schema_version → metadata → sources → …, so the
	// scan stops almost immediately (a couple of tokens). Even a
	// worst-case bundle with metadata later in the object only
	// scans the top-level key tokens, never the values (Decode into
	// json.RawMessage discards each non-metadata value without
	// buffering it).
	var bm bundleMetadata
	found := false
	for dec.More() {
		t, err := dec.Token()
		if err != nil {
			return nil, fmt.Errorf("reading key: %w", err)
		}
		key, ok := t.(string)
		if !ok {
			return nil, fmt.Errorf("non-string key %v", t)
		}
		if key == "metadata" {
			if err := dec.Decode(&bm); err != nil {
				return nil, fmt.Errorf("decoding metadata: %w", err)
			}
			found = true
			break
		}
		// Skip the value for any other key without buffering it:
		// Decode reads one complete JSON value (object/array/string/
		// number) and returns, leaving the decoder positioned after
		// it. We discard into a json.RawMessage so the value is never
		// materialized beyond the decoder's internal buffer.
		var discard json.RawMessage
		if err := dec.Decode(&discard); err != nil {
			return nil, fmt.Errorf("skipping %q: %w", key, err)
		}
	}
	if !found {
		return nil, ErrMetadataNotFound
	}

	return &model.GraphMeta{
		Name:          bm.Name,
		Description:   bm.Description,
		Owner:         bm.Owner,
		Tags:          bm.Tags,
		SourceCount:   bm.SourceCount,
		FactCount:     bm.FactCount,
		ConceptCount:  bm.ConceptCount,
		SHA256:        bm.SHA256,
		SchemaVersion: bm.SchemaVersion,
	}, nil
}

// metaPeekSize is how many leading bytes of the bundle stream we buffer
// for the gzip+json metadata parse. The v2 wire format puts
// schema_version then metadata as the first two fields, so the
// metadata object is in the first few KB of the gzip stream — 1 MB is
// far more headroom than any real bundle needs, and it's the only
// memory the streaming upload path spends regardless of bundle size.
// The remainder of the bundle (the 100s-of-GB bulk) is never buffered
// here; it flows straight to storage via the io.MultiReader built from
// the returned replayHead + the unread part body.
const metaPeekSize = 1 << 20 // 1 MB

// ExtractGraphMetaFromReader reads the leading bytes of a gzipped
// graph bundle stream, extracts the metadata section (the same fields
// ExtractGraphMetaFromTempFile returns), and returns:
//   - the parsed GraphMeta,
//   - a replayHead buffer holding the bytes the parse consumed from r
//     (so the caller can build io.MultiReader(replayHead, r) and
//     stream the full bundle to storage without re-reading or
//     spooling).
//
// This is the streaming variant of ExtractGraphMetaFromTempFile: it
// takes an io.Reader (e.g. a multipart file-part body) instead of a
// file path, and never writes the bundle to disk. Peak memory is
// metaPeekSize (1 MB) + the gzip reader's internal buffers; the rest
// of the bundle stays unbuffered and flows through the replayHead +
// the remaining r to the downstream storage stream.
//
// Implementation: an io.TeeReader captures every byte the gzip+json
// parse reads from r into replayHead. The gzip reader consumes bytes
// from r in chunks (it has its own internal lookahead buffer), so the
// bytes it reads but doesn't logically consume (the post-metadata
// portion it buffered past the metadata object's end) are also
// captured in replayHead — which is correct, because those bytes
// belong to the bundle body and must be replayed to storage. After the
// parse returns, replayHead holds exactly the bytes read from r, and r
// is positioned at the first byte NOT yet read; concatenating
// replayHead with the remainder of r reconstructs the full original
// bundle byte-for-byte.
//
// The replayHead is capped at metaPeekSize bytes. The metadata section
// is always within the first few KB (the v2 format puts it second,
// right after schema_version), so the cap never truncates a valid
// bundle. If the parse somehow reads more than metaPeekSize (a
// malformed bundle with a huge metadata value or a non-v2 field
// order), the parse fails with the gzip/json error rather than
// silently truncating — the cap is a safety bound, not a functional
// limit.
//
// Errors mirror ExtractGraphMetaFromTempFile: non-gzip → "opening
// gzip"; not a JSON object → "expected object"; no "metadata" key →
// ErrMetadataNotFound.
func ExtractGraphMetaFromReader(ctx context.Context, r io.Reader) (*model.GraphMeta, *bytes.Buffer, error) {
	replayHead := new(bytes.Buffer)
	// The TeeReader mirrors every byte read from r into replayHead.
	// A limitedReader caps the capture at metaPeekSize so a malformed
	// bundle can't OOM the registry; the gzip reader will then hit
	// EOF on the limited reader and return a parse error rather than
	// reading forever.
	limited := &io.LimitedReader{R: r, N: metaPeekSize}
	teed := io.TeeReader(limited, replayHead)

	gz, err := gzip.NewReader(teed)
	if err != nil {
		return nil, nil, fmt.Errorf("opening gzip: %w", err)
	}
	defer gz.Close()

	dec := json.NewDecoder(gz)
	t, err := dec.Token()
	if err != nil {
		return nil, nil, fmt.Errorf("reading open token: %w", err)
	}
	if d, ok := t.(json.Delim); !ok || d != '{' {
		return nil, nil, fmt.Errorf("expected object, got %v", t)
	}

	var bm bundleMetadata
	found := false
	for dec.More() {
		t, err := dec.Token()
		if err != nil {
			return nil, nil, fmt.Errorf("reading key: %w", err)
		}
		key, ok := t.(string)
		if !ok {
			return nil, nil, fmt.Errorf("non-string key %v", t)
		}
		if key == "metadata" {
			if err := dec.Decode(&bm); err != nil {
				return nil, nil, fmt.Errorf("decoding metadata: %w", err)
			}
			found = true
			break
		}
		var discard json.RawMessage
		if err := dec.Decode(&discard); err != nil {
			return nil, nil, fmt.Errorf("skipping %q: %w", key, err)
		}
	}
	if !found {
		return nil, nil, ErrMetadataNotFound
	}

	return &model.GraphMeta{
		Name:          bm.Name,
		Description:   bm.Description,
		Owner:         bm.Owner,
		Tags:          bm.Tags,
		SourceCount:   bm.SourceCount,
		FactCount:     bm.FactCount,
		ConceptCount:  bm.ConceptCount,
		SHA256:        bm.SHA256,
		SchemaVersion: bm.SchemaVersion,
	}, replayHead, nil
}
