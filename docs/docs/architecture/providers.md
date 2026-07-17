---
id: providers
sidebar_position: 3
title: Providers
---

# Providers

OKT uses the strategy/adapter pattern for external integrations. All providers are transport-agnostic (live in `internal/providers/`) and are registered in `backend/cmd/app/api.go`.

## Search providers

`backend/internal/providers/search/` — the `SearchProvider` interface:

```go
type SearchProvider interface {
    Search(ctx, query, opts) ([]SearchResult, error)
    ID() string
}
```

| Provider | File | Description |
|----------|------|-------------|
| `serper` | `search/serper.go` | Google web search (Serper API) |
| `openalex` | `search/openalex.go` | Academic works (OpenAlex) |

Each is env-gated (`SERPER_API_KEY`, `OPENALEX_EMAIL`). Repos can disable individual providers via per-repo settings.

## Fetch resolution providers

`backend/internal/providers/fetch/` — the `ResolutionProvider` interface. The `FetchStrategy` (`fetch/strategy.go:192`) chains them:

| Provider | File | What it does |
|----------|------|--------------|
| `fetch` | `fetch/http_fetch.go:150` | Plain HTTP + Trafilatura |
| `unpaywall` | `fetch/unpaywall.go` | DOI open-access lookup |
| `tls` | `fetch/tls_impersonation.go` | Custom TLS fingerprint |
| `flaresolverr` | `fetch/flaresolverr.go` | Headless browser |

Provider order: static host overrides first, then chain order. Each has a 60s timeout. If a provider returns insufficient content (< 200 chars), the chain falls through to a heavier tier. See [Knowledge Flow > Source Extraction](/docs/reference/knowledge-flow/1-source-extraction).

## Content parsers

`backend/internal/providers/content_parsing/`:
- `TrafilaturaParser` — default, extracts main content from HTML.
- `FitzPDFParser` — MuPDF-backed PDF parsing.

## AI providers

`backend/internal/providers/ai/` — the `EmbeddingProvider`, `ChatProvider`, and `MultiModalProvider` interfaces:

| Provider | Purpose |
|----------|---------|
| `ollama` | Local Ollama (chat + embedding) |
| `ollama_cloud` | Ollama Cloud |
| `openrouter` | OpenRouter (chat, embedding, multimodal) |

## Decomposition providers

`backend/internal/providers/decomposition/`:
- `SimpleChunkingProvider` — 2000-rune sliding window.
- `AIFactExtractionProvider` — LLM fact extraction (text).
- `ImageFactExtractionProvider` — multimodal fact extraction (images).
- `ConceptExtractor` — LLM concept + context extraction.
- `AliasProvider` — LLM canonical name + alias generation.

## Summarization & synthesis providers

- `AISummarizationProvider` (`internal/providers/summarization/`) — per-concept summary slices.
- `AISynthesisProvider` (`internal/providers/synthesis/`) — canonical-name group synthesis + image picker.

## Ontology source

`backend/internal/providers/ontology/` — the embedded DBpedia L3 class list (`dbpedia_l3.json`). This is the default context vocabulary for concept extraction. Refresh via `just dbpedia-pull`.

## Storage

`backend/internal/providers/storage/` — filesystem or S3 for source assets (inline images, PDF page renders, full PDF bodies).

## Registering a new provider

1. Implement the interface in `internal/providers/search/<name>.go` or `internal/providers/fetch/<name>.go`.
2. Register it in `backend/cmd/app/api.go` and pass it to `handler.NewSource(...)` (attached via `h.SetSource(...)`).