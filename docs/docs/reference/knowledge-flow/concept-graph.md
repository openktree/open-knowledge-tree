---
id: concept-graph
sidebar_position: 8
title: The Concept Graph
---

# The Concept Graph

The concept graph is the structure that emerges from stages 5-7. Concepts are nodes, aliases are node labels, contexts are node facets, shared facts are weighted edges, and syntheses are node "definitions." Here's how the pieces fit together.

## Nodes: concepts

Each concept row (`okt_repository.concepts`) is a node with:
- A **canonical name** — the display name (e.g. "Albert Einstein").
- A **context** — the ontological category (e.g. "Scientist", from the DBpedia L3 list).
- A **description** — optional longer text.
- An **embedding** — the vector of `canonical_name + " " + context` in Qdrant's `okt_concepts` collection.

Uniqueness is `(repository_id, lower(canonical_name), lower(context))`. The same name under different contexts is a different node.

## Labels: aliases

Each concept has a set of aliases (`okt_repository.concept_aliases`). An alias is any surface form the concept is known by. `FindConceptByAlias` does a text search scoped by `(repository_id, lower(context))` against `concept_aliases.lower(alias_text)`, so a fact mentioning "Einstein", "A. Einstein", or "Albert Einstein" in a Scientist-context fact all link to the same concept.

Aliases come from three sources:
1. **Seed aliases** — emitted by the concept-extraction LLM (2-3 per fact).
2. **Generated aliases** — from the alias-generation LLM (3-6 per new concept).
3. **Free recall boost** — when a fact's seed alias matches an existing concept, the alias is added via `AddConceptAlias` with `ON CONFLICT DO NOTHING`, no LLM call. This means the alias set grows organically as more facts mention the concept by new names.

## Edges: shared facts

The `concept_relations` materialized view (migration 0030) computes edges:
- `name_a`, `name_b` — the `lower(canonical_name)` of two concepts, ordered `name_a < name_b`.
- `shared_fact_count` — the number of facts linked to both concepts via `fact_concepts`.
- Self-pairs are excluded.
- Unique `(repository_id, name_a, name_b)` allows `REFRESH MATERIALIZED VIEW CONCURRENTLY`.

The view is refreshed by the `refresh_concept_relations` worker (fanned out from `extract_concepts`, plus a periodic `RefreshAllConceptRelations` job). It's per-database, deduped via River unique-args so bursts of concept extractions coalesce into one refresh.

## Facets: contexts

Contexts disambiguate the same surface name. "Apple" in context "Company" and "Apple" in context "Molecule" are two nodes. But several read paths group by `lower(canonical_name)` across contexts:
- `concept_relations` groups by `lower(canonical_name)` — so the edge counts fold all contexts.
- `concept_syntheses` groups by `lower(canonical_name)` — so one synthesis covers all contexts.

This gives you both the precise node-level view (facts link to the right disambiguated concept) and the coarse group-level view (synthesis and relations see the whole group).

## Definitions: syntheses

Each concept group (all contexts sharing a canonical name) has at most one synthesis (`okt_repository.concept_syntheses`, unique `(repository_id, lower(canonical_name))`). The synthesis folds all summary slices across the group + related concepts + images into one authoritative definition. See [Synthesis](/docs/reference/knowledge-flow/7-synthesis).

## The read paths

The graph is queried through three main read paths:

| Path | What it returns |
|------|-----------------|
| `getConcept` | The full concept group (all contexts) + the authoritative synthesis |
| `getConceptSummaries` | The per-context summary slices (frozen + open) |
| `getRelatedConcepts` | Concepts ranked by shared fact count (the edges) |
| `searchConcepts` | Concept groups filtered by canonical-name substring |

These are exposed as both MCP tools and REST API endpoints. See [MCP Tools](/docs/mcp/tools) and [REST API > Concepts](/docs/api/concepts).

## A visual summary

```
   Concept: "Einstein" / "Scientist"          Concept: "Relativity" / "Concept"
   aliases: Einstein, A. Einstein, ...        aliases: relativity, theory of relativity
        |                                              |
        |  fact_concepts                               |  fact_concepts
        |                                              |
        +---------> shared facts <---------------------+
                    (concept_relations edge:
                     shared_fact_count = N)

   Synthesis: "Einstein" (groups both contexts)
   folds: all summary slices for "einstein" across all contexts
   + related concepts (top N by shared_fact_count)
   + image candidates
```