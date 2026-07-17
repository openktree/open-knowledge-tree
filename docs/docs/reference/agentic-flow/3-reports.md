---
id: 3-reports
sidebar_position: 3
title: Phase 3 — Reports
---

# Phase 3: Create reports (anchor claims to facts)

The reports phase authors a markdown document against the knowledge graph and lets OKT annotate every sentence with the facts it rests on. This is where the agentic loop closes: sources in, annotated report out. See [Reports and Auto-Annotation](/docs/reference/concepts/reports-and-autoannotation) for the concept.

## What a report agent does

A report agent is responsible for:

1. **Authoring the report** — writing the markdown body that synthesizes what the graph knows about the topic.
2. **Submitting it for annotation** — creating the report so OKT enqueues the auto-fact-annotation job.
3. **Waiting for annotation** — polling until the annotation job drains.
4. **Reading the annotated report back** — inspecting per-sentence fact matches and similarity scores.
5. **Re-annotating if needed** — if the report was edited or new facts have since been ingested, re-running annotation.

The judgement here is *what to write* and *reading the annotations critically*: sentences with no fact match rest on model knowledge rather than consumed sources, which the system is designed to surface.

## Tools

| Tool | Role in this phase |
|------|--------------------|
| `createReport` | Submit raw markdown; an annotation job embeds each sentence and finds similar facts in the repo. |
| `getReportTasks` | Poll annotation progress until `complete`. |
| `getReport` | Read the annotated body, sentence by sentence, with matched facts and similarity scores. |
| `listReports` | Browse existing reports by title, topic, or status. |

See the [MCP Tools Reference](/docs/mcp/tools) for full parameter and return shapes.

## Building a report agent

A report agent needs the report tools above plus read access to the graph (the [Query](/docs/reference/agentic-flow/2-query) tools) so it can ground what it writes. Its prompt should emphasize that every claim should trace to a fact the agent already read.

Key behaviours to encode in the prompt:

- **Ground before writing.** Run the query phase first. The report should be synthesized from facts, syntheses, and summaries the agent has already read — not authored from model knowledge.
- **Write plain markdown.** `createReport` takes raw markdown for the body. There is no special citation syntax to add manually; the annotation job attaches fact matches per sentence automatically.
- **Wait for annotation.** After `createReport`, poll `getReportTasks` until `complete` is true before calling `getReport`. Reading before annotation completes gives you an unannotated body.
- **Read the annotations critically.** `getReport` returns each sentence with its matched facts and a cosine score (0..1, higher = stronger). Sentences with no match or a low score are claims the graph doesn't support — either rewrite them with grounding from the query phase, or loop back to research to fetch sources that cover the gap.
- **Re-annotate after edits.** If the report body is edited, or if new facts have been ingested since the last annotation, re-run the annotation job (via the REST `POST /reports/{id}/annotate` endpoint or by creating a fresh report) so the matches reflect the current corpus.

## The closed loop

The reports phase closes the agentic loop. If the annotations show thin grounding, the agent loops back: research (fetch more sources) → wait for drain → query (read the new graph) → report (re-author and re-annotate). The loop ends when the report's sentences are well-anchored to facts with good similarity scores.

## See also

- [Reports and Auto-Annotation](/docs/reference/concepts/reports-and-autoannotation) — the concept page.
- [Reports API](/docs/api/reports) — the REST endpoints, including re-annotation.