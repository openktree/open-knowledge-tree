---
id: expected-cost
sidebar_position: 5
title: Expected Cost
---

# Expected Cost

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

That run started from an empty repository and ran the full Agentic Flow: `research` → fetch → Knowledge Flow pipeline → `query` → `synthesizer` per scope → `super-synthesizer`. The $50 covers every chat and embedding call made by the OKT backend during that loop — nothing the driving agent itself spent.

## Estimated cost per scale

Linear extrapolation from the baseline (~$50 / 1,300 sources, ~$50 / 200k facts). Real runs vary with source length, fact density, and how many syntheses you produce; treat these as order-of-magnitude planning numbers, not quotes.

| Scale | Sources | Facts (approx.) | Estimated AI spend |
|---|---|---|---|
| **Small** — a focused topic | 250 | ~38k | ~$10 |
| **Baseline** — a multi-scope synthesis | 1,300 | ~200k | ~$50 |
| **Medium** — several related syntheses | 2,600 | ~400k | ~$100 |
| **Large** — a broad domain survey | 6,500 | ~1M | ~$250 |
| **XLarge** — exhaustive coverage | 13,000 | ~2M | ~$500 |

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

- **Pick a cheaper chat model.** The default `google/gemma-4-31b-it` is already a mid-tier price point. Swapping to a smaller or open model (e.g. a self-hosted Ollama model) drops the chat bill dramatically, at the cost of fact and synthesis quality. See `providers.ai` in `configs/config.default.yaml`.
- **Raise the similarity threshold** so the autocite posture classifier runs on fewer (sentence, fact) pairs — see `repository_settings.similarity_threshold`.
- **Disable the posture classifier** (`posture_classifier_enabled: false`) when you don't need supports/contradicts/related labels on your report citations. That removes one LLM call per annotation pair.
- **Reuse the graph.** The single biggest lever is not re-fetching. Once a repository has a fact base, new reports and new syntheses against it cost a fraction of building it from scratch — embeddings already exist, deduplication suppresses repeats, and syntheses read existing facts rather than re-extracting them.
- **Bring your own embeddings.** A self-hosted embedding model (Ollama, `qwen3-embedding`, etc.) makes the embedding line item effectively free at the cost of GPU/CPU on your machine.

## Cost tracking

OKT records every AI call in the `okt_system.ai_usage` table, attributed to the task and repository that triggered it. After a run, query that table to see exactly what was spent — model, token count, cost — rather than relying on the estimates here.