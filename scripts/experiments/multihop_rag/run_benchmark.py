"""Run two retrieval variants against MultiHop-RAG queries and score them.

Per question, two independent retrieval paths are run and each writes its
own prediction row:

  CONCEPT_VARIANT  (concept -> concept facts)
    1. extract_concept_queries(question)  -> 1-5 noun phrases (1 LLM call)
    2. pad phrases to NUM_CONCEPT_QUERIES -> 5 concept-search queries
    3. for each phrase: search_concepts(phrase)  -> merge all concept groups
    4. select top-N concepts by fact_count      -> TOP_N_CONCEPTS (default 5)
    5. for each selected concept: get_concept_facts(concept_id, q=phrase,
                                                   limit=FACTS_PER_CONCEPT)
    6. dedup facts across concepts by fact_id
    7. for each unique fact: get_fact(fact_id) -> source urls + linked concepts
    8. synthesize_answer(question, facts) -> 1 LLM call, short answer

  FACTS_VARIANT  (direct fact search)
    1. extract_fact_queries(question)  -> 1-5 keyword-rich tsvector queries
                                          (1 LLM call)
    2. for each query: search_facts(q=query, sort=source_count,
                                    limit=FACTS_PER_QUERY)
    3. dedup facts across queries by fact_id
    4. for each unique fact: get_fact(fact_id) -> source urls + linked concepts
    5. synthesize_answer(question, facts) -> 1 LLM call, short answer

Both variants use the SAME answer-synthesis prompt and LLM backend; only
the retrieval path differs. Each variant writes its own predictions file
(results/predictions_concept.jsonl, results/predictions_facts.jsonl) so
score.py can compare them side-by-side.

Resumable: skips ids already present in each variant's predictions file.

CLI:
  python3 run_benchmark.py                         # both variants, all queries
  python3 run_benchmark.py --sample 50             # 50 random questions
  python3 run_benchmark.py --ids 3,5,42            # specific ids
  python3 run_benchmark.py --question-type comparison_query --limit 20
  python3 run_benchmark.py --variant concept       # only the concept variant
  python3 run_benchmark.py --variant facts          # only the facts variant
  python3 run_benchmark.py --concurrency 10         # parallel questions
"""
from __future__ import annotations

import argparse
import json
import os
import random
import sys
import time
from concurrent.futures import ThreadPoolExecutor, as_completed
from typing import Any

from tqdm import tqdm

import config
import llm
import okt


# ---------------------------------------------------------------------------
# I/O helpers
# # ---------------------------------------------------------------------------


def _load_queries() -> list[dict]:
    if not os.path.exists(config.QUERIES_PATH):
        print(
            f"  queries file not found: {config.QUERIES_PATH}\n"
            "  run `python3 download_dataset.py` first.",
            file=sys.stderr,
        )
        sys.exit(2)
    out: list[dict] = []
    with open(config.QUERIES_PATH, "r", encoding="utf-8") as fh:
        for line in fh:
            try:
                out.append(json.loads(line))
            except Exception:  # noqa: BLE001
                pass
    return out


def _load_done_ids(predictions_path: str) -> set[str]:
    done: set[str] = set()
    if not os.path.exists(predictions_path):
        return done
    with open(predictions_path, "r", encoding="utf-8") as fh:
        for line in fh:
            try:
                done.add(json.loads(line)["id"])
            except Exception:  # noqa: BLE001
                pass
    return done


def _append_prediction(pred: dict, path: str) -> None:
    os.makedirs(os.path.dirname(path), exist_ok=True)
    with open(path, "a", encoding="utf-8") as fh:
        fh.write(json.dumps(pred, ensure_ascii=False) + "\n")
        fh.flush()


