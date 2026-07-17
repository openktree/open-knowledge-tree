---
id: services-overview
sidebar_position: 2
title: Services Overview
---

# Services Overview

## Postgres (app) — port 5432

The application database. Holds two schemas:

- `okt_system` — users, sessions, `casbin_rule`, repositories, `oauth_refresh_tokens`, `ai_usage`, `repository_contexts`
- `okt_repository` — per-repo data: `sources`, `facts`, `concepts`, `concept_aliases`, `fact_concepts`, `concept_summaries`, `concept_syntheses`, `concept_relations` (matview), `source_images`

The connection's `search_path` (`okt_system, okt_repository, public`) is set by the dbpool registry's `AfterConnect` hook on every connection. See [Architecture > Multi-database](/docs/architecture/multi-database).

## Postgres (tasks) — port 5434

A dedicated Postgres for the River task queue. Kept on a separate volume so a runaway job or long VACUUM on the task DB can't starve the application DB. Configured via the `TASK_*` env vars.

## Qdrant — ports 6333 (REST), 6334 (gRPC)

The vector store. Two collections:

- `okt_facts` — fact embeddings, payload `{repository_id, status}`
- `okt_concepts` — concept embeddings, payload `{repository_id}`

Qdrant is a dumb vector store — Postgres is the single source of truth for everything except the vectors. The API health-checks Qdrant at boot and fails fast if unreachable. Leave `QDRANT_HOST` empty to boot without the embedding+dedup pipeline (fact endpoints still serve). See [Architecture > Qdrant](/docs/architecture/qdrant).

## FlareSolverr (Byparr) — port 8191

A headless-browser sidecar that solves JavaScript challenges (Cloudflare "Checking your browser", Datadome, PerimeterX) that no amount of TLS fingerprinting can bypass. The fetch strategy's heaviest tier talks to it over the FlareSolverr protocol. The API self-disables this tier when `FLARESOLVERR_URL` is empty. See [Knowledge Flow > Source Extraction](/docs/reference/knowledge-flow/1-source-extraction).

## MinIO — ports 9000 (S3), 9001 (console)

S3-compatible object store backing the dev knowledge registry. Console at `http://localhost:9001` (credentials: `minioadmin` / `minioadmin`).

## Knowledge Registry — port 8081

Optional. The registry stores pre-computed source decompositions so a source that someone already processed doesn't need to be re-decomposed. The `retrieve_source` worker checks the registry before fetching. Requires the `knowledge-registry` repo at `../knowledge-registry/`. See `just dev-registry` in the justfile.

## API — port 8080

The Go backend. In dev (`api-dev`), the source is bind-mounted and the process hot-reloads. In prod (`api`), it's a static image. Handles all HTTP routes + the MCP server. See [REST API](/docs/api/overview).

## Frontend — port 5173 (dev), 3000 (prod)

The SolidJS SPA. In dev, Vite serves with hot-module replacement. In prod, nginx serves the built static files.