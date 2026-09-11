---
id: overview
sidebar_position: 0
title: Knowledge Flow Overview
---

# The Knowledge Flow

OKT's ingestion pipeline transforms a source URL into a knowledge graph through seven stages. Each stage is a River background job that enqueues the next, forming a chain that fans out at the concept stage. The workers are registered in `backend/internal/taskmanager/taskmanager.go:206-280`.

## The chain

```
retrieve_source -> source_decomposition -> embed_facts -> deduplicate_facts -> extract_concepts
                                                       |
                                                       +-> embed_concepts -> cleanup_facts
                                                       +-> summarize_concepts -> synthesize_concept
                                                       +-> refresh_concept_relations
```

| # | Job kind | Worker file | What it does |
|---|----------|-------------|--------------|
| 1 | `retrieve_source` | `tasks/retrieve_source.go` | Fetch the URL/DOI, parse the body, persist the source + sentence offsets |
| 2 | `source_decomposition` | `tasks/source_decomposition.go` | Chunk the text, LLM-extract self-contained facts, write facts + sentence references |
| 3 | `embed_facts` | `tasks/embed_facts.go` | Vectorize facts into Qdrant |
| 4 | `deduplicate_facts` | `tasks/deduplicate_facts.go` | Merge idea-level duplicates by embedding cosine similarity |
| 5 | `extract_concepts` | `tasks/extract_concepts.go` | LLM-extract concepts + aliases + context, build the graph |
| 6a | `embed_concepts` | `tasks/embed_concepts.go` | Vectorize concepts into Qdrant |
| 6b | `summarize_concepts` | `tasks/summarize_concepts.go` | Incremental per-concept summary slices |
| 6c | `refresh_concept_relations` | `tasks/refresh_concept_relations.go` | Refresh the concept_relations materialized view |
| 7 | `synthesize_concept` | `tasks/synthesize_concept.go` | Fold all slices into one authoritative synthesis per concept group |

Each stage enqueues the next via `river.ClientFromContext[pgx.Tx](ctx).Insert(...)` with a fresh context (River cancels the worker ctx on completion; chained inserts open their own tx).

## The two key ideas

**Coreference resolution happens during fact decomposition.** The fact-extraction LLM prompt explicitly instructs the model to replace every pronoun and demonstrative with the explicit entity, name the subject, and reject unresolvable fragments. There is no separate coreference module — the LLM does it inline, producing self-contained facts that survive without their surrounding context. See [Fact Decomposition](/docs/reference/knowledge-flow/2-fact-decomposition).

**Deduplication is idea-level, not string-level.** The dedup worker uses embedding cosine similarity against the Qdrant vector index to find facts that mean the same thing despite different phrasing. The survivor inherits all sources, sentence references, and concept links, so provenance is never lost. This same embedding distance is what later allows mapping the "flow of ideas" across sources. See [Embedding](/docs/reference/knowledge-flow/3-embedding) and [Deduplication](/docs/reference/knowledge-flow/4-deduplication).

## The knowledge graph

Concepts are nodes. Each concept has a canonical name, a context (from an ontology), and a set of aliases. The same surface name under different contexts ("Apple" the company vs "Apple" the molecule) creates separate concept rows — disambiguation is by `(repository_id, lower(canonical_name), lower(context))`. Aliases let facts match concepts by any name the concept is known by. The `concept_relations` materialized view computes weighted edges by shared fact count. See [Concept & Alias Extraction](/docs/reference/knowledge-flow/5-concept-alias-extraction) and [Concept Graph](/docs/reference/knowledge-flow/concept-graph).

## Summaries and synthesis: system 2 artifacts

Summaries and syntheses are the slow-accumulation layer. Summaries incrementally fold new facts into per-concept slices (frozen complete slices + one open accumulating slice), reasoning only from the provided facts. Synthesis folds all slices for a concept group into one authoritative definition, using related-concept context for graph-aware reasoning. These artifacts accumulate over time and are accessed on demand — they are the system's "system 2" thinking, as opposed to the "system 1" fast retrieval of raw facts. See [Summaries](/docs/reference/knowledge-flow/6-summaries) and [Synthesis](/docs/reference/knowledge-flow/7-synthesis).

## Where the data lives

- **Postgres** (`okt_repository` schema): sources, facts, fact_sources, fact_references, concepts, concept_aliases, fact_concepts, concept_summaries, concept_syntheses, concept_relations (matview).
- **Qdrant**: fact vectors (`okt_facts` collection) and concept vectors (`okt_concepts` collection). Qdrant is a dumb vector store — payloads carry `{repository_id, status}` only; Postgres is the single source of truth for everything except the vector.

See [Architecture > Schema](/docs/architecture/schema) for the full table reference.

## Beyond the core stages: ops and federation jobs

The seven stages above are the ingestion spine, but the worker registry in `backend/internal/taskmanager/tasks/` holds 23 job kinds. The rest fall into three groups:

**Federation (registry contribute / pull)**

- `contribute_source` — upload one source's decomposition package (facts, concepts, embeddings) to the registry; chained after `cleanup_facts` so only stable, deduplicated facts leave the repo.
- `contribute_all` — contribute every eligible source in the repository to the registry.
- `pull_all_from_registry` — pull every cached source the registry can offer this repository (the "Pull All" button).
- `pull_remote_batch` — pull a batch of remote registry source ids into the local repository (the Remote UI's "Pull page" / "Pull all results" buttons).
- `export_graph` — build a whole-repository graph bundle and push it to the registry.
- `import_graph` — pull a shared graph bundle and re-insert every entity into the target repository.

**Maintenance & ops**

- `fact_catchup` — daily sweep that deletes stale `to_delete` / `new` facts (older than `dedup.catchup_max_age`) across every database, including their Qdrant vectors.
- `audit_cleanup` — daily prune of `okt_system.permission_audit` rows past the configured retention.
- `refresh_concept_relations` — refresh the `concept_relations` materialized view (stage 6c above; also runs on a schedule via the companion `refresh_all_concept_relations` job so every database stays fresh).
- `recompute_concept_groups` — full recompute of the `concept_groups` summary table, repairing drift from the incremental updates.
- `migrate_context` — merge a concept's context into another ontology class and re-map its facts.

**Reporting**

- `annotate_report` — the report auto-citation pass: chunk the report body into sentences, embed each, and match against the repository's facts (with supports / contradicts / related posture classification).

All of these share the same River queue infrastructure and are observable through the REST `/tasks` endpoints and the `getSourceTasks` / `getReportTasks` MCP tools.