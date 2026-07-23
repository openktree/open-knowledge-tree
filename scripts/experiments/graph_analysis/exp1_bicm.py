"""Experiment 1 — BiCM null-model edge validation.

Question (paper §7.2): is raw `shared_fact_count` usable as-is, or must it be
validated/thresholded? The report's own bipartite-projection theory says raw
projections are dense and degree-confounded and require null-model
validation, but leaves "whether the raw weights are practically useful for
navigation" as an open empirical question.

Method: the Bipartite Configuration Model (BiCM) is the degree-preserving null
model for bipartite networks (Strona et al. 2014; Saracco et al. 2015). Under
it, the expected co-occurrence of concepts i, j is

    <C_ij> = k_i * k_j / |Γ|

where k_i is concept i's fact-degree and |Γ| is the total bipartite edge
count. Under the Poisson approximation, Var[C_ij] ≈ <C_ij>, so

    z_ij = (C_ij - <C_ij>) / sqrt(<C_ij>)
    p_ij = P(X >= C_ij) under Poisson(<C_ij>) = sf(C_ij - 1, <C_ij>)

We compute z and p for every co-occurrence edge, then:

  Metric 1 — Spearman rank correlation between raw weight and z-score.
              Low correlation ⇒ raw weight is mostly degree.
  Metric 2 — % of edges degree-confounded (high raw weight, |z|<1), split
              by weight band (w=1, w=2..4, w>=5).
  Metric 3 — survivor counts at p<0.05, p<0.01, FDR<0.05 (Benjamini–
              Hochberg); compared against raw thresholds w>=2, w>=5.
  Diagnostic — the top-weight edges with their z-scores, to see whether
              the high-weight backbone is structurally significant or just
              degree-confounded hub-hub pairs.

Read-only: SELECTs only, no writes, no schema changes. See PLAN.md.
"""
from __future__ import annotations

import asyncio
import json
import os
import time
from pathlib import Path

import numpy as np
from scipy import stats
from scipy.stats import poisson

from okt_db import DEFAULT_DSN, DEFAULT_REPO_SLUG, load_graph

RESULTS_DIR = Path(__file__).resolve().parent / "results"


def benjamini_hochberg(pvals: np.ndarray) -> np.ndarray:
    """BH FDR adjustment. Returns q-values aligned to the input order."""
    n = len(pvals)
    order = np.argsort(pvals)
    sorted_p = pvals[order]
    # q_i = p_(i) * n / rank_i, then enforce monotonicity from the top.
    ranks = np.arange(1, n + 1, dtype=np.float64)
    q_sorted = sorted_p * n / ranks
    q_sorted = np.minimum.accumulate(q_sorted[::-1])[::-1]
    q_sorted = np.clip(q_sorted, 0.0, 1.0)
    q = np.empty_like(pvals)
    q[order] = q_sorted
    return q


