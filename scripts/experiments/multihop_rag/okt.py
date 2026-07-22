"""Thin REST client for the OKT endpoints the benchmark needs.

Repository-scoped routes are mounted at /api/v1/repositories/{slug}/... (the
router resolves slug -> pool). We do NOT use the X-Repository-ID header for
these routes; that header is only used for the system-scope RBAC check on
/repositories/* routes.

All methods raise OKTError on non-2xx responses.
"""
from __future__ import annotations

import time
from typing import Any

import httpx

import config


class OKTError(RuntimeError):
    def __init__(self, status: int, path: str, body: str):
        super().__init__(f"OKT {status} {path}: {body[:300]}")
        self.status = status
        self.path = path
        self.body = body


def _headers() -> dict[str, str]:
    h = {"Accept": "application/json"}
    if config.OKT_TOKEN:
        h["Authorization"] = f"Bearer {config.OKT_TOKEN}"
    return h


def _client() -> httpx.Client:
    return httpx.Client(
        base_url=config.OKT_BASE,
        headers=_headers(),
        timeout=config.HTTP_TIMEOUT_S,
    )


def _get(path: str, params: dict[str, Any] | None = None) -> dict[str, Any]:
    last_err: OKTError | None = None
    for attempt in range(config.MAX_RETRIES + 1):
        try:
            with _client() as c:
                r = c.get(path, params=params)
            if r.status_code == 404:
                # Don't retry 404; it's a stable "not found".
                raise OKTError(404, path, r.text)
            if r.status_code >= 400:
                if attempt < config.MAX_RETRIES:
                    time.sleep(1.0 * (attempt + 1))
                    last_err = OKTError(r.status_code, path, r.text)
                    continue
                raise OKTError(r.status_code, path, r.text)
            return r.json()
        except httpx.HTTPError as e:
            last_err = OKTError(0, path, str(e))
            if attempt < config.MAX_RETRIES:
                time.sleep(1.0 * (attempt + 1))
                continue
            raise last_err
    raise last_err  # type: ignore[misc]


def _post(path: str, body: dict[str, Any]) -> dict[str, Any]:
    last_err: OKTError | None = None
    for attempt in range(config.MAX_RETRIES + 1):
        try:
            with _client() as c:
                r = c.post(path, json=body)
            if r.status_code >= 400:
                if attempt < config.MAX_RETRIES:
                    time.sleep(1.0 * (attempt + 1))
                    last_err = OKTError(r.status_code, path, r.text)
                    continue
                raise OKTError(r.status_code, path, r.text)
            return r.json()
        except httpx.HTTPError as e:
            last_err = OKTError(0, path, str(e))
            if attempt < config.MAX_RETRIES:
                time.sleep(1.0 * (attempt + 1))
                continue
            raise last_err
    raise last_err  # type: ignore[misc]


# --- Concept endpoints -----------------------------------------------------


def search_concepts(query: str, limit: int = 50, offset: int = 0) -> dict[str, Any]:
    """GET /api/v1/repositories/{slug}/concepts?q=<substring>&limit=&offset=

    Lexical substring match on lower(canonical_name). Returns a page
    envelope: {data: [concept groups], total, limit, offset}.
    """
    slug = config.OKT_REPO_SLUG
    return _get(
        f"/api/v1/repositories/{slug}/concepts",
        params={"q": query, "limit": limit, "offset": offset},
    )


def get_concept(concept_id: str) -> dict[str, Any]:
    """GET /api/v1/repositories/{slug}/concepts/{conceptID} — concept group detail."""
    slug = config.OKT_REPO_SLUG
    return _get(f"/api/v1/repositories/{slug}/concepts/{concept_id}")


def get_concept_facts(
    concept_id: str, query: str = "", limit: int = 10, offset: int = 0
) -> dict[str, Any]:
    """GET /api/v1/repositories/{slug}/concepts/{conceptID}/facts?q=&limit=&offset=

    Facts linked to the concept via fact_concepts, optionally filtered
    by a websearch_to_tsquery against facts.search_tsv.
    """
    slug = config.OKT_REPO_SLUG
    return _get(
        f"/api/v1/repositories/{slug}/concepts/{concept_id}/facts",
        params={"q": query, "limit": limit, "offset": offset},
    )


# --- Repo-wide fact search -------------------------------------------------


def search_facts(
    query: str,
    limit: int = 50,
    offset: int = 0,
    sort: str = "",
    status: str = "",
) -> dict[str, Any]:
    """GET /api/v1/repositories/{slug}/facts?q=&limit=&offset=&sort=&status=

    Full-text search over ALL facts in the repository. The `q` filter
    is a websearch_to_tsquery against facts.search_tsv (every token in
    `q` must appear in the fact text for a match). Sort by 'created_at'
    (default, newest first) or 'source_count' (most confirmed first).
    Status defaults to 'stable'; pass 'all' to include new/to_delete.

    Returns a page envelope: {data, total, limit, offset}.
    """
    slug = config.OKT_REPO_SLUG
    params: dict[str, Any] = {"q": query, "limit": limit, "offset": offset}
    if sort:
        params["sort"] = sort
    if status:
        params["status"] = status
    return _get(f"/api/v1/repositories/{slug}/facts", params=params)


# --- Fact endpoint ---------------------------------------------------------


def get_fact(fact_id: str) -> dict[str, Any]:
    """GET /api/v1/repositories/{slug}/facts/{factID}

    Returns {fact, sources, source_count, concepts, concept_count}.
    Sources carry url + parsed_title + first_seen_at; concepts carry
    id + canonical_name + context + description.
    """
    slug = config.OKT_REPO_SLUG
    return _get(f"/api/v1/repositories/{slug}/facts/{fact_id}")


# --- AI chat endpoint ------------------------------------------------------


def chat(provider: str, model: str, messages: list[dict[str, str]]) -> dict[str, Any]:
    """POST /api/v1/ai/{provider}/chat with {model, messages}.

    Returns the AI provider's chat response (shape varies by provider
    but always carries the OpenAI-style `choices` array when proxied
    through OKT). Token usage lands in okt_system.ai_usage automatically.
    """
    return _post(
        f"/api/v1/ai/{provider}/chat",
        body={"model": model, "messages": messages},
    )