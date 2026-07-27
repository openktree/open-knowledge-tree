---
id: overview
sidebar_position: 0
title: Example Meta-Syntheses
---

# Example Meta-Syntheses

These pages are **static, self-contained examples** of OKT meta-synthesis reports. They show what a finished cross-scope synthesis looks like — and, more importantly, they let you **click any citation to see the underlying fact and its sources**.

Each example was produced by the OKT agentic flow: a `research` agent partitioned a broad topic into scopes, one `synthesizer` ran per scope, and a `super-synthesizer` wove the per-scope documents into a single meta-synthesis. That document was then stored as a **report** and auto-annotated against the repository's facts by embedding similarity.

On these pages the annotations are **frozen**: the fact text and source URLs are embedded directly in the page — there is no API call and no live repository connection. Every `[N]` superscript in the text is a citation; click it to open a popover showing the supporting fact and where it came from.

## Two kinds of citation

Recent examples carry **two distinct citation kinds**, rendered differently so the reader can tell at a glance which is which:

- **Direct cites — `[D{N}]`, amber.** Facts the researcher (or AI researcher) explicitly chose from a source while writing the synthesis. These are the primary evidentiary backbone — the citations the author reached for. Their popovers are labelled **direct cite**.
- **Auto-matched annotations — `[N]`, blue.** Facts the system retrieved automatically from the knowledge base by embedding similarity *after* the synthesis was written, to give the reader an additional audit trail against the broad corpus. Their popovers are labelled **auto-matched**.

Older examples (agroforestry, human alimentation, healing, males, females) carry only auto-matched annotations. The paper and the MKUltra synthesis carry both kinds.

Both kinds may also carry a **posture** (supports / contradicts / related), shown as a colored chip beside the number — green for supports, red for contradicts, blue for related.

## Why these examples?

- **Attribution is visible.** You can trace every claim back to a specific fact and its source URLs, which is the core promise of an OKT report.
- **The examples are real.** They come from actual investigations run against the default repository (254 sources for the healing synthesis; ~150 for the agroforestry synthesis).
- **They are static.** You can read and analyze them without booting the stack or authenticating.

## Examples

1. [Atomic Facts and Emergent Concept Graphs](./paper) — **the OKT paper, written by OKT.** A self-referential deep dive on OKT's own mechanisms: atomic fact decomposition, the emergent concept graph, and the fact-co-occurrence relations that fall out of it. Carries both direct cites (`[D{N}]`, amber) and auto-matched annotations (`[N]`, blue). **Authored by GLM 5.2.**
2. [The Modular Tropical Agroforestry Recipe Book](./agroforestry) — a 4-scope meta-synthesis (~1,300 sources, 100,000+ facts) integrating tropical polyculture architecture, belowground mechanisms, mycorrhizal/microbial symbiosis, and mushroom & pest ecology into a modular recipe book. Citations carry a posture (supports / contradicts / related). **Authored by GLM 5.2.**
3. [Human Alimentation: A Multidimensional Feeding Meta-Synthesis](./humanalimentation) — a 9-scope meta-synthesis integrating contemporary and global foodways, ancient and historical diets, protein sources, nutrient matrices, disease and lifespan, mood and cognition, life-stage physiology, lived and community evidence, and methods, governance, conflicts, and incentives. Citations carry a posture (supports / contradicts / related). **Authored by GPT 5.6 Sol.**
4. [Miraculous Healing &amp; Spontaneous Disease](./healing) — a 9-scope meta-synthesis (254 sources) covering spontaneous remission, placebo/nocebo neuroscience, faith and prayer, energy healing, plant medicine, guru transmission, contemplative practice, vibrational therapy, and stress pathology. **Authored by GLM 5.2.**
5. [A Meta-Synthesis for Men](./males) — a 10-scope meta-synthesis (~450 sources) on male-female coexistence written from a male perspective, covering hormonal cycles, competition, emotions, sexuality, complaints, and what successful couples do. Citations carry a posture (supports / contradicts / related). **Authored by GLM 5.2.**
6. [Understanding Yourself, Understanding Him](./females) — a 10-scope meta-synthesis (~450 sources) on male-female coexistence written from a female perspective, covering your hormones, competition strategies, emotions, sexuality, his hormones and competition, and actionable steps. Citations carry a posture (supports / contradicts / related). **Authored by GLM 5.2.**
7. [MKUltra — Declassified Record, Survivor Claims, and the Continuation Debate](./mkultra) — a multi-tier synthesis organized around three deliberately distinguished evidence tiers: Tier 1 declassified documentary fact, Tier 2 attributed survivor testimony, and Tier 3 the contested continuation debate (MONARCH, FMSF-cover, O'Neill/Manson). Carries both direct cites (`[D{N}]`, amber) and auto-matched annotations (`[N]`, blue). **Authored by GLM 5.2.**
8. [Russia's Shadow Fleet: A Cross-Scope Meta-Synthesis](./shadowfleet) — a 5-scope meta-synthesis (280 sources) integrating ownership & flag obfuscation, AIS spoofing & dark-vessel techniques, implicated flag states & insurers, fleet-size & volume estimate conflicts, and enforcement responses & gaps around Russia's sanctions-evading tanker fleet. Citations carry a posture (supports / contradicts / related). **Authored by GLM 5.2.**

## How to produce your own

Run the same flow against any OKT repository: `research` (plan + gather) → one `synthesizer` per scope → `super-synthesizer` to combine. Store the result via `createReport` and OKT auto-annotates every sentence with its matching facts. See [Phase 3 — Reports](/docs/reference/agentic-flow/3-reports) and [Reports and Auto-Annotation](/docs/reference/concepts/reports-and-autoannotation).