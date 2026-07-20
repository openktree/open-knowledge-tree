#!/usr/bin/env python3
"""
Embedding deduplication threshold sweep
========================================

Standalone experiment that connects directly to the local Qdrant and
Postgres (no API server, no River workers, no migrations) and replays the
dedup worker's nearest-neighbor search for every embedded fact of a single
investigation, then sweeps candidate thresholds to estimate how many facts
would be marked `to_delete` at each level and surfaces example pairs so an
operator can pick the right `dedup.threshold` for the current embedding
model.

Read-only:
  - Qdrant: only `query_points` (recommend-by-id) — no upserts/deletes.
  - Postgres: only `SELECT` — no writes, no advisory locks.

Usage:
    python3.10 dedup_threshold_sweep.py \\
        --investigation 12c7ba67-9b61-424a-85c5-d5206b32af52 \\
        --out dedup_sweep_shadow_fleet.html

Requirements (already installed for python3.10):
    psycopg2-binary, qdrant-client
"""

from __future__ import annotations

import argparse
import collections
import concurrent.futures as cf
import dataclasses
import datetime as dt
import html
import json
import os
import random
import statistics
import sys
import time
from typing import Iterable

import psycopg2
import psycopg2.extras
from qdrant_client import QdrantClient, models


# ---------------------------------------------------------------------------
# Args


def parse_args() -> argparse.Namespace:
    p = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    p.add_argument("--investigation", required=True, help="investigation UUID (okt_repository.investigations.id)")
    p.add_argument("--pg-host", default="localhost")
    p.add_argument("--pg-port", default=5432, type=int)
    p.add_argument("--pg-user", default="okt")
    p.add_argument("--pg-password", default="okt_dev")
    p.add_argument("--pg-db", default="okt")
    p.add_argument("--qdrant-host", default="localhost")
    p.add_argument("--qdrant-port", default=6333, type=int, help="Qdrant REST port (6333). 6334 is gRPC only.")
    p.add_argument("--qdrant-collection", default="okt_facts")
    p.add_argument("--top-k", default=20, type=int, help="neighbors to fetch per fact (default 20)")
    p.add_argument("--concurrency", default=8, type=int, help="parallel Qdrant queries")
    p.add_argument(
        "--thresholds",
        default="0.80,0.82,0.84,0.86,0.88,0.90,0.91,0.92,0.93,0.94,0.95,0.96,0.97,0.98",
        help="comma-separated candidate thresholds to evaluate",
    )
    p.add_argument("--examples-per-band", default=6, type=int, help="example pairs to print per threshold band")
    p.add_argument("--out", default="dedup_sweep.html", help="HTML report output path")
    p.add_argument("--max-facts", default=0, type=int, help="cap the number of facts scanned (0 = all)")
    p.add_argument("--seed", default=42, type=int, help="seed for example sampling")
    return p.parse_args()


# ---------------------------------------------------------------------------
# DB helpers


def pg_connect(args) -> "psycopg2.extensions.connection":
    conn = psycopg2.connect(
        host=args.pg_host,
        port=args.pg_port,
        user=args.pg_user,
        password=args.pg_password,
        dbname=args.pg_db,
    )
    cur = conn.cursor()
    cur.execute("SET search_path TO okt_system, okt_repository, public")
    conn.commit()
    return conn


def load_investigation(conn, inv_id: str) -> dict:
    cur = conn.cursor()
    cur.execute(
        "SELECT id, title, topic, created_at FROM okt_repository.investigations WHERE id=%s",
        (inv_id,),
    )
    row = cur.fetchone()
    if row is None:
        raise SystemExit(f"investigation {inv_id} not found")
    inv_id, title, topic, created_at = row
    # source count + fact count for the investigation
    cur.execute(
        """
        WITH inv_sources AS (SELECT source_id FROM okt_repository.investigation_sources WHERE investigation_id=%s)
        SELECT
          (SELECT count(*) FROM inv_sources),
          count(DISTINCT fs.fact_id),
          count(DISTINCT fs.fact_id) FILTER (WHERE f.embedded_at IS NOT NULL AND f.status='stable' AND f.fact_kind='text'),
          count(DISTINCT fs.fact_id) FILTER (WHERE f.embedded_at IS NULL),
          count(DISTINCT fs.fact_id) FILTER (WHERE f.fact_kind='image')
        FROM okt_repository.fact_sources fs
        JOIN okt_repository.facts f ON f.id = fs.fact_id
        WHERE fs.source_id IN (SELECT source_id FROM inv_sources)
        """,
        (inv_id,),
    )
    n_sources, n_facts, n_embedded, n_not_embedded, n_image = cur.fetchone()
    return {
        "id": inv_id,
        "title": title,
        "topic": topic,
        "created_at": created_at.isoformat() if created_at else None,
        "n_sources": n_sources,
        "n_facts": n_facts,
        "n_embedded_text_stable": n_embedded,
        "n_not_embedded": n_not_embedded,
        "n_image": n_image,
    }