def _write_answer_audit(
    qid: str, question: str, facts: list[dict], synthesis: dict,
    variant: str,
) -> None:
    sub_map = {"concept": "answers_concept", "facts": "answers_facts",
               "direct": "answers_direct"}
    sub = sub_map.get(variant, f"answers_{variant}")
    out_dir = os.path.join(os.path.dirname(__file__), sub)
    os.makedirs(out_dir, exist_ok=True)
    path = os.path.join(out_dir, f"{qid}.md")
    lines = [f"# Question {qid} [{variant}]", "", question, ""]
    lines.append(f"## Retrieved facts ({len(facts)})")
    for i, f in enumerate(facts, 1):
        lines.append(f"\n### Fact {i}: {f['id']}")
        lines.append(f["text"].strip())
        for s in f.get("sources", []):
            site = (s.get("parsed_sitename") or "").strip()
            title = (s.get("parsed_title") or "").strip()
            author = (s.get("parsed_author") or "").strip()
            pub = s.get("published_at") or ""
            bits = []
            if site:
                bits.append(site)
            if title and title != site:
                bits.append(f'"{title}"')
            if author:
                bits.append(f"by {author}")
            if pub:
                bits.append(f"on {pub}")
            if bits:
                lines.append(f"  - source: {' '.join(bits)}")
            else:
                lines.append(f"  - source: {s.get('url') or '(untitled)'}")
    lines.append("\n## Synthesis (raw LLM output)")
    lines.append(synthesis.get("raw", ""))
    lines.append("")
    with open(path, "w", encoding="utf-8") as fh:
        fh.write("\n".join(lines))


# ---------------------------------------------------------------------------
# Shared enrichment + synthesis
# # ---------------------------------------------------------------------------


def _enrich_facts(fact_summaries: list[dict]) -> tuple[list[dict], list[str]]:
    """For each fact, call get_fact to attach sources + concepts.

    Returns (enriched_facts, source_ids). enriched_facts[i] has the
    shape expected by prompts.answer_user: {id, text, sources, concepts}.
    source_ids is the flat list of all source ids mentioned.
    """
    enriched: list[dict] = []
    source_ids: list[str] = []
    for f in fact_summaries:
        fid = f.get("id")
        if not fid:
            continue
        try:
            detail = okt.get_fact(fid)
        except okt.OKTError as e:
            print(f"    fact {fid}: get_fact failed: {e}")
            enriched.append({**f, "sources": [], "concepts": []})
            continue
        sources = detail.get("sources") or []
        concepts = detail.get("concepts") or []
        enriched.append(
            {
                "id": fid,
                "text": f.get("text") or detail.get("fact", {}).get("text", ""),
                "sources": sources,
                "concepts": concepts,
            }
        )
        for s in sources:
            sid = s.get("source_id") or s.get("id")
            if sid:
                source_ids.append(str(sid))
    return enriched, source_ids


# ---------------------------------------------------------------------------
# CONCEPT_VARIANT: concept search -> concept facts
# # ---------------------------------------------------------------------------


def _pad_phrases(phrases: list[str], n: int) -> list[str]:
    if not phrases:
        return []
    if len(phrases) >= n:
        return phrases[:n]
    out = list(phrases)
    i = 0
    while len(out) < n:
        out.append(phrases[i % len(phrases)])
        i += 1
    return out


def _concept_group_id(group: dict) -> str | None:
    ctxs = group.get("contexts") or []
    if ctxs:
        cid = ctxs[0].get("concept_id") or ctxs[0].get("id")
        if cid:
            return str(cid)
    return group.get("concept_id") or group.get("id")


def _concept_fact_count(group: dict) -> int:
    return int(group.get("fact_count") or group.get("total_fact_count") or 0)


def _concept_name(group: dict) -> str:
    return group.get("canonical_name") or group.get("name") or ""


def _rank_and_select(groups: list[dict], top_n: int) -> list[dict]:
    """Dedup by canonical_name (lowercase), rank by fact_count desc, take top_n."""
    seen: dict[str, dict] = {}
    for g in groups:
        name = _concept_name(g).lower()
        if not name:
            continue
        if name not in seen or _concept_fact_count(g) > _concept_fact_count(seen[name]):
            seen[name] = g
    ranked = sorted(seen.values(), key=_concept_fact_count, reverse=True)
    return ranked[:top_n]


