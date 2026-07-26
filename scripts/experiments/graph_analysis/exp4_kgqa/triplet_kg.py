"""Build a triplet KG from the OKT corpus SOURCE TEXT (not facts).

A triplet KG construction pipeline (REBEL, EDC, etc.) extracts (subject,
relation, object) triples directly from raw text — the original articles —
not from pre-decomposed atomic facts. This is the fair comparison: both OKT
(facts) and the triplet KG start from the same raw source text, but the
triplet KG extracts typed relations while OKT decomposes into atomic facts.

The source text is chunked to ~2000 tokens per LLM call (fits Gemma's context
window with room for the system prompt + output). Triples are extracted from
each chunk, then merged into a single triplet KG.

Uses Gemma 4 31B (the same model OKT uses for fact/concept extraction) for a
fair comparison — both sides use the same model family.
"""
from __future__ import annotations

import json
import os
import sys
import time
from pathlib import Path
from concurrent.futures import ThreadPoolExecutor, as_completed

import httpx

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
sys.path.insert(0, str(Path(__file__).resolve().parent))

import okt_client

TRIPLET_SYSTEM = """You are a relation extraction model. Given a passage of text, extract (subject, relation, object) triples that capture the relationships explicitly stated in the text. The relation should be a short verb phrase (e.g., "founded", "acquired", "located_in", "works_at", "subsidiary_of", "plays_for"). The subject and object should be named entities (people, organizations, products, places, events).

Rules:
1. Extract only relationships explicitly stated in the passage.
2. Use lowercase for relations.
3. Use the entity's full name (not pronouns).
4. If no relationship is present, return an empty array.
5. Output ONLY a JSON array of {"subject": "...", "relation": "...", "object": "..."} objects, no other text."""

CACHE_PATH = Path(__file__).resolve().parent / "results" / "triplet_cache.jsonl"
TRIPLET_OUTPUT = Path(__file__).resolve().parent / "results" / "triplet_kg.json"
TRIPLE_LIST_PATH = Path(__file__).resolve().parent / "results" / "triplet_kg_all.json"

CHUNK_SIZE = 8000  # chars per chunk (~2000 tokens)


def load_cached_triples() -> dict[str, dict]:
    """Load cached triples keyed by chunk_id."""
    cache = {}
    if CACHE_PATH.exists():
        for line in CACHE_PATH.read_text().splitlines():
            if not line.strip():
                continue
            try:
                row = json.loads(line)
                cache[row["chunk_id"]] = row
            except json.JSONDecodeError:
                continue
    return cache


def chunk_text(text: str, chunk_size: int = CHUNK_SIZE) -> list[str]:
    """Split text into chunks at sentence boundaries near chunk_size."""
    if len(text) <= chunk_size:
        return [text]
    chunks = []
    start = 0
    while start < len(text):
        end = min(start + chunk_size, len(text))
        # Try to break at a sentence boundary.
        if end < len(text):
            for i in range(end, max(end - 200, start), -1):
                if text[i] in ".!?\n":
                    end = i + 1
                    break
        chunks.append(text[start:end])
        start = end
    return chunks


def extract_triples(chunk_id: str, chunk_text: str) -> list[dict]:
    """Extract triples from a text chunk via OpenRouter (Gemma)."""
    prompt = f'Passage: "{chunk_text[:6000]}"\n\nExtract relationship triples:'
    messages = [
        {"role": "system", "content": TRIPLET_SYSTEM},
        {"role": "user", "content": prompt},
    ]
    try:
        raw = okt_client.llm_chat(messages, model="google/gemma-4-31b-it")
    except Exception:
        return []
    if not raw:
        return []
    # Parse JSON array from response.
    s = raw.strip()
    if s.startswith("```"):
        lines = s.split("\n")
        lines = [l for l in lines if not l.strip().startswith("```")]
        s = "\n".join(lines).strip()
    try:
        triples = json.loads(s)
        if not isinstance(triples, list):
            return []
        return [t for t in triples if isinstance(t, dict) and "subject" in t and "relation" in t and "object" in t]
    except json.JSONDecodeError:
        start = s.find("[")
        if start >= 0:
            for i in range(len(s) - 1, start - 1, -1):
                if s[i] == "]":
                    try:
                        result = json.loads(s[start:i+1])
                        if isinstance(result, list):
                            return [t for t in result if isinstance(t, dict)]
                    except json.JSONDecodeError:
                        continue
        return []


