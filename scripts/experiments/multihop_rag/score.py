"""Score MultiHop-RAG predictions against gold answers.

Ports the official qa_evaluate.py:
  - extract_answer(text)  — regex for `The answer to the question is "..."`
  - has_intersection(pred, gold)  — lowercase token-set overlap (any = success)
  - per question_type and overall:
      Precision = Recall = F1 = Accuracy = per-question success rate

Also reports a free source_coverage diagnostic: the fraction of
predictions whose source_ids_used overlap with the gold evidence
sources (matched by title/author/published_at from queries.jsonl's
evidence_list, when present). This is a soft retrieval-quality signal
that comes for free from the fact/source IDs we record per prediction.

Refusal-vs-hallucination breakdown:
  - refusal:     prediction is empty or "Insufficient information."
  - substantive: prediction is an actual answer (correct or hallucinated)
  - hallucinated: substantive AND wrong

Two modes:

  1. Side-by-side (default): reads results/predictions_concept.jsonl
     and results/predictions_facts.jsonl, scores each variant, and
     prints a side-by-side comparison table. Writes
     results/qa_metrics.json with both variants' metrics + a per-question
     agreement matrix.

  2. Single-file (legacy): pass --predictions-file <path> to score one
     predictions file and print the single-variant table. This keeps
     backward compatibility with the old single-variant CLI.

CLI:
  python3 score.py                              # side-by-side (both variants)
  python3 score.py --predictions-file X.jsonl   # single variant (legacy)
"""
from __future__ import annotations

import argparse
import json
import os
import sys
from typing import Any

import config


# ---------------------------------------------------------------------------
# Scoring core (ports qa_evaluate.py)


def has_intersection(pred: str, gold: str) -> bool:
    """True if any token (lowercased) is shared between pred and gold.

    Matches the official qa_evaluate.py: success = any word overlap.
    """
    if not pred or not gold:
        return False
    pred_tokens = set(pred.lower().split())
    gold_tokens = set(gold.lower().split())
    return bool(pred_tokens & gold_tokens)


ABSTENTION = "insufficient information."
LLM_ERROR = "[llm error]"


def _is_refusal(pred: str) -> bool:
    """A prediction is a refusal when it is empty or the abstention phrase."""
    if not pred:
        return True
    return pred.strip().lower().rstrip(".") == ABSTENTION.rstrip(".")


def _is_llm_error(pred: str) -> bool:
    """A prediction is an LLM error (failed synthesis call, not a real answer)."""
    if not pred:
        return False
    return pred.strip().lower() == LLM_ERROR


def score_predictions(predictions: list[dict]) -> dict[str, Any]:
    """Compute per-type and overall P/R/F1/Acc.

    Following the paper, P = R = F1 = Acc = per-question success rate
    (one gold per question). We still report all four for completeness
    and to match the official scorer's output shape.

    Categories:
      - refusal:     prediction is empty or "Insufficient information."
      - substantive: prediction is an actual answer (correct or hallucinated)
      - hallucinated: substantive AND wrong
      - llm_error:   the synthesis LLM call failed (reported separately,
                     excluded from acc/refusal/hallucination metrics)
    """
    by_type: dict[str, list[bool]] = {}
    overall: list[bool] = []
    # Per-type breakdown: n, correct, refusals, substantive, hallucinated, llm_errors.
    breakdown_by_type: dict[str, dict[str, int]] = {}
    breakdown_overall = {"n": 0, "correct": 0, "refusals": 0,
                         "substantive": 0, "hallucinated": 0,
                         "llm_errors": 0}
    for p in predictions:
        pred = p.get("prediction") or ""
        gold = p.get("gold") or ""
        ok = has_intersection(pred, gold)
        qt = p.get("question_type") or "unknown"
        refused = _is_refusal(pred)
        errored = _is_llm_error(pred)
        b = breakdown_by_type.setdefault(qt, {"n": 0, "correct": 0,
                                               "refusals": 0,
                                               "substantive": 0,
                                               "hallucinated": 0,
                                               "llm_errors": 0})
        b["n"] += 1
        breakdown_overall["n"] += 1

        if errored:
            # LLM errors are excluded from accuracy/refusal/hallucination
            # metrics — they're reported as a separate count.
            b["llm_errors"] += 1
            breakdown_overall["llm_errors"] += 1
            # Don't add to by_type/overall for accuracy calculation.
            continue

        by_type.setdefault(qt, []).append(ok)
        overall.append(ok)

        if ok:
            b["correct"] += 1
            breakdown_overall["correct"] += 1
        if refused:
            b["refusals"] += 1
            breakdown_overall["refusals"] += 1
        else:
            b["substantive"] += 1
            breakdown_overall["substantive"] += 1
            if not ok:
                b["hallucinated"] += 1
                breakdown_overall["hallucinated"] += 1

    def _metrics(results: list[bool]) -> dict[str, float]:
        if not results:
            return {"precision": 0.0, "recall": 0.0, "f1": 0.0, "accuracy": 0.0, "n": 0}
        acc = sum(1 for x in results if x) / len(results)
        # With one gold per question, P=R=F1=Acc.
        return {
            "precision": acc,
            "recall": acc,
            "f1": acc,
            "accuracy": acc,
            "n": len(results),
        }

    return {
        "by_question_type": {qt: _metrics(rs) for qt, rs in sorted(by_type.items())},
        "overall": _metrics(overall),
        "breakdown_by_question_type": breakdown_by_type,
        "breakdown_overall": breakdown_overall,
    }


