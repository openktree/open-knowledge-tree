---
id: concepts-and-contexts
sidebar_position: 2
title: Concepts and Contexts
---

# Concepts and Contexts

A **concept** is a node in OKT's knowledge graph. It groups facts that are about the same thing. A **context** is the ontological category a concept sits in, and it is the disambiguation mechanism that keeps the graph clean.

## What a concept is

Each concept has:

- A **canonical name** — the display name, e.g. "Albert Einstein", "Apple", "General Relativity".
- A **context** — an ontology class assigned during [concept extraction](/docs/reference/knowledge-flow/5-concept-alias-extraction), e.g. "Scientist", "Company", "Molecule".
- A set of **aliases** — alternate names the concept is known by. Alias lookup is how a fact gets matched to an existing concept.

A fact links to one or more concepts via the `fact_concepts` junction. The collection of all concepts and their fact links is the knowledge graph.

## Disambiguation by context

The same surface name under different contexts creates **separate concept rows**. Uniqueness is `(repository_id, lower(canonical_name), lower(context))`.

For example:

- "Apple" in context `Company` → concept row 1
- "Apple" in context `Molecule` → concept row 2

`FindConceptByAlias` is scoped by context, so a fact mentioning "Apple" in a Company-context fact matches the Company concept, not the Molecule one. The context disambiguates the surface name without requiring the fact to spell out which "Apple" it means — the LLM assigns the context during extraction.

## The context ontology

Contexts are not free-form. The worker loads a per-repo allowed context list — by default the embedded DBpedia L3 class list — and the extraction LLM is constrained to pick from that vocabulary. This keeps the graph structured rather than a bag of ad-hoc labels. Admins can add custom contexts per repository.

## Concept groups (cross-context)

While contexts create separate concept rows, several read paths **group by `lower(canonical_name)` across contexts**:

- The `concept_relations` materialized view groups by canonical name across contexts — that's how [related concepts](/docs/mcp/tools) are computed (pairs of canonical names ranked by shared fact count).
- `concept_syntheses` groups by canonical name, so one [synthesis](/docs/reference/concepts/summaries-and-synthesis) folds all contexts for the group.

This means a concept like "Einstein" that appears in contexts `Scientist` and `Person` gets one synthesis covering both, while remaining as two distinct concept rows for fact linking. The group is the unit of synthesis; the row is the unit of fact linking.

## How concepts get built

See [Concept & Alias Extraction](/docs/reference/knowledge-flow/5-concept-alias-extraction) for the full process page. In short: for each stable fact, an LLM extracts a concept + context + seed aliases. The worker looks up an existing concept by alias scoped by context; on a hit it links the fact to that concept and merges any new aliases (free recall boost, no extra LLM call). On a miss it creates a new concept row, inserts its aliases, and links the fact. Per-fact LLM failures write a permanent skip row so the next pass doesn't retry forever.

## Key tables

| Table | Purpose |
|-------|---------|
| `okt_repository.concepts` | Concept nodes: canonical_name, context, description |
| `okt_repository.concept_aliases` | Aliases for each concept; lookup index on `lower(alias_text)` scoped by context |
| `okt_repository.fact_concepts` | Junction: fact ↔ concept |
| `okt_repository.concept_relations` | Materialized view: pairs of canonical names + shared fact count |