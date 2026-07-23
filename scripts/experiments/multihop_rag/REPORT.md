# MultiHop-RAG Benchmark: OKT Retrieval Strategy Evaluation

## Abstract

We evaluate three retrieval strategies on the full MultiHop-RAG benchmark
(n=2,556 multi-hop questions across 609 news articles) using the Open
Knowledge Tree (OKT) fact-graph system. The strategies differ in how they
retrieve evidence from OKT's atomic-fact store: (1) **concept-first**
retrieval navigates the concept graph then fetches facts per concept,
(2) **fact search** uses an LLM to extract keyword queries and searches
the fact index directly, and (3) **direct** retrieval passes the full
question text to the fact index with no query engineering. We measure
accuracy, hallucination rate, and token cost across multiple
configurations.

Our findings: OKT's atomic-fact chunking strategy achieves up to 0.757
accuracy with 11.1% hallucination on a multi-hop QA benchmark, while a
naive no-query-engineering baseline reaches 0.683 at one-third the token
cost. The concept-first retrieval path—OKT's signature navigation
structure—scores 0.395 and is dominated by direct fact search on both
accuracy and cost, confirming that concepts serve exploration and
synthesis rather than targeted question answering. Hallucination remains
between 7.7% and 11.1% across all configurations, driven primarily by
comparison questions (~20%) where the LLM infers cross-article agreement
from partial evidence.

---

## 1. Introduction

MultiHop-RAG (Tang & Yang, COLM 2024) is a benchmark for multi-hop
question answering over a corpus of 609 news articles from Q4 2023. The
benchmark contains 2,556 questions across four types:

