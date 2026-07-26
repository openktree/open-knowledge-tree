"""Run the KGQA head-to-head benchmark: 3 retrieval conditions on the same
MultiHop-RAG questions, scored per hop depth.

Conditions:
  (a) triplet_kg  — retrieve triples matching the question, synthesize answer.
  (b) concept_walk — getRelatedConcepts -> getConceptFacts, synthesize answer.
  (c) facts_direct — searchFacts(question), synthesize answer (the baseline).

Resumable: skips ids already in each variant's predictions file.

CLI:
  python3 run_kgqa.py                          # all 3 variants, all questions
  python3 run_kgqa.py --sample 500             # 500 random questions
  python3 run_kgqa.py --variant triplet_kg     # only triplet_kg
  python3 run_kgqa.py --concurrency 10         # parallel questions
"""
from __future__ import annotations

import argparse
import json
import os
import random
import sys
import time
from concurrent.futures import ThreadPoolExecutor, as_completed
from pathlib import Path
from typing import Any

# Add parent dirs for imports.
sys.path.insert(0, str(Path(__file__).resolve().parent))

import okt_client
from triplet_kg import load_cached_triples

RESULTS_DIR = Path(__file__).resolve().parent / "results"
QUERIES_PATH = Path(__file__).resolve().parents[2] / "multihop_rag" / "dataset" / "queries.jsonl"
PREDICTIONS_DIR = RESULTS_DIR / "predictions"

ANSWER_SYSTEM = """You are a question-answering assistant. Given a question and a set of evidence facts (or triples), answer the question as precisely as possible. If the evidence does not contain enough information to answer, say "Insufficient information."

Format your answer as: The answer to the question is "<short answer>".

Rules:
1. Base your answer ONLY on the provided evidence.
2. If the evidence is ambiguous or insufficient, say "Insufficient information."
3. Keep the answer short — a name, a yes/no, a number, or a brief phrase.
4. Do not add explanations or hedging."""

TRIPLET_QUERY_SYSTEM = """You are a search query builder for a knowledge graph of (subject, relation, object) triples. Given a question, produce 1-5 keyword queries that would match triples in the KG. Each query should be a short phrase (entity name or relation keyword). Output ONLY a JSON array of strings, no other text."""

CONCEPT_QUERY_SYSTEM = """You are a search query builder for a concept-tagged knowledge base. Given a question, produce 1-5 short noun phrases that are likely substrings of concept names in the KB. Output ONLY a JSON array of strings, no other text."""

FACT_QUERY_SYSTEM = """You are a search query builder for a full-text search index of atomic facts. Given a question, produce 1-5 websearch_to_tsquery strings (keyword-rich, no stopwords). Output ONLY a JSON array of strings, no other text."""


def load_queries() -> list[dict]:
    queries = []
    with open(QUERIES_PATH, "r", encoding="utf-8") as f:
        for line in f:
            if line.strip():
                queries.append(json.loads(line))
    return queries


def load_predictions(variant: str) -> dict[str, dict]:
    path = PREDICTIONS_DIR / f"predictions_{variant}.jsonl"
    preds = {}
    if path.exists():
        for line in path.read_text().splitlines():
            if line.strip():
                row = json.loads(line)
                preds[row["id"]] = row
    return preds


def save_prediction(variant: str, row: dict) -> None:
    PREDICTIONS_DIR.mkdir(parents=True, exist_ok=True)
    path = PREDICTIONS_DIR / f"predictions_{variant}.jsonl"
    with open(path, "a", encoding="utf-8") as f:
        f.write(json.dumps(row) + "\n")


def llm_json_list(question: str, system: str) -> list[str]:
    """Call LLM and parse a JSON array of strings."""
    try:
        messages = [
            {"role": "system", "content": system},
            {"role": "user", "content": f"Question: {question}\n\nQueries:"},
        ]
        raw = okt_client.llm_chat(messages)
    except Exception:
        return [question]
    if not raw:
        return [question]
    s = raw.strip()
    if s.startswith("```"):
        lines = s.split("\n")
        lines = [l for l in lines if not l.strip().startswith("```")]
        s = "\n".join(lines).strip()
    try:
        result = json.loads(s)
        if isinstance(result, list):
            return [str(x) for x in result]
    except json.JSONDecodeError:
        pass
    # Fallback: extract [ ... ] block.
    start = s.find("[")
    if start >= 0:
        for i in range(len(s) - 1, start - 1, -1):
            if s[i] == "]":
                try:
                    result = json.loads(s[start:i+1])
                    if isinstance(result, list):
                        return [str(x) for x in result]
                except json.JSONDecodeError:
                    continue
    return [question]


