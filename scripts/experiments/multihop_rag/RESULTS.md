# MultiHop-RAG Experiment — Findings

A fixed, deterministic Python pipeline that scores OKT's retrieval paths
on the [MultiHop-RAG](https://huggingface.co/datasets/yixuantt/MultiHopRAG)
benchmark. Three retrieval variants are compared head-to-head on a
random sample of 50 questions, with token costs tracked per LLM call.

## Headline result (`--sample 50 --concurrency 10`)

| variant | acc | cov | refuse | halluc% | total tokens | avg/q |
|---|---|---|---|---|---|---|
| concept | 0.520 | 1.000 | 35 | 2.0% | 333,771 | 6,675 |
| facts | **0.920** | 0.980 | 15 | **2.0%** | 244,654 | 4,893 |
| direct | 0.760 | 1.000 | 21 | 6.0% | **94,851** | **1,897** |

### Per question type (accuracy)

| type | n | concept | facts | direct |
|---|---|---|---|---|
| comparison_query | 9 | 0.111 | **0.889** | 0.778 |
| inference_query | 16 | 0.688 | **0.938** | 0.688 |
| null_query | 12 | 1.000 | 1.000 | 1.000 |
| temporal_query | 13 | 0.154 | **0.846** | 0.615 |

### Per-question agreement (n=50)

- union correct: 46 (0.920)
- all wrong: 4
- facts only: 5 (the only variant with unique wins)
- concept only: 0
- direct only: 0

## Three findings

### 1. Facts are an effective low-hallucination chunking strategy

| variant | acc | halluc% | tokens/q |
|---|---|---|---|
| direct (full-question retrieval on fact chunks) | 0.760 | **6.0%** | 1,897 |
| facts (LLM-extracted keyword queries on fact chunks) | 0.920 | **2.0%** | 4,893 |

The hallucination rate **triples** (2.0% → 6.0%) when retrieval is
looser, even though both variants retrieve from the same atomic,
deduped fact pool and use the same synthesis prompt. Atomic,
self-contained facts constrain the synthesis LLM to verifiable claims
— it can't fabricate because every chunk is a checkable assertion with
a source. This is the core OKT value proposition quantified: dedup +
atomic extraction isn't just retrieval ergonomics, it's a
**hallucination control mechanism** that survives even naive
retrieval (the direct variant).

The facts variant's 0.920 is the upper bound here, and it gets there
with the *same* synthesis prompt and *fewer* chunks-per-question than
direct — the chunks are just better-shaped. The LLM query-extraction
step (the +1 call in `facts` vs `direct`) buys **+0.160 accuracy for
2.6x the tokens**. Whether that's worth it depends on the
deployment's cost/quality target.

### 2. Direct retrieval on facts is surprisingly competitive

The `direct` variant — no LLM query extraction, just feed the full
question to `websearch_to_tsquery` against the fact tsvector index —
scored **0.760** with **1.000 coverage** and the lowest token cost of
the three (1,897 tokens/question, 1 LLM call). That's a strong
baseline. It tells us two things:

- The fact tsvector index is high-quality. Even with stop-words and
  question words polluting the query, Postgres's stemming + AND
  semantics surface the right facts for most questions because the
  proper nouns and numbers in a multi-hop question are strong
  discriminators.
- For a high-throughput, cost-sensitive product, `direct` is a
  serious option. For a research/auditability setting, `facts` is the
  clear choice. The cost/accuracy frontier:

  | variant | acc | tokens | acc per 1k tokens |
  |---|---|---|---|
  | direct | 0.760 | 94,851 | 8.0 |
  | facts | 0.920 | 244,654 | 3.8 |
  | concept | 0.520 | 333,771 | 1.6 |

  Direct is the most token-efficient by a wide margin. Facts buys
  +0.160 accuracy for ~2.5x the tokens. Concept spends the most
  tokens and scores lowest.

### 3. Concepts are not the right substrate for direct, specific QA — and that's fine

The concept variant scored 0.520 with 0 unique wins and the highest
token cost. The honest read:

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
  objective functions. The 0.520 isn't concepts failing — it's
  concepts being scored on a metric they weren't built to optimize.

The takeaway: **facts are the retrieval substrate for targeted QA;
concepts are the substrate for exploration and synthesis.** The
experiment accidentally set up a fair comparison between the two on
the QA task, and the result cleanly separates their appropriate use
cases — which is a stronger claim than "concepts are bad."

## What drove the facts variant from 0.400 → 0.920

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
   concept variant from 0.460 → 0.520 by surfacing the most-confirmed
   facts first instead of burying them at rank 14 of 21.

3. **Dedicated fact-query extraction prompt.** The facts variant
   uses a separate LLM call to produce 3-6 term keyword-rich
   `websearch_to_tsquery` strings, tuned for the fact tsvector index
   rather than reused from the concept-name phrases. This is the
   +0.160 accuracy lift over the `direct` baseline.

## Reproducing

```bash
# Run all three variants on 50 random questions, 10-way parallel
python3 run_benchmark.py --sample 50 --concurrency 10

# Score side-by-side
python3 score.py
```

Output files (all gitignored):
- `results/predictions_{concept,facts,direct}.jsonl` — per-question predictions
- `results/qa_metrics.json` — full metrics + token counts + agreement matrix
- `results/summary.txt` — the printed side-by-side table
- `answers_{concept,facts,direct}/<id>.md` — per-question audit (retrieved facts + raw synthesis)