---
id: 1-research
sidebar_position: 1
title: Phase 1 — Research
---

# Phase 1: Research (grow the graph)

The research phase gathers sources around a topic and feeds them into the repository so the [Knowledge Flow](/docs/reference/knowledge-flow/overview) can turn them into facts, concepts, and syntheses. This is the only phase that *writes* to the graph; query and reports only read.

## What a research agent does

A research agent is responsible for:

1. **Scoping the topic** — deciding what the investigation is about and naming it.
2. **Finding sources** — searching the web or academic indexes for candidate URLs or DOIs.
3. **Fetching sources** — feeding each candidate into OKT, which enqueues the 7-stage pipeline.
4. **Organizing sources** — optionally collecting them under an investigation so they can be read back together.
5. **Waiting for ingestion** — polling until the pipeline drains before query or report phases can trust the graph.

The judgement in this phase is *which* sources to fetch: a good research agent iterates its search queries as it sees what comes back, skips hits that are already ingested, and stops when the candidate pool is exhausted or the topic is covered.

## Tools

| Tool | Role in this phase |
|------|--------------------|
| `getRepositories` | Pick the repository to work in. |
| `listSearchProviders` | See which search providers (web, academic) are enabled. |
| `searchSources` | Find candidate URLs or DOIs. Iterate the query as needed. |
| `createInvestigation` | Open a container that collects the sources for this topic. |
| `fetchAndProcessSource` | Fetch a URL/DOI into the repo (optionally with `investigationId` to link it in). This enqueues the full 7-stage Knowledge Flow pipeline. |
| `addInvestigationSource` | Reorganize an already-fetched source into the investigation. |
| `getInvestigation` | Read back the investigation and its sources. |
| `getSourceTasks` | Poll ingestion progress; **do not query or synthesize until the pipeline drains.** |

See the [MCP Tools Reference](/docs/mcp/tools) for full parameter and return shapes.

## Building a research agent

A research agent should be scoped tightly: it only needs the tools above, and its prompt should emphasize iterating search queries, skipping `already_exists` hits, and polling `getSourceTasks` to completion before declaring the phase done.

Key behaviours to encode in the prompt:

- **One investigation per topic.** Open it once with `createInvestigation`; feed every fetched source into it via `fetchAndProcessSource` with `investigationId`.
- **Iterate the query.** A single `searchSources` call is rarely enough. Encode a loop: search, fetch the useful hits, then search again with a sharper or broader query until the results are exhausted.
- **Respect the drain.** After fetching, poll `getSourceTasks` until `complete` is true before handing off to the query phase. Querying or reporting against a half-ingested graph produces thin results.
- **Skip duplicates.** `searchSources` returns `already_exists` for hits already in the repo — don't re-fetch them.

## The search-fetch-drain loop

The research phase is itself a loop, not a single pass. A good research agent repeats three steps until the candidate pool is exhausted or the topic is covered:

1. **Search** — call `searchSources` with the current query. Inspect the hits: titles, snippets, and the `already_exists` flag.
2. **Fetch** — for each hit that is not already ingested and looks on-topic, call `fetchAndProcessSource` with the URL (or DOI) and the `investigationId`. This enqueues the 7-stage Knowledge Flow pipeline for that source.
3. **Drain** — poll `getSourceTasks` (scoped to the source or the investigation) until `complete` is true. Do not start the next search until the current batch has drained, or you will be searching a graph that does not yet reflect what you just fetched.

Between iterations, refine the query. If the first search returned mostly off-topic hits, narrow the query. If it returned a few good hits but the corpus is still thin after ingestion, broaden or rephrase — try synonyms, the names of key people or organizations that surfaced in the first batch, or a different search provider (`listSearchProviders` shows what is available; `serper` is web, `openalex` is academic works).

The drain step is what keeps the loop honest. The Knowledge Flow is async: `fetchAndProcessSource` returns immediately with a `source_id`, and the pipeline runs in the background. Searching again immediately is fine, but handing off to the [Query](/docs/reference/agentic-flow/2-query) phase before the pipeline drains means the query agent reads a graph that doesn't yet contain the facts from the sources you just fetched. The drain protocol in `getSourceTasks` exists for exactly this reason — poll until `pending_count == 0` and `complete` is true.

## Handing off

When the pipeline has drained and the investigation holds enough sources, the research phase hands off to the [Query](/docs/reference/agentic-flow/2-query) phase. If a later query phase finds the graph thin, it loops back here with a sharper search query.

## See also

- [Knowledge Flow](/docs/reference/knowledge-flow/overview) — the 7-stage pipeline each `fetchAndProcessSource` enqueues.
- [Investigations API](/docs/api/investigations) — the REST endpoints for investigations.
- [MCP Tools Reference](/docs/mcp/tools) — full parameter and return shapes for every tool.