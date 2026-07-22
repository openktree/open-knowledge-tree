package graph

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
)

// MarshalGzip serializes a GraphBundle to gzipped JSON. The registry
// stores the raw gzipped bytes in S3 (so pulls stream the original
// bytes without re-gzipping), and the OKT import path gunzips on
// receipt. Gzip keeps large book-source bundles bounded — a 50MB
// bundle of parsed text + embeddings typically compresses to ~8MB.
func MarshalGzip(b *GraphBundle) ([]byte, error) {
	jsonBytes, err := json.Marshal(b)
	if err != nil {
		return nil, fmt.Errorf("marshaling graph bundle: %w", err)
	}
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	if _, err := gz.Write(jsonBytes); err != nil {
		return nil, fmt.Errorf("gzipping graph bundle: %w", err)
	}
	if err := gz.Close(); err != nil {
		return nil, fmt.Errorf("closing gzip writer: %w", err)
	}
	return buf.Bytes(), nil
}

// UnmarshalGzip decodes a gzipped JSON GraphBundle. The inverse of
// MarshalGzip; used by the OKT import path on a bundle pulled from
// the registry (presigned URL fast path) or read from a local upload.
func UnmarshalGzip(data []byte) (*GraphBundle, error) {
	gz, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("opening gzip reader: %w", err)
	}
	defer gz.Close()
	jsonBytes, err := io.ReadAll(gz)
	if err != nil {
		return nil, fmt.Errorf("gunzipping graph bundle: %w", err)
	}
	var b GraphBundle
	if err := json.Unmarshal(jsonBytes, &b); err != nil {
		return nil, fmt.Errorf("unmarshaling graph bundle: %w", err)
	}
	return &b, nil
}
