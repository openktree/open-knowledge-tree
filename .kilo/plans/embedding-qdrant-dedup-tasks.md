# Embedding + Qdrant + Per-Repo Cross-Source Dedup with Source Linking

## Goal

Add embedding generation (3072-dim via OpenRouter by default), Qdrant as a dumb vector index (no fact information in payloads), and a per-repository **cross-source** deduplication pipeline. Dedup merges link sources onto the surviving fact, producing a `source_count` signal ("confirmed by N sources") surfaced in the API and UI so users can validate a fact against every source that supports it.

Chained per source via `river.ClientFromContext[pgx.Tx](ctx)` (idiomatic River worker→worker chaining; avoids passing the Manager into workers).

## Locked decisions

| # | Decision | Value |
|---|----------|-------|
| D1 | Fact-source relationship | **Junction only.** Drop `facts.source_id` + `facts.chunk_index`; all source links live in `fact_sources`. All sources equal, no "origin" concept. Rewrite migration 0013 directly (clean DB start authorized). |
| D2 | Embedding dimension | **3072.** Default provider `openrouter` with `google/gemini-embedding-2` (or `text-embedding-3-large`). Ollama `qwen3-embedding` (1024) kept as a local-dev alternative. |
| D3 | Source count | **Computed** via aggregate JOIN in list queries. No denormalization yet. Detail endpoint returns full source rows (`id, url, parsed_title, chunk_index, first_seen_at`) for user validation. |
| D4 | Qdrant role | **Dumb vector index.** Payload = `{repository_id, status}` only. No `source_id`, no text. Postgres is the single source of truth for everything except the vector. |
| D5 | Qdrant collection layout | **Single collection** (`okt_facts`), payload-filtered by `repository_id`. No per-repo collections. Tier isolation lives in Postgres; Qdrant is shared infrastructure that respects repo boundaries via payload filters + a payload index on `repository_id`. |
| D6 | Dedup scope | **Per-repository, cross-source.** Working set = all `new` + `stable` facts in the repo, across all sources. |
| D7 | Dedup concurrency | **`pg_advisory_xact_lock(hashtext(repository_id))`** at the top of `deduplicate_facts.Work`. One dedup pass per repo at a time. Simpler than a River unique-args constraint, survives River restarts, no import cycles. |

## Dedup rules (per repo, cross-source, sequential)

Process `new` facts in UUID-ascending order. For each `new` fact `nf`, search Qdrant for the nearest neighbor **within the same repository** (`repository_id == repoID`), excluding self, score ≥ `dedup.threshold`, `limit=1`:

| Hit `m` status | Action |
|----------------|--------|
| `stable` | Mark `nf` → `to_delete`. Link `nf`'s sources to `m` via `AddFactSource(m.id, nf_source_id, nf_chunk_index)` for each source of `nf`. |
| `new` | Drop the lexicographically-larger UUID. The survivor inherits the loser's sources via `AddFactSource`. Mark loser → `to_delete`. |
| `to_delete` | Skip (already marked, not a valid keeper). |

After all `new` facts processed: promote survivors `new → stable` (Postgres + Qdrant payload). Update Qdrant payload `status` for all changed facts. Enqueue `cleanup_facts` for the repo.

## Pipeline overview (3 tasks + 1 cron)

