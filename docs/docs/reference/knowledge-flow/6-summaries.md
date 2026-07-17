---
id: 6-summaries
sidebar_position: 6
title: Stage 6 — Summaries
---

# Stage 6: Summaries

The sixth stage is OKT's "system 2" thinking — a slow accumulation artifact that crystallizes knowledge per concept, accessible on demand. Summaries incrementally fold new facts into per-concept slices.

Entry point: `(*SummarizeConceptsWorker).Work` — `backend/internal/taskmanager/tasks/summarize_concepts.go:118`.

Per concept (`summarizeOneConcept`, `summarize_concepts.go:185`):

1. **Claim a per-concept lock**: `ClaimConceptForSummary` sets `concepts.summarizing_at`; stale after 2 hours. This prevents two workers from summarizing the same concept concurrently.
2. **Load uncovered facts**: `ListUncoveredFactIDsForSummary` — facts not in any existing summary's `covered_fact_ids`.
3. **Reconstruct the open slice's covered set** + the new facts.
4. **Incremental slicing**: produce complete slices of `BatchSize` (default 20) facts (`is_complete=TRUE`, frozen) plus one open remainder (`is_complete=FALSE`, keeps accumulating). The open slice is the one that grows as new facts arrive; complete slices are frozen and never touched again.
5. **One LLM call per slice** (`summarizer.Summarize`) outside any tx.
6. **Upsert slice**: `CreateSummary` or `UpdateSummary`.
7. **Chain out**: enqueues `synthesize_concept` (`summarize_concepts.go:395`) **only when at least one complete slice was written** this pass. Open-slice-only updates wait for more facts.

## The summarization prompt

`buildSystemPrompt` — `backend/internal/providers/summarization/prompt.go:33-101`.

Key instructions:
- "Reason ONLY from the provided facts. Do NOT use your training data."
- Credulous: "Reflect what the facts say, not to judge them."
- "PRIORITIZE VARIETY, NOT EXHAUSTION" — merge same-point facts, preserve distinct perspectives.
- Inverted pyramid: core first, nuances last (survives truncation).
- Strict word budget derived from `MaxTokens`.
- Citation syntax: `[text](<fact:fact_id>)` — the `fact:` prefix discriminates fact citations from concept citations.
- Radical source neutrality: no source gets prestige-based credibility.

`buildUserMessage` (line 111) renders each fact as `N. [<fact:fact_id>] <text> (<attribution>)`.

## The incremental design

The slicing design is what makes summaries a **slow accumulation** artifact. As new facts arrive for a concept:
- The open slice (`is_complete=FALSE`) accumulates them until it reaches `BatchSize`, then it freezes into a complete slice and a new open slice starts.
- Complete slices are immutable — they represent a crystallized batch of knowledge.
- A future reader can see the concept's knowledge grow over time by reading slices in order.

## Key tables

| Table | Purpose |
|-------|---------|
| `okt_repository.concept_summaries` | Per-concept summary slices: sequence_num, is_complete, content, covered_fact_ids, model |

The unique partial index `uq_concept_summaries_concept_open` on `(concept_id) WHERE is_complete = FALSE` ensures at most one open (accumulating) slice per concept.
