---
id: 3-embedding
sidebar_position: 3
title: Stage 3 — Embedding
---

# Stage 3: Embedding

The third stage vectorizes facts into Qdrant so that downstream stages can search by semantic similarity. This is what gives every fact a vector for semantic search.

Entry point: `(*EmbedFactsWorker).Work` — `backend/internal/taskmanager/tasks/embed_facts.go:89`.

1. Lists `new` facts with `embedded_at IS NULL` (`ListNewFactsForEmbedding`).
2. Bulk-embeds via `ai.EmbeddingProvider.Embed`. The provider can be Ollama, Ollama Cloud, or OpenRouter (`backend/internal/providers/ai/`).
3. Upserts vectors into **Qdrant** with payload `{repository_id, status}` (`qdrant.UpsertFactVectors`, `backend/internal/qdrantstore/points.go:48`). The Qdrant point ID is the fact UUID.
4. Marks each fact `embedded_at` + `embedded_model` (`MarkFactEmbedded`).
5. **Chain out**: enqueues `deduplicate_facts` (`embed_facts.go:204`).

Embeddings live in Qdrant, not Postgres. Postgres only stores `embedded_at` / `embedded_model` on the fact row to track that the vector exists. Qdrant is a dumb vector store — payloads carry `{repository_id, status}` only; Postgres is the single source of truth for everything except the vector.

## Qdrant collections

| Collection | Payload | Purpose |
|------------|---------|---------|
| `okt_facts` | `{repository_id, status}` | Fact vectors for semantic search + dedup |
| `okt_concepts` | `{repository_id}` | Concept vectors (created in stage 6a) |

Both collections are created at boot via `EnsureCollection` / `EnsureConceptCollection` (`backend/cmd/app/api.go:430-470`).
