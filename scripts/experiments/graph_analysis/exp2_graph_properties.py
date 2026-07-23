"""Experiment 2 — 7 graph-property measurements (paper §6.1).

The report's structural-quality claims are "design-level, not findings": it
claims the emergent co-occurrence graph is navigable, coherent, and
backbone-structured, but never ran the measurements. This experiment runs
them.

Crucially, it runs them across four W thresholds (1, 2, 5, 10) because
Experiment 1 showed W dramatically reshapes the edge set (76k → 23k → 3k →
747). A graph that is incoherent at W=1 may be well-structured at W=5; a
graph that is navigable at W=5 may fragment at W=10. The report's
structural-quality claims are only meaningful at a specific W.

The seven measurements (paper §6.1), per W threshold:

  1. Degree distribution — CCDF + Broido–Clauset power-law taxonomy.
  2. Connected components — count + size distribution.
  3. Community structure — Louvain modularity Q + NMI vs corpus domains
     (parsed_sitename). The highest-priority measurement per §6.3.
  4. Small-world index σ vs degree-matched random.
  5. Edge-weight distribution — fraction w=1/≥2/≥5.
  6. Concept fragmentation — multi-context concept_groups count.
  7. source_count correlation — Pearson r between fact source_count and
     the centrality of its linked concepts.

Read-only: SELECTs only. See PLAN.md.
"""
from __future__ import annotations

import asyncio
import json
import math
import os
import time
from pathlib import Path

import asyncpg
import networkx as nx
import numpy as np
from scipy import stats

from okt_db import DEFAULT_DSN, DEFAULT_REPO_SLUG, load_graph

RESULTS_DIR = Path(__file__).resolve().parent / "results"
W_THRESHOLDS = [1, 2, 5, 10]


def _broido_clauset_label(alpha: float, sigma: float) -> str:
    """Broido & Clauset (2019) taxonomy: classify power-law evidence."""
    if sigma is None or math.isnan(sigma):
        return "N/A"
    if sigma < 0.5:
        return "strongest"
    if sigma < 0.8:
        return "strong"
    if sigma < 1.0:
        return "moderate"
    if sigma < 1.3:
        return "weak"
    return "super-weak"


def _degree_distribution(g: nx.Graph) -> dict:
    """Measurement 1: degree distribution + power-law fit."""
    degrees = np.array([d for _, d in g.degree()], dtype=np.float64)
    if len(degrees) == 0 or degrees.max() < 2:
        return {"n_nodes": int(len(degrees)), "alpha": None, "sigma": None,
                "taxonomy": "N/A", "mean": float(degrees.mean()) if len(degrees) else 0,
                "max": float(degrees.max()) if len(degrees) else 0}
    try:
        import powerlaw
        fit = powerlaw.Fit(degrees, discrete=True, verbose=False)
        alpha = fit.power_law.alpha
        sigma = fit.power_law.sigma
        # Compare power-law vs lognormal: R > 0 favors power-law
        try:
            R, p = fit.distribution_compare("power_law", "lognormal")
        except Exception:
            R, p = None, None
    except Exception:
        alpha, sigma, R, p = None, None, None, None
    return {
        "n_nodes": int(len(degrees)),
        "mean_degree": round(float(degrees.mean()), 2),
        "max_degree": int(degrees.max()),
        "median_degree": float(np.median(degrees)),
        "alpha": round(float(alpha), 3) if alpha else None,
        "sigma": round(float(sigma), 3) if sigma else None,
        "taxonomy": _broido_clauset_label(alpha if alpha else 0, sigma if sigma else 99),
        "powerlaw_vs_lognormal_R": round(float(R), 3) if R is not None else None,
        "powerlaw_vs_lognormal_p": round(float(p), 4) if p is not None else None,
        "interpretation": (
            "R<0: lognormal fits better than power-law (Broido-Clauset: scale-free is rare)"
            if R is not None and R < 0
            else "R>0: power-law plausible"
            if R is not None and R > 0
            else "inconclusive"
        ),
    }


def _connected_components(g: nx.Graph) -> dict:
    """Measurement 2: connected components."""
    comps = list(nx.connected_components(g))
    sizes = sorted((len(c) for c in comps), reverse=True)
    total = g.number_of_nodes()
    return {
        "n_components": len(comps),
        "largest_component_size": sizes[0] if sizes else 0,
        "largest_component_pct": round(100 * sizes[0] / total, 2) if sizes and total else 0,
        "second_largest": sizes[1] if len(sizes) > 1 else 0,
        "n_isolated_nodes": sum(1 for n in g.nodes() if g.degree(n) == 0),
        "component_sizes_top10": sizes[:10],
    }


