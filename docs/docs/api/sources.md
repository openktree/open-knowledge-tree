---
id: sources
sidebar_position: 3
title: Sources API
---

# Sources API

All source routes are repo-scoped under `/{repoID}`.

## List sources

`GET /api/v1/repositories/{repoID}/sources`

Permission: `source:read`. Returns sources in the repository with pagination + filters.

---

## Create source (fetch)

`POST /api/v1/repositories/{repoID}/sources`

Permission: `source:write`. Enqueues the fetch + processing pipeline.

**Body:** `{url?, doi?, investigation_id?}`

Provide either `url` or `doi`. When `investigation_id` is set, the fetched source is linked into that investigation. Returns the source ID + job ID.

---

## Upload source

`POST /api/v1/repositories/{repoID}/sources/upload`

Permission: `source:write`. Upload a file (PDF, HTML, text) directly.

**Body:** `multipart/form-data` with the file.

---

## Get source

`GET /api/v1/repositories/{repoID}/sources/{sourceID}`

Permission: `source:read`. Returns the source row: URL, status, parsed content, sentence offsets, DOI, images.

---

## Delete source

`DELETE /api/v1/repositories/{repoID}/sources/{sourceID}`

Permission: `source:delete`.

---

## Process source

`POST /api/v1/repositories/{repoID}/sources/{sourceID}/process`

Permission: `source:update`. Enqueues decomposition for an already-fetched source.

---

## Retry source

`POST /api/v1/repositories/{repoID}/sources/{sourceID}/retry`

Permission: `source:update`. Re-enqueues a failed source through the pipeline.

---

## List source facts

`GET /api/v1/repositories/{repoID}/sources/{sourceID}/facts`

Permission: `source:read`. Returns facts extracted from this source.

---

## List source references

`GET /api/v1/repositories/{repoID}/sources/{sourceID}/references`

Permission: `source:read`. Returns sentence-level references (which sentences from this source support which facts).

---

## Serve source image

`GET /api/v1/repositories/{repoID}/sources/{sourceID}/images/{imageID}`

Permission: `source:read`. Streams a source image (inline image or PDF page render).

---

## Serve source body

`GET /api/v1/repositories/{repoID}/sources/{sourceID}/body`

Permission: `source:read`. Streams the original source body (e.g. the PDF).

---

## System-level source routes

These are not repo-scoped:

| Method | Path | Permission | Description |
|--------|------|------------|-------------|
| `GET` | `/api/v1/sources/providers` | `source_provider:read` | List configured search + fetch providers |
| `POST` | `/api/v1/sources/{provider}/search` | `source_provider:execute` | Test search against a provider |
| `POST` | `/api/v1/sources/classify` | `source_provider:execute` | Classify a URL/DOI into a resource type |
| `POST` | `/api/v1/sources/retrieve` | `source_provider:execute` | Enqueue a source fetch (system-level) |
| `GET` | `/api/v1/sources/decomposition/providers` | `decomposition:read` | List chunking + fact-extraction providers |