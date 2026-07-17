---
id: registry
sidebar_position: 6
title: Knowledge Registry
---

# Knowledge Registry

The **Knowledge Registry** is a standalone service that catalogs OKT sources and their decompositions (facts, concepts, embeddings) so that an OKT instance can **reuse research another instance has already paid for**. It is the layer that turns a one-time research spend into a shared, addressable asset.

It runs as its own Go binary (`registry/cmd/registry`) with its own database and object store, completely independent of the main OKT backend. The OKT backend talks to it as a client.

## What problem it solves

Building a knowledge graph from scratch is expensive (see [Expected Cost](/docs/reference/expected-cost)). Most of that cost is the chat-model calls that decompose source text into facts. But once a source is fetched and decomposed, the result is deterministic-ish: the same URL, fetched and decomposed by the same model, yields substantially the same facts. The registry captures those results so the next OKT instance that encounters the same URL skips the fetch + decompose and just imports.

This is what makes the cost-curve bend: the **first** instance to research a topic pays the full bill; every later instance that pulls from the registry pays roughly zero for the overlapping sources.

## What it stores

The registry stores two kinds of artifact, both keyed by source identity:

| Artifact | Where | Contents |
|---|---|---|
| **Source metadata** | SQLite / Postgres | `id`, `url`, `doi`, `sha256`, `title`, `s3_key` — enough to find a source by URL, DOI, or content hash. |
| **Source content + decompositions** | S3 / MinIO / R2 | The fetched body (markdown, text, images) plus one decomposition package per model. A decomposition carries facts, concepts, summaries, fact embeddings, and concept embeddings — the full output of the Knowledge Flow for that source. |

A source can have **multiple decompositions**, one per model ID (`google/gemma-4-31b-it`, `deepseek/deepseek-v4-flash:turbo`, etc.). The `decompositions` table records each one's model, fact count, embedding model, embedding dimensions, and the S3 key of the package. Callers pull the decomposition that matches their configured chat model so the imported facts are consistent with what the local instance would have produced.

A `fact_hashes` table maps each fact's content hash to the source + decomposition it came from, enabling cross-source fact linking without re-embedding.

## How an OKT instance uses it

The OKT backend's `retrieve_source` worker is the primary consumer. On every `fetchAndProcessSource`, before it goes to the network, it asks the registry: *"Do you already have this URL / DOI / SHA256?"*

```
OKT backend                         Registry
   |                                   |
   |  search(url or doi or sha256)     |
   |---------------------------------->|
   |                                   |
   |  found? + decompositions list     |
   |<----------------------------------|
   |                                   |
   |  pull decomposition(model)        |
   |---------------------------------->|
   |                                   |
   |  DecompositionPackage (JSON)      |
   |<----------------------------------|
   |                                   |
   |  import facts/concepts/embeddings |
   |  into local repo (no fetch, no    |
   |  LLM call)                        |
   |                                   |
```

If the registry has the source and a decomposition for a model the instance allows (`providers.registry.allowed_models`), the worker imports it directly and skips fetch + decompose entirely. If not, the worker proceeds normally, and a follow-up `contribute_source` job pushes the result back to the registry so the next instance benefits.

### Push (contribute)

After the Knowledge Flow finishes a source — facts are stable and deduplicated — the `contribute_source` worker uploads the decomposition to the registry. The push level is configurable per repository:

- **`facts`** — sources + facts + fact embeddings only.
- **`concepts`** (default) — adds concepts, concept-concept links, and concept embeddings.

The push is asynchronous and best-effort; a registry outage never blocks the local pipeline.

### Pull (import)

On `retrieve_source`, the worker calls `SearchSource(url, doi)`. If found, it pulls the decomposition for an allowed model, deserializes the `DecompositionPackage`, and inserts the facts, concepts, summaries, and embeddings into the local repository — reusing the remote fact UUIDs so `fact_hashes` links them across sources.

### Bulk pull

`pull_all_from_registry.go` is a maintenance worker that backfills an entire repository from the registry in one pass — useful when seeding a fresh OKT instance from a well-populated registry.

## HTTP API

The registry exposes a small REST API under `/api/v1`. Source endpoints use optional auth (`auth_mode: open | read-open | closed`), so a public registry can serve anonymous reads while requiring a key for writes.

