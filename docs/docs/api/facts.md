---
id: facts
sidebar_position: 4
title: Facts API
---

# Facts API

Facts are the atomic, self-contained units extracted from sources. All fact routes are repo-scoped.

## List facts in repository

`GET /api/v1/repositories/{repoID}/facts`

Permission: `fact:read`. Returns facts in the repository with pagination + filters.

**Query params:**
- `q` — full-text search (Postgres `websearch_to_tsquery`)
- `concept` — concept UUID or canonical name filter
- `context` — context filter (with canonical-name `concept`)
- `limit`, `offset` — pagination

---

## Get fact

`GET /api/v1/repositories/{repoID}/facts/{factID}`

Permission: `fact:read`. Returns the fact's metadata, source URLs, and linked concepts.

---

## List fact concepts

`GET /api/v1/repositories/{repoID}/facts/{factID}/concepts`

Permission: `fact:read`. Returns the concepts linked to this fact.

---

## Fact lifecycle

Facts move through these statuses:

| Status | Meaning |
|--------|---------|
| `new` | Just extracted, not yet embedded or deduped |
| `stable` | Survived dedup, ready for concept extraction |
| `to_delete` | Lost a dedup match; will be cleaned up by `cleanup_facts` |

See [Deduplication](/docs/reference/knowledge-flow/4-deduplication) for the dedup algorithm.