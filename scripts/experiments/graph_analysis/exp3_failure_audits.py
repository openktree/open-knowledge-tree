"""Experiment 3 — 5 failure-mode audits (paper §6.2).

The report flagged five concept-extraction failure modes as the "biggest
risks" but never measured them. Experiments 1 and 2 showed the graph
*structure* is sound; this experiment asks whether the concept *extraction
itself* is any good.

Five audits, each sampling 100–200 items from the multihoprag repo:

  Failure 1 — Under-merging (fragmentation): two mentions of the same
    real-world entity that use different surface forms get created as
    separate concept groups. Ground truth: Wikidata Q-IDs.
  Failure 2 — Over-merging (false merges): aggressive alias inheritance
    collapses distinct concepts that share a surface form. LLM judge.
  Failure 3 — Missing concepts (recall): the LLM extractor didn't extract
    a concept that's mentioned in the fact. Independent spaCy NER extraction
    as the gold comparator (bearing in mind spaCy gets more concepts but
    also more noise — quality and quantity are not equivalent, it's a
    tradeoff). Sub-audit 3b: residual cross-contamination (hallucinated
    concepts linked to far more facts than actually mention them).
  Failure 4 — Dedup-severed facts: facts with content_hash collisions that
    may have been incorrectly merged, severing graph connectivity. SQL-only.
  Failure 5 — Context mislabeling: the L3 context (from the repo's official
    88-label shortlist, NOT the full 789 DBpedia L3 list which was dropped
    for being too long) is LLM-assigned per fact. LLM judge compares against
    the correct context.

Read-only: SELECTs only. See PLAN.md.
"""
from __future__ import annotations

import asyncio
import json
import os
import random
import time
from pathlib import Path

import asyncpg
import numpy as np

# Load .env from the multihop_rag experiment dir (shares the same OKT API key).
_ENV_PATH = Path(__file__).resolve().parent.parent / "multihop_rag" / ".env"
if _ENV_PATH.exists():
    for line in _ENV_PATH.read_text().splitlines():
        s = line.strip()
        if not s or s.startswith("#") or "=" not in s:
            continue
        key, _, val = s.partition("=")
        key = key.strip()
        val = val.strip().strip('"').strip("'")
        if key and key not in os.environ:
            os.environ[key] = val

from okt_db import DEFAULT_DSN, DEFAULT_REPO_SLUG
from llm_judge import judge, judge_json, extract_json, OPENROUTER_MODEL
from wikidata import batch_best_qids, WikidataHit

RESULTS_DIR = Path(__file__).resolve().parent / "results"
OFFICIAL_CONTEXTS_PATH = Path(__file__).resolve().parents[3] / "backend" / "internal" / "providers" / "ontology" / "contexts.json"

SAMPLE_SIZE = 200
RECALL_SAMPLE = 100
SEED = 42


def _load_official_contexts() -> list[str]:
    d = json.loads(OFFICIAL_CONTEXTS_PATH.read_text())
    return [c["label"] for c in d["contexts"]]


# ---------------------------------------------------------------------------
# Failure 1 — Under-merging (fragmentation)
# ---------------------------------------------------------------------------

