"""Validate a predictions file for systemic correctness.

Catches failures that would silently invalidate a full run — the kind
we hit during this experiment:

  1. Point-id collision (71032 propositions → 2526 points) → evidence
     tokens near-zero for every row.
  2. Wrong Qdrant collection in resynth → evidence_tokens=7 (empty
     fetch) for every row.
  3. Silent extraction failures → a degenerate "always abstain" or
     "always [LLM ERROR]" result.
  4. Retrieval returning nothing → fact_ids_used / chunk_ids_used
     empty for most non-null questions.

Exit 0 = valid, 1 = WARN (some checks soft-failed), 2 = FAIL (do not
score this file — systemic failure detected).

CLI:
  python3 validate.py results/predictions_dense_x_10_<stamp>.jsonl
  python3 validate.py results/predictions_facts_10_<stamp>.jsonl --kind okt
"""
from __future__ import annotations

import argparse
import json
import os
import sys
from typing import Any


def _load(path: str) -> list[dict]:
    out: list[dict] = []
    with open(path, "r", encoding="utf-8") as fh:
        for line in fh:
            try:
                out.append(json.loads(line))
            except Exception:  # noqa: BLE001
                pass
    return out


def validate(
    preds: list[dict],
    kind: str = "auto",
    max_llm_error_rate: float = 0.05,
    min_evidence_tokens: int = 10,
    min_retrieval_rate: float = 0.90,
    min_diversity: float = 0.10,
) -> dict[str, Any]:
    """Run all validation checks. Returns a report dict.

    kind: 'okt' (fact_ids_used), 'baseline' (chunk_ids_used), or 'auto'
    (detect from the variant field). The retrieval check uses the
    right id field per kind.

    max_llm_error_rate: fail if >5% of rows are [LLM ERROR].
    min_evidence_tokens: warn if any row's evidence_tokens is below
        this (catches empty-fetch / wrong-collection). FAIL if the
        MEDIAN evidence_tokens is below this (systemic, not a few
        outliers).
    min_retrieval_rate: fail if <90% of non-null questions retrieved
        any evidence (fact_ids_used / chunk_ids_used non-empty).
    min_diversity: fail if <10% of predictions are unique (catches a
        degenerate always-abstain / always-same-answer failure).
    """
    n = len(preds)
    if n == 0:
        return {"status": "FAIL", "reason": "no predictions loaded"}

    # Detect kind from the first row's variant field.
    if kind == "auto":
        variant = (preds[0].get("variant") or "").lower()
        if variant in ("dense_x", "traditional"):
            kind = "baseline"
        else:
            kind = "okt"

    id_field = "chunk_ids_used" if kind == "baseline" else "fact_ids_used"

    # 1. LLM error rate.
    llm_errors = sum(1 for p in preds if p.get("prediction", "") == "[LLM ERROR]")
    llm_error_rate = llm_errors / n

    # 2. Evidence tokens.
    ev_tokens = [int(p.get("evidence_tokens", 0)) for p in preds]
    ev_sorted = sorted(ev_tokens)
    ev_median = ev_sorted[n // 2] if n else 0
    ev_zero = sum(1 for t in ev_tokens if t < min_evidence_tokens)
    ev_has_field = sum(1 for p in preds if "evidence_tokens" in p)

    # 3. Retrieval rate (non-null questions that retrieved something).
    null_count = 0
    retrieved = 0
    non_null = 0
    for p in preds:
        if p.get("question_type") == "null_query":
            null_count += 1
            continue
        non_null += 1
        ids = p.get(id_field, [])
        if ids:
            retrieved += 1
    retrieval_rate = retrieved / non_null if non_null else 1.0

    # 4. Prediction diversity (unique predictions / total).
    preds_set = {p.get("prediction", "") for p in preds}
    diversity = len(preds_set) / n if n else 0.0

    # 5. All-same-answer check (degenerate).
    most_common = max((sum(1 for p in preds if p.get("prediction") == x), x)
                      for x in preds_set) if preds_set else (0, "")
    most_common_rate = most_common[0] / n if n else 0.0

    report = {
        "kind": kind,
        "n": n,
        "llm_errors": llm_errors,
        "llm_error_rate": round(llm_error_rate, 4),
        "evidence_tokens": {
            "min": min(ev_tokens),
            "median": ev_median,
            "mean": round(sum(ev_tokens) / n) if n else 0,
            "max": max(ev_tokens),
            "below_threshold": ev_zero,
            "field_present": ev_has_field,
        },
        "retrieval": {
            "non_null": non_null,
            "retrieved": retrieved,
            "rate": round(retrieval_rate, 4),
        },
        "diversity": {
            "unique_predictions": len(preds_set),
            "rate": round(diversity, 4),
            "most_common_pred": most_common[1][:60],
            "most_common_rate": round(most_common_rate, 4),
        },
    }

    # Determine status.
    fails: list[str] = []
    warns: list[str] = []
    if llm_error_rate > max_llm_error_rate:
        fails.append(f"LLM error rate {llm_error_rate:.1%} > {max_llm_error_rate:.0%} "
                     f"({llm_errors}/{n} rows)")
    if ev_has_field > 0 and ev_median < min_evidence_tokens:
        fails.append(f"median evidence_tokens {ev_median} < {min_evidence_tokens} "
                     f"(systemic empty-fetch / wrong-collection)")
    elif ev_has_field > 0 and ev_zero > n * 0.05:
        warns.append(f"{ev_zero}/{n} rows have evidence_tokens < {min_evidence_tokens} "
                     f"(some empty fetches)")
    if non_null > 0 and retrieval_rate < min_retrieval_rate:
        fails.append(f"retrieval rate {retrieval_rate:.1%} < {min_retrieval_rate:.0%} "
                     f"({retrieved}/{non_null} non-null questions retrieved evidence)")
    if most_common_rate > 0.95 and most_common[1] not in ("Insufficient information.",):
        fails.append(f"degenerate: {most_common_rate:.1%} of predictions are "
                     f"'{most_common[1][:40]}'")
    if diversity < min_diversity:
        warns.append(f"low prediction diversity: {len(preds_set)} unique / {n} "
                     f"({diversity:.1%})")

    if fails:
        report["status"] = "FAIL"
        report["failures"] = fails
    elif warns:
        report["status"] = "WARN"
        report["warnings"] = warns
    else:
        report["status"] = "OK"
    return report


def _print_report(r: dict[str, Any]) -> None:
    print(f"  status: {r['status']}")
    print(f"  kind: {r['kind']}  n: {r['n']}")
    print(f"  LLM errors: {r['llm_errors']} ({r['llm_error_rate']:.1%})")
    ev = r["evidence_tokens"]
    print(f"  evidence_tokens: min={ev['min']} median={ev['median']} "
          f"mean={ev['mean']} max={ev['max']}  "
          f"(below_threshold={ev['below_threshold']}, field_present={ev['field_present']})")
    rv = r["retrieval"]
    print(f"  retrieval: {rv['retrieved']}/{rv['non_null']} non-null "
          f"({rv['rate']:.1%})")
    dv = r["diversity"]
    print(f"  diversity: {dv['unique_predictions']} unique preds "
          f"({dv['rate']:.1%}); most common: '{dv['most_common_pred']}' "
          f"({dv['most_common_rate']:.1%})")
    for f in r.get("failures", []):
        print(f"  FAIL: {f}")
    for w in r.get("warnings", []):
        print(f"  WARN: {w}")


def main() -> int:
    ap = argparse.ArgumentParser(description="Validate a predictions file")
    ap.add_argument("file", type=str, help="predictions .jsonl file")
    ap.add_argument("--kind", choices=["okt", "baseline", "auto"], default="auto")
    ap.add_argument("--max-llm-error-rate", type=float, default=0.05)
    ap.add_argument("--min-evidence-tokens", type=int, default=10)
    ap.add_argument("--min-retrieval-rate", type=float, default=0.90)
    args = ap.parse_args()

    if not os.path.exists(args.file):
        print(f"  file not found: {args.file}", file=sys.stderr)
        return 2

    preds = _load(args.file)
    print(f"  loaded {len(preds)} predictions from {args.file}")
    r = validate(preds, kind=args.kind,
                 max_llm_error_rate=args.max_llm_error_rate,
                 min_evidence_tokens=args.min_evidence_tokens,
                 min_retrieval_rate=args.min_retrieval_rate)
    _print_report(r)
    return {"OK": 0, "WARN": 1, "FAIL": 2}[r["status"]]


if __name__ == "__main__":
    raise SystemExit(main())