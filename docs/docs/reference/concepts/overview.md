---
id: overview
sidebar_position: 0
title: Concepts Overview
---

# OKT Core Concepts

OKT produces and stores a small set of artifact types. This section defines each one and how they relate to the pipeline and the graph.

- **[Facts](/docs/reference/concepts/facts)** — atomic, self-contained units of knowledge extracted from sources. The substrate everything else is built on.
- **[Concepts and Contexts](/docs/reference/concepts/concepts-and-contexts)** — nodes in the knowledge graph. A concept groups facts about the same thing; the context disambiguates surface-name collisions ("Apple" the company vs "Apple" the molecule).
- **[Summaries and Synthesis](/docs/reference/concepts/summaries-and-synthesis)** — the slow-accumulation layer. Summaries incrementally fold new facts into per-concept slices; synthesis folds all slices for a concept group into one authoritative definition.
- **[Reports and Auto-Annotation](/docs/reference/concepts/reports-and-autoannotation)** — markdown documents an agent or human authors; OKT annotates each sentence with the facts it rests on by embedding similarity.

## How the artifacts stack

```
sources
   |
   v
facts ──────> concepts (grouped by canonical name + context)
   |              |
   |              v
   |          summaries (per-concept incremental slices)
   |              |
   |              v
   |          synthesis (one per concept group)
   |
   v
reports ──> annotations (per-sentence fact matches)
```

Facts are the atoms. Concepts are the nodes that group them. Summaries and synthesis are the crystallized knowledge per concept. Reports are authored against the graph and annotated back to facts. Every artifact traces back to the sources it came from.