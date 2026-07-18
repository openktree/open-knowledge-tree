---
id: tutorial-search-provider
sidebar_position: 1
title: Adding a Search Provider
---

# Tutorial: Adding a Search Provider

Search providers find candidate sources by URL or DOI. OKT ships with [Serper](https://serper.dev) (Google web search) and [OpenAlex](https://openalex.org) (academic works). This tutorial shows how to add a new one — for example, [Semantic Scholar](https://www.semanticscholar.org/product/api#api-reference).

## The interface

Every search provider implements a single method:

```go
// backend/internal/providers/search/search.go
type SearchProvider interface {
    Search(ctx context.Context, query string, opts SearchOptions) (SearchResponse, error)
}
```

`SearchResponse` carries `[]SearchResult` (title, URL, snippet, optional DOI/OpenAlexID/PublishedAt) plus pagination (`NextCursor`, `Total`).

## Step 1: Create the provider file

Create `backend/internal/providers/search/semanticscholar.go`:

```go
package search

import (
    "context"
    "encoding/json"
    "fmt"
    "io"
    "net/http"
    "net/url"
    "time"
)

type SemanticScholarSearchProvider struct {
    apiKey     string
    httpClient *http.Client
}

func NewSemanticScholarSearchProvider(apiKey string) *SemanticScholarSearchProvider {
    return &SemanticScholarSearchProvider{
        apiKey: apiKey,
        httpClient: &http.Client{
            Timeout: 15 * time.Second,
        },
    }
}

func (p *SemanticScholarSearchProvider) Search(ctx context.Context, query string, opts SearchOptions) (SearchResponse, error) {
    perPage := opts.PerPage
    if perPage <= 0 { perPage = 10 }
    if perPage > 100 { perPage = 100 }

    apiURL := fmt.Sprintf(
        "https://api.semanticscholar.org/graph/v1/paper/search?query=%s&limit=%d&fields=title,url,abstract,externalIds,publicationDate",
        url.QueryEscape(query), perPage,
    )

    req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL, nil)
    if err != nil {
        return SearchResponse{}, err
    }
    if p.apiKey != "" {
        req.Header.Set("x-api-key", p.apiKey)
    }

    resp, err := p.httpClient.Do(req)
    if err != nil {
        return SearchResponse{}, err
    }
    defer resp.Body.Close()

    if resp.StatusCode != http.StatusOK {
        b, _ := io.ReadAll(resp.Body)
        return SearchResponse{}, fmt.Errorf("semantic scholar returned %d: %s", resp.StatusCode, b)
    }

    var result struct {
        Total   int64 `json:"total"`
        Data []struct {
            Title       string `json:"title"`
            URL         string `json:"url"`
            Abstract    string `json:"abstract"`
            ExternalIds struct {
                DOI string `json:"DOI"`
            } `json:"externalIds"`
            PublicationDate string `json:"publicationDate"`
        } `json:"data"`
    }
    if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
        return SearchResponse{}, err
    }

    results := make([]SearchResult, 0, len(result.Data))
    for _, r := range result.Data {
        sr := SearchResult{
            Title:   r.Title,
            URL:     r.URL,
            Snippet: r.Abstract,
        }
        if r.ExternalIds.DOI != "" {
            sr.DOI = r.ExternalIds.DOI
        }
        results = append(results, sr)
    }

    return SearchResponse{
        Results: results,
        Total:   result.Total,
    }, nil
}
```

## Step 2: Add a config block

In `backend/internal/config/config.go`, add to `SearchProvidersConfig`:

```go
type SemanticScholarProviderConfig struct {
    APIKey string `mapstructure:"api_key"`
}
```

Add the field to `SearchProvidersConfig`:

```go
type SearchProvidersConfig struct {
    Provider        string                       `mapstructure:"provider"`
    Serper          SerperProviderConfig          `mapstructure:"serper"`
    OpenAlex        OpenAlexProviderConfig        `mapstructure:"openalex"`
    SemanticScholar SemanticScholarProviderConfig `mapstructure:"semantic_scholar"`  // new
}
```

In `backend/configs/config.default.yaml`, add under `providers.search`:

```yaml
providers:
  search:
    provider: "serper"
    serper:
      api_key: ""
    openalex:
      email: ""
    semantic_scholar:
      api_key: ""
```

## Step 3: Register it in the composition root

In `backend/cmd/app/api.go`, add to the `searchProviders` map:

```go
s2Key := cfg.Providers.Search.SemanticScholar.APIKey
if s2Key == "" {
    s2Key = os.Getenv("SEMANTICSCHOLAR_API_KEY")
}
if s2Key != "" {
    searchProviders["semantic_scholar"] = search.NewSemanticScholarSearchProvider(s2Key)
}
```

That's it. The handler, MCP tools, and per-repository settings all pick it up automatically from the shared `searchProviders` map.

## Step 4: Add an e2e test

Create `backend/e2e/semanticscholar_test.go`:

```go
//go:build e2e

package e2e

import (
    "os"
    "testing"

    "github.com/openktree/open-knowledge-tree/backend/internal/providers/search"
)

func TestSemanticScholarSearchProvider_Search(t *testing.T) {
    apiKey := os.Getenv("SEMANTICSCHOLAR_API_KEY")
    if apiKey == "" {
        t.Skip("SEMANTICSCHOLAR_API_KEY not set")
    }

    p := search.NewSemanticScholarSearchProvider(apiKey)
    resp, err := p.Search(t.Context(), "CRISPR gene editing", search.SearchOptions{PerPage: 5})
    if err != nil {
        t.Fatalf("Search failed: %v", err)
    }
    if len(resp.Results) == 0 {
        t.Fatal("expected at least one result")
    }
    t.Logf("got %d results, first: %s", len(resp.Results), resp.Results[0].Title)
}
```

## Step 5: Add an env var for the key

Add to your `.env`:

```
SEMANTICSCHOLAR_API_KEY=your-key
```

The key is optional — the provider self-skips when empty, so the stack boots fine without it.

## Summary

| File | Change |
|------|--------|
| `backend/internal/providers/search/semanticscholar.go` | New file — implements `SearchProvider` |
| `backend/internal/config/config.go` | Add `SemanticScholarProviderConfig` struct + field |
| `backend/configs/config.default.yaml` | Add `semantic_scholar:` block under `providers.search` |
| `backend/cmd/app/api.go` | Instantiate and add to `searchProviders` map |
| `backend/e2e/semanticscholar_test.go` | New file — env-gated e2e test |
| `.env` | Add `SEMANTICSCHOLAR_API_KEY` |
