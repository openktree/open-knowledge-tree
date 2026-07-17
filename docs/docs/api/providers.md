---
id: providers
sidebar_position: 8
title: Providers API
---

# Providers API

Provider configuration and testing. These are system-level routes (not repo-scoped), except where noted.

## List providers

`GET /api/v1/sources/providers`

Permission: `source_provider:read`. Lists configured search and fetch providers with their status.

---

## Test search

`POST /api/v1/sources/{provider}/search`

Permission: `source_provider:execute`. Runs a search against a provider (serper or openalex) and returns candidate results.

**Body:** `{query, per_page?}`

---

## Classify resource

`POST /api/v1/sources/classify`

Permission: `source_provider:execute`. Classifies a URL or DOI into a resource type (e.g. `SourceURL`, `SourceDOI`).

**Body:** `{url?}` or `{doi?}`

---

## Enqueue retrieve source

`POST /api/v1/sources/retrieve`

Permission: `source_provider:execute`. Enqueues a source fetch (system-level, without a repo scope).

---

## List decomposition providers

`GET /api/v1/sources/decomposition/providers`

Permission: `decomposition:read`. Lists the chunking + fact-extraction providers configured for the deployment.

---

## Search provider types

| Provider ID | Type | Description |
|-------------|------|-------------|
| `serper` | search | Google web search |
| `openalex` | search | Academic works (OpenAlex) |

See [Architecture > Providers](/docs/architecture/providers) for the full provider strategy.