1. **`source_decomposition`** (existing, modified) — chunks parsed text, extracts facts, inserts them into `okt_repository.facts` as `status='new'`, links each fact to its extracting source via `fact_sources`. On success, enqueues an `embed_facts` job.
2. **`embed_facts`** (new) — retrieves `status='new'` facts for the repo with `embedded_at IS NULL` (cross-source). Bulk-embeds via the configured embedding provider. Upserts points into Qdrant (point id = fact UUID, payload `{repository_id, status}`, **no text**). Marks `embedded_at` + `embedded_model`. Enqueues `deduplicate_facts` for the repo.
3. **`deduplicate_facts`** (new) — acquires `pg_advisory_xact_lock(hashtext(repository_id))`; loads `new` + `stable` facts for the repo with their `fact_sources` sets; deduplicates **sequentially** against Qdrant nearest-neighbors within the repository (score ≥ threshold). Marks losers `to_delete` in Postgres and links their sources onto the survivor via `AddFactSource`. Promotes remaining `new` facts to `stable` (Postgres + Qdrant payload). Enqueues `cleanup_facts` for the repo.
4. **`cleanup_facts`** (new) — deletes `to_delete` facts for the repo from Postgres **and** Qdrant (idempotent).
5. **`fact_catchup`** (new, River periodic job, daily) — removes (Postgres + Qdrant) facts with `status IN ('to_delete','new')` older than `catchup_max_age` (default 1 week), in bounded batches per repo database. Stuck/orphaned rows.

## Config changes

**`backend/internal/config/config.go`** — add to `ProvidersConfig`:
```go
Embedding EmbeddingConfig `mapstructure:"embedding"`
Qdrant    QdrantConfig    `mapstructure:"qdrant"`
Dedup     DedupConfig     `mapstructure:"dedup"`
```
```go
type EmbeddingConfig struct {
    Provider   string `mapstructure:"provider"`   // "openrouter" (default) | "ollama"
    Model      string `mapstructure:"model"`      // "google/gemini-embedding-2" | "qwen3-embedding"
    Dimensions int    `mapstructure:"dimensions"` // 3072 (gemini/text-embedding-3-large) | 1024 (qwen3)
}
type QdrantConfig struct {
    Host          string `mapstructure:"host"`
    Port          int    `mapstructure:"port"`           // 6334 gRPC
    APIKey        string `mapstructure:"api_key"`
    Collection    string `mapstructure:"collection"`     // "okt_facts"
    AllowRecreate bool   `mapstructure:"allow_recreate"` // false by default; dev-only dimension-mismatch drop+recreate
}
type DedupConfig struct {
    Threshold     float64 `mapstructure:"threshold"`       // 0.95
    CatchupMaxAge string  `mapstructure:"catchup_max_age"` // "168h"
}
```

**`backend/configs/config.default.yaml`** — under `providers:`:
```yaml
  embedding:
    provider: "openrouter"
    model: "google/gemini-embedding-2"
    dimensions: 3072
    # Local-dev / cost-free alternative (different dimension — requires
    # recreating the Qdrant collection and re-embedding all facts):
    # provider: "ollama"
    # model: "qwen3-embedding"
    # dimensions: 1024
  qdrant:
    host: "localhost"
    port: 6334
    api_key: ""
    collection: "okt_facts"
    allow_recreate: false
  dedup:
    threshold: 0.95
    catchup_max_age: 168h
```
`task.queues` add: `embed_facts: 5`, `deduplicate_facts: 5`, `cleanup_facts: 5`.

## AI provider: embeddings support

**`backend/internal/providers/ai/ai.go`** — add a new interface (separate from `AIProvider` so chat-only providers are unaffected):
```go
type EmbeddingProvider interface {
    Embed(ctx context.Context, db store.DBTX, req EmbeddingRequest) (EmbeddingResponse, error)
    Describe() ProviderDescription
}
type EmbeddingRequest struct {
    Model  string   `json:"model"`
    Inputs []string `json:"inputs"`
}
type EmbeddingResponse struct {
    Model      string         `json:"model"`
    Embeddings [][]float32    `json:"embeddings"`
    Usage      EmbeddingUsage `json:"usage"`
}
```

Implementations (bulk):
- `ollama.go` — add `Embed` calling Ollama `POST /api/embed` (model + `input[]`). Map response `embeddings` (float32). Implement `EmbeddingProvider` on `*OllamaProvider`.
- `openrouter.go` — add `Embed` calling `POST https://openrouter.ai/api/v1/embeddings` (OpenAI-compatible shape: `model` + `input` as array, response `data[].embedding`). Implement on `*OpenRouterProvider`.

