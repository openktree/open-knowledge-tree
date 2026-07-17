# Image Fact Extraction (multimodal decomposition)

## Goal

Extend the decomposition pipeline so that images attached to a source are
processed alongside the parsed text. Each image is sent to a configured
**multimodal** model (default Gemma 3 27B via OpenRouter) together with the
source URL, source title, and the image's alt text. The model returns
**fact-oriented** descriptions (not generic "this is a photo of…" captions),
which become **image facts** — facts that carry an `image_url` so the
frontend can render the picture next to the fact text when a user views a
topic.

Image facts flow through the **existing** embedding → dedup → cleanup chain
unchanged: only the `facts.text` is embedded, so image facts dedup against
text facts and each other on semantic content.

## Context (verified against current code)

- Pipeline: `retrieve_source` → persists `source_images` (`kind` `inline`
  with url+alt, or `page` with PNG `bytes`) → `source_decomposition` chunks
  **only `parsed_text`**, calls `AIFactExtractionProvider.ExtractFacts`
  (text-only prompt) → `CreateFact` + `AddFactSource(chunk_index)` → chains
  `embed_facts` → `deduplicate_facts` → `cleanup_facts`.
- Images are stored today but **never used** in decomposition.
- `ChatMessage.Content` is a `string` (`ai/ai.go:12`); OpenRouter/Ollama
  serialize it as plain text. No multimodal path exists.
- `ParsedDoc.Images` = `[]ImageRef{URL, Alt}` (trafilatura, inline);
  `ParsedDoc.PageImages` = `[]PageImage{Page, Bytes, Format}` (PDF only).
  Persisted to `source_images` with `kind` inline/page.
- Config has `decomposition.fact_extraction.{provider, model}` only; no image
  extraction section.
- `fact_sources.chunk_index` is `int32`; text chunks are 0-based, so `-1` is
  a safe sentinel for image facts (no junction schema change needed).

## Locked decisions

| # | Decision | Value |
|---|----------|-------|
| D1 | Image fact representation | **New `facts.image_url` (nullable TEXT) + `facts.fact_kind` (`'text'`/`'image'`, default `'text'`).** Migration `0015`. Frontend renders `<img>` from the column; dedup embeds only `facts.text`. |
| D2 | Extraction placement | **Inside `source_decomposition`.** Text chunks first, then images, in the same job. Single `embed_facts` chain. |
| D3 | Multimodal provider surface | **Extend `ChatMessage` with content parts** (string fast path + `[]ContentPart`). All providers emit the right wire shape; text-only callers unchanged. No new interface. |
| D4 | Inline image bytes source | **HTTP fetch via a `FetchImageBytes` helper on the existing `FetchResolutionProvider`** (reuses browser-like headers + configured client). Page images use DB bytes directly. No new provider. |
| D5 | Image fact `chunk_index` | **`-1`** (sentinel). Distinguishes image facts from text facts at the junction level without a schema change there. `ListFactsBySource` ORDER BY `chunk_index` puts image facts last. |
| D6 | Frontend scope | **Backend-only for this plan.** API returns `fact_kind` + `image_url` on fact rows; frontend rendering is a follow-up. |

## Prompt (fact-oriented, not descriptive)

Reuse the structure of the text `fact_extraction` system prompt but scoped to
images. Key rules:

- Extract only atomic, self-contained facts the image conveys **and that are
  relevant to the source topic** (`{source_title}`, `{source_url}`).
- Extract: data points from charts/graphs (with units + axis labels), named
  entities depicted, quantitative measurements, procedure steps shown,
  relationships/flows, quoted text rendered in the image, definitions or
  classifications illustrated.
- **Do NOT** produce generic image descriptions ("This image shows…",
  "A photo of…", "The image depicts…"). No aesthetic/quality observations,
  no layout/navigation chrome, no brand logos, no decorative elements.
- Each fact 1-2 sentences, self-contained (resolve pronouns/demonstratives,
  name the subject and context), verifiable from the image alone.
- If the image is purely decorative or carries no topical signal, return
  `[]`.
