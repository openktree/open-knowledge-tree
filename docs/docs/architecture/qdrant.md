---
id: qdrant
sidebar_position: 5
title: Qdrant
---

# Qdrant

Qdrant is OKT's vector store. It is a **dumb vector store** — Postgres is the single source of truth for everything except the vectors themselves. Qdrant payloads carry only `{repository_id, status}` (or `{repository_id}` for concepts).

## Collections

| Collection | Payload | Purpose |
|------------|---------|---------|
| `okt_facts` | `{repository_id, status}` | Fact vectors for semantic search + dedup |
| `okt_concepts` | `{repository_id}` | Concept vectors for similarity |

Both are created at boot via `EnsureCollection` / `EnsureConceptCollection` (`backend/cmd/app/api.go:430-470`). If Qdrant is unreachable, the API boots without the embedding+dedup pipeline (fact endpoints still serve, but no dedup or embedding happens).

## Point IDs

The Qdrant point ID is the fact/concept UUID — the same ID used in Postgres. This makes it trivial to go from a Qdrant search result back to the Postgres row.

## Fact lifecycle in Qdrant

When a fact is created (status `new`), it has no Qdrant point. `embed_facts` upserts the vector. After dedup:
- The **winner** keeps its Qdrant point (payload updated to `status: stable`).
- The **loser** (status `to_delete`) is deleted from Qdrant by `cleanup_facts`.

## Operations

| Operation | File | When |
|-----------|------|------|
| `UpsertFactVectors` | `qdrantstore/points.go:48` | `embed_facts` worker |
| `SearchSimilarByID` | `qdrantstore/search.go` | `deduplicate_facts` worker |
| `UpsertConceptVectors` | `qdrantstore/points.go` | `embed_concepts` worker |
| Delete fact points | `qdrantstore/points.go` | `cleanup_facts` worker |

## Configuration

Qdrant connection is via env vars:

| Variable | Description |
|----------|-------------|
| `QDRANT_HOST` | Qdrant host (empty = disable pipeline) |
| `QDRANT_PORT` | Qdrant gRPC port (default 6334) |

The REST port (6333) is for the dashboard; the gRPC port (6334) is what the application talks to.

## Why not pgvector?

Qdrant was chosen because:
- It's a purpose-built vector store with efficient HNSW indexing.
- It keeps vector search load off the Postgres instance.
- It supports per-collection payload filtering (by `repository_id`).

Postgres stores `embedded_at` / `embedded_model` on the fact row to track that the vector exists, but the vector itself never lives in Postgres.