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
	"sync/atomic"
	"time"

	"github.com/openktree/open-knowledge-tree/backend/internal/config"
)

// Client is the HTTP client for the Knowledge Registry service.
// The registry is a flat namespace — no repository scoping.
//
// AuthMode can be ""/"none" (no auth) or "bearer" (Bearer token in
// Authorization header). The write key is used for push operations,
// the read key for search/pull; read key falls back to write key.
type Client struct {
	baseURL  string
	http     *http.Client
	models   []string
	authMode string // "" / "none" / "bearer"
	writeKey string // for push operations (Bearer token)
	readKey  string // for search/pull; falls back to writeKey
	// contextVocab caches the registry's canonical context list
	// (GET /api/v1/contexts) so the contribute/pull workers don't
	// call the registry per concept. The cache is per-Client (one
	// per registry id) and best-effort: on a registry error,
	// ListContexts returns the last-good cached value so a
	// transient outage doesn't break ingestion. TTL 10 min.
	contextVocab atomic.Pointer[contextVocabCache]
}

// contextVocabCache is the cached payload of GET /api/v1/contexts.
type contextVocabCache struct {
	labels    []string
	fetchedAt time.Time
}

// contextVocabTTL is how long a cached context vocab is considered
// fresh. Chosen to be long enough to avoid per-contribute fan-out
// but short enough to pick up a registry-side vocab refresh within
// a reasonable window.
const contextVocabTTL = 10 * time.Minute

func New(cfg config.RegistryConfig) *Client {
	if cfg.URL == "" {
		return &Client{}
	}
	readKey := cfg.ReadAPIKey
	if readKey == "" {
		readKey = cfg.APIKey
	}
	return &Client{
		baseURL: cfg.URL,
		http: &http.Client{
			Timeout: 5 * time.Minute,
		},
		models:   cfg.AllowedModels,
		authMode: cfg.AuthMode,
		writeKey: cfg.APIKey,
		readKey:  readKey,
	}
}

// addAuth attaches authentication to an outgoing request.
// For read operations (GET), it uses readKey; for write operations
// (POST), it uses writeKey. When the relevant key is empty or
// authMode is "" or "none", no headers are added (public registry).
func (c *Client) addAuth(req *http.Request, _ []byte, isWrite bool) {
	key := c.readKey
	if isWrite && c.writeKey != "" {
		key = c.writeKey
	}
	if key == "" || c.authMode == "" || c.authMode == "none" {
		return
	}
	req.Header.Set("Authorization", "Bearer "+key)
}

var ErrRegistryDisabled = fmt.Errorf("registry: not configured")

type SearchResult struct {
	Found    bool   `json:"found,omitempty"`
	SourceID string `json:"source_id,omitempty"`
	Title    string `json:"title,omitempty"`
	URL      string `json:"url,omitempty"`
	DOI      string `json:"doi,omitempty"`
	SHA256   string `json:"sha256,omitempty"`
}

// SearchSource checks the registry for a source matching the given URL or DOI.
func (c *Client) SearchSource(ctx context.Context, sourceURL, doi string) (*SearchResult, error) {
	if c.baseURL == "" {
		return nil, ErrRegistryDisabled
	}
	q := make(url.Values)
	if sourceURL != "" {
		q.Set("url", sourceURL)
	}
	if doi != "" {
		q.Set("doi", doi)
	}
	if len(q) == 0 {
		return nil, fmt.Errorf("registry: url or doi required")
	}
	endpoint := fmt.Sprintf("%s/api/v1/search?%s", c.baseURL, q.Encode())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("registry: creating search request: %w", err)
	}
	c.addAuth(req, nil, false)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("registry: searching source: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return &SearchResult{}, nil
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("registry: search returned status %d: %s", resp.StatusCode, string(body))
	}
	var result SearchResult
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("registry: decoding search response: %w", err)
	}
	result.Found = true
	return &result, nil
}

type SourcePackage struct {
	Source struct {
		ID     string `json:"id"`
		URL    string `json:"url"`
		DOI    string `json:"doi"`
		SHA256 string `json:"sha256"`
		Title  string `json:"title"`
		S3Key  string `json:"s3_key"`
	} `json:"source"`
	Content struct {
		Text     string `json:"text"`
		Markdown string `json:"markdown"`
	} `json:"content"`
	Decompositions []DecompRef `json:"decompositions,omitempty"`
	PresignedURLs  interface{} `json:"presigned_urls,omitempty"`
}