def _gather_facts_for_concepts(
    concept_ids: list[str],
    fact_queries: list[str],
    facts_per_concept: int,
) -> tuple[list[dict], list[dict]]:
    """For each concept_id, fetch facts linked to that concept.

    For each concept, we try, in order, until we get a non-empty page:

      1. Each fact-query string (keyword-rich tsvector queries tuned for
         facts.search_tsv). These are NOT the concept-name phrases —
         they are separate, longer, fact-text-oriented queries produced
         by extract_fact_queries. We try each one and take the first
         that returns hits, since websearch_to_tsquery requires every
         token to match and tighter queries return fewer/no rows.
      2. Empty query (newest facts for the concept, unfiltered) as the
         fallback so the concept always contributes its evidence.

    Dedup by fact_id across concepts.

    Returns (deduped_fact_summaries, per_concept_hits) where
    per_concept_hits records which query matched for each concept
    (for the audit trail).
    """
    out: list[dict] = []
    seen_ids: set[str] = set()
    per_concept_hits: list[dict] = []

    for cid in concept_ids:
        page = None
        matched_query = ""
        for q in fact_queries:
            try:
                page = okt.get_concept_facts(
                    cid, query=q, limit=facts_per_concept, offset=0
                )
            except okt.OKTError as e:
                print(f"    concept {cid}: get_concept_facts(q={q!r}) failed: {e}")
                page = None
                continue
            if page.get("data"):
                matched_query = q
                break
        if page is None or not page.get("data"):
            try:
                page = okt.get_concept_facts(
                    cid, query="", limit=facts_per_concept, offset=0
                )
                matched_query = "(fallback: no filter)"
            except okt.OKTError as e:
                print(f"    concept {cid}: get_concept_facts(q='') failed: {e}")
                per_concept_hits.append(
                    {"concept_id": cid, "matched_query": None, "count": 0}
                )
                continue
        data = page.get("data") or []
        per_concept_hits.append(
            {"concept_id": cid, "matched_query": matched_query, "count": len(data)}
        )
        for f in data:
            fid = f.get("id")
            if not fid or fid in seen_ids:
                continue
            seen_ids.add(fid)
            out.append(f)
    return out, per_concept_hits


def run_concept_variant(
    q: dict,
    top_n: int,
    facts_per_concept: int,
    num_concept_queries: int,
    predictions_path: str,
) -> dict:
    """Run the concept -> concept facts retrieval variant for one question."""
    qid = q["id"]
    question = q["query"]
    started = time.time()
    total_usage = {"prompt": 0, "completion": 0}

    # 1-2. Extract concept-name phrases + pad to NUM_CONCEPT_QUERIES.
    # These are tuned for the ILIKE concept-name lookup (short entity
    # substrings), NOT for fact-tsv search.
    extracted_phrases, q_usage = llm.extract_concept_queries(question)
    total_usage = llm._add_usage(total_usage, q_usage)
    phrases = _pad_phrases(extracted_phrases, num_concept_queries)

    # 1b. Separately extract fact-tsv queries. These are longer,
    # keyword-rich, and tuned for websearch_to_tsquery against
    # facts.search_tsv. The concept variant uses them ONLY as the `q`
    # filter when fetching facts per concept — not for concept search.
    extracted_fact_queries, fq_usage = llm.extract_fact_queries(question)
    total_usage = llm._add_usage(total_usage, fq_usage)

    concept_ids_used: list[str] = []
    concept_search_hits: list[dict] = []
    concept_fact_hits: list[dict] = []
    if phrases:
        # 3. Search concepts for each concept-name phrase.
        groups: list[dict] = []
        for p in phrases:
            try:
                page = okt.search_concepts(p, limit=50, offset=0)
            except okt.OKTError as e:
                print(f"  {qid}: search_concepts({p!r}) failed: {e}")
                concept_search_hits.append(
                    {"query": p, "count": 0, "top_concepts": [], "error": str(e)}
                )
                continue
            page_groups = page.get("data") or []
            for g in page_groups:
                cid = _concept_group_id(g)
                if cid:
                    groups.append(g)
                    concept_ids_used.append(str(cid))
            top_names = [g.get("canonical_name", "") for g in page_groups[:3]]
            concept_search_hits.append(
                {"query": p, "count": len(page_groups), "top_concepts": top_names}
            )

        # 4. Rank + select top N.
        selected = _rank_and_select(groups, top_n)
        selected_ids = [c for c in map(_concept_group_id, selected) if c]

        # 5-6. Gather facts per concept (deduped). Use the dedicated
        # fact-tsv queries (not the concept-name phrases) as the `q`
        # filter; fall back to newest if none match.
        fact_summaries, concept_fact_hits = _gather_facts_for_concepts(
            selected_ids, extracted_fact_queries, facts_per_concept
        )

        # 7. Enrich facts with sources + concepts.
        enriched, source_ids_used = _enrich_facts(fact_summaries)
        fact_ids_used = [f["id"] for f in enriched]
    else:
        enriched = []
        fact_ids_used = []
        source_ids_used = []

    # 8. Synthesize answer.
    synthesis = llm.synthesize_answer(question, enriched)
    total_usage = llm._add_usage(total_usage, synthesis["usage"])

    pred = {
        "id": qid,
        "variant": "concept",
        "query": question,
        "question_type": q.get("question_type", ""),
        "gold": q.get("gold_answer", ""),
        "prediction": synthesis["answer"],
        "fact_ids_used": fact_ids_used,
        "source_ids_used": source_ids_used,
        "concept_ids_used": concept_ids_used[:top_n],
        "extracted_phrases": extracted_phrases,
        "extracted_fact_queries": extracted_fact_queries,
        "concept_queries": phrases,
        "concept_search_hits": concept_search_hits,
        "concept_fact_hits": concept_fact_hits,
        "latency_ms": int((time.time() - started) * 1000),
        "tokens": total_usage,
        "llm_calls": {
            "concept_query_extraction": q_usage,
            "fact_query_extraction": fq_usage,
            "synthesis": synthesis["usage"],
        },
        "params": {
            "top_n": top_n,
            "facts_per_concept": facts_per_concept,
            "num_concept_queries": num_concept_queries,
        },
    }
    _append_prediction(pred, predictions_path)
    _write_answer_audit(qid, question, enriched, synthesis, "concept")
    return pred


