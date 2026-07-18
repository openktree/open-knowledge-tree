---
id: intro
sidebar_position: 0
title: Introduction
---

# Open Knowledge Tree

Open Knowledge Tree (OKT) turns raw sources — web pages, PDFs, DOIs — into a queryable knowledge graph. Given a URL, it fetches the content, decomposes it into atomic, self-contained facts, groups those facts into concepts, and slowly accumulates syntheses that crystallize what is known about each concept. The result is a graph of facts, concepts, and syntheses that an agent or a human can search, cite, and reason over.

## Why it exists

Most knowledge tools store documents. OKT stores **ideas**. A document is a bag of sentences riddled with pronouns, context, and repetition. OKT resolves the coreference (replacing "he" with the actual entity), deduplicates the ideas (two sources saying the same thing become one fact), and links the facts into a concept graph where the same idea appears under different names ("Apple" the company vs "Apple" the molecule) is disambiguated by context. On top of the graph, syntheses accumulate the state of knowledge per concept, accessible on demand.

## The pipeline

A source flows through seven stages, each a background job that enqueues the next:

1. **Source extraction** — fetch the URL/DOI through a chain of providers (HTTP, Unpaywall, TLS impersonation, headless browser) and parse the body into clean text with sentence offsets.
2. **Fact decomposition** — chunk the text, run an LLM extraction that resolves coreference and emits self-contained atomic facts, each citing the sentences it came from.
3. **Embedding** — vectorize every fact into Qdrant.
4. **Deduplication** — find idea-level duplicates by embedding cosine similarity; the survivor inherits all sources and references.
5. **Concept & alias extraction** — run an LLM over each stable fact to extract a concept, a context (from an ontology), and seed aliases; match against existing concepts via aliases scoped by context, or create a new one. This builds the knowledge graph.
6. **Summaries** — incrementally summarize the facts under each concept into frozen slices plus one open accumulating slice. This is the "system 2" slow accumulation layer.
7. **Synthesis** — fold all summary slices for a concept group (plus related concepts and images) into a single authoritative definition. This is the on-demand, future-queryable crystallized knowledge.

See [Knowledge Flow](/docs/reference/knowledge-flow/overview) for the detailed walkthrough.

## How to use it

- **Quick start**: run with Docker in two commands — no git needed. See [Getting Started](/docs/getting-started).
- **Agent (MCP)**: the 18 MCP tools let an agent fetch sources, search facts and concepts, track ingestion, and create annotated reports. See [MCP Tools](/docs/mcp/overview).
- **HTTP API**: the REST API exposes the same operations plus auth, repositories, and provider management. See [REST API](/docs/api/overview).
- **Frontend**: a SolidJS SPA for browsing facts, concepts, investigations, and reports.
- **Local dev**: `just dev` boots the full stack via Docker Compose. See [Local Dev](/docs/local-dev/overview).

## Tech stack

| Layer | Tech |
|-------|------|
| Backend | Go 1.22+, Chi, pgx/v5, sqlc, Casbin, Viper, River |
| Frontend | SolidJS, Vite, Tailwind CSS |
| Data | PostgreSQL 16, Qdrant |
| Task queue | River (Postgres-backed) |
| Dev | Docker Compose, `just` |