def _community_structure(g: nx.Graph, concept_domains: dict[str, str]) -> dict:
    """Measurement 3: Louvain communities + NMI vs corpus domains."""
    if g.number_of_edges() == 0:
        return {"modularity": None, "n_communities": 0, "nmi": None}
    try:
        communities = nx.algorithms.community.louvain_communities(g, seed=42)
    except Exception:
        return {"modularity": None, "n_communities": 0, "nmi": None, "error": "louvain failed"}
    modularity = nx.algorithms.community.modularity(g, communities)

    # NMI: compare community assignment vs domain (parsed_sitename) assignment.
    # Build node → community_id and node → domain_id maps.
    node_to_comm = {}
    for i, comm in enumerate(communities):
        for node in comm:
            node_to_comm[node] = i
    # Only nodes that have a domain label.
    labeled_nodes = [n for n in g.nodes() if n in concept_domains and concept_domains[n]]
    if len(labeled_nodes) < 10:
        return {"modularity": round(float(modularity), 4), "n_communities": len(communities),
                "nmi": None, "n_labeled_nodes": len(labeled_nodes)}
    comm_labels = np.array([node_to_comm.get(n, -1) for n in labeled_nodes])
    domains = [concept_domains[n] for n in labeled_nodes]
    domain_set = sorted(set(domains))
    domain_to_id = {d: i for i, d in enumerate(domain_set)}
    domain_labels = np.array([domain_to_id[d] for d in domains])
    try:
        nmi = nx.algorithms.community.normalized_mutual_info_information(
            comm_labels, domain_labels
        )
    except Exception:
        # NetworkX 3.2 may not have this; compute NMI manually.
        from sklearn.metrics import normalized_mutual_info_score
        nmi = normalized_mutual_info_score(domain_labels, comm_labels)
    return {
        "modularity": round(float(modularity), 4),
        "n_communities": len(communities),
        "nmi_vs_domains": round(float(nmi), 4),
        "n_labeled_nodes": len(labeled_nodes),
        "n_domains": len(domain_set),
    }


def _small_world(g: nx.Graph) -> dict:
    """Measurement 4: small-world index σ vs degree-matched random.

    Computed on the giant (largest connected) component, because average
    shortest path length is undefined on a disconnected graph. The giant
    component is the navigable subgraph an agent would actually traverse.
    """
    if g.number_of_edges() == 0:
        return {"sigma": None, "avg_path_length": None, "clustering": None,
                "giant_size": 0}
    comps = sorted(nx.connected_components(g), key=len, reverse=True)
    giant = g.subgraph(comps[0])
    n_giant = giant.number_of_nodes()
    if n_giant < 10:
        return {"sigma": None, "avg_path_length": None, "clustering": None,
                "giant_size": n_giant}
    c_clust = nx.average_clustering(giant)
    # Average shortest path length: exact for small graphs, sampled for large.
    # All-pairs BFS is O(n*m) which is too slow for n>5000.
    if n_giant <= 2000:
        c_path = nx.average_shortest_path_length(giant)
    else:
        # Sample 500 random source nodes and average their BFS distances.
        import random
        rng = random.Random(42)
        nodes = list(giant.nodes())
        sample = rng.sample(nodes, min(500, len(nodes)))
        total_dist = 0.0
        n_pairs = 0
        for src in sample:
            lengths = nx.single_source_shortest_path_length(giant, src)
            for tgt, d in lengths.items():
                if tgt != src:
                    total_dist += d
                    n_pairs += 1
        c_path = total_dist / n_pairs if n_pairs else float("inf")
    # Degree-matched random graph (configuration model). For large graphs
    # (>5k nodes) this is expensive, so reduce trials and use the Erdős–Rényi
    # approximation as a fallback: C_rand ≈ p = 2m/(n(n-1)), L_rand ≈ ln(n)/ln(k).
    n_giant = giant.number_of_nodes()
    m_giant = giant.number_of_edges()
    k_mean = 2 * m_giant / n_giant if n_giant else 0
    if n_giant > 5000:
        # Analytical approximation for large graphs.
        p = 2 * m_giant / (n_giant * (n_giant - 1)) if n_giant > 1 else 0
        c_rand = p
        l_rand = math.log(n_giant) / math.log(max(k_mean, 1.1)) if k_mean > 1 else float("inf")
    else:
        n_trials = 5 if n_giant > 500 else 10
        c_rand_sum = 0.0
        l_rand_sum = 0.0
        valid = 0
        deg_seq = [d for _, d in giant.degree()]
        for _ in range(n_trials):
            try:
                rg = nx.configuration_model(deg_seq, seed=np.random.RandomState())
                rg = nx.Graph(rg)
                rg.remove_edges_from(nx.selfloop_edges(rg))
                c_rand_sum += nx.average_clustering(rg)
                if nx.is_connected(rg):
                    l_rand_sum += nx.average_shortest_path_length(rg)
                    valid += 1
            except Exception:
                continue
        if valid == 0:
            # Configuration model produced disconnected graphs every trial
            # (common for sparse graphs). Fall back to the analytical
            # Erdős–Rényi approximation: C_rand ≈ p, L_rand ≈ ln(n)/ln(k).
            p = 2 * m_giant / (n_giant * (n_giant - 1)) if n_giant > 1 else 0
            c_rand = p
            l_rand = math.log(n_giant) / math.log(max(k_mean, 1.1)) if k_mean > 1 else float("inf")
        else:
            c_rand = c_rand_sum / n_trials
            l_rand = l_rand_sum / valid if valid else float("inf")
    sigma = (c_clust / c_rand) / (c_path / l_rand) if c_rand > 0 and l_rand > 0 and c_path > 0 else None
    return {
        "sigma": round(float(sigma), 3) if sigma else None,
        "avg_path_length": round(float(c_path), 3),
        "clustering": round(float(c_clust), 4),
        "c_rand": round(float(c_rand), 4),
        "l_rand": round(float(l_rand), 3) if l_rand != float("inf") else None,
        "giant_size": n_giant,
        "giant_pct_of_nodes_with_edges": round(100 * n_giant / g.number_of_nodes(), 2),
        "interpretation": (
            "σ>1: small-world (high clustering + short paths); caveat: Van den Berg–van Leeuwen "
            "show sparseness alone can produce σ>1"
            if sigma and sigma > 1
            else "σ≤1: not small-world"
        ),
    }