def _format_breakdown_table(breakdown: dict[str, Any], title: str) -> str:
    """Print refusal vs substantive vs hallucinated vs llm_error counts."""
    lines = [f"\n{title}:"]
    lines.append(
        f"  {'type':<22} {'n':>5} {'corr':>5} {'refuse':>7} "
        f"{'subst':>7} {'halluc':>7}  {'halluc%':>8} {'llm_err':>8}"
    )
    items: list[tuple[str, dict[str, int]]] = []
    if "by_question_type" in breakdown:
        items = sorted(breakdown["by_question_type"].items())
    overall = breakdown.get("overall") or breakdown
    for qt, b in items:
        n = b["n"]
        halluc = b["hallucinated"]
        halluc_pct = (halluc / n * 100) if n else 0.0
        lines.append(
            f"  {qt:<22} {n:>5} {b['correct']:>5} {b['refusals']:>7} "
            f"{b['substantive']:>7} {halluc:>7}  {halluc_pct:>7.1f}% "
            f"{b.get('llm_errors', 0):>8}"
        )
    n = overall.get("n", 0)
    halluc = overall.get("hallucinated", 0)
    halluc_pct = (halluc / n * 100) if n else 0.0
    lines.append(
        f"  {'OVERALL':<22} {n:>5} {overall.get('correct', 0):>5} "
        f"{overall.get('refusals', 0):>7} {overall.get('substantive', 0):>7} "
        f"{halluc:>7}  {halluc_pct:>7.1f}% "
        f"{overall.get('llm_errors', 0):>8}"
    )
    return "\n".join(lines)


# ---------------------------------------------------------------------------
# Source-coverage diagnostic (free retrieval-quality signal)


def _gold_evidence_titles(q: dict) -> list[str]:
    """Pull the titles of the gold evidence docs from queries.jsonl.

    queries.jsonl rows do not currently carry evidence_list (we only
    store id/query/question_type/gold_answer). When the downloader is
    extended to also write evidence_list, this will return real titles.
    Until then, returns [] so coverage is reported as N/A.
    """
    ev = q.get("evidence_list") or []
    return [e.get("title") for e in ev if e.get("title")]


def source_coverage(predictions: list[dict], queries_by_id: dict[str, dict]) -> dict[str, Any]:
    """Fraction of predictions whose source_ids_used is non-empty.

    A full retrieval-recall metric would need the gold evidence source
    ids matched into the repo; we don't have that mapping here. We
    report the simpler "did the pipeline retrieve ANY source for this
    question" rate, broken down by question_type, as a sanity signal.
    """
    by_type: dict[str, list[bool]] = {}
    overall: list[bool] = []
    for p in predictions:
        retrieved = bool(p.get("source_ids_used"))
        qt = p.get("question_type") or "unknown"
        by_type.setdefault(qt, []).append(retrieved)
        overall.append(retrieved)
    out = {
        "by_question_type": {
            qt: {"retrieved_any": sum(rs) / len(rs) if rs else 0.0, "n": len(rs)}
            for qt, rs in sorted(by_type.items())
        },
        "overall": {
            "retrieved_any": sum(overall) / len(overall) if overall else 0.0,
            "n": len(overall),
        },
    }
    return out


# ---------------------------------------------------------------------------
# Retrieval recall@k (addresses report Limitation #6)
#
# The MultiHop-RAG dataset ships an `evidence_list` per question: a list
# of {author, source, title, published_at, fact} dicts pointing at the
# gold evidence articles. We match each prediction's retrieved sources
# against this gold set by (title, source, published_at) and compute:
#   - recall@k = |retrieved ∩ gold| / |gold|   (per question, averaged)
#   - gold_coverage = fraction of questions with at least one gold source hit
#
# Matching key: the OKT variant stores source_ids as OKT source UUIDs,
# but it ALSO enriches each fact with source metadata (parsed_sitename,
# parsed_title, published_at). The baseline variant stores doc_ids
# (article slugs). We resolve both to a (title, source, published_at)
# tuple via queries_by_id (OKT) or the prediction's retrieval_hits
# (baseline) and match against evidence_list.
# ---------------------------------------------------------------------------


