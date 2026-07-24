"""Run the Dense X / Traditional RAG baselines against MultiHop-RAG.

Per question, per variant:

  DENSE_X_VARIANT  (propositions)
    1. embed the question text with google/gemini-embedding-2
    2. dense-only cosine search against multihoprag_propositions (limit=k)
    3. build the synthesis prompt from the retrieved propositions +
       their source metadata (title/source/author/published_at)
    4. synthesize_answer(question, facts) -> 1 LLM call, short answer

  TRADITIONAL_VARIANT  (passages)
    Same, but against multihoprag_passages.

Both variants use the SAME embedding model (gemini-embedding-2) and the
SAME synthesis LLM (gemma) as the OKT re-run, so the comparison isolates
chunking + retrieval strategy. Retrieval is dense-only (cosine, no
lexical tsvector, no RRF) — the defining characteristic of traditional
RAG / Dense X (vs OKT's hybrid lexical+semantic fusion).

The retrieved chunks are passed to the SHARED answer-synthesis prompt
(prompts.answer_user) by shaping them as "facts" with source metadata,
so the synthesis LLM sees the same prompt structure across all systems.
This keeps the synthesis step constant across OKT + baselines.

Resumable: skips ids already present in each variant's predictions file.

CLI:
  python3 baselines/run_baseline.py                         # both variants
  python3 baselines/run_baseline.py --variant dense_x      # only Dense X
  python3 baselines/run_baseline.py --variant traditional   # only Traditional
  python3 baselines/run_baseline.py --top-k 20              # chunks per query
  python3 baselines/run_baseline.py --concurrency 30        # parallel questions
  python3 baselines/run_baseline.py --sample 50             # smoke test
"""
from __future__ import annotations

import argparse
import json
import os
import random
import sys
import time
from concurrent.futures import ThreadPoolExecutor, as_completed
from typing import Any

# The baseline scripts live in baselines/ but import the top-level
# experiment modules (config, llm). Add the parent dir to sys.path so
# `python3 baselines/run_baseline.py` works from anywhere.
sys.path.insert(0, os.path.dirname(os.path.dirname(os.path.abspath(__file__))))

from tqdm import tqdm

import config
import llm
import prompts
from baselines import embeddings, qdrant_store


# Map variant -> (collection, label, answers subdir).
_VARIANT_MAP = {
    "dense_x": (config.BASELINE_PROPOSITION_COLLECTION, "dense_x", "answers_dense_x"),
    "traditional": (config.BASELINE_PASSAGE_COLLECTION, "traditional", "answers_traditional"),
}


# Timestamp for the output filenames. Computed once at process start so
# every variant in a single invocation shares the same stamp (a run that
# does both dense_x and traditional writes two files with the same stamp,
# making them easy to group as "this run"). Format: YYYYMMDD-HHMMSS.
_RUN_STAMP = time.strftime("%Y%m%d-%H%M%S")


# ---------------------------------------------------------------------------
# I/O helpers (resumable + timestamped, so runs never overwrite)
# ---------------------------------------------------------------------------
#
# Output filenames carry a timestamp so a re-run with the same variant + k
# produces a NEW file instead of clobbering the prior run:
#
#   results/predictions_dense_x_10_20260723-181030.jsonl
#   results/predictions_traditional_20_20260723-182015.jsonl
#
# Resumability (skipping already-done question ids) is UNION-based across
# ALL prior timestamped files for that variant+k: _load_done_ids globs
# `predictions_<variant>_<k>_*.jsonl`, reads every match, and unions their
# ids. So if a prior run (any timestamp) already answered question 0026,
# the new run skips it — even though the new run writes to a fresh file.
# This gives the safety of never-overwrite AND the convenience of resume.
# ---------------------------------------------------------------------------


