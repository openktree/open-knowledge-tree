"""Wikidata Q-ID lookup — ground truth for concept-auditing experiments.

Given a surface name (and optionally a context hint), queries the Wikidata
REST API for candidate entities and returns their Q-IDs, labels, and
descriptions. Used by Experiment 3 (Failure 1: under-merging) to determine
whether two OKT concept groups that should be one entity are fragmented.

No API key needed — Wikidata's public REST API is free. Uses asyncio + aiohttp
for concurrent lookups (200 sequential requests would take ~27 minutes).
"""
from __future__ import annotations

import asyncio
import json
import urllib.parse
import urllib.request
from dataclasses import dataclass

WIKIDATA_SEARCH_URL = "https://www.wikidata.org/w/api.php"
TIMEOUT_S = 15
MAX_CONCURRENT = 10


@dataclass
class WikidataHit:
    qid: str
    label: str
    description: str


def search_entities(name: str, language: str = "en", limit: int = 5) -> list[WikidataHit]:
    """Search Wikidata for entities matching a surface name (sync).

    OKT concept names are lowercased; Wikidata search is case-insensitive
    but works better with proper casing. We try the name as-is first.
    """
    params = urllib.parse.urlencode({
        "action": "wbsearchentities",
        "search": name,
        "language": language,
        "format": "json",
        "limit": str(limit),
    })
    url = f"{WIKIDATA_SEARCH_URL}?{params}"
    try:
        req = urllib.request.Request(url, headers={"User-Agent": "OKT-graph-analysis/1.0"})
        with urllib.request.urlopen(req, timeout=TIMEOUT_S) as resp:
            data = json.loads(resp.read().decode())
        hits = []
        for item in data.get("search", []):
            hits.append(WikidataHit(
                qid=item.get("id", ""),
                label=item.get("label", ""),
                description=item.get("description", ""),
            ))
        return hits
    except Exception:
        return []


def best_qid(name: str, context_hint: str = "") -> WikidataHit | None:
    """Return the single best Wikidata match for a name, or None."""
    hits = search_entities(name, limit=5)
    if not hits:
        return None
    if not context_hint:
        return hits[0]
    hint_lower = context_hint.lower()
    for h in hits:
        if hint_lower in (h.description or "").lower():
            return h
    return hits[0]


async def search_entities_async(
    session, name: str, language: str = "en", limit: int = 5
) -> list[WikidataHit]:
    """Async version using aiohttp for concurrent lookups."""
    params = urllib.parse.urlencode({
        "action": "wbsearchentities",
        "search": name,
        "language": language,
        "format": "json",
        "limit": str(limit),
    })
    url = f"{WIKIDATA_SEARCH_URL}?{params}"
    try:
        async with session.get(url) as resp:
            data = await resp.json(content_type=None)
        hits = []
        for item in data.get("search", []):
            hits.append(WikidataHit(
                qid=item.get("id", ""),
                label=item.get("label", ""),
                description=item.get("description", ""),
            ))
        return hits
    except Exception:
        return []


async def batch_best_qids(
    names: list[tuple[str, str]], max_concurrent: int = MAX_CONCURRENT
) -> dict[str, WikidataHit | None]:
    """Look up best Wikidata matches for a batch of (name, context_hint) pairs.

    Returns a dict {name: WikidataHit | None}. Uses the sync urllib search
    (Wikidata blocks aiohttp's user-agent with 403) run in a thread pool
    for concurrency.
    """
    import concurrent.futures

    results: dict[str, WikidataHit | None] = {}
    with concurrent.futures.ThreadPoolExecutor(max_workers=max_concurrent) as pool:
        future_to_name = {
            pool.submit(best_qid, name, hint): name for name, hint in names
        }
        for future in concurrent.futures.as_completed(future_to_name):
            name = future_to_name[future]
            try:
                results[name] = future.result()
            except Exception:
                results[name] = None
    return results
    # Prefer hits whose description contains the context hint (case-insensitive).
    hint_lower = context_hint.lower()
    for h in hits:
        if hint_lower in (h.description or "").lower():
            return h
    return hits[0]