def _evidence_key(e: dict[str, Any]) -> tuple[str, str]:
    """Normalize an evidence_list entry / retrieved source into a
    (title, source) tuple for matching.

    We match on (title, source) only — NOT published_at — because the
    baseline retrieval_hits recorded in the prediction file may omit
    the date (a known limitation of the run_variant logging), while the
    gold evidence_list always carries it. Dropping the date from the
    match key makes recall@k computable on the existing predictions
    without re-running. (title, source) is sufficient to uniquely
    identify a corpus article in this 609-doc dataset.
    """
    title = (e.get("title") or "").strip().lower()
    source = (e.get("source") or e.get("parsed_sitename") or "").strip().lower()
    return (title, source)


def _gold_set(q: dict) -> set[tuple[str, str]]:
    """The set of gold evidence keys for a question. Empty for null_query."""
    ev = q.get("evidence_list") or []
    return {_evidence_key(e) for e in ev if e.get("title") or e.get("source")}


def _retrieved_set_okt(pred: dict, queries_by_id: dict[str, dict]) -> set[tuple[str, str]]:
    """For OKT variants, we don't have per-source metadata on the
    prediction row directly — but the answer audit isn't loaded here.
    We approximate via the OKT source UUIDs matched back through
    queries_by_id when possible. Since OKT source_ids_used are UUIDs
    (not titles), we cannot match them to evidence_list without the
    OKT DB. So for OKT we return an empty set and report recall as
    N/A — the recall@k metric is computed for the BASELINES (where we
    control the payload and store title/source/date directly).

    A future extension can backfill OKT source metadata onto the
    prediction row (the fact enrichment already fetches it) to enable
    OKT recall@k too.
    """
    return set()


def _retrieved_set_baseline(pred: dict) -> set[tuple[str, str]]:
    """For baseline variants, retrieval_hits carries title/source per
    retrieved chunk. We dedup by (title, source) so multiple chunks
    from the same article count as one source hit.
    """
    hits = pred.get("retrieval_hits") or []
    out: set[tuple[str, str]] = set()
    for h in hits:
        if h.get("title") or h.get("source"):
            out.add(_evidence_key(h))
    return out


def retrieval_recall(
    predictions: list[dict], queries_by_id: dict[str, dict]
) -> dict[str, Any]:
    """Per-type + overall retrieval recall@k and gold coverage.

    recall@k (per question) = |retrieved_sources ∩ gold_sources| / |gold|
    gold_coverage           = fraction of questions with ≥1 gold source hit

    Questions with an empty gold set (null_query, or missing evidence_list)
    are excluded from recall (denominator 0) but counted in gold_coverage
    as N/A. When NO prediction has an evidence_list (old queries.jsonl),
    the whole metric is reported as unavailable so the legacy output stays
    valid.
    """
    # Detect whether evidence_list is present at all.
    has_evidence = any(q.get("evidence_list") for q in queries_by_id.values())
    if not has_evidence:
        return {
            "available": False,
            "by_question_type": {},
            "overall": {"recall_at_k": None, "gold_coverage": None, "n": 0},
            "note": "queries.jsonl has no evidence_list; re-run download_dataset.py",
        }

    by_type_recall: dict[str, list[float]] = {}
    by_type_cov: dict[str, list[bool]] = {}
    overall_recall: list[float] = []
    overall_cov: list[bool] = []
    # Track which variants are "unmeasurable" (OKT: source_ids are UUIDs
    # we can't resolve to titles without the OKT DB). Their recall is
    # reported as None, not 0.0, so the table shows N/A not a misleading 0.
    unmeasurable_variants: set[str] = set()

    for p in predictions:
        qid = p.get("id")
        q = queries_by_id.get(qid, {})
        gold = _gold_set(q)
        qt = p.get("question_type") or "unknown"
        variant = (p.get("variant") or "").lower()
        if variant in ("dense_x", "traditional"):
            retrieved = _retrieved_set_baseline(p)
        else:
            # OKT variants: source_ids are UUIDs, not titles. We cannot
            # match them to evidence_list without the OKT DB, so this
            # variant's recall is unmeasurable. Mark it and skip its
            # per-question accumulation (it reports N/A in the table).
            unmeasurable_variants.add(variant)
            continue
        if gold:
            hit = retrieved & gold
            rec = len(hit) / len(gold)
            by_type_recall.setdefault(qt, []).append(rec)
            overall_recall.append(rec)
            by_type_cov.setdefault(qt, []).append(bool(hit))
            overall_cov.append(bool(hit))
        # If gold is empty (null_query), skip recall but the question
        # still counts toward coverage denominator handling below.

    def _avg(xs: list[float]) -> Any:
        return sum(xs) / len(xs) if xs else None

    by_type_out: dict[str, dict[str, Any]] = {}
    for qt in sorted(set(by_type_recall) | set(by_type_cov)):
        rs = by_type_recall.get(qt, [])
        cs = by_type_cov.get(qt, [])
        by_type_out[qt] = {
            "recall_at_k": _avg(rs),
            "gold_coverage": (sum(cs) / len(cs)) if cs else None,
            "n": len(rs),
        }

    overall_recall_val: Any = _avg(overall_recall)
    overall_cov_val: Any = (
        (sum(overall_cov) / len(overall_cov)) if overall_cov else None
    )
    if unmeasurable_variants and not overall_recall:
        # All predictions were from unmeasurable variants -> N/A.
        overall_recall_val = None
        overall_cov_val = None

    return {
        "available": True,
        "by_question_type": by_type_out,
        "overall": {
            "recall_at_k": overall_recall_val,
            "gold_coverage": overall_cov_val,
            "n": len(overall_recall),
            "unmeasurable": sorted(unmeasurable_variants),
        },
    }


