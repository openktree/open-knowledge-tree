"""Chunking for the MultiHop-RAG baselines.

Two chunking strategies, both operating on the 609 corpus .md files in
dataset/corpus/ (each is a markdown file with YAML frontmatter: title,
source, author, category, published_at):

1. **Passage chunker** (Traditional RAG): fixed-size passage chunks of
   ~BASELINE_PASSAGE_TOKENS whitespace tokens with
   BASELINE_PASSAGE_OVERLAP token overlap. The canonical RAG default
   (512 tokens, 50 overlap). No semantic awareness — pure sliding
   window. The frontmatter is stripped from the chunk text (it's
   metadata, not prose) but stored in the point payload so the synthesis
   LLM still sees publication attribution.

2. **Proposition extractor** (Dense X Retrieval): produces "atomic
   expressions within text that each encapsulate a distinct factoid and
   are presented in a concise, self-contained natural language format"
   (Dense X paper, arXiv:2312.06648). Two backends:
     - "llm" (default, portable): use the configured synthesis LLM
       (gemma) with a proposition-extraction prompt to split each passage
       into self-contained propositions. Faithful to the Dense X
       definition; the model differs from the paper's released
       propositionizer but the unit is the same.
     - "model": use the released propositionizer
       (chentong00/propositionizer-windows) via transformers. Slower on
       CPU but faithful to the paper. Requires torch + transformers
       (imported lazily so the default path needs neither).

Both strategies return a list of {doc_id, chunk_index, text, title,
source, author, published_at, kind} dicts ready for embedding + Qdrant
upsert.
"""
from __future__ import annotations

import os
import re
import sys
import json
from typing import Any

import config

# The corpus .md files start with a YAML frontmatter block delimited by
# "---" lines. We parse it into a dict so the chunker can attach
# metadata to each chunk's payload (the synthesis prompt needs
# publication name + date for multi-hop attribution questions).
_FRONTMATTER_RE = re.compile(r"^---\s*\n(.*?)\n---\s*\n?(.*)$", re.DOTALL)


def _parse_frontmatter(text: str) -> tuple[dict[str, str], str]:
    """Split a corpus .md into (metadata dict, body text).

    The frontmatter is YAML-ish (key: value lines). We do a minimal
    parse: split on the first ":" and strip quotes. Robust enough for
    the MultiHop-RAG corpus (the downloader wrote it, so the format is
    known).
    """
    m = _FRONTMATTER_RE.match(text)
    if not m:
        return {}, text
    meta: dict[str, str] = {}
    for line in m.group(1).splitlines():
        if ":" not in line:
            continue
        key, _, val = line.partition(":")
        key = key.strip()
        val = val.strip().strip('"').strip("'")
        if key:
            meta[key] = val
    body = m.group(2).strip()
    return meta, body


def _load_corpus() -> list[dict[str, Any]]:
    """Load all corpus .md files. Returns a list of {doc_id, body, meta}.

    doc_id is the filename stem (the slug the downloader wrote). Sorted
    by filename for deterministic ordering.
    """
    if not os.path.isdir(config.CORPUS_DIR):
        raise RuntimeError(
            f"corpus dir not found: {config.CORPUS_DIR}. "
            "Run `python3 download_dataset.py` first."
        )
    out: list[dict[str, Any]] = []
    for fname in sorted(os.listdir(config.CORPUS_DIR)):
        if not fname.endswith(".md"):
            continue
        path = os.path.join(config.CORPUS_DIR, fname)
        with open(path, "r", encoding="utf-8") as fh:
            raw = fh.read()
        meta, body = _parse_frontmatter(raw)
        doc_id = os.path.splitext(fname)[0]
        out.append({"doc_id": doc_id, "body": body, "meta": meta})
    return out


def _tokenize(text: str) -> list[str]:
    """Whitespace tokenization (news articles are English; a simple
    split is sufficient and avoids a tokenizer dependency)."""
    return text.split()


def chunk_passages() -> list[dict[str, Any]]:
    """Fixed-size passage chunking for Traditional RAG.

    Splits each document's body into overlapping windows of
    BASELINE_PASSAGE_TOKENS whitespace tokens with
    BASELINE_PASSAGE_OVERLAP token overlap. Returns a flat list of
    chunk dicts (chunk_index resets per document).
    """
    docs = _load_corpus()
    size = config.BASELINE_PASSAGE_TOKENS
    overlap = config.BASELINE_PASSAGE_OVERLAP
    step = max(1, size - overlap)
    out: list[dict[str, Any]] = []
    for doc in docs:
        tokens = _tokenize(doc["body"])
        if not tokens:
            continue
        chunk_index = 0
        for start in range(0, len(tokens), step):
            chunk_tokens = tokens[start : start + size]
            if not chunk_tokens:
                break
            text = " ".join(chunk_tokens)
            out.append(
                {
                    "doc_id": doc["doc_id"],
                    "chunk_index": chunk_index,
                    "text": text,
                    "title": doc["meta"].get("title", ""),
                    "source": doc["meta"].get("source", ""),
                    "author": doc["meta"].get("author", ""),
                    "published_at": doc["meta"].get("published_at", ""),
                    "kind": "passage",
                }
            )
            chunk_index += 1
            if start + size >= len(tokens):
                break
    return out


