---
id: 1-source-extraction
sidebar_position: 1
title: Stage 1 — Source Extraction
---

# Stage 1: Source Extraction

The first stage takes a URL or DOI, fetches the content through a chain of resolution providers, parses the body into clean text, and persists the source with deterministic sentence offsets that every downstream stage keys into.

Entry point: `(*RetrieveSourceWorker).Work` — `backend/internal/taskmanager/tasks/retrieve_source.go:208`.

## Classification

The input is classified via `fetch.ClassifyURL` (`backend/internal/providers/fetch/classify.go`) into a `Resource{Value, Type, DOI}`. A bare DOI from the MCP path synthesizes a `SourceDOI` type. The classification determines which resolution providers are tried.

## The fetch strategy

`(*FetchStrategy).Resolve` — `backend/internal/providers/fetch/strategy.go:192` — runs a chain of `ResolutionProvider` implementations, each with a 60-second per-provider timeout:

| Provider | File | What it does |
|----------|------|--------------|
| `fetch` (HTTP) | `providers/fetch/http_fetch.go:150` | Plain HTTP GET + Trafilatura body extraction |
| `unpaywall` | `providers/fetch/unpaywall.go` | DOI open-access lookup; discovers OA URLs |
| `tls` (TLS impersonation) | `providers/fetch/tls_impersonation.go` | Custom TLS fingerprint to mimic a real browser |
| `flaresolverr` | `providers/fetch/flaresolverr.go` | Headless browser for JS challenges (Cloudflare, Datadome) |

Provider order: static host overrides first, then chain order (`orderedProviders` at `strategy.go:126`). If a provider returns insufficient content (`MinExtractedLength = 200` chars, or `IsJSBoilerplate`), the chain falls through to a heavier tier. This tiered design means a simple blog post resolves with plain HTTP, while a Cloudflare-protected PDF needs the headless browser.

### Two-pass OA retry

When Unpaywall discovers a direct OA URL but can't fetch it, the strategy retries URL-capable providers against that URL (`strategy.go:262-292`). This handles the common case where a DOI's landing page is paywalled but the full-text PDF is on a different, open host.

### Content parsing

Each provider calls a `content_parsing.Parser` (`Parse`) on the body. The default parser is `content_parsing.NewTrafilaturaParser()` (`providers/content_parsing/trafilatura.go`). PDF support via `providers/content_parsing/pdf.go` (Fitz/MuPDF). The parser returns a `ParsedDoc` with title, text, markdown, author, sitename, language, and published date.

## Registry pre-check

Before fetching, `tryRegistryImport` (~`retrieve_source.go:278`) checks whether the optional knowledge registry has pre-computed artifacts for this URL/DOI. If so, it imports them directly and skips the fetch — jumping straight to `deduplicate_facts` + `summarize_concepts`. This avoids re-decomposing a source someone already processed.

## Persistence

`(*RetrieveSourceWorker).persistSource` — `retrieve_source.go:960`:

- Upserts the `okt_repository.sources` row (status `fetching` -> `fetched`/`failed`, `parsed_*` columns, `doi`, `published_at`, `storage_key`).
- `MarkSourceParsed` writes `parsed_title/text/html/markdown/author/sitename/language/published_at/parse_status`.
- `buildSentenceOffsets` (`retrieve_source.go:1749`) runs `decomposition.SegmentSentences` over the markdown (preferred) or text and stores the deterministic global sentence array in `sources.sentence_offsets` (`retrieve_source.go:1423`, `SetSentenceOffsets`). **This is the stable contract that `fact_references.sentence_index` keys into** — a fact cites sentences by their index in this global array.
- Inline images and PDF page renders go to `okt_repository.source_images` (migration 0008/0017); binary bodies are persisted via `providers/storage` (filesystem or S3).

## Chain out

When `args.Process` is true and the source has parsed text, the worker enqueues `source_decomposition` (`retrieve_source.go:466`), passing the source ID and repository ID.

## Key tables

| Table | Purpose |
|-------|---------|
| `okt_repository.sources` | The source row: URL, status, parsed content, sentence_offsets, DOI, storage info |
| `okt_repository.source_images` | Inline images + PDF page renders extracted from the source |

See [Architecture > Schema](/docs/architecture/schema) for full column details.