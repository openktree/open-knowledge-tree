package search

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/openktree/open-knowledge-tree/backend/internal/config"
)

const serperURL = "https://google.serper.dev/search"

// serperDefaultPerPage is the page size we ask Serper for when the
// caller does not supply one. Serper's own default is 10; we keep
// the same value so an unspecified search behaves the way it did
// before pagination was added.
const serperDefaultPerPage = 10

// serperMaxPerPage caps the page size we forward to Serper. The
// upstream hard-cap is 100; we enforce it here so a misbehaving
// caller can't ask for a huge page and burn through API budget.
const serperMaxPerPage = 100

// SerperSearchProvider implements SearchProvider against the Serper
// Google Search API (https://google.serper.dev/search).
type SerperSearchProvider struct {
	apiKey     string
	httpClient *http.Client
}

func NewSerperSearchProvider(apiKey string) *SerperSearchProvider {
	return &SerperSearchProvider{
		apiKey: apiKey,
		httpClient: &http.Client{
			Timeout: 15 * time.Second,
		},
	}
}

func NewSerperSearchProviderFromConfig(serperCfg config.SerperProviderConfig) *SerperSearchProvider {
	return NewSerperSearchProvider(serperCfg.APIKey)
}

type serperRequest struct {
	Query string `json:"q"`
	Num   int    `json:"num,omitempty"`
	Page  int    `json:"page,omitempty"`
}

type serperResponse struct {
	Organic []struct {
		Title   string `json:"title"`
		Link    string `json:"link"`
		Snippet string `json:"snippet"`
	} `json:"organic"`
}

func (p *SerperSearchProvider) Search(ctx context.Context, query string, opts SearchOptions) (SearchResponse, error) {
	perPage := opts.PerPage
	if perPage <= 0 {
		perPage = serperDefaultPerPage
	}
	if perPage > serperMaxPerPage {
		perPage = serperMaxPerPage
	}

	// Serper's `page` is 1-indexed. We treat an empty cursor as
	// page 1; otherwise we parse the cursor as the page number the
	// caller wants. A non-integer cursor falls back to page 1
	// (friendlier than erroring — the worst case is the first page
	// is returned again, which is recoverable by the caller).
	page := 1
	if opts.Cursor != "" {
		if n, err := strconv.Atoi(opts.Cursor); err == nil && n > 0 {
			page = n
		}
	}

	body, err := json.Marshal(serperRequest{Query: query, Num: perPage, Page: page})
	if err != nil {
		return SearchResponse{}, fmt.Errorf("marshaling serper request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, serperURL, bytes.NewReader(body))
	if err != nil {
		return SearchResponse{}, fmt.Errorf("creating serper request: %w", err)
	}
	req.Header.Set("X-API-KEY", p.apiKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := p.httpClient.Do(req)
	if err != nil {
		return SearchResponse{}, fmt.Errorf("serper request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return SearchResponse{}, fmt.Errorf("serper returned status %d: %s", resp.StatusCode, string(b))
	}

	var result serperResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return SearchResponse{}, fmt.Errorf("decoding serper response: %w", err)
	}

	results := make([]SearchResult, 0, len(result.Organic))
	for _, r := range result.Organic {
		results = append(results, SearchResult{
			Title:   r.Title,
			URL:     r.Link,
			Snippet: r.Snippet,
		})
	}

	// Serper does not return a total result count, so Total stays
	// 0 (the caller treats 0 as "unknown"). NextCursor is the next
	// page number as a string, but only when the current page
	// returned results — an empty `organic` array is Serper's
	// signal that the query is past the end, so we stop paging
	// there to avoid an infinite "load more" loop in the UI.
	var nextCursor string
	if len(results) > 0 {
		nextCursor = strconv.Itoa(page + 1)
	}

	return SearchResponse{
		Results:    results,
		Total:      0,
		NextCursor: nextCursor,
	}, nil
}