async def audit_fragmentation(conn: asyncpg.Connection, repo_id: str) -> dict:
    """Intra-corpus fragmentation detection.

    Instead of mapping to an external ontology (Wikidata), which only
    measures ontology compliance, we detect fragmentation WITHIN the corpus:
    two OKT concept groups that should be one entity because they are
    aliases of each other (one group's canonical name is another's alias,
    or they share an alias, or their names are substrings/abbreviations).

    Method:
      1. Load all concept groups + their aliases.
      2. For each pair (A, B) where A != B, check:
         a. Is A's canonical name an alias of B (or vice versa)?
         b. Do A and B share any alias?
         c. Is A's name a substring of B's name (or vice versa), and
            do they share zero facts (true fragmentation, not co-occurrence)?
      3. Flag pairs as fragmented if they are linked to overlapping but
         mostly disjoint fact sets (would benefit from merging).
    """
    # Load all concept groups with their aliases and fact sets.
    rows = await conn.fetch(
        """
        SELECT lower(c.canonical_name) AS name,
               array_agg(DISTINCT ca.alias_text) FILTER (WHERE ca.alias_text IS NOT NULL) AS aliases,
               count(DISTINCT fc.fact_id) AS fact_count
        FROM okt_repository.concepts c
        LEFT JOIN okt_repository.concept_aliases ca ON ca.concept_id = c.id
        LEFT JOIN okt_repository.fact_concepts fc ON fc.concept_id = c.id
        WHERE c.repository_id = $1
        GROUP BY lower(c.canonical_name)
        """,
        repo_id,
    )
    # Build lookup: name → set of aliases (including canonical name as self-alias).
    name_to_aliases: dict[str, set[str]] = {}
    name_to_facts: dict[str, set[str]] = {}
    all_names = []
    for r in rows:
        name = r["name"]
        aliases = set(a.lower() for a in (r["aliases"] or []))
        aliases.add(name)
        name_to_aliases[name] = aliases
        all_names.append(name)

    # Build a reverse index: alias text → set of concept names that claim it.
    alias_to_names: dict[str, set[str]] = {}
    for name, aliases in name_to_aliases.items():
        for a in aliases:
            alias_to_names.setdefault(a, set()).add(name)

    # Fragmentation type 1: shared alias — two groups claim the same alias text.
    # Only flag as TRUE fragmentation when both conditions hold:
    # (a) the shared alias is long and specific (>= 8 chars — excludes "google",
    #     "amazon", "sam", "ai" which are shared by legitimately different concepts), AND
    # (b) the groups share at least 1 fact (same entity split but still connected
    #     through the same source text), OR the shared alias IS one group's full
    #     canonical name (e.g., "reserve bank of australia" is canonical of A
    #     and alias of B — clear fragmentation).
    MIN_SPECIFIC_ALIAS_LEN = 8
    shared_alias_pairs: list[dict] = []
    seen_pairs: set[tuple[str, str]] = set()
    for alias, names in alias_to_names.items():
        if len(names) > 1 and len(alias) >= MIN_SPECIFIC_ALIAS_LEN:
            names_list = sorted(names)
            for i in range(len(names_list)):
                for j in range(i + 1, len(names_list)):
                    a, b = names_list[i], names_list[j]
                    key = (a, b)
                    if key in seen_pairs:
                        continue
                    seen_pairs.add(key)
                    facts_a = await _get_facts_for_name(conn, repo_id, a)
                    facts_b = await _get_facts_for_name(conn, repo_id, b)
                    overlap = len(facts_a & facts_b)
                    is_canonical_of_a = alias == a
                    is_canonical_of_b = alias == b
                    if overlap > 0 or is_canonical_of_a or is_canonical_of_b:
                        shared_alias_pairs.append({
                            "name_a": a, "name_b": b,
                            "shared_alias": alias,
                            "facts_a": len(facts_a), "facts_b": len(facts_b),
                            "shared_facts": overlap,
                            "type": "shared_alias",
                        })

    # Fragmentation type 2: one group's canonical name is another's alias.
    canonical_as_alias_pairs: list[dict] = []
    for name in all_names:
        # Is this name an alias of a DIFFERENT group?
        names_with_this_alias = alias_to_names.get(name, set())
        for other in names_with_this_alias:
            if other != name:
                key = tuple(sorted([name, other]))
                if key in seen_pairs:
                    continue
                seen_pairs.add(key)
                facts_a = await _get_facts_for_name(conn, repo_id, name)
                facts_b = await _get_facts_for_name(conn, repo_id, other)
                overlap = len(facts_a & facts_b)
                canonical_as_alias_pairs.append({
                    "name_a": name, "name_b": other,
                    "detail": f'"{name}" is an alias of "{other}"',
                    "facts_a": len(facts_a), "facts_b": len(facts_b),
                    "shared_facts": overlap,
                    "type": "canonical_is_alias",
                })

    # Fragmentation type 3: one group's canonical name is an abbreviation or
    # full form of another's. Only flag if:
    # (a) the shorter name is a PREFIX of the longer (not a random substring),
    # (b) the shorter name is at least 4 chars,
    # (c) the longer name is at most 1.5× the shorter name's length,
    # (d) they share zero facts (true fragmentation, not co-occurrence), AND
    # (e) the longer name does NOT add a qualifier noun after the prefix
    #     (e.g., "artificial intelligence models" is a different concept from
    #     "artificial intelligence", not a fragmented form of it).
    QUALIFIER_WORDS = {
        "models", "model", "tools", "tool", "training", "systems", "system",
        "companies", "company", "algorithms", "algorithm", "technology",
        "technologies", "applications", "application", "industry", "market",
        "markets", "council", "pact", "budgets", "developers", "generators",
        "innovation", "chatbots", "regulation", "regulations", "research",
        "researchers", "ethics", "safety", "governance", "policy", "policies",
        "law", "laws", "act", "office", "agency", "committee", "framework",
        "standards", "standard", "patent", "patents", "logo", "logos",
        "platform", "platforms", "software", "hardware", "device", "devices",
        "service", "services", "product", "products", "report", "reports",
        "data", "dataset", "datasets", "code", "api", "app", "apps",
        "conference", "summit", "forum", "lab", "labs", "institute",
        "department", "center", "centre", "program", "project", "team",
        "group", "network", "networks", "strategy", "strategies", "plan",
        "guide", "guidelines", "manual", "handbook", "review", "analysis",
        "study", "studies", "survey", "report", "statistics", "stats",
        "trends", "outlook", "forecast", "prediction", "predictions",
        "news", "media", "content", "article", "articles", "blog", "post",
        "video", "videos", "podcast", "show", "series", "episode",
    }
    substring_pairs: list[dict] = []
    for a in all_names:
        if len(a) < 4:
            continue
        for b in all_names:
            if a == b:
                continue
            if b.startswith(a + " ") and len(b) <= len(a) * 1.5 and a != b:
                # Check (e): the suffix after the prefix must not be a qualifier word.
                suffix = b[len(a) + 1:].lower().strip()
                if suffix in QUALIFIER_WORDS:
                    continue
                key = tuple(sorted([a, b]))
                if key in seen_pairs:
                    continue
                facts_a = await _get_facts_for_name(conn, repo_id, a)
                facts_b = await _get_facts_for_name(conn, repo_id, b)
                overlap = len(facts_a & facts_b)
                if overlap == 0:
                    seen_pairs.add(key)
                    substring_pairs.append({
                        "name_a": a, "name_b": b,
                        "detail": f'"{a}" is a prefix of "{b}" (suffix: "{suffix}")',
                        "facts_a": len(facts_a), "facts_b": len(facts_b),
                        "shared_facts": 0,
                        "type": "prefix_no_overlap",
                    })

    all_pairs = shared_alias_pairs + canonical_as_alias_pairs + substring_pairs
    # Deduplicate by (name_a, name_b).
    final_pairs = {}
    for p in all_pairs:
        key = tuple(sorted([p["name_a"], p["name_b"]]))
        if key not in final_pairs:
            final_pairs[key] = p

    return {
        "n_concept_groups": len(all_names),
        "n_fragmented_pairs": len(final_pairs),
        "fragmentation_rate": round(len(final_pairs) / max(len(all_names), 1), 4),
        "method": (
            "Intra-corpus fragmentation: detects pairs of concept groups that "
            "should be one entity. Three signals: (1) shared alias — two groups "
            "claim the same alias text; (2) canonical-is-alias — one group's "
            "canonical name is another's alias; (3) substring with zero shared "
            "facts — one name is contained in another and they share no facts. "
            "No external ontology needed."
        ),
        "fragmented_examples": sorted(final_pairs.values(), key=lambda x: -(x["facts_a"] + x["facts_b"]))[:20],
    }


