---
id: docker-compose
sidebar_position: 1
title: Docker Compose
---

# Docker Compose

The full stack is defined in `backend/docker-compose.yml`. Services are grouped by profiles: `dev`, `prod`, and `test`.

## Service map

| Service | Profiles | Port | Purpose |
|---------|----------|------|---------|
| `postgres` | (always) | 5432 | Application database (`okt`) |
| `postgres-tasks` | (always) | 5434 | River task queue database (`okt_tasks`) |
| `flaresolverr` | dev, prod | 8191 | Headless browser for JS-challenge bypass (Byparr) |
| `qdrant` | dev, prod | 6333, 6334 | Vector store (REST + gRPC) |
| `minio` | dev | 9000, 9001 | S3-compatible object store for dev registry |
| `registry-dev` | dev | 8081 | Local knowledge registry (sqlite + S3) |
| `api-dev` | dev | 8080 | Go API with hot-reload (bind-mounted source) |
| `frontend-dev` | dev | 5173 | Vite dev server (bind-mounted source) |
| `api` | prod | 8080 | Production API container |
| `frontend` | prod | 3000 | Production frontend (nginx) |
| `test-postgres` | test | 5433 | Ephemeral tmpfs Postgres for e2e tests |

## Profiles

### Dev

```bash
just dev
```

Boots everything except the test services. The API and frontend are bind-mounted for hot-reload. FlareSolverr, Qdrant, MinIO, and the registry are included.

### Prod

```bash
just up
```

Boots the production images (no bind mounts, no MinIO/registry). The frontend is served by nginx.

### Test

```bash
just test-e2e
```

Boots only `test-postgres` (tmpfs, port 5433) and runs the e2e suite against it. **Never run e2e tests against the dev database** — the test harness drops schemas.

## Volumes

| Volume | Service | What it holds |
|--------|---------|---------------|
| `pgdata` | postgres | Application data (users, repos, facts, concepts, syntheses) |
| `pgdata_tasks` | postgres-tasks | River job queue data |
| `qdrant_data` | qdrant | Embedding vectors |
| `source_assets` | api/api-dev | Inline images, PDF page renders, full PDF bodies |
| `minio_data` | minio | Registry S3 objects (dev only) |
| `registry_data` | registry-dev | Registry sqlite DB (dev only) |

## Resetting the dev DB

If golang-migrate complains about a dirty `schema_migrations` row, use:

```bash
just reset-db
```

This runs `down -v` through the compose project (removing all named volumes), then boots the dev profile. A bare `docker compose down -v` may leave stale volumes if the project name differs.