"""LLM client for the MultiHop-RAG benchmark.

Per question, per variant:
  - CONCEPT_VARIANT:  extract_concept_queries(question) -> list[str]
                      (1-5 noun phrases for lexical concept-name search)
  - FACTS_VARIANT:    extract_fact_queries(question)   -> list[str]
                      (1-5 keyword-rich tsvector queries for fact search)
  - both variants:    synthesize_answer(question, facts) -> {answer, raw, usage}

Two backends, selected by `config.LLM_BACKEND`:
  - "openrouter" (default): direct calls to OpenRouter's chat completions
    using `OPENROUTER_API_KEY`. The OKT_TOKEN is used only for concept/fact
    retrieval. Keeps the LLM credentials and calls in the experiment logic
    so the OKT API key doesn't need the `ai_provider:execute` scope.
  - "okt": proxy through OKT's `/api/v1/ai/{provider}/chat` endpoint. Token
    usage lands in okt_system.ai_usage automatically, but requires the
    OKT_TOKEN to carry the `ai_provider:execute` permission.

Both backends return the OpenAI-style `{choices, usage}` shape.
"""
from __future__ import annotations

import json
import re
import time
from typing import Any

import httpx

import config
import okt
from prompts import (
    CONCEPT_QUERY_SYSTEM,
    concept_query_user,
    FACT_QUERY_SYSTEM,
    fact_query_user,
    ANSWER_SYSTEM,
    answer_user,
)


# Matches: The answer to the question is "..."
_ANSWER_RE = re.compile(
    r'The answer to the question is\s+"([^"]*)"\s*\.?\s*$',
    re.IGNORECASE,
)
_PHRASE_ARRAY_RE = re.compile(r"\[[^\]]*\]", re.DOTALL)


def _chat_okt(messages: list[dict[str, str]]) -> dict[str, Any]:
    """Proxy through OKT's chat endpoint (requires ai_provider:execute scope)."""
    return okt.chat(
        provider=config.OKT_AI_PROVIDER,
        model=config.OKT_MODEL,
        messages=messages,
    )


def _chat_openrouter(messages: list[dict[str, str]]) -> dict[str, Any]:
    """Call OpenRouter chat completions directly using OPENROUTER_API_KEY.

    Returns the OpenAI-style response shape. Sets HTTP-Referer and
    X-Title headers (OpenRouter ranks / attributes apps by these).
    """
    if not config.OPENROUTER_API_KEY:
        raise RuntimeError(
            "OPENROUTER_API_KEY not set (required when LLM_BACKEND=openrouter)"
        )
    headers = {
        "Authorization": f"Bearer {config.OPENROUTER_API_KEY}",
        "Content-Type": "application/json",
        "HTTP-Referer": config.OPENROUTER_REFERER,
        "X-Title": config.OPENROUTER_TITLE,
    }
    body = {
        "model": config.OPENROUTER_MODEL,
        "messages": messages,
    }
    url = f"{config.OPENROUTER_BASE}/chat/completions"
    with httpx.Client(timeout=config.LLM_TIMEOUT_S) as c:
        r = c.post(url, headers=headers, json=body)
    if r.status_code >= 400:
        raise RuntimeError(f"OpenRouter {r.status_code}: {r.text[:300]}")
    return r.json()


def _chat(messages: list[dict[str, str]]) -> dict[str, Any]:
    """Call the configured LLM backend. Returns the raw response dict."""
    backend = (config.LLM_BACKEND or "openrouter").lower()
    if backend == "okt":
        return _chat_okt(messages)
    return _chat_openrouter(messages)


def _extract_content(resp: dict[str, Any]) -> str:
    """Pull the assistant message content out of an OKT chat response.

    OKT proxies the underlying provider's response. Two shapes are
    observed depending on the provider/wiring:
      - OpenAI-style: {choices: [{message: {content: "..."}}]}
      - OKT proxied: {messages: [{role: "assistant", content: "..."}]}
    We try both, preferring the last assistant message in the proxied
    shape (the conversation's final reply).
    """
    choices = resp.get("choices") or []
    if choices:
        msg = choices[0].get("message") or {}
        content = msg.get("content", "")
        if content:
            return content
    msgs = resp.get("messages") or []
    # Walk in reverse to find the last assistant message.
    for m in reversed(msgs):
        if (m.get("role") or "").lower() == "assistant" and m.get("content"):
            return m["content"]
    # Last resort: the last message of any role.
    if msgs:
        return msgs[-1].get("content", "") or ""
    return ""