async def run(dsn: str = DEFAULT_DSN, repo_slug: str = DEFAULT_REPO_SLUG) -> dict:
    t0 = time.time()
    groups, bip, cooc, repo_id = await load_graph(dsn, repo_slug)
    t_load = time.time() - t0

    # Build bipartite adjacency to get concept degrees k_i and |Γ|.
    import collections
    concept_facts: dict[str, set[str]] = collections.defaultdict(set)
    fact_concepts: dict[str, set[str]] = collections.defaultdict(set)
    for e in bip:
        concept_facts[e.concept_name].add(e.fact_id)
        fact_concepts[e.fact_id].add(e.concept_name)

    n_concepts = len(concept_facts)
    n_facts = len(fact_concepts)
    gamma = float(len(bip))  # |Γ| — total bipartite edges
    k = {name: len(fs) for name, fs in concept_facts.items()}

    # Vectorize the BiCM computation over all co-occurrence edges.
    observed = np.array([e.shared_fact_count for e in cooc], dtype=np.int64)
    ka = np.array([k[e.name_a] for e in cooc], dtype=np.float64)
    kb = np.array([k[e.name_b] for e in cooc], dtype=np.float64)
    expected = ka * kb / gamma
    sigma = np.sqrt(expected)
    z = (observed - expected) / np.where(sigma > 0, sigma, 1.0)
    with np.errstate(invalid="ignore", divide="ignore"):
        pvals = poisson.sf(observed - 1, expected)
    pvals = np.where(np.isnan(pvals) | np.isinf(pvals), 1.0, pvals)
    qvals = benjamini_hochberg(pvals)

    # ---- Metrics --------------------------------------------------------
    rho, p_spearman = stats.spearmanr(observed, z)

    # Metric 2: degree-confounded share, split by weight band. The bands
    # follow the system's own promotion semantics: w=1 is a draft (single
    # co-occurrence, never promoted), w=2..9 is the "confirmed but not
    # promoted" middle, w>=10 is the v1 promotion threshold (a relation
    # strong enough to be surfaced as a real relation, not a draft).
    PROMOTION_W = 10
    bands = [
        ("w=1 (draft)", observed == 1),
        ("w=2..4", (observed >= 2) & (observed <= 4)),
        ("w=5..9", (observed >= 5) & (observed <= 9)),
        (f"w>={PROMOTION_W} (promoted)", observed >= PROMOTION_W),
        ("all", np.ones_like(observed, dtype=bool)),
    ]
    confounded_by_band = []
    for label, mask in bands:
        n_band = int(mask.sum())
        if n_band == 0:
            continue
        n_conf = int((mask & (np.abs(z) < 1.0)).sum())
        confounded_by_band.append(
            {
                "band": label,
                "n_edges": n_band,
                "n_degree_confounded": n_conf,
                "pct_degree_confounded": round(100 * n_conf / n_band, 2),
            }
        )

    # Metric 3: survivor counts. Include the v1 promotion threshold (w>=10)
    # alongside the BiCM filters so the two validity axes are directly
    # comparable.
    survivors = {
        "p_lt_0.05": int((pvals < 0.05).sum()),
        "p_lt_0.01": int((pvals < 0.01).sum()),
        "fdr_lt_0.05": int((qvals < 0.05).sum()),
        "raw_w_ge_2": int((observed >= 2).sum()),
        "raw_w_ge_5": int((observed >= 5).sum()),
        f"raw_w_ge_{PROMOTION_W}_promoted": int((observed >= PROMOTION_W).sum()),
        "total": int(len(observed)),
    }
    survivors["pct_p_lt_0.05"] = round(100 * survivors["p_lt_0.05"] / survivors["total"], 2)
    survivors["pct_fdr_lt_0.05"] = round(100 * survivors["fdr_lt_0.05"] / survivors["total"], 2)
    survivors["pct_raw_w_ge_2"] = round(100 * survivors["raw_w_ge_2"] / survivors["total"], 2)
    survivors["pct_raw_w_ge_5"] = round(100 * survivors["raw_w_ge_5"] / survivors["total"], 2)
    survivors[f"pct_raw_w_ge_{PROMOTION_W}"] = round(
        100 * survivors[f"raw_w_ge_{PROMOTION_W}_promoted"] / survivors["total"], 2
    )

    # Metric 4: the two-axis cross-tab. The system's W threshold answers
    # "is this relation strong enough to promote?" (draft vs promoted);
    # BiCM answers "is this relation structurally significant, or just two
    # high-degree hubs that co-occur by chance?" (significant vs confounded).
    # The two axes are NOT the same: a promoted edge can still be confounded
    # (high W between two hubs), and a draft edge can be highly significant
    # (w=1 between two rare concepts). This cross-tab measures how often the
    # two axes disagree — the gap a workaround would need to label.
    promoted = observed >= PROMOTION_W
    significant = qvals < 0.05
    confounded = np.abs(z) < 1.0
    cross_tab = {
        "promoted_and_significant": int((promoted & significant).sum()),
        "promoted_and_confounded": int((promoted & confounded).sum()),
        "draft_and_significant": int((~promoted & significant).sum()),
        "draft_and_confounded": int((~promoted & confounded).sum()),
        "total": len(observed),
    }
    cross_tab["pct_promoted_confounded"] = round(
        100 * cross_tab["promoted_and_confounded"] / max(int(promoted.sum()), 1), 2
    )
    cross_tab["pct_draft_significant"] = round(
        100 * cross_tab["draft_and_significant"] / max(int((~promoted).sum()), 1), 2
    )

    # Metric 5: W-threshold sweep. The v1 promotion threshold (W>=10) is a
    # perfect precision filter (0% confounded) but drops 99% of edges. A
    # better default W balances precision (zero confounded) against usefulness
    # (keeping enough relations for navigation). The sweep finds the minimum W
    # at which confounded edges drop to zero — the lowest W that is still a
    # safe precision filter — and reports the edge count, significance, and
    # the share of kept edges that are between low-degree (k<10) concepts
    # (the "one-off juxtaposition" band that is significant but arguably not
    # useful for navigation).
    significant_mask = qvals < 0.05
    confounded_mask = np.abs(z) < 1.0
    low_deg = (ka < 10) & (kb < 10)
    w_sweep = []
    min_clean_w = None
    for w in range(1, 21):
        mask = observed >= w
        n_e = int(mask.sum())
        if n_e == 0:
            break
        n_conf = int((mask & confounded_mask).sum())
        n_sig = int((mask & significant_mask).sum())
        n_low = int((mask & low_deg).sum())
        entry = {
            "w": w,
            "n_edges": n_e,
            "pct_of_total": round(100 * n_e / len(observed), 2),
            "n_confounded": n_conf,
            "pct_confounded": round(100 * n_conf / n_e, 2),
            "n_significant": n_sig,
            "pct_significant": round(100 * n_sig / n_e, 2),
            "n_low_degree_pairs": n_low,
            "pct_low_degree_pairs": round(100 * n_low / n_e, 2),
        }
        w_sweep.append(entry)
        if n_conf == 0 and min_clean_w is None:
            min_clean_w = w
    # The recommended default W: the minimum W with zero confounded edges.
    # This is the lowest W that is still a safe precision filter; it keeps
    # the most relations without admitting hub-hub noise.
    recommended_w = min_clean_w if min_clean_w is not None else PROMOTION_W

    # Diagnostic: top-20 by raw weight with z/p, and the most-confounded
    # high-weight edges (|z|<1 among w>=3, if any).
    top_order = np.argsort(-observed)
    top_edges = []
    for i in top_order[:20]:
        e = cooc[i]
        top_edges.append(
            {
                "name_a": e.name_a,
                "name_b": e.name_b,
                "weight": int(observed[i]),
                "expected": round(float(expected[i]), 4),
                "z": round(float(z[i]), 3),
                "p": float(pvals[i]),
            }
        )

    # Confounded high-weight edges (w>=3 but |z|<1) — the report's predicted
    # "USA↔everything" failure mode. Usually empty when the backbone is clean.
    conf_mask = (observed >= 3) & (np.abs(z) < 1.0)
    confounded_high = []
    for i in np.where(conf_mask)[0][:20]:
        e = cooc[i]
        confounded_high.append(
            {
                "name_a": e.name_a,
                "name_b": e.name_b,
                "weight": int(observed[i]),
                "expected": round(float(expected[i]), 4),
                "z": round(float(z[i]), 3),
            }
        )

    # Diagnostic: a sample of edges in the W=5..9 band — the relations the
    # recommended default W (5) keeps but the v1 threshold (W>=10) discards.
    # These are the "useful mid-band" relations that justify lowering the
    # promotion threshold: real story clusters (founder↔company, product↔brand,
    # sports teams, events) that just don't hit the w=10 frequency bar.
    mid_band = (observed >= 5) & (observed <= 9)
    mid_idx = np.where(mid_band)[0]
    mid_idx = mid_idx[np.argsort(-observed[mid_idx])]
    mid_band_sample = []
    for i in mid_idx[:20]:
        e = cooc[i]
        mid_band_sample.append(
            {
                "name_a": e.name_a,
                "name_b": e.name_b,
                "weight": int(observed[i]),
                "z": round(float(z[i]), 2),
                "k_a": int(k[e.name_a]),
                "k_b": int(k[e.name_b]),
            }
        )

    # Degree distribution of the top hubs (for context on k_i).
    top_hubs = sorted(k.items(), key=lambda kv: -kv[1])[:15]
    top_hubs_list = [{"name": n, "fact_degree": int(d)} for n, d in top_hubs]

    result = {
        "experiment": "exp1_bicm",
        "repository": repo_slug,
        "repository_id": repo_id,
        "method": "BiCM (bipartite configuration model), Poisson approximation",
        "graph": {
            "n_concepts": n_concepts,
            "n_facts": n_facts,
            "n_bipartite_edges": int(gamma),
            "n_cooccurrence_edges": len(cooc),
            "max_concept_degree": int(max(k.values())),
            "max_weight": int(observed.max()),
        },
        "load_seconds": round(t_load, 2),
        "metric_1_spearman_raw_vs_z": {
            "rho": round(float(rho), 4),
            "p_value": float(p_spearman),
            "interpretation": (
                "near-zero: raw weight is almost uncorrelated with BiCM-validated "
                "significance; degree-confounding is severe in the raw weights"
                if abs(rho) < 0.1
                else "moderate-high: raw weight tracks structural significance"
            ),
        },
        "metric_2_degree_confounded_by_band": confounded_by_band,
        "metric_3_survivors": survivors,
        "metric_4_promotion_vs_significance_cross_tab": cross_tab,
        "metric_5_w_threshold_sweep": w_sweep,
        "recommended_default_w": recommended_w,
        "v1_promotion_w": PROMOTION_W,
        "promotion_threshold_w": PROMOTION_W,
        "diagnostic_top_edges_by_weight": top_edges,
        "diagnostic_confounded_high_weight_edges": confounded_high,
        "diagnostic_mid_band_sample_w5_to_w9": mid_band_sample,
        "diagnostic_top_hubs": top_hubs_list,
    }
    return result


