"""Re-synthesize predictions from already-retrieved evidence.

When only the synthesis prompt changes (not the retrieval), we don't need
to re-run the full pipeline. This script:

  1. Reads an existing predictions file (OKT or baseline variant).
  2. For each prediction, re-fetches the retrieved evidence by ID:
     - OKT variants (facts/direct): okt.get_fact(fact_id) for each fact_id.
     - Baseline variants (dense_x/traditional): Qdrant retrieve by point id.
  3. Re-runs synthesize_answer with the CURRENT prompt (e.g. aggressive).
  4. Writes a new timestamped predictions file with updated predictions
     + evidence_tokens (the retrieved-evidence token count).

This is cheap: no embedding calls, no retrieval, no OKT search — just the
synthesis LLM call (1 call/q) + cheap ID-based re-fetches.

CLI:
  python3 resynthesize.py results/predictions_direct_10.jsonl --variant okt
  python3 resynthesize.py results/predictions_dense_x_10_<stamp>.jsonl --variant baseline
  python3 resynthesize.py results/predictions_traditional_10_<stamp>.jsonl --variant baseline
"""
from __future__ import annotations

import argparse
import json
import os
import sys
import time
from concurrent.futures import ThreadPoolExecutor, as_completed
from typing import Any

from tqdm import tqdm

import config
import llm
import prompts
import okt
from baselines import qdrant_store


def _load_predictions(path: str) -> list[dict]:
    out: list[dict] = []
    with open(path, "r", encoding="utf-8") as fh:
        for line in fh:
            try:
                out.append(json.loads(line))
            except Exception:  # noqa: BLE001
                pass
    return out


def _refetch_okt_facts(fact_ids: list[str]) -> list[dict]:
    """Re-fetch facts from OKT by fact_id. Returns the enriched fact
    dicts that prompts.answer_user expects: {id, text, sources, concepts}.
    """
    out: list[dict] = []
    for fid in fact_ids:
        if not fid:
            continue
        try:
            detail = okt.get_fact(fid)
        except okt.OKTError as e:
            print(f"    fact {fid}: get_fact failed: {e}", file=sys.stderr)
            out.append({"id": fid, "text": "", "sources": [], "concepts": []})
            continue
        sources = detail.get("sources") or []
        concepts = detail.get("concepts") or []
        text = detail.get("fact", {}).get("text", "") or ""
        out.append({"id": fid, "text": text, "sources": sources, "concepts": concepts})
    return out


def _refetch_baseline_chunks(point_ids: list[str]) -> list[dict]:
    """Re-fetch chunks from Qdrant by point id. Returns the fact dicts
    that prompts.answer_user expects: {id, text, sources, concepts}.
    """
    if not point_ids:
        return []
    c = qdrant_store._client()
    out: list[dict] = []
    # Qdrant retrieve by id (batch of up to 100).
    for start in range(0, len(point_ids), 100):
        batch = point_ids[start : start + 100]
        try:
            points = c.retrieve(
                collection_name=config.BASELINE_PROPOSITION_COLLECTION
                if "dense_x" in str(point_ids) else config.BASELINE_PASSAGE_COLLECTION,
                ids=batch,
                with_payload=True,
                with_vectors=False,
            )
        except Exception:  # noqa: BLE001
            # Try the other collection if the first failed.
            try:
                points = c.retrieve(
                    collection_name=config.BASELINE_PASSAGE_COLLECTION,
                    ids=batch,
                    with_payload=True,
                    with_vectors=False,
                )
            except Exception as e:  # noqa: BLE101
                print(f"    qdrant retrieve failed: {e}", file=sys.stderr)
                points = []
        for p in points:
            payload = p.payload or {}
            src = {
                "parsed_sitename": payload.get("source", ""),
                "parsed_title": payload.get("title", ""),
                "parsed_author": payload.get("author", ""),
                "published_at": payload.get("published_at", ""),
                "url": payload.get("doc_id", ""),
            }
            out.append(
                {
                    "id": str(p.id),
                    "text": payload.get("text", ""),
                    "sources": [src],
                    "concepts": [],
                }
            )
    return out