# ---------------------------------------------------------------------------
# Output


def _format_table(metrics: dict[str, Any], coverage: dict[str, Any],
                   title: str = "MultiHop-RAG QA Benchmark — Results") -> str:
    lines = []
    lines.append("=" * 70)
    lines.append(title)
    lines.append("=" * 70)
    lines.append("")
    lines.append(f"Overall (n={metrics['overall']['n']}):")
    lines.append(f"  Accuracy: {metrics['overall']['accuracy']:.3f}")
    lines.append(f"  Precision: {metrics['overall']['precision']:.3f}")
    lines.append(f"  Recall:    {metrics['overall']['recall']:.3f}")
    lines.append(f"  F1:        {metrics['overall']['f1']:.3f}")
    lines.append("")
    lines.append("By question_type:")
    lines.append(f"  {'type':<22} {'n':>5} {'acc':>7} {'prec':>7} {'rec':>7} {'f1':>7} {'cov':>7}")
    for qt, m in metrics["by_question_type"].items():
        cov = coverage["by_question_type"].get(qt, {}).get("retrieved_any", 0.0)
        lines.append(
            f"  {qt:<22} {m['n']:>5} {m['accuracy']:>7.3f} {m['precision']:>7.3f} "
            f"{m['recall']:>7.3f} {m['f1']:>7.3f} {cov:>7.3f}"
        )
    lines.append("")
    lines.append("Source coverage (retrieved_any):")
    lines.append(f"  overall: {coverage['overall']['retrieved_any']:.3f} "
                 f"(n={coverage['overall']['n']})")
    if "breakdown_by_question_type" in metrics:
        lines.append(_format_breakdown_table(
            {"by_question_type": metrics["breakdown_by_question_type"],
             "overall": metrics["breakdown_overall"]},
            "Refusal vs hallucination breakdown",
        ))
        lines.append("")
        lines.append("Legend:")
        lines.append("  refuse   = predicted 'Insufficient information.' (abstention)")
        lines.append("  subst    = predicted a substantive answer (not a refusal)")
        lines.append("  halluc   = substantive AND wrong (potential hallucination)")
        lines.append("  halluc_rate = halluc / n")
    lines.append("=" * 70)
    return "\n".join(lines)


# ---------------------------------------------------------------------------
# Side-by-side variant comparison
# # ---------------------------------------------------------------------------


def _load_predictions(path: str) -> list[dict]:
    out: list[dict] = []
    if not os.path.exists(path):
        return out
    with open(path, "r", encoding="utf-8") as fh:
        for line in fh:
            try:
                out.append(json.loads(line))
            except Exception:  # noqa: BLE001
                pass
    return out


