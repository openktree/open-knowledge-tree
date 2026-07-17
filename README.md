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

## Releases

Releases are managed by [release-please](https://github.com/googleapis/release-please)
from [Conventional Commits](https://www.conventionalcommits.org/). Each service in the
monorepo has an **independent SemVer** and is released on its own cadence. Merge a
service's release PR (auto-opened by release-please) and the matching release
workflow builds + pushes the Docker image to GHCR.

### Components and tags

| Service | Scope | Tag | Image |
|---------|-------|-----|-------|
| API | `api` | `api-v1.2.0` | `ghcr.io/openktree/api:1.2.0` (+ `:latest`) |
| Registry | `registry` | `registry-v0.4.1` | `ghcr.io/openktree/registry:0.4.1` (+ `:latest`) |
| Frontend | `frontend` | `frontend-v1.0.3` | `ghcr.io/openktree/frontend:1.0.3` (+ `:latest`) |
| Docs | `docs` | `docs-v0.1.0` | `ghcr.io/openktree/docs:0.1.0` (+ `:latest`) |

### Commit scopes

Use these scopes so release-please routes the change to the right service:

```
feat(api): add MCP tool for X
fix(registry): correct context seeding order
feat(frontend): add sources filter
docs(docs): clarify deployment guide
```

Other scopes (`chore`, `ci`, `test`) don't trigger releases.

### Release flow

1. PRs merged to `main` with a conventional scope (`feat`/`fix`/`docs`/…).
2. release-please opens a **per-service release PR** with the version bump and generated
   changelog (`CHANGELOG.md` next to the service).
3. Maintainer merges the release PR → release-please creates the tag `api-v1.2.0` (etc.).
4. Tag push triggers `.github/workflows/release-<service>.yml`:
   - `api`, `registry` → GoReleaser + Docker buildx → GHCR (`linux/amd64`).
   - `frontend`, `docs` → Docker buildx → GHCR (`linux/amd64`).

### Docs hosting

The Docusaurus site is deployed to **Cloudflare Pages** on every push to `main`
that touches `docs/` (see `.github/workflows/docs-cloudflare.yml`). The
`ghcr.io/openktree/docs` image is a self-hostable nginx fallback for offline /
mirrored installs. To enable Cloudflare Pages, set the `CLOUDFLARE_PROJECT_NAME`
repo variable and the `CLOUDFLARE_API_TOKEN` / `CLOUDFLARE_ACCOUNT_ID` secrets.

## License

See the repository for license information.