# ---------------------------------------------------------------------------
# Proposition extraction (Dense X)
# ---------------------------------------------------------------------------

_PROPOSITION_SYSTEM = (
    "You extract propositions from a text passage.\n"
    "A proposition is an atomic, self-contained expression that "
    "encapsulates a single factoid, presented in a concise natural "
    "language sentence. Each proposition must be:\n"
    "1. Self-contained: understandable without the surrounding context "
    "(resolve pronouns to their referents; add the subject if it's "
    "implied).\n"
    "2. Atomic: one fact per proposition. If a sentence states two facts, "
    "split into two propositions.\n"
    "3. Concise: a single short sentence, no filler.\n"
    "4. Faithful: do not add information not present in the source text.\n"
    "Output ONLY a JSON array of proposition strings. No prose, no "
    "markdown fences. If the passage contains no factual claims, "
    "return []."
)


def _proposition_user(passage: str, meta: dict[str, str]) -> str:
    bits = []
    if meta.get("source"):
        bits.append(meta["source"])
    if meta.get("title"):
        bits.append(f'"{meta["title"]}"')
    if meta.get("author"):
        bits.append(f"by {meta['author']}")
    if meta.get("published_at"):
        bits.append(f"on {meta['published_at']}")
    header = f"[Source: {' '.join(bits)}]" if bits else ""
    return f"{header}\n\nPassage:\n{passage}\n\nPropositions:"


def _extract_propositions_llm(
    passages: list[dict[str, Any]],
    concurrency: int,
) -> list[dict[str, Any]]:
    """LLM-based proposition extraction (default, portable backend).

    Uses the configured synthesis LLM (gemma via OpenRouter) with a
    proposition-extraction prompt. Reuses the experiment's llm._chat so
    retries/backoff/usage tracking match the rest of the harness.
    """
    import json
    import llm
    from concurrent.futures import ThreadPoolExecutor, as_completed

    out: list[dict[str, Any]] = []
    total_usage = {"prompt": 0, "completion": 0}

    def _one(p: dict[str, Any]) -> list[dict[str, Any]]:
        messages = [
            {"role": "system", "content": _PROPOSITION_SYSTEM},
            {"role": "user", "content": _proposition_user(p["text"], {
                "source": p.get("source", ""),
                "title": p.get("title", ""),
                "author": p.get("author", ""),
                "published_at": p.get("published_at", ""),
            })},
        ]
        try:
            resp = llm._chat(messages)
        except Exception as e:  # noqa: BLE001
            print(f"  proposition extraction failed for {p['doc_id']}#{p['chunk_index']}: {e}", file=sys.stderr)
            return []
        content = llm._extract_content(resp).strip()
        # Reuse the phrase-array parser (handles JSON arrays + fenced).
        props = llm._parse_string_array(content)
        return [
            {
                "doc_id": p["doc_id"],
                "chunk_index": p["chunk_index"],
                "text": prop,
                "title": p.get("title", ""),
                "source": p.get("source", ""),
                "author": p.get("author", ""),
                "published_at": p.get("published_at", ""),
                "kind": "proposition",
            }
            for prop in props
            if prop.strip()
        ]

    if concurrency > 1:
        with ThreadPoolExecutor(max_workers=concurrency) as ex:
            futures = {ex.submit(_one, p): p for p in passages}
            for fut in as_completed(futures):
                out.extend(fut.result())
    else:
        for p in passages:
            out.extend(_one(p))
    return out


