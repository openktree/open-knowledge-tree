package registry

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
)

// GraphMeta is a shared knowledge graph's metadata as returned by
// the registry's GET /api/v1/graphs/{id} + list endpoints. The bundle
// bytes live in S3; this struct carries the presigned download URL
// the client fetches via FetchGraphPresigned.
type GraphMeta struct {
	ID            string   `json:"id"`
	Name          string   `json:"name"`
	Description   string   `json:"description,omitempty"`
	Owner         string   `json:"owner,omitempty"`
	Tags          []string `json:"tags,omitempty"`
	SourceCount   int      `json:"source_count"`
	FactCount     int      `json:"fact_count"`
	ConceptCount  int      `json:"concept_count"`
	S3Key         string   `json:"s3_key"`
	SHA256        string   `json:"sha256,omitempty"`
	SchemaVersion int      `json:"schema_version"`
	PresignedURL  string   `json:"presigned_url,omitempty"`
	CreatedAt     string   `json:"created_at"`
	UpdatedAt     string   `json:"updated_at"`
}

// ListGraphsResponse is the paginated response from GET /api/v1/graphs.
type ListGraphsResponse struct {
	Graphs []GraphMeta `json:"graphs"`
	Total  int         `json:"total"`
}

// PushGraphResult is the response from POST /api/v1/graphs.
type PushGraphResult struct {
	GraphID string `json:"graph_id"`
	New     bool   `json:"new"`
}

// PushGraph pushes a gzipped graph bundle to the registry. The body
// is the raw gzipped bytes (see internal/providers/graph.MarshalGzip);
// the registry streams the body opaquely to S3 and indexes the graph
// from the X-Graph-* headers this client sets from meta. Returns the
// resolved graph id (deduped by (name, sha256) when meta.ID is empty).
//
// Mirrors PushSource: a write operation (uses writeKey), returns the
// registry-assigned id. For multi-GB bundles prefer PushGraphStream
// (streams the body instead of buffering it in a []byte).
func (c *Client) PushGraph(ctx context.Context, meta *GraphMeta, gzippedBundle []byte) (*PushGraphResult, error) {
	return c.PushGraphStream(ctx, meta, bytes.NewReader(gzippedBundle), int64(len(gzippedBundle)))
}

// PushGraphStream pushes a gzipped graph bundle to the registry from
// an io.Reader, avoiding the in-memory []byte buffer that PushGraph
// requires. The reader yields the raw gzipped bytes; contentLength is
// set as Content-Length when > 0 (enables chunked transfer-encoding
// when unknown). Auth is Bearer only (addAuth doesn't sign the body),
// so streaming is safe.
//
// The registry indexes the graph from the X-Graph-* headers this
// client sets from meta — the body is a pure byte pipe to S3, never
// parsed by the registry. This is the wire format introduced in
// registry v0.6.0 (replacing the embedded-metadata body-decode path
// that broke on multi-GB bundles). The body's internal metadata
// section (the one the importer reads) is unchanged; only the
// registry stops parsing the body for its own indexing.
func (c *Client) PushGraphStream(ctx context.Context, meta *GraphMeta, body io.Reader, contentLength int64) (*PushGraphResult, error) {
	if c.baseURL == "" {
		return nil, ErrRegistryDisabled
	}
	endpoint := fmt.Sprintf("%s/api/v1/graphs", c.baseURL)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, body)
	if err != nil {
		return nil, fmt.Errorf("registry: creating push graph request: %w", err)
	}
	req.Header.Set("Content-Type", "application/gzip")
	if contentLength > 0 {
		req.ContentLength = contentLength
	}
	// Indexing metadata in headers — the registry reads these
	// instead of parsing the body. See the wire-format note above.
	setGraphHeaders(req.Header, meta)
	c.addAuth(req, nil, true)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("registry: pushing graph: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("registry: push graph returned status %d: %s", resp.StatusCode, string(body))
	}
	var result PushGraphResult
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("registry: decoding push graph response: %w", err)
	}
	return &result, nil
}

// setGraphHeaders sets the X-Graph-* indexing headers on the outbound
// POST /api/v1/graphs request from meta. The registry reads these
// instead of parsing the body, keeping the body a pure byte pipe to
// S3. Tags is JSON-marshaled; counts + schema_version are stringified.
func setGraphHeaders(h http.Header, meta *GraphMeta) {
	h.Set("X-Graph-Name", meta.Name)
	if meta.Description != "" {
		h.Set("X-Graph-Description", meta.Description)
	}
	if meta.Owner != "" {
		h.Set("X-Graph-Owner", meta.Owner)
	}
	if len(meta.Tags) > 0 {
		if tagsJSON, err := json.Marshal(meta.Tags); err == nil {
			h.Set("X-Graph-Tags", string(tagsJSON))
		}
	}
	if meta.SourceCount != 0 {
		h.Set("X-Graph-Source-Count", strconv.Itoa(meta.SourceCount))
	}
	if meta.FactCount != 0 {
		h.Set("X-Graph-Fact-Count", strconv.Itoa(meta.FactCount))
	}
	if meta.ConceptCount != 0 {
		h.Set("X-Graph-Concept-Count", strconv.Itoa(meta.ConceptCount))
	}
	if meta.SHA256 != "" {
		h.Set("X-Graph-SHA256", meta.SHA256)
	}
	if meta.SchemaVersion != 0 {
		h.Set("X-Graph-Schema-Version", strconv.Itoa(meta.SchemaVersion))
	}
}

