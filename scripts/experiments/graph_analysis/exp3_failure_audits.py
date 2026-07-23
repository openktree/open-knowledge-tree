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

FRAG_SYSTEM = """You are an entity-linking expert. Given a concept name and its context, return the most likely Wikidata entity it refers to. Respond in JSON: {"qid": "Q123", "label": "...", "confidence": "high|medium|low", "note": "..."}. If the name is too generic to map to a single entity, return {"qid": null, "label": null, "confidence": "low", "note": "generic"}."""

async def audit_fragmentation(conn: asyncpg.Connection, repo_id: str) -> dict:
    """Sample concept groups, map each to Wikidata, count fragmentation."""
    rng = random.Random(SEED)
    # Get all concept groups (lower(canonical_name)) with their contexts.
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
    # Sample, oversampling high-degree groups (where fragmentation matters most).
    sample = rng.sample(rows, min(SAMPLE_SIZE, len(rows)))

    # Map each to Wikidata concurrently (200 sequential calls would take ~27min).
    names_with_hints = [(r["name"], r["contexts"][0] if r["contexts"] else "") for r in sample]
    qid_results = await batch_best_qids(names_with_hints)

    qid_map: dict[str, dict] = {}
    qid_to_groups: dict[str, list[str]] = {}
    for r in sample:
        name = r["name"]
        hit = qid_results.get(name)
        if hit:
            qid_map[name] = {"qid": hit.qid, "label": hit.label, "desc": hit.description}
            qid_to_groups.setdefault(hit.qid, []).append(name)
        else:
            qid_map[name] = {"qid": None, "label": None, "desc": None}

    # Fragmentation: Q-IDs that appear under >1 OKT concept group.
    fragmented = {qid: groups for qid, groups in qid_to_groups.items() if len(groups) > 1}
    n_mapped = sum(1 for v in qid_map.values() if v["qid"])
    n_fragmented_entities = len(fragmented)
    n_fragmented_groups = sum(len(g) for g in fragmented.values())

    return {
        "n_sampled": len(sample),
        "n_mapped_to_wikidata": n_mapped,
        "n_unique_qids": len(qid_to_groups),
        "n_fragmented_entities": n_fragmented_entities,
        "n_fragmented_groups": n_fragmented_groups,
        "fragmentation_rate": round(n_fragmented_entities / max(n_mapped, 1), 4),
        "fragmented_examples": [
            {"qid": qid, "groups": groups}
            for qid, groups in sorted(fragmented.items(), key=lambda kv: -len(kv[1]))[:10]
        ],
        "sample_details": [{"name": n, **qid_map[n]} for n in sorted(qid_map.keys())[:50]],
    }


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