def _load_queries() -> list[dict]:
    if not os.path.exists(config.QUERIES_PATH):
        print(
            f"  queries file not found: {config.QUERIES_PATH}\n"
            "  run `python3 download_dataset.py` first.",
            file=sys.stderr,
        )
        sys.exit(2)
    out: list[dict] = []
    with open(config.QUERIES_PATH, "r", encoding="utf-8") as fh:
        for line in fh:
            try:
                out.append(json.loads(line))
            except Exception:  # noqa: BLE001
                pass
    return out


def _glob_predictions(variant_label: str, top_k: int) -> list[str]:
    """All prior timestamped predictions files for this variant + k,
    sorted newest-first by mtime. Includes the non-timestamped legacy
    name `predictions_<label>.jsonl` if it exists (backward compat with
    files written before timestamps were added).
    """
    import glob

    base = os.path.join(config.RESULTS_DIR, "predictions")
    patterns = [
        f"{base}_{variant_label}_{top_k}_*.jsonl",  # timestamped (current)
        f"{base}_{variant_label}.jsonl",             # legacy (pre-timestamp)
    ]
    hits: list[str] = []
    for pat in patterns:
        hits.extend(glob.glob(pat))
    # Dedup + newest-first by mtime.
    hits = sorted(set(hits), key=lambda p: os.path.getmtime(p), reverse=True)
    return hits


def _load_done_ids(variant_label: str, top_k: int) -> set[str]:
    """Union of done ids across ALL prior runs for this variant + k.

    A question is considered done if ANY prior timestamped file (or the
    legacy non-timestamped file) contains it. This makes re-runs resume
    across timestamps without overwriting.
    """
    done: set[str] = set()
    for path in _glob_predictions(variant_label, top_k):
        with open(path, "r", encoding="utf-8") as fh:
            for line in fh:
                try:
                    done.add(json.loads(line)["id"])
                except Exception:  # noqa: BLE001
                    pass
    return done


def _predictions_path_for(variant_label: str, top_k: int) -> str:
    """The (timestamped) output file for THIS run.

    One file per (variant, k, run-start-time). All appends in this run
    go here. The timestamp is fixed at process start (_RUN_STAMP) so a
    run that does both variants writes two files with the same stamp.
    """
    base = os.path.join(config.RESULTS_DIR, "predictions")
    return f"{base}_{variant_label}_{top_k}_{_RUN_STAMP}.jsonl"


def _append_prediction(pred: dict, path: str) -> None:
    os.makedirs(os.path.dirname(path), exist_ok=True)
    with open(path, "a", encoding="utf-8") as fh:
        fh.write(json.dumps(pred, ensure_ascii=False) + "\n")
        fh.flush()


def _write_answer_audit(
    qid: str,
    question: str,
    chunks: list[dict],
    synthesis: dict,
    variant: str,
    top_k: int,
) -> None:
    """Write the per-question audit .md. The audit dir is timestamped
    (variant_<k>_<stamp>) so a re-run doesn't overwrite prior audits —
    each run's audits are grouped in their own subdir.
    """
    _, label, sub = _VARIANT_MAP[variant]
    out_dir = os.path.join(
        os.path.dirname(__file__), "..", f"{sub}_{top_k}_{_RUN_STAMP}"
    )
    os.makedirs(out_dir, exist_ok=True)
    path = os.path.join(out_dir, f"{qid}.md")
    lines = [f"# Question {qid} [{variant}]", "", question, ""]
    lines.append(f"## Retrieved chunks ({len(chunks)})")
    for i, c in enumerate(chunks, 1):
        lines.append(f"\n### Chunk {i}: {c.get('doc_id','')}#{c.get('chunk_index','')} (score={c.get('score',0):.3f})")
        lines.append(c.get("text", "").strip())
        site = (c.get("source") or "").strip()
        title = (c.get("title") or "").strip()
        author = (c.get("author") or "").strip()
        pub = c.get("published_at") or ""
        bits = []
        if site:
            bits.append(site)
        if title and title != site:
            bits.append(f'"{title}"')
        if author:
            bits.append(f"by {author}")
        if pub:
            bits.append(f"on {pub}")
        if bits:
            lines.append(f"  - source: {' '.join(bits)}")
    lines.append("\n## Synthesis (raw LLM output)")
    lines.append(synthesis.get("raw", ""))
    lines.append("")
    with open(path, "w", encoding="utf-8") as fh:
        fh.write("\n".join(lines))