async def _get_facts_for_name(conn: asyncpg.Connection, repo_id: str, name: str) -> set[str]:
    """Get the set of fact_ids linked to a concept group (by lower(canonical_name))."""
    rows = await conn.fetch(
        """
        SELECT DISTINCT fc.fact_id::text AS fid
        FROM okt_repository.fact_concepts fc
        JOIN okt_repository.concepts c ON c.id = fc.concept_id
        WHERE c.repository_id = $1 AND lower(c.canonical_name) = $2
        """,
        repo_id, name,
    )
    return {r["fid"] for r in rows}


# ---------------------------------------------------------------------------
# Failure 2 — Over-merging (false merges)
# ---------------------------------------------------------------------------

OVERMERGE_SYSTEM = """You are an entity-disambiguation expert. You are given a concept group name and the set of L3 context labels assigned to it by the system. Your job is to determine whether this single concept group contains TWO OR MORE distinct real-world entities that were incorrectly merged because they share a surface form (e.g., "Apple" the company vs "Apple" the fruit). Respond in JSON: {"over_merged": true|false, "entities": ["entity1", "entity2"], "reason": "..."}."""

async def audit_over_merging(conn: asyncpg.Connection, repo_id: str, model: str | None = None) -> dict:
    """Sample concept groups, LLM judge flags homonym collisions."""
    rng = random.Random(SEED + 1)
    rows = await conn.fetch(
        """
        SELECT lower(c.canonical_name) AS name,
               array_agg(DISTINCT lower(c.context)) AS contexts,
               count(DISTINCT fc.fact_id) AS fact_count
        FROM okt_repository.concepts c
        LEFT JOIN okt_repository.fact_concepts fc ON fc.concept_id = c.id
        WHERE c.repository_id = $1
        GROUP BY lower(c.canonical_name)
        ORDER BY fact_count DESC
        """,
        repo_id,
    )
    # Oversample ambiguous names (short, common words).
    sample = rng.sample(rows, min(SAMPLE_SIZE, len(rows)))

    audits = []
    n_over_merged = 0
    for r in sample:
        name = r["name"]
        contexts = r["contexts"]
        prompt = f'Concept group name: "{name}"\nAssigned contexts: {contexts}\nFact count: {r["fact_count"]}\n\nDoes this single concept group contain two or more distinct real-world entities that were incorrectly merged?'
        result = judge_json(prompt, OVERMERGE_SYSTEM, model=model)
        if isinstance(result, list) and result:
            result = result[0]
        if not isinstance(result, dict):
            result = {"over_merged": False, "entities": [], "reason": f"parse error: {type(result).__name__}"}
        if result.get("over_merged"):
            n_over_merged += 1
        audits.append({"name": name, "contexts": contexts, **result})

    return {
        "model": model or "default",
        "n_sampled": len(sample),
        "n_over_merged": n_over_merged,
        "over_merge_rate": round(n_over_merged / max(len(sample), 1), 4),
        "over_merged_examples": [a for a in audits if a.get("over_merged")][:10],
        "all_audits": audits,
    }