def _variant_path(variant: str) -> str:
    """Resolve a variant name to its predictions file.

    Two naming conventions are supported:
      - Legacy (pre-timestamp): predictions_<variant>.jsonl
      - Timestamped (current):    predictions_<variant>_<k>_<stamp>.jsonl

    When multiple timestamped files exist (different k or runs), the
    NEWEST by mtime is returned. When both conventions exist for the
    same variant, the timestamped one wins (it's the current runner's
    output). The OKT variants (concept/facts/direct) still use the
    legacy non-timestamped name; the baselines (dense_x/traditional)
    use the timestamped name with an embedded k.
    """
    import glob

    base = os.path.join(config.RESULTS_DIR, "predictions")
    # Timestamped first (newest of any k/stamp), then legacy fallback.
    ts = glob.glob(f"{base}_{variant}_*_*.jsonl")
    if ts:
        return max(ts, key=os.path.getmtime)
    legacy = f"{base}_{variant}.jsonl"
    return legacy


def _format_side_by_side(
    variant_metrics: dict[str, dict[str, Any]],
    variant_coverage: dict[str, dict[str, Any]],
    variant_predictions: dict[str, list[dict]],
) -> str:
    """Print a side-by-side comparison of N retrieval variants.

    variant_metrics: {variant_name: metrics_dict from score_predictions}
    variant_coverage: {variant_name: coverage_dict}
    variant_predictions: {variant_name: predictions_list} for token accounting
    """
    variant_names = list(variant_metrics.keys())
    col_w = 10
    lines = []
    lines.append("=" * 100)
    lines.append("MultiHop-RAG — Side-by-side variant comparison")
    lines.append("=" * 100)
    lines.append("")

    # Overall accuracy + cost row.
    lines.append("Overall:")
    header = (
        f"  {'variant':<10} {'n':>5} {'acc':>7} {'cov':>7} "
        f"{'refuse':>7} {'subst':>7} {'halluc':>7} {'halluc%':>8} "
        f"{'llm_err':>8} {'tokens':>10} {'prompt':>9} {'completion':>10}"
    )
    lines.append(header)
    for name in variant_names:
        m = variant_metrics[name]
        cov = variant_coverage[name]
        b = m["breakdown_overall"]
        n = b["n"]
        halluc = b["hallucinated"]
        halluc_rate = (halluc / n * 100) if n else 0.0
        # n for accuracy excludes llm_errors; report scored_n.
        scored_n = n - b.get("llm_errors", 0)
        acc = m["overall"]["accuracy"]
        toks = _sum_tokens(variant_predictions.get(name, []))
        lines.append(
            f"  {name:<10} {scored_n:>5} {acc:>7.3f} "
            f"{cov['overall']['retrieved_any']:>7.3f} "
            f"{b['refusals']:>7} {b['substantive']:>7} {halluc:>7} "
            f"{halluc_rate:>7.1f}% "
            f"{b.get('llm_errors', 0):>8} "
            f"{toks['total']:>10} {toks['prompt']:>9} {toks['completion']:>10}"
        )
    lines.append("")

    # Per question_type accuracy across all variants.
    qts = sorted(set().union(*[
        vm["by_question_type"].keys() for vm in variant_metrics.values()
    ]))
    lines.append("By question_type (accuracy):")
    header = f"  {'type':<22} {'n':>5}" + "".join(f"  {name:>{col_w}}" for name in variant_names)
    lines.append(header)
    for qt in qts:
        ns = [variant_metrics[v]["by_question_type"].get(qt, {"n": 0, "accuracy": 0.0})["n"]
              for v in variant_names]
        n = max(ns) if ns else 0
        row = f"  {qt:<22} {n:>5}"
        for name in variant_names:
            m = variant_metrics[name]["by_question_type"].get(qt, {"accuracy": 0.0})
            row += f"  {m['accuracy']:>{col_w}.3f}"
        lines.append(row)
    lines.append("")

    # Per question_type coverage across all variants.
    lines.append("By question_type (source coverage):")
    lines.append(header)
    for qt in qts:
        ns = [variant_coverage[v]["by_question_type"].get(qt, {"n": 0, "retrieved_any": 0.0})["n"]
              for v in variant_names]
        n = max(ns) if ns else 0
        row = f"  {qt:<22} {n:>5}"
        for name in variant_names:
            c = variant_coverage[name]["by_question_type"].get(qt, {"retrieved_any": 0.0})
            row += f"  {c['retrieved_any']:>{col_w}.3f}"
        lines.append(row)
    lines.append("")

    # Per question_type hallucination rate across all variants.
    lines.append("By question_type (hallucination rate):")
    lines.append(header)
    for qt in qts:
        ns = [variant_metrics[v]["breakdown_by_question_type"].get(qt, {"n": 0}).get("n", 0)
              for v in variant_names]
        n = max(ns) if ns else 0
        row = f"  {qt:<22} {n:>5}"
        for name in variant_names:
            b = variant_metrics[name]["breakdown_by_question_type"].get(qt, {"n": 0, "hallucinated": 0})
            rate = (b["hallucinated"] / b["n"]) if b["n"] else 0.0
            row += f"  {rate:>{col_w}.3f}"
        lines.append(row)
    lines.append("")

    # Token cost per variant, broken down by LLM call type.
    lines.append("Token cost per variant (sum across all questions):")
    lines.append(f"  {'variant':<10} {'total':>10} {'prompt':>9} {'completion':>10}  breakdown")
    for name in variant_names:
        toks = _sum_tokens(variant_predictions.get(name, []))
        breakdown = _sum_tokens_by_call(variant_predictions.get(name, []))
        bstr = ", ".join(f"{k}={v['prompt']+v['completion']}" for k, v in breakdown.items())
        lines.append(
            f"  {name:<10} {toks['total']:>10} {toks['prompt']:>9} "
            f"{toks['completion']:>10}  {bstr}"
        )
    lines.append("")

    # Per-question token cost (avg).
    lines.append("Avg tokens per question:")
    for name in variant_names:
        preds = variant_predictions.get(name, [])
        n = len(preds) or 1
        toks = _sum_tokens(preds)
        lines.append(
            f"  {name:<10} avg_total={toks['total']/n:>7.0f}  "
            f"avg_prompt={toks['prompt']/n:>7.0f}  "
            f"avg_completion={toks['completion']/n:>5.0f}"
        )
    lines.append("")

    # Evidence-token budget (the retrieved-evidence context the
    # synthesis LLM receives from the retriever — isolates the RAG
    # context size, distinct from total tokens which includes the
    # prompt template + completion). This is the key metric for the
    # "information density" comparison: facts are small, passages are
    # large, so facts@10 and traditional@5 can be compared on roughly
    # equal evidence-token budgets.
    lines.append("Avg evidence tokens per question (retrieved context only):")
    for name in variant_names:
        preds = variant_predictions.get(name, [])
        n = len(preds) or 1
        ev = sum(int(p.get("evidence_tokens", 0)) for p in preds)
        avg_n = sum(
            1 for p in preds if "evidence_tokens" in p
        ) or 1
        lines.append(
            f"  {name:<10} avg_evidence={ev/avg_n:>7.0f}  "
            f"(recorded on {avg_n}/{n} rows)"
        )
    lines.append("=" * 100)
    return "\n".join(lines)