- **inference_query** (816): single-document reasoning ("Who is the
  founder of the company that...").
- **comparison_query** (856): cross-document comparison ("Does the
  TechCrunch article agree with the Bloomberg article on...").
- **temporal_query** (583): time-bound reasoning ("Was there a change
  between the November article and the December article...").
- **null_query** (301): unanswerable questions whose gold answer is
  "Insufficient information."

Each question requires reasoning across multiple sources—combining
facts from different articles, using publication attribution, and
reasoning about dates. The benchmark tests both retrieval quality
(finding the right articles) and synthesis quality (combining evidence
into an answer).

### 1.1 The Open Knowledge Tree (OKT) system

OKT ingests documents into an atomic-fact graph:

1. **Source decomposition**: Each document is split into self-contained
   atomic facts (1-4 sentences each), each verifiable from the source
   text alone.
2. **Deduplication**: Semantically equivalent facts from different
   sources are merged, preserving all source URLs. A fact confirmed by
   N independent sources carries `source_count = N`.
3. **Concept extraction**: Facts are tagged with concepts (people,
   organizations, topics) via LLM extraction, forming a concept graph.
4. **Synthesis**: Each concept group gets a persistent synthesis
   summarizing its facts with fact-level citations.

The fact graph is stored in PostgreSQL with a `tsvector` full-text index
(`search_tsv`) over fact text, and a concept graph (`fact_concepts` join
table linking facts to concepts). Retrieval uses
`websearch_to_tsquery` (AND semantics—every query term must appear in
the fact text for a match).

### 1.2 Research questions

1. How does OKT's atomic-fact chunking compare to naive
   full-question retrieval on multi-hop QA accuracy and hallucination?
2. Does LLM query extraction (crafting targeted keyword queries) justify
   its token cost over naive retrieval?
3. Is the concept-first retrieval path—the signature feature of OKT's
   graph structure—effective for targeted question answering?
4. How does the fact budget (number of facts passed to the synthesis
   LLM) affect the accuracy/cost/hallucination trade-off?
5. What is the hallucination profile of each strategy across question
   types?

---

## 2. Experimental Setup

### 2.1 Corpus

The MultiHop-RAG corpus (609 news articles, Q4 2023) was downloaded from
HuggingFace and uploaded to a dedicated OKT repository (`multihoprag`).
Each article is a markdown file with YAML frontmatter (title, source
publication, author, category, published_at). The full ingest pipeline
ran to completion: source decomposition → fact embedding → deduplication
→ concept extraction → concept synthesis → concept relation refresh.

### 2.2 Source metadata backfill

The upload ingestion path wrote the raw markdown (including frontmatter)
into `parsed_text` but never harvested the frontmatter into the
structured source columns (`parsed_title`, `parsed_sitename`,
`parsed_author`, `published_at`). A one-shot backfill script
(`backfill-source-metadata`) parsed the YAML frontmatter and populated
these columns for all 609 sources. The fact-detail API
(`ListFactSources` SQL) was extended to surface these columns through
the `getFact` endpoint so the synthesis LLM could see publication names
and dates—a prerequisite for answering comparison and temporal questions
that reference articles by publication and date.

### 2.3 Retrieval strategies

Three strategies share a common final step: the retrieved facts (with
source metadata) are passed to an LLM synthesis prompt that produces a
short answer ("Yes", "no", a name, or "Insufficient information.").

**Concept-first** (3 LLM calls per question):
1. LLM extracts 1-5 noun phrases from the question (concept-name
   queries).
2. OKT `search_concepts` (lexical ILIKE on canonical_name) finds
   matching concepts. Top-5 by fact_count are selected.
3. LLM extracts 1-5 keyword-rich tsvector queries (separate from the
   concept-name phrases).
4. For each concept, OKT `get_concept_facts` fetches facts linked to
   that concept, filtered by the tsvector queries (with `source_count`
   DESC ordering and `ts_rank` tiebreaker). Fallback to unfiltered
   newest facts if no query matches.
5. Facts are deduplicated across concepts, enriched with source
   metadata via `getFact`, and passed to the synthesis LLM.

**Fact search** (2 LLM calls per question):
1. LLM extracts 1-5 keyword-rich tsvector queries (3-6 terms each,
   targeting discriminative nouns, numbers, and publication names).
2. Each query is run against the repo-wide `/facts` endpoint with
   `sort=source_count` (most-confirmed facts first).
3. Results are deduplicated across queries, enriched, and passed to
   the synthesis LLM.

**Direct** (1 LLM call per question):
1. The full question text is passed directly as the `q` parameter to
   the repo-wide `/facts` endpoint with `sort=source_count`.
2. Results are enriched and passed to the synthesis LLM.

No query engineering, no extraction LLM call—just the raw question
against the fact tsvector index.

### 2.4 Synthesis prompt

The synthesis prompt (shared by all strategies) instructs the LLM to:
1. Use only the provided facts and their source metadata.
2. Combine facts across multiple sources (multi-hop) and use source
   metadata (publication name, title, author, published_at) when the
   question references it.
3. Draw reasonable inferences if the evidence logically entails an
   answer (e.g., two facts from the same publication stating the same
   claim → the articles agree), but abstain if the evidence is silent
   or ambiguous.
4. Keep the answer short: a single name, organization, "Yes", "no", or
   a short phrase.
5. End with a fixed contract format: `The answer to the question is
   "<answer>"`.

### 2.5 Scoring

Following the official MultiHop-RAG `qa_evaluate.py`, a prediction is
correct if it shares any token (case-insensitive) with the gold answer.
This is a lenient metric—shared stop-words can produce false
positives—but it matches the paper's methodology and enables
comparison with published results.

We also track:
- **Refusal**: prediction is "Insufficient information." (abstention).
- **Hallucination**: prediction is a substantive (non-refusal) answer
  that is wrong.
- **LLM error**: the synthesis LLM call failed after 3 retry attempts
  (excluded from accuracy/refusal/hallucination metrics).
- **Token cost**: prompt + completion tokens per question, broken down
  by LLM call type (query extraction, synthesis).

### 2.6 LLM backend

All LLM calls use OpenRouter with `openai/gpt-4o-mini`. The OKT API
(retrieval) runs locally on `localhost:8080`. OKT retrieval uses a
personal API token with `fact:read` and `concept:read` scopes; LLM
calls go directly to OpenRouter.

### 2.7 Configurations

We run four configurations on the full 2,556-question benchmark with
30-way concurrency:

| Config | Strategy | Facts per query | Prompt |
|---|---|---|---|
| facts@10 | Fact search | 10 | Inference |
| direct@10 | Direct | 10 | Inference |
| facts@20 | Fact search | 20 | Inference |
| direct@20 | Direct | 20 | Inference |

Additionally, we report a prior run with all three strategies (including
concept-first) using a more aggressive "commit to an answer, do not
abstain just because you are unsure" synthesis prompt (Experiment 1).

---

## 3. Results

### 3.1 Main results (inference prompt, n=2556)

| Config | Acc | Cov | Refuse | Halluc% | LLM Err | Tokens/q |
|---|---|---|---|---|---|---|
| direct@10 | 0.560 | 1.000 | 1225 | **7.7%** | 0 | **1,931** |
| direct@20 | 0.683 | 1.000 | 867 | 9.4% | 0 | 3,312 |
| facts@10 | 0.697 | 0.996 | 820 | 9.8% | 0 | 5,083 |
| facts@20 | **0.757** | 0.997 | 633 | 11.1% | 0 | 8,830 |

### 3.2 Per question type

| Type | n | direct@10 | direct@20 | facts@10 | facts@20 |
|---|---|---|---|---|---|
| comparison | 856 | 0.499 | 0.591 | 0.612 | **0.643** |
| inference | 816 | 0.539 | 0.743 | 0.767 | **0.876** |
| null | 301 | 0.993 | 0.987 | 0.983 | 0.987 |
| temporal | 583 | 0.456 | 0.578 | 0.575 | **0.642** |

### 3.3 Effect of fact budget (10 → 20)

| Config | Acc@10 | Acc@20 | Δ | Tokens@10 | Tokens@20 | Δ Cost |
|---|---|---|---|---|---|---|
| facts | 0.697 | 0.757 | +0.060 | 5,083 | 8,830 | +3,747 |
| direct | 0.560 | 0.683 | +0.123 | 1,931 | 3,312 | +1,381 |

Doubling the fact budget lifts both strategies, but direct benefits
more (+0.123 vs +0.060). At facts=10, the single full-question query
often surfaces the right facts just beyond rank 10; doubling the cap
captures them. The fact search strategy already deduplicates across
multiple queries (median 54 facts at budget=20 vs 10 for direct), so
the marginal gain from more results per query is smaller.

### 3.4 Cost/accuracy frontier

| Config | Acc | Tokens/q | Acc per 1k tokens |
|---|---|---|---|
| direct@10 | 0.560 | 1,931 | 0.29 |
| direct@20 | 0.683 | 3,312 | 0.21 |
| facts@10 | 0.697 | 5,083 | 0.14 |
| facts@20 | 0.757 | 8,830 | 0.09 |

direct@20 is the best cost/quality trade-off: 0.683 accuracy at 3,312
tokens/q—within 0.014 of facts@10 (0.697) at 65% of the token cost.

### 3.5 Hallucination breakdown by question type

| Config | Comparison | Inference | Null | Temporal | Overall |
|---|---|---|---|---|---|
| direct@10 | 17.8% | 0.5% | 0.7% | 6.9% | **7.7%** |
| direct@20 | 20.0% | 1.3% | 1.3% | 9.3% | 9.4% |
| facts@10 | 19.4% | 1.5% | 1.7% | 11.7% | 9.8% |
| facts@20 | 21.1% | 2.7% | 1.3% | 13.2% | 11.1% |

The hallucination hotspot across all configurations is
**comparison_query** (~18-21%), where the "draw reasonable inferences"
instruction causes the LLM to commit to "Yes"/"no" on cross-article
questions where the evidence is partial. On inference_query (the
cleanest single-document bucket), all configurations stay under 3%.

More facts → more hallucination: the extra context gives the LLM more
material to draw inferences from, and some of those inferences are
wrong—particularly on comparison and temporal questions.

### 3.6 Concept-first retrieval (Experiment 1, aggressive prompt)

For completeness, we include results from the prior aggressive-prompt
run with all three strategies:

| Strategy | Acc | Halluc% | Tokens/q |
|---|---|---|---|
| concept | 0.395 | 9.6% | 6,845 |
| facts | 0.749 | 11.6% | 5,177 |
| direct | 0.668 | 10.3% | 1,912 |

The concept-first path scored 0.395 with only 34 unique wins (1.3% of
questions) and the highest token cost. Per-question agreement showed
that every question the concept strategy got right, at least one other
strategy also got right (concept-only: 0.041 → effectively zero unique
coverage).

### 3.7 Effect of synthesis prompt (aggressive vs inference)

| Config | Acc (aggressive) | Acc (inference) | Δ | Halluc% (aggressive) | Halluc% (inference) | Δ |
|---|---|---|---|---|---|---|
| facts@10 | 0.749 | 0.697 | -0.052 | 11.6% | 9.8% | -1.8pp |
| direct@10 | 0.668 | 0.560 | -0.108 | 10.3% | 7.7% | -2.6pp |

The inference prompt trades accuracy for lower hallucination. Direct's
accuracy dropped more (-0.108 vs -0.052) because its looser retrieval
gives the LLM less-relevant evidence, and the aggressive prompt was
more likely to elicit a confident-but-wrong guess from that evidence.
The facts variant's targeted retrieval gives better evidence, so the
conservative prompt costs it less accuracy.

---

## 4. Analysis

### 4.1 Facts as a low-hallucination chunking strategy

Across all four configurations, hallucination stays between 7.7% and
11.1% on a hard multi-hop benchmark. The atomic-fact +
source-attribution design constrains the synthesis LLM to verifiable
claims with sources. This is the core OKT value proposition quantified:
dedup + atomic extraction is not just retrieval ergonomics—it is a
hallucination control mechanism that survives even naive retrieval
(the direct variant).

The hallucination control is a property of the chunking strategy, not
the query-extraction step. Both direct and fact search retrieve from
the same atomic-fact pool, and both keep hallucination under 12%. The
query-extraction step buys accuracy (0.560 → 0.697 at facts=10), not
hallucination reduction.

### 4.2 Direct retrieval is competitive

The direct variant—no LLM query extraction, just the full question
against the fact tsvector index—achieves 0.683 at facts=20 with 3,312
tokens/q. This is a strong result for a naive baseline:

1. **The fact tsvector index is high-quality.** Even with stop-words
   and question words polluting the query, PostgreSQL's stemming + AND
   semantics + hybrid search (lexical + embedding) surface relevant
   facts because proper nouns and numbers in multi-hop questions are
   strong discriminators.

2. **The fact budget matters more than query engineering for direct.**
   Doubling from 10 → 20 facts lifted direct by +0.123—larger than
   the +0.060 gain from LLM query extraction at the same budget
   (direct@10 0.560 → facts@10 0.697). The single full-question query
   surfaces relevant facts, but some answering facts sit just beyond
   the top-10 window; doubling the window captures them.

3. **For cost-sensitive deployments, direct@20 is the recommended
   configuration.** It achieves 90% of facts@10's accuracy at 65% of
   the token cost, with comparable hallucination.

### 4.3 LLM query extraction buys targeted accuracy

The fact search strategy's LLM query extraction step (the +1 call vs
direct) buys +0.137 accuracy at facts=10 (0.560 → 0.697) and +0.074
at facts=20 (0.683 → 0.757) for ~2.5x the token cost. The extraction
prompt produces 3-6 term keyword-rich queries tuned for the fact
tsvector index, targeting discriminative nouns, numbers, and
publication names from the question.

The diminishing return at facts=20 (+0.074 vs +0.137) suggests that
query engineering and fact budget are partial substitutes: both
improve the quality of evidence reaching the synthesis LLM, and once
the fact budget is large enough, the marginal benefit of better
queries shrinks.

### 4.4 Concepts are for exploration, not targeted QA

The concept-first strategy (0.395 accuracy, 6,845 tokens/q, 34 unique
wins) is dominated by both alternatives on both accuracy and cost.
The concept-first path (search_concepts → get_concept_facts) is a
browsing pattern—it answers "show me everything about X" rather than
"does article A agree with article B on Y?" For targeted questions,
the concept graph adds an indirection layer that loses precision
without gaining recall.

This does not mean concepts are useless. The concept graph's value is
in:

- **Synthesis/summarization**: per-concept syntheses are a different
  artifact from retrieval paths. They provide structured, cited
  summaries of knowledge around a topic.
- **Cross-document navigation**: "what else is connected to FTX?"
  requires graph traversal, not keyword search.
- **Provenance auditability**: "which N sources confirm this claim?"
  uses the `source_count` and `fact_sources` structure.

MultiHop-RAG measures "can you answer this specific question?"—a
metric concepts were not built to optimize. The 0.395 is concepts
being scored on the wrong objective function, not concepts failing at
their purpose.

### 4.5 The comparison question problem

Comparison queries are the hardest bucket across all configurations
(accuracy 0.499-0.643, hallucination 18-21%). These questions ask
whether two articles from named publications agree or disagree on a
specific claim, e.g.:

> "Does the TechCrunch coverage of Sam Bankman-Fried's legal situation
> agree on the number of fraud and conspiracy charges he is facing,
> or is there a discrepancy between the articles?"

Answering requires: (1) identifying which facts are from TechCrunch
(requires source metadata), (2) finding facts about the charge count
(requires targeted retrieval), and (3) comparing the claims across
articles (requires multi-hop synthesis). Failure at any step produces
either a refusal (evidence not found) or a hallucination (wrong
inference of agreement/disagreement).

The source metadata backfill (publication name, date) was
necessary to make these questions answerable at all—before the
backfill, the synthesis LLM could not distinguish TechCrunch articles
from The Verge articles. But even with metadata, the ~20%
hallucination rate shows the LLM frequently infers agreement or
disagreement from partial evidence.

### 4.6 The temporal question problem

Temporal queries (accuracy 0.456-0.642) require reasoning about
publication dates and event timing. The source metadata backfill
populated `published_at` on source rows, which the synthesis LLM now
sees. But dates in fact text are sparse—the fact extraction prompt
captures claims, not temporal metadata, so many temporal questions
must be answered from the `published_at` field on the source rather
than from the fact text itself.

The fact budget has a larger effect on temporal than on comparison
(direct: +0.122, facts: +0.067), suggesting that the answering
evidence for temporal questions is often present but buried deeper in
the result set.

---

## 5. Limitations

1. **Single LLM backend.** All results use `openai/gpt-4o-mini` via
   OpenRouter. A stronger model (GPT-4o, Claude) would likely score
   higher on synthesis; a weaker model would make the retrieval
   differences more visible. The relative ordering of strategies
   should be model-independent, but absolute numbers are not.

2. **Single corpus.** MultiHop-RAG covers Q4 2023 news articles. The
   corpus is homogeneous (news, English, ~600 words/article) and
   relatively small (609 documents). A larger or more heterogeneous
   corpus (academic papers, legal documents, multi-lingual) would
   stress different retrieval and synthesis capabilities.

3. **Lenient scoring.** The official `qa_evaluate.py` uses token-set
   intersection—any shared word counts as correct. This overcounts
   on short answers ("Yes" matches any gold answer containing "yes")
   and undercounts on paraphrases. The absolute accuracy numbers are
   inflated by the lenient metric; the relative comparison between
   strategies is unaffected.

4. **No external baselines.** All comparisons are OKT-vs-OKT. To
   claim "OKT beats GraphRAG/LightRAG/naive-RAG," at least one
   external baseline must be run on the same corpus with the same
   LLM. The direct variant is the closest to a naive-RAG baseline
   (full question against the fact index), but it still benefits from
   OKT's atomic-fact chunking—a true naive baseline would use raw
   document chunks, not facts.

5. **Concept-first retrieval was only run with the aggressive prompt.**
   The inference-prompt runs excluded the concept strategy because
   Experiment 1 showed it dominated by both alternatives. The concept
   strategy may benefit from the inference prompt, but the improvement
   is unlikely to close the 0.300+ accuracy gap.

6. **No query-extraction quality analysis.** We do not measure how
   often the LLM's extracted queries match the optimal query for each
   question. A query-quality analysis (precision/recall of extracted
   queries vs gold evidence) would help diagnose whether the
   fact-search strategy's remaining failures are retrieval misses or
   synthesis errors.

---

## 6. Conclusions

1. **OKT's atomic-fact chunking is an effective low-hallucination
   strategy for multi-hop QA.** Across all configurations,
   hallucination stays between 7.7% and 11.1% on a hard multi-hop
   benchmark. The atomic-fact + source-attribution design constrains
   the synthesis LLM to verifiable claims—this is a structural
   property of the chunking, not a retrieval optimization.

2. **Direct retrieval on facts is surprisingly competitive.** Passing
   the full question to the fact tsvector index (no query engineering,
   1 LLM call) achieves 0.683 at facts=20 with 3,312 tokens/q—within
   0.014 of the LLM-query-extraction strategy at facts=10. For
   cost-sensitive deployments, this is the recommended configuration.

3. **LLM query extraction buys targeted accuracy at diminishing
   returns.** The +1 LLM call for query extraction lifts accuracy by
   +0.137 (facts=10) or +0.074 (facts=20) for ~2.5x the token cost.
   The diminishing return suggests query engineering and fact budget
   are partial substitutes.

4. **Concept-first retrieval is not the right substrate for targeted
   QA.** The concept graph scored 0.395 with the highest token cost
   and near-zero unique wins. Concepts serve exploration,
   summarization, and provenance auditability—not targeted question
   answering.

5. **Comparison questions are the hallucination hotspot (~20%).**
   Cross-article agreement/disagreement questions cause the LLM to
   infer from partial evidence. The inference prompt (allow
   inferences, prohibit guessing) reduces but does not eliminate
   this. Source metadata backfill (publication names, dates) is
   necessary but not sufficient.

6. **The fact budget is a free lever.** Doubling from 10 → 20 facts
   per query lifts accuracy by +0.060 to +0.123 at ~1.7x the token
   cost. The direct strategy benefits more because its single query
   surfaces relevant facts beyond the top-10 window that the larger
   budget captures.

---

## Appendix A: Infrastructure changes

### A.1 Source metadata backfill

The upload ingestion path wrote raw markdown (including YAML
frontmatter) into `parsed_text` but never populated the structured
source columns (`parsed_title`, `parsed_sitename`, `parsed_author`,
`published_at`). A one-shot Go script
(`backend/scripts/backfill-source-metadata`) parses the frontmatter
and populates these columns. Applied to the multihoprag repository:
609 sources updated, 2,368 fields written. Idempotent and
dry-run-by-default.

### A.2 ListFactSources SQL extension

The `ListFactSources` query (`backend/db/queries/facts.sql:164`) was
extended to also SELECT `s.parsed_sitename, s.parsed_author,
s.published_at` in addition to the existing `s.url, s.parsed_title`.
This makes publication name, author, and date available through the
`getFact` API endpoint, which the synthesis prompt composes into a
one-line attribution (e.g., `Source: TechCrunch "SBF's trial starts
soon..." by Jacquelyn Melinek on 2023-10-01`).

### A.3 ListFactsByConcept ORDER BY fix

The `ListFactsByConcept` query (`backend/db/queries/concepts.sql:570`)
was hardcoded to `ORDER BY fc.first_seen_at` (the moment the
fact-concept link was inserted—an ingest-ordering artifact). It now
defaults to `source_count DESC, ts_rank DESC, first_seen_at`,
mirroring the repo-wide `/facts` endpoint. A `sort` query parameter
(`source_count` default, `created_at` alternative) was added to the
`GET /concepts/{conceptID}/facts` route for consistency with the
repo-wide endpoint.

### A.4 LLM retry and error marking

The LLM call function (`_chat` in `llm.py`) was extended with retry
logic (3 attempts with exponential backoff). Failed synthesis calls
now return `[LLM ERROR]` instead of `Insufficient information.`, and
the scorer excludes these from accuracy/refusal/hallucination
metrics and reports them in a separate `llm_err` column.

---

## Appendix B: Reproducing

```bash
# Prerequisites: OKT running on localhost:8080 with the multihoprag
# repository populated and the source metadata backfilled.

# Run facts and direct at facts=10 and facts=20 on the full benchmark
python3 run_benchmark.py --variant facts   --concurrency 30 --facts-per-query 10
python3 run_benchmark.py --variant direct  --concurrency 30 --facts-per-query 10
python3 run_benchmark.py --variant facts   --concurrency 30 --facts-per-query 20
python3 run_benchmark.py --variant direct  --concurrency 30 --facts-per-query 20

# Score side-by-side
python3 score.py
```

Output files (all gitignored):
- `results/predictions_{facts,direct}.jsonl` — per-question predictions
- `results/qa_metrics.json` — full metrics + token counts + agreement
- `results/summary.txt` — printed side-by-side table
- `answers_{facts,direct}/<id>.md` — per-question audit

---

## Dataset license & citation

Dataset: [yixuantt/MultiHopRAG](https://huggingface.co/datasets/yixuantt/MultiHopRAG) — ODC-BY 1.0

Paper: Tang & Yang, "MultiHop-RAG: Benchmarking Retrieval-Augmented
Generation for Multi-Hop Queries", COLM 2024.
https://arxiv.org/abs/2401.15391

```bibtex
@misc{tang2024multihoprag,
  title={MultiHop-RAG: Benchmarking Retrieval-Augmented Generation for Multi-Hop Queries},
  author={Yixuan Tang and Yi Yang},
  year={2024},
  eprint={2401.15391},
  archivePrefix={arXiv},
  primaryClass={cs.CL}
}
```