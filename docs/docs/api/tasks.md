---
id: tasks
sidebar_position: 9
title: Tasks API
---

# Tasks API

The task endpoints expose the River job queue: the 7-stage ingestion pipeline plus annotation jobs.

## List repo jobs

`GET /api/v1/repositories/{repoID}/tasks`

Permission: `task:read`. Returns jobs for the repository with pagination + filters.

**Query params:**
- `state` — `available|running|retryable|pending|scheduled|completed|cancelled|discarded`
- `kind` — job kind (e.g. `retrieve_source`, `source_decomposition`, `embed_facts`, `deduplicate_facts`, `extract_concepts`, `synthesize_concept`)
- `limit`, `offset`

---

## System-level task routes

| Method | Path | Permission | Description |
|--------|------|------------|-------------|
| `GET` | `/api/v1/tasks` | `task:read` | List all jobs (system-wide) |
| `GET` | `/api/v1/tasks/stats` | `task:read` | Aggregate job stats (counts by state/kind) |
| `GET` | `/api/v1/tasks/{jobID}` | `task:read` | Get a single job's details |
| `GET` | `/api/v1/admin/tasks/{jobID}` | `task:read` | Admin job detail (when task DB configured) |
| `POST` | `/api/v1/admin/tasks/{jobID}/cancel` | `task:cancel` | Cancel a job |
| `POST` | `/api/v1/admin/tasks/rescue` | `task:manage` | Rescue stuck jobs |

---

## The pipeline job kinds

| Kind | Stage | Fan-out |
|------|-------|---------|
| `retrieve_source` | 1 | 1 per source |
| `source_decomposition` | 2 | 1 per source |
| `embed_facts` | 3 | 1 per source batch |
| `deduplicate_facts` | 4 | 1 per source |
| `extract_concepts` | 5 | 1 per fact batch |
| `embed_concepts` | 6a | 1 per concept batch |
| `cleanup_facts` | terminal | 1 per source |
| `summarize_concepts` | 6b | 1 per concept |
| `synthesize_concept` | 7 | 1 per concept group |
| `refresh_concept_relations` | matview | 1 per database (deduped) |
| `annotate_report` | off-chain | 1 per report |

A single source fans out into many jobs (one per fact for embed/dedup/extract, one per concept for summarize/synthesize). 100 sources can produce thousands of jobs and take an hour to fully drain. See [Architecture > Task Manager](/docs/architecture/task-manager).