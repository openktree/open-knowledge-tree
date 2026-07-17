# Open Knowledge Tree (OKT)

Open Knowledge Tree is a monorepo for a multi-tenant research platform that
crawls, fetches, parses, and classifies external resources (web pages,
academic works, PDFs) and grows a navigable graph of facts, concepts,
sources, investigations, and synthesized reports from them.

See [`AGENTS.md`](./AGENTS.md) for the full contributor guide, conventions,
folder structure, testing policy, and artifact placement rules.

## Core services

| Service | Path | Description |
|---------|------|-------------|
| **API** | `backend/` | Go 1.22+ backend. Chi router, pgx/v5, sqlc-generated store, Casbin RBAC, OAuth 2.1 authorization server, MCP server, River background jobs, layered Viper config. Multi-database Postgres layout (system + per-repository schemas) with tiered isolation. |
| **Frontend** | `frontend/` | SolidJS + Vite + Tailwind CSS single-page app. `@solidjs/router`, signal-based stores, thin fetch API layer. |
| **Registry** | `registry/` | Standalone Go service that catalogs repositories and routes push/pull/search across OKT instances. Backed by SQLite + MinIO/S3. |
| **Docs** | `docs/` | Docusaurus site for user and reference documentation. |

External integrations (search providers like Serper and OpenAlex, resolution
providers like Unpaywall, FlareSolverr, HTTP fetch) live under
`backend/internal/providers/` and are wired in `backend/cmd/app/api.go`.

## Requirements

- **[just](https://github.com/casey/just)** (command runner) — required, all
  dev workflows are `just` recipes.
- Docker with Compose v2 (the stack runs in containers).
- Go 1.22+ (for running e2e tests and building backend binaries on demand).
- Node 18+ (for the frontend and docs site).

Binaries are never shipped in the repo — build them on demand with the
recipes below.

## Common commands

```bash
# Dev: hot-reload API + frontend via docker compose (profile: dev)
just dev

# Standalone Knowledge Registry + MinIO (registry listens on :8081)
just knowledge-registry

# Production build of the full stack (profile: prod)
just up

# Stop everything (all profiles)
just down

# Wipe dev databases and restart dev stack from a clean state
just reset-db

# Run the Go e2e suite (boots an isolated test Postgres on :5433)
just test-e2e

# Frontend pre-commit gate: page-size policy + production build
just check-frontend

# Docusaurus dev server (localhost:3001)
just docs

# Promote a user to system admin by email
just bootstrap-admin carlos@example.com

# Tail logs
just api-logs         # okt-api-dev
just frontend-logs    # okt-frontend-dev
just registry-logs    # okt-registry-dev
```

See the `justfile` at the repo root for the full list of recipes.

## License

See the repository for license information.