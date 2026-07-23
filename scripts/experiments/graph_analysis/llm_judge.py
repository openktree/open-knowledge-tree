"""LLM-as-judge client for the graph-analysis experiments.

Thin OpenRouter client with retry/backoff, JSON extraction, and a
content-hash cache. Mirrors the patterns in multihop_rag/llm.py but is
self-contained (no imports from other experiment dirs).

Uses the same model OKT uses for concept extraction
(google/gemma-4-31b-it) so the judge's judgments are calibrated to the
same model family that produced the concepts being audited.
"""
from __future__ import annotations

import hashlib
import json
import os
import time
from pathlib import Path

import httpx

OPENROUTER_API_KEY = os.environ.get("OPENROUTER_API_KEY", "")
OPENROUTER_BASE = os.environ.get("OPENROUTER_BASE", "https://openrouter.ai/api/v1").rstrip("/")
OPENROUTER_MODEL = os.environ.get("OPENROUTER_MODEL", "google/gemma-4-31b-it")

CACHE_DIR = Path(__file__).resolve().parent / "results" / "llm_cache"
MAX_RETRIES = 3
TIMEOUT_S = 60


def _cache_key(system: str, prompt: str) -> Path:
    h = hashlib.sha256((system + "\n" + prompt).encode()).hexdigest()[:16]
    return CACHE_DIR / f"{h}.json"


def _cached(key: Path) -> str | None:
    if key.exists():
        return key.read_text()
    return None


def _store(key: Path, text: str) -> None:
    CACHE_DIR.mkdir(parents=True, exist_ok=True)
    key.write_text(text)


def extract_json(text: str) -> dict | list | None:
    """Robust JSON extraction: strips markdown fences, finds first {...} or [...]."""
    s = text.strip()
    if s.startswith("```"):
        lines = s.split("\n")
        lines = [l for l in lines if not l.strip().startswith("```")]
        s = "\n".join(lines).strip()
    try:
        return json.loads(s)
    except json.JSONDecodeError:
        pass
    # Find first { or [ and matching close.
    for start_char, end_char in [("{", "}"), ("[", "]")]:
        start = s.find(start_char)
        if start == -1:
            continue
        depth = 0
        for i in range(start, len(s)):
            if s[i] == start_char:
                depth += 1
            elif s[i] == end_char:
                depth -= 1
                if depth == 0:
                    try:
                        return json.loads(s[start : i + 1])
                    except json.JSONDecodeError:
                        break
    return None


def judge(
    prompt: str,
    system: str = "You are a helpful assistant. Respond in JSON only.",
    *,
    model: str | None = None,
    use_cache: bool = True,
) -> str:
    """Call the LLM with retry/backoff and content-hash caching. Returns raw text."""
    model = model or OPENROUTER_MODEL
    if use_cache:
        key = _cache_key(system, prompt)
        cached = _cached(key)
        if cached is not None:
            return cached

    if not OPENROUTER_API_KEY:
        raise RuntimeError("OPENROUTER_API_KEY not set")

    headers = {
        "Authorization": f"Bearer {OPENROUTER_API_KEY}",
        "Content-Type": "application/json",
    }
    body = {
        "model": model,
        "messages": [
            {"role": "system", "content": system},
            {"role": "user", "content": prompt},
        ],
        "temperature": 0.0,
    }

    last_err = None
    for attempt in range(MAX_RETRIES + 1):
        try:
            with httpx.Client(timeout=TIMEOUT_S) as client:
                r = client.post(f"{OPENROUTER_BASE}/chat/completions", headers=headers, json=body)
            if r.status_code == 429:
                time.sleep(2 ** attempt)
                continue
            r.raise_for_status()
            data = r.json()
            text = data["choices"][0]["message"]["content"]
            if use_cache:
                _store(key, text)
            return text
        except Exception as e:
            last_err = e
            time.sleep(2 ** attempt)

    raise RuntimeError(f"LLM call failed after {MAX_RETRIES} retries: {last_err}")


def judge_json(prompt: str, system: str = "You are a helpful assistant. Respond in JSON only.", **kw) -> dict | list | None:
    """Call judge() and parse the response as JSON."""
    text = judge(prompt, system, **kw)
    return extract_json(text)