---
id: summaries-and-synthesis
sidebar_position: 3
title: Summaries and Synthesis
---

# Summaries and Synthesis

Summaries and synthesis are OKT's **slow-accumulation layer** — the "system 2" thinking that crystallizes knowledge per concept over time. They are distinct artifacts with distinct roles.

## Summaries (per-concept, incremental)

A **summary** is an incremental slice that folds new facts into a per-concept record. The [summaries stage](/docs/reference/knowledge-flow/6-summaries) runs as facts become linked to a concept.

### Slicing design

As new facts arrive for a concept, the summarizer produces slices:

- **Complete slices** (`is_complete=TRUE`) — frozen, immutable, each covering a batch of facts (default 20). Once written they are never touched again.
- **One open slice** (`is_complete=FALSE`) — the accumulating slice that grows as new facts arrive. When it reaches the batch size it freezes into a complete slice and a new open slice starts.

The unique partial index `uq_concept_summaries_concept_open` on `(concept_id) WHERE is_complete = FALSE` ensures at most one open slice per concept at a time.

### The summarization prompt

The summarizer is instructed to:

- **Reason ONLY from the provided facts** — no training-data knowledge.
- Be **credulous** — reflect what the facts say, not judge them.
- **Prioritize variety, not exhaustion** — merge same-point facts, preserve distinct perspectives.
- Use an **inverted pyramid** — core first, nuances last, so truncation loses the least.
- Cite facts inline with `[text](<fact:fact_id>)`.

### Why "slow accumulation"

A future reader can read the slices in order and see a concept's knowledge grow over time. Each slice is a crystallized batch; the open slice is the leading edge. This is why summaries are a *slow* artifact — they accumulate as sources arrive, not in one shot.

## Synthesis (per concept group, authoritative)

A **synthesis** is the final fold: it takes **all** the summary slices across a concept group (all contexts sharing the canonical name) and weaves them into one authoritative definition. See the [synthesis stage](/docs/reference/knowledge-flow/7-synthesis).

### What goes in

- All summary slices for the canonical-name group.
- **Related concepts** loaded from the `concept_relations` materialized view — the strongest few carry their own synthesis text. This is the **Graph-Aware Reasoning** context: the model can name bridging concepts, identify interpretive battlegrounds, and flag suppressed topics.
- **Image candidates** from the group's image facts, optionally filtered by an image-picker LLM call when there are too many.

### The synthesis prompt

The synthesis prompt is the most elaborate in the system. It frames the model as a "Synthesis Agent" producing *the single authoritative definition* for the concept. Eight core principles, including:

1. Attribution-grounded tone — every claim traces to a fact.
2. Radical source neutrality — no source gets prestige-based credibility.
3. Reason through the evidence — synthesize, don't summarize.
4. Preserve all perspectives, including minority ones.
5. Stakeholder motivation analysis — who benefits from each claim being true.
6. Detect institutional deception patterns.
7. Ground everything in the summaries — no outside knowledge.
8. Honest assessment — say what the evidence supports and what it doesn't.

Two distinctive mechanisms:

- **Parallel Scenarios** — for contested claims, build both "the claim is genuine" and "the claim is an artifact" at full strength, then compare. Only collapse to a single view if the evidence is conclusive.
- **Anti-Asymmetry Rules** — don't frame mainstream claims as "evidence" and dissenting claims as "claim"; both sides get the same scrutiny.

### When synthesis runs

Synthesis is enqueued **only when at least one complete summary slice was written** in a summarization pass. Open-slice-only updates wait for more facts. This means synthesis reflects crystallized batches of knowledge, not the still-growing open slice.

### One per group

The unique constraint `(repository_id, lower(canonical_name))` on `concept_syntheses` ensures one synthesis per concept group. A concept like "Einstein" appearing in contexts `Scientist` and `Person` gets one synthesis covering both.

## Summaries vs synthesis at a glance

| | Summaries | Synthesis |
|---|-----------|-----------|
| Scope | Per concept row | Per concept group (across contexts) |
| Input | New facts for one concept | All summary slices for the group + related concepts + images |
| Mutability | Complete slices frozen; one open slice grows | Re-written when new complete slices arrive |
| Role | Incremental accumulation | Authoritative definition |
| When it runs | As facts link to a concept | When at least one complete slice was written |

## Key tables

| Table | Purpose |
|-------|---------|
| `okt_repository.concept_summaries` | Per-concept summary slices: sequence_num, is_complete, content, covered_fact_ids, model |
| `okt_repository.concept_syntheses` | One synthesis per canonical-name group: content, covered_summary_ids, covered_concept_ids, embedded_image_ids |