async def audit_recall(conn: asyncpg.Connection, repo_id: str) -> dict:
    """Sample facts, run spaCy NER, diff against OKT-linked concepts.

    Note: spaCy NER retrieves more entities but also more noise (fails to
    split/bound some concepts). This is a tradeoff, not a pure gold standard.
    We report raw recall and note the spaCy noise separately.
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
    # Re-sample deterministically.
    sample = rng.sample(rows, min(RECALL_SAMPLE, len(rows)))

    recalls = []
    for r in sample:
        fact_text = r["text"]
        okt_concepts = set(c for c in (r["okt_concepts"] or []) if c)
        # spaCy NER extraction.
        doc = nlp(fact_text)
        spacy_entities = set()
        for ent in doc.ents:
            if ent.label_ in ("PERSON", "ORG", "GPE", "LOC", "PRODUCT", "WORK_OF_ART", "EVENT", "FAC", "NORP", "LAW", "LANGUAGE"):
                spacy_entities.add(ent.text.lower().strip())
        # Also extract noun chunks ( spaCy gets more but noisier).
        for chunk in doc.noun_chunks:
            text = chunk.text.lower().strip()
            if len(text) > 2 and not text.startswith(("the ", "a ", "an ", "this ", "that ")):
                spacy_entities.add(text)

        # Recall: of the spaCy entities, how many are in OKT concepts?
        # Match by substring (OKT concept name in spaCy entity or vice versa).
        matched = set()
        for se in spacy_entities:
            for oc in okt_concepts:
                if se in oc or oc in se or se == oc:
                    matched.add(se)
                    break
        recall = len(matched) / max(len(spacy_entities), 1)
        recalls.append({
            "fid": r["fid"],
            "fact_text": fact_text[:200],
            "n_spacy_entities": len(spacy_entities),
            "n_okt_concepts": len(okt_concepts),
            "n_matched": len(matched),
            "recall": round(recall, 4),
            "spacy_entities": sorted(spacy_entities)[:15],
            "okt_concepts": sorted(okt_concepts)[:15],
            "missed": sorted(spacy_entities - matched)[:10],
        })

    mean_recall = float(np.mean([r["recall"] for r in recalls])) if recalls else 0
    median_recall = float(np.median([r["recall"] for r in recalls])) if recalls else 0

    return {
        "n_sampled": len(sample),
        "mean_recall": round(mean_recall, 4),
        "median_recall": round(median_recall, 4),
        "recall_distribution": {
            "p25": float(np.percentile([r["recall"] for r in recalls], 25)) if recalls else 0,
            "p75": float(np.percentile([r["recall"] for r in recalls], 75)) if recalls else 0,
            "p90": float(np.percentile([r["recall"] for r in recalls], 90)) if recalls else 0,
        },
        "caveat": (
            "spaCy NER retrieves more entities than LLM extraction but also more "
            "noise (fails to split/bound some concepts). This is a tradeoff, not a "
            "pure gold standard. Raw recall numbers are an upper bound on the gap; "
            "some 'missed' spaCy entities are noise, not real misses."
        ),
        "sample_details": recalls[:30],
    }


async def audit_hallucination(conn: asyncpg.Connection, repo_id: str) -> dict:
    """Sub-audit 3b: residual cross-contamination.

    For each concept, count facts where the concept name is a substring of
    the fact text (cheap proxy for 'actually mentioned') vs total linked facts.
    Concepts with a large gap (linked to many facts but rarely mentioned) are
    suspect — residual cross-contamination post-fix.
    """
    rows = await conn.fetch(
        """
        SELECT lower(c.canonical_name) AS name,
               count(DISTINCT fc.fact_id) AS n_linked_facts
        FROM okt_repository.concepts c
        JOIN okt_repository.fact_concepts fc ON fc.concept_id = c.id
        WHERE c.repository_id = $1
        GROUP BY lower(c.canonical_name)
        HAVING count(DISTINCT fc.fact_id) > 20
        ORDER BY n_linked_facts DESC
        LIMIT 200
        """,
        repo_id,
    )
    # For each high-degree concept, sample 20 linked facts and check substring.
    suspects = []
    for r in rows:
        name = r["name"]
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
        n_mentioned = sum(1 for fr in fact_rows if name in fr["text"].lower())
        mention_rate = n_mentioned / max(len(fact_rows), 1)
        if mention_rate < 0.5:
            suspects.append({
                "name": name,
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
            "the concept name is a substring of the fact text. Concepts mentioned "
            "in <50% of their linked facts are flagged as suspect (residual "
            "cross-contamination). This is a cheap proxy, not a gold standard."
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
        f3 = await audit_recall(conn, repo_id)
        print(f"  done in {time.time()-t3:.1f}s: mean recall={f3['mean_recall']}")

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
    print(f"\nFailure 3 — Missing concepts (recall): [spaCy comparator]")
    print(f"  Sampled: {f3['n_sampled']}, mean recall: {f3['mean_recall']:.1%}, median: {f3['median_recall']:.1%}")
    print(f"  ⚠ {f3['caveat']}")

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