// PullGraph fetches a graph's metadata (GET /api/v1/graphs/{id}). The
// metadata includes a presigned download URL for the bundle; call
// FetchGraphPresigned to stream the raw gzipped bytes. Mirrors
// PullSource (metadata-only; the bundle is fetched separately).
func (c *Client) PullGraph(ctx context.Context, graphID string) (*GraphMeta, error) {
	if c.baseURL == "" {
		return nil, ErrRegistryDisabled
	}
	endpoint := fmt.Sprintf("%s/api/v1/graphs/%s", c.baseURL, url.PathEscape(graphID))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("registry: creating pull graph request: %w", err)
	}
	c.addAuth(req, nil, false)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("registry: pulling graph: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("registry: graph %s not found", graphID)
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("registry: pull graph returned status %d: %s", resp.StatusCode, string(body))
	}
	var meta GraphMeta
	if err := json.NewDecoder(resp.Body).Decode(&meta); err != nil {
		return nil, fmt.Errorf("registry: decoding graph metadata: %w", err)
	}
	return &meta, nil
}

// FetchGraphPresigned fetches the raw gzipped bundle bytes for a
// graph, preferring the registry's presigned S3 URL (fast path:
// registry issues a tiny presigned URL, caller fetches the raw blob
// from object storage) and falling back to the registry's
// GET /api/v1/graphs/{id}/bundle endpoint (which streams from the
// registry VM) when no presigned URL is available (filesystem backend,
// dev mode). Returns the raw gzipped bytes ready for
// graph.UnmarshalGzip.
//
// Mirrors FetchDecompositionPresigned's two-tier shape.
func (c *Client) FetchGraphPresigned(ctx context.Context, graphID string) ([]byte, error) {
	if c.baseURL == "" {
		return nil, ErrRegistryDisabled
	}
	meta, err := c.PullGraph(ctx, graphID)
	if err != nil {
		return nil, err
	}
	if meta.PresignedURL != "" {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, meta.PresignedURL, nil)
		if err != nil {
			return nil, fmt.Errorf("registry: creating presigned graph fetch request: %w", err)
		}
		resp, err := c.http.Do(req)
		if err != nil {
			return nil, fmt.Errorf("registry: fetching presigned graph: %w", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(resp.Body)
			return nil, fmt.Errorf("registry: presigned graph fetch returned status %d: %s", resp.StatusCode, string(body))
		}
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			return nil, fmt.Errorf("registry: reading presigned graph body: %w", err)
		}
		return body, nil
	}
	// Fallback: no presigned URL (filesystem backend, dev). Use the
	// registry's bundle endpoint which streams the raw bytes.
	endpoint := fmt.Sprintf("%s/api/v1/graphs/%s/bundle", c.baseURL, url.PathEscape(graphID))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("registry: creating graph bundle request: %w", err)
	}
	c.addAuth(req, nil, false)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("registry: fetching graph bundle: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("registry: graph bundle returned status %d: %s", resp.StatusCode, string(body))
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("registry: reading graph bundle body: %w", err)
	}
	return body, nil
}

// ListGraphs fetches a paginated, optionally searched list of shared
// graphs from the registry. The q parameter is a free-text LIKE over
// name + description; tag is an exact tag match. Either may be empty
// (empty = no filter). Mirrors ListSources.
func (c *Client) ListGraphs(ctx context.Context, limit, offset int, q, tag string) (*ListGraphsResponse, error) {
	if c.baseURL == "" {
		return nil, ErrRegistryDisabled
	}
	params := make(url.Values)
	params.Set("limit", strconv.Itoa(limit))
	params.Set("offset", strconv.Itoa(offset))
	if q != "" {
		params.Set("q", q)
	}
	if tag != "" {
		params.Set("tag", tag)
	}
	endpoint := fmt.Sprintf("%s/api/v1/graphs?%s", c.baseURL, params.Encode())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("registry: creating list graphs request: %w", err)
	}
	c.addAuth(req, nil, false)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("registry: listing graphs: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("registry: list graphs returned status %d: %s", resp.StatusCode, string(body))
	}
	var result ListGraphsResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("registry: decoding list graphs response: %w", err)
	}
	return &result, nil
}

// DeleteGraph deletes a graph from the registry. Owner-or-admin only
// (enforced by the registry). Mirrors the source delete pattern.
func (c *Client) DeleteGraph(ctx context.Context, graphID string) error {
	if c.baseURL == "" {
		return ErrRegistryDisabled
	}
	endpoint := fmt.Sprintf("%s/api/v1/graphs/%s", c.baseURL, url.PathEscape(graphID))
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, endpoint, nil)
	if err != nil {
		return fmt.Errorf("registry: creating delete graph request: %w", err)
	}
	c.addAuth(req, nil, true)
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("registry: deleting graph: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("registry: delete graph returned status %d: %s", resp.StatusCode, string(body))
	}
	return nil
}