- Image alt text (provided by the source page) is passed in as a hint; do not
  merely restate it.
- Response format: `JSON array of strings`, `[]` when nothing extractable,
  ONLY the JSON array.

The image bytes + alt text + source title/url go in as the user message
(multimodal). Returned strings become `facts.text`; `facts.image_url` is set
by the **worker** (not the model) from the image row's URL.

## Changes by layer

### 1. AI layer — `backend/internal/providers/ai/`

**`ai.go`** — extend `ChatMessage` to a union:

```go
type ChatMessage struct {
    Role    string        `json:"role"`
    Content string        `json:"content,omitempty"`     // text fast path
    Parts   []ContentPart `json:"parts,omitempty"`       // multimodal
}
type ContentPart struct {
    Type     string `json:"type"`              // "text" | "image_url"
    Text     string `json:"text,omitempty"`
    ImageURL *struct {
        URL string `json:"url"`                 // data:image/...;base64,...
    } `json:"image_url,omitempty"`
}
func (m ChatMessage) IsMultimodal() bool { return len(m.Parts) > 0 }
```

- Custom `MarshalJSON`: emit `content` (string) when `Parts` empty
  (existing text path), else emit the content-parts array (OpenAI vision
  wire format: `content: [{type:"text",...},{type:"image_url",image_url:{url:...}}]`).
- Helper `NewImageMessage(role, text string, images []ImageData) ChatMessage`
  where `ImageData{URL string; Bytes []byte; ContentType string}` — builds
  base64 data URLs for each image.

**`openrouter.go`** — OpenRouter speaks OpenAI vision natively; the custom
`MarshalJSON` on `ChatMessage` produces the correct request body. No
provider-specific change beyond ensuring the request struct uses
`[]ChatMessage` (already does).

**`ollama.go` + `ollama_cloud.go`** — Ollama's message format is
`{role, content, images: [base64...]}`. Detect `IsMultimodal()` on each
message and, locally for the Ollama request body, split parts into
`content` (concatenated text parts) + `images` (base64 bytes without the
`data:` prefix). Override the request struct locally; do not change the
shared `ChatMessage` shape.

No new interface — `AIProvider.Chat` already accepts `[]ChatMessage`.

### 2. Fetch layer — `backend/internal/providers/fetch/image.go`

```go
// FetchImageBytes fetches an image URL with browser-like headers (mirrors
// FetchResolutionProvider's header set + configured client) and returns the
// raw bytes + detected content-type. Caps at maxBytes; returns
// ErrImageTooLarge on overflow.
func (p *FetchResolutionProvider) FetchImageBytes(ctx context.Context, url string, maxBytes int64) ([]byte, string, error)
```

- Reuses the provider's `*http.Client` and header defaults for consistency
  with how the source page was fetched.
- Sniffs content-type from the response (fall back to URL extension).
- No new provider; this is a method on the existing fetch provider.

### 3. Decomposition layer — `backend/internal/providers/decomposition/image_fact_extraction.go`

```go
type ImageFactRequest struct {
    SourceURL   string
    SourceTitle string
    ImageAlt    string
    ImageURL    string                 // canonical URL → facts.image_url
    ImageBytes  []byte                 // from DB (page) or FetchImageBytes (inline)
    Attribution FactExtractionAttribution
}
type ImageFactExtractionProvider interface {
    ExtractImageFacts(ctx context.Context, db store.DBTX, req ImageFactRequest) ([]string, error)
    Describe() ProviderDescription
}
type AIImageFactExtractionProvider struct {
    ai    ai.AIProvider
    model string
    fetch interface {
        FetchImageBytes(ctx context.Context, url string, maxBytes int64) ([]byte, string, error)
    }
    maxImageBytes int64
}
```

- `ExtractImageFacts` builds the multimodal user message (text prompt with
  `{source_title}`/`{source_url}`/`{alt}` substituted + image bytes part),
  calls `ai.Chat` with `Attribution.Operation = "image_extraction"`, parses
  the JSON array of strings (reuse `cleanJSONArray` from the text extractor).
- `Describe()` reports `Configured = aiDesc.Configured && p.Model != ""`.