def load_investigation_facts(conn, inv_id: str, limit: int = 0) -> list[dict]:
    """Return the embedded, stable, text facts of the investigation, with text + sources."""
    cur = conn.cursor(cursor_factory=psycopg2.extras.RealDictCursor)
    sql = """
        WITH inv_sources AS (SELECT source_id FROM okt_repository.investigation_sources WHERE investigation_id=%s)
        SELECT DISTINCT ON (f.id)
            f.id::text AS fact_id,
            f.text,
            f.fact_kind,
            f.status,
            f.embedded_at IS NOT NULL AS embedded,
            f.embedded_model
        FROM okt_repository.fact_sources fs
        JOIN okt_repository.facts f ON f.id = fs.fact_id
        WHERE fs.source_id IN (SELECT source_id FROM inv_sources)
          AND f.embedded_at IS NOT NULL
          AND f.status = 'stable'
          AND f.fact_kind = 'text'
        ORDER BY f.id
    """
    cur.execute(sql, (inv_id,))
    rows = cur.fetchall()
    if limit and limit > 0:
        rows = rows[:limit]
    return rows


def load_fact_sources(conn, fact_ids: list[str]) -> dict[str, list[str]]:
    """Return source titles (parsed_title or parsed_sitename fallback) per fact_id."""
    if not fact_ids:
        return {}
    cur = conn.cursor()
    cur.execute(
        """
        SELECT fs.fact_id::text, COALESCE(s.parsed_title, s.parsed_sitename, s.url)
        FROM okt_repository.fact_sources fs
        JOIN okt_repository.sources s ON s.id = fs.source_id
        WHERE fs.fact_id = ANY(%s::uuid[])
        """,
        (fact_ids,),
    )
    out: dict[str, list[str]] = collections.defaultdict(list)
    for fact_id, title in cur.fetchall():
        out[fact_id].append(title or "")
    return out


def load_fact_sources_full(conn, fact_ids: list[str]) -> dict[str, list[str]]:
    """Return source_ids per fact_id (full mapping, not titles)."""
    if not fact_ids:
        return {}
    cur = conn.cursor()
    cur.execute(
        """
        SELECT fs.fact_id::text, fs.source_id::text
        FROM okt_repository.fact_sources fs
        WHERE fs.fact_id = ANY(%s::uuid[])
        """,
        (fact_ids,),
    )
    out: dict[str, list[str]] = collections.defaultdict(list)
    for fact_id, source_id in cur.fetchall():
        out[fact_id].append(source_id)
    return out


def load_fact_texts(conn, fact_ids: list[str]) -> dict[str, str]:
    if not fact_ids:
        return {}
    cur = conn.cursor()
    cur.execute(
        "SELECT id::text, text FROM okt_repository.facts WHERE id = ANY(%s::uuid[])",
        (fact_ids,),
    )
    return {fid: text for fid, text in cur.fetchall()}


# ---------------------------------------------------------------------------
# Qdrant sweep


def fetch_neighbors(
    client: QdrantClient,
    collection: str,
    repo_id: str,
    fact_id: str,
    top_k: int,
) -> list[tuple[str, float]]:
    """Return [(neighbor_id, score)] for the nearest neighbors of fact_id (excluding self)."""
    q = models.RecommendQuery(recommend=models.RecommendInput(positive=[fact_id]))
    res = client.query_points(
        collection_name=collection,
        query=q,
        query_filter=models.Filter(
            must=[models.FieldCondition(key="repository_id", match=models.MatchValue(value=repo_id))],
            must_not=[models.HasIdCondition(has_id=[fact_id])],
        ),
        limit=top_k,
        score_threshold=0.0,
        with_payload=False,
        with_vectors=False,
    )
    return [(str(p.id), float(p.score)) for p in res.points]


def sweep(
    client: QdrantClient,
    collection: str,
    repo_id: str,
    fact_ids: list[str],
    top_k: int,
    concurrency: int,
    on_progress,
) -> dict[str, list[tuple[str, float]]]:
    """For each fact_id, return its list of (neighbor_id, score)."""
    results: dict[str, list[tuple[str, float]]] = {}
    results_lock = __import__("threading").Lock()
    done = [0]
    total = len(fact_ids)

    def worker(fid: str):
        try:
            return fid, fetch_neighbors(client, collection, repo_id, fid, top_k)
        except Exception as e:
            return fid, [("__error__", 0.0, str(e)[:200])]  # type: ignore
        finally:
            done[0] += 1
            if done[0] % 50 == 0 or done[0] == total:
                on_progress(done[0], total)

    with cf.ThreadPoolExecutor(max_workers=concurrency) as ex:
        for fut in cf.as_completed([ex.submit(worker, fid) for fid in fact_ids]):
            fid, neigh = fut.result()
            with results_lock:
                results[fid] = neigh
    return results