def _sum_tokens(predictions: list[dict]) -> dict[str, int]:
    """Sum total/prompt/completion tokens across all predictions."""
    total = prompt = completion = 0
    for p in predictions:
        t = p.get("tokens") or {}
        prompt += int(t.get("prompt", 0))
        completion += int(t.get("completion", 0))
        total += int(t.get("prompt", 0)) + int(t.get("completion", 0))
    return {"total": total, "prompt": prompt, "completion": completion}


def _sum_tokens_by_call(predictions: list[dict]) -> dict[str, dict[str, int]]:
    """Sum tokens by LLM call type (concept_query_extraction, synthesis, etc).

    The `llm_calls` field on each prediction carries per-call usage.
    """
    out: dict[str, dict[str, int]] = {}
    for p in predictions:
        calls = p.get("llm_calls") or {}
        for call_name, usage in calls.items():
            slot = out.setdefault(call_name, {"prompt": 0, "completion": 0})
            slot["prompt"] += int(usage.get("prompt", 0))
            slot["completion"] += int(usage.get("completion", 0))
    return out


def _format_legend() -> str:
    return (
        "\nLegend:\n"
        "  refuse   = predicted 'Insufficient information.' (abstention)\n"
        "  subst    = predicted a substantive answer (not a refusal)\n"
        "  halluc   = substantive AND wrong (potential hallucination)\n"
        "  halluc%  = hallucinated / n, as a percentage\n"
        "  llm_err  = LLM synthesis call failed (excluded from acc/refuse/halluc)\n"
        "  tokens   = sum of prompt + completion tokens across all questions\n"
        "  breakdown = per-LLM-call token cost "
        "(concept_query_extraction + fact_query_extraction + synthesis)\n"
    )


def _fmt_recall_val(v: Any) -> str:
    if v is None:
        return "  N/A "
    return f"{v:>{10}.3f}"