def build_triplet_kg(max_sources: int = 0) -> dict:
    """Build a triplet KG from the corpus source text.

    Args:
        max_sources: maximum number of sources to process (0 = all).

    Returns a dict with stats + the triple list.
    """
    CACHE_PATH.parent.mkdir(parents=True, exist_ok=True)
    cache = load_cached_triples()
    print(f"Triplet cache: {len(cache)} chunks already processed")

    # Fetch source text directly from the DB.
    import asyncio, asyncpg
    from okt_db import DEFAULT_DSN, DEFAULT_REPO_SLUG, repo_id_for_slug

    async def fetch_sources():
        conn = await asyncpg.connect(DEFAULT_DSN)
        rid = await repo_id_for_slug(conn, DEFAULT_REPO_SLUG)
        limit_clause = f"LIMIT {max_sources}" if max_sources > 0 else ""
        rows = await conn.fetch(f"""
            SELECT s.id::text AS sid, s.parsed_title, s.parsed_text, s.published_at
            FROM okt_repository.sources s
            WHERE s.repository_id = $1
              AND s.parsed_text IS NOT NULL
              AND length(s.parsed_text) > 100
            ORDER BY s.parsed_at
            {limit_clause}
        """, rid)
        await conn.close()
        return [{"id": r["sid"], "title": r["parsed_title"], "text": r["parsed_text"],
                 "published_at": r["published_at"].isoformat() if r["published_at"] else None} for r in rows]

    sources = asyncio.run(fetch_sources())
    print(f"Loaded {len(sources)} sources with text")

    # Chunk each source and build the list of chunks to process.
    # Each chunk carries the source's published_at for temporal context.
    all_chunks = []  # (chunk_id, source_id, source_date, chunk_text)
    for src in sources:
        chunks = chunk_text(src["text"])
        src_date = src.get("published_at")
        for i, chunk in enumerate(chunks):
            cid = f"{src['id']}_chunk{i}"
            if cid not in cache:
                all_chunks.append((cid, src["id"], src_date, chunk))
    print(f"Total chunks: {sum(len(chunk_text(s['text'])) for s in sources) // 1} (approx)")
    print(f"Need to process {len(all_chunks)} new chunks (cached: {len(cache)})")

    if not all_chunks:
        print("All chunks already cached.")
    else:
        t0 = time.time()
        with open(CACHE_PATH, "a", encoding="utf-8") as cache_file:
            with ThreadPoolExecutor(max_workers=10) as pool:
                futures = {pool.submit(extract_triples, cid, text): (cid, sid, sdate) for cid, sid, sdate, text in all_chunks}
                done = 0
                n_with_triples = 0
                for future in as_completed(futures):
                    cid, sid, sdate = futures[future]
                    try:
                        triples = future.result()
                    except Exception:
                        triples = []
                    cache_row = {"chunk_id": cid, "source_id": sid, "published_at": sdate, "triples": triples}
                    cache_file.write(json.dumps(cache_row) + "\n")
                    cache_file.flush()
                    cache[cid] = cache_row
                    if triples:
                        n_with_triples += 1
                    done += 1
                    if done % 50 == 0:
                        elapsed = time.time() - t0
                        rate = done / elapsed if elapsed > 0 else 0
                        eta = (len(all_chunks) - done) / rate if rate > 0 else 0
                        print(f"  {done}/{len(all_chunks)} chunks ({rate:.1f}/s, ETA {eta:.0f}s), {n_with_triples} with triples")

    # Collect all triples from cache, with published_at from the source.
    all_triples = []
    for row in cache.values():
        src_date = row.get("published_at")
        for t in row.get("triples", []):
            all_triples.append({**t, "source_id": row.get("source_id", ""),
                               "published_at": src_date,
                               "chunk_id": row.get("chunk_id", "")})

    # Stats.
    relations = {}
    for t in all_triples:
        r = t["relation"].lower()
        relations[r] = relations.get(r, 0) + 1
    top_relations = sorted(relations.items(), key=lambda x: -x[1])[:20]
    subjects = set(t["subject"].lower() for t in all_triples)
    objects = set(t["object"].lower() for t in all_triples)

    result = {
        "n_sources": len(sources),
        "n_chunks_processed": len(cache),
        "n_chunks_with_triples": sum(1 for r in cache.values() if r.get("triples")),
        "n_triples": len(all_triples),
        "n_relation_types": len(relations),
        "n_unique_subjects": len(subjects),
        "n_unique_objects": len(objects),
        "top_relations": top_relations,
        "extraction_model": "google/gemma-4-31b-it",
        "method": "Triples extracted from raw source text (parsed_text), chunked at ~2000 tokens. Uses Gemma 4 31B (same model OKT uses for fact/concept extraction) for a fair comparison.",
    }
    TRIPLET_OUTPUT.write_text(json.dumps(result, indent=2))
    TRIPLE_LIST_PATH.write_text(json.dumps(all_triples, indent=2))

    print(f"\nTriplet KG built: {result['n_triples']} triples from {result['n_sources']} sources ({result['n_chunks_processed']} chunks)")
    print(f"Relation types: {result['n_relation_types']}")
    print(f"Unique subjects: {result['n_unique_subjects']}, objects: {result['n_unique_objects']}")
    print(f"Top relations: {top_relations[:10]}")
    return result


if __name__ == "__main__":
    max_sources = int(sys.argv[1]) if len(sys.argv) > 1 else 0
    build_triplet_kg(max_sources)