| Method | Path | Purpose |
|---|---|---|
| `POST` | `/api/v1/auth/register` | Create a registry user. |
| `POST` | `/api/v1/auth/login` | Login, get a JWT. |
| `GET` | `/api/v1/tokens` | List your API tokens. |
| `POST` | `/api/v1/tokens` | Create an API token (read / write / readwrite). |
| `DELETE` | `/api/v1/tokens/{id}` | Revoke a token. |
| `POST` | `/api/v1/sources` | **Push** a source (metadata + content). |
| `GET` | `/api/v1/sources` | **List** sources (paginated, optional text query). |
| `GET` | `/api/v1/search` | **Search** by `url`, `doi`, `sha256`, or `text`. Returns source + presigned download URLs + decomposition refs. |
| `GET` | `/api/v1/sources/{sid}` | **Pull** a source package (metadata + content + decomposition refs). |
| `GET` | `/api/v1/sources/{sid}/presigned` | Get a presigned download URL for the source body / images. |
| `POST` | `/api/v1/sources/{sid}/presigned` | Get a presigned upload URL (large-body direct upload). |
| `POST` | `/api/v1/sources/{sid}/decompositions/{model}` | **Push** a decomposition for a given model. |
| `GET` | `/api/v1/sources/{sid}/decompositions` | List decompositions for a source. |
| `GET` | `/api/v1/sources/{sid}/decompositions/{model}` | **Pull** a decomposition for a given model. |
| `GET` | `/api/v1/contexts` | List the canonical context vocabulary. |
| `GET` | `/health` | Health check. |

There is also a small server-rendered UI under `/ui` (login, register, dashboard, token management, admin) for operators who want a browser flow instead of curl.

## Auth model

- **Users** — email + password, role `viewer` / `editor` / `admin`.
- **API tokens** — per-user, scoped `read` / `write` / `readwrite`, optional expiry. Sent as `Authorization: Bearer <token>`.
- **Auth mode** (`auth.auth_mode`):
  - `open` — no auth required for any source endpoint (dev / public good registry).
  - `read-open` — reads are public, writes require a token.
  - `closed` — every source endpoint requires a token.

The OKT backend's registry client is configured with `auth_mode: none` (no header) or `bearer` (API key). A separate read-only key (`read_api_key`) lets you hand out public read access while keeping the write key restricted.

## Storage backends

| Layer | Options | Default |
|---|---|---|
| Metadata DB | SQLite, Postgres | SQLite at `registry.db` |
| Object store | S3, MinIO, Cloudflare R2, local filesystem | S3 endpoint `http://localhost:9000`, bucket `okt-registry` |

Large bodies and images are uploaded directly to the object store via presigned URLs, so the registry process never buffers them. Decomposition packages are JSON files stored by the registry process on push and read back on pull.

## Configuration

The registry reads its config from a YAML file and env vars with prefix `REGISTRY_` (e.g. `REGISTRY_S3_BUCKET`, `REGISTRY_AUTH_JWT_SECRET`). Key settings:

| Setting | Default | Purpose |
|---|---|---|
| `port` | `8080` | Listen port. |
| `database.driver` | `sqlite` | `sqlite` or `postgres`. |
| `database.url` | `registry.db` | DSN / file path. |
| `storage.backend` | `s3` | `s3` or `filesystem`. |
| `s3.endpoint` | `http://localhost:9000` | MinIO / S3 / R2 endpoint. |
| `s3.bucket` | `okt-registry` | Object bucket. |
| `s3.presign_base_url` | (empty) | Public-facing URL for presigned links; empty = no presigning (dev). |
| `auth.auth_mode` | `open` | `open` / `read-open` / `closed`. |
| `auth.jwt_secret` | `change-me-in-production` | JWT signing secret. |
| `auth.bootstrap_admins` | (empty) | Emails promoted to admin on first login. |

## Wiring it into an OKT instance

On the OKT backend side, the registry is a provider configured under `providers.registry` (or the multi-registry `providers.registries[]` list):

```yaml
providers:
  registry:
    url: "http://registry:8081"
    auth_mode: "bearer"
    api_key: "${REGISTRY_API_KEY}"        # write key (push)
    read_api_key: "${REGISTRY_READ_KEY}"  # read key (search/pull); falls back to api_key
    allowed_models: ["*"]                 # or ["google/gemma-4-31b-it", ...]
```

When `url` is empty, the integration is disabled and the worker always falls through to fetch + decompose. When set, every `retrieve_source` job checks the registry first.

Each OKT repository can independently enable/disable the registry integration and pick which configured registry to talk to (per-repo `registry_id` + `registry_enabled` columns), so a multi-tenant instance can have some repos sharing from the public registry and others kept fully local.

## Running it

The dev stack boots a registry alongside the API and frontend:

```bash
just knowledge-registry   # registry + MinIO on :8081 / :9000
# or
just dev                  # full stack including registry
```

See [Architecture — Registry](/docs/architecture/registry) for the internal component layout.