type DecompRef struct {
	ModelID        string `json:"model_id"`
	FactCount      int    `json:"fact_count"`
	HasEmbeddings  bool   `json:"has_embeddings"`
	EmbeddingModel string `json:"embedding_model,omitempty"`
	PresignedURL   string `json:"presigned_url,omitempty"`
	S3Key          string `json:"s3_key,omitempty"`
	// PromptsetHash is the philosophy hash the registry stamps on
	// each decomposition it stores. Pulling repos use it to filter
	// the DecompRef list (Service.ListRelevantDecompositions) so
	// decompositions from promptsets the repo hasn't accepted are
	// skipped before the per-decomposition pull. Empty when the
	// registry server hasn't shipped promptset_hash on its
	// source-listing response — the client treats empty as the
	// default and accepts it (legacy behavior).
	PromptsetHash string `json:"promptset_hash,omitempty"`
}

// PullSource retrieves a source package from the registry.
func (c *Client) PullSource(ctx context.Context, sourceID string) (*SourcePackage, error) {
	if c.baseURL == "" {
		return nil, ErrRegistryDisabled
	}
	endpoint := fmt.Sprintf("%s/api/v1/sources/%s", c.baseURL, url.PathEscape(sourceID))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("registry: creating pull request: %w", err)
	}
	c.addAuth(req, nil, false)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("registry: pulling source: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("registry: source %s not found", sourceID)
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("registry: pull source returned status %d: %s", resp.StatusCode, string(body))
	}
	var pkg SourcePackage
	if err := json.NewDecoder(resp.Body).Decode(&pkg); err != nil {
		return nil, fmt.Errorf("registry: decoding source package: %w", err)
	}
	return &pkg, nil
}

type DecompositionPackage struct {
	ModelID          string            `json:"model_id"`
	PromptsetHash    string            `json:"promptset_hash,omitempty"`
	Facts            []FactData        `json:"facts,omitempty"`
	Concepts         []ConceptData     `json:"concepts,omitempty"`
	Embeddings       *EmbeddingData    `json:"embeddings,omitempty"`
	ConceptEmbeddings *EmbeddingData   `json:"concept_embeddings,omitempty"`
	Links            []FactConceptLink `json:"links,omitempty"`
}

type FactConceptLink struct {
	FactContentHash string `json:"fact_content_hash"`
	ConceptName     string `json:"concept_name"`
	ConceptContext  string `json:"concept_context"`
}

type FactData struct {
	Content      string   `json:"content"`
	ContentHash  string   `json:"content_hash"`
	Confidence   float64  `json:"confidence"`
	SentenceIdx  int      `json:"sentence_index"`
	ImageURL     string   `json:"image_url,omitempty"`
	ImageCaption string   `json:"image_caption,omitempty"`
}

type ConceptData struct {
	CanonicalName string   `json:"canonical_name"`
	Context       string   `json:"context"`
	Aliases       []string `json:"aliases,omitempty"`
	OntologyClass string   `json:"ontology_class,omitempty"`
	Embedding     []float64 `json:"embedding,omitempty"`
}

// EmbeddingData matches the registry's embedding payload shape:
// one model + dimensions for the whole package, with a vectors
// map keyed by "fact:<uuid>" or "concept:<uuid>". This is the
// shape the registry deserializes, so using it directly fixes
// the has_embeddings=false bug (the backend was sending a JSON
// array while the registry expected a single object).
type EmbeddingData struct {
	Model      string               `json:"model"`
	Dimensions int                  `json:"dimensions"`
	Vectors    map[string][]float64 `json:"vectors"`
}

// EmbeddingRef is the legacy per-embedding shape used by the pull
// path (retrieve_source) which iterates a flat list. Kept for
// backward compatibility with existing pull code; new code should
// use EmbeddingData.
type EmbeddingRef struct {
	Key         string    `json:"key"`
	ContentHash string    `json:"content_hash"`
	Dimensions  int       `json:"dimensions"`
	Model       string    `json:"model"`
	Values      []float64 `json:"values,omitempty"`
}

// PullDecomposition retrieves a decomposition package for a source and model.
func (c *Client) PullDecomposition(ctx context.Context, sourceID, modelID string) (*DecompositionPackage, error) {
	if c.baseURL == "" {
		return nil, ErrRegistryDisabled
	}
	endpoint := fmt.Sprintf("%s/api/v1/sources/%s/decompositions/%s", c.baseURL, url.PathEscape(sourceID), url.PathEscape(modelID))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("registry: creating pull decomposition request: %w", err)
	}
	c.addAuth(req, nil, false)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("registry: pulling decomposition: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("registry: decomposition %s/%s not found", sourceID, modelID)
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("registry: pull decomposition returned status %d: %s", resp.StatusCode, string(body))
	}
	var decomp DecompositionPackage
	if err := json.NewDecoder(resp.Body).Decode(&decomp); err != nil {
		return nil, fmt.Errorf("registry: decoding decomposition package: %w", err)
	}
	return &decomp, nil
}

