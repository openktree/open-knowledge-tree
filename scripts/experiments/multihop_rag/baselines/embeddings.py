"""Embedding client for the MultiHop-RAG baselines.

Calls OpenRouter's POST /v1/embeddings endpoint directly (the same
endpoint OKT's OpenRouterProvider.Embed calls — see
backend/internal/providers/ai/openrouter.go:182), reusing
OPENROUTER_API_KEY + OPENROUTER_BASE from config. This guarantees the
baselines embed with the SAME model OKT uses
(google/gemini-embedding-2, 3072-dim) so the dense-retrieval
comparison isolates chunking + retrieval strategy, not the embedding
model.

Mirrors OKT's embedBatchRecursive: OpenRouter returns an empty `data`
array when a batch exceeds the underlying embedding model's input-count
or total-token limit. On that signal we auto-halve the batch and retry
the halves, so a too-large batch degrades gracefully instead of failing.
"""
from __future__ import annotations

import sys
import time
from typing import Any

import httpx

import config


# OpenRouter returns this exact shape when the batch is too big for the
# underlying model: {"data": [], ...} with HTTP 200. We detect it and
# halve.
_EMPTY_DATA_SENTINEL = "openrouter: embed response has no data"


class EmbedError(RuntimeError):
    def __init__(self, msg: str):
        super().__init__(msg)


def _headers() -> dict[str, str]:
    if not config.OPENROUTER_API_KEY:
        raise EmbedError(
            "OPENROUTER_API_KEY not set (required for baseline embeddings)"
        )
    return {
        "Authorization": f"Bearer {config.OPENROUTER_API_KEY}",
        "Content-Type": "application/json",
        "HTTP-Referer": config.OPENROUTER_REFERER,
        "X-Title": config.OPENROUTER_TITLE,
    }


def _embed_batch(inputs: list[str]) -> tuple[list[list[float]], dict[str, int]]:
    """Send one batch to POST /v1/embeddings. Returns (vectors, usage).

    Raises EmbedError on non-recoverable failures. Returns the empty-data
    sentinel as a normal (vectors=[], usage={}) result so the caller can
    detect it and halve.
    """
    url = f"{config.OPENROUTER_BASE}/embeddings"
    body = {
        "model": config.BASELINE_EMBEDDING_MODEL,
        "input": inputs,
    }
    with httpx.Client(timeout=config.LLM_TIMEOUT_S) as c:
        r = c.post(url, headers=_headers(), json=body)
    if r.status_code >= 400:
        raise EmbedError(
            f"OpenRouter embeddings {r.status_code}: {r.text[:300]}"
        )
    resp = r.json()
    data = resp.get("data") or []
    if not data:
        # Empty data = batch too big for the model. Signal the caller.
        return [], {"prompt_tokens": 0, "total_tokens": 0}
    # Sort by index (OpenRouter may return out of order).
    data_sorted = sorted(data, key=lambda d: d.get("index", 0))
    vectors = [d.get("embedding") or [] for d in data_sorted]
    usage = resp.get("usage") or {}
    return vectors, {
        "prompt_tokens": int(usage.get("prompt_tokens", 0)),
        "total_tokens": int(usage.get("total_tokens", 0)),
    }


def _embed_batch_recursive(
    inputs: list[str],
) -> tuple[list[list[float]], dict[str, int]]:
    """Embed a batch, auto-halving on the empty-data signal.

    Mirrors OKT's embedBatchRecursive (openrouter.go:282). If the batch
    returns no data AND has more than one input, split in half and
    recurse. A single input that returns no data is a hard error (the
    text is too long for the model's context window).
    """
    if not inputs:
        return [], {"prompt_tokens": 0, "total_tokens": 0}
    try:
        vectors, usage = _embed_batch(inputs)
    except EmbedError as e:
        raise EmbedError(f"embed batch failed (len={len(inputs)}): {e}")
    if vectors:
        return vectors, usage
    # Empty data: too big. Halve if possible.
    if len(inputs) == 1:
        raise EmbedError(
            f"{_EMPTY_DATA_SENTINEL} (single input of ~{len(inputs[0])} chars; "
            "reduce BASELINE_PASSAGE_TOKENS or the proposition length)"
        )
    mid = len(inputs) // 2
    left_v, left_u = _embed_batch_recursive(inputs[:mid])
    right_v, right_u = _embed_batch_recursive(inputs[mid:])
    merged = left_v + right_v
    merged_usage = {
        "prompt_tokens": left_u["prompt_tokens"] + right_u["prompt_tokens"],
        "total_tokens": left_u["total_tokens"] + right_u["total_tokens"],
    }
    return merged, merged_usage


def embed_texts(
    texts: list[str], batch_size: int | None = None, retries: int | None = None
) -> tuple[list[list[float]], dict[str, int]]:
    """Embed a list of texts in batches with retries + auto-halve.

    Returns (vectors, usage) where vectors[i] is the embedding for
    texts[i] (order preserved) and usage is the summed token usage.

    batch_size defaults to config.BASELINE_EMBED_BATCH_SIZE (32, matching
    OKT's defaultOpenRouterEmbedBatchSize). retries defaults to
    config.MAX_RETRIES.
    """
    if not texts:
        return [], {"prompt_tokens": 0, "total_tokens": 0}
    bs = batch_size or config.BASELINE_EMBED_BATCH_SIZE
    max_retries = retries if retries is not None else config.MAX_RETRIES

    all_vectors: list[list[float]] = []
    total_usage = {"prompt_tokens": 0, "total_tokens": 0}
    for start in range(0, len(texts), bs):
        batch = texts[start : start + bs]
        last_err: Exception | None = None
        for attempt in range(max_retries + 1):
            try:
                vectors, usage = _embed_batch_recursive(batch)
                all_vectors.extend(vectors)
                total_usage["prompt_tokens"] += usage["prompt_tokens"]
                total_usage["total_tokens"] += usage["total_tokens"]
                break
            except EmbedError as e:
                last_err = e
                if attempt < max_retries:
                    wait = 2.0 * (attempt + 1)
                    print(
                        f"  embed batch [{start}:{start+len(batch)}] failed "
                        f"(attempt {attempt+1}/{max_retries+1}), retrying in "
                        f"{wait:.0f}s: {e}",
                        file=sys.stderr,
                    )
                    time.sleep(wait)
                    continue
                raise EmbedError(
                    f"embed batch [{start}:{start+len(batch)}] exhausted retries: {e}"
                )
        else:
            raise EmbedError(
                f"embed batch [{start}:{start+len(batch)}] failed: {last_err}"
            )
    return all_vectors, total_usage


def embed_one(text: str) -> list[float]:
    """Embed a single text. Returns the vector (list[float])."""
    vectors, _ = embed_texts([text])
    if not vectors:
        raise EmbedError(f"embed_one returned no vector for text of len {len(text)}")
    return vectors[0]