# ---------------------------------------------------------------------------
# FACTS_VARIANT: direct fact search with LLM-extracted queries
# # ---------------------------------------------------------------------------


def _gather_facts_for_queries(
    queries: list[str],
    facts_per_query: int,
) -> tuple[list[dict], list[dict]]:
    """Run each tsvector query against the repo-wide /facts endpoint.

    Returns (deduped_fact_summaries, per_query_hits) where per_query_hits
    records the count and top fact ids returned for each query (for the
    audit trail).
    """
    out: list[dict] = []
    seen_ids: set[str] = set()
    per_query_hits: list[dict] = []

    for q in queries:
        try:
            page = okt.search_facts(
                q, limit=facts_per_query, offset=0, sort="source_count"
            )
        except okt.OKTError as e:
            print(f"    search_facts(q={q!r}) failed: {e}")
            per_query_hits.append(
                {"query": q, "count": 0, "top_facts": [], "error": str(e)}
            )
            continue
        data = page.get("data") or []
        per_query_hits.append(
            {
                "query": q,
                "count": len(data),
                "top_facts": [
                    (f.get("id"), (f.get("text") or "")[:80]) for f in data[:3]
                ],
            }
        )
        for f in data:
            fid = f.get("id")
            if not fid or fid in seen_ids:
                continue
            seen_ids.add(fid)
            out.append(f)
    return out, per_query_hits


def run_facts_variant(
    q: dict,
    facts_per_query: int,
    predictions_path: str,
) -> dict:
    """Run the direct fact-search retrieval variant for one question.

    Uses 1 LLM call to extract 1-5 keyword-rich tsvector queries from
    the question, then searches the repo-wide /facts endpoint with
    each query (sorted by source_count), dedups, enriches, and
    synthesizes.
    """
    qid = q["id"]
    question = q["query"]
    started = time.time()
    total_usage = {"prompt": 0, "completion": 0}

    # 1. Extract tsvector-oriented fact queries.
    extracted_queries, q_usage = llm.extract_fact_queries(question)
    total_usage = llm._add_usage(total_usage, q_usage)

    fact_search_hits: list[dict] = []
    if extracted_queries:
        # 2-3. Run each query against /facts, dedup.
        fact_summaries, fact_search_hits = _gather_facts_for_queries(
            extracted_queries, facts_per_query
        )
        # 4. Enrich.
        enriched, source_ids_used = _enrich_facts(fact_summaries)
        fact_ids_used = [f["id"] for f in enriched]
    else:
        enriched = []
        fact_ids_used = []
        source_ids_used = []

    # 5. Synthesize answer.
    synthesis = llm.synthesize_answer(question, enriched)
    total_usage = llm._add_usage(total_usage, synthesis["usage"])

    pred = {
        "id": qid,
        "variant": "facts",
        "query": question,
        "question_type": q.get("question_type", ""),
        "gold": q.get("gold_answer", ""),
        "prediction": synthesis["answer"],
        "fact_ids_used": fact_ids_used,
        "source_ids_used": source_ids_used,
        "concept_ids_used": [],
        "extracted_queries": extracted_queries,
        "fact_search_hits": fact_search_hits,
        "latency_ms": int((time.time() - started) * 1000),
        "tokens": total_usage,
        "llm_calls": {
            "fact_query_extraction": q_usage,
            "synthesis": synthesis["usage"],
        },
        "params": {
            "facts_per_query": facts_per_query,
        },
    }
    _append_prediction(pred, predictions_path)
    _write_answer_audit(qid, question, enriched, synthesis, "facts")
    return pred


