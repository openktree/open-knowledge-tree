---
id: tools
sidebar_position: 1
title: MCP Tools Reference
---

# MCP Tools Reference

All 18 tools registered in `backend/internal/api/handler/mcp.go:192-605`. Every tool takes a `repository` argument (UUID or slug) unless noted.

---

## Discovery

### getRepositories

List the repositories the authenticated user can access. No input.

**Returns:** array of `{id, name, slug, description, tier, roles}`.

---

### listSearchProviders

List the search providers available in this deployment and enabled for the given repository.

| Param | Type | Required | Description |
|-------|------|----------|-------------|
| `repository` | string | yes | Repository UUID or slug |

**Returns:** array of `{id, name, enabled, is_default}`. Provider ids: `serper` (Google web search), `openalex` (academic works). Repos can disable individual providers.

---

## Search

### searchSources

Search for candidate source URLs via a registered search provider.

| Param | Type | Required | Description |
|-------|------|----------|-------------|
| `repository` | string | yes | Repository UUID or slug |
| `query` | string | yes | Search query |
| `provider` | string | no | `serper` or `openalex` (default: configured default, typically `serper`) |
| `per_page` | number | no | Page size (0 = provider default) |
| `cursor` | string | no | Pagination cursor from `next_cursor` |

**Returns:** array of `{title, url, snippet, doi?, openalex_id?, published_at?, already_exists, existing_status}`. Feed `url` or `doi` into `fetchAndProcessSource`; skip hits where `already_exists` is true.

---

### searchFacts

Full-text search over a repository's facts.

| Param | Type | Required | Description |
|-------|------|----------|-------------|
| `repository` | string | yes | Repository UUID or slug |
| `query` | string | no | Postgres `websearch_to_tsquery` syntax (space-separated words, quoted phrases, OR/AND, negation with `-`). Empty returns newest. |
| `concept` | string | no | Concept UUID or canonical name filter |
| `context` | string | no | Context filter (only with canonical-name `concept`) |
| `limit` | number | no | Max facts (1-200, default 10) |
| `offset` | number | no | Pagination offset (default 0) |

**Returns:** array of `{id, text, status, fact_kind, source_count, created_at}`. Use `getFact` with an id to see source URLs.

---

### searchConcepts

List concept groups in a repository, optionally filtered by canonical-name substring.

| Param | Type | Required | Description |
|-------|------|----------|-------------|
| `repository` | string | yes | Repository UUID or slug |
| `query` | string | no | Canonical-name substring (case-insensitive) |
| `limit` | number | no | Max groups (1-200, default 50) |
| `offset` | number | no | Pagination offset (default 0) |

**Returns:** array of `{canonical_name, fact_count, contexts: [{concept_id, context, fact_count, aliases}]}`.

---

## Fact Detail

### getFact

Get a single fact's metadata, source URLs, and linked concepts.

| Param | Type | Required | Description |
|-------|------|----------|-------------|
| `repository` | string | yes | Repository UUID or slug |
| `factId` | string | yes | Fact UUID (from `searchFacts`) |

**Returns:** `{id, text, status, fact_kind, embedded_model, created_at, image_url, sources: [{url, parsed_title, first_seen_at}], source_count, concepts: [{id, canonical_name, context, description}], concept_count}`.

---

## Concept Detail

### getConcept

Get a concept's full group (all contexts sharing the canonical name) plus the authoritative synthesis.

| Param | Type | Required | Description |
|-------|------|----------|-------------|
| `repository` | string | yes | Repository UUID or slug |
| `concept` | string | yes | Concept UUID or canonical name |

**Returns:** the concept group (contexts, aliases, fact counts) + `synthesis` (the `concept_syntheses` content) when one exists.

---

### getConceptSummaries

Get the per-context summary slices for a concept group.

| Param | Type | Required | Description |
|-------|------|----------|-------------|
| `repository` | string | yes | Repository UUID or slug |
| `concept` | string | yes | Concept UUID or canonical name |

**Returns:** array of `{context, sequence_num, content, model, is_complete, covered_fact_count}`. See [Summaries](/docs/reference/knowledge-flow/6-summaries).

---

### getRelatedConcepts

List concepts related to a concept group, ranked by shared fact count.

| Param | Type | Required | Description |
|-------|------|----------|-------------|
| `repository` | string | yes | Repository UUID or slug |
| `concept` | string | yes | Concept UUID or canonical name |
| `limit` | number | no | Max entries (1-200, default 50) |
| `offset` | number | no | Pagination offset (default 0) |

**Returns:** array of `{canonical_name, concept_id, shared_fact_count}`.

---

## Ingestion

### fetchAndProcessSource

Fetch a URL or DOI into a repository. Enqueues the full 7-stage pipeline.

| Param | Type | Required | Description |
|-------|------|----------|-------------|
| `repository` | string | yes | Repository UUID or slug |
| `url` | string | no* | The URL to fetch. Required unless `doi` is given. |
| `doi` | string | no* | Bare DOI (e.g. `10.1234/example`). Used instead of `url`. |
| `investigationId` | string | no | Investigation UUID. When set, the worker links the source into this investigation. |