def _format_recall_block(variant_recall: dict[str, dict[str, Any]]) -> str:
    """Print retrieval recall@k + gold coverage per variant.

    Only baseline variants (dense_x, traditional) have matched
    retrieved-source metadata; OKT variants report N/A because their
    source_ids are OKT UUIDs not resolvable to titles without the OKT
    DB (see retrieval_recall docstring).
    """
    lines: list[str] = []
    lines.append("\nRetrieval recall@k (retrieved sources ∩ gold evidence / gold):")
    any_avail = any(r.get("available") for r in variant_recall.values())
    if not any_avail:
        lines.append(
            "  N/A — queries.jsonl has no evidence_list. "
            "Re-run `python3 download_dataset.py` to populate it."
        )
        return "\n".join(lines)
    variant_names = list(variant_recall.keys())
    col_w = 10
    header = f"  {'variant':<10} {'recall@k':>{col_w}} {'gold_cov':>{col_w}} {'n':>5}"
    lines.append(header)
    for name in variant_names:
        r = variant_recall[name]
        if not r.get("available"):
            lines.append(f"  {name:<10} {'N/A':>{col_w}} {'N/A':>{col_w}} {'N/A':>5}")
            continue
        o = r["overall"]
        lines.append(
            f"  {name:<10} {_fmt_recall_val(o.get('recall_at_k'))} "
            f"{_fmt_recall_val(o.get('gold_coverage'))} {o.get('n', 0):>5}"
        )
    lines.append("")
    # Per question_type recall.
    qts = sorted(set().union(*[
        set(r.get("by_question_type", {}).keys()) for r in variant_recall.values()
    ]))
    if qts:
        lines.append("By question_type (recall@k):")
        header2 = f"  {'type':<22}" + "".join(f"  {name:>{col_w}}" for name in variant_names)
        lines.append(header2)
        for qt in qts:
            row = f"  {qt:<22}"
            for name in variant_names:
                r = variant_recall[name]
                bt = r.get("by_question_type", {}).get(qt, {})
                row += f"  {_fmt_recall_val(bt.get('recall_at_k'))}"
            lines.append(row)
        lines.append("")
        lines.append("By question_type (gold coverage):")
        lines.append(header2)
        for qt in qts:
            row = f"  {qt:<22}"
            for name in variant_names:
                r = variant_recall[name]
                bt = r.get("by_question_type", {}).get(qt, {})
                row += f"  {_fmt_recall_val(bt.get('gold_coverage'))}"
            lines.append(row)
    lines.append(
        "\n  recall@k  = |retrieved sources ∩ gold evidence| / |gold evidence| "
        "(per question, averaged)\n"
        "  gold_cov  = fraction of questions with ≥1 gold source retrieved\n"
        "  OKT variants report N/A (source_ids are UUIDs; backfilling source\n"
        "  metadata onto the prediction row is future work)."
    )
    # Surface unmeasurable variants explicitly.
    unmeas = []
    for name in variant_names:
        r = variant_recall[name]
        if r.get("available") and r["overall"].get("unmeasurable"):
            unmeas.append((name, r["overall"]["unmeasurable"]))
    if unmeas:
        lines.append("\n  Unmeasurable (cannot resolve retrieved ids to gold titles):")
        for name, variants in unmeas:
            lines.append(f"    {name}: {', '.join(variants)}")
    return "\n".join(lines)