def _edge_weight_distribution(g: nx.Graph) -> dict:
    """Measurement 5: edge-weight distribution."""
    weights = np.array([d["weight"] for _, _, d in g.edges(data=True)])
    if len(weights) == 0:
        return {"n_edges": 0}
    return {
        "n_edges": int(len(weights)),
        "w_min": int(weights.min()),
        "w_max": int(weights.max()),
        "w_mean": round(float(weights.mean()), 3),
        "w_median": float(np.median(weights)),
        "frac_w1": round(float((weights == 1).mean()), 4),
        "frac_w_ge_2": round(float((weights >= 2).mean()), 4),
        "frac_w_ge_5": round(float((weights >= 5).mean()), 4),
    }


def _concept_fragmentation(concept_contexts: dict[str, set[str]]) -> dict:
    """Measurement 6: multi-context concept groups (fragmentation)."""
    multi = {n: ctxs for n, ctxs in concept_contexts.items() if len(ctxs) > 1}
    total = len(concept_contexts)
    return {
        "n_concept_groups": total,
        "n_multi_context": len(multi),
        "pct_multi_context": round(100 * len(multi) / total, 2) if total else 0,
        "max_contexts_per_group": max((len(ctxs) for ctxs in concept_contexts.values()), default=0),
        "sample_multi_context": [
            {"name": n, "contexts": sorted(ctxs)}
            for n, ctxs in sorted(multi.items(), key=lambda kv: -len(kv[1]))[:10]
        ],
    }


def _source_count_correlation(
    g: nx.Graph, fact_source_count: dict[str, int], fact_concepts: dict[str, set[str]]
) -> dict:
    """Measurement 7: correlation between fact source_count and concept centrality."""
    # For each fact, compute the mean centrality (degree) of its linked concepts.
    deg_cent = dict(nx.degree_centrality(g))
    fact_mean_centrality = []
    fact_sc = []
    for fid, concepts in fact_concepts.items():
        sc = fact_source_count.get(fid, 1)
        centralities = [deg_cent.get(c, 0) for c in concepts if c in deg_cent]
        if centralities:
            fact_mean_centrality.append(float(np.mean(centralities)))
            fact_sc.append(sc)
    if len(fact_sc) < 10:
        return {"n_facts": len(fact_sc), "pearson_r": None, "spearman_rho": None}
    r, p_r = stats.pearsonr(fact_sc, fact_mean_centrality)
    rho, p_rho = stats.spearmanr(fact_sc, fact_mean_centrality)
    return {
        "n_facts": len(fact_sc),
        "pearson_r": round(float(r), 4),
        "pearson_p": float(p_r),
        "spearman_rho": round(float(rho), 4),
        "spearman_p": float(p_rho),
        "mean_source_count": round(float(np.mean(fact_sc)), 3),
        "max_source_count": int(np.max(fact_sc)),
    }