def synthesize_answer(question: str, evidence: list[dict], evidence_type: str = "fact") -> dict:
    """Synthesize an answer from evidence (facts or triples)."""
    if evidence_type == "triple":
        evidence_text = "\n".join(
            f"- [{e.get('published_at', 'no date')}] {e.get('subject', '')} | {e.get('relation', '')} | {e.get('object', '')}"
            for e in evidence[:30]
        )
    else:
        evidence_text = "\n".join(
            f"- {e.get('text', '')}"
            for e in evidence[:30]
        )
    if not evidence_text.strip():
        evidence_text = "(no evidence retrieved)"
    prompt = f"Question: {question}\n\nEvidence:\n{evidence_text}\n\nAnswer:"
    messages = [
        {"role": "system", "content": ANSWER_SYSTEM},
        {"role": "user", "content": prompt},
    ]
    try:
        raw = okt_client.llm_chat(messages)
    except Exception as e:
        return {"answer": "[LLM ERROR]", "raw": f"[LLM call failed: {e}]"}
    if not raw:
        return {"answer": "Insufficient information.", "raw": ""}
    # Extract answer.
    import re
    m = re.search(r'the answer to the question is\s+"?(.+?)"?\s*(?:\.|$)', raw, re.IGNORECASE)
    if m:
        answer = m.group(1).strip().strip('"')
    else:
        answer = raw.strip().split("\n")[0].strip()
    return {"answer": answer, "raw": raw}


def synthesize_answer_with_sources(question: str, facts: list[dict]) -> dict:
    """Synthesize an answer from facts enriched with source metadata.

    Mirrors the original benchmark's synthesis: each fact includes its source
    title and published_at, giving the synthesis LLM the same metadata (source
    name, date) the original benchmark's ANSWER_SYSTEM prompt uses.
    """
    evidence_lines = []
    for f in facts[:30]:
        text = f.get("text", "")
        sources = f.get("sources", [])
        source_str = ""
        if sources:
            src = sources[0]
            title = src.get("title", "")
            date = src.get("published_at", "")
            if date and len(str(date)) >= 10:
                date = str(date)[:10]
            source_str = f" (source: {title}, {date})" if title else f" (source: {date})" if date else ""
        evidence_lines.append(f"- {text}{source_str}")
    evidence_text = "\n".join(evidence_lines) if evidence_lines else "(no evidence retrieved)"
    prompt = f"Question: {question}\n\nEvidence:\n{evidence_text}\n\nAnswer:"
    messages = [
        {"role": "system", "content": ANSWER_SYSTEM},
        {"role": "user", "content": prompt},
    ]
    try:
        raw = okt_client.llm_chat(messages)
    except Exception as e:
        return {"answer": "[LLM ERROR]", "raw": f"[LLM call failed: {e}]"}
    if not raw:
        return {"answer": "Insufficient information.", "raw": ""}
    import re
    m = re.search(r'the answer to the question is\s+"?(.+?)"?\s*(?:\.|$)', raw, re.IGNORECASE)
    if m:
        answer = m.group(1).strip().strip('"')
    else:
        answer = raw.strip().split("\n")[0].strip()
    return {"answer": answer, "raw": raw}


# --- Condition (a): Triplet-KG retrieval ----------------------------------

def run_triplet_kg(question: str, question_type: str, triplet_index: dict) -> dict:
    """Retrieve triples matching the question, synthesize answer."""
    queries = llm_json_list(question, TRIPLET_QUERY_SYSTEM)
    all_triples: list[dict] = []
    seen_facts: set[str] = set()
    for q in queries:
        q_lower = q.lower()
        for triple in triplet_index.get("all", []):
            if q_lower in triple.get("subject", "").lower() or q_lower in triple.get("object", "").lower() or q_lower in triple.get("relation", "").lower():
                fid = triple.get("fact_id", "")
                if fid not in seen_facts:
                    all_triples.append(triple)
                    seen_facts.add(fid)
            if len(all_triples) >= 30:
                break
        if len(all_triples) >= 30:
            break
    result = synthesize_answer(question, all_triples, evidence_type="triple")
    return {
        "answer": result["answer"],
        "n_triples_retrieved": len(all_triples),
        "queries": queries,
    }


# --- Condition (b): OKT concept-graph walk --------------------------------

