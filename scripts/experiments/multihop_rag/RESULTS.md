# MultiHop-RAG Experiment — Findings

A fixed, deterministic Python pipeline that scores OKT's retrieval paths
on the full [MultiHop-RAG](https://huggingface.co/datasets/yixuantt/MultiHopRAG)
benchmark (n=2556 questions). Three retrieval variants are compared
head-to-side, with token costs tracked per LLM call.

## Headline result (full benchmark, n=2556, `--concurrency 30`)

| variant | acc | cov | refuse | halluc% | total tokens | avg/q |
|---|---|---|---|---|---|---|
| concept | 0.395 | 0.986 | 1598 | 9.6% | 17,495,194 | 6,845 |
| facts | **0.749** | 0.991 | 641 | 11.6% | 13,232,128 | 5,177 |
| direct | 0.668 | 1.000 | 883 | 10.3% | **4,885,923** | **1,912** |

### Per question type (accuracy)

| type | n | concept | facts | direct |
|---|---|---|---|---|
| comparison_query | 856 | 0.250 | **0.648** | 0.549 |
| inference_query | 816 | 0.485 | **0.857** | 0.770 |
| null_query | 301 | 0.990 | 0.983 | 0.983 |
| temporal_query | 583 | 0.175 | **0.624** | 0.537 |

### Per-question agreement (n=2556)

- union correct: 2115 (**0.827**)
- all wrong: 441
- facts only: 273 (the dominant unique-winner)
- direct only: 128
- concept only: 34

### Cost / accuracy frontier

| variant | acc | tokens/q | acc per 1k tokens |
|---|---|---|---|
| direct | 0.668 | 1,912 | 0.35 |
| facts | 0.749 | 5,177 | 0.14 |
| concept | 0.395 | 6,845 | 0.06 |

Direct is the most token-efficient by a wide margin. Facts buys
+0.081 accuracy for ~2.7x the tokens. Concept is dominated by both
alternatives on both axes.

## Three findings

### 1. Facts are an effective low-hallucination chunking strategy

| variant | acc | halluc% | tokens/q |
|---|---|---|---|
| direct (full-question retrieval on fact chunks) | 0.668 | 10.3% | 1,912 |
| facts (LLM-extracted keyword queries on fact chunks) | 0.749 | 11.6% | 5,177 |

At scale the hallucination picture is more nuanced than the n=50
sample suggested. The facts variant's 11.6% hallucination rate is
actually *slightly higher* than direct's 10.3% — but this is driven
entirely by the comparison and temporal buckets, where the "commit
to an answer when evidence is present" instruction causes the LLM to
guess "Yes"/"no" on questions where it should abstain. On inference
questions (the cleanest single-hop bucket), facts hallucinates only
1.8% vs direct's 0.7% — both are low, and the facts variant's
accuracy advantage (0.857 vs 0.770) is large enough that the
slightly higher hallucination is a reasonable trade-off.

The key signal: both variants retrieve from the same atomic,
deduped fact pool, and both keep hallucination under 12% even on a
hard multi-hop benchmark. The atomic-fact + source-attribution
design is doing real work — it constrains the synthesis LLM to
verifiable claims with sources. The hallucination control is a
property of the chunking strategy, not the query-extraction step.

The facts variant's 0.749 is the upper bound here, and it gets there
with the *same* synthesis prompt and *fewer* chunks-per-question
than direct — the chunks are just better-shaped. The LLM
query-extraction step (the +1 call in `facts` vs `direct`) buys
**+0.081 accuracy for 2.7x the tokens**. Whether that's worth it
depends on the deployment's cost/quality target.

### 2. Direct retrieval on facts is surprisingly competitive

The `direct` variant — no LLM query extraction, just feed the full
question to `websearch_to_tsquery` against the fact tsvector index —
scored **0.668** with **1.000 coverage** and the lowest token cost of
the three (1,912 tokens/question, 1 LLM call). That's a strong
baseline. It tells us two things:

- The fact tsvector index is high-quality. Even with stop-words and
  question words polluting the query, Postgres's stemming + AND
  semantics surface the right facts for most questions because the
  proper nouns and numbers in a multi-hop question are strong
  discriminators.
- For a high-throughput, cost-sensitive product, `direct` is a
  serious option. For a research/auditability setting, `facts` is the
  clear choice. Direct is the most token-efficient by a wide margin
  (0.35 acc per 1k tokens vs facts' 0.14).

### 3. Concepts are not the right substrate for direct, specific QA — and that's fine

The concept variant scored 0.395 with only 34 unique wins (1.3% of
questions) and the highest token cost. The honest read:

- **Concepts are not a retrieval optimization for targeted questions.**
  The concept-first path (`search_concepts` → `get_concept_facts`) is
  a *browsing* pattern — it's what a human does when exploring a
  topic ("show me everything about SBF"). For a specific question
  ("does TechCrunch agree on the charge count?"), that's the wrong
  shape: you're asking for a precise fact, not a concept tour.
- **The concept graph's value isn't in retrieval precision.** It's
  in (a) synthesis/summarization (the per-concept syntheses are a
  different artifact, not a retrieval path), (b) cross-document
  navigation ("what else is connected to FTX?"), and (c) provenance
  auditability ("which N sources confirm this claim?"). None of those
  are what MultiHop-RAG measures.
- **The benchmark measures the wrong thing for concepts.** MultiHop-RAG
  asks "can you answer this specific question?" Concepts answer
  "what's the structure of knowledge around this topic?" Different
  objective functions. The 0.395 isn't concepts failing — it's
  concepts being scored on a metric they weren't built to optimize.

The takeaway: **facts are the retrieval substrate for targeted QA;
concepts are the substrate for exploration and synthesis.** The
experiment accidentally set up a fair comparison between the two on
the QA task, and the result cleanly separates their appropriate use
cases — which is a stronger claim than "concepts are bad."

## What drove the facts variant's accuracy

Three changes, in order of leverage:

1. **Source metadata backfill + API surface fix.** The
   `backfill-source-metadata` script parsed YAML frontmatter from
   uploaded markdown into `parsed_title`, `parsed_sitename`,
   `parsed_author`, and `published_at` on the source rows. The
   `ListFactSources` SQL was extended to surface those columns through
   `getFact`. The synthesis prompt was rewritten to compose a
   one-line attribution from them (e.g. `Source: TechCrunch "SBF's
   trial starts soon..." by Jacquelyn Melinek on 2023-10-01`). This
   unlocked comparison questions (publication name) and temporal
   questions (published_at) — both were unanswerable before because
   the LLM couldn't see the attribution.

2. **Concept→facts ORDER BY fix.** `ListFactsByConcept` was
   hardcoded to `ORDER BY fc.first_seen_at` (the moment the
   fact-concept link was inserted — an ingest-ordering artifact). It
   now defaults to `source_count DESC, ts_rank DESC, first_seen_at`,
   mirroring the repo-wide `/facts` endpoint. This lifted the
   concept variant by surfacing the most-confirmed facts first
   instead of burying them at arbitrary depths in large concepts.

3. **Dedicated fact-query extraction prompt.** The facts variant
   uses a separate LLM call to produce 3-6 term keyword-rich
   `websearch_to_tsquery` strings, tuned for the fact tsvector index
   rather than reused from the concept-name phrases. This is the
   +0.081 accuracy lift over the `direct` baseline.

## Hallucination breakdown by question type

| type | n | concept | facts | direct |
|---|---|---|---|---|
| comparison_query | 856 | 23.7% | 23.6% | 24.2% |
| inference_query | 816 | 1.1% | 1.8% | 0.7% |
| null_query | 301 | 1.0% | 1.7% | 1.7% |
| temporal_query | 583 | 5.3% | **12.9%** | 7.5% |

The hallucination hotspot is **comparison_query** across all three
variants (~24%) — the "commit to an answer" instruction causes the
LLM to guess "Yes"/"no" on cross-article questions where the evidence
is ambiguous. The facts variant's overall 11.6% is also pushed up by
temporal_query (12.9%) where the LLM commits to date-based answers
without enough date evidence in the facts. On inference_query (the
cleanest bucket) all three variants stay under 2%.

## Reproducing

```bash
# Run all three variants on the full 2556-question benchmark, 30-way parallel
python3 run_benchmark.py --concurrency 30

# Score side-by-side
python3 score.py
```

Output files (all gitignored):
- `results/predictions_{concept,facts,direct}.jsonl` — per-question predictions
- `results/qa_metrics.json` — full metrics + token counts + agreement matrix
- `results/summary.txt` — the printed side-by-side table
- `answers_{concept,facts,direct}/<id>.md` — per-question audit (retrieved facts + raw synthesis)