# ---------------------------------------------------------------------------
# Analysis


@dataclasses.dataclass
class ThresholdStat:
    threshold: float
    facts_with_neighbor_above: int
    facts_marked_to_delete: int  # estimate = facts_with_neighbor_above (greedy)
    cluster_count: int  # connected components >= 2 facts
    facts_in_clusters: int
    survivors: int
    reduction_pct: float
    # Cross-source-only view: a fact is deduped only if it has a neighbor
    # ≥ T whose source set is disjoint from its own. The current
    # `deduplicate_facts` worker only ever merges across different
    # sources (same-source duplicates survive), so this column is the
    # honest "what the current worker would actually do at T" estimate.
    facts_with_cross_source_neighbor_above: int
    cross_source_survivors: int
    cross_source_reduction_pct: float


def analyze_thresholds(
    fact_ids: list[str],
    neighbor_map: dict[str, list[tuple[str, float]]],
    thresholds: list[float],
    fact_source_ids: dict[str, list[str]] | None = None,
) -> list[ThresholdStat]:
    """For each threshold, build the graph (fact → neighbor with score>=T) and compute stats.

    Note: neighbor_map's neighbors are facts from the SAME repo but may be
    OUTSIDE the investigation. We track both views:
      - facts_with_neighbor_above: investigation facts whose nearest neighbor
        (anywhere in repo) is >= T. Upper bound on dedup (ignores source constraints).
      - facts_with_cross_source_neighbor_above: same, but only counting
        neighbors whose source set is disjoint from the fact's. This matches
        the current deduplicate_facts worker behavior (which only merges
        across different sources).
      - connected components: built only over edges where BOTH endpoints are
        in the investigation set — this approximates cluster shape locally.
    """
    inv_set = set(fact_ids)

    stats = []
    for T in thresholds:
        facts_with_neighbor_above = 0
        facts_with_cross_source_neighbor_above = 0
        edges: list[tuple[str, str, float]] = []
        for fid in fact_ids:
            neigh = neighbor_map.get(fid, [])
            above = [(n, s) for (n, s) in neigh if n != "__error__" and s >= T]
            if above:
                facts_with_neighbor_above += 1
            if fact_source_ids is not None and fid in fact_source_ids:
                my_sources = set(fact_source_ids.get(fid, []))
                cross = False
                for n, s in above:
                    if n in fact_source_ids:
                        their_sources = set(fact_source_ids.get(n, []))
                        if my_sources.isdisjoint(their_sources):
                            cross = True
                            break
                    else:
                        # Neighbor not in the investigation (or no source
                        # mapping); assume cross-source to avoid over-counting
                        # the worker's restrictive behavior.
                        cross = True
                        break
                if cross:
                    facts_with_cross_source_neighbor_above += 1
            # only edges within the investigation
            for n, s in above:
                if n in inv_set:
                    edges.append((fid, n, s))
        # connected components via union-find
        parent = {fid: fid for fid in fact_ids}

        def find(x):
            while parent[x] != x:
                parent[x] = parent[parent[x]]
                x = parent[x]
            return x

        def union(a, b):
            ra, rb = find(a), find(b)
            if ra != rb:
                parent[ra] = rb

        for a, b, _ in edges:
            union(a, b)
        comp_sizes = collections.Counter(find(fid) for fid in fact_ids)
        cluster_count = sum(1 for _root, sz in comp_sizes.items() if sz >= 2)
        facts_in_clusters = sum(sz for _root, sz in comp_sizes.items() if sz >= 2)
        survivors = len(fact_ids) - facts_with_neighbor_above  # greedy estimate
        reduction_pct = 100.0 * facts_with_neighbor_above / max(1, len(fact_ids))
        cross_source_survivors = len(fact_ids) - facts_with_cross_source_neighbor_above
        cross_source_reduction_pct = 100.0 * facts_with_cross_source_neighbor_above / max(1, len(fact_ids))
        stats.append(
            ThresholdStat(
                threshold=T,
                facts_with_neighbor_above=facts_with_neighbor_above,
                facts_marked_to_delete=facts_with_neighbor_above,
                cluster_count=cluster_count,
                facts_in_clusters=facts_in_clusters,
                survivors=survivors,
                reduction_pct=reduction_pct,
                facts_with_cross_source_neighbor_above=facts_with_cross_source_neighbor_above,
                cross_source_survivors=cross_source_survivors,
                cross_source_reduction_pct=cross_source_reduction_pct,
            )
        )
    return stats