### 4. Config — `backend/internal/config/config.go` + `backend/configs/config.default.yaml`

YAML (new sibling under `decomposition`):

```yaml
decomposition:
  chunking: { chunk_size: 2000, chunk_overlap: 200 }
  fact_extraction: { provider: "openrouter", model: "google/gemma-4-31b-it" }
  image_extraction:
    enabled: true
    provider: "openrouter"
    model: "google/gemma-3-27b-it"
    max_image_bytes: 5242880       # 5 MB
    max_images_per_source: 20
```

Go:

```go
type DecompositionImageConfig struct {
    Enabled            bool   `mapstructure:"enabled"`
    Provider           string `mapstructure:"provider"`
    Model              string `mapstructure:"model"`
    MaxImageBytes      int64  `mapstructure:"max_image_bytes"`
    MaxImagesPerSource int    `mapstructure:"max_images_per_source"`
}
```

Add `ImageExtraction DecompositionImageConfig` to
`DecompositionProvidersConfig`.

### 5. Migration — `backend/db/migrations/0015_facts_image.up.sql` + `.down.sql`

```sql
-- up
ALTER TABLE okt_repository.facts
    ADD COLUMN IF NOT EXISTS image_url TEXT,
    ADD COLUMN IF NOT EXISTS fact_kind TEXT NOT NULL DEFAULT 'text'
        CHECK (fact_kind IN ('text','image'));
UPDATE okt_repository.facts SET fact_kind = 'text' WHERE fact_kind IS NULL;

-- down
ALTER TABLE okt_repository.facts
    DROP COLUMN IF EXISTS image_url,
    DROP COLUMN IF EXISTS fact_kind;
```

Idempotent (`IF NOT EXISTS`); same file runs against every database per the
multi-DB rules.

### 6. sqlc queries — `backend/db/queries/facts.sql`

- `CreateFact` params: add `FactKind string` + `ImageUrl pgtype.Text`
  (nullable). `INSERT (id, text, fact_kind, image_url)`.
- `ListFactsByRepoWithSourceCount`, `ListFactsBySource`, `GetFactByID`:
  add `fact_kind` + `image_url` to the SELECT lists.
- Run `sqlc generate` → `store/models.go` `OktRepositoryFact` gains
  `FactKind string` + `ImageUrl pgtype.Text`.

### 7. `source_decomposition` worker — `backend/internal/taskmanager/tasks/source_decomposition.go`

Worker gets new fields:

```go
imageExtractor  decomposition.ImageFactExtractionProvider
fetchProvider   interface {
    FetchImageBytes(ctx context.Context, url string, maxBytes int64) ([]byte, string, error)
}
imageCfg        config.DecompositionImageConfig
```

Flow change (after the text-chunk loop, **before** `MarkSourceProcessed`):

1. If `imageExtractor == nil` or `!imageCfg.Enabled` → skip (current
   behavior preserved).
2. `queries.ListSourceImages(ctx, sourceID)`.
3. Cap at `imageCfg.MaxImagesPerSource` (log + truncate).
4. For each image:
   - `kind == "page"` → `bytes = image.Bytes`.
   - `kind == "inline"` → `bytes, ct, err = fetchProvider.FetchImageBytes(ctx, image.Url, imageCfg.MaxImageBytes)`; on error, log + continue (matches per-chunk text error tolerance).
   - Build `ImageFactRequest{SourceURL: source.Url, SourceTitle: deref(source.ParsedTitle), ImageAlt: deref(image.AltText), ImageURL: image.Url, ImageBytes: bytes, Attribution: {RepositoryID, SourceID, TaskID}}`.
   - `facts, err := imageExtractor.ExtractImageFacts(ctx, pool.Pool, req)`; on error, log + continue.
   - For each fact: `CreateFact{Text, FactKind: "image", ImageUrl: &image.Url}` + `AddFactSource(chunk_index = -1)`.
5. `totalFacts += len(imageFacts)`; `MarkSourceProcessed`; chain `embed_facts`
   if `totalFacts > 0` (unchanged gate).

