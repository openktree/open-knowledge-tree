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


def fact_query_user(question: str) -> str:
    return f"Question: {question}\n\nFact search queries:"


# --- Shared answer-synthesis prompt ---------------------------------------

ANSWER_SYSTEM = (
    "You answer multi-hop questions using evidence gathered from a knowledge base.\n"
    "Rules:\n"
    "1. Use ONLY the provided facts and their source metadata to answer.\n"
    "2. Many questions require reasoning across MULTIPLE sources (multi-hop): "
    "combine facts from different sources, and use source metadata "
    "(publication name, title, author, published_at) when the question "
    "references it. The publication name is the 'Source:' prefix on each "
    "fact (e.g. 'Source: TechCrunch \"...\" by Jacquelyn Melinek on "
    "2023-10-01'). Questions that mention a publication by name "
    "('the TechCrunch article', 'the Bloomberg piece') are asking you to "
    "filter to facts whose Source line names that publication.\n"
    "3. Keep the answer SHORT: a single name, organization, 'Yes', 'no', "
    "or a short phrase.\n"
    "4. For comparison and temporal questions that expect a 'Yes' or 'no' "
    "answer: commit to an answer when the evidence is present, even if it "
    "requires a judgment call. Only abstain when the evidence is truly "
    "absent or directly contradicts the question. Do NOT abstain just "
    "because you are unsure — make your best guess from the available "
    "facts.\n"
    "5. If the provided evidence is genuinely absent or empty, respond "
    "with the abstention phrase.\n"
    "6. ALWAYS end your final message with exactly one line in this format:\n"
    '   The answer to the question is "<answer>"\n'
    '   For abstention, the line must be:\n'
    '   The answer to the question is "Insufficient information."\n'
    "7. Do not include any text after that final line."
)


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
    if not facts:
        lines.append("(no facts were retrieved for this question)")
    for i, f in enumerate(facts, 1):
        lines.append(f"--- Fact {i} (id: {f['id']}) ---")
        lines.append(f"Text: {f.get('text', '').strip()}")
        srcs = f.get("sources", [])
        if srcs:
            for s in srcs:
                # Compose a one-line attribution with whatever metadata is
                # available. Lead with the publication name (parsed_sitename)
                # because MultiHop-RAG questions identify articles by
                # outlet ("the TechCrunch article", "the Bloomberg piece").
                # Fall back to the article title, then to the URL.
                site = (s.get("parsed_sitename") or "").strip()
                title = (s.get("parsed_title") or "").strip()
                author = (s.get("parsed_author") or "").strip()
                pub = s.get("published_at") or ""
                # Trim published_at to date-only when present (it comes back
                # as "2023-09-28" already from the date column, but be
                # defensive in case a timestamp slips through).
                if pub and "T" in str(pub):
                    pub = str(pub).split("T")[0]
                # Attribution line.
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
    lines.append("")
    lines.append(
        "Based on the evidence above, give a short answer. End with exactly:"
    )
    lines.append('The answer to the question is "<answer>"')
    return "\n".join(lines)