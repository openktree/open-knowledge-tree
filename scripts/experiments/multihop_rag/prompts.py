"""Static prompts for the LLM calls in the MultiHop-RAG benchmark.

Two retrieval variants share this module. Each variant has its own
query-generation prompt tuned for its retrieval backend, and both
variants share the answer-synthesis prompt.

  1. CONCEPT_VARIANT  — concept search -> concept facts
     - CONCEPT_QUERY_SYSTEM: produces 1-5 short noun phrases used as
       substring (ILIKE) queries against canonical concept names.
     - The same phrases are reused as the tsvector `q` filter when
       fetching facts per concept.

  2. FACTS_VARIANT    — direct fact search
     - FACT_QUERY_SYSTEM: produces 1-5 websearch_to_tsquery strings
       tuned for the facts.search_tsv index. These are NOT noun
       phrases; they are keyword-rich query fragments the fact text
       is likely to contain.

The answer-synthesis prompt (ANSWER_SYSTEM) is shared by both
variants: the LLM sees the question + gathered facts + source
metadata and emits a short answer in the fixed contract format.
"""
from __future__ import annotations


# --- Concept variant: phrase extraction ----------------────────-------------

CONCEPT_QUERY_SYSTEM = (
    "You build search queries for a concept-tagged knowledge base.\n"
    "The knowledge base has an index of CONCEPTS (people, organisations, "
    "products, events, topics). Concept search is a LEXICAL SUBSTRING "
    "match on the concept's canonical name (case-insensitive ILIKE), so "
    "your query must be a likely substring of a concept name in the KB.\n"
    "Output ONLY a JSON array of 1-5 short noun phrases (lowercase, no "
    "punctuation, no articles). Each phrase should be a single concept "
    "name or a tight entity reference (e.g. "
    "['sam bankman-fried', 'ftx', 'fraud trial', 'wire fraud', "
    "'conspiracy charges']).\n"
    "Rules:\n"
    "1. Prefer concrete entities named or strongly implied by the "
    "question (people, companies, products, publications).\n"
    "2. Add the key topic words the question turns on (e.g. 'charges', "
    "'valuation', 'acquisition', 'launch').\n"
    "3. Do NOT include question words, articles, conjunctions, or the "
    "full question text.\n"
    "4. Do NOT include dates or numbers as standalone phrases — fold "
    "them into entity phrases if needed (e.g. 'ftx 2022 collapse').\n"
    "5. Aim for diversity: cover the distinct entities/topics the "
    "multi-hop question touches, not 5 paraphrases of the same one.\n"
    "Respond with the JSON array only — no prose, no markdown fences."
)


def concept_query_user(question: str) -> str:
    return f"Question: {question}\n\nConcept search phrases:"


# Back-compat aliases for the old single-variant code path.
PHRASE_EXTRACTION_SYSTEM = CONCEPT_QUERY_SYSTEM
phrase_extraction_user = concept_query_user


# --- Facts variant: fact-query extraction ----------------------------------
# Three query modes, selectable via --query-mode:
#   multi   (default): 1-5 short 3-6 term tsvector queries (original behavior)
#   single:  one comprehensive query containing ALL key terms from the question
#   top3:    up to 3 short queries, then trim results to max_facts

FACT_QUERY_SYSTEM = (
    "You build search queries for a full-text fact index.\n"
    "The index is a PostgreSQL tsvector built from self-contained "
    "atomic fact sentences (e.g. 'Sam Bankman-Fried, the founder of "
    "FTX, is facing seven counts of criminal charges: two counts of "
    "wire fraud and five counts of conspiracy charges.'). The search "
    "operator is websearch_to_tsquery, which means EVERY term in your "
    "query must appear in the fact's text for it to match.\n"
    "Output ONLY a JSON array of 1-5 query strings. Each query is a "
    "short, keyword-rich fragment designed to match the fact(s) that "
    "answer the question. Rules:\n"
    "1. Each query MUST be 3-6 terms long. One-word queries are too "
    "broad (they return dozens of loosely-related facts); long "
    "clauses almost never match because websearch_to_tsquery requires "
    "every token to be present. The sweet spot is a tight phrase of "
    "the most discriminating 3-6 terms that would co-occur in a "
    "single fact sentence.\n"
    "2. Use the most specific nouns and numbers from the question. "
    "Numbers, quantities, and proper nouns are the strongest "
    "discriminators (e.g. 'seven counts wire fraud conspiracy', "
    "'$440 million blackstone hipgnosis bid', "
    "'apple watch series 9 double tap').\n"
    "3. Drop stop-words, question words, and generic verbs. The index "
    "stems tokens, so prefer the base form ('charge' not 'charges' is "
    "fine; both stem to the same lexeme).\n"
    "4. Produce DIVERSE queries — each should target a different "
    "sub-fact the multi-hop answer needs. For a comparison question "
    "about two articles, emit one query per article's distinctive "
    "claim plus a query for the shared subject.\n"
    "5. For comparison/temporal questions that reference publication "
    "sources (e.g. 'the TechCrunch article', 'the Bloomberg piece'), "
    "include the publication name as a term in at least one query — "
    "fact text often mentions the source.\n"
    "6. For null/abstention questions, still emit the best-guess "
    "queries — if the facts aren't in the KB, the search will return "
    "nothing and the synthesizer will correctly abstain.\n"
    "7. Do NOT copy phrases from the question verbatim — the question "
    "contains many non-fact tokens (question words, articles, "
    "conjunctions). Distill to the keyword-rich core.\n"
    "Respond with the JSON array only — no prose, no markdown fences."
)

