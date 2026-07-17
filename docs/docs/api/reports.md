---
id: reports
sidebar_position: 7
title: Reports API
---

# Reports API

Reports are markdown documents that get auto-annotated with supporting facts via embedding similarity. All routes are repo-scoped.

## List reports

`GET /api/v1/repositories/{repoID}/reports`

Permission: `report:read`.

**Query params:**
- `search` — matches title or topic via ILIKE
- `status` — `pending`, `processing`, `annotated`, or `failed`
- `limit`, `offset` — pagination

---

## Create report

`POST /api/v1/repositories/{repoID}/reports`

Permission: `report:write`. Enqueues an autofact-annotation job.

**Body:** `{title, text, topic?}`

The job chunks the report into sentences, embeds each, and searches the repository's facts for similar ones above the similarity threshold. Matches become `report_annotations`.

---

## Upload report

`POST /api/v1/repositories/{repoID}/reports/upload`

Permission: `report:write`. Upload a markdown file as a report.

**Body:** `multipart/form-data` with the file + title + topic form fields.

---

## Get report

`GET /api/v1/repositories/{repoID}/reports/{reportID}`

Permission: `report:read`. Returns metadata + the annotated body (`body_md` with inline fact citations) + annotations array.

---

## Update report

`PUT /api/v1/repositories/{repoID}/reports/{reportID}`

Permission: `report:update`.

---

## Delete report

`DELETE /api/v1/repositories/{repoID}/reports/{reportID}`

Permission: `report:delete`.

---

## Annotate report

`POST /api/v1/repositories/{repoID}/reports/{reportID}/annotate`

Permission: `report:update`. Re-enqueues the annotation job (e.g. after editing the body or after new facts have been ingested).

---

## List annotations

`GET /api/v1/repositories/{repoID}/reports/{reportID}/annotations`

Permission: `report:read`. Returns the per-sentence annotations (each with the matched fact + cosine score).

---

## Report statuses

| Status | Meaning |
|--------|---------|
| `pending` | Created, annotation job not yet started |
| `processing` | Annotation job is running |
| `annotated` | Annotation complete; `body_md` + `annotations` are ready |
| `failed` | Annotation job failed |