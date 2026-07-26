"""Score the KGQA head-to-head predictions per hop depth.

Reads predictions_{variant}.jsonl for each variant and scores them against
gold answers, broken down by question_type (1-hop, 2-hop, 3-hop proxy via
the MultiHop-RAG question_type field).

Scoring mirrors the existing multihop_rag/score.py:
  - success = any lowercase token overlap between prediction and gold
  - refusal = "Insufficient information."
  - hallucinated = substantive AND wrong
"""
from __future__ import annotations

import json
import os
from pathlib import Path
from collections import defaultdict

PREDICTIONS_DIR = Path(__file__).resolve().parent / "results" / "predictions"
RESULTS_DIR = Path(__file__).resolve().parent / "results"

VARIANTS = ["triplet_kg", "concept_walk", "concept_definitions", "facts_direct"]
ABSTENTION = "insufficient information."


def has_intersection(pred: str, gold: str) -> bool:
    if not pred or not gold:
        return False
    pred_tokens = set(pred.lower().split())
    gold_tokens = set(gold.lower().split())
    return bool(pred_tokens & gold_tokens)


def is_refusal(pred: str) -> bool:
    if not pred:
        return True
    return pred.strip().lower().rstrip(".") == ABSTENTION.rstrip(".")


def score_variant(variant: str) -> dict:
    path = PREDICTIONS_DIR / f"predictions_{variant}.jsonl"
    if not path.exists():
        return {"variant": variant, "status": "no predictions file"}

    rows = []
    for line in path.read_text().splitlines():
        if line.strip():
            rows.append(json.loads(line))

    by_type = defaultdict(list)
    for row in rows:
        by_type[row.get("question_type", "unknown")].append(row)

    results = {
        "variant": variant,
        "n_questions": len(rows),
        "overall": {},
        "by_question_type": {},
    }

    # Overall.
    correct = sum(1 for r in rows if has_intersection(r.get("prediction", ""), r.get("gold", "")))
    refusals = sum(1 for r in rows if is_refusal(r.get("prediction", "")))
    substantive = len(rows) - refusals
    hallucinated = substantive - correct
    results["overall"] = {
        "n": len(rows),
        "correct": correct,
        "accuracy": round(correct / max(len(rows), 1), 4),
        "refusals": refusals,
        "refusal_rate": round(refusals / max(len(rows), 1), 4),
        "hallucinated": hallucinated,
        "hallucination_rate": round(hallucinated / max(len(rows), 1), 4),
    }

    # Per question type.
    for qtype, type_rows in sorted(by_type.items()):
        correct = sum(1 for r in type_rows if has_intersection(r.get("prediction", ""), r.get("gold", "")))
        refusals = sum(1 for r in type_rows if is_refusal(r.get("prediction", "")))
        substantive = len(type_rows) - refusals
        hallucinated = substantive - correct
        results["by_question_type"][qtype] = {
            "n": len(type_rows),
            "correct": correct,
            "accuracy": round(correct / max(len(type_rows), 1), 4),
            "refusals": refusals,
            "refusal_rate": round(refusals / max(len(type_rows), 1), 4),
            "hallucinated": hallucinated,
            "hallucination_rate": round(hallucinated / max(len(type_rows), 1), 4),
        }

    return results


def main() -> None:
    all_results = {}
    for variant in VARIANTS:
        all_results[variant] = score_variant(variant)

    out = RESULTS_DIR / "kgqa_headtohead.json"
    out.write_text(json.dumps(all_results, indent=2))

    # Print side-by-side table.
    print(f"\n{'='*70}")
    print("KGQA Head-to-Head — Overall Results")
    print(f"{'='*70}")
    print(f"{'Variant':<18} {'N':>5} {'Acc':>7} {'Refuse':>7} {'Halluc':>7}")
    print("-" * 50)
    for variant in VARIANTS:
        r = all_results[variant]
        if "status" in r:
            print(f"{variant:<18} {r['status']}")
            continue
        o = r["overall"]
        print(f"{variant:<18} {o['n']:>5} {o['accuracy']:>6.1%} {o['refusal_rate']:>6.1%} {o['hallucination_rate']:>6.1%}")

    print(f"\n{'='*70}")
    print("Per Question Type")
    print(f"{'='*70}")
    all_types = sorted(set(
        qt for v in all_results.values()
        if "by_question_type" in v
        for qt in v["by_question_type"]
    ))
    for qtype in all_types:
        print(f"\n  {qtype}:")
        print(f"  {'Variant':<18} {'N':>5} {'Acc':>7} {'Refuse':>7} {'Halluc':>7}")
        print(f"  {'-'*48}")
        for variant in VARIANTS:
            r = all_results[variant]
            if "by_question_type" not in r or qtype not in r["by_question_type"]:
                continue
            t = r["by_question_type"][qtype]
            print(f"  {variant:<18} {t['n']:>5} {t['accuracy']:>6.1%} {t['refusal_rate']:>6.1%} {t['hallucination_rate']:>6.1%}")

    print(f"\nWrote {out}")


if __name__ == "__main__":
    main()