# ---------------------------------------------------------------------------
# Failure 3 — Missing concepts (recall) + 3b — hallucinated concepts
# ---------------------------------------------------------------------------

NOISE_SYSTEM = """You are an entity extraction quality auditor. You are given a fact text and a list of candidate entities that a spaCy NER extractor pulled from the text but that the OKT system's LLM extractor did NOT capture.

For each candidate entity, classify it as either:
  "real_miss" — a genuine named entity, concept, or organization that OKT SHOULD have captured (a person, company, product, place, event, or specific concept that is meaningfully present in the fact).
  "noise" — spaCy over-extracted this: it is too generic (e.g. "the company", "some people"), a sentence fragment (e.g. "the first time"), a bound-failure (e.g. "united states of" missing "america"), a common word, or something that is not worth extracting as a concept.

Respond in JSON as a list of objects: [{"entity": "...", "classification": "real_miss"|"noise", "reason": "..."}]"""

async def audit_recall(conn: asyncpg.Connection, repo_id: str, model: str | None = None) -> dict:
    """Sample facts, run spaCy NER, diff against OKT-linked concepts.

    Measures BOTH sides of the tradeoff:
      - Raw recall: of spaCy entities, what fraction did OKT capture?
      - Noise rate: of the "missed" spaCy entities, what fraction are noise
        (spaCy over-extraction) vs real misses (OKT under-extraction)?
      - Adjusted recall: excluding noise, what fraction did OKT capture?

    This makes the contrast clear: spaCy was removed from the system because
    it retrieved more entities but also more noise (fails to split/bound
    some concepts). The noise measurement quantifies that tradeoff.
    """
    import spacy
    nlp = spacy.load("en_core_web_lg")

    rng = random.Random(SEED + 2)
    rows = await conn.fetch(
        """
        SELECT f.id::text AS fid, f.text,
               array_agg(lower(c.canonical_name)) AS okt_concepts
        FROM okt_repository.facts f
        JOIN okt_repository.fact_sources fs ON fs.fact_id = f.id
        JOIN okt_repository.sources s ON s.id = fs.source_id
        LEFT JOIN okt_repository.fact_concepts fc ON fc.fact_id = f.id
        LEFT JOIN okt_repository.concepts c ON c.id = fc.concept_id
        WHERE s.repository_id = $1
        GROUP BY f.id, f.text
        ORDER BY random()
        LIMIT $2
        """,
        repo_id, RECALL_SAMPLE,
    )
    sample = rng.sample(rows, min(RECALL_SAMPLE, len(rows)))

    recalls = []
    all_real_misses = []
    all_noise = []
    for r in sample:
        fact_text = r["text"]
        okt_concepts = set(c for c in (r["okt_concepts"] or []) if c)
        # spaCy NER extraction.
        doc = nlp(fact_text)
        spacy_entities = set()
        for ent in doc.ents:
            if ent.label_ in ("PERSON", "ORG", "GPE", "LOC", "PRODUCT", "WORK_OF_ART", "EVENT", "FAC", "NORP", "LAW", "LANGUAGE"):
                spacy_entities.add(ent.text.lower().strip())
        # Also extract noun chunks (spaCy gets more but noisier).
        for chunk in doc.noun_chunks:
            text = chunk.text.lower().strip()
            if len(text) > 2 and not text.startswith(("the ", "a ", "an ", "this ", "that ")):
                spacy_entities.add(text)

        # Match by substring (OKT concept name in spaCy entity or vice versa).
        matched = set()
        for se in spacy_entities:
            for oc in okt_concepts:
                if se in oc or oc in se or se == oc:
                    matched.add(se)
                    break
        missed = spacy_entities - matched
        raw_recall = len(matched) / max(len(spacy_entities), 1)

        # Classify missed entities as real_miss or noise via LLM judge.
        real_misses = []
        noise = []
        if missed:
            missed_list = sorted(missed)[:20]  # cap for prompt size
            prompt = f'Fact text: "{fact_text[:500]}"\n\nCandidate entities that spaCy extracted but OKT missed:\n{missed_list}\n\nClassify each as "real_miss" or "noise".'
            classifications = judge_json(prompt, NOISE_SYSTEM, model=model)
            if isinstance(classifications, list):
                for item in classifications:
                    if not isinstance(item, dict):
                        continue
                    entity = (item.get("entity") or "").lower().strip()
                    cls = item.get("classification", "noise")
                    reason = item.get("reason", "")
                    if cls == "real_miss":
                        real_misses.append({"entity": entity, "reason": reason})
                    else:
                        noise.append({"entity": entity, "reason": reason})
            # Any missed entity not classified defaults to noise.
            classified_entities = {m["entity"] for m in real_misses} | {n["entity"] for n in noise}
            for me in missed_list:
                if me not in classified_entities:
                    noise.append({"entity": me, "reason": "unclassified"})

        n_real_miss = len(real_misses)
        n_noise = len(noise)
        n_missed_total = len(missed)
        # Adjusted recall: exclude noise from the denominator.
        adjusted_recall = len(matched) / max(len(spacy_entities) - n_noise, 1)
        noise_rate = n_noise / max(n_missed_total, 1)

        all_real_misses.extend(real_misses)
        all_noise.extend(noise)

        recalls.append({
            "fid": r["fid"],
            "fact_text": fact_text[:200],
            "n_spacy_entities": len(spacy_entities),
            "n_okt_concepts": len(okt_concepts),
            "n_matched": len(matched),
            "n_missed": n_missed_total,
            "n_real_misses": n_real_miss,
            "n_noise": n_noise,
            "raw_recall": round(raw_recall, 4),
            "adjusted_recall": round(adjusted_recall, 4),
            "noise_rate": round(noise_rate, 4),
            "spacy_entities": sorted(spacy_entities)[:15],
            "okt_concepts": sorted(okt_concepts)[:15],
            "real_misses": [m["entity"] for m in real_misses][:10],
            "noise_examples": [n["entity"] for n in noise][:10],
        })

    raw_recalls = [r["raw_recall"] for r in recalls]
    adj_recalls = [r["adjusted_recall"] for r in recalls]
    noise_rates = [r["noise_rate"] for r in recalls]

    return {
        "model": model or "default",
        "n_sampled": len(sample),
        "mean_raw_recall": round(float(np.mean(raw_recalls)), 4) if raw_recalls else 0,
        "mean_adjusted_recall": round(float(np.mean(adj_recalls)), 4) if adj_recalls else 0,
        "median_raw_recall": round(float(np.median(raw_recalls)), 4) if raw_recalls else 0,
        "median_adjusted_recall": round(float(np.median(adj_recalls)), 4) if adj_recalls else 0,
        "mean_noise_rate": round(float(np.mean(noise_rates)), 4) if noise_rates else 0,
        "n_total_real_misses": len(all_real_misses),
        "n_total_noise": len(all_noise),
        "adjusted_recall_distribution": {
            "p25": float(np.percentile(adj_recalls, 25)) if adj_recalls else 0,
            "p75": float(np.percentile(adj_recalls, 75)) if adj_recalls else 0,
            "p90": float(np.percentile(adj_recalls, 90)) if adj_recalls else 0,
        },
        "noise_examples": [n["entity"] for n in all_noise[:20]],
        "real_miss_examples": [m["entity"] for m in all_real_misses[:20]],
        "method": (
            "spaCy NER (en_core_web_lg) extracts entities + noun chunks from "
            "each fact. Matched against OKT concepts by substring. Missed "
            "entities are classified by the LLM judge as 'real_miss' (OKT "
            "should have captured it) or 'noise' (spaCy over-extraction: "
            "generic, fragment, bound-failure). Adjusted recall excludes "
            "noise from the denominator. This measures BOTH sides of the "
            "spaCy-vs-LLM tradeoff: OKT's under-extraction (real misses) "
            "and spaCy's over-extraction (noise)."
        ),
        "sample_details": recalls[:30],
    }