// FetchDecompositionPresigned fetches a decomposition directly from
// the registry's object storage using a presigned URL. It is the
// fast path for the UI's source detail dialog: the registry's
// PullDecomposition buffers the S3 object in memory and re-marshals
// it (~80-100MB peak for a 19MB payload, blocks the pullSem slot
// for seconds), while this path issues a tiny presigned URL and
// streams the raw bytes straight from R2/S3 to the caller.
//
// Returns the presigned URL and the raw response body. The
// caller's HTTP client should have a reasonable read timeout —
// for very large payloads this avoids holding the registry's
// pullSem during the entire transfer.
//
// Falls back to "" + nil when the registry doesn't issue a
// presigned URL for this decomposition (filesystem backend, dev
// mode, or older registry). Callers should fall back to
// PullDecomposition in that case.
func (c *Client) FetchDecompositionPresigned(ctx context.Context, sourceID, modelID string) (string, []byte, error) {
	if c.baseURL == "" {
		return "", nil, ErrRegistryDisabled
	}
	pkg, err := c.PullSource(ctx, sourceID)
	if err != nil {
		return "", nil, fmt.Errorf("registry: fetching source package for presign: %w", err)
	}
	var ref *DecompRef
	for i := range pkg.Decompositions {
		if pkg.Decompositions[i].ModelID == modelID {
			ref = &pkg.Decompositions[i]
			break
		}
	}
	if ref == nil {
		return "", nil, fmt.Errorf("registry: decomposition %s/%s not found", sourceID, modelID)
	}
	if ref.PresignedURL == "" {
		return "", nil, nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ref.PresignedURL, nil)
	if err != nil {
		return "", nil, fmt.Errorf("registry: creating presigned fetch request: %w", err)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return "", nil, fmt.Errorf("registry: fetching presigned decomposition: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return "", nil, fmt.Errorf("registry: presigned fetch returned status %d: %s", resp.StatusCode, string(body))
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", nil, fmt.Errorf("registry: reading presigned body: %w", err)
	}
	return ref.PresignedURL, body, nil
}

// PushSource pushes a source to the registry. Returns the source ID.
// The content fields (parsedText, parsedMarkdown) are stored in the
// registry's S3 object so pulling repos can import the extracted
// content without re-fetching the URL.
func (c *Client) PushSource(ctx context.Context, sourceURL, doi, sha256, title, parsedText, parsedMarkdown string) (string, error) {
	if c.baseURL == "" {
		return "", ErrRegistryDisabled
	}
	body := map[string]interface{}{
		"url":    sourceURL,
		"doi":    doi,
		"sha256": sha256,
		"title":  title,
		"content": map[string]interface{}{
			"text":    parsedText,
			"markdown": parsedMarkdown,
		},
	}
	b, err := json.Marshal(body)
	if err != nil {
		return "", fmt.Errorf("registry: marshalling push source body: %w", err)
	}
	endpoint := fmt.Sprintf("%s/api/v1/sources", c.baseURL)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(b))
	if err != nil {
		return "", fmt.Errorf("registry: creating push source request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	c.addAuth(req, b, true)
	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("registry: pushing source: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("registry: push source returned status %d: %s", resp.StatusCode, string(body))
	}
	var result struct {
		ID string `json:"source_id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", fmt.Errorf("registry: decoding push source response: %w", err)
	}
	return result.ID, nil
}

// PushDecomposition pushes a decomposition package to the registry.
func (c *Client) PushDecomposition(ctx context.Context, sourceID, modelID string, decomp *DecompositionPackage) (string, error) {
	if c.baseURL == "" {
		return "", ErrRegistryDisabled
	}
	b, err := json.Marshal(decomp)
	if err != nil {
		return "", fmt.Errorf("registry: marshalling decomposition package: %w", err)
	}
	endpoint := fmt.Sprintf("%s/api/v1/sources/%s/decompositions/%s", c.baseURL, url.PathEscape(sourceID), url.PathEscape(modelID))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(b))
	if err != nil {
		return "", fmt.Errorf("registry: creating push decomposition request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	c.addAuth(req, b, true)
	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("registry: pushing decomposition: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("registry: push decomposition returned status %d: %s", resp.StatusCode, string(body))
	}
	var result struct {
		ID string `json:"source_id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", fmt.Errorf("registry: decoding push decomposition response: %w", err)
	}
	return result.ID, nil
}

// RemoteSourceMeta is a source as returned by the registry's ListSources endpoint.
type RemoteSourceMeta struct {
	ID        string `json:"id"`
	RepoID    string `json:"repo_id"`
	URL       string `json:"url,omitempty"`
	DOI       string `json:"doi,omitempty"`
	SHA256    string `json:"sha256,omitempty"`
	Title     string `json:"title,omitempty"`
	S3Key     string `json:"s3_key"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}

// ListSourcesResponse is the paginated response from the registry's ListSources.
type ListSourcesResponse struct {
	Sources []RemoteSourceMeta `json:"sources"`
	Total   int                `json:"total"`
}

// ListSources fetches a paginated, optionally searched list of sources from the
// registry. When query is non-empty it does a LIKE search across title/url/doi.
func (c *Client) ListSources(ctx context.Context, limit, offset int, query string) (*ListSourcesResponse, error) {
	if c.baseURL == "" {
		return nil, ErrRegistryDisabled
	}
	q := make(url.Values)
	q.Set("limit", strconv.Itoa(limit))
	q.Set("offset", strconv.Itoa(offset))
	if query != "" {
		q.Set("q", query)
	}
	endpoint := fmt.Sprintf("%s/api/v1/sources?%s", c.baseURL, q.Encode())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("registry: creating list sources request: %w", err)
	}
	c.addAuth(req, nil, false)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("registry: listing sources: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("registry: list sources returned status %d: %s", resp.StatusCode, string(body))
	}
	var result ListSourcesResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("registry: decoding list sources response: %w", err)
	}
	if result.Total == 0 && len(result.Sources) > 0 {
		result.Total = offset + len(result.Sources)
	}
	return &result, nil
}

func (c *Client) IsAllowedModel(modelID string) bool {
	if c.baseURL == "" {
		return false
	}
	return IsAllowed(c.models, modelID)
}

// AllowedModels returns the global allowed_models list this client
// was configured with. Used by the import workers as the fallback
// when a repo hasn't set a per-repo allowed_models override.
func (c *Client) AllowedModels() []string {
	return c.models
}

// IsAllowed reports whether a model id is in the given whitelist.
// The list follows the same rules as Client.IsAllowedModel: ["*"]
// allows all, an empty list allows none, otherwise exact match.
// This is the package-level helper the import workers use with a
// per-repo resolved list (which may be the per-repo override or the
// global registry config fallback).
func IsAllowed(models []string, modelID string) bool {
	if len(models) == 0 {
		return false
	}
	for _, m := range models {
		if m == "*" || m == modelID {
			return true
		}
	}
	return false
}

// ListContextsResponse is the payload of GET /api/v1/contexts on the
// registry. The registry publishes its canonical context vocabulary
// (the 88 DBpedia L3 labels it seeded at boot) so OKT instances can
// map their local contexts to the registry's set on contribute, and
// translate registry contexts back to local ones on pull.
type ListContextsResponse struct {
	Contexts []string `json:"contexts"`
}

// ListContexts fetches the registry's canonical context vocabulary.
// Returns ErrRegistryDisabled when the client is unconfigured. The
// result is cached per-Client for contextVocabTTL; on a registry
// error, the last-good cached value is returned (best-effort) so a
// transient outage doesn't break ingestion. When the registry is
// reachable but returns an empty list, the cache is populated with
// an empty slice (distinguished from "never fetched" = nil) so the
// caller can apply the "empty vocab = validation off" guard in the
// contribute worker.
func (c *Client) ListContexts(ctx context.Context) ([]string, error) {
	if c.baseURL == "" {
		return nil, ErrRegistryDisabled
	}
	if cached := c.contextVocab.Load(); cached != nil && time.Since(cached.fetchedAt) < contextVocabTTL {
		return cached.labels, nil
	}
	endpoint := fmt.Sprintf("%s/api/v1/contexts", c.baseURL)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return c.fallbackContexts(fmt.Errorf("registry: creating list contexts request: %w", err))
	}
	c.addAuth(req, nil, false)
	resp, err := c.http.Do(req)
	if err != nil {
		return c.fallbackContexts(fmt.Errorf("registry: listing contexts: %w", err))
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return c.fallbackContexts(fmt.Errorf("registry: list contexts returned status %d: %s", resp.StatusCode, string(body)))
	}
	var result ListContextsResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return c.fallbackContexts(fmt.Errorf("registry: decoding list contexts response: %w", err))
	}
	c.contextVocab.Store(&contextVocabCache{labels: result.Contexts, fetchedAt: time.Now()})
	return result.Contexts, nil
}

// fallbackContexts returns the last-good cached context list when a
// fresh fetch fails, so a transient registry outage doesn't break
// the contribute/pull workers. When there's no cached value, the
// original error is returned.
func (c *Client) fallbackContexts(err error) ([]string, error) {
	if cached := c.contextVocab.Load(); cached != nil {
		return cached.labels, nil
	}
	return nil, err
}
