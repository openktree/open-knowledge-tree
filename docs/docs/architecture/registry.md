---
id: registry
sidebar_position: 7
title: Knowledge Registry
---

# Knowledge Registry (Architecture)

The Knowledge Registry is a standalone Go service that lives at `registry/` in the monorepo. It is **not** part of the main OKT backend — it has its own `go.mod`, its own database, its own object store, and its own HTTP server. The OKT backend talks to it as a client over HTTP.

## Component layout

```
registry/
├── cmd/registry/main.go           Entry point — wires deps, starts HTTP server
├── internal/
│   ├── config/config.go           Viper config (YAML + REGISTRY_* env)
│   ├── api/
│   │   ├── router.go              Chi router + middleware
│   │   └── handler/               HTTP handlers (source, auth, token, admin, ui, context, health)
│   ├── auth/                      JWT issue/verify, API-token hashing, middleware
│   ├── service/registry.go        Core push/pull/search/list logic
│   ├── model/types.go             DTOs (SourceData, DecompositionPackage, etc.)
│   ├── store/                     Metadata DB interface + SQLite + Postgres impls
│   ├── storage/                   Object-store interface + S3 + filesystem impls
│   └── migration/                 golang-migrate driver
├── db/migrations/                 Schema (single 0001_init with repositories, sources,
│                                  decompositions, fact_hashes, contexts, users, api_tokens)
└── schema.go                      Re-exported model types for tooling
```

The service is intentionally small: one binary, one schema, one job — catalog sources and serve their decompositions.

## Data flow

```
            OKT backend (client)                      Registry (server)
            -------------------                       -----------------
  retrieve_source  ─── search(url/doi/sha256) ──>   SearchSource
                          |                              |
                          |                   found? ────┐
                          |                              │ no → worker falls through to fetch
                          |                              │ yes
                          |<─── source + decomp refs ────┘
                          |
                          └─── pull decomposition(model) ──>  PullDecomposition
                                                          (reads JSON from S3)
                                 DecompositionPackage  ────────────>
                          (import facts/concepts/embeddings locally;
                           no fetch, no LLM call)

  contribute_source  ─── push source ───────────────>  PushSource
                          (after Knowledge Flow)         (writes content to S3,
                          ─── push decomposition(model)   metadata to DB)
                                                       PushDecomposition
                                                          (writes JSON to S3,
                                                           row to decompositions)
```

## Storage split

| Layer | Engine | Holds | Keyed by |
|---|---|---|---|
| Metadata DB | SQLite (default) or Postgres | `repositories`, `sources`, `decompositions`, `fact_hashes`, `contexts`, `users`, `api_tokens` | source `id`, `url`, `doi`, `sha256`; decomposition `(source_id, model_id)` |
| Object store | S3 / MinIO / R2 / filesystem | Source bodies (markdown, text, images), decomposition packages (JSON) | `sources/<id>.json`, `sources/<id>/body`, `sources/<id>/images/<img>`, `sources/<id>/decompositions/<model>.json` |

Large bodies and images bypass the registry process entirely: the registry hands the client a presigned upload or download URL and the client writes/reads the object store directly. Decomposition packages are small enough to flow through the registry process.

## Schema

`registry/db/migrations/0001_init.up.sql` defines seven tables:

- **`repositories`** — the registry currently serves a single logical namespace (`default`). Spin up another registry instance for isolation. Columns: `id`, `name`, `description`, `owner`, timestamps.
- **`sources`** — one row per fetched source. `UNIQUE(url)`, `UNIQUE(doi)`; indexed on `repo_id` and `sha256`. The `s3_key` points at the source package in the object store.
- **`decompositions`** — one row per (source, model) pair. `UNIQUE(source_id, model_id)`; indexed on both. Carries `fact_count`, `summary_count`, `has_embeddings`, `embedding_model`, `embedding_dims`, and the `s3_key` of the decomposition JSON.
- **`fact_hashes`** — maps each fact's content hash to its (source, decomposition, fact_id) so callers can detect cross-source fact overlap without re-embedding. PK is `(content_hash, source_id, decomposition_id)`.
- **`contexts`** — the canonical context vocabulary (`label` PK).
- **`users`** / **`api_tokens`** — registry auth; tokens are scoped `read` / `write` / `readwrite`.

## Object-store keys

The service computes keys in `registry/internal/service/registry.go`:

| Asset | Key |
|---|---|
| Source package | `sources/<sourceID>.json` |
| Source body | `sources/<sourceID>/body` |
| Image | `sources/<sourceID>/images/<imageID>` |
| Decomposition | `sources/<sourceID>/decompositions/<sanitizedModelID>.json` |

`sanitizeModelID` replaces path-unsafe characters so model IDs like `deepseek/deepseek-v4-flash:turbo` become safe S3 keys.

## Backend-side integration

The OKT backend's registry client lives at `backend/internal/providers/registry/client.go`. It is constructed from `config.RegistryConfig` and registered in a `registry.ClientMap` keyed by registry id. Two workers consume it:

- **`retrieve_source`** (`tasks/retrieve_source.go`) — calls `SearchSource` before fetching; on a hit, pulls + imports the decomposition and skips fetch.
- **`contribute_source`** (`tasks/contribute_source.go`) — chained after `cleanup_facts`, pushes the finished decomposition back. Push level (`facts` vs `concepts`) is per-repo.
- **`pull_all_from_registry`** (`tasks/pull_all_from_registry.go`) — maintenance bulk-pull to seed a fresh repo.

Per-repo gating (`registry_id` + `registry_enabled` columns on the repo row) lets a multi-tenant instance opt some repos in and others out.

## Deployment

The registry ships as its own Docker image (`Dockerfile.registry`) and is a profile in the root `docker-compose.yml`. Dev runs it on `:8081` with a co-located MinIO on `:9000`:

```bash
just knowledge-registry   # registry + MinIO only
just dev                  # full stack including registry
```

Prod runs on Fly.io as `okt-registry-dev` / `okt-registry-prod`, backed by a managed Postgres + R2 for object storage. See the README [Releases](https://github.com/openktree/open-knowledge-tree#releases) section for the per-service deploy flow.

See [Reference — Knowledge Registry](/docs/reference/registry) for the HTTP API, configuration reference, and usage flows.