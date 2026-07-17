---
id: facts
sidebar_position: 1
title: Facts
---

# Facts

A **fact** is an atomic, self-contained unit of knowledge extracted from a source. Facts are the substrate of the entire system — concepts, summaries, synthesis, and reports all derive from them.

## What makes a fact

Each fact is a single sentence-scale statement that can be read and understood **without its surrounding context**. During [fact decomposition](/docs/reference/knowledge-flow/2-fact-decomposition), the extraction LLM is instructed to:

- **Resolve all references** — replace every pronoun and demonstrative with the explicit entity. Never write "He proposed the theory"; write "Albert Einstein proposed the theory of general relativity."
- **Name the subject** — the fact carries its own subject, not one implied by a prior sentence.
- **Include the topic** — the fact is self-contained about what it is.
- **Reject incomplete fragments** — unresolvable facts are discarded, not stored ambiguously.

There is no separate coreference-resolution module. Coreference is handled inline by the extraction prompt, which is what makes OKT facts self-contained.

## Fact kinds

The extraction prompt defines a set of fact types: `claim`, `account`, `measurement`, `formula`, `quote`, `procedure`, `reference`, `code`, `perspective`, `image`. The `fact_kind` field records which type a fact is. Image facts carry an `image_url` and are produced by a separate multimodal extraction pass.

## Provenance

Every fact links back to the sources it came from:

- `fact_sources` — the junction table linking a fact to one or more sources. A fact has no `source_id` column; all source links live in this junction, so a fact can be supported by multiple sources after [deduplication](/docs/reference/knowledge-flow/4-deduplication) merges them.
- `fact_references` — sentence-level provenance: one row per `(fact_id, source_id, sentence_index)`, recording exactly which sentences the fact was derived from.

This means every fact can be traced back to the exact sentences in the exact sources that produced it.

## Lifecycle

A fact moves through three statuses:

| Status | Meaning |
|--------|---------|
| `new` | Just extracted, not yet embedded or deduplicated |
| `stable` | Embedded and deduplicated; eligible for concept extraction, summaries, synthesis |
| `to_delete` | Marked for deletion (e.g. superseded by a duplicate merge) |

## After facts

Once facts are `stable`, downstream stages run:

- [Concept & alias extraction](/docs/reference/knowledge-flow/5-concept-alias-extraction) links each fact to one or more concepts, building the graph.
- [Summaries](/docs/reference/knowledge-flow/6-summaries) fold facts into per-concept slices.
- [Synthesis](/docs/reference/knowledge-flow/7-synthesis) folds slices into one authoritative definition per concept group.
- [Reports](/docs/reference/concepts/reports-and-autoannotation) are annotated with similar facts by embedding similarity.

See the [Fact Decomposition](/docs/reference/knowledge-flow/2-fact-decomposition) and [Deduplication](/docs/reference/knowledge-flow/4-deduplication) process pages for the full mechanics.