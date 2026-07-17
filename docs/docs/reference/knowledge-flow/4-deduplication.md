---
id: 4-deduplication
sidebar_position: 4
title: Stage 4 — Deduplication
---

# Stage 4: Deduplication

The fourth stage merges idea-level duplicates by embedding cosine similarity. This is where OKT solves one of the core problems: it collapses facts that mean the same thing despite different phrasing.

Entry point: `(*DeduplicateFactsWorker).Work` — `backend/internal/taskmanager/tasks/deduplicate_facts.go:100`.

This is where **embedding distance maps the flow of ideas**. The algorithm (commented at lines 84-99):

1. Acquire `pg_advisory_xact_lock(hashtext(repository_id))` (`deduplicate_facts.go:144`) — serializes per-repo dedup so concurrent sources don't interleave.
2. Load `new` + `stable` facts (`ListFactsForDedup`), sort UUID-ascending.
3. For each `new` fact: `qdrant.SearchSimilarByID(ctx, nfID, repoUUID, nfID, threshold, 1)` (`deduplicate_facts.go:206`) — finds the nearest neighbor by **cosine similarity in embedding space**, excluding self, with score >= `dedupCfg.Threshold`.
   - Hit `stable` fact -> mark the `new` fact as `to_delete`; relink sources to the stable survivor.
   - Hit `new` fact -> lexicographically-larger UUID loses; survivor inherits sources.
   - Hit `to_delete` fact -> skip.
4. `mergeSources` (`deduplicate_facts.go:363`): relinks `fact_sources`, `fact_references` (`DeleteDuplicateFactReferences` + `RelinkFactReferences`), and `fact_concepts` (`RelinkFactConcepts`) onto the winner. **Dedup preserves all provenance** — the survivor accumulates every source and sentence reference the loser had.
5. Promote surviving `new` -> `stable` (`MarkFactsStableByRepo`); update Qdrant payloads.
6. **Chain out**: enqueues `extract_concepts` (`deduplicate_facts.go:327`).

## Why this matters

Dedup is **idea-level, not string-match**. Two facts phrased differently but meaning the same thing merge. This means a fact from a Wikipedia article and a fact from a research paper saying the same thing become one fact with two sources. The embedding distance is what later allows mapping the flow of ideas across sources — you can trace how the same idea appears, propagates, and mutates across the corpus.

## The fact lifecycle

```
new -> stable    (after dedup, survived)
new -> to_delete  (after dedup, lost to a duplicate)
to_delete -> deleted  (after cleanup_facts, stage 6a chain-out)
```

`cleanup_facts` (enqueued by `embed_concepts` at `embed_concepts.go:223`) is the terminal cleanup that deletes `to_delete` facts from both Qdrant and Postgres.
