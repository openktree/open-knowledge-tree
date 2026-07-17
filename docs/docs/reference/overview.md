---
id: overview
sidebar_position: 0
title: Reference
---

# Reference

This section is the reference for OKT's two process flows and its core concepts.

## Processes

Open Knowledge Tree runs two complementary processes. The **Knowledge Flow** turns a single source into a graph of facts, concepts, and syntheses. The **Agentic Flow** is what an agent does on top of that graph — research, query, and author reports — looping back to fetch more sources whenever the graph is thin.

### Knowledge Flow (per-source, automatic)

Given a URL or DOI, OKT fetches the content, decomposes it into atomic self-contained facts, embeds and deduplicates them by meaning, extracts concepts and aliases to build the graph, and incrementally accumulates summaries and an authoritative synthesis per concept. Seven stages, each a background job that enqueues the next. The moment a source is fetched the pipeline runs to completion without further input.

See [Knowledge Flow](/docs/reference/knowledge-flow/overview) for the stage-by-stage walkthrough.

### Agentic Flow (agent-driven, iterative)

An agent works on top of the graph in three phases — **research** (gather sources, open investigations, fetch them), **query** (search facts and concepts, read syntheses, walk related concepts), and **reports** (author markdown and get back per-sentence fact annotations). The phases are not a pipeline; the agent loops back to research whenever a query surfaces a thin area, and only authors a report once the graph covers the topic.

See [Agentic Flow](/docs/reference/agentic-flow/overview) for the phase-by-phase walkthrough.

### How they relate

The Knowledge Flow is the substrate. The Agentic Flow is the loop an agent runs against that substrate. The Agentic Flow feeds the Knowledge Flow (every `fetchAndProcessSource` enqueues the 7-stage pipeline) and reads from it (every `searchFacts`, `getConcept`, `getRelatedConcepts` reads what the pipeline produced). The two processes together form the engine: sources in, annotated reports out, with a growing knowledge graph as the durable artifact in between.

```
   Agentic Flow (loop)
   +---------------------------------------------------+
   |  research -> query -> (thin?) -> research -> ...  |
   |       |         |                                  |
   |       v         v                                  |
   |  fetchAndProcessSource   searchFacts/getConcept    |
   |       |                  read syntheses             |
   +-------|---------|----------------------------------+
           v         ^
   Knowledge Flow (per source)
   +-----------------+
   |  source -> facts -> embed -> dedup                |
   |             -> concepts -> summaries -> synthesis  |
   +-----------------+
```

The Knowledge Flow runs unattended once a source is fetched. The Agentic Flow is where judgement lives — which sources to fetch, when the graph is enough, what to write in the report. The MCP tools are the agent's verbs across both processes.

## Concepts

The Concepts pages define the core artifacts OKT produces and stores: [facts](/docs/reference/concepts/facts), [concepts and contexts](/docs/reference/concepts/concepts-and-contexts), [summaries and synthesis](/docs/reference/concepts/summaries-and-synthesis), and [reports and auto-annotation](/docs/reference/concepts/reports-and-autoannotation). See [Concepts](/docs/reference/concepts/overview) for the overview.

## Examples

The [Examples](/docs/reference/examples/overview) subsection ships static, self-contained snapshots of real OKT meta-synthesis reports. Every sentence that has supporting facts is clickable — click it to see the underlying facts and their source URLs, the same interaction the in-app report view provides. See [Example Meta-Syntheses](/docs/reference/examples/overview).