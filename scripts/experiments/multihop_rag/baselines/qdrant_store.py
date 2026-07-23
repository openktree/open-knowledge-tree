"""Standalone Qdrant client for the MultiHop-RAG baselines.

Connects to the already-running Qdrant instance (default localhost:6334,
same as OKT) and manages TWO standalone collections:
  - multihoprag_passages      (Traditional RAG: fixed-size passage chunks)
  - multihoprag_propositions  (Dense X: proposition units)

These are SEPARATE from OKT's okt_facts collection. The baselines retrieve
dense-only (cosine similarity, no lexical tsvector, no RRF fusion) so the
comparison isolates the retrieval strategy from OKT's hybrid path.

Payload schema (stored on every point):
  {
    "doc_id":      str,   # the source article slug (filename stem)
    "title":       str,   # article title (from YAML frontmatter)
    "source":      str,   # publication name (e.g. "TechCrunch")
    "author":      str,
    "published_at": str,  # ISO date string
    "chunk_index": int,   # 0-based index within the document
    "text":        str,   # the chunk/proposition text (for the synthesis prompt)
    "kind":        str,   # "passage" or "proposition"
  }
We store the text + source metadata in the payload (unlike OKT, which keeps
Postgres as the source of truth) because the baselines have no Postgres
dependency — Qdrant is the only store.

Point ids are deterministic UUIDs derived from (doc_id, chunk_index) so
re-running index_build.py is idempotent (upsert overwrites).
"""
from __future__ import annotations

import sys
import time
from typing import Any

from qdrant_client import QdrantClient
from qdrant_client.http import models as qm

import config


class QdrantError(RuntimeError):
    pass


def _client() -> QdrantClient:
    kwargs: dict[str, Any] = {
        "host": config.BASELINE_QDRANT_HOST,
        "port": config.BASELINE_QDRANT_PORT,
    }
    if config.BASELINE_QDRANT_API_KEY:
        kwargs["api_key"] = config.BASELINE_QDRANT_API_KEY
    return QdrantClient(**kwargs)


def _ensure_collection(client: QdrantClient, name: str) -> None:
    """Create the collection if it doesn't exist. Idempotent."""
    collections = {c.name for c in client.get_collections().collections}
    if name in collections:
        return
    client.create_collection(
        collection_name=name,
        vectors_config=qm.VectorParams(
            size=config.BASELINE_EMBEDDING_DIMENSIONS,
            distance=qm.Distance.COSINE,
        ),
    )


def ensure_collections() -> None:
    """Create both baseline collections if missing. Idempotent."""
    c = _client()
    _ensure_collection(c, config.BASELINE_PASSAGE_COLLECTION)
    _ensure_collection(c, config.BASELINE_PROPOSITION_COLLECTION)


def drop_collection(name: str) -> None:
    """Drop a collection if it exists. Used by index_build.py --rebuild."""
    c = _client()
    try:
        c.delete_collection(collection_name=name)
    except Exception:  # noqa: BLE001
        pass


def collection_count(name: str) -> int:
    """Approximate point count in a collection (for the index-build log)."""
    c = _client()
    try:
        info = c.count_points(collection_name=name)
        return int(info.count) if info and info.count else 0
    except Exception:  # noqa: BLE001
        return 0


def _uuid_for(doc_id: str, chunk_index: int, text: str = "") -> str:
    """Deterministic point id so re-upserts overwrite the same point
    (idempotent index rebuilds).

    For passages (one chunk per (doc_id, chunk_index)), the text is
    empty and the id is derived from (doc_id, chunk_index) — unique.
    For propositions, many propositions share the same (doc_id,
    chunk_index) (they all come from one passage), so the text MUST
    be included in the key — otherwise they'd all collapse to one
    point and overwrite each other. Including the proposition text in
    the uuid5 input makes each proposition a distinct point.
    """
    import uuid

    NAMESPACE = uuid.NAMESPACE_URL  # a well-known fixed namespace
    key = f"{doc_id}::{chunk_index}"
    if text:
        key = f"{key}::{text}"
    return str(uuid.uuid5(NAMESPACE, key))


def upsert_points(
    collection: str,
    points: list[dict[str, Any]],
    batch_size: int = 64,
) -> None:
    """Upsert a list of {doc_id, chunk_index, vector, text, ...metadata}
    points into the collection. Idempotent via deterministic point ids.

    Point id = uuid5(doc_id::chunk_index[::text]). Passages pass no
    text (one point per chunk); propositions pass their text (many
    propositions per chunk → text disambiguates).
    """
    if not points:
        return
    c = _client()
    for start in range(0, len(points), batch_size):
        batch = points[start : start + batch_size]
        qd_points = []
        for p in batch:
            pid = _uuid_for(p["doc_id"], p["chunk_index"], p.get("text", ""))
            payload = {
                "doc_id": p["doc_id"],
                "title": p.get("title", ""),
                "source": p.get("source", ""),
                "author": p.get("author", ""),
                "published_at": p.get("published_at", ""),
                "chunk_index": p["chunk_index"],
                "text": p.get("text", ""),
                "kind": p.get("kind", ""),
            }
            qd_points.append(
                qm.PointStruct(
                    id=pid,
                    vector=p["vector"],
                    payload=payload,
                )
            )
        last_err: Exception | None = None
        for attempt in range(config.MAX_RETRIES + 1):
            try:
                c.upsert(
                    collection_name=collection,
                    points=qd_points,
                    wait=True,
                )
                last_err = None
                break
            except Exception as e:  # noqa: BLE001
                last_err = e
                if attempt < config.MAX_RETRIES:
                    wait = 2.0 * (attempt + 1)
                    print(
                        f"  qdrant upsert batch [{start}:{start+len(batch)}] "
                        f"failed (attempt {attempt+1}/{config.MAX_RETRIES+1}), "
                        f"retrying in {wait:.0f}s: {e}",
                        file=sys.stderr,
                    )
                    time.sleep(wait)
                    continue
                raise QdrantError(
                    f"qdrant upsert failed after retries: {last_err}"
                    )
            if last_err:
                raise QdrantError(f"qdrant upsert failed: {last_err}")


def search(
    collection: str,
    query_vector: list[float],
    limit: int,
    min_score: float = 0.0,
) -> list[dict[str, Any]]:
    """Dense-only cosine search. Returns up to `limit` hits as dicts:
      {id, score, doc_id, title, source, author, published_at,
       chunk_index, text, kind}
    Sorted by score descending (Qdrant returns them that way). Filters
    out hits below min_score.
    """
    c = _client()
    try:
        results = c.search(
            collection_name=collection,
            query_vector=query_vector,
            limit=limit,
            score_threshold=min_score if min_score > 0 else None,
        )
    except Exception as e:  # noqa: BLE001
        raise QdrantError(f"qdrant search failed: {e}")
    out: list[dict[str, Any]] = []
    for hit in results:
        payload = hit.payload or {}
        out.append(
            {
                "id": hit.id,
                "score": float(hit.score),
                "doc_id": payload.get("doc_id", ""),
                "title": payload.get("title", ""),
                "source": payload.get("source", ""),
                "author": payload.get("author", ""),
                "published_at": payload.get("published_at", ""),
                "chunk_index": int(payload.get("chunk_index", 0)),
                "text": payload.get("text", ""),
                "kind": payload.get("kind", ""),
            }
        )
    return out