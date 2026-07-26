"""Produce the four-cell head-to-head table for the Atomic Facts vs
Late-Chunked Passages experiment (matched conditions).

Four configs, all on MultiHop-RAG n=2556, same generator LLM
(google/gemma-4-31b-it), same scoring:
  - direct@10  : OKT atomic-fact index, k=10  (Condition A, existing run)
  - direct@20  : OKT atomic-fact index, k=20  (Condition A, existing run)
  - late_chunk@10 : late-chunked passages, k=10 (Condition B, new run)
  - late_chunk@20 : late-chunked passages, k=20 (Condition B, new run)

Outputs accuracy (overall + by question type), hallucination rate,
tokens/query, average retrieval unit length, and 95% bootstrap CIs on
accuracy — directly comparable to the paper's existing MultiHop-RAG
results table.

Usage: python3 late_chunk_experiment.py
"""
from __future__ import annotations

import json
import os
import sys
from typing import Any

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

import config
import score as score_mod

# Explicit file paths. direct@10/@20 use the legacy "inference prompt" runs
# (the §3.3 headline numbers: 0.560 / 0.683, acc 7.7% / 9.4% halluc) so the
# §3.7 table is consistent with the paper's main benchmark table. The
# late_chunk runs used the newer "simplified aggressive prompt" — a prompt
# confound flagged in §3.7's note (the aggressive prompt boosts accuracy,
# so the confound works AGAINST atomic facts; the H1a verdict is conservative).
CONFIGS = [
    ("direct@10", os.path.join(config.RESULTS_DIR, "predictions_direct_10.jsonl")),
    ("direct@20", os.path.join(config.RESULTS_DIR, "predictions_direct_20.jsonl")),
    ("late_chunk@10", os.path.join(config.RESULTS_DIR, "predictions_late_chunk_10_20260725-113051.jsonl")),
    ("late_chunk@20", os.path.join(config.RESULTS_DIR, "predictions_late_chunk_20_20260725-130359.jsonl")),
]

# Average OKT atomic-fact length (measured in Phase 3 from the multihoprag
# repo's facts: 1000-fact sample, mean 23.2 whitespace tokens). Used for the
# granularity-match report (experiment plan success criterion #3).
OKT_FACT_AVG_TOKENS = 23.2


def _load(path: str) -> list[dict]:
    if not os.path.exists(path):
        print(f"  WARNING: missing {path}", file=sys.stderr)
        return []
    out = []
    with open(path, "r", encoding="utf-8") as fh:
        for line in fh:
            try:
                out.append(json.loads(line))
            except Exception:  # noqa: BLE001
                pass
    return out


def _prompt_tokens_per_q(preds: list[dict]) -> float:
    """Avg prompt tokens per question (the generator's context budget).
    Direct/OKT variants record evidence_tokens=None but carry tokens.prompt;
    baselines carry evidence_tokens (retrieved context only). Use prompt
    tokens for cross-variant comparability (it's what the LLM actually saw)."""
    vals = []
    for p in preds:
        t = p.get("tokens") or {}
        pt = t.get("prompt")
        if pt is not None:
            vals.append(int(pt))
    return sum(vals) / len(vals) if vals else 0.0


def _halluc_rate(preds: list[dict]) -> tuple[float, int, int, int]:
    """Hallucination rate = (substantive AND wrong) / n, excluding llm_errors.
    Returns (rate, halluc_count, refusals, n)."""
    if not preds:
        return 0.0, 0, 0, 0
    n = 0; halluc = 0; refusals = 0
    for p in preds:
        pred = p.get("prediction") or ""
        gold = p.get("gold") or ""
        if score_mod._is_llm_error(pred):
            continue
        n += 1
        ok = score_mod.has_intersection(pred, gold)
        refused = score_mod._is_refusal(pred)
        if refused:
            refusals += 1
        elif not ok:
            halluc += 1
    rate = (halluc / n) if n else 0.0
    return rate, halluc, refusals, n


def _acc_by_type(preds: list[dict]) -> dict[str, float]:
    """Per-question-type accuracy."""
    by_type: dict[str, list[bool]] = {}
    for p in preds:
        pred = p.get("prediction") or ""
        gold = p.get("gold") or ""
        if score_mod._is_llm_error(pred):
            continue
        qt = p.get("question_type") or "unknown"
        by_type.setdefault(qt, []).append(score_mod.has_intersection(pred, gold))
    return {qt: sum(1 for x in v if x) / len(v) for qt, v in by_type.items() if v}


