---
id: task-manager
sidebar_position: 4
title: Task Manager (River)
---

# Task Manager (River)

OKT uses [River](https://riverqueue.com/) (Postgres-backed) as its background job queue. The task manager is wired in `backend/internal/taskmanager/taskmanager.go`.

## Worker registration

Workers are registered in `taskmanager.New` (`taskmanager.go:206-280`), in pipeline order:

| # | Job kind | Queue | Worker file |
|---|----------|-------|-------------|
| 1 | `retrieve_source` | `QueueRetrieveSource` | `tasks/retrieve_source.go` |
| 2 | `source_decomposition` | `QueueSourceDecomposition` | `tasks/source_decomposition.go` |
| 3 | `embed_facts` | `QueueEmbedFacts` | `tasks/embed_facts.go` |
| 4 | `deduplicate_facts` | `QueueDeduplicateFacts` | `tasks/deduplicate_facts.go` |
| 5 | `extract_concepts` | `QueueExtractConcepts` | `tasks/extract_concepts.go` |
| 6a | `embed_concepts` | `QueueEmbedConcepts` | `tasks/embed_concepts.go` |
| 6b | `summarize_concepts` | `QueueSummarizeConcepts` | `tasks/summarize_concepts.go` |
| 6c | `refresh_concept_relations` | `QueueRefreshConceptRelations` | `tasks/refresh_concept_relations.go` |
| 7 | `synthesize_concept` | `QueueSynthesizeConcept` | `tasks/synthesize_concept.go` |
| - | `cleanup_facts` | `QueueCleanupFacts` | `tasks/cleanup_facts.go` |
| - | `contribute_source` / `contribute_all` | `QueueContributeSource` | `tasks/contribute_source.go` |
| - | `pull_all_from_registry` | `QueuePullAllFromRegistry` | `tasks/pull_all_from_registry.go` |
| - | `fact_catchup` | `QueueFactCatchup` | `tasks/fact_catchup.go` |
| - | `migrate_context` | `QueueMigrateContext` | `tasks/migrate_context.go` |
| - | `annotate_report` | `QueueAnnotateReport` | `tasks/annotate_report.go` |

## The serial chain

```
retrieve_source -> source_decomposition -> embed_facts -> deduplicate_facts -> extract_concepts
                                                       |
                                                       +-> embed_concepts -> cleanup_facts
                                                       +-> summarize_concepts -> synthesize_concept
                                                       +-> refresh_concept_relations
```

Each stage enqueues the next via `river.ClientFromContext[pgx.Tx](ctx).Insert(...)` with a fresh context (River cancels the worker ctx on completion).

## Fan-out

A single source fans out into many jobs:
- One per fact for `embed_facts` / `deduplicate_facts` / `extract_concepts`.
- One per concept for `summarize_concepts` / `synthesize_concept`.

So 100 sources can produce thousands of jobs and take an hour to fully drain.

## Advisory locks

Several stages use Postgres advisory locks to prevent concurrent execution on the same scope:
- `deduplicate_facts` — `pg_advisory_xact_lock(hashtext(repository_id))` — serializes per-repo dedup.
- `synthesize_concept` — `pg_try_advisory_lock(hashtext(repo_id || ':' || lower(canonical_name)))` — one writer per concept group.
- `summarize_concepts` — `ClaimConceptForSummary` (per-concept lock via `concepts.summarizing_at`, stale after 2h).

## Drain protocol

The `getSourceTasks` MCP tool and `GET /tasks` API expose a summary mode that returns `pending_count` + `complete`. A source is fully ingested when `complete=true` and `pending_count=0`. Poll with `verbose=false` (the global summary), not `verbose=true` (per-page, can't confirm drain). Wait proportionally: 1 source takes minutes, 100 sources take an hour. Sleep 15-30s between polls. Never synthesize while `pending_count > 0`.

## Adding a new task

1. Define `JobArgs` + a `Worker` in `internal/taskmanager/tasks/<name>.go`.
2. Register the worker in `taskmanager.New`.
3. If the HTTP layer needs to enqueue it, expose an `Enqueue*` helper on the `Manager`.
4. Tasks share a River schema; the DB connection is read from the `*dbpool.Pool` passed to `taskmanager.New` (resolved from `cfg.Task.Database`).