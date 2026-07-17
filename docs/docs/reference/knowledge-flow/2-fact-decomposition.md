---
id: 2-fact-decomposition
sidebar_position: 2
title: Stage 2 — Fact Decomposition
---

# Stage 2: Fact Decomposition

The second stage takes the parsed source text, chunks it, and runs an LLM extraction that resolves coreference and emits self-contained atomic facts. Each fact cites the global sentence indices it came from, preserving provenance back to the source.

Entry point: `(*SourceDecompositionWorker).Work` — `backend/internal/taskmanager/tasks/source_decomposition.go:115`.

## Chunking

The worker picks the text: prefer `parsed_markdown` over `parsed_text` (`source_decomposition.go:216`). It chunks the text with `w.chunkingProvider.Chunk` — the default is `decomposition.SimpleChunkingProvider` (`backend/internal/providers/decomposition/chunking.go:63`), a sliding rune-window of 2000 runes with overlap.

Before chunking, the worker reconstructs the global sentence array from `source.sentence_offsets` (`sentencesFromOffsets`, `source_decomposition.go:658`). Each chunk is rendered with `[Sn]` sentence markers via `buildLabeledChunkText` so the LLM can cite sentence indices in its output.

## Phase 1: parallel AI extraction

`decomposition.ExtractParallel` fans `ExtractFacts` over chunks with bounded concurrency (default 4). Each chunk is sent to the LLM with the fact-extraction prompt.

### The fact-extraction prompt

`factExtractionPrompt` — `backend/internal/providers/decomposition/fact_extraction.go:13-160`.

The prompt defines fact types: `claim`, `account`, `measurement`, `formula`, `quote`, `procedure`, `reference`, `code`, `perspective`, each with examples.

#### Self-containedness rules (coreference resolution)

Lines 56-72 of the prompt are the core of OKT's coreference resolution:

- "Resolve all references. Replace every pronoun, demonstrative with the explicit entity."
- "Name the subject."
- "Include the topic."
- "Reject incomplete fragments."
- "Discard unresolvable facts."
- "Never write 'He proposed the theory' — write 'Albert Einstein proposed the theory of general relativity.'"

There is **no separate coreference module**. Coreference is delegated entirely to the LLM via the prompt. The model is instructed to substitute the actual entity from context; unresolvable facts are skipped, not stored ambiguously. This is what makes OKT facts self-contained — a fact can be read and understood without its surrounding context.

#### Output format

The LLM returns a JSON array: `[{"text":"...","sentences":[0,1]}]` where `sentences` are the global `[Sn]` indices the fact was derived from.

### Image fact extraction

When an image extractor is configured and enabled, `extractImageFacts` (`source_decomposition.go:339`) runs a multimodal extraction per image. The image fact-extraction prompt (`image_fact_extraction.go:27-76`) mirrors the text prompt's self-containedness rules, scoped to images. Image facts are persisted with `fact_kind='image'` and an `image_url`.

## Phase 2: serial persistence

For each chunk's `ExtractedFact{Text, Sentences}` (`source_decomposition.go:260`):

1. `queries.CreateFact` — inserts a fact row with status `new`, `fact_kind='text'`.
2. `queries.AddFactSource` — inserts the fact-source junction row (idempotent via `ON CONFLICT`).
3. For each cited `sentence_index`: `queries.AddFactReference` — one row per (fact, source, sentence). Hallucinated out-of-range indices are silently dropped.

Finally, `queries.MarkSourceProcessed` marks the source as fully decomposed.

## Chain out

When `totalFacts > 0`, the worker enqueues `embed_facts` (`source_decomposition.go:370`), passing the repository ID and source ID.

## Key tables

| Table | Purpose |
|-------|---------|
| `okt_repository.facts` | Atomic facts: text, status (`new`/`stable`/`to_delete`), fact_kind, image_url, embedded_at |
| `okt_repository.fact_sources` | Junction: fact &lt;-&gt; source, with chunk_index |
| `okt_repository.fact_references` | Sentence-level provenance: (fact_id, source_id, sentence_index) |

The `facts` table has no `source_id` column — all source links live in the junction, so a fact can be supported by multiple sources after deduplication merges them.