# ---------------------------------------------------------------------------
# DIRECT_VARIANT: pass the full question directly as the search query
# # ---------------------------------------------------------------------------


def run_direct_variant(
    q: dict,
    facts_per_query: int,
    predictions_path: str,
) -> dict:
    """Run the no-query-extraction retrieval variant for one question.

    Skips the LLM query-extraction call entirely and passes the full
    question text directly as the `q` parameter to the repo-wide
    /facts endpoint. This is the naive baseline: websearch_to_tsquery
    requires every token in the question to appear in a fact's text,
    so most multi-hop questions (long, full of stop-words and question
    words) will return zero hits — the honest score for the naive path.

    The variant makes only 1 LLM call total (synthesis), so its token
    cost is the minimum of the three variants.
    """
    qid = q["id"]
    question = q["query"]
    started = time.time()
    total_usage = {"prompt": 0, "completion": 0}

    # 1. Use the question text directly as the search query. No LLM
    # call. websearch_to_tsquery will AND every token, so this typically
    # returns 0 facts for multi-hop questions, but it's the honest
    # baseline for "no query engineering."
    fact_search_hits: list[dict] = []
    fact_summaries, fact_search_hits = _gather_facts_for_queries(
        [question], facts_per_query
    )

    # 2. Enrich.
    if fact_summaries:
        enriched, source_ids_used = _enrich_facts(fact_summaries)
        fact_ids_used = [f["id"] for f in enriched]
    else:
        enriched = []
        fact_ids_used = []
        source_ids_used = []

    # 3. Synthesize answer.
    synthesis = llm.synthesize_answer(question, enriched)
    total_usage = llm._add_usage(total_usage, synthesis["usage"])

    pred = {
        "id": qid,
        "variant": "direct",
        "query": question,
        "question_type": q.get("question_type", ""),
        "gold": q.get("gold_answer", ""),
        "prediction": synthesis["answer"],
        "fact_ids_used": fact_ids_used,
        "source_ids_used": source_ids_used,
        "concept_ids_used": [],
        # No extraction LLM call; the only "query" is the question text.
        "search_query": question,
        "fact_search_hits": fact_search_hits,
        "latency_ms": int((time.time() - started) * 1000),
        "tokens": total_usage,
        "llm_calls": {
            "synthesis": synthesis["usage"],
        },
        "params": {
            "facts_per_query": facts_per_query,
        },
    }
    _append_prediction(pred, predictions_path)
    _write_answer_audit(qid, question, enriched, synthesis, "direct")
    return pred


# ---------------------------------------------------------------------------
# Dispatch
# # ---------------------------------------------------------------------------


def run_one(
    q: dict,
    variant: str,
    top_n: int,
    facts_per_concept: int,
    num_concept_queries: int,
    facts_per_query: int,
    predictions_path: str,
) -> dict:
    if variant == "concept":
        return run_concept_variant(
            q, top_n, facts_per_concept, num_concept_queries, predictions_path
        )
    if variant == "facts":
        return run_facts_variant(q, facts_per_query, predictions_path)
    if variant == "direct":
        return run_direct_variant(q, facts_per_query, predictions_path)
    raise ValueError(f"unknown variant: {variant}")


