"""Long-context embedding client for the late-chunking baseline.

Self-hosts jina-embeddings-v3 (the model the late-chunking paper
arXiv:2409.04701 itself uses) via transformers. No API key required —
the model is public (CC-BY-NC, fine for a research benchmark) and
downloaded once via HuggingFace. You have torch+CUDA+transformers
already installed.

Late chunking implementation (arXiv:2409.04701):
  1. Tokenize the FULL document (all segments concatenated) with the
     model's tokenizer — one forward pass over the whole text so every
     token sees the full surrounding context.
  2. Run the transformer forward pass → last_hidden_state (per-token
     embeddings, contextualized by the whole document).
  3. For each segment, mean-pool the token embeddings whose positions
     fall within that segment's character span, using the tokenizer's
     offset mapping to map segment boundaries back to token positions.
  4. L2-normalize each segment embedding.

This is the structural opposite of chunk-then-embed-each-separately
(which the traditional + dense_x baselines do via OpenRouter): the
context is preserved in the forward pass, not in the chunk text.

The query side uses the SAME model with mean pooling over the query
tokens (no late chunking — it's a single short text), so Condition
B's dense retrieval is fair within the late-chunking condition.

The model is loaded once at first use and kept resident on the GPU for
the process lifetime (loading ~2GB takes ~30s; reloading per-call would
dominate runtime). For 609 docs + 2556 queries on an RTX 4080 SUPER the
whole index build + benchmark takes minutes, not hours.
"""
from __future__ import annotations

import sys
import time
from typing import Any

import numpy as np

import config


class LongEmbedError(RuntimeError):
    pass


# Module-level lazy singletons so the model loads once and stays resident.
_MODEL = None
_TOKENIZER = None
_DEVICE = None


def _ensure_loaded() -> None:
    """Lazy-load the model + tokenizer once, keep resident."""
    global _MODEL, _TOKENIZER, _DEVICE
    if _MODEL is not None:
        return
    import torch
    from transformers import AutoModel, AutoTokenizer

    model_name = config.BASELINE_LATE_CHUNK_MODEL
    print(f"  [late-chunk] loading {model_name} (first run downloads ~2GB) ...", file=sys.stderr)
    t0 = time.time()
    _TOKENIZER = AutoTokenizer.from_pretrained(model_name, trust_remote_code=True)
    use_cuda = torch.cuda.is_available()
    dtype = torch.float16 if use_cuda else torch.float32
    _MODEL = AutoModel.from_pretrained(
        model_name, trust_remote_code=True, dtype=dtype
    ).eval()
    _DEVICE = torch.device("cuda" if use_cuda else "cpu")
    _MODEL = _MODEL.to(_DEVICE)
    print(
        f"  [late-chunk] loaded in {time.time()-t0:.0f}s on {_DEVICE} "
        f"(hidden={_MODEL.config.hidden_size}, "
        f"max_pos={_MODEL.config.max_position_embeddings})",
        file=sys.stderr,
    )


def _mean_pool_normalize(token_vecs: np.ndarray, mask: np.ndarray) -> np.ndarray:
    """Mean-pool token_vecs (T, D) where mask (T,) is 1 for kept tokens,
    then L2-normalize. Returns (D,)."""
    keep = mask.astype(np.float32)
    s = keep.sum()
    if s <= 0:
        # Empty segment — return zero vector (will be filtered upstream).
        return np.zeros(token_vecs.shape[-1], dtype=np.float32)
    pooled = (token_vecs * keep[:, None]).sum(axis=0) / s
    norm = np.linalg.norm(pooled)
    if norm > 0:
        pooled = pooled / norm
    return pooled.astype(np.float32)