**Early verification step (before implementation)**: confirm OpenRouter exposes `/api/v1/embeddings` with `google/gemini-embedding-2` and/or `text-embedding-3-large`. If not, add a direct `openai_embedding.go` or `google_embedding.go` implementation behind the same `EmbeddingProvider` interface. The interface makes this a contained swap.

`cmd/app/api.go` — build an `EmbeddingProvider` from `cfg.Providers.Embedding.Provider` (look up the existing chat provider instance by name and assert it implements `EmbeddingProvider`), plus build a Qdrant client (`qdrantstore.NewClient`) from `cfg.Providers.Qdrant`. Pass both into `taskmanager.New`.

## Qdrant integration

New package `backend/internal/qdrantstore/` (transport-agnostic, no HTTP). gRPC client (`github.com/qdrant/go-client/qdrant`, port 6334):
- `client.go` — `NewClient(cfg config.QdrantConfig) (*Store, error)`.
- `ensure.go` — `EnsureCollection(ctx, collection, dimensions, distance="Cosine")` creates the collection if absent. On dimension mismatch: if `cfg.AllowRecreate`, drop+recreate (dev affordance); else return an error (fail loud in prod).
- `points.go`:
  - `UpsertFactVectors(ctx, collection, []FactPoint)` — point id = fact UUID, vector, payload `{repository_id, status}`. **No source_id, no text in payload.**
  - `SearchSimilar(ctx, collection, vec, filterRepositoryID, excludeFactID, minScore, limit) ([]Hit, error)` — filtered by `repository_id`, excludes self, score ≥ threshold.
  - `DeleteFactVectors(ctx, collection, ids []uuid.UUID)`.
  - `UpdateFactStatusPayload(ctx, collection, factID, status)` — keep payload status in sync for future search filters.
  - `FactPoint{ ID uuid.UUID; Vector []float32; RepositoryID uuid.UUID; Status string }`.

Dependency: `go get github.com/qdrant/go-client` (package `github.com/qdrant/go-client/qdrant`, gRPC port 6334). Adds gRPC/protobuf transitive deps.

## DB changes — rewrite migration 0013 (clean DB start authorized)

**`backend/db/migrations/0013_facts.up.sql`** (rewritten — complete facts schema in one migration):
```sql
-- Extend sources status CHECK to include 'processed'.
ALTER TABLE okt_repository.sources
    DROP CONSTRAINT IF EXISTS sources_status_check;
ALTER TABLE okt_repository.sources
    ADD CONSTRAINT sources_status_check
        CHECK (status IN ('pending', 'fetching', 'fetched', 'failed', 'processed'));
ALTER TABLE okt_repository.sources
    ADD COLUMN IF NOT EXISTS processed_at TIMESTAMPTZ;

-- Facts: one row per unique atomic claim. No source_id — all source
-- links live in fact_sources (N:M). A fact shared by 10 sources has
-- 10 rows in fact_sources and 1 row here. source_count is computed
-- at read time (aggregate JOIN), not denormalized.
CREATE TABLE IF NOT EXISTS okt_repository.facts (
    id             UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    text           TEXT NOT NULL,
    status         TEXT NOT NULL DEFAULT 'new'
        CHECK (status IN ('new', 'stable', 'to_delete')),
    embedded_at    TIMESTAMPTZ,
    embedded_model TEXT,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_facts_status     ON okt_repository.facts(status);
CREATE INDEX IF NOT EXISTS idx_facts_created_at ON okt_repository.facts(created_at);

-- Junction: links facts to the sources that extracted or confirmed them.
-- chunk_index is per-extraction (the same fact from source A's chunk 3
-- and source B's chunk 7 records both). first_seen_at tracks when each
-- source link was established (dedup merges add rows here).
CREATE TABLE IF NOT EXISTS okt_repository.fact_sources (
    fact_id       UUID NOT NULL REFERENCES okt_repository.facts(id) ON DELETE CASCADE,
    source_id     UUID NOT NULL REFERENCES okt_repository.sources(id) ON DELETE CASCADE,
    chunk_index   INT NOT NULL DEFAULT 0,
    first_seen_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (fact_id, source_id)
);
CREATE INDEX IF NOT EXISTS idx_fact_sources_source ON okt_repository.fact_sources(source_id);
CREATE INDEX IF NOT EXISTS idx_fact_sources_fact   ON okt_repository.fact_sources(fact_id);
```

