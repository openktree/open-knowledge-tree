"""Read-only async DB access for the graph-analysis experiments.

Connects to the dev Postgres (port 5432) where the `multihoprag` repository
lives. The AGENTS.md prohibition is about the e2e harness that DROPs schemas
on the test DB (port 5433); these experiments are pure read-only SELECTs
against the dev DB, the same data the existing `multihop_rag` experiments
read via the API on :8080 — direct SQL is just faster for full-graph dumps.

All functions are read-only: SELECT only, no writes, no schema changes.
"""
from __future__ import annotations

import asyncio
import os
from dataclasses import dataclass
from typing import Iterable

import asyncpg

DEFAULT_DSN = os.environ.get(
    "OKT_DB_DSN",
    "postgres://okt:okt_dev@localhost:5432/okt",
)
DEFAULT_REPO_SLUG = os.environ.get("OKT_REPO_SLUG", "multihoprag")


@dataclass
class ConceptGroup:
    """A concept group (all contexts sharing lower(canonical_name))."""
    name: str          # lower(canonical_name) — the grouping key
    fact_degree: int   # number of distinct facts linked to this group


@dataclass
class BipartiteEdge:
    """An edge in the fact↔concept bipartite graph."""
    fact_id: str
    concept_name: str  # lower(canonical_name)


@dataclass
class CooccurrenceEdge:
    """A weighted edge in the concept↔concept co-occurrence projection."""
    name_a: str
    name_b: str
    shared_fact_count: int


async def repo_id_for_slug(conn: asyncpg.Connection, slug: str) -> str:
    row = await conn.fetchrow(
        "SELECT id FROM okt_system.repositories WHERE slug = $1", slug
    )
    if row is None:
        raise ValueError(f"repository not found: {slug!r}")
    return str(row["id"])


async def fetch_concept_groups(
    conn: asyncpg.Connection, repo_id: str
) -> list[ConceptGroup]:
    """One row per concept group, with its fact-degree k_i."""
    rows = await conn.fetch(
        """
        SELECT lower(c.canonical_name) AS name,
               COUNT(DISTINCT fc.fact_id) AS k
        FROM okt_repository.fact_concepts fc
        JOIN okt_repository.concepts c ON c.id = fc.concept_id
        WHERE c.repository_id = $1
        GROUP BY lower(c.canonical_name)
        """,
        repo_id,
    )
    return [ConceptGroup(name=r["name"], fact_degree=r["k"]) for r in rows]


async def fetch_bipartite_edges(
    conn: asyncpg.Connection, repo_id: str
) -> list[BipartiteEdge]:
    """Every fact↔concept edge, deduplicated on (fact_id, concept_name)."""
    rows = await conn.fetch(
        """
        SELECT DISTINCT
               fc.fact_id::text AS fact_id,
               lower(c.canonical_name) AS concept_name
        FROM okt_repository.fact_concepts fc
        JOIN okt_repository.concepts c ON c.id = fc.concept_id
        WHERE c.repository_id = $1
        """,
        repo_id,
    )
    return [BipartiteEdge(fact_id=r["fact_id"], concept_name=r["concept_name"]) for r in rows]


async def fetch_cooccurrence_edges(
    conn: asyncpg.Connection, repo_id: str
) -> list[CooccurrenceEdge]:
    """Weighted concept↔concept edges from the concept_relations matview."""
    rows = await conn.fetch(
        """
        SELECT name_a, name_b, shared_fact_count
        FROM okt_repository.concept_relations
        WHERE repository_id = $1
        """,
        repo_id,
    )
    return [
        CooccurrenceEdge(
            name_a=r["name_a"], name_b=r["name_b"], shared_fact_count=r["shared_fact_count"]
        )
        for r in rows
    ]


async def load_graph(
    dsn: str = DEFAULT_DSN, repo_slug: str = DEFAULT_REPO_SLUG
) -> tuple[list[ConceptGroup], list[BipartiteEdge], list[CooccurrenceEdge], str]:
    """Load the full bipartite + projection graph for a repository.

    Returns (concept_groups, bipartite_edges, cooccurrence_edges, repo_id).
    """
    # asyncpg connections are single-operation-at-a-time, so run the three
    # fetches sequentially. Each is a single SELECT over ~95k rows and
    # completes in well under a second; parallelism would need a pool.
    conn = await asyncpg.connect(dsn)
    try:
        repo_id = await repo_id_for_slug(conn, repo_slug)
        groups = await fetch_concept_groups(conn, repo_id)
        bip = await fetch_bipartite_edges(conn, repo_id)
        cooc = await fetch_cooccurrence_edges(conn, repo_id)
        return groups, bip, cooc, repo_id
    finally:
        await conn.close()


__all__ = [
    "ConceptGroup",
    "BipartiteEdge",
    "CooccurrenceEdge",
    "repo_id_for_slug",
    "fetch_concept_groups",
    "fetch_bipartite_edges",
    "fetch_cooccurrence_edges",
    "load_graph",
    "DEFAULT_DSN",
    "DEFAULT_REPO_SLUG",
]