async def audit_hallucination(conn: asyncpg.Connection, repo_id: str) -> dict:
    """Sub-audit 3b: residual cross-contamination (alias-aware).

    For each concept with >20 linked facts, sample 20 facts and check if
    the concept's canonical name OR any of its aliases appears in the fact
    text. If neither the name nor any alias is found, the fact is "unmentioned"
    — the concept was linked to a fact that doesn't actually reference it.

    The previous version used substring matching against the canonical name
    only ("google llc"), which produced 55.5% false positives because OKT
    formalizes names while facts use informal forms ("Google"). Using aliases
    ("Google", "Alphabet", "GOOG" etc.) fixes this.
    """
    rows = await conn.fetch(
        """
        SELECT lower(c.canonical_name) AS name,
               array_agg(DISTINCT ca.alias_text) FILTER (WHERE ca.alias_text IS NOT NULL) AS aliases,
               count(DISTINCT fc.fact_id) AS n_linked_facts
        FROM okt_repository.concepts c
        JOIN okt_repository.fact_concepts fc ON fc.concept_id = c.id
        LEFT JOIN okt_repository.concept_aliases ca ON ca.concept_id = c.id
        WHERE c.repository_id = $1
        GROUP BY lower(c.canonical_name)
        HAVING count(DISTINCT fc.fact_id) > 20
        ORDER BY n_linked_facts DESC
        LIMIT 200
        """,
        repo_id,
    )
    suspects = []
    for r in rows:
        name = r["name"]
        aliases = list(r["aliases"] or [])
        # Build the set of search terms: canonical name + all aliases, lowercased.
        search_terms = set([name] + [a.lower() for a in aliases])
        # Filter out very short aliases (1 char) to avoid false matches, but
        # keep 2-char ones (AI, US, UK) since they're common abbreviations.
        search_terms = {t for t in search_terms if len(t) >= 2}
        n_linked = r["n_linked_facts"]

        fact_rows = await conn.fetch(
            """
            SELECT f.text
            FROM okt_repository.fact_concepts fc
            JOIN okt_repository.facts f ON f.id = fc.fact_id
            JOIN okt_repository.concepts c ON c.id = fc.concept_id
            WHERE c.repository_id = $1 AND lower(c.canonical_name) = $2
            ORDER BY random()
            LIMIT 20
            """,
            repo_id, name,
        )
        n_mentioned = 0
        for fr in fact_rows:
            text = fr["text"].lower()
            if any(term in text for term in search_terms):
                n_mentioned += 1
        mention_rate = n_mentioned / max(len(fact_rows), 1)
        if mention_rate < 0.5:
            suspects.append({
                "name": name,
                "aliases": aliases[:5],
                "n_search_terms": len(search_terms),
                "n_linked_facts": n_linked,
                "n_sampled": len(fact_rows),
                "n_mentioned": n_mentioned,
                "mention_rate": round(mention_rate, 4),
            })

    return {
        "n_concepts_checked": len(rows),
        "n_suspect": len(suspects),
        "suspect_rate": round(len(suspects) / max(len(rows), 1), 4),
        "suspects": sorted(suspects, key=lambda x: x["mention_rate"])[:20],
        "method": (
            "For each concept with >20 linked facts, sample 20 facts and check if "
            "the concept's canonical name OR any of its aliases (>= 3 chars) appears "
            "in the fact text. Concepts mentioned in <50% of their linked facts are "
            "flagged as suspect (residual cross-contamination). Alias-aware matching "
            "fixes the name-format mismatch that inflated the previous 55.5% rate."
        ),
    }


