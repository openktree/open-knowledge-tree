---
id: investigations
sidebar_position: 6
title: Investigations API
---

# Investigations API

Investigations collect sources around a topic. All routes are repo-scoped.

## List investigations

`GET /api/v1/repositories/{repoID}/investigations`

Permission: `investigation:read`.

---

## Create investigation

`POST /api/v1/repositories/{repoID}/investigations`

Permission: `investigation:write`.

**Body:** `{title, topic?}`

---

## Get investigation

`GET /api/v1/repositories/{repoID}/investigations/{investigationID}`

Permission: `investigation:read`. Returns metadata + sources.

---

## Update investigation

`PUT /api/v1/repositories/{repoID}/investigations/{investigationID}`

Permission: `investigation:update`.

---

## Delete investigation

`DELETE /api/v1/repositories/{repoID}/investigations/{investigationID}`

Permission: `investigation:delete`.

---

## List investigation sources

`GET /api/v1/repositories/{repoID}/investigations/{investigationID}/sources`

Permission: `investigation:read`.

---

## Add source to investigation

`POST /api/v1/repositories/{repoID}/investigations/{investigationID}/sources`

Permission: `investigation:write`.

**Body:** `{source_id}`

---

## Remove source from investigation

`DELETE /api/v1/repositories/{repoID}/investigations/{investigationID}/sources/{sourceID}`

Permission: `investigation:delete`.

---

## List investigation facts

`GET /api/v1/repositories/{repoID}/investigations/{investigationID}/facts`

Permission: `investigation:read`. Returns all facts from all sources in the investigation.

---

## List investigation concepts

`GET /api/v1/repositories/{repoID}/investigations/{investigationID}/concepts`

Permission: `investigation:read`. Returns all concepts from all facts in the investigation.