---
id: 7-synthesis
sidebar_position: 7
title: Stage 7 — Synthesis
---

# Stage 7: Synthesis

The seventh stage is the final fold: it takes all the per-concept summary slices (from stage 6) and weaves them into one authoritative definition per concept group. Synthesis is the slowest, most expensive LLM call in the pipeline — it runs once per concept group and is what a reader consults when they want the crystallized state of knowledge for a concept, not just mentions of it.

Entry point: `(*SynthesizeConceptsWorker).Work` — `backend/internal/taskmanager/tasks/synthesize_concept.go:93`; per-group `synthesizeOneGroup` (`synthesize_concept.go:153`).

1. **Resolve the group**: `concept_id` -> `canonical_name` group key.
2. **Acquire a group-keyed advisory lock**: `pg_try_advisory_lock(hashtext(repo_id || ':' || lower(canonical_name)))` (`TryAdvisoryLockForSynthesis`). If the lock is held, skip — another worker is synthesizing this group.
3. **Load ALL summary slices** across the canonical-name group (`ListSummariesByCanonicalNameGroup`) + group concept_ids.
4. **No-delta skip**: if the existing synthesis's `covered_summary_ids` already contains all slice IDs, skip.
5. **Load related concepts** (`loadRelatedConcepts`) from the `concept_relations` materialized view — top N by `shared_fact_count`, with the strongest few carrying their own synthesis text. This is the **Graph-Aware Reasoning** context.
6. **Image candidates**: load the group's image facts (`ListGroupImageFacts`, capped at `MaxImageCandidates`).
   - If the count is at most `MaxImages`: pass all directly.
   - If the count exceeds `MaxImages`: a separate **image-picker LLM call** (`PickImages`) returns chosen fact_ids.
7. **Run the synthesis LLM call** with all slices + related concepts + chosen images.
8. **Upsert** the single `concept_syntheses` row for the group (`covered_summary_ids`, `covered_concept_ids`, `embedded_image_ids` extracted from markdown).

## The synthesis prompt

`synthesisSystemPrompt` — `backend/internal/providers/synthesis/prompt.go:26-227`. This is the most elaborate prompt in the system.

The prompt frames the model as a "Synthesis Agent" producing "the single authoritative definition" for the concept. Eight core principles:

1. **Attribution-Grounded Tone** — every claim traces to a fact.
2. **Radical Source Neutrality** — no source gets prestige-based credibility.
3. **Reason Through the Evidence** — synthesize, don't summarize.
4. **Preserve All Perspectives** — including minority ones.
5. **Stakeholder Motivation Analysis** — who benefits from each claim being true.
6. **Detect Institutional Deception Patterns** — look for coordinated framing.
7. **Ground Everything in the Summaries** — don't introduce outside knowledge.
8. **Honest Assessment** — say what the evidence supports and what it doesn't.

Two distinctive mechanisms:

- **Parallel Scenarios** (mandatory): for contested claims, build BOTH "the claim is genuine" and "the claim is an artifact" at full strength, then compare. Only collapse to a single view if the evidence is conclusive. This prevents the model from committing to one interpretation prematurely.
- **Anti-Asymmetry Rules**: don't frame mainstream claims as "evidence" and dissenting claims as "claim"; don't apply falsifiability one-directionally. Both sides get the same scrutiny.

- **Graph-Aware Reasoning**: the related-concepts block lets the model name bridging concepts, identify interpretive battlegrounds, and flag suppressed topics.

Citations:
- Facts: `[text](<fact:fact_id>)`
- Related concepts: `[name](<concept:concept_id>)`
- Images: `![alt](<fact:fact_id>)`

Response structure: Opening -> Scope & Boundaries -> Key Themes -> Tensions & Debates -> Significance.

## The image-picker prompt

`imagePickerSystemPrompt` — `prompt.go:237-245`. Narrow: returns only chosen UUIDs, one per line.

`buildSynthesisUserMessage` (line 267) renders slices as `N. [concept_id=<concept:id>, seq=<n>, facts=<count>]` + the related-concepts block + candidate images.

## Key tables

| Table | Purpose |
|-------|---------|
| `okt_repository.concept_syntheses` | One synthesis per canonical-name group: content, covered_summary_ids, covered_concept_ids, embedded_image_ids |

The unique constraint `(repository_id, lower(canonical_name))` on `concept_syntheses` ensures one synthesis per concept group.