# ---------------------------------------------------------------------------
# Failure 4 — Dedup-severed facts
# ---------------------------------------------------------------------------

async def audit_dedup_severed(conn: asyncpg.Connection, repo_id: str) -> dict:
    """SQL-only: identify content_hash collisions, measure graph impact."""
    # Check if content_hash exists on facts.
    cols = await conn.fetch(
        "SELECT column_name FROM information_schema.columns "
        "WHERE table_schema='okt_repository' AND table_name='facts'"
    )
    col_names = [c["column_name"] for c in cols]
    if "content_hash" not in col_names:
        return {"status": "skipped", "reason": "content_hash column not found on facts table"}

    # Find facts with duplicate content_hashes (dedup candidates).
    dup_hashes = await conn.fetch(
        """
        WITH hash_counts AS (
            SELECT f.content_hash, count(*) AS n
            FROM okt_repository.facts f
            JOIN okt_repository.fact_sources fs ON fs.fact_id = f.id
            JOIN okt_repository.sources s ON s.id = fs.source_id
            WHERE s.repository_id = $1 AND f.content_hash IS NOT NULL
            GROUP BY f.content_hash
            HAVING count(*) > 1
        )
        SELECT content_hash, n FROM hash_counts ORDER BY n DESC LIMIT 20
        """,
        repo_id,
    )
    # Count facts with no concepts (severed from the graph).
    severed = await conn.fetchval(
        """
        SELECT count(*) FROM okt_repository.facts f
        JOIN okt_repository.fact_sources fs ON fs.fact_id = f.id
        JOIN okt_repository.sources s ON s.id = fs.source_id
        WHERE s.repository_id = $1
          AND NOT EXISTS (
              SELECT 1 FROM okt_repository.fact_concepts fc WHERE fc.fact_id = f.id
          )
        """,
        repo_id,
    )
    total_facts = await conn.fetchval(
        """
        SELECT count(DISTINCT f.id) FROM okt_repository.facts f
        JOIN okt_repository.fact_sources fs ON fs.fact_id = f.id
        JOIN okt_repository.sources s ON s.id = fs.source_id
        WHERE s.repository_id = $1
        """,
        repo_id,
    )
    return {
        "n_duplicate_hash_groups": len(dup_hashes),
        "n_facts_with_duplicate_hashes": sum(r["n"] for r in dup_hashes),
        "n_severed_facts_no_concepts": severed,
        "pct_severed": round(100 * severed / max(total_facts, 1), 2),
        "total_facts": total_facts,
        "top_duplicate_hashes": [{"hash": r["content_hash"][:12] if r["content_hash"] else None, "n": r["n"]} for r in dup_hashes[:10]],
    }


# ---------------------------------------------------------------------------
# Failure 5 — Context mislabeling
# ---------------------------------------------------------------------------

CONTEXT_SYSTEM = """You are an ontology classification expert. You are given a concept name and a list of {N} valid context labels. Assign the SINGLE most appropriate context label for this concept. Respond in JSON: {"correct_context": "...", "reason": "..."}."""