def nearest_neighbor_scores(neighbor_map: dict[str, list[tuple[str, float]]]) -> list[float]:
    """Top-1 nearest-neighbor score per fact (only facts that returned at least one neighbor)."""
    out = []
    for fid, neigh in neighbor_map.items():
        valid = [(n, s) for (n, s) in neigh if n != "__error__"]
        if valid:
            out.append(max(s for _, s in valid))
    return out


def histogram(scores: list[float], bin_edges: list[float]) -> list[tuple[float, float, int]]:
    """Return [(low, high, count)] for each bin edge pair."""
    out = []
    for lo, hi in zip(bin_edges[:-1], bin_edges[1:]):
        n = sum(1 for s in scores if lo <= s < hi)
        out.append((lo, hi, n))
    # catch the top edge inclusive
    if bin_edges:
        lo, hi = bin_edges[-2], bin_edges[-1]
        n = sum(1 for s in scores if lo <= s <= hi)
        out[-1] = (lo, hi, n)
    return out


def example_pairs_per_band(
    fact_ids: list[str],
    neighbor_map: dict[str, list[tuple[str, float]]],
    fact_text: dict[str, str],
    fact_sources: dict[str, list[str]],
    thresholds: list[float],
    n_per_band: int,
    seed: int,
) -> list[dict]:
    """For each threshold T, return up to n_per_band example (fact, neighbor, score) tuples
    whose score is in [T, T+band_size) — to surface what the merges look like at that level."""
    rng = random.Random(seed)
    # Build candidate pairs: (factA, factB, score) for the strongest neighbor
    # of each fact (one pair per fact, to avoid over-weighting boilerplate).
    pairs = []
    for fid in fact_ids:
        neigh = neighbor_map.get(fid, [])
        valid = [(n, s) for (n, s) in neigh if n != "__error__"]
        if not valid:
            continue
        # pick the highest-scoring neighbor (top-1)
        valid.sort(key=lambda x: -x[1])
        n, s = valid[0]
        pairs.append((fid, n, s))

    bands = []
    for i, T in enumerate(thresholds):
        # band = [T, next_T) ; last band extends to 1.0
        hi = thresholds[i + 1] if i + 1 < len(thresholds) else 1.01
        in_band = [(a, b, s) for (a, b, s) in pairs if T <= s < hi]
        rng.shuffle(in_band)
        chosen = in_band[:n_per_band]
        examples = []
        for a, b, s in chosen:
            examples.append(
                {
                    "fact_a": a,
                    "fact_b": b,
                    "score": s,
                    "text_a": (fact_text.get(a) or "")[:400],
                    "text_b": (fact_text.get(b) or "")[:400],
                    "sources_a": fact_sources.get(a, []),
                    "sources_b": fact_sources.get(b, []),
                }
            )
        bands.append({"threshold": T, "band_high": hi, "n_in_band": len(in_band), "examples": examples})
    return bands


# ---------------------------------------------------------------------------
# HTML report


HTML_TEMPLATE = """<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<title>Dedup threshold sweep — {inv_title}</title>
<style>
  body {{ font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, sans-serif; max-width: 1200px; margin: 1.5em auto; padding: 0 1em; color: #1a1a1a; line-height: 1.5; }}
  h1, h2, h3 {{ color: #2c3e50; }}
  h1 {{ border-bottom: 3px solid #2c3e50; padding-bottom: .3em; }}
  h2 {{ margin-top: 2em; border-bottom: 1px solid #ccc; padding-bottom: .2em; }}
  table {{ border-collapse: collapse; width: 100%; margin: 1em 0; font-size: 0.92em; }}
  th, td {{ padding: 6px 10px; border: 1px solid #ccc; text-align: left; vertical-align: top; }}
  th {{ background: #f4f6f8; }}
  tr:nth-child(even) td {{ background: #fafbfc; }}
  code {{ background: #f4f6f8; padding: 1px 4px; border-radius: 3px; font-size: 0.9em; }}
  .stat-card {{ display: inline-block; background: #f4f6f8; border: 1px solid #ddd; padding: .8em 1.2em; margin: .5em .8em .5em 0; border-radius: 6px; }}
  .stat-card .v {{ font-size: 1.6em; font-weight: 600; color: #2c3e50; display: block; }}
  .stat-card .l {{ color: #666; font-size: 0.85em; text-transform: uppercase; letter-spacing: 0.05em; }}
  .bar-container {{ position: relative; height: 18px; background: #eef0f2; border-radius: 3px; overflow: hidden; min-width: 200px; }}
  .bar {{ position: absolute; left: 0; top: 0; bottom: 0; background: linear-gradient(90deg, #4a90e2, #2c3e50); }}
  .pair {{ border-left: 4px solid #888; padding: .5em 1em; margin: .8em 0; background: #fafbfc; }}
  .pair.high {{ border-left-color: #2ecc40; }}
  .pair.mid  {{ border-left-color: #ffcc00; }}
  .pair.low  {{ border-left-color: #ff4136; }}
  .pair .score {{ font-weight: 600; color: #2c3e50; }}
  .pair .text {{ font-family: "SF Mono", Menlo, Consolas, monospace; font-size: 0.86em; color: #333; margin: .3em 0; white-space: pre-wrap; word-break: break-word; }}
  .pair .src  {{ font-size: 0.78em; color: #666; margin-top: .2em; }}
  .muted {{ color: #666; font-size: 0.85em; }}
  .tag {{ display: inline-block; padding: 1px 6px; background: #eef0f2; border-radius: 3px; font-size: 0.78em; color: #444; margin-right: 4px; }}
  .tag.cur {{ background: #2c3e50; color: #fff; }}
  .footer {{ margin-top: 3em; padding-top: 1em; border-top: 1px solid #ccc; color: #666; font-size: 0.85em; }}
  .heatmap-cell {{ display: inline-block; width: 16px; height: 16px; margin-right: 2px; border-radius: 2px; }}
</style>
</head>
<body>
{body}
</body>
</html>
"""