def _resynthesize_one(pred: dict, variant_kind: str) -> dict:
    """Re-fetch evidence + re-synthesize for one prediction row.
    Returns the updated prediction row.
    """
    qid = pred["id"]
    question = pred["query"]
    started = time.time()

    # Re-fetch the evidence by ID.
    if variant_kind == "okt":
        fact_ids = pred.get("fact_ids_used") or []
        facts = _refetch_okt_facts(fact_ids)
    else:  # baseline
        point_ids = pred.get("chunk_ids_used") or []
        facts = _refetch_baseline_chunks(point_ids)

    # Compute evidence tokens (the retrieved-evidence token budget).
    ev_tokens = prompts.evidence_tokens(facts)

    # Re-synthesize with the current (aggressive) prompt.
    synthesis = llm.synthesize_answer(question, facts)

    # Update the prediction row.
    pred = dict(pred)  # shallow copy
    pred["prediction"] = synthesis["answer"]
    pred["evidence_tokens"] = ev_tokens
    pred["resynthesized"] = True
    pred["resynth_synthesis_usage"] = synthesis["usage"]
    pred["latency_ms"] = int((time.time() - started) * 1000)
    # Preserve the original tokens (retrieval cost) but update synthesis.
    if "llm_calls" in pred and "synthesis" in pred["llm_calls"]:
        pred["llm_calls"]["synthesis"] = synthesis["usage"]
    pred["tokens"] = llm._add_usage(
        {"prompt": 0, "completion": 0}, synthesis["usage"]
    )
    return pred


def main() -> int:
    ap = argparse.ArgumentParser(description="Re-synthesize predictions with the current prompt")
    ap.add_argument("input", type=str, help="input predictions file")
    ap.add_argument(
        "--variant",
        type=str,
        required=True,
        choices=["okt", "baseline"],
        help="okt = re-fetch facts from OKT by fact_id; baseline = re-fetch chunks from Qdrant by point id",
    )
    ap.add_argument("--concurrency", type=int, default=30)
    ap.add_argument(
        "--output",
        type=str,
        default="",
        help="output file (default: <input>_resynth_<stamp>.jsonl)",
    )
    args = ap.parse_args()

    if not os.path.exists(args.input):
        print(f"  input not found: {args.input}", file=sys.stderr)
        return 2

    preds = _load_predictions(args.input)
    print(f"  loaded {len(preds)} predictions from {args.input}")

    stamp = time.strftime("%Y%m%d-%H%M%S")
    if args.output:
        out_path = args.output
    else:
        base, ext = os.path.splitext(args.input)
        out_path = f"{base}_resynth_{stamp}.jsonl"
    os.makedirs(os.path.dirname(out_path) or ".", exist_ok=True)

    # Write incrementally.
    done_ids: set[str] = set()
    if os.path.exists(out_path):
        with open(out_path, "r", encoding="utf-8") as fh:
            for line in fh:
                try:
                    done_ids.add(json.loads(line)["id"])
                except Exception:  # noqa: BLE001
                    pass
    todo = [p for p in preds if p["id"] not in done_ids]
    print(f"  {len(todo)} to re-synthesize ({len(done_ids)} already done in {out_path})")

    def _one(pred: dict) -> dict:
        try:
            return _resynthesize_one(pred, args.variant)
        except Exception as e:  # noqa: BLE001
            print(f"  {pred['id']}: resynth error: {e}", file=sys.stderr)
            pred = dict(pred)
            pred["prediction"] = "[LLM ERROR]"
            pred["resynthesized"] = False
            pred["resynth_error"] = str(e)
            return pred

    with open(out_path, "a", encoding="utf-8") as fh:
        if args.concurrency > 1:
            with ThreadPoolExecutor(max_workers=args.concurrency) as ex:
                futures = {ex.submit(_one, p): p for p in todo}
                for fut in tqdm(as_completed(futures), total=len(futures), desc="resynth"):
                    out = fut.result()
                    fh.write(json.dumps(out, ensure_ascii=False) + "\n")
                    fh.flush()
        else:
            for p in tqdm(todo, desc="resynth"):
                out = _one(p)
                fh.write(json.dumps(out, ensure_ascii=False) + "\n")
                fh.flush()

    print(f"  Output: {out_path}")
    print(f"  Score with: python3 score.py --predictions-file {out_path}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())