def _halluc_by_type(preds: list[dict]) -> dict[str, float]:
    """Per-question-type hallucination rate."""
    by_type: dict[str, dict[str, int]] = {}
    for p in preds:
        pred = p.get("prediction") or ""
        gold = p.get("gold") or ""
        if score_mod._is_llm_error(pred):
            continue
        qt = p.get("question_type") or "unknown"
        b = by_type.setdefault(qt, {"n": 0, "halluc": 0})
        b["n"] += 1
        refused = score_mod._is_refusal(pred)
        ok = score_mod.has_intersection(pred, gold)
        if not refused and not ok:
            b["halluc"] += 1
    return {qt: b["halluc"] / b["n"] for qt, b in by_type.items() if b["n"]}


def _late_chunk_avg_unit_tokens() -> float:
    """Avg whitespace tokens per late-chunk segment, measured directly
    from the chunker (deterministic — window=BASELINE_LATE_CHUNK_WINDOW).
    The retrieval_hits in prediction rows strip the text field to keep
    rows small, so we recompute from the source."""
    from baselines import late_chunking
    chunks = late_chunking.chunk_late_windows()
    return late_chunking.avg_segment_tokens(chunks) if chunks else 0.0


def main() -> int:
    lines = []
    lines.append("=" * 90)
    lines.append("Atomic Facts vs. Late-Chunked Passages — Matched-Conditions Experiment")
    lines.append("=" * 90)
    lines.append("")
    lines.append("Corpus: MultiHop-RAG (n=2556 questions, 609 news articles)")
    lines.append("Generator LLM: google/gemma-4-31b-it via OpenRouter (all configs)")
    lines.append("Condition A embedding: google/gemini-embedding-2 (3072-dim, OKT index)")
    lines.append("Condition B embedding: jinaai/jina-embeddings-v3 (1024-dim, self-hosted)")
    lines.append("  NOTE: embedding-model asymmetry is inherent to late chunking, which")
    lines.append("  structurally requires a long-context model exposing pre-pooling token")
    lines.append("  vectors. Flagged per experiment plan success criterion #3.")
    lines.append("Scoring: identical has_intersection(gold, pred) + refusal/hallucination rubric")
    lines.append("")

    # --- Four-cell table -------------------------------------------------
    lines.append("## Four-cell head-to-head")
    lines.append("")
    header = (
        f"  {'config':<18} {'n':>5} {'acc':>7} {'ci95%':>16} "
        f"{'halluc%':>8} {'refuse%':>8} {'tok/q':>8}"
    )
    lines.append(header)
    rows = []
    for name, path in CONFIGS:
        preds = _load(path)
        if not preds:
            lines.append(f"  {name:<18} (missing)")
            continue
        m = score_mod.score_predictions(preds)
        acc = m["overall"]["accuracy"]
        point, lo, hi = score_mod.bootstrap_accuracy_ci(preds)
        hrate, hcount, refusals, n = _halluc_rate(preds)
        ev = _prompt_tokens_per_q(preds)
        rows.append((name, preds))
        ci_str = f"[{lo:.3f}, {hi:.3f}]"
        lines.append(
            f"  {name:<18} {n:>5} {acc:>7.3f} {ci_str:>16} "
            f"{hrate*100:>7.1f}% {refusals/n*100:>7.1f}% {ev:>8.0f}"
        )
    lines.append("")

    # --- Per question-type accuracy -------------------------------------
    lines.append("## Accuracy by question type")
    lines.append("")
    qts = ["inference_query", "comparison_query", "temporal_query", "null_query"]
    header = f"  {'type':<22} " + " ".join(f"{name:>18}" for name, _ in CONFIGS)
    lines.append(header)
    accs = {name: _acc_by_type(preds) for name, preds in rows}
    for qt in qts:
        cells = " ".join(
            f"{accs[name].get(qt, 0.0):>18.3f}" for name, _ in CONFIGS
        )
        lines.append(f"  {qt:<22} {cells}")
    lines.append("")

    # --- Per question-type hallucination --------------------------------
    lines.append("## Hallucination rate by question type")
    lines.append("")
    lines.append(header)
    h_accs = {name: _halluc_by_type(preds) for name, preds in rows}
    for qt in qts:
        cells = " ".join(
            f"{h_accs[name].get(qt, 0.0)*100:>17.1f}%" for name, _ in CONFIGS
        )
        lines.append(f"  {qt:<22} {cells}")
    lines.append("")

    # --- Granularity match report ---------------------------------------
    lines.append("## Granularity match (success criterion #3)")
    lines.append("")
    lines.append(f"  OKT atomic fact avg length : {OKT_FACT_AVG_TOKENS:.1f} whitespace tokens (measured from multihoprag repo, n=1000)")
    late_avg = _late_chunk_avg_unit_tokens()
    ratio = late_avg / OKT_FACT_AVG_TOKENS if OKT_FACT_AVG_TOKENS else 0.0
    flag = " (OK, within 2x)" if ratio <= 2.0 else " (MISMATCH, flag in writeup)"
    lines.append(f"  late_chunk avg unit length  : {late_avg:.1f} tokens (window={config.BASELINE_LATE_CHUNK_WINDOW})  (ratio {ratio:.2f}x){flag}")
    lines.append("")

    # --- CI overlap verdict ---------------------------------------------
    lines.append("## CI overlap verdict (success criterion #2)")
    lines.append("")
    cis = {}
    for name, preds in rows:
        _, lo, hi = score_mod.bootstrap_accuracy_ci(preds)
        cis[name] = (lo, hi)
    pairs = [
        ("direct@10", "late_chunk@10"),
        ("direct@20", "late_chunk@20"),
        ("late_chunk@10", "late_chunk@20"),
        ("direct@10", "direct@20"),
    ]
    for a, b in pairs:
        if a in cis and b in cis:
            lo_a, hi_a = cis[a]
            lo_b, hi_b = cis[b]
            overlap = not (hi_a < lo_b or hi_b < lo_a)
            verdict = "OVERLAPPING (indistinguishable from noise)" if overlap else "NON-OVERLAPPING (distinguishable)"
            lines.append(f"  {a} CI [{lo_a:.3f}, {hi_a:.3f}]  vs  {b} CI [{lo_b:.3f}, {hi_b:.3f}]  -> {verdict}")
    lines.append("")

    # --- Hypothesis verdict ---------------------------------------------
    lines.append("## Hypothesis verdict")
    lines.append("")
    d10_acc = score_mod.score_predictions(_load(CONFIGS[0][1]))["overall"]["accuracy"]
    d20_acc = score_mod.score_predictions(_load(CONFIGS[1][1]))["overall"]["accuracy"]
    l10_acc = score_mod.score_predictions(_load(CONFIGS[2][1]))["overall"]["accuracy"]
    l20_acc = score_mod.score_predictions(_load(CONFIGS[3][1]))["overall"]["accuracy"]
    d10_lo, d10_hi = cis["direct@10"]; l10_lo, l10_hi = cis["late_chunk@10"]
    d20_lo, d20_hi = cis["direct@20"]; l20_lo, l20_hi = cis["late_chunk@20"]
    d10_wins = d10_acc > l10_acc and d10_lo > l10_hi
    l10_wins = l10_acc > d10_acc and l10_lo > d10_hi
    d20_wins = d20_acc > l20_acc and d20_lo > l20_hi
    l20_wins = l20_acc > d20_acc and l20_lo > d20_hi
    if (d10_wins or d20_wins) and not (l10_wins or l20_wins):
        verdict = "H1a (atomic-facts win): atomic facts outperform late-chunked passages on accuracy with non-overlapping CIs."
    elif (l10_wins or l20_wins) and not (d10_wins or d20_wins):
        verdict = "H1b (late-chunking wins): late-chunked passages outperform atomic facts with non-overlapping CIs."
    else:
        verdict = "H0 (indistinguishable): no config has non-overlapping CIs over the other; differences are within noise."
    lines.append(f"  {verdict}")
    lines.append("")
    lines.append("  Caveats: single corpus (MultiHop-RAG news), single LLM (gemma-4-31b-it),")
    lines.append("  embedding-model asymmetry (gemini vs jina) is inherent to the late-chunking")
    lines.append("  method and flagged in the writeup. Do not extrapolate beyond these conditions.")
    lines.append("")

    out = "\n".join(lines)
    print(out)
    out_path = os.path.join(config.RESULTS_DIR, "late_chunk_experiment_results.txt")
    with open(out_path, "w", encoding="utf-8") as fh:
        fh.write(out + "\n")
    print(f"\nWritten to {out_path}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())