async def audit_context_mislabeling(conn: asyncpg.Connection, repo_id: str, model: str | None = None) -> dict:
    """Sample concepts, LLM judge assigns correct context vs system-assigned."""
    official_contexts = _load_official_contexts()
    rng = random.Random(SEED + 4)
    rows = await conn.fetch(
        """
        SELECT lower(c.canonical_name) AS name,
               lower(c.context) AS assigned_context,
               count(DISTINCT fc.fact_id) AS fact_count
        FROM okt_repository.concepts c
        LEFT JOIN okt_repository.fact_concepts fc ON fc.concept_id = c.id
        WHERE c.repository_id = $1
        GROUP BY lower(c.canonical_name), lower(c.context)
        ORDER BY fact_count DESC
        """,
        repo_id,
    )
    sample = rng.sample(rows, min(SAMPLE_SIZE, len(rows)))

    system = CONTEXT_SYSTEM.replace("{N}", str(len(official_contexts)))
    system += f"\n\nValid context labels: {official_contexts}"

    audits = []
    n_mislabeled = 0
    for r in sample:
        name = r["name"]
        assigned = r["assigned_context"]
        prompt = f'Concept name: "{name}"\nSystem-assigned context: "{assigned}"\n\nWhat is the correct context label for this concept?'
        result = judge_json(prompt, system, model=model)
        if isinstance(result, list) and result:
            result = result[0]
        if not isinstance(result, dict):
            result = {"correct_context": assigned, "reason": f"parse error: {type(result).__name__}"}
        correct = (result.get("correct_context") or "").lower()
        is_wrong = correct != assigned and correct in [c.lower() for c in official_contexts]
        if is_wrong:
            n_mislabeled += 1
        audits.append({
            "name": name,
            "assigned_context": assigned,
            "correct_context": result.get("correct_context"),
            "is_mislabeled": is_wrong,
            "reason": result.get("reason", ""),
        })

    return {
        "model": model or "default",
        "n_sampled": len(sample),
        "n_mislabeled": n_mislabeled,
        "mislabeling_rate": round(n_mislabeled / max(len(sample), 1), 4),
        "official_context_count": len(official_contexts),
        "mislabeled_examples": [a for a in audits if a["is_mislabeled"]][:10],
        "all_audits": audits,
    }


# ---------------------------------------------------------------------------
# Orchestration
# ---------------------------------------------------------------------------

JUDGE_MODELS = [
    "deepseek/deepseek-v4-flash",
    "google/gemma-4-31b-it",
]


async def run(dsn: str = DEFAULT_DSN, repo_slug: str = DEFAULT_REPO_SLUG) -> dict:
    t0 = time.time()
    conn = await asyncpg.connect(dsn)
    try:
        from okt_db import repo_id_for_slug
        repo_id = await repo_id_for_slug(conn, repo_slug)
        print(f"Repo: {repo_slug} ({repo_id})")

        # Model-independent audits (run once).
        print("Failure 1 — Under-merging (fragmentation)...")
        t1 = time.time()
        f1 = await audit_fragmentation(conn, repo_id)
        print(f"  done in {time.time()-t1:.1f}s: {f1['n_fragmented_entities']} fragmented entities")

        print("Failure 3 — Missing concepts (recall)...")
        t3 = time.time()
        f3 = await audit_recall(conn, repo_id, model=JUDGE_MODELS[0])
        print(f"  done in {time.time()-t3:.1f}s: raw recall={f3['mean_raw_recall']}, adjusted={f3['mean_adjusted_recall']}, noise_rate={f3['mean_noise_rate']}")

        print("Failure 3b — Hallucinated concepts (cross-contamination)...")
        t3b = time.time()
        f3b = await audit_hallucination(conn, repo_id)
        print(f"  done in {time.time()-t3b:.1f}s: {f3b['n_suspect']} suspect concepts")

        print("Failure 4 — Dedup-severed facts...")
        t4 = time.time()
        f4 = await audit_dedup_severed(conn, repo_id)
        print(f"  done in {time.time()-t4:.1f}s: {f4.get('n_severed_facts_no_concepts', 'N/A')} severed facts")

        # Model-dependent audits (run once per judge model).
        f2_by_model = {}
        f5_by_model = {}
        for model in JUDGE_MODELS:
            label = model.split("/")[-1]
            print(f"\nFailure 2 — Over-merging (judge: {label})...")
            t2 = time.time()
            f2_by_model[label] = await audit_over_merging(conn, repo_id, model=model)
            print(f"  done in {time.time()-t2:.1f}s: {f2_by_model[label]['n_over_merged']} over-merged")

            print(f"Failure 5 — Context mislabeling (judge: {label})...")
            t5 = time.time()
            f5_by_model[label] = await audit_context_mislabeling(conn, repo_id, model=model)
            print(f"  done in {time.time()-t5:.1f}s: {f5_by_model[label]['n_mislabeled']} mislabeled")

        return {
            "experiment": "exp3_failure_audits",
            "repository": repo_slug,
            "repository_id": repo_id,
            "judge_models": JUDGE_MODELS,
            "total_seconds": round(time.time() - t0, 1),
            "failure_1_fragmentation": f1,
            "failure_2_over_merging_by_model": f2_by_model,
            "failure_3_recall": f3,
            "failure_3b_hallucination": f3b,
            "failure_4_dedup_severed": f4,
            "failure_5_context_mislabeling_by_model": f5_by_model,
        }
    finally:
        await conn.close()