def _extract_propositions_model(
    passages: list[dict[str, Any]],
) -> list[dict[str, Any]]:
    """Faithful Dense X backend: the released propositionizer model.

    Uses chentong00/propositionizer-windows via transformers. Slow on
    CPU (download + inference) but faithful to the paper. Imported
    lazily so the default path (BASELINE_PROPOSITIONIZER=llm) needs no
    torch/transformers dependency.
    """
    try:
        import torch
        from transformers import AutoModelForSeq2SeqLM, AutoTokenizer
    except ImportError as e:
        raise RuntimeError(
            "BASELINE_PROPOSITIONIZER=model requires torch + transformers. "
            f"Install them: pip install torch transformers. ({e})"
        )
    model_name = "chentong00/propositionizer-windows"
    print(f"  loading propositionizer model: {model_name} ...", file=sys.stderr)
    tokenizer = AutoTokenizer.from_pretrained(model_name)
    model = AutoModelForSeq2SeqLM.from_pretrained(model_name)
    device = "cuda" if torch.cuda.is_available() else "cpu"
    model = model.to(device)
    model.eval()

    out: list[dict[str, Any]] = []
    for i, p in enumerate(passages):
        text = p["text"]
        # The propositionizer takes raw text; it handles its own
        # segmentation. Tokenize + generate.
        inputs = tokenizer(
            text,
            return_tensors="pt",
            truncation=True,
            max_length=512,
        ).to(device)
        with torch.no_grad():
            outputs = model.generate(
                **inputs,
                max_new_tokens=256,
                num_beams=4,
            )
        decoded = tokenizer.decode(outputs[0], skip_special_tokens=True)
        # The model outputs one proposition per line (the paper's format).
        props = [line.strip() for line in decoded.splitlines() if line.strip()]
        for prop in props:
            out.append(
                {
                    "doc_id": p["doc_id"],
                    "chunk_index": p["chunk_index"],
                    "text": prop,
                    "title": p.get("title", ""),
                    "source": p.get("source", ""),
                    "author": p.get("author", ""),
                    "published_at": p.get("published_at", ""),
                    "kind": "proposition",
                }
            )
        if (i + 1) % 50 == 0:
            print(f"  propositionized {i+1}/{len(passages)} passages", file=sys.stderr)
    return out


def _propositions_cache_path(backend: str) -> str:
    """Disk cache for extracted propositions, keyed by backend.

    Cache file: dataset/propositions_<backend>.jsonl — one JSON object
    per proposition (same shape as the chunk dicts). Lets a partial or
    repeated index build skip the LLM extraction step entirely (the
    expensive part); embedding + upsert still re-run but those are
    idempotent (deterministic point ids) and cheap relative to the LLM
    calls.

    Invalidate by deleting the file or changing BASELINE_PROPOSITIONIZER
    (the backend name is in the filename so switching llm<->model
    creates a separate cache).
    """
    return os.path.join(config.DATASET_DIR, f"propositions_{backend}.jsonl")


def _load_cached_propositions(path: str) -> list[dict[str, Any]] | None:
    if not os.path.exists(path):
        return None
    out: list[dict[str, Any]] = []
    try:
        with open(path, "r", encoding="utf-8") as fh:
            for line in fh:
                line = line.strip()
                if line:
                    out.append(json.loads(line))
    except Exception:  # noqa: BLE001
        return None
    return out if out else None


def _save_cached_propositions(path: str, props: list[dict[str, Any]]) -> None:
    os.makedirs(os.path.dirname(path), exist_ok=True)
    with open(path, "w", encoding="utf-8") as fh:
        for p in props:
            fh.write(json.dumps(p, ensure_ascii=False) + "\n")


def chunk_propositions(concurrency: int | None = None) -> list[dict[str, Any]]:
    """Proposition extraction for Dense X Retrieval.

    Two-step: (1) passage-chunk the corpus (so the propositionizer sees
    reasonably-sized inputs), (2) extract propositions from each
    passage via the configured backend (llm or model).

    The extracted propositions are cached to
    dataset/propositions_<backend>.jsonl so a re-run (e.g. after a
    partial index build was killed) skips the expensive LLM extraction
    and goes straight to embedding + upsert. Delete the cache file to
    force re-extraction.
    """
    backend = (config.BASELINE_PROPOSITIONIZER or "llm").lower()
    cache_path = _propositions_cache_path(backend)
    cached = _load_cached_propositions(cache_path)
    if cached is not None:
        print(f"  loaded {len(cached)} cached propositions from {cache_path}")
        return cached

    # Step 1: passage-chunk. We reuse the passage chunker to produce
    # reasonably-sized inputs (the propositionizer / LLM has context
    # limits; feeding full articles would truncate).
    passages = chunk_passages()
    print(f"  passage-chunked {len(passages)} passages for propositionization")

    # Step 2: extract propositions.
    if backend == "model":
        props = _extract_propositions_model(passages)
    else:
        conc = concurrency if concurrency is not None else config.BASELINE_INDEX_CONCURRENCY
        props = _extract_propositions_llm(passages, conc)
    print(f"  extracted {len(props)} propositions")
    # Cache so future index builds skip the LLM step.
    _save_cached_propositions(cache_path, props)
    print(f"  cached to {cache_path}")
    return props