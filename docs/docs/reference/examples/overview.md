---
id: overview
sidebar_position: 0
title: Example Meta-Syntheses
---

# Example Meta-Syntheses

These pages are **static, self-contained examples** of OKT meta-synthesis reports. They show what a finished cross-scope synthesis looks like — and, more importantly, they let you **click any citation to see the underlying fact and its sources**.

Each example was produced by the OKT agentic flow: a `research` agent partitioned a broad topic into scopes, one `synthesizer` ran per scope, and a `super-synthesizer` wove the per-scope documents into a single meta-synthesis. That document was then stored as a **report** and auto-annotated against the repository's facts by embedding similarity.

On these pages the annotations are **frozen**: the fact text and source URLs are embedded directly in the page — there is no API call and no live repository connection. Every `[N]` superscript in the text is a citation; click it to open a popover showing the supporting fact and where it came from.

## Why these examples?

- **Attribution is visible.** You can trace every claim back to a specific fact and its source URLs, which is the core promise of an OKT report.
- **The examples are real.** They come from actual investigations run against the default repository (254 sources for the healing synthesis; ~150 for the agroforestry synthesis).
- **They are static.** You can read and analyze them without booting the stack or authenticating.

## Examples

1. [Miraculous Healing &amp; Spontaneous Disease](./healing) — a 9-scope meta-synthesis (254 sources) covering spontaneous remission, placebo/nocebo neuroscience, faith and prayer, energy healing, plant medicine, guru transmission, contemplative practice, vibrational therapy, and stress pathology.
2. [A Highland Tropical Food Forest for Cartago, Costa Rica](./agroforestry) — a 5-scope meta-synthesis (~150 sources) on climate/soils, avocado, coffee, polyculture design, and regenerative practices for a food forest at ~1800 m on Andisol.

## How to produce your own

Run the same flow against any OKT repository: `research` (plan + gather) → one `synthesizer` per scope → `super-synthesizer` to combine. Store the result via `createReport` and OKT auto-annotates every sentence with its matching facts. See [Phase 3 — Reports](/docs/reference/agentic-flow/3-reports) and [Reports and Auto-Annotation](/docs/reference/concepts/reports-and-autoannotation).