def esc(s) -> str:
    if s is None:
        return ""
    return html.escape(str(s))


def truncate(s: str, n: int = 400) -> str:
    if len(s) <= n:
        return s
    return s[: n - 1] + "…"


def render_html(
    inv: dict,
    args: argparse.Namespace,
    fact_ids: list[str],
    neighbor_map: dict[str, list[tuple[str, float]]],
    stats: list[ThresholdStat],
    bands: list[dict],
    scores_top1: list[float],
    bin_edges: list[float],
    hist: list[tuple[float, float, int]],
    runtime_seconds: float,
    repo_id: str,
    embedded_model: str,
) -> str:
    body_parts = []
    # Title
    body_parts.append(f"<h1>Dedup threshold sweep — {esc(inv['title'])}</h1>")
    body_parts.append(f"<p class='muted'>{esc(inv['topic'])}</p>")

    # Stat cards
    body_parts.append("<div>")
    body_parts.append(f"<div class='stat-card'><span class='v'>{inv['n_sources']:,}</span><span class='l'>Sources in investigation</span></div>")
    body_parts.append(f"<div class='stat-card'><span class='v'>{inv['n_facts']:,}</span><span class='l'>Total facts</span></div>")
    body_parts.append(f"<div class='stat-card'><span class='v'>{inv['n_embedded_text_stable']:,}</span><span class='l'>Embedded text-stable (swept)</span></div>")
    body_parts.append(f"<div class='stat-card'><span class='v'>{inv['n_not_embedded']:,}</span><span class='l'>Not embedded (excluded)</span></div>")
    body_parts.append(f"<div class='stat-card'><span class='v'>{inv['n_image']:,}</span><span class='l'>Image facts (excluded)</span></div>")
    body_parts.append(f"<div class='stat-card'><span class='v'>{esc(embedded_model) or '—'}</span><span class='l'>Embedding model</span></div>")
    body_parts.append(f"<div class='stat-card'><span class='v'>{len(fact_ids):,}</span><span class='l'>Facts scanned</span></div>")
    body_parts.append(f"<div class='stat-card'><span class='v'>{runtime_seconds:.1f}s</span><span class='l'>Sweep runtime</span></div>")
    body_parts.append("</div>")

    # Methodology
    body_parts.append("<h2>Method</h2>")
    body_parts.append(
        f"<p>For each of the <b>{len(fact_ids):,}</b> embedded text-stable facts in the investigation, "
        f"the script queries Qdrant (<code>{esc(args.qdrant_host)}:{args.qdrant_port}</code>, collection "
        f"<code>{esc(args.qdrant_collection)}</code>) for the top-{args.top_k} nearest neighbors within "
        f"the same repository (<code>{esc(repo_id)}</code>), excluding self, with <code>score_threshold=0</code> "
        f"so every neighbor is returned regardless of score. This replays the <code>deduplicate_facts</code> "
        f"worker's <code>SearchSimilarByID</code> logic at concurrency={args.concurrency}.</p>"
        f"<p>For each candidate threshold <code>T</code>, a fact is counted as <i>would be deduped</i> if at least "
        f"one of its neighbors has cosine similarity ≥ <code>T</code>. Connected components over edges with both "
        f"endpoints inside the investigation approximate cluster shape (this is an upper bound on the greedy worker, "
        f"which marks losers one pass at a time). The current production threshold is "
        f"<code>dedup.threshold: 0.94</code>.</p>"
        f"<p class='muted'>Read-only: no DB writes, no Qdrant upserts/deletes, no advisory locks. "
        f"Generated {dt.datetime.now().isoformat(timespec='seconds')}.</p>"
    )

    # Nearest-neighbor score histogram
    body_parts.append("<h2>Nearest-neighbor score distribution</h2>")
    body_parts.append(
        f"<p>Distribution of the top-1 nearest-neighbor cosine score across the {len(scores_top1):,} facts "
        f"that returned at least one neighbor. A high count in the 0.95–1.00 bins indicates many near-duplicate "
        f"facts; a long tail in the 0.80–0.90 range indicates paraphrases or topically-related-but-distinct facts.</p>"
    )
    max_count = max((c for _, _, c in hist), default=1) or 1
    body_parts.append("<table><thead><tr><th>Score bin</th><th>Count</th><th>Bar</th><th>% of swept</th></tr></thead><tbody>")
    for lo, hi, c in hist:
        pct = 100.0 * c / max(1, len(scores_top1))
        bar_w = 100.0 * c / max_count
        body_parts.append(
            f"<tr><td>[{lo:.2f}, {hi:.2f})</td><td>{c:,}</td>"
            f"<td><div class='bar-container'><div class='bar' style='width:{bar_w:.1f}%'></div></div></td>"
            f"<td>{pct:.2f}%</td></tr>"
        )
    body_parts.append("</tbody></table>")

    # Threshold table
    body_parts.append("<h2>Per-threshold estimate</h2>")
    body_parts.append(
        "<p>For each candidate threshold <code>T</code>, two views:</p>"
        "<ul>"
        "<li><b>Any-neighbor</b> — facts whose nearest neighbor (anywhere in the repo) is ≥ T. "
        "This is the <i>upper bound</i> on what a threshold-only dedup could collapse if it ignored "
        "source attribution.</li>"
        "<li><b>Cross-source only</b> — facts whose nearest neighbor ≥ T comes from a "
        "<i>disjoint</i> set of sources. This matches the current <code>deduplicate_facts</code> worker, "
        "which only ever merges facts across different sources — two near-duplicate facts extracted "
        "from the <i>same</i> source are never merged.</li>"
        "</ul>"
        "<p>Connected components are computed over edges where both endpoints are inside the "
        "investigation set (this approximates cluster shape; the greedy worker marks losers one "
        "pass at a time, so this is an upper bound). The current production threshold is "
        "<code>dedup.threshold: 0.94</code>.</p>"
    )
    body_parts.append(
        "<table><thead><tr>"
        "<th rowspan='2'>Threshold</th>"
        "<th colspan='3'>Any-neighbor (upper bound)</th>"
        "<th colspan='3'>Cross-source only (current worker)</th>"
        "<th rowspan='2'>Inv. clusters (≥2)</th>"
        "<th rowspan='2'>Facts in clusters</th>"
        "</tr><tr>"
        "<th>Facts w/ neighbor ≥ T</th><th>Survivors</th><th>Reduction %</th>"
        "<th>Facts deduped</th><th>Survivors</th><th>Reduction %</th>"
        "</tr></thead><tbody>"
    )
    for s in stats:
        is_cur = abs(s.threshold - 0.94) < 1e-9
        tag = " <span class='tag cur'>current</span>" if is_cur else ""
        body_parts.append(
            f"<tr><td><b>{s.threshold:.2f}</b>{tag}</td>"
            f"<td>{s.facts_with_neighbor_above:,}</td>"
            f"<td>{s.survivors:,}</td>"
            f"<td>{s.reduction_pct:.2f}%</td>"
            f"<td>{s.facts_with_cross_source_neighbor_above:,}</td>"
            f"<td>{s.cross_source_survivors:,}</td>"
            f"<td>{s.cross_source_reduction_pct:.2f}%</td>"
            f"<td>{s.cluster_count:,}</td>"
            f"<td>{s.facts_in_clusters:,}</td></tr>"
        )
    body_parts.append("</tbody></table>")

    # Score quantiles
    if scores_top1:
        qs = statistics.quantiles(scores_top1, n=10, method="inclusive")
        body_parts.append("<h3>Top-1 nearest-neighbor score quantiles</h3>")
        body_parts.append("<table><thead><tr><th>Min</th><th>P10</th><th>P20</th><th>P30</th><th>P40</th><th>P50</th><th>P60</th><th>P70</th><th>P80</th><th>P90</th><th>Max</th></tr></thead><tbody><tr>")
        body_parts.append(f"<td>{min(scores_top1):.3f}</td>")
        for q in qs:
            body_parts.append(f"<td>{q:.3f}</td>")
        body_parts.append(f"<td>{max(scores_top1):.3f}</td>")
        body_parts.append("</tr></tbody></table>")

    # Examples per band
    body_parts.append("<h2>Example pairs at each threshold band</h2>")
    body_parts.append(
        "<p>For each threshold band, up to " + str(args.examples_per_band) + " example (fact, nearest-neighbor, score) "
        "triples are shown so an operator can eyeball whether the merges look correct at that level. <span class='tag' style='background:#2ecc40'>green</span> = near-identical text, "
        "<span class='tag' style='background:#ffcc00'>yellow</span> = paraphrase / overlapping subject, "
        "<span class='tag' style='background:#ff4136'>red</span> = likely distinct facts (false-positive risk if threshold is set here).</p>"
    )
    for band in bands:
        T = band["threshold"]
        hi = band["band_high"]
        n = band["n_in_band"]
        is_cur = abs(T - 0.94) < 1e-9
        tag = " <span class='tag cur'>current</span>" if is_cur else ""
        body_parts.append(f"<h3>Band [{T:.2f}, {hi:.2f}){tag}</h3>")
        body_parts.append(f"<p class='muted'>{n:,} facts have their top-1 nearest neighbor in this band.</p>")
        if not band["examples"]:
            body_parts.append("<p class='muted'>No examples in this band.</p>")
            continue
        for ex in band["examples"]:
            if ex["score"] >= 0.95:
                cls = "high"
            elif ex["score"] >= 0.88:
                cls = "mid"
            else:
                cls = "low"
            srcs_a = "".join("<span class='tag'>" + esc(s) + "</span>" for s in ex["sources_a"][:5])
            srcs_b = "".join("<span class='tag'>" + esc(s) + "</span>" for s in ex["sources_b"][:5])
            none_span = "<span class='muted'>none</span>"
            body_parts.append("<div class='pair " + cls + "'>")
            body_parts.append("<div class='score'>score = " + f"{ex['score']:.4f}" + "</div>")
            body_parts.append("<div class='text'>A: " + esc(truncate(ex["text_a"])) + "</div>")
            body_parts.append("<div class='src'>A sources: " + (srcs_a or none_span) + "</div>")
            body_parts.append("<div class='text'>B: " + esc(truncate(ex["text_b"])) + "</div>")
            body_parts.append("<div class='src'>B sources: " + (srcs_b or none_span) + "</div>")
            body_parts.append("<div class='muted'>fact_a=" + esc(ex["fact_a"]) + " · fact_b=" + esc(ex["fact_b"]) + "</div>")
            body_parts.append("</div>")

    # Footer
    body_parts.append("<div class='footer'>")
    body_parts.append(f"<p>Generated by <code>scripts/experiments/dedup-threshold-sweep/dedup_threshold_sweep.py</code></p>")
    body_parts.append(f"<p>Args: <code>{esc(' '.join(sys.argv[1:]))}</code></p>")
    body_parts.append(f"<p>Investigation: <code>{esc(inv['id'])}</code> · Repo: <code>{esc(repo_id)}</code></p>")
    body_parts.append("</div>")

    return HTML_TEMPLATE.format(inv_title=esc(inv["title"]), body="".join(body_parts))