# ---------------------------------------------------------------------------
# Retrieval + synthesis
# ---------------------------------------------------------------------------


def _chunks_to_facts(chunks: list[dict[str, Any]]) -> list[dict[str, Any]]:
    """Shape retrieved chunks into the `facts` schema prompts.answer_user
    expects: {id, text, sources, concepts}.

    Each chunk carries its source metadata in the payload; we compose a
    one-line attribution the same way prompts.answer_user does, so the
    synthesis LLM sees identical prompt structure across OKT + baselines.
    """
    out: list[dict[str, Any]] = []
    for c in chunks:
        src = {
            "parsed_sitename": c.get("source", ""),
            "parsed_title": c.get("title", ""),
            "parsed_author": c.get("author", ""),
            "published_at": c.get("published_at", ""),
            "url": c.get("doc_id", ""),
        }
        out.append(
            {
                "id": c.get("id", c.get("doc_id", "")),
                "text": c.get("text", ""),
                "sources": [src],
                "concepts": [],
            }
        )
    return out


def run_variant(
    q: dict,
    variant: str,
    top_k: int,
    predictions_path: str,
) -> dict[str, Any]:
    """Run one baseline variant for one question."""
    collection, label, _ = _VARIANT_MAP[variant]
    qid = q["id"]
    question = q["query"]
    started = time.time()
    total_usage: dict[str, int] = {"prompt": 0, "completion": 0}

    # 1. Embed the question with the same model the index uses.
    try:
        qvec = embeddings.embed_one(question)
    except Exception as e:  # noqa: BLE001
        print(f"  {qid}: embed failed: {e}", file=sys.stderr)
        pred = _base_pred(q, variant, question, started, total_usage, [], [],
                          "[LLM ERROR]", f"[embed failed: {e}]")
        _append_prediction(pred, predictions_path)
        return pred

    # 2. Dense-only cosine search.
    try:
        hits = qdrant_store.search(
            collection=collection,
            query_vector=qvec,
            limit=top_k,
            min_score=config.BASELINE_MIN_SCORE,
        )
    except qdrant_store.QdrantError as e:
        print(f"  {qid}: qdrant search failed: {e}", file=sys.stderr)
        hits = []

    # 3. Shape chunks into the shared synthesis prompt's fact schema.
    facts = _chunks_to_facts(hits)
    source_ids_used = [h.get("doc_id", "") for h in hits if h.get("doc_id")]
    chunk_ids_used = [str(h.get("id", "")) for h in hits if h.get("id")]

    # 4. Synthesize with the shared prompt + LLM.
    ev_tokens = prompts.evidence_tokens(facts)
    synthesis = llm.synthesize_answer(question, facts)
    total_usage = llm._add_usage(total_usage, synthesis["usage"])

    pred = {
        "id": qid,
        "variant": label,
        "query": question,
        "question_type": q.get("question_type", ""),
        "gold": q.get("gold_answer", ""),
        "evidence_list": q.get("evidence_list", []),
        "prediction": synthesis["answer"],
        "evidence_tokens": ev_tokens,
        # For recall@k: the source articles (doc_ids) the baseline retrieved.
        "source_ids_used": source_ids_used,
        "chunk_ids_used": chunk_ids_used,
        # Record the retrieval scores so score.py can plot score distributions.
        "retrieval_scores": [round(h.get("score", 0.0), 4) for h in hits],
        "concept_ids_used": [],
        "retrieval_hits": [
            {
                "doc_id": h.get("doc_id", ""),
                "chunk_index": h.get("chunk_index", 0),
                "score": round(h.get("score", 0.0), 4),
                "title": h.get("title", ""),
                "source": h.get("source", ""),
                "published_at": h.get("published_at", ""),
            }
            for h in hits
        ],
        "latency_ms": int((time.time() - started) * 1000),
        "tokens": total_usage,
        "llm_calls": {
            "synthesis": synthesis["usage"],
        },
        "params": {
            "top_k": top_k,
            "collection": collection,
            "retrieval": "dense-only",
            "embedding_model": config.BASELINE_EMBEDDING_MODEL,
        },
    }
    _append_prediction(pred, predictions_path)
    _write_answer_audit(qid, question, hits, synthesis, variant, top_k)
    return pred


