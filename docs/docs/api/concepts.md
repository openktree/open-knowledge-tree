---
id: concepts
sidebar_position: 5
title: Concepts API
---

# Concepts API

Concepts are the nodes of the knowledge graph. All concept routes are repo-scoped.

## List concepts

`GET /api/v1/repositories/{repoID}/concepts`

Permission: `concept:read`. Returns concept groups (all contexts sharing a canonical name) with fact counts.

**Query params:**
- `q` — canonical-name substring (case-insensitive)
- `limit`, `offset` — pagination

---

## Get concept

`GET /api/v1/repositories/{repoID}/concepts/{conceptID}`

Permission: `concept:read`. Returns the concept group (all contexts, aliases, fact counts) + the synthesis when one exists.

---

## List concept facts

`GET /api/v1/repositories/{repoID}/concepts/{conceptID}/facts`

Permission: `concept:read`. Returns facts linked to this concept.

---

## List concept summaries

`GET /api/v1/repositories/{repoID}/concepts/{conceptID}/summaries`

Permission: `concept:read`. Returns the per-context summary slices (frozen + open). See [Summaries](/docs/reference/knowledge-flow/6-summaries).

---

## Get concept definition (synthesis)

`GET /api/v1/repositories/{repoID}/concepts/{conceptID}/definition`

Permission: `concept:read`. Returns the authoritative synthesis for the concept group.

---

## List concept relations

`GET /api/v1/repositories/{repoID}/concepts/{conceptID}/relations`

Permission: `concept:read`. Returns related concepts ranked by shared fact count (from the `concept_relations` materialized view).

---

## Get concept relation details

`GET /api/v1/repositories/{repoID}/concepts/{conceptID}/relations/{otherConceptID}`

Permission: `concept:read`. Returns details about the relation between two concepts (shared facts, etc.).

---

See [Concept & Alias Extraction](/docs/reference/knowledge-flow/5-concept-alias-extraction) and [Concept Graph](/docs/reference/knowledge-flow/concept-graph) for how concepts are built.