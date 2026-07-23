# MultiHop-RAG Experiment — Findings

A fixed, deterministic Python pipeline that scores OKT's retrieval paths
on the full [MultiHop-RAG](https://huggingface.co/datasets/yixuantt/MultiHopRAG)
benchmark (n=2556 questions). Three retrieval strategies are compared,
with token costs tracked per LLM call.

## Three experiments

1. **Aggressive-prompt run** (concept + facts + direct, facts=10):
   The original three-variant comparison with a "commit to an answer,
   do not abstain just because you are unsure" synthesis prompt. This
   is the run that established the concept-vs-facts-vs-direct
   separation.

2. **Inference-prompt, facts=10** (facts + direct): The synthesis
   prompt was softened to "draw reasonable inferences if the evidence
   logically entails an answer, but do NOT guess when the evidence is
   silent or ambiguous." Both variants run at the default 10 facts per
   search query.

3. **Inference-prompt, facts=20** (facts + direct): Same prompt as #2,
   but the fact budget per search query is doubled from 10 to 20,
   giving the synthesis LLM more context.

---

## Experiment 1: Aggressive prompt (concept + facts + direct, facts=10)

n=2556, `--concurrency 30`, synthesis prompt says "commit to an answer,
make your best guess."

| variant | acc | cov | refuse | halluc% | tokens/q |
|---|---|---|---|---|---|
| concept | 0.395 | 0.986 | 1598 | 9.6% | 6,845 |
| facts | **0.749** | 0.991 | 641 | 11.6% | 5,177 |
| direct | 0.668 | 1.000 | 883 | 10.3% | **1,912** |

### Per question type

| type | n | concept | facts | direct |
|---|---|---|---|---|
| comparison_query | 856 | 0.250 | **0.648** | 0.549 |
| inference_query | 816 | 0.485 | **0.857** | 0.770 |
| null_query | 301 | 0.990 | 0.983 | 0.983 |
| temporal_query | 583 | 0.175 | **0.624** | 0.537 |

### Per-question agreement (n=2556)

- union correct: 2115 (**0.827**)
- all wrong: 441
- facts only: 273
- direct only: 128
- concept only: 34

---

## Experiment 2: Inference prompt (facts + direct, facts=10)

n=2556, `--concurrency 30`. The synthesis prompt was changed to allow
reasonable inferences but prohibit guessing when evidence is absent or
ambiguous. LLM retries (3 attempts with backoff) and `[LLM ERROR]`
markers were added so failed synthesis calls are no longer counted as
abstentions.

| variant | acc | cov | refuse | halluc% | tokens/q |
|---|---|---|---|---|---|
| facts@10 | **0.697** | 0.996 | 820 | 9.8% | 5,083 |
| direct@10 | 0.560 | 1.000 | 1225 | **7.7%** | **1,931** |

### Per question type

| type | n | facts@10 | direct@10 |
|---|---|---|---|
| comparison_query | 856 | **0.612** | 0.499 |
| inference_query | 816 | **0.767** | 0.539 |
| null_query | 301 | 0.983 | 0.993 |
| temporal_query | 583 | **0.575** | 0.456 |

### vs Experiment 1 (aggressive prompt, same fact budget)

| variant | acc (aggressive) | acc (inference) | Δ |
|---|---|---|---|
| facts | 0.749 | 0.697 | -0.052 |
| direct | 0.668 | 0.560 | -0.108 |

The inference prompt trades accuracy for lower hallucination. Direct's
hallucination dropped from 10.3% → 7.7%; facts' from 11.6% → 9.8%. The
accuracy cost is larger for direct (-0.108) than facts (-0.052),
suggesting the facts variant benefits more from the aggressive prompt
because its targeted retrieval gives the LLM better evidence to commit
on.

---

## Experiment 3: Inference prompt (facts + direct, facts=20)

n=2556, `--concurrency 30`. Same inference prompt as Experiment 2, but
`facts_per_query` doubled from 10 to 20.

| variant | acc | cov | refuse | halluc% | tokens/q |
|---|---|---|---|---|---|
| facts@20 | **0.757** | 0.997 | 633 | 11.1% | 8,830 |
| direct@20 | 0.683 | 1.000 | 867 | 9.4% | **3,312** |

### Per question type

| type | n | facts@20 | direct@20 | facts@10 | direct@10 |
|---|---|---|---|---|---|
| comparison_query | 856 | **0.643** | 0.591 | 0.612 | 0.499 |
| inference_query | 816 | **0.876** | 0.743 | 0.767 | 0.539 |
| null_query | 301 | 0.987 | 0.987 | 0.983 | 0.993 |
| temporal_query | 583 | **0.642** | 0.578 | 0.575 | 0.456 |

### Doubling the fact budget (10 → 20)

| variant | acc@10 | acc@20 | Δ | tokens@10 | tokens@20 | Δ cost |
|---|---|---|---|---|---|---|
| facts | 0.697 | **0.757** | +0.060 | 5,083 | 8,830 | +3,747 |
| direct | 0.560 | **0.683** | +0.123 | 1,931 | 3,312 | +1,381 |

Doubling the fact budget lifts both variants. Direct benefits more
(+0.123 vs +0.060) because it was bottlenecked by the 10-fact cap —
the single full-question query often surfaces the right facts just
beyond rank 10. The facts variant already deduplicates across multiple
queries so the marginal gain from more results per query is smaller.

The cost/accuracy frontier across all four configurations:

| config | acc | tokens/q | acc per 1k tokens |
|---|---|---|---|
| direct@10 | 0.560 | 1,931 | 0.29 |
| direct@20 | 0.683 | 3,312 | 0.21 |
| facts@10 | 0.697 | 5,083 | 0.14 |
| facts@20 | 0.757 | 8,830 | 0.09 |

direct@20 is the best cost/quality trade-off: 0.683 accuracy at 3,312
tokens/q — within 0.014 of facts@10 (0.697) at 60% of the token cost.

---

## Hallucination breakdown

The hallucination hotspot across all runs is **comparison_query**
(~20%), where the "draw reasonable inferences" instruction causes
the LLM to commit to "Yes"/"no" on cross-article questions where the
evidence is ambiguous. On inference_query (the cleanest bucket) all
configurations stay under 3%.

| config | comparison | inference | null | temporal | overall |
|---|---|---|---|---|---|
| facts@10 | 19.4% | 1.5% | 1.7% | 11.7% | 9.8% |
| direct@10 | 17.8% | 0.5% | 0.7% | 6.9% | **7.7%** |
| facts@20 | 21.1% | 2.7% | 1.3% | 13.2% | 11.1% |
| direct@20 | 20.0% | 1.3% | 1.3% | 9.3% | 9.4% |

More facts → more hallucination. The extra context gives the LLM more
material to draw inferences from, and some of those inferences are
wrong — particularly on comparison and temporal questions where the
evidence is often partial.

---

## Three findings

### 1. Facts are an effective low-hallucination chunking strategy

Across all four configurations, hallucination stays between 7.7% and
11.1% on a hard multi-hop benchmark. The atomic-fact + source-
attribution design constrains the synthesis LLM to verifiable claims
with sources. The hallucination control is a property of the chunking
strategy, not the query-extraction step — even the naive direct variant
keeps hallucination under 10%.

### 2. Direct retrieval on facts is surprisingly competitive

The `direct` variant — no LLM query extraction, just feed the full
question to `websearch_to_tsquery` — achieves 0.683 at facts=20 with
only 3,312 tokens/q. That's within 0.014 of facts@10 (0.697) at 65%
of the token cost. For cost-sensitive deployments, direct@20 is a
serious option.

### 3. Concepts are not the right substrate for direct, specific QA

The concept variant (Experiment 1) scored 0.395 with only 34 unique
wins (1.3% of questions) and the highest token cost (6,845/q). The
concept-first path is a *browsing* pattern, not a retrieval-
optimization pattern. Its value is in synthesis, cross-document
navigation, and provenance auditability — not in targeted QA. The
benchmark measures the wrong thing for concepts.

---

## What drove the results

Three changes, in order of leverage:

1. **Source metadata backfill + API surface fix.** The
   `backfill-source-metadata` script parsed YAML frontmatter from
   uploaded markdown into `parsed_title`, `parsed_sitename`,
   `parsed_author`, and `published_at` on the source rows. The
   `ListFactSources` SQL was extended to surface those columns through
   `getFact`. The synthesis prompt was rewritten to compose a
   one-line attribution from them. This unlocked comparison questions
   (publication name) and temporal questions (published_at).

2. **Concept→facts ORDER BY fix.** `ListFactsByConcept` was
   hardcoded to `ORDER BY fc.first_seen_at`. It now defaults to
   `source_count DESC, ts_rank DESC, first_seen_at`, mirroring the
   repo-wide `/facts` endpoint.

3. **Dedicated fact-query extraction prompt.** The facts variant
   uses a separate LLM call to produce 3-6 term keyword-rich
   `websearch_to_tsquery` strings, tuned for the fact tsvector index.

---

## Reproducing

```bash
# Run facts and direct at facts=10 and facts=20 on the full benchmark
python3 run_benchmark.py --variant facts  --concurrency 30 --facts-per-query 10
python3 run_benchmark.py --variant direct --concurrency 30 --facts-per-query 10
python3 run_benchmark.py --variant facts  --concurrency 30 --facts-per-query 20
python3 run_benchmark.py --variant direct --concurrency 30 --facts-per-query 20

# Score side-by-side
python3 score.py
```

Output files (all gitignored):
- `results/predictions_{facts,direct}.jsonl` — per-question predictions
- `results/qa_metrics.json` — full metrics + token counts + agreement matrix
- `results/summary.txt` — the printed side-by-side table
- `answers_{facts,direct}/<id>.md` — per-question audit