def _extract_usage(resp: dict[str, Any]) -> dict[str, int]:
    u = resp.get("usage") or {}
    return {
        "prompt": int(u.get("prompt_tokens", 0)),
        "completion": int(u.get("completion_tokens", 0)),
    }


def _parse_string_array(content: str) -> list[str]:
    """Pull a JSON array of strings out of an LLM response.

    Tries: first `[...]` regex match -> json.loads; falls back to
    parsing the whole content as JSON. Returns [] on any failure.
    """
    if not content:
        return []
    m = _PHRASE_ARRAY_RE.search(content)
    if m:
        try:
            parsed = json.loads(m.group(0))
            if isinstance(parsed, list):
                return [str(x).strip().lower() for x in parsed if str(x).strip()]
        except Exception:  # noqa: BLE001
            pass
        return []
    try:
        parsed = json.loads(content)
        if isinstance(parsed, list):
            return [str(x).strip().lower() for x in parsed if str(x).strip()]
    except Exception:  # noqa: BLE001
        pass
    return []


def _extract_query_list(
    system_prompt: str, user_prompt: str, label: str
) -> tuple[list[str], dict[str, int]]:
    """One LLM call -> (list[str] of queries, token usage).

    Usage is {"prompt": N, "completion": N} from the LLM response, or
    zeros on failure. Returns ([], zeros) on any error.
    """
    zero = {"prompt": 0, "completion": 0}
    messages = [
        {"role": "system", "content": system_prompt},
        {"role": "user", "content": user_prompt},
    ]
    try:
        resp = _chat(messages)
    except Exception as e:  # noqa: BLE001
        print(f"  {label} failed: {e}")
        return [], zero
    content = _extract_content(resp).strip()
    usage = _extract_usage(resp)
    return _parse_string_array(content), usage


def extract_concept_queries(question: str) -> tuple[list[str], dict[str, int]]:
    """1 LLM call -> (1-5 noun phrases for lexical concept-name search, usage)."""
    return _extract_query_list(
        CONCEPT_QUERY_SYSTEM, concept_query_user(question), "concept query extraction"
    )


def extract_fact_queries(question: str) -> tuple[list[str], dict[str, int]]:
    """1 LLM call -> (1-5 keyword-rich websearch_to_tsquery strings, usage)."""
    return _extract_query_list(
        FACT_QUERY_SYSTEM, fact_query_user(question), "fact query extraction"
    )


def _add_usage(a: dict[str, int], b: dict[str, int]) -> dict[str, int]:
    """Sum two token-usage dicts."""
    return {
        "prompt": int(a.get("prompt", 0)) + int(b.get("prompt", 0)),
        "completion": int(a.get("completion", 0)) + int(b.get("completion", 0)),
    }


# Back-compat alias for the old single-variant code path. Old callers
# expected a list[str]; new callers use the tuple form. We expose the
# list-only form here for any legacy code that still imports it.
def extract_phrases(question: str) -> list[str]:
    queries, _ = extract_concept_queries(question)
    return queries


def synthesize_answer(question: str, facts: list[dict]) -> dict[str, Any]:
    """1 LLM call -> {answer, raw, usage}. Always returns a dict.

    `answer` is the extracted short answer (or the raw text fallback).
    """
    messages = [
        {"role": "system", "content": ANSWER_SYSTEM},
        {"role": "user", "content": answer_user(question, facts)},
    ]
    started = time.time()
    try:
        resp = _chat(messages)
    except Exception as e:  # noqa: BLE001
        return {
            "answer": "Insufficient information.",
            "raw": f"[LLM call failed: {e}]",
            "usage": {"prompt": 0, "completion": 0},
            "latency_ms": int((time.time() - started) * 1000),
        }
    raw = _extract_content(resp)
    usage = _extract_usage(resp)
    answer = extract_answer(raw)
    return {
        "answer": answer,
        "raw": raw,
        "usage": usage,
        "latency_ms": int((time.time() - started) * 1000),
    }


def extract_answer(raw: str) -> str:
    """Parse `The answer to the question is "..."` from the response.

    Falls back to the last non-empty line of the response if the pattern
    is missing (matches the official qa_evaluate.py fallback behavior).
    """
    if not raw:
        return "Insufficient information."
    m = _ANSWER_RE.search(raw)
    if m:
        return m.group(1).strip()
    # Fallback: last non-empty stripped line.
    for line in reversed(raw.splitlines()):
        s = line.strip()
        if s:
            return s
    return raw.strip()