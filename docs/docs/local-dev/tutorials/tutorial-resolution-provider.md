---
id: tutorial-resolution-provider
sidebar_position: 2
title: Adding a Resolution Provider
---

# Tutorial: Adding a Resolution Provider

Resolution providers fetch the actual content of a source URL. OKT ships with HTTP fetch, Unpaywall (DOI → open-access PDF), TLS impersonation, and FlareSolverr (headless browser). This tutorial shows how to add a new one.

## The interface

```go
// backend/internal/providers/fetch/resolution.go
type ResolutionProvider interface {
    Resolve(ctx context.Context, resource Resource) (ResolvedContent, error)
    Supports(sourceType SourceType) bool
    Describe() ProviderDescription
}

type Resource struct {
    Value string       // The URL or DOI
    Type  SourceType   // SourceURL or SourceDOI
    DOI   string       // Bare DOI when known
}
```

The `Resolve` method returns `ResolvedContent` containing the raw body, content type, final URL after redirects, and a parsed `ParsedDoc` (sentences with offsets). Return `ErrInsufficientContent` or `ErrBodyTooLarge` to tell the strategy to try the next provider instead of failing hard.

## Step 1: Create the provider file

Create `backend/internal/providers/fetch/wayback.go`:

```go
package fetch

import (
    "context"
    "encoding/json"
    "fmt"
    "io"
    "net/http"
    "time"

    "github.com/openktree/open-knowledge-tree/backend/internal/providers/content_parsing"
)

type WaybackResolutionProvider struct {
    httpClient *http.Client
    parsers    []content_parsing.Parser
}

func NewWaybackResolutionProvider(parsers ...content_parsing.Parser) *WaybackResolutionProvider {
    if len(parsers) == 0 {
        parsers = []content_parsing.Parser{content_parsing.NewTrafilaturaParser()}
    }
    return &WaybackResolutionProvider{
        httpClient: &http.Client{Timeout: 30 * time.Second},
        parsers:    parsers,
    }
}

func (p *WaybackResolutionProvider) Supports(sourceType SourceType) bool {
    return sourceType == SourceURL
}

func (p *WaybackResolutionProvider) Describe() ProviderDescription {
    return ProviderDescription{
        Name:        "wayback",
        Description: "Wayback Machine (Internet Archive) — fetches archived snapshots of URLs",
        Requires:    "Nothing — public API, no key needed",
        Configured:  true,
        Supports:    []string{"url"},
        Timeout:     "30s",
        Notes:       "Useful for pages that are no longer live.",
    }
}

func (p *WaybackResolutionProvider) Resolve(ctx context.Context, resource Resource) (ResolvedContent, error) {
    // Check availability via the Availability API
    availURL := fmt.Sprintf("https://archive.org/wayback/available?url=%s", resource.Value)
    req, err := http.NewRequestWithContext(ctx, http.MethodGet, availURL, nil)
    if err != nil {
        return ResolvedContent{}, err
    }

    resp, err := p.httpClient.Do(req)
    if err != nil {
        return ResolvedContent{}, err
    }
    defer resp.Body.Close()

    var avail struct {
        ArchivedSnapshots struct {
            Closest struct {
                URL   string `json:"url"`
                Valid bool   `json:"available"`
            } `json:"closest"`
        } `json:"archived_snapshots"`
    }
    if err := json.NewDecoder(resp.Body).Decode(&avail); err != nil {
        return ResolvedContent{}, err
    }
    if !avail.ArchivedSnapshots.Closest.Valid {
        return ResolvedContent{}, ErrInsufficientContent
    }

    // Fetch the archived snapshot
    snapResp, err := p.httpClient.Get(avail.ArchivedSnapshots.Closest.URL)
    if err != nil {
        return ResolvedContent{}, err
    }
    defer snapResp.Body.Close()

    body, err := io.ReadAll(io.LimitReader(snapResp.Body, MaxBodyBytes))
    if err != nil {
        return ResolvedContent{}, err
    }

    // Parse content using the standard parsers
    parsed := content_parsing.ParsedDoc{}
    for _, parser := range p.parsers {
        if p, err := parser.Parse(body); err == nil && len(p.Sentences) > len(parsed.Sentences) {
            parsed = *p
        }
    }

    return ResolvedContent{
        Body:        body,
        ContentType: snapResp.Header.Get("Content-Type"),
        StatusCode:  snapResp.StatusCode,
        FinalURL:    avail.ArchivedSnapshots.Closest.URL,
        Parsed:      parsed,
    }, nil
}
```

## Step 2: Register it in the fetch strategy

In `backend/cmd/app/api.go`, find where the fetch strategy is built and add:

```go
wayback := fetch.NewWaybackResolutionProvider()
// Add to the strategy chain after the existing providers
```

The exact placement depends on where in the chain you want Wayback to sit. After the plain HTTP fetch but before FlareSolverr is typical — try the live page first, then check the archive.

## Step 3: Add config (optional)

If your provider needs configuration (API keys, timeouts), add a config block in `config.go` and `config.default.yaml` following the same pattern as the existing providers:

```yaml
providers:
  resolution:
    wayback:
      enabled: true
      timeout: 30s
```

## Step 4: Add an e2e test

```go
//go:build e2e

package e2e

import (
    "testing"

    "github.com/openktree/open-knowledge-tree/backend/internal/providers/fetch"
)

func TestWaybackResolutionProvider_Resolve(t *testing.T) {
    p := fetch.NewWaybackResolutionProvider()
    resource := fetch.Resource{
        Value: "https://example.com",
        Type:  fetch.SourceURL,
    }
    resp, err := p.Resolve(t.Context(), resource)
    if err != nil {
        t.Fatalf("Resolve failed: %v", err)
    }
    if len(resp.Body) == 0 {
        t.Fatal("expected non-empty body")
    }
}
```

## Summary

| File | Change |
|------|--------|
| `backend/internal/providers/fetch/wayback.go` | New file — implements `ResolutionProvider` |
| `backend/cmd/app/api.go` | Register in the fetch strategy chain |
| `backend/internal/config/config.go` | Add config struct (if needed) |
| `backend/configs/config.default.yaml` | Add config block (if needed) |
| `backend/e2e/wayback_test.go` | New file — e2e test |
