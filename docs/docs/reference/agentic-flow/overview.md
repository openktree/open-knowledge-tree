---
id: overview
sidebar_position: 0
title: Agentic Flow Overview
---

# Agentic Flow

The [Knowledge Flow](/docs/reference/knowledge-flow/overview) turns one source into a graph of facts, concepts, and syntheses. The **Agentic Flow** is what an agent does *on top of that graph* — it is not a fixed sequence. An agent researches, queries, and authors reports in whatever order the task demands, looping back to fetch more sources whenever the graph is thin.

## Three phases

1. **Research** — gather sources around a topic and feed them into the repository. See [Research](/docs/reference/agentic-flow/1-research).
2. **Query** — read what the system knows: search facts and concepts, read syntheses, walk related concepts. See [Query](/docs/reference/agentic-flow/2-query).
3. **Reports** — author a report; OKT annotates every sentence with the facts it rests on. See [Reports](/docs/reference/agentic-flow/3-reports).

Each phase is a set of [MCP tools](/docs/mcp/tools) — the agent's verbs. The order is up to the agent.

## How the phases compose

The three phases are not a pipeline. An agent typically:

1. Starts with **research** — searches for sources, opens an investigation, fetches them.
2. Polls `getSourceTasks` until the pipeline drains.
3. **Queries** the graph — searches facts and concepts, reads syntheses, walks related concepts.
4. If a query surfaces a thin area (few facts, no synthesis), loops back to **research** with a sharper query.
5. Once the graph covers the topic, **creates a report** and reads it back annotated.

The loop — research → query → (thin?) → research → ... → report — is the core agentic pattern.

## Building agents for OKT

OKT exposes its capabilities as MCP tools, so any agent framework that speaks MCP can drive the agentic flow. The phases map cleanly onto custom agents — one agent per phase, or one orchestrator that does all three. The [phase pages](/docs/reference/agentic-flow/1-research) describe the *responsibilities* of each phase so you can build agents against them in whatever framework you use.

A common pattern is to build three specialized agents — a **researcher**, a **querier**, and a **reporter** — each with a tight prompt scoped to its phase's tools, plus a primary agent that orchestrates them. Keeping each agent narrow (one phase, one toolset, one prompt) makes the loop easier to debug and the prompts easier to tune.