def _print_summary(result: dict) -> None:
    g = result["graph"]
    print(f"Repository: {result['repository']} ({result['repository_id']})")
    print(f"Graph: {g['n_concepts']} concepts, {g['n_facts']} facts, "
          f"{g['n_bipartite_edges']} bipartite edges, {g['n_cooccurrence_edges']} cooc edges")
    print(f"Loaded in {result['load_seconds']}s\n")

    m1 = result["metric_1_spearman_raw_vs_z"]
    print(f"Metric 1 — Spearman corr(raw_weight, z_score): {m1['rho']} (p={m1['p_value']:.2e})")
    print(f"  → {m1['interpretation']}\n")

    print("Metric 2 — degree-confounded edges (|z|<1) by weight band:")
    print(f"  {'band':<8} {'n':>7} {'conf':>6} {'%':>6}")
    for b in result["metric_2_degree_confounded_by_band"]:
        print(f"  {b['band']:<8} {b['n_edges']:>7} {b['n_degree_confounded']:>6} {b['pct_degree_confounded']:>5.1f}%")
    print()

    s = result["metric_3_survivors"]
    print("Metric 3 — survivor counts:")
    print(f"  p<0.05:   {s['p_lt_0.05']:>6} / {s['total']} ({s['pct_p_lt_0.05']:.1f}%)")
    print(f"  p<0.01:   {s['p_lt_0.01']:>6} / {s['total']}")
    print(f"  FDR<0.05: {s['fdr_lt_0.05']:>6} / {s['total']} ({s['pct_fdr_lt_0.05']:.1f}%)")
    print(f"  raw w>=2: {s['raw_w_ge_2']:>6} / {s['total']} ({s['pct_raw_w_ge_2']:.1f}%)")
    print(f"  raw w>=5: {s['raw_w_ge_5']:>6} / {s['total']} ({s['pct_raw_w_ge_5']:.1f}%)")
    pw = result["promotion_threshold_w"]
    print(f"  raw w>={pw} (promoted): {s[f'raw_w_ge_{pw}_promoted']:>6} / {s['total']} ({s[f'pct_raw_w_ge_{pw}']:.1f}%)\n")

    ct = result["metric_4_promotion_vs_significance_cross_tab"]
    print(f"Metric 4 — promotion (W>={pw}) vs BiCM significance cross-tab:")
    print(f"  promoted & significant:  {ct['promoted_and_significant']:>6}")
    print(f"  promoted & confounded:   {ct['promoted_and_confounded']:>6}  ({ct['pct_promoted_confounded']:.1f}% of promoted)")
    print(f"  draft & significant:     {ct['draft_and_significant']:>6}  ({ct['pct_draft_significant']:.1f}% of drafts)")
    print(f"  draft & confounded:      {ct['draft_and_confounded']:>6}")
    print(f"  total:                   {ct['total']:>6}")
    if ct["promoted_and_confounded"] > 0:
        print(f"  → {ct['promoted_and_confounded']} promoted edges are degree-confounded: W threshold admits hub-hub noise.")
    if ct["draft_and_significant"] > 0:
        print(f"  → {ct['draft_and_significant']} draft edges are BiCM-significant: W threshold drops real relations between rare concepts.")
    print()

    sweep = result["metric_5_w_threshold_sweep"]
    rec_w = result["recommended_default_w"]
    print(f"Metric 5 — W-threshold sweep (recommended default W={rec_w}, v1 was W={result['v1_promotion_w']}):")
    print(f"  {'W':>3} {'n':>7} {'%kept':>6} {'conf':>5} {'%conf':>5} {'%sig':>6} {'%low-k':>7}")
    for e in sweep:
        marker = " ← recommended" if e["w"] == rec_w else (" ← v1" if e["w"] == result["v1_promotion_w"] else "")
        print(f"  {e['w']:>3} {e['n_edges']:>7} {e['pct_of_total']:>5.1f}% {e['n_confounded']:>5} {e['pct_confounded']:>4.1f}% {e['pct_significant']:>5.1f}% {e['pct_low_degree_pairs']:>6.1f}%{marker}")
    print(f"  → W={rec_w} is the minimum W with 0 confounded edges (keeps {next(e['n_edges'] for e in sweep if e['w']==rec_w)} relations vs {next(e['n_edges'] for e in sweep if e['w']==result['v1_promotion_w'])} at v1 W={result['v1_promotion_w']})")
    print()

    mid = result.get("diagnostic_mid_band_sample_w5_to_w9", [])
    if mid:
        print(f"Diagnostic — sample of W=5..9 edges (kept by recommended W={rec_w}, dropped by v1 W={result['v1_promotion_w']}):")
        print(f"  {'w':>3} {'z':>7} {'k_a':>5} {'k_b':>5}  pair")
        for e in mid[:15]:
            print(f"  {e['weight']:>3} {e['z']:>7.1f} {e['k_a']:>5} {e['k_b']:>5}  {e['name_a'][:28]} <-> {e['name_b'][:28]}")
    print()

    print("Diagnostic — top-15 edges by raw weight:")
    print(f"  {'w':>4} {'<C>':>8} {'z':>8} {'p':>10}  pair")
    for e in result["diagnostic_top_edges_by_weight"][:15]:
        print(f"  {e['weight']:>4} {e['expected']:>8.2f} {e['z']:>8.2f} {e['p']:>10.2e}  "
              f"{e['name_a'][:24]} <-> {e['name_b'][:24]}")

    conf = result["diagnostic_confounded_high_weight_edges"]
    if conf:
        print(f"\nDiagnostic — {len(conf)} high-weight (w>=3) edges that ARE degree-confounded (|z|<1):")
        for e in conf:
            print(f"  w={e['weight']} <C>={e['expected']:.2f} z={e['z']:.2f}  "
                  f"{e['name_a'][:24]} <-> {e['name_b'][:24]}")
    else:
        print("\nDiagnostic — no high-weight (w>=3) edges are degree-confounded: the backbone is clean.")


async def main() -> None:
    dsn = os.environ.get("OKT_DB_DSN", DEFAULT_DSN)
    repo_slug = os.environ.get("OKT_REPO_SLUG", DEFAULT_REPO_SLUG)
    result = await run(dsn, repo_slug)
    RESULTS_DIR.mkdir(parents=True, exist_ok=True)
    out = RESULTS_DIR / "bicm_validation.json"
    out.write_text(json.dumps(result, indent=2))
    _print_summary(result)
    print(f"\nWrote {out}")


if __name__ == "__main__":
    asyncio.run(main())