def run_concept_walk(question: str, question_type: str) -> dict:
    """Walk the concept graph: search concepts -> rank -> facts (with fallback).

    Mirrors the original benchmark's concept variant (run_benchmark.py
    CONCEPT_VARIANT) which scored 39.5%:
      1. Extract concept-name phrases (noun phrases for ILIKE concept search)
      2. Extract fact-tsv queries (keyword-rich websearch_to_tsquery strings)
      3. Search concepts with limit=50 per phrase, merge ALL candidates
      4. Rank all candidates by fact_count, select top 5
      5. For each concept, try each fact-query; FALL BACK to empty query
         (unfiltered newest facts) if none match — concept always contributes
      6. Enrich facts with sources via get_fact (source titles/dates in synthesis)
      7. Synthesize answer with source metadata
    """
    concept_queries = llm_json_list(question, CONCEPT_QUERY_SYSTEM)
    fact_queries = llm_json_list(question, FACT_QUERY_SYSTEM)

    # 3. Search concepts with limit=50 per phrase, merge ALL candidates.
    all_concept_groups: list[dict] = []
    for cq in concept_queries:
        try:
            concepts = okt_client.search_concepts(cq, limit=50)
        except Exception:
            concepts = []
        all_concept_groups.extend(concepts)

    # 4. Rank by fact_count (total_fact_count), select top 5.
    ranked = sorted(all_concept_groups, key=lambda g: g.get("total_fact_count", 0), reverse=True)
    top_concepts = ranked[:5]

    # 5. For each concept, try each fact-query; fall back to empty query.
    all_facts: list[dict] = []
    seen_fact_ids: set[str] = set()
    concept_ids_used: list[str] = []
    for concept in top_concepts:
        cid = okt_client._concept_id(concept)
        if not cid:
            continue
        concept_ids_used.append(cid)
        # Try each fact-query; take the first that returns hits.
        found = False
        for fq in fact_queries:
            try:
                facts = okt_client.get_concept_facts(cid, query=fq, limit=10)
            except Exception:
                facts = []
            if facts:
                for fact in facts:
                    fid = fact.get("id", "")
                    if fid not in seen_fact_ids:
                        all_facts.append(fact)
                        seen_fact_ids.add(fid)
                found = True
                break
        # Fallback: unfiltered newest facts for the concept.
        if not found:
            try:
                facts = okt_client.get_concept_facts(cid, query="", limit=10)
            except Exception:
                facts = []
            if facts:
                for fact in facts:
                    fid = fact.get("id", "")
                    if fid not in seen_fact_ids:
                        all_facts.append(fact)
                        seen_fact_ids.add(fid)
        if len(all_facts) >= 30:
            break

    # 6. Enrich facts with sources via get_fact (source titles/dates).
    enriched: list[dict] = []
    for fact in all_facts[:30]:
        fid = fact.get("id", "")
        if not fid:
            enriched.append(fact)
            continue
        try:
            detail = okt_client.get_fact(fid)
            sources = detail.get("sources", [])
            fact["sources"] = [{"title": s.get("parsed_title", ""), "published_at": s.get("published_at")} for s in sources[:3]]
        except Exception:
            fact["sources"] = []
        enriched.append(fact)

    # 7. Synthesize with source metadata.
    result = synthesize_answer_with_sources(question, enriched)
    return {
        "answer": result["answer"],
        "n_facts_retrieved": len(enriched),
        "n_concepts_visited": len(set(concept_ids_used)),
        "queries": concept_queries,
        "fact_queries": fact_queries,
    }


# --- Condition (c): OKT facts-direct (baseline) ----------------------------

def run_facts_direct(question: str, question_type: str) -> dict:
    """Direct fact search — the existing baseline."""
    queries = llm_json_list(question, FACT_QUERY_SYSTEM)
    all_facts: list[dict] = []
    seen_fact_ids: set[str] = set()
    for q in queries:
        try:
            facts = okt_client.search_facts(q, limit=10, sort="source_count")
        except Exception:
            facts = []
        if not facts:
            continue
        for fact in facts:
            fid = fact.get("id", "")
            if fid not in seen_fact_ids:
                all_facts.append(fact)
                seen_fact_ids.add(fid)
            if len(all_facts) >= 20:
                break
        if len(all_facts) >= 20:
            break
    result = synthesize_answer(question, all_facts, evidence_type="fact")
    return {
        "answer": result["answer"],
        "n_facts_retrieved": len(all_facts),
        "queries": queries,
    }


# --- Condition (d): OKT concept definitions (synthesis) -------------------

