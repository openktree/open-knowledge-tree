"""Build the Qdrant collections for the MultiHop-RAG baselines.

Two collections, both built from the 609 corpus .md files in
dataset/corpus/:

  1. multihoprag_passages      (Traditional RAG)
     Fixed-size passage chunks (~512 tokens, 50 overlap) → embed each
     with google/gemini-embedding-2 → upsert into Qdrant.

  2. multihoprag_propositions  (Dense X Retrieval)
     Passage-chunk → propositionize (LLM or released model) → embed each
     proposition with google/gemini-embedding-2 → upsert into Qdrant.

Both collections use the SAME embedding model OKT uses
(google/gemini-embedding-2, 3072-dim) so the dense-retrieval comparison
isolates chunking + retrieval strategy, not the embedding model.

Idempotent: point ids are deterministic UUIDs derived from (doc_id,
chunk_index), so re-running overwrites the same points. Use --rebuild
to drop+recreate the collections first (when the chunk size or
propositionizer changes).

CLI:
  python3 baselines/index_build.py                  # build both
  python3 baselines/index_build.py --only passages   # just passages
  python3 baselines/index_build.py --only propositions
  python3 baselines/index_build.py --rebuild         # drop + recreate
  python3 baselines/index_build.py --embed-concurrency 8
"""
from __future__ import annotations

import argparse
import os
import sys
import time
from concurrent.futures import ThreadPoolExecutor, as_completed
from typing import Any

# The baseline scripts live in baselines/ but import the top-level
# experiment modules (config, llm). Add the parent dir to sys.path so
# `python3 baselines/index_build.py` works from anywhere.
sys.path.insert(0, os.path.dirname(os.path.dirname(os.path.abspath(__file__))))

from tqdm import tqdm

import config
from baselines import chunking, embeddings, qdrant_store


def _embed_and_upsert(
    chunks: list[dict[str, Any]],
    collection: str,
    embed_concurrency: int,
) -> None:
    """Embed each chunk's text and upsert into the Qdrant collection.

    Embeds in concurrent batches (each batch = config.BASELINE_EMBED_BATCH_SIZE
    texts) to keep the OpenRouter embeddings endpoint busy without
    overloading it. Upserts happen as each batch completes so memory
    stays bounded.
    """
    if not chunks:
        print(f"  no chunks to index for {collection}", file=sys.stderr)
        return
    bs = config.BASELINE_EMBED_BATCH_SIZE
    batches = [chunks[i : i + bs] for i in range(0, len(chunks), bs)]

    def _one(batch: list[dict[str, Any]]) -> int:
        texts = [c["text"] for c in batch]
        vectors, _ = embeddings.embed_texts(texts)
        if len(vectors) != len(batch):
            raise RuntimeError(
                f"embed count mismatch: {len(vectors)} != {len(batch)} "
                f"(some texts may exceed the model's context window)"
            )
        points = []
        for c, vec in zip(batch, vectors):
            p = dict(c)
            p["vector"] = vec
            points.append(p)
        qdrant_store.upsert_points(collection, points)
        return len(points)

    total = 0
    started = time.time()
    if embed_concurrency > 1:
        with ThreadPoolExecutor(max_workers=embed_concurrency) as ex:
            futures = {ex.submit(_one, b): b for b in batches}
            for fut in tqdm(
                as_completed(futures), total=len(futures), desc="embed+upsert"
            ):
                total += fut.result()
    else:
        for b in tqdm(batches, desc="embed+upsert"):
            total += _one(b)
    elapsed = time.time() - started
    print(
        f"  indexed {total} points into {collection} in {elapsed:.0f}s "
        f"({total/elapsed:.0f} pts/s)"
    )


def build_passages(rebuild: bool, embed_concurrency: int) -> None:
    print("\n=== Traditional RAG: passage collection ===")
    coll = config.BASELINE_PASSAGE_COLLECTION
    if rebuild:
        print(f"  dropping {coll} ...")
        qdrant_store.drop_collection(coll)
    qdrant_store.ensure_collections()
    existing = qdrant_store.collection_count(coll)
    if existing > 0 and not rebuild:
        print(f"  {coll} already has {existing} points; skipping (use --rebuild to force)")
        return
    print("  chunking corpus into passages ...")
    chunks = chunking.chunk_passages()
    print(f"  {len(chunks)} passages")
    _embed_and_upsert(chunks, coll, embed_concurrency)


def build_propositions(rebuild: bool, embed_concurrency: int) -> None:
    print("\n=== Dense X: proposition collection ===")
    coll = config.BASELINE_PROPOSITION_COLLECTION
    if rebuild:
        print(f"  dropping {coll} ...")
        qdrant_store.drop_collection(coll)
    qdrant_store.ensure_collections()
    existing = qdrant_store.collection_count(coll)
    if existing > 0 and not rebuild:
        # Don't skip — upsert is idempotent (deterministic point ids), so
        # re-running completes a partial build without clobbering existing
        # points. This makes the build safely resumable after a kill.
        print(
            f"  {coll} already has {existing} points; will upsert on top "
            f"(idempotent — safe resume)"
        )
    print("  extracting propositions from corpus ...")
    chunks = chunking.chunk_propositions()
    print(f"  {len(chunks)} propositions")
    if not chunks:
        print("  no propositions extracted; aborting", file=sys.stderr)
        return
    _embed_and_upsert(chunks, coll, embed_concurrency)


def main() -> int:
    ap = argparse.ArgumentParser(description="Build baseline Qdrant collections")
    ap.add_argument(
        "--only",
        choices=["passages", "propositions"],
        default="",
        help="build only one collection (default: both)",
    )
    ap.add_argument(
        "--rebuild",
        action="store_true",
        help="drop + recreate collections before indexing",
    )
    ap.add_argument(
        "--embed-concurrency",
        type=int,
        default=config.BASELINE_INDEX_CONCURRENCY,
        help="concurrent embedding batches (each = BASELINE_EMBED_BATCH_SIZE texts)",
    )
    args = ap.parse_args()

    if args.only == "passages":
        build_passages(args.rebuild, args.embed_concurrency)
    elif args.only == "propositions":
        build_propositions(args.rebuild, args.embed_concurrency)
    else:
        build_passages(args.rebuild, args.embed_concurrency)
        build_propositions(args.rebuild, args.embed_concurrency)
    print("\nDone. Collections ready for run_baseline.py.")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())