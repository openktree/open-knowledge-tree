---
id: 5-concept-alias-extraction
sidebar_position: 5
title: Stage 5 — Concept & Alias Extraction
---

# Stage 5: Concept & Alias Extraction

The fifth stage runs an LLM over each stable fact to extract a concept, a context (from an ontology), and seed aliases. It matches against existing concepts via aliases scoped by context, or creates a new one. This is what builds the knowledge graph.

Entry point: `(*ExtractConceptsWorker).Work` — `backend/internal/taskmanager/tasks/extract_concepts.go:155`.

## The working set

The worker loads `stable` facts that have no `fact_concepts` row and no `fact_concept_skips` row (`ListStableFactsForConceptExtraction`, one parallel wave per round — `Concurrency × FactBatchSize` facts, defaults 4 × 10 = 40). No advisory lock is needed; the `fact_concepts` unique index + `NOT EXISTS` filter make concurrent passes safe.

## The context ontology

The worker loads the per-repo allowed context list from `okt_system.repository_contexts` (`extract_concepts.go:204`). The default is the embedded DBpedia L3 class list (`backend/internal/providers/ontology/dbpedia_l3.json`), plus any admin-custom contexts. This gives the LLM a fixed vocabulary of contexts to assign, keeping the graph structured rather than free-form. The L3 list can be refreshed via `just dbpedia-pull`.

## Phase 1: parallel AI extraction

Per fact (`extract_concepts.go:279`):

1. `conceptExtractor.ExtractConcepts` returns `[]ExtractedConcept{Concept, Context, SeedAliases}`.
2. On a miss path (no existing concept match), `aliasProvider.GenerateAliases` produces `AliasResult{CanonicalName, Aliases}`.

### The concept-extraction prompt

`conceptExtractionPrompt` — `backend/internal/providers/decomposition/concept_extraction.go:13-59`.

The prompt instructs the LLM to extract 1-2 word concepts (people, places, molecules, orgs, ideas), assign a context from the provided L3 ontology list, and emit 2-3 seed aliases.

### The alias-generation prompt

`aliasGenerationPrompt` — `backend/internal/providers/decomposition/alias_generation.go:13-28`.

This prompt generates the canonical name (strict charset: letters, digits, spaces, hyphens, apostrophes) plus 3-6 alternate aliases. The canonical name is the concept's display name; the aliases are what `FindConceptByAlias` searches against.

## Phase 2: serial persistence

Per fact, in a short write tx (`extract_concepts.go:323`), `linkFactToConcept` (`extract_concepts.go:582`):

- **Match path**: `FindConceptByAlias` does a text search scoped by `(repository_id, lower(context))` against `concept_aliases.lower(alias_text)`. On hit -> `AddFactConcept` + merge `seed_aliases` via `AddConceptAlias` (`ON CONFLICT DO NOTHING` — a free recall boost, no LLM call).
- **Miss path**: `CreateConcept` (canonical_name + context), insert the canonical name + original concept text + generated aliases + seed aliases as `concept_aliases`, then `AddFactConcept`.

Per-fact LLM failures write a permanent `fact_concept_skips` row (`recordSkip`, `extract_concepts.go:546`) so the next pass doesn't retry forever.

## Disambiguation by context

This is the key mechanism for building a clean knowledge graph. The same surface name under different contexts creates **separate concept rows**. Uniqueness is `(repository_id, lower(canonical_name), lower(context))` (migration 0023).

For example:
- "Apple" in context "Company" -> concept row 1
- "Apple" in context "Molecule" -> concept row 2

`FindConceptByAlias` is scoped by context, so a fact mentioning "Apple" in a Company-context fact matches the Company concept, not the Molecule one. The context disambiguates the surface name without requiring the fact to spell out which "Apple" it means — the LLM assigns the context during extraction.

## Grouping across contexts

While contexts create separate concept rows, several read paths group by `lower(canonical_name)` across contexts:

- The `concept_relations` materialized view (migration 0030) groups by `lower(canonical_name)` across contexts for the relations view.
- `concept_syntheses` groups by `lower(canonical_name)` so one authoritative definition folds all contexts.

This means a concept like "Einstein" that appears in contexts "Scientist" and "Person" gets one synthesis that covers both, while remaining as two distinct concept rows for fact linking.

## Chain out

The worker fans out from `extract_concepts`:

- `embed_concepts` (`extract_concepts.go:379`) — vectorize concepts into Qdrant.
- `summarize_concepts` chunks (`extract_concepts.go:404`, gated on `summarizationEnabled`) — start the slow-accumulation layer.
- `refresh_concept_relations` (`extract_concepts.go:416`, deduped per-database via River unique-args) — refresh the matview.

## Key tables

| Table | Purpose |
|-------|---------|
| `okt_repository.concepts` | Concept nodes: canonical_name, context, description, embedded_at |
| `okt_repository.concept_aliases` | Aliases for each concept; index on `lower(alias_text)` for lookup |
| `okt_repository.fact_concepts` | Junction: fact &lt;-&gt; concept |
| `okt_repository.fact_concept_skips` | Permanent skip markers for facts that failed concept extraction |
| `okt_repository.concept_relations` | Materialized view: pairs of canonical names + shared fact count |