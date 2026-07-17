---
id: overview
sidebar_position: 0
title: Architecture Overview
---

# Architecture Overview

OKT is a layered Go application with a strict separation between transport (HTTP) and domain logic.

## Layer diagram

```
cmd/app/                      Entry point — wires deps, starts server
internal/
  api/                        HTTP layer (single namespace)
    wiring.go                 Composition root: Handler, Router, route groups
    handler/                  HTTP handlers grouped by domain
    middleware/               AuthRequired, RequirePermission, OAuthBearer
    httputil/                 Response helpers + context keys
  auth/                       JWT + crypto helpers (transport-agnostic)
  oauth/                      OAuth 2.1 authorization server (transport-agnostic)
  config/                     Config struct + Load()
  rbac/                       Casbin service, adapter, seed
  store/                      sqlc-generated code (DO NOT EDIT)
  dbpool/                     Multi-database pool registry
  taskmanager/                River task manager + worker registration
  providers/                  External integrations
    search/                   Search providers (serper, openalex)
    fetch/                    Resolution providers + strategy
    decomposition/            Chunking + LLM fact/concept/alias extraction
    summarization/            LLM summary slices
    synthesis/                LLM synthesis
    content_parsing/          Trafilatura + PDF parsing
    ai/                       LLM providers (ollama, openrouter)
    ontology/                 DBpedia L3 ontology source
    storage/                  Filesystem / S3 storage
  qdrantstore/                Qdrant vector store client
db/
  migrations/                 golang-migrate files (embedded)
  queries/                   sqlc query files
```

## The transport-agnostic rule

Every file under `internal/api/` is HTTP-specific. Domain packages (`auth`, `rbac`, `store`, `providers`, `config`) know nothing about HTTP and stay reusable for any future transport (CLI, worker, gRPC).

## Key patterns

- **sqlc**: Write SQL in `db/queries/*.sql` -> generated typesafe Go in `internal/store`. Never hand-write store boilerplate.
- **Provider Strategy**: Search and resolution use strategy/adapter patterns (`internal/providers/`). Register new providers in `cmd/app/api.go`.
- **RBAC**: Casbin with a custom `pgx` adapter. Policies are rows in PostgreSQL.
- **Config**: Layered Viper loading: `configs/config.default.yaml` -> `configs/config.local.yaml` -> `.env` overrides.
- **Schema**: migrations live in `db/migrations/NNNN_*.up.sql` / `.down.sql`, embedded as `backend.MigrationsFS` and applied by golang-migrate at boot.
- **Task queue**: River (Postgres-backed). Workers registered in `taskmanager.New`.

## The wiring layer

`backend/cmd/app/api.go` is the composition root. It wires:
1. Search providers (serper, openalex)
2. Content parsers (Trafilatura, PDF)
3. Fetch resolution providers + strategy
4. Storage backend
5. RBAC (Casbin)
6. Bootstrap (default admin + repository)
7. AI providers (ollama, openrouter)
8. Decomposition providers (chunking, fact extraction, image extraction, concept extraction, alias generation)
9. Embedding, summarization, synthesis providers
10. Ontology source (DBpedia L3)
11. Qdrant store
12. Task manager
13. HTTP handler + OAuth server + MCP server
14. HTTP server + graceful shutdown