def run_concept_definitions(question: str, question_type: str) -> dict:
    """Retrieve concept definitions (LLM-generated syntheses), not raw facts.

    This is a different retrieval path from concept_walk: instead of pulling
    the full fact list linked to each concept, it pulls the compressed
    synthesis/definition the synthesize_concept worker produced. The
    synthesis is a short summary — less detail than facts but more processed.
    """
    queries = llm_json_list(question, CONCEPT_QUERY_SYSTEM)
    all_definitions: list[str] = []
    concept_ids_used: list[str] = []
    for q in queries:
        concepts = okt_client.search_concepts(q, limit=5)
        for concept in concepts[:3]:
            cid = okt_client._concept_id(concept)
            if not cid:
                continue
            concept_ids_used.append(cid)
            defn = okt_client.get_concept_definition(cid)
            if defn and "synthesis" in defn:
                content = defn["synthesis"].get("content", "")
                if content:
                    all_definitions.append(content)
            if len(all_definitions) >= 10:
                break
        if len(all_definitions) >= 10:
            break
    # Synthesize from definitions.
    evidence = [{"text": d} for d in all_definitions]
    result = synthesize_answer(question, evidence, evidence_type="fact")
    return {
        "answer": result["answer"],
        "n_definitions_retrieved": len(all_definitions),
        "n_concepts_visited": len(set(concept_ids_used)),
        "queries": queries,
    }


VARIANT_RUNNERS = {
    "triplet_kg": run_triplet_kg,
    "concept_walk": run_concept_walk,
    "concept_definitions": run_concept_definitions,
    "facts_direct": run_facts_direct,
}


def run_question(q: dict, variant: str, triplet_index: dict | None) -> dict:
    """Run one variant for one question. Returns a prediction row."""
    qid = q["id"]
    question = q["query"]
    qtype = q.get("question_type", "unknown")
    gold = q.get("gold_answer", "")
    runner = VARIANT_RUNNERS[variant]
    if variant == "triplet_kg":
        result = runner(question, qtype, triplet_index or {})
    else:
        result = runner(question, qtype)
    return {
        "id": qid,
        "variant": variant,
        "query": question,
        "question_type": qtype,
        "gold": gold,
        "prediction": result["answer"],
        "n_evidence_retrieved": result.get("n_facts_retrieved", result.get("n_triples_retrieved", 0)),
        "queries": result.get("queries", []),
    }


def main() -> None:
    parser = argparse.ArgumentParser(description="KGQA head-to-head benchmark")
    parser.add_argument("--sample", type=int, default=0, help="Random sample N questions (0 = all)")
    parser.add_argument("--variant", choices=list(VARIANT_RUNNERS.keys()), default=None, help="Run only this variant")
    parser.add_argument("--concurrency", type=int, default=10, help="Parallel questions")
    args = parser.parse_args()

    queries = load_queries()
    if args.sample > 0 and args.sample < len(queries):
        rng = random.Random(42)
        queries = rng.sample(queries, args.sample)
    print(f"Loaded {len(queries)} questions")

    # Load triplet index for triplet_kg variant.
    triplet_index = None
    variants_to_run = [args.variant] if args.variant else list(VARIANT_RUNNERS.keys())
    if "triplet_kg" in variants_to_run:
        print("Loading triplet index...")
        triplets_path = RESULTS_DIR / "triplet_kg_all.json"
        if triplets_path.exists():
            triplets = json.loads(triplets_path.read_text())
            triplet_index = {"all": triplets}
            print(f"  {len(triplets)} triples loaded")
        else:
            print("  WARNING: triplet_kg_all.json not found. Run triplet_kg.py first.")
            print("  Skipping triplet_kg variant.")
            variants_to_run = [v for v in variants_to_run if v != "triplet_kg"]

    for variant in variants_to_run:
        print(f"\n{'='*60}")
        print(f"Variant: {variant}")
        print(f"{'='*60}")
        existing = load_predictions(variant)
        to_run = [q for q in queries if q["id"] not in existing]
        print(f"  {len(existing)} already done, {len(to_run)} to run")
        if not to_run:
            print("  (all done)")
            continue

        t0 = time.time()
        with ThreadPoolExecutor(max_workers=args.concurrency) as pool:
            futures = {
                pool.submit(run_question, q, variant, triplet_index): q
                for q in to_run
            }
            done = 0
            for future in as_completed(futures):
                q = futures[future]
                try:
                    row = future.result()
                    save_prediction(variant, row)
                    done += 1
                    if done % 50 == 0:
                        elapsed = time.time() - t0
                        rate = done / elapsed
                        eta = (len(to_run) - done) / rate if rate > 0 else 0
                        print(f"  {done}/{len(to_run)} ({rate:.1f}/s, ETA {eta:.0f}s)")
                except Exception as e:
                    print(f"  ERROR on {q['id']}: {e}")
        print(f"  {done} done in {time.time()-t0:.0f}s")

    print("\nDone. Run score_kgqa.py to score.")


if __name__ == "__main__":
    main()