**Returns:** `{job_id, source_id, resource_type}`. Use `getSourceTasks` with the `source_id` to track progress.

---

### getSourceTasks

Track ingestion progress for a repository, a single source, or an investigation's sources.

| Param | Type | Required | Description |
|-------|------|----------|-------------|
| `repository` | string | yes | Repository UUID or slug |
| `sourceId` | string | no | Source UUID filter (mutually exclusive with `investigationId`) |
| `investigationId` | string | no | Investigation UUID filter (mutually exclusive with `sourceId`) |
| `verbose` | boolean | no | `true` = per-job row list; `false` (default) = global summary |
| `state` | string | no | River job state filter (inspection only) |
| `kind` | string | no | Job kind filter (inspection only) |
| `limit` | number | no | Max jobs per page (verbose only, default 50) |
| `cursor` | string | no | Pagination cursor (verbose only) |

**Returns (summary mode):** `{counts_by_state, counts_by_kind, counts_by_kind_and_state, pending_count, running_count, total, complete}`. `complete=true` means the pipeline has drained (`pending_count==0` globally). Wait proportionally: 1 source takes minutes, 10 sources take 10-20 min, 100 sources take an hour. Sleep 15-30s between polls. Never synthesize while `pending_count > 0`.

---

## Investigations

### getInvestigation

Get an investigation's metadata and the sources it collects.

| Param | Type | Required | Description |
|-------|------|----------|-------------|
| `repository` | string | yes | Repository UUID or slug |
| `investigationId` | string | yes | Investigation UUID |

**Returns:** `{id, title, topic, created_at, updated_at, sources: [{url, parsed_title, doi, created_at, added_at}]}`.

---

### createInvestigation

Create a new investigation in a repository.

| Param | Type | Required | Description |
|-------|------|----------|-------------|
| `repository` | string | yes | Repository UUID or slug |
| `title` | string | yes | Investigation title |
| `topic` | string | no | Free-text description |

**Returns:** `{id, title, topic, created_at}`.

---

### addInvestigationSource

Link an already-fetched source to an investigation. Idempotent.

| Param | Type | Required | Description |
|-------|------|----------|-------------|
| `repository` | string | yes | Repository UUID or slug |
| `investigationId` | string | yes | Investigation UUID |
| `sourceId` | string | yes | Source UUID (same repository) |

**Returns:** success/no-op confirmation. The preferred flow is `fetchAndProcessSource` with `investigationId`; use this only to reorganize already-fetched sources.

---

## Reports

### createReport

Create a report from raw markdown and enqueue autofact annotation.

| Param | Type | Required | Description |
|-------|------|----------|-------------|
| `repository` | string | yes | Repository UUID or slug |
| `title` | string | yes | Report title |
| `text` | string | yes | Report body as raw markdown |
| `topic` | string | no | Free-text description |

**Returns:** `{report_id, status}`. The annotation job chunks the report into sentences, embeds each, and searches the repository's facts for similar ones above the similarity threshold. Use `getReport` to read the annotated body.

---

### getReport

Get a report's metadata and per-sentence annotations.

| Param | Type | Required | Description |
|-------|------|----------|-------------|
| `repository` | string | yes | Repository UUID or slug |
| `reportId` | string | yes | Report UUID (from `createReport`) |

**Returns:** `{id, title, topic, status, body_md, sentence_count, similarity_threshold, embedded_model, created_at, annotations: [{sentence_index, sentence_text, fact: {id, text, status, fact_kind, source_count, created_at}, score}]}`. Score is cosine similarity 0..1 (higher = stronger match).

---

### listReports

List reports in a repository with optional filtering.

| Param | Type | Required | Description |
|-------|------|----------|-------------|
| `repository` | string | yes | Repository UUID or slug |
| `search` | string | no | Matches title or topic via ILIKE |
| `status` | string | no | `pending`, `processing`, `annotated`, or `failed` |
| `limit` | number | no | Max reports (1-200, default 50) |
| `offset` | number | no | Pagination offset (default 0) |

**Returns:** array of `{id, title, topic, status, sentence_count, created_at, updated_at}`. Use `getReport` for the full body.

---

### getReportTasks

Track annotation job progress. Mirrors `getSourceTasks` (same summary/verbose modes and drain protocol).

| Param | Type | Required | Description |
|-------|------|----------|-------------|
| `repository` | string | yes | Repository UUID or slug |
| `reportId` | string | no | Report UUID filter |
| `verbose` | boolean | no | `true` = per-job rows; `false` (default) = global summary |
| `state` | string | no | River job state filter (inspection only) |
| `kind` | string | no | Job kind filter (inspection only) |
| `limit` | number | no | Max jobs per page (verbose only, default 50) |
| `cursor` | string | no | Pagination cursor (verbose only) |

**Returns (summary mode):** same shape as `getSourceTasks`. `complete=true` means the annotation has drained and `getReport` can be called.