**`backend/db/migrations/0013_facts.down.sql`** (rewritten to match):
```sql
DROP TABLE IF EXISTS okt_repository.fact_sources;
DROP TABLE IF EXISTS okt_repository.facts;
ALTER TABLE okt_repository.sources DROP COLUMN IF EXISTS processed_at;
ALTER TABLE okt_repository.sources DROP CONSTRAINT IF EXISTS sources_status_check;
ALTER TABLE okt_repository.sources
    ADD CONSTRAINT sources_status_check
        CHECK (status IN ('pending', 'fetching', 'fetched', 'failed'));
```

**Delete `backend/db/migrations/0014_fact_status.up.sql` + `.down.sql`** — `status` column + check are now in the rewritten 0013. No `0015` migration needed.

## sqlc queries (`backend/db/queries/facts.sql`)

Rewrite `CreateFact` (no more `source_id`/`chunk_index` on facts):
```sql
-- name: CreateFact :one
INSERT INTO okt_repository.facts (id, text)
VALUES ($1, $2)
RETURNING *;

-- name: AddFactSource :one
-- Idempotent: a fact linked to the same source twice (e.g. re-processing)
-- doesn't double-count. ON CONFLICT preserves the original first_seen_at.
INSERT INTO okt_repository.fact_sources (fact_id, source_id, chunk_index)
VALUES ($1, $2, $3)
ON CONFLICT (fact_id, source_id) DO NOTHING
RETURNING *;
```

New / changed queries:
- `ListNewFactsForEmbedding(repo_id)` — `status='new' AND embedded_at IS NULL`, JOIN `fact_sources` + `sources` for repo filter. Returns facts + their embedding text.
- `ListFactsForDedup(repo_id)` — all `new` + `stable` facts in the repo, with their `fact_sources` rows (worker needs the current source set per fact for merge linking).
- `ListFactsToDelete(repo_id)` — `status='to_delete'`, returns IDs (+ `repository_id` via JOIN for Qdrant cleanup).
- `MarkFactStatus(id, status)`, `BulkMarkFactStatus(ids []uuid, status)`.
- `MarkFactsStableByRepo(repo_id)` — promote surviving `new` → `stable`.
- `MarkFactEmbedded(id, model)`.
- `DeleteFactByID(id)` — junction cascades.
- `DeleteStaleFactsInDB(status_set, cutoff, limit)` — **bounded** (`LIMIT $3`), returns deleted IDs for Qdrant cleanup. Used by catchup in a loop until 0 deleted.
- `ListFactsByRepoWithSourceCount(repo_id, status_filter)` — `SELECT f.*, count(fs.source_id) AS source_count FROM facts f LEFT JOIN fact_sources fs ON fs.fact_id = f.id JOIN sources s ON fs.source_id = s.id WHERE s.repository_id = $1 AND ($2::text = '' OR f.status = $2) GROUP BY f.id ORDER BY f.created_at DESC`.
- `ListFactSources(fact_id)` — `SELECT fs.*, s.url, s.parsed_title FROM fact_sources fs JOIN sources s ON fs.source_id = s.id WHERE fs.fact_id = $1 ORDER BY fs.first_seen_at`.
- `ListFactsBySource(source_id)` — **rewritten**: `SELECT f.* FROM facts f JOIN fact_sources fs ON fs.fact_id = f.id WHERE fs.source_id = $1 AND ($2::text = '' OR f.status = $2) ORDER BY fs.chunk_index, fs.first_seen_at`.

Then `cd backend && sqlc generate`.

## Code changes forced by the schema change