# Single comprehensive query: one query that captures ALL key entities,
# numbers, and topic terms from the question. This covers the full
# semantic space of the question in one shot, rather than splitting
# across multiple narrow queries. The risk: websearch_to_tsquery ANDs
# every token, so a 10-term query requires all 10 to appear in one fact
# — which is unlikely for multi-hop facts. Use this to test whether
# breadth (one wide query) beats diversity (many narrow queries).
FACT_QUERY_SINGLE_SYSTEM = (
    "You build ONE search query for a full-text fact index.\n"
    "The index is a PostgreSQL tsvector of atomic fact sentences. "
    "The search operator is websearch_to_tsquery — EVERY term in "
    "your query must appear in a fact's text for it to match.\n"
    "Output ONLY a JSON array with ONE string: a single query "
    "containing ALL the key entities, numbers, publication names, "
    "and topic terms from the question. Rules:\n"
    "1. Include every proper noun, number, publication name, and "
    "discriminating noun from the question.\n"
    "2. Drop stop-words, question words, articles, conjunctions, "
    "and generic verbs.\n"
    "3. Aim for 5-15 terms — enough to cover the full semantic space "
    "of the question, but not so many that no single fact contains "
    "them all. Prefer specific nouns and numbers.\n"
    "4. For comparison questions mentioning two publications, include "
    "both publication names and the shared subject.\n"
    "5. For temporal questions, include the dates/time references and "
    "the subject entities.\n"
    "Respond with a JSON array of one string only — no prose."
)


def fact_query_user(question: str) -> str:
    return f"Question: {question}\n\nFact search queries:"


# --- Shared answer-synthesis prompt ---------------------------------------

ANSWER_SYSTEM = (
    "You answer multi-hop questions using retrieved evidence.\n"
    "Evidence format: each fact has text + source metadata (publication, "
    "title, author, date). Questions may reference articles by publication "
    "name (e.g. 'the TechCrunch article') — filter to facts whose Source "
    "line names that publication.\n"
    "Rules:\n"
    "1. Use ONLY the provided facts.\n"
    "2. Deduce the answer by combining facts across sources (multi-hop) "
    "when the evidence supports it. Commit to your best answer — do not "
    "abstain from uncertainty alone.\n"
    "3. Abstain ONLY when the evidence is absent or empty.\n"
    "4. Keep the answer short: a name, organization, 'Yes', 'no', or short phrase.\n"
    '5. End with exactly: The answer to the question is "<answer>"\n'
    '   For abstention: The answer to the question is "Insufficient information."'
)


def _render_evidence_lines(facts: list[dict]) -> list[str]:
    """Render the per-fact evidence block (the lines answer_user emits
    for the facts, excluding the question + boilerplate). Factored out
    so the runners can count the evidence-token budget the synthesis
    LLM receives — the 'context size' each RAG variant gives the model.
    """
    lines: list[str] = []
    if not facts:
        lines.append("(no facts were retrieved for this question)")
        return lines
    for i, f in enumerate(facts, 1):
        lines.append(f"--- Fact {i} (id: {f['id']}) ---")
        lines.append(f"Text: {f.get('text', '').strip()}")
        srcs = f.get("sources", [])
        if srcs:
            for s in srcs:
                site = (s.get("parsed_sitename") or "").strip()
                title = (s.get("parsed_title") or "").strip()
                author = (s.get("parsed_author") or "").strip()
                pub = s.get("published_at") or ""
                if pub and "T" in str(pub):
                    pub = str(pub).split("T")[0]
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
                    lines.append(f"Source: {' '.join(bits)}")
                elif s.get("url"):
                    lines.append(f"Source: {s['url']}")
                else:
                    lines.append("Source: (untitled)")
        if f.get("concepts"):
            names = ", ".join(
                c.get("canonical_name", "") for c in f["concepts"] if c.get("canonical_name")
            )
            if names:
                lines.append(f"Concepts: {names}")
    return lines


def evidence_tokens(facts: list[dict]) -> int:
    """Whitespace-token count of the retrieved-evidence block passed to
    the synthesis LLM (the facts + their source/concept metadata lines,
    as rendered by answer_user). Excludes the question and the fixed
    prompt boilerplate, so it isolates the RAG context budget — the
    'how much text the retriever gave the model to read' metric.
    """
    lines = _render_evidence_lines(facts)
    return len(" ".join(lines).split())


def answer_user(question: str, facts: list[dict]) -> str:
    """Build the user message with the question and the gathered facts.

    Each fact dict carries: id, text, sources (list of {url, parsed_title,
    parsed_sitename, parsed_author, published_at}), and optionally linked
    concepts. We expose the source metadata — publication name (parsed_sitename),
    article title, author, and published_at — because MultiHop-RAG questions
    frequently reference source attribution and dates ("the TechCrunch article
    published on November 1, 2023").
    """
    lines = [f"Question: {question}", "", "Evidence (facts with source metadata):"]
    lines.extend(_render_evidence_lines(facts))
    lines.append("")
    lines.append(
        "Based on the evidence above, give a short answer. End with exactly:"
    )
    lines.append('The answer to the question is "<answer>"')
    return "\n".join(lines)