async def run(dsn: str = DEFAULT_DSN, repo_slug: str = DEFAULT_REPO_SLUG) -> dict:
    t0 = time.time()
    groups, bip, cooc, repo_id = await load_graph(dsn, repo_slug)
    t_load = time.time() - t0

    # Build concept → set of facts, fact → set of concepts.
    import collections
    concept_facts: dict[str, set[str]] = collections.defaultdict(set)
    fact_concepts: dict[str, set[str]] = collections.defaultdict(set)
    for e in bip:
        concept_facts[e.concept_name].add(e.fact_id)
        fact_concepts[e.fact_id].add(e.concept_name)

    # Build concept → set of contexts (for fragmentation, Measurement 6).
    # Re-query the DB for context labels (not in the bipartite edges).
    conn = await asyncpg.connect(dsn)
    try:
        ctx_rows = await conn.fetch(
            """
            SELECT lower(c.canonical_name) AS name, lower(c.context) AS context
            FROM okt_repository.concepts c
            WHERE c.repository_id = $1
            """,
            repo_id,
        )
        concept_contexts: dict[str, set[str]] = collections.defaultdict(set)
        for r in ctx_rows:
            concept_contexts[r["name"]].add(r["context"])

        # Domain labels for NMI: each concept's domain = the most common
        # parsed_sitename among its facts' sources.
        domain_rows = await conn.fetch(
            """
            SELECT lower(c.canonical_name) AS concept, s.parsed_sitename AS domain,
                   count(*) AS n
            FROM okt_repository.fact_concepts fc
            JOIN okt_repository.concepts c ON c.id = fc.concept_id
            JOIN okt_repository.fact_sources fs ON fs.fact_id = fc.fact_id
            JOIN okt_repository.sources s ON s.id = fs.source_id
            WHERE c.repository_id = $1 AND s.parsed_sitename IS NOT NULL AND s.parsed_sitename != ''
            GROUP BY lower(c.canonical_name), s.parsed_sitename
            """,
            repo_id,
        )
        concept_domain_counts: dict[str, dict[str, int]] = collections.defaultdict(dict)
        for r in domain_rows:
            concept_domain_counts[r["concept"]][r["domain"]] = r["n"]
        concept_domains = {
            c: max(domains.items(), key=lambda kv: kv[1])[0]
            for c, domains in concept_domain_counts.items()
            if domains
        }

        # Fact source_count (from fact_sources junction).
        sc_rows = await conn.fetch(
            """
            SELECT fs.fact_id::text AS fid, count(DISTINCT fs.source_id) AS sc
            FROM okt_repository.fact_sources fs
            JOIN okt_repository.sources s ON s.id = fs.source_id
            WHERE s.repository_id = $1
            GROUP BY fs.fact_id
            """,
            repo_id,
        )
        fact_source_count = {r["fid"]: r["sc"] for r in sc_rows}
    finally:
        await conn.close()

    # Run the 7 measurements at each W threshold.
    results_by_w = {}
    for w in W_THRESHOLDS:
        t_w = time.time()
        # Build the weighted concept graph at this W threshold. Only add nodes
        # that have at least one edge at this W — isolated concepts (degree 0
        # at this W) are not part of the navigable graph and would inflate
        # the component count. We track them separately.
        g = nx.Graph()
        for e in cooc:
            if e.shared_fact_count >= w:
                g.add_edge(e.name_a, e.name_b, weight=e.shared_fact_count)
        n_isolated = len(concept_facts) - g.number_of_nodes()
        t_build = time.time() - t_w

        m1 = _degree_distribution(g)
        m2 = _connected_components(g)
        m3 = _community_structure(g, concept_domains)
        m4 = _small_world(g)
        m5 = _edge_weight_distribution(g)
        m6 = _concept_fragmentation(concept_contexts)
        m7 = _source_count_correlation(g, fact_source_count, fact_concepts)

        results_by_w[f"w{w}"] = {
            "w_threshold": w,
            "n_nodes_with_edges": g.number_of_nodes(),
            "n_edges": g.number_of_edges(),
            "n_isolated_concepts": n_isolated,
            "n_total_concepts": len(concept_facts),
            "build_seconds": round(t_build, 2),
            "m1_degree_distribution": m1,
            "m2_connected_components": m2,
            "m3_community_structure": m3,
            "m4_small_world": m4,
            "m5_edge_weight_distribution": m5,
            "m6_concept_fragmentation": m6,
            "m7_source_count_correlation": m7,
        }

    return {
        "experiment": "exp2_graph_properties",
        "repository": repo_slug,
        "repository_id": repo_id,
        "w_thresholds": W_THRESHOLDS,
        "graph": {
            "n_concepts": len(concept_facts),
            "n_facts": len(fact_concepts),
            "n_bipartite_edges": len(bip),
            "n_cooccurrence_edges": len(cooc),
        },
        "load_seconds": round(t_load, 2),
        "results_by_w": results_by_w,
    }