Add `Images int` to `SourceDecompositionResult`.

### 8. Wiring — `backend/cmd/app/api.go` + `backend/internal/taskmanager/taskmanager.go`

- `api.go`: after building the text `factExtractor`, build `imageExtractor`
  from `cfg.Providers.Decomposition.ImageExtraction`:
  - Look up `aiProviders[imgCfg.Provider]` (same map as text extraction).
  - `decomposition.NewAIImageFactExtractionProvider(aiProv, imgCfg.Model, fetchResolutionProvider, imgCfg.MaxImageBytes)`.
  - Nil when `!imgCfg.Enabled` or provider missing (graceful degradation,
    matches existing nil-provider pattern).
- `taskmanager.New`: add `imageExtractor decomposition.ImageFactExtractionProvider`
  + `fetchProvider` + `imageCfg config.DecompositionImageConfig` params;
  pass into `NewSourceDecompositionWorker`.
- `handler.Source`: add `ImageFactExtractors` map (parallel to
  `FactExtractors`); `ListDecompositionProviders` adds an `image_extraction`
  entry to the response.

### 9. Handler/API — `backend/internal/api/handler/source.go`

- `ListFacts`, `ListRepoFacts`, `GetFact`: include `fact_kind` + `image_url`
  in the JSON response (available on the sqlc-regenerated model). No new
  endpoints.
- `ProcessSource` unchanged — still enqueues `source_decomposition`; the
  worker handles both text and image extraction.

### 10. Tests — `backend/e2e/image_extraction_test.go` (`//go:build e2e`)

- Skip when the configured image-extraction model/provider key is unset
  (mirror `serper_test.go`'s env-gated skip).
- **Happy path**: create a source with one inline image (seed
  `source_images` directly, or retrieve a known HTML fixture), trigger
  decomposition, assert `facts` contains rows with `fact_kind='image'` and
  `image_url` set, and `fact_sources.chunk_index = -1`. Assert text facts are
  still produced (mixed source).
- **Error path**: image URL 404 → that image skipped (log), source still
  marked `processed`, text facts still present, `totalFacts` excludes the
  failed image.
- **Decorative image path**: image with no extractable content → model
  returns `[]`, no image fact row, source still `processed`.
- Update any test asserting the full `ListDecompositionProviders` response
  shape to include the new `image_extraction` entry.
- Update `source_decomposition` test (if present) to verify the new
  `Images` field on `SourceDecompositionResult`.

Verify:

```bash
cd backend && OKT_TEST_DATABASE_URL="postgres://okt:okt_dev@localhost:5432/okt?sslmode=disable" \
  go test -count=1 -tags=e2e -timeout 180s -skip TestSerperSearchProvider_Search ./e2e/...
```

## Execution order

1. Config (`config.go` + `config.default.yaml`) + migration `0015` +
   `db/queries/facts.sql` changes + `sqlc generate` (foundation).
2. Multimodal `ChatMessage` + `NewImageMessage` + provider `MarshalJSON`
   updates (ai layer).
3. `FetchResolutionProvider.FetchImageBytes` (fetch layer).
4. `ImageFactExtractionProvider` + prompt (decomposition layer).
5. `source_decomposition` worker changes + `CreateFact` query update.
6. Wiring (`api.go`, `taskmanager.go`, `handler.Source` response shapes,
   `ListDecompositionProviders`).
7. Tests.
8. Gates: `sqlc generate` + `go build ./...` + `go vet ./...` + e2e suite.
   `just check-frontend` is **not** in scope (frontend untouched).

## Out of scope (explicit follow-ups)

- Frontend rendering of `image_url` in the topic/fact views.
- Hosting/mirroring image bytes locally (today `source_images.bytes` is
  recorded but not served; `facts.image_url` points at the original URL).
- PDF page-image extraction tuning (DPI, page selection) — current PDF parser
  renders every page; `max_images_per_source` caps the count but does not
  pick the most informative pages.
- Prompt iteration once we have real-world image sources to evaluate
  against (the prompt is locked to "facts, not descriptions" per the
  requirement; tuning can happen after first end-to-end run).