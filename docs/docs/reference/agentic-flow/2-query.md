---
id: 2-query
sidebar_position: 2
title: Phase 2 — Query
---

# Phase 2: Query (read the graph)

The query phase reads what the system knows. Once sources are ingested, an agent searches facts and concepts, reads syntheses, and walks related concepts to understand the topic — and to decide whether the graph is thick enough to author a report, or thin enough to loop back to [research](/docs/reference/agentic-flow/1-research).

## What a query agent does

A query agent is responsible for:

1. **Searching the corpus** — full-text search over facts to see what the sources actually say.
2. **Drilling into facts** — reading a fact's source URLs and linked concepts to check provenance.
3. **Navigating the concept graph** — listing concepts, reading the authoritative synthesis for each, and walking to related concepts.
4. **Reading summaries** — the incremental per-context slices that show how knowledge accumulated.
5. **Assessing coverage** — deciding whether the graph covers the topic or is thin (few facts, no synthesis, missing concepts).

This phase is pure read — it writes nothing. The judgement here is *what to read next* and *is this enough*.

## Tools

| Tool | Role in this phase |
|------|--------------------|
| `searchFacts` | Full-text search over atomic facts; filter by concept or context. |
| `getFact` | Drill into a fact's source URLs and linked concepts. |
| `searchConcepts` | List concept groups, optionally filtered by canonical name. |
| `getConcept` | Read a concept's full group plus the authoritative synthesis. |
| `getConceptSummaries` | Read the incremental summary slices per context. |
| `getRelatedConcepts` | Walk the graph to concepts sharing facts. |

See the [MCP Tools Reference](/docs/mcp/tools) for full parameter and return shapes.

## Building a query agent

A query agent only needs read tools. Its prompt should emphasize breadth-first exploration followed by depth on the most relevant concepts.

Key behaviours to encode in the prompt:

- **Start with `searchFacts`.** A broad fact search tells you what the corpus actually contains. Use `searchConcepts` to find the concepts the facts link to.
- **Read the synthesis first.** `getConcept` returns the authoritative synthesis for a concept group — read it before the raw facts, because it's the crystallized state of knowledge. If you need the incremental build-up, `getConceptSummaries` gives the per-context slices in order.
- **Walk the graph.** `getRelatedConcepts` returns concepts ranked by shared fact count. Follow the strongest relations to find bridging topics the research phase may have missed.
- **Check provenance.** `getFact` returns the source URLs behind a fact. Use it to verify that a claim you want to cite in a report rests on real sources, not a single thin one.
- **Decide: enough or thin?** If a concept has no synthesis, few facts, or the related concepts are unexplored, loop back to the research phase with a sharper query. If the graph covers the topic, hand off to the [Reports](/docs/reference/agentic-flow/3-reports) phase.

## The thin-area loop

The query phase is where the agentic loop's judgement lives. A query agent that always hands off to reports without checking coverage produces thin reports; one that loops back to research whenever a concept is thin produces grounded ones. Encode the threshold in the prompt: e.g. "if a concept relevant to the topic has no synthesis or fewer than N facts, request more sources."

But the query phase does more than just say "thin — go fetch more." It is what tells the research phase *what* to fetch and *why*. A good query agent hands back a concrete research brief, not a vague "need more sources." The brief is derived from reading the graph:

- **Missing concepts** — topics the report needs but no concept row exists for yet. The research agent should search for sources likely to mention them.
- **Thin concepts** — concepts with few facts or no synthesis. The research agent should fetch more sources about that specific subject, not just the topic at large.
- **Unexplored related concepts** — `getRelatedConcepts` surfaces bridging topics that share facts with the ones you have. If a bridging concept is itself thin, that is a high-value fetch target, because filling it also enriches the concepts it bridges to.
- **Provenance gaps** — `getFact` shows the source URLs behind a fact. If a key claim rests on a single source, the research agent should fetch corroborating sources so the fact can be deduplicated into a multi-source one.
- **Context imbalance** — a concept group may be rich in one context and empty in another (e.g. "Einstein" has many `Scientist` facts but no `Person` facts). The research agent can target sources that fill the missing context.

The hand-back to research is therefore a *directed* request: "fetch sources about X because the graph has N facts there and no synthesis," or "fetch sources mentioning Y so the Z concept gains a related-concept link." The query phase reads the graph's shape and tells research where the holes are and what kind of source would fill them.

## Handing off

When the graph covers the topic — syntheses exist for the key concepts, facts are well-provenanced, and the related concepts have been explored — the query phase hands off to the [Reports](/docs/reference/agentic-flow/3-reports) phase. When it doesn't, it hands back to the [Research](/docs/reference/agentic-flow/1-research) phase with a directed research brief.

## See also

- [Concepts and Contexts](/docs/reference/concepts/concepts-and-contexts) — how the concept graph is structured.
- [Summaries and Synthesis](/docs/reference/concepts/summaries-and-synthesis) — the artifacts a query agent reads to assess coverage.
- [MCP Tools Reference](/docs/mcp/tools) — full parameter and return shapes for every tool.