def _base_pred(
    q: dict,
    variant: str,
    question: str,
    started: float,
    total_usage: dict[str, int],
    source_ids: list[str],
    chunk_ids: list[str],
    answer: str,
    raw: str,
) -> dict[str, Any]:
    _, label, _ = _VARIANT_MAP[variant]
    return {
        "id": q["id"],
        "variant": label,
        "query": question,
        "question_type": q.get("question_type", ""),
        "gold": q.get("gold_answer", ""),
        "evidence_list": q.get("evidence_list", []),
        "prediction": answer,
        "source_ids_used": source_ids,
        "chunk_ids_used": chunk_ids,
        "retrieval_scores": [],
        "concept_ids_used": [],
        "retrieval_hits": [],
        "latency_ms": int((time.time() - started) * 1000),
        "tokens": total_usage,
        "llm_calls": {},
        "params": {"variant": variant, "retrieval": "dense-only"},
        "raw_synthesis": raw,
    }


# ---------------------------------------------------------------------------
# CLI
# ---------------------------------------------------------------------------


def _parse_args() -> argparse.Namespace:
    ap = argparse.ArgumentParser(
        description="Run Dense X / Traditional RAG baselines on MultiHop-RAG"
    )
    ap.add_argument("--limit", type=int, default=0, help="first N questions (0=all)")
    ap.add_argument("--sample", type=int, default=0, help="N random questions (0=off)")
    ap.add_argument("--ids", type=str, default="", help="comma-separated ids")
    ap.add_argument("--question-type", type=str, default="")
    ap.add_argument(
        "--variant",
        type=str,
        default="all",
        choices=["all", "dense_x", "traditional"],
    )
    ap.add_argument(
        "--top-k",
        type=int,
        default=10,
        help="chunks/facts retrieved per question (matches OKT facts-per-query)",
    )
    ap.add_argument("--concurrency", type=int, default=1)
    ap.add_argument("--no-smoke", action="store_true",
                    help="skip the 5-question smoke-test gate before the full run")
    ap.add_argument("--seed", type=int, default=42)
    return ap.parse_args()


def _select_queries(queries: list[dict], args) -> list[dict]:
    selected = queries
    if args.question_type:
        selected = [q for q in selected if q.get("question_type") == args.question_type]
    if args.ids:
        wanted = {x.strip() for x in args.ids.split(",") if x.strip()}
        selected = [q for q in selected if q["id"] in wanted]
    if args.sample:
        rng = random.Random(args.seed)
        selected = rng.sample(selected, min(args.sample, len(selected)))
    elif args.limit:
        selected = selected[: args.limit]
    return selected


