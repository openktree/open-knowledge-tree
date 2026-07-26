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
from baselines import late_chunking, embeddings_long


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
    # Post-build validation: the actual Qdrant point count MUST match the
    # number of chunks we upserted. A mismatch means points are
    # colliding (the point-id key isn't unique per chunk — e.g. the
    # proposition bug where (doc_id, chunk_index) collapsed many
    # propositions to one point). Fail fast instead of letting a full
    # benchmark run against a broken index.
    actual = qdrant_store.collection_count(collection)
    if actual != total:
        raise SystemExit(
            f"  INDEX BUILD FAILED: {collection} has {actual} points but we "
            f"upserted {total} chunks. Point-id collision detected — the "
            f"point-id key is not unique per chunk. Aborting before any "
            f"benchmark run uses this broken index. (expected {total}, "
            f"got {actual})"
        )
    print(f"  validated: {actual} points == {total} chunks (no collision)")


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


def build_late_chunks(rebuild: bool, embed_concurrency: int) -> None:
    """Late-chunked passage collection (arXiv:2409.04701).

    Splits each document into ~fact-sized windows, then passes each
    document's full segment list to the Jina embeddings API with
    late_chunking=true so the API runs the long-context forward pass over
    the whole document and mean-pools each segment's tokens within the
    contextualized representation. Each document is one API call; the
    embed_concurrency controls how many documents are embedded in
    parallel (capped by the Jina RPM limit).
    """
    print("\n=== Late Chunking: late_chunk collection ===")
    coll = config.BASELINE_LATE_CHUNK_COLLECTION
    if rebuild:
        print(f"  dropping {coll} ...")
        qdrant_store.drop_collection(coll)
    qdrant_store.ensure_collections()
    existing = qdrant_store.collection_count(coll)
    if existing > 0 and not rebuild:
        print(
            f"  {coll} already has {existing} points; will upsert on top "
            f"(idempotent — safe resume)"
        )
    print(
        f"  windowing corpus into {config.BASELINE_LATE_CHUNK_WINDOW}-token "
        f"segments ..."
    )
    chunks = late_chunking.chunk_late_windows()
    avg = late_chunking.avg_segment_tokens(chunks)
    print(
        f"  {len(chunks)} late-chunk segments (avg {avg:.1f} tokens/segment)"
    )
    if not chunks:
        print("  no segments; aborting", file=sys.stderr)
        return
    # Group segments by document so each API call late-pools one whole
    # document. Embedding is per-document (not per-segment) — this is the
    # defining difference from the naive chunk-then-embed baselines.
    grouped = late_chunking.segments_by_doc(chunks)
    print(f"  {len(grouped)} documents to late-chunk-embed")

    def _one(doc_segments: tuple[dict, list[dict]]) -> int:
        _meta, segs = doc_segments
        seg_texts = [s["text"] for s in segs]
        vectors, _ = embeddings_long.embed_document_late_chunked(seg_texts)
        if len(vectors) != len(segs):
            raise RuntimeError(
                f"late-chunk embed count mismatch: {len(vectors)} != "
                f"{len(segs)} for doc {segs[0]['doc_id']} (segments may "
                f"exceed the model's 8192-token context window)"
            )
        points = []
        for s, vec in zip(segs, vectors):
            p = dict(s)
            p["vector"] = vec
            points.append(p)
        qdrant_store.upsert_points(coll, points)
        return len(points)

    total = 0
    started = time.time()
    # The self-hosted model is a module-level singleton resident on the
    # GPU; concurrent forward passes would contend for VRAM and the
    # python GIL releases only inside the CUDA kernel launch, not enough
    # to speed up sequential docs. Force serial (embed_concurrency=1) —
    # the per-doc forward pass is the bottleneck and one-at-a-time keeps
    # memory bounded.
    if embed_concurrency > 1:
        with ThreadPoolExecutor(max_workers=embed_concurrency) as ex:
            futures = {ex.submit(_one, g): g for g in grouped}
            for fut in tqdm(
                as_completed(futures), total=len(futures), desc="late-chunk"
            ):
                total += fut.result()
    else:
        for g in tqdm(grouped, desc="late-chunk"):
            total += _one(g)
    elapsed = time.time() - started
    print(
        f"  indexed {total} points into {coll} in {elapsed:.0f}s "
        f"({total/elapsed:.0f} pts/s)"
    )
    # Post-build validation (same collision check as the other baselines).
    actual = qdrant_store.collection_count(coll)
    if actual != total:
        raise SystemExit(
            f"  INDEX BUILD FAILED: {coll} has {actual} points but we "
            f"upserted {total} chunks. Point-id collision detected — "
            f"aborting. (expected {total}, got {actual})"
        )
    print(f"  validated: {actual} points == {total} chunks (no collision)")


def main() -> int:
    ap = argparse.ArgumentParser(description="Build baseline Qdrant collections")
    ap.add_argument(
        "--only",
        choices=["passages", "propositions", "late_chunks"],
        default="",
        help="build only one collection (default: all)",
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
    elif args.only == "late_chunks":
        build_late_chunks(args.rebuild, args.embed_concurrency)
    else:
        build_passages(args.rebuild, args.embed_concurrency)
        build_propositions(args.rebuild, args.embed_concurrency)
        build_late_chunks(args.rebuild, args.embed_concurrency)
    print("\nDone. Collections ready for run_baseline.py.")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())