# ---------------------------------------------------------------------------
# CLI
# # ---------------------------------------------------------------------------


def _parse_args() -> argparse.Namespace:
    ap = argparse.ArgumentParser(description="MultiHop-RAG retrieval-variant benchmark")
    ap.add_argument("--limit", type=int, default=0,
                    help="process only the first N questions (0 = all)")
    ap.add_argument("--sample", type=int, default=0,
                    help="process N randomly sampled questions (0 = disabled)")
    ap.add_argument("--ids", type=str, default="",
                    help="comma-separated question ids to process (e.g. 3,5,42)")
    ap.add_argument("--question-type", type=str, default="",
                    help="filter by question_type (inference_query, "
                         "comparison_query, temporal_query, null_query)")
    ap.add_argument("--variant", type=str, default="all",
                    choices=["all", "concept", "facts", "direct"],
                    help="which retrieval variant(s) to run: "
                         "'all' runs concept + facts + direct; "
                         "'concept'/'facts'/'direct' run a single variant")
    ap.add_argument("--top-n", type=int, default=config.TOP_N_CONCEPTS,
                    help="top-N concepts selected (concept variant)")
    ap.add_argument("--facts-per-concept", type=int,
                    default=config.FACTS_PER_CONCEPT,
                    help="facts fetched per concept (concept variant)")
    ap.add_argument("--num-concept-queries", type=int,
                    default=config.NUM_CONCEPT_QUERIES,
                    help="number of concept-search queries (concept variant)")
    ap.add_argument("--facts-per-query", type=int,
                    default=config.FACTS_PER_QUERY,
                    help="facts fetched per fact-search query (facts variant)")
    ap.add_argument("--concurrency", type=int, default=1,
                    help="parallel questions (best-effort; OKT must handle it)")
    ap.add_argument("--seed", type=int, default=42)
    return ap.parse_args()


def _select_queries(queries: list[dict], args) -> list[dict]:
    selected = queries
    if args.question_type:
        selected = [q for q in selected if q.get("question_type") == args.question_type]
    if args.ids:
        wanted = {x.strip() for x in args.ids.split(",") if x.strip()}
        selected = [q for q in selected if q["id"] in wanted]
    if args.sample:
        rng = random.Random(args.seed)
        selected = rng.sample(selected, min(args.sample, len(selected)))
    elif args.limit:
        selected = selected[: args.limit]
    return selected


def _predictions_path_for(variant: str) -> str:
    base = os.path.join(config.RESULTS_DIR, "predictions")
    return f"{base}_{variant}.jsonl"


def main() -> int:
    args = _parse_args()
    queries = _load_queries()
    print(f"  loaded {len(queries)} queries from {config.QUERIES_PATH}")

    selected = _select_queries(queries, args)

    variants = ["concept", "facts", "direct"] if args.variant == "all" else [args.variant]

    for variant in variants:
        predictions_path = _predictions_path_for(variant)
        done = _load_done_ids(predictions_path)
        todo = [q for q in selected if q["id"] not in done]
        print(
            f"\n[{variant}] {len(todo)} to run "
            f"({len(selected) - len(todo)} already done in {predictions_path})"
        )
        if not todo:
            continue

        if args.concurrency > 1:
            with ThreadPoolExecutor(max_workers=args.concurrency) as ex:
                futures = {
                    ex.submit(
                        run_one, q, variant, args.top_n,
                        args.facts_per_concept, args.num_concept_queries,
                        args.facts_per_query, predictions_path,
                    ): q
                    for q in todo
                }
                for fut in tqdm(
                    as_completed(futures), total=len(futures), desc=variant
                ):
                    try:
                        fut.result()
                    except Exception as e:  # noqa: BLE001
                        qid = futures[fut]["id"]
                        print(f"  {qid}: pipeline error: {e}", file=sys.stderr)
        else:
            for q in tqdm(todo, desc=variant):
                try:
                    run_one(
                        q, variant, args.top_n, args.facts_per_concept,
                        args.num_concept_queries, args.facts_per_query,
                        predictions_path,
                    )
                except Exception as e:  # noqa: BLE001
                    print(f"  {q['id']}: pipeline error: {e}", file=sys.stderr)

        print(f"  Predictions: {predictions_path}")

    print("\nNext: python3 score.py")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())