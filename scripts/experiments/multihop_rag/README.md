# MultiHop-RAG Benchmark (concept → fact path)

A fixed, deterministic Python pipeline that scores OKT's **standard
agentic retrieval path** — concept search → concept facts → synthesize —
on the [MultiHop-RAG](https://huggingface.co/datasets/yixuantt/MultiHopRAG)
benchmark.

No opencode, no agent loop, no embeddings. The pipeline is a fixed
sequence of REST calls against a running OKT instance, so the score
reflects OKT's retrieval quality on the concept → fact path, not an
LLM's luck at picking tools.

## What it measures

- **Headline metric:** QA accuracy (Precision / Recall / F1 / Accuracy
  — all equal to per-question success rate, matching the paper's
  definition) overall and per question type.
- **Question types:** `inference_query`, `comparison_query`,
  `temporal_query`, `null_query` (gold answer is `"Insufficient
  information."` — tests abstention).
- **Free diagnostic:** `source_coverage` — the fraction of questions
  for which the pipeline retrieved any source at all, broken down by
  question type. (A full retrieval-recall metric would need a gold-doc →
  source-id mapping; that's a future extension. The pipeline records
  `fact_ids_used`, `source_ids_used`, and `concept_ids_used` per
  prediction so you can do that analysis offline.)
- **Short answers:** the synthesis prompt enforces a short-answer
  contract (`The answer to the question is "..."`) for reliable
  extraction.

## Retrieval path (fixed)

Per question:

1. **Extract phrases** — 1 LLM call extracts 1–3 short noun phrases
   from the question.
2. **Pad to `NUM_CONCEPT_QUERIES`** (default 5) — reuses phrases
   cyclically if the extractor returns fewer.
3. **Search concepts** — `GET /api/v1/repositories/{slug}/concepts?q=<phrase>`
   for each phrase (lexical `ILIKE` on `canonical_name`). Merge all concept
   groups.
4. **Select top-N** — rank by `fact_count` desc, take `TOP_N_CONCEPTS`
   (default 5).
5. **Get facts per concept** — `GET /api/v1/repositories/{slug}/concepts/{id}/facts
   ?q=<question>&limit=<FACTS_PER_CONCEPT>` (default 10 — the standard
   page). Dedup by `fact_id` across concepts.
6. **Enrich facts** — `GET /api/v1/repositories/{slug}/facts/{factID}` for source
   URLs + linked concepts (metadata needed for multi-hop reasoning).
7. **Synthesize** — 1 LLM call with the question + gathered facts +
   source metadata → short answer.

**No fallback:** if concept search returns nothing for all phrases,
the question is answered with `"Insufficient information."` — the
honest score for the lexical concept path.

## Setup

### Prerequisites

- Python 3.10+
- `pip install -r requirements.txt`
- OKT running on `http://localhost:8080` (override with `OKT_BASE`)
- An OKT account with `fact:read` + `concept:read` permissions on the
  `multihoprag` repo
- An AI provider configured in OKT (Ollama or OpenRouter)

### 1. Download the dataset

```bash
python3 download_dataset.py
```

Writes:
- `dataset/corpus/*.md` — one markdown file per corpus article (title
  as filename, YAML metadata header, article body below).
- `dataset/queries.jsonl` — one JSON line per query:
  `{id, query, question_type, gold_answer}`.

### 2. Create the `multihoprag` repo in OKT

In the OKT UI, create a repository named `multihoprag` (the slug is
`multihoprag`). Use the default database.

### 3. Upload the corpus

Upload the 609 markdown files in `dataset/corpus/` to the `multihoprag`
repo via the OKT UI (Sources → Upload). Then wait for the ingest
pipeline to drain:

- `retrieve_source` → `source_decomposition` → `embed_facts` →
  `deduplicate_facts` → `extract_concepts` → `embed_concepts` →
  `summarize_concepts` → `synthesize_concept` →
  `refresh_concept_relations`

You can monitor progress on the Sources and Tasks pages. The concept
graph must exist before running the benchmark (the pipeline relies on
`searchConcepts`).

### 4. Get an OKT API token (data retrieval only)

Create a personal API key in the OKT UI (Profile → API Keys) with read
access to `concept` and `fact` on the `multihoprag` repo. The token only
needs data-retrieval scopes — LLM calls go directly to the AI provider,
not through OKT. Put it in `scripts/experiments/multihop_rag/.env`:

```bash
# scripts/experiments/multihop_rag/.env
token=okt_l-<your-personal-access-token>
```

### 5. Configure the LLM backend

The benchmark calls the LLM directly from the experiment logic (phrase
extraction + answer synthesis). The default backend is OpenRouter. Put
your OpenRouter key in the repo-root `.env`:

```bash
# .env (repo root)
OPENROUTER_API_KEY=sk-or-v1-<your-key>
```

Or export the variables before running:

```bash
export OPENROUTER_API_KEY="sk-or-v1-<your-key>"
export OPENROUTER_MODEL="openai/gpt-4o-mini"   # default
export LLM_BACKEND="openrouter"                 # default; or "okt"
```

To proxy LLM calls through OKT instead (requires the OKT token to carry
the `ai_provider:execute` scope), set `LLM_BACKEND=okt` and configure
`OKT_AI_PROVIDER` + `OKT_MODEL`.

## Run

```bash
# Smoke test — 20 random questions (quick end-to-end check)
python3 run_benchmark.py --sample 20

# First N questions
python3 run_benchmark.py --limit 50

# Specific ids
python3 run_benchmark.py --ids 3,5,42

# Filter by question type
python3 run_benchmark.py --question-type null_query --limit 20

# Sweep top-N concepts (one run per value, separate predictions files)
python3 run_benchmark.py --top-n 3,5,10

# Parallel (best-effort; OKT must handle concurrent requests)
python3 run_benchmark.py --concurrency 4
```

The runner is **resumable**: it skips question ids already present in
`results/predictions.jsonl`. Interrupt with Ctrl+C and re-run to pick
up where you left off.

Output files (all gitignored):
- `results/predictions.jsonl` — one row per question:
  ```json
  {"id":"0001","query":"...","question_type":"inference_query",
   "gold":"Sam Bankman-Fried","prediction":"Sam Bankman-Fried",
   "fact_ids_used":["uuid","uuid"],
   "source_ids_used":["uuid","uuid"],
   "concept_ids_used":["uuid","uuid"],
   "extracted_phrases":["ftx","alameda research"],
   "concept_queries":["ftx","alameda research","ftx","alameda research","ftx"],
   "concept_search_hits":[
     {"query":"ftx","count":3,"top_concepts":["FTX","Alameda Research","Bankruptcy"]},
     {"query":"alameda research","count":1,"top_concepts":["Alameda Research"]},
     {"query":"ftx","count":3,"top_concepts":["FTX","Alameda Research","Bankruptcy"]}
   ],
   "latency_ms":1234,
   "tokens":{"prompt":500,"completion":20},
   "params":{"top_n":5,"facts_per_concept":10,"num_concept_queries":5}}
  ```
  The `extracted_phrases` / `concept_queries` / `concept_search_hits` fields
  exist for troubleshooting: when a question scores wrong, they tell you
  whether retrieval missed the topic (`count: 0` across all queries → the
  lexical concept path didn't match the corpus's concept names) or the
  synthesis step failed (hits present, wrong answer). `concept_search_hits`
  caps `top_concepts` at 3 names per query so the row stays small.
- `answers/<id>.md` — full audit per question: retrieved facts with
  source metadata + raw LLM synthesis output.

## Score

```bash
python3 score.py
# or against a sweep file
python3 score.py --predictions-file results/predictions_topn10.jsonl
```

Prints a table and writes:
- `results/qa_metrics.json` — full metrics (overall + per type, plus
  source_coverage).
- `results/summary.txt` — the printed table.

Example output:

```
======================================================================
MultiHop-RAG QA Benchmark — Results
======================================================================

Overall (n=2556):
  Accuracy: 0.412
  Precision: 0.412
  Recall:    0.412
  F1:        0.412

By question_type:
  type                      n     acc    prec     rec      f1     cov
  comparison_query        510   0.351   0.351   0.351   0.351   0.880
  inference_query       1278   0.521   0.521   0.521   0.521   0.910
  null_query             252   0.310   0.310   0.310   0.310   0.050
  temporal_query         516   0.290   0.290   0.290   0.290   0.860

Source coverage (retrieved_any):
  overall: 0.790 (n=2556)
======================================================================
```

## Environment variables

| Variable | Default | Description |
|----------|---------|-------------|
| `OKT_BASE` | `http://localhost:8080` | OKT base URL |
| `OKT_TOKEN` | (required) | OKT personal API key (`okt_l-...`); data retrieval only |
| `OKT_REPO_SLUG` | `multihoprag` | the repo to query |
| `LLM_BACKEND` | `openrouter` | `openrouter` (direct) or `okt` (proxy through OKT) |
| `OPENROUTER_API_KEY` | (required if `LLM_BACKEND=openrouter`) | OpenRouter API key |
| `OPENROUTER_BASE` | `https://openrouter.ai/api/v1` | OpenRouter API base |
| `OPENROUTER_MODEL` | `openai/gpt-4o-mini` | model for phrase extraction + synthesis |
| `OPENROUTER_REFERER` | OKT GitHub URL | `HTTP-Referer` header for OpenRouter attribution |
| `OPENROUTER_TITLE` | `OKT MultiHop-RAG benchmark` | `X-Title` header for OpenRouter attribution |
| `OKT_AI_PROVIDER` | `ollama` | provider id for `/api/v1/ai/{provider}/chat` (only when `LLM_BACKEND=okt`) |
| `OKT_MODEL` | `gpt-4o-mini` | model name (only when `LLM_BACKEND=okt`) |
| `NUM_CONCEPT_QUERIES` | `5` | concept-search queries per question |
| `TOP_N_CONCEPTS` | `5` | concepts selected per question |
| `FACTS_PER_CONCEPT` | `10` | facts fetched per concept (page size) |
| `MAX_RETRIES` | `2` | HTTP retries on transient errors |
| `HTTP_TIMEOUT_S` | `30` | OKT REST timeout |
| `LLM_TIMEOUT_S` | `120` | OKT AI chat timeout |

CLI flags override env vars for the pipeline parameters (see
`python3 run_benchmark.py --help`).

## Files

```
multihop_rag/
├── README.md              this file
├── requirements.txt
├── download_dataset.py     downloads HF dataset → dataset/corpus/*.md + queries.jsonl
├── run_benchmark.py        the fixed concept→fact pipeline
├── score.py                qa_evaluate.py port + source_coverage diagnostic
├── okt.py                  thin REST client (concepts, facts, chat)
├── llm.py                  phrase extraction + answer synthesis (via OKT chat)
├── prompts.py              PHRASE_EXTRACTION_PROMPT + ANSWER_PROMPT
├── config.py               env-driven configuration
├── dataset/                gitignored — downloaded dataset
├── answers/                gitignored — per-question audit .md
└── results/                gitignored — predictions.jsonl + metrics
```

## Dataset license & citation

Dataset: [yixuantt/MultiHopRAG](https://huggingface.co/datasets/yixuantt/MultiHopRAG) — **ODC-BY 1.0**
(https://opendatacommons.org/licenses/by/1-0/)

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

## Scope / what's intentionally out

- **No backend changes.** Uses existing OKT REST endpoints for retrieval
  (`/api/v1/repositories/{slug}/concepts`, `/facts`). LLM calls go directly
  to the configured provider (default OpenRouter) from the experiment logic,
  not through OKT — the OKT token only needs data-retrieval scopes.
- **No opencode, no agent loop.** Fixed Python pipeline → reproducible.
- **No embeddings / no Qdrant calls.** Concept search is lexical
  (`ILIKE`); fact search is lexical (`tsvector`) — the standard
  concept-first path exposed via MCP.
- **No new justfile recipes.** Run via `python3 scripts/...` directly.
- **No fallback** when concept search returns nothing — abstention is
  the honest score for this path.
- **No full retrieval-recall metric.** `source_coverage` is a free
  sanity signal; a real recall@k would need gold-doc → source-id
  mapping (future work — the fact/source IDs are recorded per
  prediction to enable it).