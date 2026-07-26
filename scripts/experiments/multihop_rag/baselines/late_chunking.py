"""Late-chunked passage production for the MultiHop-RAG late-chunking baseline.

Operates on the 609 corpus .md files in dataset/corpus/. For each document:
  1. Strip the YAML frontmatter (reuse chunking._parse_frontmatter).
  2. Split the body into whitespace-token windows of
     BASELINE_LATE_CHUNK_WINDOW tokens (default 24, tuned to match the
     average OKT atomic-fact length of ~23 tokens measured in Phase 3).
  3. Emit one segment dict per window with the same payload schema as the
     other baselines so the scorer + Qdrant store are reusable as-is.

The EMBEDDING step (embeddings_long.embed_document_late_chunked) is what
makes this "late chunking" rather than naive chunk-then-embed: it passes
ALL of a document's segments as the `input` array to the Jina API with
`late_chunking: true`, which concatenates them into one document, runs the
long-context transformer forward pass over the full text to produce
contextualized per-token embeddings, then mean-pools each segment's
tokens WITHIN that context — so each chunk embedding carries the
disambiguating context of the surrounding document (arXiv:2409.04701).
This is the structural opposite of chunking-then-embedding-each-chunk-
separately, which is what the traditional + dense_x baselines do.

Chunk windows have NO overlap by design — late chunking preserves context
via the embedding forward pass, not via window overlap. Overlap would
duplicate tokens across chunks and defeat the clean comparison.
"""
from __future__ import annotations

import os
import sys
from typing import Any

import config
from baselines import chunking


def chunk_late_windows() -> list[dict[str, Any]]:
    """Late-chunk window each corpus document into ~fact-sized segments.

    Returns a flat list of segment dicts (chunk_index resets per
    document) with the standard payload schema:
      {doc_id, chunk_index, text, title, source, author, published_at,
       kind="late_chunk"}
    The segments are NOT embedded here — the index builder passes each
    document's segment list to embeddings_long.embed_document_late_chunked
    so the API can run late pooling across the whole document.
    """
    docs = chunking._load_corpus()
    window = config.BASELINE_LATE_CHUNK_WINDOW
    out: list[dict[str, Any]] = []
    for doc in docs:
        tokens = chunking._tokenize(doc["body"])
        if not tokens:
            continue
        chunk_index = 0
        for start in range(0, len(tokens), window):
            seg_tokens = tokens[start : start + window]
            if not seg_tokens:
                break
            out.append(
                {
                    "doc_id": doc["doc_id"],
                    "chunk_index": chunk_index,
                    "text": " ".join(seg_tokens),
                    "title": doc["meta"].get("title", ""),
                    "source": doc["meta"].get("source", ""),
                    "author": doc["meta"].get("author", ""),
                    "published_at": doc["meta"].get("published_at", ""),
                    "kind": "late_chunk",
                }
            )
            chunk_index += 1
    return out


def segments_by_doc(
    chunks: list[dict[str, Any]],
) -> list[tuple[dict[str, Any], list[dict[str, Any]]]]:
    """Group flat chunk list into [(doc_meta, [segments...]), ...] in
    document order, so the index builder can pass one document's full
    segment list to the late-chunking embedder at a time.
    """
    by_doc: dict[str, list[dict[str, Any]]] = {}
    doc_meta: dict[str, dict[str, Any]] = {}
    order: list[str] = []
    for c in chunks:
        did = c["doc_id"]
        if did not in by_doc:
            by_doc[did] = []
            doc_meta[did] = c
            order.append(did)
        by_doc[did].append(c)
    return [(doc_meta[did], by_doc[did]) for did in order]


def avg_segment_tokens(chunks: list[dict[str, Any]]) -> float:
    """Average whitespace-token length of the chunk windows — for the
    granularity-match report (experiment plan success criterion #3).
    """
    if not chunks:
        return 0.0
    total = sum(len(c["text"].split()) for c in chunks)
    return total / len(chunks)