# ---------------------------------------------------------------------------
# Main


def main() -> int:
    args = parse_args()
    thresholds = [float(t) for t in args.thresholds.split(",")]
    thresholds.sort()

    print(f"[+] connecting to postgres {args.pg_host}:{args.pg_port}/{args.pg_db}", file=sys.stderr)
    conn = pg_connect(args)
    inv = load_investigation(conn, args.investigation)
    print(
        f"[+] investigation: {inv['title']!r}  sources={inv['n_sources']}  facts={inv['n_facts']}  "
        f"embedded_text_stable={inv['n_embedded_text_stable']}",
        file=sys.stderr,
    )

    if inv["n_embedded_text_stable"] == 0:
        print("[!] no embedded text-stable facts in this investigation; nothing to sweep", file=sys.stderr)
        return 1

    facts = load_investigation_facts(conn, args.investigation, limit=args.max_facts)
    fact_ids = [f["fact_id"] for f in facts]
    print(f"[+] loaded {len(fact_ids)} embedded text-stable facts", file=sys.stderr)

    # Repo id (single repo for now)
    repo_id = "3028db69-f2bd-49f8-a7b7-f2c7cc705278"
    # Resolve dynamically from the investigation's sources
    cur = conn.cursor()
    cur.execute(
        """
        SELECT s.repository_id::text
        FROM okt_repository.investigation_sources isrc
        JOIN okt_repository.sources s ON s.id = isrc.source_id
        WHERE isrc.investigation_id = %s
        LIMIT 1
        """,
        (args.investigation,),
    )
    row = cur.fetchone()
    if row:
        repo_id = row[0]
    print(f"[+] repo_id = {repo_id}", file=sys.stderr)

    embedded_model = facts[0]["embedded_model"] if facts else ""

    # Qdrant
    print(f"[+] connecting to qdrant {args.qdrant_host}:{args.qdrant_port} collection={args.qdrant_collection}", file=sys.stderr)
    client = QdrantClient(host=args.qdrant_host, port=args.qdrant_port)

    # Sweep
    print(f"[+] sweeping top-{args.top_k} neighbors per fact, concurrency={args.concurrency}", file=sys.stderr)
    t0 = time.time()

    def progress(done, total):
        pct = 100.0 * done / total
        elapsed = time.time() - t0
        rate = done / max(0.001, elapsed)
        eta = (total - done) / max(0.001, rate)
        print(f"  [{done}/{total}] {pct:.1f}%  elapsed={elapsed:.1f}s  rate={rate:.1f} q/s  eta={eta:.1f}s", file=sys.stderr)

    neighbor_map = sweep(client, args.qdrant_collection, repo_id, fact_ids, args.top_k, args.concurrency, progress)
    runtime = time.time() - t0
    print(f"[+] sweep finished in {runtime:.1f}s", file=sys.stderr)

    # Analysis
    # Load source_ids for all investigation facts + every neighbor that
    # appears at score >= min(thresholds), so the cross-source view can
    # accurately tell same-source duplicates (which the current worker
    # never merges) from cross-source duplicates (which it does merge).
    print("[+] loading fact source_ids for cross-source view", file=sys.stderr)
    min_t = min(thresholds) if thresholds else 0.0
    neighbor_ids_for_sources = set(fact_ids)
    for fid, neigh in neighbor_map.items():
        for n, s in neigh:
            if n != "__error__" and s >= min_t:
                neighbor_ids_for_sources.add(n)
    fact_source_ids = load_fact_sources_full(conn, list(neighbor_ids_for_sources))
    stats = analyze_thresholds(fact_ids, neighbor_map, thresholds, fact_source_ids)
    scores_top1 = nearest_neighbor_scores(neighbor_map)
    bin_edges = [0.40, 0.50, 0.60, 0.70, 0.75, 0.80, 0.82, 0.84, 0.86, 0.88, 0.90, 0.91, 0.92, 0.93, 0.94, 0.95, 0.96, 0.97, 0.98, 0.99, 1.01]
    hist = histogram(scores_top1, bin_edges)

    # Need fact text + sources for example pairs (only load the ones we'll show)
    print("[+] loading fact texts + sources for example pairs", file=sys.stderr)
    needed_ids = set(fact_ids)
    # also include neighbor ids that appear in examples
    for band in (bands_pre := example_pairs_per_band(
        fact_ids, neighbor_map, {}, {}, thresholds, args.examples_per_band, args.seed
    )):
        for ex in band["examples"]:
            needed_ids.add(ex["fact_a"])
            needed_ids.add(ex["fact_b"])
    fact_text = load_fact_texts(conn, list(needed_ids))
    fact_sources = load_fact_sources(conn, list(needed_ids))
    bands = example_pairs_per_band(
        fact_ids, neighbor_map, fact_text, fact_sources, thresholds, args.examples_per_band, args.seed
    )

    print(f"[+] rendering HTML report to {args.out}", file=sys.stderr)
    html_doc = render_html(
        inv, args, fact_ids, neighbor_map, stats, bands, scores_top1, bin_edges, hist, runtime, repo_id, embedded_model
    )
    with open(args.out, "w", encoding="utf-8") as f:
        f.write(html_doc)

    # Also dump raw data as JSON for re-use
    json_path = args.out.rsplit(".", 1)[0] + ".json"
    with open(json_path, "w", encoding="utf-8") as f:
        json.dump(
            {
                "investigation": inv,
                "repo_id": repo_id,
                "embedded_model": embedded_model,
                "facts_scanned": len(fact_ids),
                "top_k": args.top_k,
                "runtime_seconds": runtime,
                "thresholds": thresholds,
                "threshold_stats": [dataclasses.asdict(s) for s in stats],
                "score_histogram": [{"low": l, "high": h, "count": c} for (l, h, c) in hist],
                "score_quantiles": (
                    statistics.quantiles(scores_top1, n=10, method="inclusive") if scores_top1 else []
                ),
                "score_min": min(scores_top1) if scores_top1 else None,
                "score_max": max(scores_top1) if scores_top1 else None,
                "score_mean": statistics.fmean(scores_top1) if scores_top1 else None,
                "score_stdev": statistics.pstdev(scores_top1) if len(scores_top1) > 1 else None,
                "neighbor_map": {fid: [[n, s] for (n, s) in neigh] for fid, neigh in neighbor_map.items()},
            },
            f,
            indent=2,
            default=str,
        )
    print(f"[+] raw data: {json_path}", file=sys.stderr)
    print(f"[+] done. open {args.out}", file=sys.stderr)
    return 0


if __name__ == "__main__":
    sys.exit(main())