# Graph-Weakness Experiments — Plan

Read-only experiments that turn the OKT paper's "designed but not executed" /
"conceded but never measured" graph claims into evidence. **No OKT code, schema,
migration, or task changes.** All experiments read the existing `multihoprag`
repository and its already-ingested corpus.

## Scope decisions

- **Experiments only** — no OKT changes as output of this task.
- **Rater:** LLM-as-judge (OpenRouter, same gemini model OKT uses for extraction)
  + Wikidata as ground truth.
- **KGQA corpus:** MultiHop-RAG questions on the existing OKT corpus
  (retrieval-style KGQA, scope documented in Exp 4).
- **Report:** standalone `REPORT.md` in this dir, mirroring the paper's §6/§7
  structure.
- **No relation-label feature**, no OKT changes.
- **Old repo (`../../../old/open-knowledge-tree/experiments`):** reference
  material only. Learn from patterns and prior numbers (51% recall ceiling,
  strict-grounding prompt F1 95.7%, cross-contamination evidence, dedup
  thresholds) but rewrite all code fresh. No imports from it.

## Access mode

- **Full-graph dumps (Exp 1/2):** read-only async DB access (`asyncpg`) against
  the dev Postgres on :5432. The `multihoprag` repo lives there; the test DB on
  :5433 is empty unless the e2e harness has run. The AGENTS.md prohibition is
  about the e2e harness that DROPs schemas, not about read-only SELECTs. The
  existing `multihop_rag` experiments already read this same dev DB via the API
  on :8080; direct SQL is the same data, just faster than paginated REST for
  full-graph dumps.
- **KGQA probes (Exp 4):** existing REST client
  `scripts/experiments/multihop_rag/okt.py` (`getRelatedConcepts` +
  `searchFacts`) — no changes to it.
- **Wikidata (Exp 3):** REST/SPARQL lookup, external.

## The `multihoprag` graph (measured)

| quantity | value |
|---|---|
| concept groups (lower(name)) | 18,241 |
| facts linked to ≥1 concept | 36,075 |
| bipartite fact↔concept edges | 94,607 |
| co-occurrence relations (pairs) | 76,233 |
| max concept degree k_max | 644 (Google LLC) |
| edges at w=1 | 70.2% |
| max shared_fact_count | 156 |

Top concepts by fact-degree: Google LLC (644), Amazon (604), USA (479),
Samuel Bankman-Fried (453), OpenAI (418), Apple (414), Taylor Swift (382),
Artificial Intelligence (371), Manchester United (366), Meta (366).

This is the MultiHop-RAG news corpus. The 70.2% w=1 mass is exactly the
projection-density problem the report flags in §7.2 — the graph is dominated by
incidental co-occurrence, which is what BiCM (Exp 1) is built to quantify.

## Experiment 1 — BiCM null-model edge validation

**Question (paper §7.2):** is raw `shared_fact_count` usable as-is, or must it
be validated/thresholded? The report's own bipartite-projection theory says raw
projections are dense and degree-confounded and require null-model validation,
but leaves "whether the raw weights are practically useful for navigation" as an
open empirical question.

**Method:**
1. Load the bipartite `fact↔concept` graph for the `multihoprag` repo from the
   DB (one query, ~95k rows).
2. Compute observed `C_ij = |shared facts|` for every concept pair.
3. Compute the BiCM (bipartite configuration model) null: expected
   `⟨C_ij⟩ = k_i · k_j / |Γ|` and variance under degree preservation (Poisson
   approximation per the cited Strona et al. BiCM literature).
4. For each edge: z-score `(C_ij − ⟨C_ij⟩) / σ_null`, p-value, FDR
   (Benjamini-Hochberg).
5. **Metric 1:** Spearman rank correlation between raw `shared_fact_count` and
   BiCM z-score across all edges. Low correlation ⇒ raw weight is mostly degree.
6. **Metric 2:** % of edges flagged "degree-confounded" (high raw weight,
   |z|<1). Report's prediction: ubiquitous concepts ("United States") dominate.
7. **Metric 3:** survivor count at p<0.05, p<0.01, FDR<0.05; threshold sweep
   w≥1/2/5.
8. **Diagnostic:** the top-weight edges whose z-score is near 0 — are they the
   high-degree hub pairs the report predicts (Google↔Amazon, USA↔everything)?

**Output:** `results/bicm_validation.json` (per-edge table + summary). No LLM
cost. Seconds of compute at this scale.

**What it settles:** whether Experiments 2 and 4 should use raw weights or
BiCM-validated weights, and whether the report's "defensible engineering choice
for a retrieval-aid graph" claim holds at this corpus scale. Cheapest
experiment; runs first and de-risks the rest.

## Experiment 2 — 7 graph-property measurements (§6.1) [planned, not started]

Question: is the emergent graph navigable, coherent, and backbone-structured?
(Report's §6.3 priorities.) Seven measurements: degree distribution + Broido–
Clauset taxonomy, connected components, community structure (Louvain + NMI vs
corpus domains), small-world index σ, edge-weight distribution (cross-checked
against Exp 1's BiCM survivors), concept fragmentation audit, source_count
centrality correlation. Uses BiCM-validated weights from Exp 1.

## Experiment 3 — 5 failure-mode audits (§6.2) [planned, not started]

Five gold-standard-grounded audits: under-merging (fragmentation), over-merging
(false merges), missing concepts (recall), dedup-severed facts, context
mislabeling. Rater: LLM-as-judge + Wikidata ground truth. Compare against
prior reference points (51% recall ceiling, cross-contamination evidence) where
relevant; does not re-measure those from scratch.

## Experiment 4 — KGQA head-to-head (§4.4 / §7.2) [planned, not started]

Question the report concedes but never measures: does a triplet KG beat OKT's
concept-graph on a task that needs relation semantics? Three conditions on the
existing MultiHop-RAG question set: (a) triplet-KG retrieval, (b) OKT
concept-graph walk via existing REST tools, (c) OKT facts-direct baseline
(0.683 acc). Scored per hop depth. Most expensive; runs last.

## Execution order

```
Exp 1 (BiCM)            ──► Exp 2 (uses BiCM-validated weights)
Exp 3 (failure audits)  ──► independent, parallel with 1-2
Exp 4 (KGQA)            ──► runs last, most expensive
REPORT.md               ──► written after all four, mirrors §6/§7 structure
```

## Files

```
scripts/experiments/graph_analysis/
├── PLAN.md                   # this file
├── README.md                 # run instructions + scope caveats (written with Exp 1)
├── okt_db.py                 # read-only async DB access
├── llm_judge.py              # OpenRouter LLM-as-judge client [Exp 3]
├── wikidata.py               # Wikidata Q-ID lookup [Exp 3]
├── exp1_bicm.py              # null-model edge validation
├── exp2_graph_properties.py  # 7 measurements [planned]
├── exp3_failure_audits.py    # 5 failure modes [planned]
├── exp4_kgqa_headtohead.py   # triplet-vs-concept-graph-vs-facts [planned]
├── results/                  # gitignored: json + plots + audited samples
└── REPORT.md                 # findings, written last
```

All code written fresh. No imports from the old repo. The old repo's findings
are cited in `REPORT.md` as prior reference points where relevant, not
re-measured.

## What is NOT in this plan

- No OKT backend code changes, no migrations, no new tasks, no MCP tool changes.
- No relation-label/summary feature.
- No cross-corpus concept identity / Wikidata linking in the OKT schema
  (Wikidata is used read-only, by the experiments, as ground truth only).
- No imports or copies from the old repo — it is reference material only.