---
id: getting-started-dev
sidebar_position: 0
title: Getting Started with Development
---

# Getting Started with Development

This page covers the full developer workflow: running from source with hot-reload, the knowledge registry, testing, and the `just` command runner.

## Prerequisites

- [Docker](https://docs.docker.com/get-docker/) with Compose v2
- [`just`](https://github.com/casey/just) — a command runner (install via `cargo install just`, `brew install just`, or `sudo apt install just`)
- [Go](https://go.dev/dl/) 1.25+ (for e2e tests and running scripts)
- [Node](https://nodejs.org/) 20+ (for the frontend build)

## Quick start from source

```bash
git clone https://github.com/openktree/open-knowledge-tree.git
cd open-knowledge-tree
cp .env.example .env   # edit with your API keys
just dev
```

This boots the **dev profile**: API with hot-reload (source-mounted), Vite dev server for the frontend, Postgres, Qdrant, FlareSolverr, and MinIO. The API rebuilds and restarts automatically when Go files change. The frontend hot-reloads in the browser.

## `just` commands

| Command | What it does |
|---------|-------------|
| `just dev` | Boot the dev stack (hot-reload API + frontend) |
| `just up` | Boot the pre-built production stack (from GHCR images) |
| `just down` | Stop the production stack |
| `just down-all` | Stop all stacks (dev + prod + test) |
| `just reset-db` | Wipe dev databases and restart on empty state |
| `just reset-repo <id>` | Wipe all data for a single repository |
| `just test-e2e` | Run e2e tests against an isolated test Postgres |
| `just check-frontend` | Page-size policy + frontend production build |
| `just docs` | Start the Docusaurus dev server (port 3001) |
| `just bootstrap-admin <email>` | Promote a user to system admin |
| `just api-logs` | Tail API container logs |
| `just frontend-logs` | Tail frontend container logs |

Run `just` with no arguments to see all available recipes.

## Hot-reload

### API (Go)

The dev stack bind-mounts `backend/` into the container. The [Air](https://github.com/air-verse/air) file watcher detects Go file changes, rebuilds the binary, and restarts the process. No manual restart needed.

### Frontend (Vite)

The dev stack bind-mounts `frontend/src/` into the Vite dev server. Changes to `.jsx`, `.js`, `.css`, and `.ts` files hot-reload in the browser without a full page refresh.

## Knowledge Registry

The [Knowledge Registry](/docs/reference/registry) is an optional component that caches pre-computed source decompositions. When a source exists in the registry, the ingestion pipeline skips the expensive decomposition step and imports the cached facts, concepts, and embeddings directly.

### Running the registry locally

```bash
just dev-registry
```

This boots the registry service + MinIO (its S3 backend) alongside the dev stack. The registry listens on `http://localhost:8081`. MinIO console is at `http://localhost:9001` (`minioadmin` / `minioadmin`).

To connect the API to your local registry, set `REGISTRY_URL=http://localhost:8081` in your `.env` or in `configs/config.local.yaml`:

```yaml
providers:
  registry:
    url: "http://localhost:8081"
```

### Standalone registry

To run the registry without the full OKT stack:

```bash
just knowledge-registry
```

This boots only the registry + MinIO from `backend/docker-compose.registry.yml`.

### Resetting the registry

To wipe the registry to a clean state (drops all cached sources, facts, concepts, and embeddings):

```bash
just reset-registry
```

This removes the registry's SQLite database and MinIO bucket contents, then restarts the registry so it re-seeds its canonical context vocabulary.

## Testing

### E2E tests

```bash
just test-e2e
```

This boots an isolated Postgres on port 5433 (tmpfs), applies all migrations, and runs the full e2e test suite against it. The test Postgres is destroyed after the run.

:::warning
Never run e2e tests against the dev database (port 5432). The test harness drops all schemas before re-applying migrations, which deletes all application data.
:::

### Frontend checks

```bash
just check-frontend
```

Runs the page-size policy checker (ensures no page exceeds the size budget) and then builds the frontend for production. Use this before pushing frontend changes.

## Configuration

OKT uses a layered YAML configuration. The default config is embedded in the binary and written to disk on first run. See [Configuration Reference](/docs/reference/config) for all valid values and how to override them.

Create `backend/configs/config.local.yaml` for local overrides (it's gitignored):

```yaml
providers:
  registry:
    url: "http://localhost:8081"
```

Environment variables can also override config values. See the [Configuration Reference](/docs/reference/config) for the full list.

## Ports reference

| Port | Service | Notes |
|------|---------|-------|
| 5432 | Postgres (app) | Main database |
| 5434 | Postgres (tasks) | River task queue |
| 5433 | Postgres (test) | Ephemeral, only during `test-e2e` |
| 6333 | Qdrant REST | Vector search HTTP API |
| 6334 | Qdrant gRPC | Vector search gRPC |
| 8080 | API | Go backend |
| 8081 | Registry | Knowledge Registry (when running) |
| 8191–8193 | FlareSolverr | JS-challenge bypass (×3 instances) |
| 9000 | MinIO S3 | Object store for registry |
| 9001 | MinIO Console | Browser UI for MinIO |
| 3000 | Frontend (prod) | Nginx-served SPA |
| 5173 | Frontend (dev) | Vite dev server |
| 3001 | Docs (dev) | Docusaurus dev server |