def _smoke_test(
    variant: str, top_k: int, queries: list[dict], n: int = 5
) -> bool:
    """Run N questions end-to-end and validate before the full run.

    Catches systemic failures (wrong collection, empty retrieval,
    evidence_tokens near-zero) BEFORE committing to 2556 questions.
    Returns True if the smoke test passed.
    """
    import validate as _v
    print(f"\n[{variant}] smoke test: {n} questions...")
    smoke_path = os.path.join(
        config.RESULTS_DIR, f"_smoke_{variant}_{top_k}.jsonl"
    )
    os.makedirs(os.path.dirname(smoke_path), exist_ok=True)
    if os.path.exists(smoke_path):
        os.remove(smoke_path)
    smoke_queries = queries[:n]
    for q in smoke_queries:
        try:
            run_variant(q, variant, top_k, smoke_path)
        except Exception as e:  # noqa: BLE001
            print(f"  smoke {q['id']}: error: {e}", file=sys.stderr)
    preds = []
    with open(smoke_path, "r", encoding="utf-8") as fh:
        for line in fh:
            try:
                preds.append(json.loads(line))
            except Exception:  # noqa: BLE001
                pass
    os.remove(smoke_path)
    if not preds:
        print(f"  SMOKE FAIL: no predictions produced", file=sys.stderr)
        return False
    r = _v.validate(preds, kind="baseline")
    _v._print_report(r)
    if r["status"] == "FAIL":
        print(f"  SMOKE FAIL: systemic failure detected — aborting full run.",
              file=sys.stderr)
        return False
    # Clean up the smoke-test audit dir.
    _, _, sub = _VARIANT_MAP[variant]
    import shutil
    smoke_audit = os.path.join(
        os.path.dirname(__file__), "..", f"{sub}_{top_k}_{_RUN_STAMP}"
    )
    if os.path.isdir(smoke_audit):
        shutil.rmtree(smoke_audit, ignore_errors=True)
    print(f"  smoke OK — proceeding with full run")
    return True


def main() -> int:
    args = _parse_args()
    queries = _load_queries()
    print(f"  loaded {len(queries)} queries from {config.QUERIES_PATH}")
    selected = _select_queries(queries, args)

    variants = (
        ["dense_x", "traditional"] if args.variant == "all" else [args.variant]
    )

    for variant in variants:
        _, label, _ = _VARIANT_MAP[variant]
        predictions_path = _predictions_path_for(label, args.top_k)
        # Resume across ALL prior timestamped files for this variant+k.
        done = _load_done_ids(label, args.top_k)
        todo = [q for q in selected if q["id"] not in done]
        prior_files = _glob_predictions(label, args.top_k)
        print(
            f"\n[{variant}] {len(todo)} to run "
            f"({len(selected) - len(todo)} already done across "
            f"{len(prior_files)} prior file(s))"
        )
        print(f"  writing to: {predictions_path}")
        if not todo:
            continue

        # Smoke test: run 5 questions and validate before the full run.
        # Catches wrong-collection / empty-retrieval / evidence_tokens=0
        # before wasting a full 2556-question run. Skip with --no-smoke.
        if not args.no_smoke and len(todo) > 5:
            if not _smoke_test(variant, args.top_k, todo):
                return 2
            # Re-read done ids (smoke test wrote to a throwaway file, not the
            # real path, so todo is unchanged).
            done = _load_done_ids(label, args.top_k)
            todo = [q for q in selected if q["id"] not in done]

        if args.concurrency > 1:
            with ThreadPoolExecutor(max_workers=args.concurrency) as ex:
                futures = {
                    ex.submit(run_variant, q, variant, args.top_k, predictions_path): q
                    for q in todo
                }
                for fut in tqdm(as_completed(futures), total=len(futures), desc=variant):
                    try:
                        fut.result()
                    except Exception as e:  # noqa: BLE001
                        qid = futures[fut]["id"]
                        print(f"  {qid}: pipeline error: {e}", file=sys.stderr)
        else:
            for q in tqdm(todo, desc=variant):
                try:
                    run_variant(q, variant, args.top_k, predictions_path)
                except Exception as e:  # noqa: BLE001
                    print(f"  {q['id']}: pipeline error: {e}", file=sys.stderr)
        print(f"  Predictions: {predictions_path}")
        print(
            f"  Score with: python3 score.py --predictions-file {predictions_path}"
        )

    print("\nNext: python3 score.py")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())