def embed_document_late_chunked(
    segments: list[str],
    retries: int | None = None,
) -> tuple[list[list[float]], dict[str, int]]:
    """Late-chunk embed one document's segments.

    Tokenizes the FULL concatenated document, runs one transformer
    forward pass, then mean-pools each segment's tokens within the
    contextualized representation (using offset mapping to find each
    segment's token span).

    Returns (vectors, usage) where vectors[i] is the late-chunked
    embedding for segments[i] (L2-normalized, dim=1024).
    """
    if not segments:
        return [], {"prompt_tokens": 0, "total_tokens": 0}
    import torch

    _ensure_loaded()
    # Concatenate segments with the tokenizer's separator (single space
    # for jina's tokenizer; offset mapping tracks each segment's span).
    # Re-join with "\n\n" so segment boundaries are recoverable as
    # character offsets; we then find each segment's char span in the
    # joined string and map char spans → token positions via offsets.
    sep = "\n\n"
    # Build the joined document and record each segment's char span.
    spans: list[tuple[int, int]] = []
    pos = 0
    for seg in segments:
        start = pos
        spans.append((start, start + len(seg)))
        pos = start + len(seg) + len(sep)
    # Re-construct joined with separators between segments.
    joined = sep.join(segments)

    # Tokenize the whole document with offset mapping (character spans
    # per token). truncation=True caps at max_pos (8192) — docs in this
    # corpus are max ~12k whitespace tokens but the tokenizer is BPE so
    # token count is closer; if a doc truncates, the tail segments get
    # zero-vectors and are flagged by the caller's collision check.
    max_len = min(config.BASELINE_LATE_CHUNK_MAX_TOKENS, _MODEL.config.max_position_embeddings)
    enc = _TOKENIZER(
        joined,
        return_tensors="pt",
        return_offsets_mapping=True,
        truncation=True,
        max_length=max_len,
    )
    offsets = enc.pop("offset_mapping")[0].numpy()  # (T, 2) char spans
    input_ids = enc["input_ids"].to(_DEVICE)
    attention_mask = enc["attention_mask"].to(_DEVICE)

    with torch.no_grad():
        out = _MODEL(input_ids=input_ids, attention_mask=attention_mask)
    # last_hidden_state: (1, T, D) on GPU/FP16 → (T, D) numpy float32.
    hidden = out.last_hidden_state[0].to(torch.float32).cpu().numpy()
    attn = attention_mask[0].to(torch.float32).cpu().numpy()

    # For each segment, find tokens whose char span overlaps the
    # segment's char span, mean-pool them.
    vectors: list[list[float]] = []
    for (seg_start, seg_end) in spans:
        # A token belongs to this segment if its char span overlaps.
        # offset is (start_char, end_char). overlap iff
        # token_start < seg_end AND token_end > seg_start.
        token_starts = offsets[:, 0]
        token_ends = offsets[:, 1]
        in_seg = (token_starts < seg_end) & (token_ends > seg_start) & (attn > 0)
        if not in_seg.any():
            # Tokenizer truncated before this segment — emit zero vector.
            vectors.append([0.0] * hidden.shape[-1])
            continue
        vec = _mean_pool_normalize(hidden, in_seg.astype(np.float32))
        vectors.append(vec.tolist())

    prompt_tokens = int(input_ids.shape[1])
    return vectors, {"prompt_tokens": prompt_tokens, "total_tokens": prompt_tokens}


def embed_query(text: str, retries: int | None = None) -> list[float]:
    """Embed a query with the SAME long-context model.

    No late chunking on the query — it's a single short text. Mean-pool
    all its tokens (with attention mask) and L2-normalize. Using the
    same model as the index keeps Condition B's dense retrieval fair
    within the late-chunking condition.
    """
    if not text:
        raise LongEmbedError("embed_query: empty query text")
    import torch

    _ensure_loaded()
    max_len = min(config.BASELINE_LATE_CHUNK_MAX_TOKENS, _MODEL.config.max_position_embeddings)
    enc = _TOKENIZER(
        text,
        return_tensors="pt",
        truncation=True,
        max_length=max_len,
    )
    input_ids = enc["input_ids"].to(_DEVICE)
    attention_mask = enc["attention_mask"].to(_DEVICE)
    with torch.no_grad():
        out = _MODEL(input_ids=input_ids, attention_mask=attention_mask)
    hidden = out.last_hidden_state[0].to(torch.float32).cpu().numpy()
    attn = attention_mask[0].to(torch.float32).cpu().numpy()
    vec = _mean_pool_normalize(hidden, attn)
    return vec.tolist()