def _print_summary(result: dict) -> None:
    g = result["graph"]
    print(f"Repository: {result['repository']}")
    print(f"Graph: {g['n_concepts']} concepts, {g['n_facts']} facts, "
          f"{g['n_bipartite_edges']} bipartite, {g['n_cooccurrence_edges']} cooc edges")
    print(f"Loaded in {result['load_seconds']}s\n")

    for w_key, w_res in result["results_by_w"].items():
        w = w_res["w_threshold"]
        print(f"{'='*60}")
        print(f"W≥{w}: {w_res['n_nodes_with_edges']} nodes (with edges), "
              f"{w_res['n_isolated_concepts']} isolated, "
              f"{w_res['n_edges']} edges (built in {w_res['build_seconds']}s)")

        m1 = w_res["m1_degree_distribution"]
        print(f"  M1 Degree: mean={m1['mean_degree']}, max={m1['max_degree']}, "
              f"α={m1['alpha']}, taxonomy={m1['taxonomy']}")
        if m1.get("powerlaw_vs_lognormal_R") is not None:
            print(f"       powerlaw-vs-lognormal R={m1['powerlaw_vs_lognormal_R']} "
                  f"({m1['interpretation']})")

        m2 = w_res["m2_connected_components"]
        print(f"  M2 Components: {m2['n_components']} components, "
              f"largest={m2['largest_component_size']} ({m2['largest_component_pct']}%), "
              f"isolated={m2['n_isolated_nodes']}")

        m3 = w_res["m3_community_structure"]
        nmi_str = f", NMI={m3['nmi_vs_domains']}" if m3.get("nmi_vs_domains") is not None else ""
        print(f"  M3 Communities: Q={m3['modularity']}, {m3['n_communities']} communities"
              f"{nmi_str} ({m3.get('n_labeled_nodes', 0)} labeled nodes, {m3.get('n_domains', 0)} domains)")

        m4 = w_res["m4_small_world"]
        if m4.get("sigma") is not None:
            print(f"  M4 Small-world: σ={m4['sigma']}, L={m4['avg_path_length']}, "
                  f"C={m4['clustering']}, C_rand={m4['c_rand']}, L_rand={m4['l_rand']}")
            print(f"       {m4['interpretation']}")
        else:
            print(f"  M4 Small-world: σ=N/A (graph too small or disconnected)")

        m5 = w_res["m5_edge_weight_distribution"]
        print(f"  M5 Weights: mean={m5.get('w_mean')}, "
              f"frac w=1: {m5.get('frac_w1')}, frac w≥5: {m5.get('frac_w_ge_5')}")

        m6 = w_res["m6_concept_fragmentation"]
        print(f"  M6 Fragmentation: {m6['n_multi_context']}/{m6['n_concept_groups']} "
              f"({m6['pct_multi_context']}%) multi-context, max={m6['max_contexts_per_group']}")

        m7 = w_res["m7_source_count_correlation"]
        print(f"  M7 source_count: r={m7.get('pearson_r')}, ρ={m7.get('spearman_rho')}, "
              f"n={m7.get('n_facts')}, mean_sc={m7.get('mean_source_count')}")
        print()


async def main() -> None:
    dsn = os.environ.get("OKT_DB_DSN", DEFAULT_DSN)
    repo_slug = os.environ.get("OKT_REPO_SLUG", DEFAULT_REPO_SLUG)
    result = await run(dsn, repo_slug)
    RESULTS_DIR.mkdir(parents=True, exist_ok=True)
    out = RESULTS_DIR / "graph_properties.json"
    out.write_text(json.dumps(result, indent=2))
    _print_summary(result)
    print(f"Wrote {out}")


if __name__ == "__main__":
    asyncio.run(main())