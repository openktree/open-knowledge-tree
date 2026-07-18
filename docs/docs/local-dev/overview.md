---
id: overview
sidebar_position: 0
title: Local Dev Overview
---

# Local Dev Overview

> **Just want to run OKT?** You don't need this page. See [Getting Started](/docs/getting-started) — two commands, no git required.

This page is for developers who want to contribute to OKT or run from source with hot-reload, e2e tests, and the full toolchain.

## Prerequisites

- [Docker](https://docs.docker.com/get-docker/) with Compose v2
- [`just`](https://github.com/casey/just) (the command runner)
- (Optional) Go 1.22+ for running e2e tests locally
- (Optional) Node 20+ for the frontend build

## Quick start

```bash
# 1. Create a .env at the repo root with your API keys
cat > .env <<'EOF'
SERPER_API_KEY=your-serper-key
OPENROUTER_API_KEY=your-openrouter-key
OLLAMA_API_KEY=your-ollama-key
EOF

# 2. Boot the dev stack (hot-reload API + frontend)
just dev
```

This starts:
- **Postgres** (port 5432) — the application database
- **Postgres** (port 5434) — the River task queue database
- **Qdrant** (ports 6333/6334) — the vector store
- **FlareSolverr** (port 8191) — headless browser for JS-challenge bypass
- **MinIO** (ports 9000/9001) — S3-compatible object store (dev registry)
- **API** (port 8080) — Go backend with hot-reload
- **Frontend** (port 5173) — Vite dev server

## Default credentials

| Service | User | Password | Port |
|---------|------|----------|------|
| Postgres (app) | `okt` | `okt_dev` | 5432 |
| Postgres (tasks) | `okt` | `okt_dev` | 5434 |
| MinIO | `minioadmin` | `minioadmin` | 9000/9001 |

A default admin user + repository are created on first boot via `EnsureDefaultAdmin` + `EnsureDefaultRepository` (`backend/cmd/app/api.go:203-208`).

## Environment variables

The `.env` file at the repo root is loaded by Docker Compose. Key variables:

| Variable | Required | Description |
|----------|----------|-------------|
| `SERPER_API_KEY` | yes | Google web search API key |
| `OPENROUTER_API_KEY` | no* | OpenRouter API key (LLM calls for fact extraction, concept extraction, synthesis) |
| `OLLAMA_API_KEY` | no* | Ollama Cloud API key (alternative LLM provider) |
| `OPENALEX_EMAIL` | no | Email for OpenAlex API (polite pool) |
| `UNPAYWALL_EMAIL` | no | Email for Unpaywall DOI lookup |
| `OKT_FETCH_IMPERSONATE` | no | TLS impersonation profile (default `chrome_124`) |
| `FLARESOLVERR_URL` | no | FlareSolverr endpoint (defaults to the dev service) |

At least one LLM provider key (`OPENROUTER_API_KEY` or `OLLAMA_API_KEY`) is required for the pipeline to work — fact extraction, concept extraction, and synthesis all call an LLM.

## Common commands

```bash
just dev              # Boot dev stack (hot-reload)
just up               # Boot prod stack
just down             # Stop all services
just reset-db         # Wipe dev DBs and restart
just test-e2e         # Run e2e tests (uses isolated test Postgres on port 5433)
just check-frontend   # Page-size policy + frontend build
just api-logs         # Tail API logs
just frontend-logs    # Tail frontend logs
just bootstrap-admin user@example.com  # Promote a user to sysadmin
```

See the full [justfile](https://github.com/open-knowledge-tree/open-knowledge-tree-go/blob/main/justfile) for all recipes.