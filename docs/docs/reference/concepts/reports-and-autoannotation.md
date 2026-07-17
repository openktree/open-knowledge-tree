---
id: reports-and-autoannotation
sidebar_position: 4
title: Reports and Auto-Annotation
---

# Reports and Auto-Annotation

A **report** is a markdown document an agent or human authors against the knowledge graph. The defining feature of an OKT report is **auto-annotation**: every sentence in the report is matched against the repository's facts by embedding similarity, so each claim is anchored to the facts that support (or contradict) it.

## What a report is

A report is just raw markdown text plus a title and optional topic. It has no special schema — you write it as you would any markdown document. What makes it an OKT report is what happens after you submit it: an annotation job runs and attaches fact matches to each sentence.

## The annotation pipeline

When a report is created (via the REST API or the `createReport` MCP tool), OKT enqueues an **auto-fact annotation job**. The job:

1. **Chunks the report into sentences**.
2. **Embeds each sentence** into Qdrant.
3. **Searches the repository's facts** for similar ones above the configured similarity threshold.
4. **Stores each match as a `report_annotation`** — the sentence index, the matched fact (id, text, source count, etc.), and the cosine similarity score (0..1, higher = stronger match).

The annotated body is then readable via `getReport` (MCP) or `GET /reports/{id}` (REST): the `body_md` carries inline fact citations, and the `annotations` array has the per-sentence detail.

## Re-annotation

A report can be re-annotated at any time — the `POST /reports/{id}/annotate` endpoint enqueues the annotation job again. This matters when:

- The report body was edited (new sentences need matches).
- New facts have been ingested since the last annotation (existing sentences may now match more or better facts).

Re-annotation is idempotent in effect: it recomputes the full annotation set from the current body and fact corpus.

## Report statuses

| Status | Meaning |
|--------|---------|
| `pending` | Created, annotation job not yet started |
| `processing` | Annotation job is running |
| `annotated` | Annotation complete; `body_md` + `annotations` are ready |
| `failed` | Annotation job failed |

## Why auto-annotation matters

The annotation is what makes a report **grounded** rather than just generated. Each sentence carries the facts it rests on, with a similarity score, so a reader can:

- See which claims are well-supported vs thin.
- Drill from a sentence into the matching fact and from there into the source URLs and sentence offsets.
- Detect claims with no match in the corpus — sentences that rest on model knowledge rather than consumed sources, which the system is explicitly designed to surface.

This is the closed loop of the [Agentic Flow](/docs/reference/agentic-flow/overview): the agent researches sources, the Knowledge Flow grows the graph, the agent authors a report, and OKT annotates the report back to the facts the graph produced. Sources in, annotated reports out.

## See also

- [Reports API](/docs/api/reports) — the REST endpoints.
- [MCP Tools Reference](/docs/mcp/tools) — `createReport`, `getReport`, `getReportTasks`, `listReports`.