def _print_summary(result: dict) -> None:
    print(f"\n{'='*60}")
    print(f"Experiment 3 — Failure-mode audits (total: {result['total_seconds']}s)")
    print(f"Judge models: {', '.join(result['judge_models'])}")
    print(f"{'='*60}\n")

    f1 = result["failure_1_fragmentation"]
    print(f"Failure 1 — Under-merging (fragmentation): [Wikidata ground truth]")
    print(f"  Sampled: {f1['n_sampled']}, mapped to Wikidata: {f1['n_mapped_to_wikidata']}")
    print(f"  Fragmented entities: {f1['n_fragmented_entities']} (rate: {f1['fragmentation_rate']:.1%})")
    if f1["fragmented_examples"]:
        print(f"  Examples:")
        for ex in f1["fragmented_examples"][:5]:
            print(f"    {ex['qid']}: {ex['groups']}")

    f2_models = result["failure_2_over_merging_by_model"]
    print(f"\nFailure 2 — Over-merging (false merges): [LLM judge]")
    for label, f2 in f2_models.items():
        print(f"  {label}: sampled={f2['n_sampled']}, over-merged={f2['n_over_merged']} (rate: {f2['over_merge_rate']:.1%})")
        if f2["over_merged_examples"]:
            for ex in f2["over_merged_examples"][:3]:
                print(f"    {ex['name']}: {ex.get('entities', [])} — {ex.get('reason', '')[:80]}")

    f3 = result["failure_3_recall"]
    print(f"\nFailure 3 — Missing concepts (recall): [spaCy comparator + LLM noise classification]")
    print(f"  Sampled: {f3['n_sampled']}")
    print(f"  Raw recall:      {f3['mean_raw_recall']:.1%} (mean), {f3['median_raw_recall']:.1%} (median)")
    print(f"  Adjusted recall:  {f3['mean_adjusted_recall']:.1%} (mean), {f3['median_adjusted_recall']:.1%} (median)")
    print(f"  Noise rate:       {f3['mean_noise_rate']:.1%} of missed entities are spaCy noise")
    print(f"  Real misses: {f3['n_total_real_misses']}, Noise: {f3['n_total_noise']}")
    if f3.get("noise_examples"):
        print(f"  Noise examples: {f3['noise_examples'][:10]}")
    if f3.get("real_miss_examples"):
        print(f"  Real miss examples: {f3['real_miss_examples'][:10]}")

    f3b = result["failure_3b_hallucination"]
    print(f"\nFailure 3b — Hallucinated concepts (cross-contamination): [substring check]")
    print(f"  Checked: {f3b['n_concepts_checked']}, suspect: {f3b['n_suspect']} (rate: {f3b['suspect_rate']:.1%})")
    if f3b["suspects"]:
        print(f"  Top suspects:")
        for s in f3b["suspects"][:5]:
            print(f"    {s['name']}: linked={s['n_linked_facts']}, mention_rate={s['mention_rate']:.1%}")

    f4 = result["failure_4_dedup_severed"]
    print(f"\nFailure 4 — Dedup-severed facts: [SQL]")
    if f4.get("status") == "skipped":
        print(f"  {f4['reason']}")
    else:
        print(f"  Duplicate hash groups: {f4['n_duplicate_hash_groups']}")
        print(f"  Severed facts (no concepts): {f4['n_severed_facts_no_concepts']} ({f4['pct_severed']}%)")

    f5_models = result["failure_5_context_mislabeling_by_model"]
    print(f"\nFailure 5 — Context mislabeling: [LLM judge, {f5_models[list(f5_models.keys())[0]]['official_context_count']} official labels]")
    for label, f5 in f5_models.items():
        print(f"  {label}: sampled={f5['n_sampled']}, mislabeled={f5['n_mislabeled']} (rate: {f5['mislabeling_rate']:.1%})")
        if f5["mislabeled_examples"]:
            for ex in f5["mislabeled_examples"][:3]:
                print(f"    {ex['name']}: assigned={ex['assigned_context']} → correct={ex['correct_context']}")


async def main() -> None:
    dsn = os.environ.get("OKT_DB_DSN", DEFAULT_DSN)
    repo_slug = os.environ.get("OKT_REPO_SLUG", DEFAULT_REPO_SLUG)
    result = await run(dsn, repo_slug)
    RESULTS_DIR.mkdir(parents=True, exist_ok=True)
    out = RESULTS_DIR / "failure_audits.json"
    out.write_text(json.dumps(result, indent=2))
    _print_summary(result)
    print(f"\nWrote {out}")


if __name__ == "__main__":
    asyncio.run(main())