# ---------------------------------------------------------------------------
# CLI
# # ---------------------------------------------------------------------------


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--predictions-file", type=str, default="",
                    help="single predictions file (legacy single-variant mode). "
                         "When omitted, runs side-by-side over all "
                         "results/predictions_<variant>.jsonl files.")
    ap.add_argument("--queries-file", type=str, default=config.QUERIES_PATH)
    ap.add_argument("--variants", type=str, default="",
                    help="comma-separated variant names to include in the "
                         "side-by-side comparison (default: all that exist). "
                         "e.g. --variants concept,facts,direct")
    args = ap.parse_args()

    # Load queries (for gold evidence titles, when present).
    queries_by_id: dict[str, dict] = {}
    if os.path.exists(args.queries_file):
        with open(args.queries_file, "r", encoding="utf-8") as fh:
            for line in fh:
                try:
                    q = json.loads(line)
                    queries_by_id[q["id"]] = q
                except Exception:  # noqa: BLE001
                    pass

    # --- Single-variant (legacy) mode ---------------------------------
    if args.predictions_file:
        if not os.path.exists(args.predictions_file):
            print(f"  predictions file not found: {args.predictions_file}",
                  file=sys.stderr)
            return 2
        predictions = _load_predictions(args.predictions_file)
        print(f"  loaded {len(predictions)} predictions from {args.predictions_file}")
        metrics = score_predictions(predictions)
        coverage = source_coverage(predictions, queries_by_id)
        out = {"qa_metrics": metrics, "source_coverage": coverage}
        os.makedirs(os.path.dirname(config.QA_METRICS_PATH), exist_ok=True)
        with open(config.QA_METRICS_PATH, "w", encoding="utf-8") as fh:
            json.dump(out, fh, indent=2, ensure_ascii=False)
        table = _format_table(metrics, coverage)
        with open(config.SUMMARY_PATH, "w", encoding="utf-8") as fh:
            fh.write(table + "\n")
        print(table)
        print(f"\nMetrics JSON: {config.QA_METRICS_PATH}")
        print(f"Summary:      {config.SUMMARY_PATH}")
        return 0

    # --- Side-by-side mode (N variants) -------------------------------
    # Discover variant prediction files.
    if args.variants:
        candidate = [v.strip() for v in args.variants.split(",") if v.strip()]
    else:
        # Default: OKT variants + the new baselines, if present.
        candidate = ["concept", "facts", "direct", "dense_x", "traditional"]
    variant_preds: dict[str, list[dict]] = {}
    variant_metrics: dict[str, dict[str, Any]] = {}
    variant_coverage: dict[str, dict[str, Any]] = {}
    variant_recall: dict[str, dict[str, Any]] = {}
    for v in candidate:
        preds = _load_predictions(_variant_path(v))
        if preds:
            variant_preds[v] = preds
            variant_metrics[v] = score_predictions(preds)
            variant_coverage[v] = source_coverage(preds, queries_by_id)
            variant_recall[v] = retrieval_recall(preds, queries_by_id)
    if not variant_preds:
        print(
            "  no variant predictions found. Run "
            "`python3 run_benchmark.py` first, or pass --predictions-file.",
            file=sys.stderr,
        )
        return 2
    loaded_summary = ", ".join(f"{v}: {len(p)}" for v, p in variant_preds.items())
    print(f"  loaded {loaded_summary}")

    # Per-question agreement matrix across all variants.
    # For each id present in ALL variants, mark which variants got it right.
    id_sets = [set(p["id"] for p in preds) for preds in variant_preds.values()]
    shared_ids = sorted(set.intersection(*id_sets)) if id_sets else []
    by_id = {v: {p["id"]: p for p in preds} for v, preds in variant_preds.items()}
    # Union of correct across variants: any variant correct.
    union_correct = 0
    all_wrong = 0
    per_variant_only = {v: 0 for v in variant_preds}
    for qid in shared_ids:
        oks = {v: has_intersection(by_id[v][qid].get("prediction", ""),
                                   by_id[v][qid].get("gold", ""))
               for v in variant_preds}
        n_correct = sum(1 for ok in oks.values() if ok)
        if n_correct > 0:
            union_correct += 1
            # Count variants that got it uniquely right (all others wrong).
            if n_correct == 1:
                for v in variant_preds:
                    if oks[v]:
                        per_variant_only[v] += 1
        else:
            all_wrong += 1

    out: dict[str, Any] = {
        v: {
            "qa_metrics": variant_metrics[v],
            "source_coverage": variant_coverage[v],
            "retrieval_recall": variant_recall[v],
        }
        for v in variant_preds
    }
    out["agreement"] = {
        "shared_n": len(shared_ids),
        "union_correct": union_correct,
        "all_wrong": all_wrong,
        "per_variant_only": per_variant_only,
    }
    out["tokens"] = {
        v: _sum_tokens(variant_preds[v]) for v in variant_preds
    }
    os.makedirs(os.path.dirname(config.QA_METRICS_PATH), exist_ok=True)
    with open(config.QA_METRICS_PATH, "w", encoding="utf-8") as fh:
        json.dump(out, fh, indent=2, ensure_ascii=False)

    table = _format_side_by_side(variant_metrics, variant_coverage, variant_preds)
    table += _format_recall_block(variant_recall)
    table += _format_legend()
    # Agreement block.
    a = out["agreement"]
    n = a["shared_n"]
    union_rate = (a["union_correct"] / n) if n else 0.0
    table += f"\nPer-question agreement (n={n} shared ids across all variants):\n"
    table += f"  union correct:    {a['union_correct']:>4}  ({union_rate:.3f})\n"
    table += f"  all wrong:        {a['all_wrong']:>4}\n"
    for v in variant_preds:
        cnt = a["per_variant_only"][v]
        rate = (cnt / n) if n else 0.0
        table += f"  {v+' only':<16} {cnt:>4}  ({rate:.3f})\n"
    with open(config.SUMMARY_PATH, "w", encoding="utf-8") as fh:
        fh.write(table + "\n")
    print(table)
    print(f"\nMetrics JSON: {config.QA_METRICS_PATH}")
    print(f"Summary:      {config.SUMMARY_PATH}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())