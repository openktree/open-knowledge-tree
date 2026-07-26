"""Thin REST client for the OKT endpoints the KGQA experiment needs.

Wraps the existing multihop_rag/okt.py patterns but is self-contained in the
exp4_kgqa/ folder. Adds get_related_concepts (the REST endpoint at
GET /api/v1/repositories/{slug}/concepts/{conceptID}/relations) which the
existing okt.py doesn't expose.
"""
from __future__ import annotations

import os
import time
from typing import Any

import httpx

# Load .env from the multihop_rag experiment dir (handles the token->OKT_TOKEN alias).
_ENV_PATH = os.path.join(os.path.dirname(__file__), "..", "..", "multihop_rag", ".env")
if os.path.exists(_ENV_PATH):
    _ALIASES = {"token": "OKT_TOKEN", "user": "OKT_USER"}
    with open(_ENV_PATH, "r", encoding="utf-8") as fh:
        for line in fh:
            s = line.strip()
            if not s or s.startswith("#") or "=" not in s:
                continue
            key, _, val = s.partition("=")
            key = key.strip()
            val = val.strip().strip('"').strip("'")
            canonical = _ALIASES.get(key, key)
            if canonical and canonical not in os.environ:
                os.environ[canonical] = val

OKT_BASE = os.environ.get("OKT_BASE", "http://localhost:8080").rstrip("/")
OKT_TOKEN = os.environ.get("OKT_TOKEN", "")
OKT_REPO_SLUG = os.environ.get("OKT_REPO_SLUG", "multihoprag")
OPENROUTER_API_KEY = os.environ.get("OPENROUTER_API_KEY", "")
OPENROUTER_BASE = os.environ.get("OPENROUTER_BASE", "https://openrouter.ai/api/v1").rstrip("/")
OPENROUTER_MODEL = os.environ.get("OPENROUTER_MODEL", "google/gemma-4-31b-it")
HTTP_TIMEOUT_S = 60
MAX_RETRIES = 3


class OKTError(RuntimeError):
    def __init__(self, status: int, path: str, body: str):
        super().__init__(f"OKT {status} {path}: {body[:300]}")
        self.status = status
        self.path = path
        self.body = body


def _headers() -> dict[str, str]:
    h = {"Accept": "application/json"}
    if OKT_TOKEN:
        h["Authorization"] = f"Bearer {OKT_TOKEN}"
    return h


def _get(path: str, params: dict[str, Any] | None = None) -> dict[str, Any]:
    last_err = None
    for attempt in range(MAX_RETRIES + 1):
        try:
            with httpx.Client(timeout=HTTP_TIMEOUT_S) as c:
                r = c.get(f"{OKT_BASE}{path}", headers=_headers(), params=params)
            if r.status_code == 404:
                return {"data": [], "total": 0}
            r.raise_for_status()
            return r.json()
        except httpx.HTTPStatusError as e:
            last_err = OKTError(e.response.status_code, path, e.response.text)
            if e.response.status_code == 404:
                return {"data": [], "total": 0}
            time.sleep(2 ** attempt)
        except Exception as e:
            last_err = e
            time.sleep(2 ** attempt)
    raise last_err if last_err else RuntimeError("unknown error")


def _concept_id(group: dict) -> str:
    """Extract a concept_id from a concept group (nested in contexts[0])."""
    cid = group.get("id", "")
    if cid:
        return cid
    contexts = group.get("contexts", [])
    if contexts:
        return contexts[0].get("concept_id", "")
    return ""


def search_concepts(query: str, limit: int = 50) -> list[dict[str, Any]]:
    r = _get(f"/api/v1/repositories/{OKT_REPO_SLUG}/concepts",
             params={"q": query, "limit": limit})
    return r.get("data", []) or []


def get_concept_facts(concept_id_or_name: str, query: str = "", limit: int = 10) -> list[dict[str, Any]]:
    params: dict[str, Any] = {"limit": limit}
    if query:
        params["q"] = query
    r = _get(f"/api/v1/repositories/{OKT_REPO_SLUG}/concepts/{concept_id_or_name}/facts", params=params)
    return r.get("data", []) or []


def get_related_concepts(concept_id_or_name: str, limit: int = 50) -> list[dict[str, Any]]:
    """GET /api/v1/repositories/{slug}/concepts/{conceptID}/relations

    The endpoint accepts a concept UUID or canonical name.
    Returns related concepts ranked by shared_fact_count.
    """
    r = _get(f"/api/v1/repositories/{OKT_REPO_SLUG}/concepts/{concept_id_or_name}/relations",
             params={"limit": limit})
    return r.get("data", r.get("related", [])) or []


def search_facts(query: str, limit: int = 50, sort: str = "source_count") -> list[dict[str, Any]]:
    params: dict[str, Any] = {"q": query, "limit": limit}
    if sort:
        params["sort"] = sort
    r = _get(f"/api/v1/repositories/{OKT_REPO_SLUG}/facts", params=params)
    return r.get("data", []) or []


def get_fact(fact_id: str) -> dict[str, Any]:
    return _get(f"/api/v1/repositories/{OKT_REPO_SLUG}/facts/{fact_id}")


def get_concept_definition(concept_id: str) -> dict[str, Any] | None:
    """GET /api/v1/repositories/{slug}/concepts/{conceptID}/definition

    Returns the LLM-generated synthesis/definition for a concept group.
    This is the compressed summary, NOT the raw facts.
    """
    try:
        r = _get(f"/api/v1/repositories/{OKT_REPO_SLUG}/concepts/{concept_id}/definition")
        return r
    except OKTError:
        return None


def llm_chat(messages: list[dict[str, str]], model: str | None = None) -> str:
    """Call OpenRouter directly for answer synthesis / triplet extraction."""
    model = model or OPENROUTER_MODEL
    headers = {
        "Authorization": f"Bearer {OPENROUTER_API_KEY}",
        "Content-Type": "application/json",
    }
    body = {"model": model, "messages": messages, "temperature": 0.0}
    last_err = None
    for attempt in range(MAX_RETRIES + 1):
        try:
            with httpx.Client(timeout=120) as c:
                r = c.post(f"{OPENROUTER_BASE}/chat/completions", headers=headers, json=body)
            r.raise_for_status()
            return r.json()["choices"][0]["message"]["content"]
        except Exception as e:
            last_err = e
            time.sleep(2 ** attempt)
    raise RuntimeError(f"OpenRouter call failed: {last_err}")