**`backend/internal/taskmanager/tasks/source_decomposition.go`** — the `CreateFact` call site changes:
```go
// Before:
queries.CreateFact(ctx, store.CreateFactParams{ID: factID, SourceID: sourceID, Text: factText, ChunkIndex: int32(chunk.Index)})
// After:
created, _ := queries.CreateFact(ctx, store.CreateFactParams{ID: factID, Text: factText})
queries.AddFactSource(ctx, store.AddFactSourceParams{FactID: created.ID, SourceID: sourceID, ChunkIndex: int32(chunk.Index)})
```

**`backend/internal/store/facts.sql.go`** — regenerated by sqlc; `OktRepositoryFact` struct loses `SourceID` + `ChunkIndex`, gains `EmbeddedAt` + `EmbeddedModel`. Audit shows no handler reads `fact.SourceID` directly (handlers read `source.RepositoryID` from the source row, not from the fact), so no handler breakage beyond the generated code.

## New task files (`backend/internal/taskmanager/tasks/`)

Each: `QueueXxx` const, `XxxArgs{...}` with `Kind()`, a `XxxWorker`, and a `NewXxxWorker(...)` constructor. Chained via `river.ClientFromContext[pgx.Tx](ctx)` (no Manager import, no constructor churn for `source_decomposition`).

