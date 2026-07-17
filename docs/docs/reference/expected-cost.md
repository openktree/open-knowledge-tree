---
id: expected-cost
sidebar_position: 5
title: Expected Cost
---

# Expected Cost

:::caution Reference only

The numbers on this page are **planning estimates, not quotes**. Actual spend varies widely with your model configuration (chat model choice, embedding model choice, posture classifier on/off), the length and density of the sources you fetch, how many syntheses you run, deduplication hit rate, and provider pricing changes. Use the table to size a budget; then confirm against the `ai_usage` table after your run (see [Cost tracking](#cost-tracking)).
:::

This page gives you a rough idea of what an OKT research run costs in **AI provider usage** — the tokens spent on chat models (fact decomposition, summarization, synthesis, posture classification) and on the embedding model. It does **not** include the cost of the agent that drives the Agentic Flow (opencode, Claude Code, Cursor, etc.), which is billed separately by whoever hosts that agent.

## What you pay for

OKT calls two kinds of AI model while it works:

| Model role | What it does | Default model |
|---|---|---|
| **Chat** | Decomposes source text into atomic facts; writes per-concept summaries and syntheses; classifies citation posture; picks images. | `google/gemma-4-31b-it` via OpenRouter |
| **Embedding** | Vectorizes every fact and concept for semantic search and deduplication. | `google/gemini-embedding-2` |

Both are billed by the token by your AI provider (OpenRouter by default; Ollama / Ollama Cloud are also supported). The chat model dominates the bill: every fetched source produces tens to hundreds of facts, and each fact is a short LLM call. Embeddings are cheap by comparison but scale with fact count.

The only other charged dependency is **Postgres** (free self-hosted; Fly.io managed Postgres starts around a few dollars/month) and optional **Qdrant** if you run the vector index outside Postgres. Those are infrastructure costs, not AI costs, and are not in the table below.

## Baseline measurement

Our reference run, on the `default` repository with the default models above, produced:

| Metric | Value |
|---|---|
| Total spend | **~$50** |
| Sources gathered | ~1,300 |
| Facts extracted | ~200,000 |
| Investigations | 16 |
| Scopes synthesized | 4–9 per meta-synthesis |

That run started from an empty repository and ran the full Agentic Flow: `research` → fetch → Knowledge Flow pipeline → `query` → `synthesizer` per scope → `super-synthesizer`. The $50 covers every chat and embedding call made by the OKT backend during that loop — nothing the driving agent itself spent. Wall-clock time was roughly **6 hours**, driven by the agent iterating through scopes; tighter user config (fewer scopes, lower dedup retries, fewer investigations) shortens that, and broader config lengthens it.

A note on what "1,300 sources" means: because the agent chooses the queries, those 1,300 sources are the cream of a much larger literature. 1,300 curated sources is a **large** synthesis, not a small one — the scale table below reflects that.

## Estimated cost per scale

Linear extrapolation from the baseline (~$50 / 1,300 sources, ~$50 / 200k facts). Real runs vary with source length, fact density, model pricing, and how many syntheses you produce; treat these as **order-of-magnitude planning numbers, not quotes**. A cheaper chat model or a corpus of short sources can move the real spend several multiples in either direction. Wall-clock time scales roughly with source count and is gated by your scope/investigation config; the baseline run took ~6h.

| Scale | Sources | Facts (approx.) | Estimated AI spend | Rough duration |
|---|---|---|---|---|
| **Small** — a single focused question | 50–150 | ~8k–23k | ~$2–$6 | ~30 min |
| **Medium** — a topic with 2–3 scopes | 300–600 | ~46k–92k | ~$12–$25 | ~1.5–3 h |
| **Large** — a full multi-scope meta-synthesis (baseline) | 1,300 | ~200k | ~$50 | ~6 h |
| **XLarge** — several related meta-syntheses against the same graph | 3,000+ | ~460k+ | ~$120+ | ~12 h+ |

## What is and isn't included

**Included** in the numbers above:
- Chat-model calls for fact decomposition, concept/alias extraction, summaries, syntheses, posture classification.
- Embedding-model calls for every fact and concept.
- Search-provider calls that have a per-query cost (e.g. Serper). These are usually a small fraction of the AI bill.

**Not included**:
- **The driving agent.** opencode, Claude Code, Cursor, or whatever agent runs the Agentic Flow against OKT's MCP tools bills its own tokens separately. That cost depends on the agent's model and how many tool calls it makes, and can rival or exceed the OKT backend cost for a long research session.
- **Hosting.** The Postgres database, the OKT API container, the frontend, and (if used) a managed Qdrant instance. Fly.io or any cloud VM runs these for a few dollars a month at dev scale.
- **Your time.** A multi-scope synthesis is an overnight run, not a coffee break.

## How to reduce cost

- **Reuse the graph.** The single biggest lever is not re-fetching. Once a repository has a fact base, new reports and new syntheses against it cost a fraction of building it from scratch — embeddings already exist, deduplication suppresses repeats, and syntheses read existing facts rather than re-extracting them.
- **Share graphs via the Knowledge Registry.** The registry (see [Registry](/docs/reference/registry)) catalogs OKT repositories across instances and routes push/pull/search between them. If a teammate or another OKT instance has already built a graph for a topic you care about, you can pull from it instead of re-researching. The registry is what makes a one-time research spend reusable across teams and deployments.

More cost-reduction guidance (model selection, threshold tuning, posture classifier toggling) will be added here once we've measured each lever properly.

## Cost tracking

OKT records every AI call in the `okt_system.ai_usage` table, attributed to the task and repository that triggered it. After a run, query that table to see exactly what was spent — model, token count, cost — rather than relying on the estimates here.