- `embed_facts.go` — `EmbedFactsArgs{SourceID, RepositoryID}`. Deps: `EmbeddingProvider`, `*qdrantstore.Store`, `embeddingCfg`, `registry`, `systemQueries`. Work: resolve per-repo pool; fetch `new` facts for the repo with `embedded_at IS NULL` (cross-source); if none, return early (no-op, don't enqueue dedup); bulk embed; upsert to Qdrant (payload `{repository_id, status}`); mark `embedded_at` + `embedded_model`; enqueue `deduplicate_facts` for the repo via `river.ClientFromContext`.
- `deduplicate_facts.go` — `DeduplicateFactsArgs{RepositoryID}`. Deps: `*qdrantstore.Store`, `dedupCfg`, `registry`, `systemQueries`. Work: `SELECT pg_advisory_xact_lock(hashtext($RepositoryID))` (serialize per-repo dedup); load `new` + `stable` facts for the repo with their `fact_sources` sets; for each `new` (UUID asc) search Qdrant within the repository (exclude self, score ≥ threshold, `limit=1`); apply dedup rules; for each merge `AddFactSource(survivor_id, loser_source_id, loser_chunk_index)` for every source of the loser; promote survivors to `stable` (Postgres + Qdrant payload); enqueue `cleanup_facts` for the repo.
- `cleanup_facts.go` — `CleanupFactsArgs{RepositoryID}`. Deps: `*qdrantstore.Store`, `registry`, `systemQueries`. Work: resolve per-repo pool; fetch `to_delete` fact IDs; `DeleteFactVectors`; `DeleteFactByID` per fact (junction cascades). Idempotent.
- `fact_catchup.go` — `FactCatchupArgs{}` periodic job. Deps: `*qdrantstore.Store`, `registry`, `systemQueries`, `dedupCfg`. Work: iterate `registry.Names()`; for each database pool run `DeleteStaleFactsInDB(status IN ('to_delete','new'), created_at < now() - catchup_max_age, LIMIT 10000)` in a loop until 0 deleted; collect deleted IDs per batch; `DeleteFactVectors(ids)` per batch. Bounded deletes avoid multi-minute WAL spikes at millions of facts. `RunOnStart: false`.

## Modify `source_decomposition.go`

After `MarkSourceProcessed`, enqueue `embed_facts` via `river.ClientFromContext[pgx.Tx](ctx).Insert(ctx, tasks.EmbedFactsArgs{SourceID, RepositoryID}, &river.InsertOpts{Queue: tasks.QueueEmbedFacts})`. Guard so a missing client (tests) is logged, not fatal. Update the `CreateFact` + `AddFactSource` call site as described above.

## Wiring (`taskmanager.New` + `cmd/app/api.go`)

`taskmanager.New` signature gains: `embeddingProvider ai.EmbeddingProvider`, `embeddingCfg config.EmbeddingConfig`, `qdrantStore *qdrantstore.Store`, `dedupCfg config.DedupConfig`. Register the 3 new workers. Register the catchup periodic job via `river.NewPeriodicJob(&river.PeriodicInterval{Interval: 24*time.Hour, Offset: ...}, func() (river.JobArgs, *river.InsertOpts) { return tasks.FactCatchupArgs{}, &river.InsertOpts{Queue: ...} }, &river.PeriodicJobOpts{RunOnStart: false})`. No `enqueuerAdapter` changes needed (chaining via `river.ClientFromContext`, not the HTTP enqueuer).

`cmd/app/api.go`: build embedding provider + qdrant store, call `qdrantstore.EnsureCollection` at boot (after registry, before `taskmanager.New`), pass new deps into `taskmanager.New`.

## Docker Compose (`backend/docker-compose.yml`)

Add a `qdrant` service: `image: qdrant/qdrant:v1.13`, expose `6333` (dashboard/REST) + `6334` (gRPC), a named volume `qdrant_data`, healthcheck `curl -f http://localhost:6333/health`, add to `dev` and `prod` profiles, and make `api`/`api-dev` `depends_on` it (with `service_healthy` condition). Add env `QDRANT_HOST: qdrant`, `QDRANT_PORT: 6334` to `api` and `api-dev`. Test profile does **not** get Qdrant (e2e tests that need it skip when unreachable, mirroring the serper/openalex env-gated pattern). Add `qdrant_data` to `volumes:`.

## HTTP API changes

**`GET /{slug}/facts`** — response shape changes:
```json
{
  "facts": [
    {"id": "...", "text": "...", "status": "stable", "created_at": "...", "embedded_at": "...", "source_count": 10}
  ]
}
```
Implement via `ListFactsByRepoWithSourceCount`. `status` query param stays (stable/new/all). Add optional `sort` query param: `created_at` (default — "newest" preferred) | `source_count` ("most confirmed"). Default sort remains newest.

**New `GET /{slug}/facts/{factID}`** — fact detail with full source list for validation:
```json
{
  "fact": {"id": "...", "text": "...", "status": "stable", "created_at": "...", "source_count": 10},
  "sources": [
    {"id": "...", "url": "https://...", "title": "NF-kB in Cancer", "chunk_index": 3, "first_seen_at": "..."}
  ]
}
```
Wire in `wiring.go` under the `/{repoID}` group: `r.Get("/facts/{factID}", h.authed(h.source.GetFact))`. Add `GetFact` to `handler/source.go` — resolve per-repo pool, fetch fact, fetch `ListFactSources`, return both. Enforce repo ownership (fact's sources must belong to the repo).

**`GET /{slug}/sources/{sourceID}/facts`** — rewrite to use the junction-based `ListFactsBySource`. Response gains `source_count` per fact.

## Frontend changes

**`frontend/src/pages/Facts/FactRow.jsx`** — expand from 7 lines. Show:
- `fact.text`
- Status badge (color-coded: new=amber, stable=green, to_delete=red strikethrough).
- **Source count badge**: "Confirmed by N sources" — clickable, navigates to fact detail page.
- This likely splits into `FactRow.jsx` + `FactBadges.jsx` + `FactSourceList.jsx` inside the existing `Facts/` folder to stay within the page-size policy.

**`frontend/src/pages/Facts/FactsContent.jsx`** — add sort control: "Newest" (default, current behavior) vs "Most confirmed" (`source_count` desc). The "most confirmed" sort surfaces the dedup weight signal as an explicit user toggle.

**`frontend/src/services/api.js`** — add:
```js
getFact(slug, factID) {
  return request(`/repositories/${slug}/facts/${factID}`);
}
```
`listRepoFacts` gains optional `sort` param: `?status=...&sort=source_count`.

**New `frontend/src/pages/FactDetail/`** (or fold into `Facts/` folder) — the fact detail view: full text, status, source count, and the source list with clickable URLs (each source links to the `SourceDetail` page + external URL) so the user can validate the fact against each source. This is the human-in-the-loop validation surface.

**`frontend/src/pages/SourceDetail/SourceFacts.jsx`** — update to show `source_count` per fact (a fact extracted from this source may be confirmed by 9 others; the user should see that cross-confirmation here).

## Tests (`backend/e2e/`)

Per the Testing Policy in AGENTS.md, add e2e coverage:
- **`embed_dedup_test.go`** (`//go:build e2e`) — skip when Qdrant unreachable (mirrors the serper/openalex env-gated pattern). Verify:
  - Embed produces Qdrant points with payload `{repository_id, status}`; facts get `embedded_at` set.
  - **Cross-source dedup**: insert near-duplicate facts from two different sources in the same repo; run `deduplicate_facts`; assert one is `stable`, the other `to_delete`, and the survivor's `fact_sources` contains **both** sources.
  - `stable` vs `new`: stable wins, new marked `to_delete`, new's source linked to stable.
  - `new` + `new`: deterministic UUID tie-break, loser's sources linked to winner.
  - **Concurrency**: two `deduplicate_facts` for the same repo — advisory lock serializes them, no double-mark.
  - `cleanup_facts` removes `to_delete` from Postgres + Qdrant.
- **`fact_sources_test.go`** — `AddFactSource` idempotency (same fact+source twice → one row). `ListFactsByRepoWithSourceCount` returns correct counts. `GET /facts/{factID}` returns full source list with URLs.
- Extend `taskmanager_test.go` patterns for `embed_facts`/`deduplicate_facts`/`cleanup_facts` chaining at the River job level (where Qdrant isn't required — assert the chain enqueues, not the dedup logic).
- `RecordingTaskEnqueuer` in `testutil/setup.go` does **not** need changes (workers chain via `river.ClientFromContext`, not the HTTP enqueuer).

## Execution / verification

```bash
# 1. Early verification: confirm OpenRouter embeddings endpoint
#    (curl https://openrouter.ai/api/v1/embeddings with a test payload)

# 2. Config + migration + sqlc
cd backend && sqlc generate
cd backend && go build ./...

# 3. Bring up the stack
just dev   # brings up qdrant + api; EnsureCollection runs at boot

# 4. e2e
just test-e2e

# 5. Frontend gate
just check-frontend
```

Dimension switch test (dev only): set `qdrant.allow_recreate: true`, swap the embedding block to ollama/qwen3/1024, `just dev` → `EnsureCollection` drops+recreates the collection at the new dimension. **Never enable `allow_recreate` in production** — at millions of facts a drop+recreate means re-embedding everything.

## Notes / trade-offs

- `river.ClientFromContext` for chaining keeps workers decoupled from the Manager (no import cycle, no constructor churn for the existing decomposition worker beyond the enqueue call).
- **Dedup is per-repository, cross-source.** Qdrant searches filter by `repository_id`, not `source_id`. The "per-source … out of scope" line from earlier drafts is deleted.
- Qdrant payload carries `{repository_id, status}` only — **no `source_id`, no fact text.** Postgres is the single source of truth for everything except the vector. This keeps Qdrant replaceable and matches the "Qdrant as a search utility" framing.
- **Source linking on merge** turns dedup from a cleanup step into a signal generator: a fact confirmed by 10 sources is stronger than one confirmed by 1, and the UI surfaces this for human validation.
- Qdrant point id = fact UUID (string/UUID point id supported by the client).
- `allow_recreate` is a dev-phase affordance for dimension switches; it is `false` by default and must never be enabled in production.
- Bounded batch deletes in `fact_catchup` avoid multi-minute WAL spikes at millions of facts.
- The prompt-builder port (typed facts + worked examples from `example/`) is deliberately deferred to the next chunk — this plan covers storage + dedup + source linking + API + UI only.