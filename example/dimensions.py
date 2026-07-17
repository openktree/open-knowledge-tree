import json
import logging
import re

from kt_config.types import COMPOUND_FACT_TYPES
from kt_db.models import Fact, Node
from kt_models.gateway import ModelGateway
from kt_models.link_normalizer import normalize_ai_links
from kt_models.prompt_fragments import CITE_FACTS_INSTRUCTION

logger = logging.getLogger(__name__)

_CITATION_INSTRUCTION = CITE_FACTS_INSTRUCTION

DIMENSION_SYSTEM_PROMPT = f"""You are a reasoning engine. You must reason ONLY from the provided facts.
Do NOT use your training data or prior knowledge. Base your analysis exclusively on the facts given.

When facts include source attribution (shown in parentheses after the fact), \
mention the key sources, organizations, or people behind the claims in your analysis. \
For example, instead of "studies show X is effective", write "according to research \
published by [organization], X is effective". This is especially important for claims \
and contested topics — the reader should know WHO is saying something, not just WHAT \
is being said.

{_CITATION_INSTRUCTION}

Return a JSON object with exactly these keys:
- "content": (string) Your analysis/synthesis of the concept based on the facts. \
Mention key sources and entities by name when attributing claims. Embed fact citation \
links (see above) for specific pieces of evidence.
- "confidence": (float between 0 and 1) How confident you are in your analysis given the facts.
- "suggested_concepts": (list of strings) Related concepts, entities, people, places, \
techniques, or topics that are mentioned in or strongly implied by the facts and deserve \
their own dedicated exploration. Be generous — include anything a curious researcher \
would want to investigate next. Aim for 5-15 suggestions.
- "relevant_facts": (list of integers) The 1-based indices of facts that were actually \
relevant and useful for your analysis. Only include facts that are genuinely about this \
specific concept — not facts that merely mention the same broad topic or field. Exclude \
facts that are too generic, off-topic, or that you could not meaningfully use.

Return ONLY the JSON object, no other text."""


CREDULOUS_DIMENSION_PROMPT = f"""You are building the case for a specific position. \
You must reason ONLY from the provided facts. Do NOT use your training data or prior knowledge.

Your task: build the STRONGEST possible case for this position based on the supporting facts. \
Present the argument as compellingly as possible. Note challenges as obstacles to address, \
not as refutations.

When facts include source attribution (shown in parentheses after the fact), \
name the sources, organizations, or people that support the position. Attribution \
strengthens the case — "according to [organization], X" is more compelling than bare \
assertions.

{_CITATION_INSTRUCTION}

Return a JSON object with exactly these keys:
- "content": (string) The strongest case for this position, built from the facts. \
Attribute claims to their sources by name. Embed fact citation links for key evidence.
- "confidence": (float between 0 and 1) How strong the case is given the available evidence.
- "suggested_concepts": (list of strings) Related concepts, entities, people, events, or \
topics mentioned in the facts that could be explored further to strengthen or challenge \
this position. Aim for 5-15 suggestions.
- "relevant_facts": (list of integers) The 1-based indices of facts that were actually \
relevant and useful for your analysis. Only include facts that are genuinely about this \
specific concept — not facts that merely mention the same broad topic or field. Exclude \
facts that are too generic, off-topic, or that you could not meaningfully use.

Return ONLY the JSON object, no other text."""


ENTITY_DIMENSION_PROMPT = f"""You are a reasoning engine. You must reason ONLY from the provided facts.
Do NOT use your training data or prior knowledge. Base your analysis exclusively on the facts given.

This is an ENTITY node — it represents a real-world thing (person, organization, location, \
publication, etc.) rather than an abstract concept. Focus your analysis on:
1. What role this entity plays in relation to the topics covered by the facts
2. Key factual details about this entity found in the evidence
3. How this entity connects to other entities and concepts mentioned in the facts

When facts include source attribution (shown in parentheses after the fact), \
mention the sources by name to ground your analysis. Note which organizations, \
publications, or people are providing information about this entity.

{_CITATION_INSTRUCTION}

Return a JSON object with exactly these keys:
- "content": (string) Your analysis of this entity's role and significance based on the facts. \
Attribute claims to their sources by name. Embed fact citation links for key details.
- "confidence": (float between 0 and 1) How confident you are given the available evidence.
- "suggested_concepts": (list of strings) Related entities, concepts, people, organizations, \
locations, events, or topics mentioned in or strongly implied by the facts. Include both \
other entities AND abstract concepts. Aim for 5-15 suggestions.
- "relevant_facts": (list of integers) The 1-based indices of facts that were actually \
relevant and useful for your analysis. Only include facts that are genuinely about this \
specific entity — not facts that merely mention the same broad topic or field. Exclude \
facts that are too generic, off-topic, or that you could not meaningfully use.

Return ONLY the JSON object, no other text."""


EVENT_DIMENSION_PROMPT = f"""You are a reasoning engine. You must reason ONLY from the provided facts.
Do NOT use your training data or prior knowledge. Base your analysis exclusively on the facts given.

This is an EVENT node — it represents a temporal occurrence (historical event, incident, \
discovery, crisis, etc.). Focus your analysis on:
1. What happened — timeline, key moments, escalation/resolution
2. Who was involved — participants, actors, affected parties
3. Causes — what led to this event, preconditions
4. Effects — consequences, aftermath, ongoing impact
5. Context — historical/social/technical setting

When facts include source attribution (shown in parentheses after the fact), \
mention the sources by name to ground your analysis.

{_CITATION_INSTRUCTION}

Return a JSON object with exactly these keys:
- "content": (string) Your analysis of this event based on the facts. \
Attribute claims to their sources by name. Embed fact citation links for key moments and claims.
- "confidence": (float between 0 and 1) How confident you are given the available evidence.
- "suggested_concepts": (list of strings) Related events, participating entities, causal \
concepts, effects, and topics mentioned in or strongly implied by the facts. Aim for 5-15 suggestions.
- "relevant_facts": (list of integers) The 1-based indices of facts that were actually \
relevant and useful for your analysis. Only include facts that are genuinely about this \
specific event — not facts that merely mention the same broad topic or field. Exclude \
facts that are too generic, off-topic, or that you could not meaningfully use.

Return ONLY the JSON object, no other text."""


METHOD_DIMENSION_PROMPT = f"""You are a reasoning engine. You must reason ONLY from the provided facts.
Do NOT use your training data or prior knowledge. Base your analysis exclusively on the facts given.

This is a METHOD node — it represents a procedure, algorithm, technique, or process \
with defined steps. Focus your analysis on:
1. Purpose — what problem this method solves
2. Steps/procedure — how it works, key phases
3. Inputs/prerequisites — what's needed before applying
4. Outputs/results — what it produces
5. Alternatives — other methods for the same purpose
6. Limitations — when it doesn't work, edge cases

When facts include source attribution (shown in parentheses after the fact), \
mention the sources by name to ground your analysis.

{_CITATION_INSTRUCTION}

Return a JSON object with exactly these keys:
- "content": (string) Your analysis of this method based on the facts. \
Attribute claims to their sources by name. Embed fact citation links for key steps and claims.
- "confidence": (float between 0 and 1) How confident you are given the available evidence.
- "suggested_concepts": (list of strings) Prerequisite concepts, alternative methods, \
related entities, applications, and topics mentioned in or strongly implied by the facts. \
Aim for 5-15 suggestions.
- "relevant_facts": (list of integers) The 1-based indices of facts that were actually \
relevant and useful for your analysis. Only include facts that are genuinely about this \
specific method — not facts that merely mention the same broad topic or field. Exclude \
facts that are too generic, off-topic, or that you could not meaningfully use.

Return ONLY the JSON object, no other text."""


INQUIRY_DIMENSION_PROMPT = f"""You are a reasoning engine. You must reason ONLY from the provided facts.
Do NOT use your training data or prior knowledge. Base your analysis exclusively on the facts given.

This is an INQUIRY node — it represents a question or investigation topic that \
accumulates answers over time. Focus your analysis on:
1. What is being asked — restate the question clearly
2. What the available facts reveal — direct answers and partial answers
3. What remains unknown — gaps in the evidence
4. Confidence assessment — how well the facts answer the question
5. Further investigation paths — what to explore next

When facts include source attribution (shown in parentheses after the fact), \
mention the sources by name to ground your analysis.

{_CITATION_INSTRUCTION}

Return a JSON object with exactly these keys:
- "content": (string) Your analysis addressing the inquiry based on the facts. \
Attribute claims to their sources by name. Embed fact citation links for direct evidence.
- "confidence": (float between 0 and 1) How well the available evidence answers the question.
- "suggested_concepts": (list of strings) Concepts to investigate, entities to research, \
methods to consider, and topics that could help answer this question. Aim for 5-15 suggestions.
- "relevant_facts": (list of integers) The 1-based indices of facts that were actually \
relevant and useful for your analysis. Only include facts that are genuinely about this \
specific inquiry — not facts that merely mention the same broad topic or field. Exclude \
facts that are too generic, off-topic, or that you could not meaningfully use.

Return ONLY the JSON object, no other text."""


def _fact_label(content: str, max_words: int = 8) -> str:
    """Extract a short label from fact content for the citation token."""
    words = content.split()
    label = " ".join(words[:max_words])
    if len(words) > max_words:
        label += "…"
    return label.replace("{", "").replace("}", "").replace("|", "-")


def _extract_source_names(fact: Fact) -> str:
    """Extract concise source identifiers from a fact's loaded sources.

    Prefers structured ``author_org`` / ``author_person`` fields on
    FactSource, falling back to the ``who`` regex on the attribution
    string, then to RawSource.title.

    Returns a parenthesised string like ``(BBC; Emma Roth)`` or an
    empty string when no sources are available.
    """
    names: list[str] = []
    seen: set[str] = set()
    for fs in getattr(fact, "sources", []):
        parts: list[str] = []

        # Prefer structured author fields
        org = getattr(fs, "author_org", None)
        person = getattr(fs, "author_person", None)
        if org:
            parts.append(org)
        if person:
            parts.append(person)

        # Fallback: regex on attribution string (backwards compat)
        if not parts and fs.attribution:
            m = re.search(r"who:\s*([^;]+)", fs.attribution)
            if m:
                parts.append(m.group(1).strip())

        # Last resort: raw source title
        if not parts and getattr(fs, "raw_source", None) and fs.raw_source.title:
            parts.append(fs.raw_source.title)

        for name in parts:
            key = name.lower()
            if key not in seen:
                seen.add(key)
                names.append(name)
    if not names:
        return ""
    return f" ({'; '.join(names)})"


def _format_fact(index: int, fact: Fact) -> str:
    """Format a single fact for inclusion in a prompt.

    Compound facts (quote, procedure, reference, code, account, legal) are
    presented as indented blocks to preserve their structure. Atomic facts
    are kept inline.

    Each fact includes a {fact:<uuid>|<label>} citation token so the LLM can
    embed persistent /facts/<uuid> links in the generated content.
    """
    attr = _extract_source_names(fact)
    label = _fact_label(fact.content)
    fact_tag = " {fact:" + str(fact.id) + "|" + label + "}"
    if fact.fact_type in COMPOUND_FACT_TYPES:
        return f"  {index}. [{fact.fact_type}]{attr}{fact_tag}\n    {fact.content}"
    return f"  {index}. [{fact.fact_type}] {fact.content}{attr}{fact_tag}"


def _build_fact_prompt(node: Node, facts: list[Fact], attractor: str | None = None) -> str:
    """Build the user message containing the concept and its facts."""
    lines: list[str] = [f'Concept: "{node.concept}"', ""]

    if attractor:
        lines.append(f"Attractor/perspective: {attractor}")
        lines.append("")

    lines.append("Facts:")
    for i, fact in enumerate(facts, 1):
        lines.append(_format_fact(i, fact))

    lines.append("")
    lines.append("Based ONLY on the facts above, provide your analysis of this concept.")
    return "\n".join(lines)


def _parse_dimension_response(response: str, model_id: str) -> dict[str, object]:
    """Parse a model response into a dimension dict.

    Returns a dict with keys: content, confidence, suggested_concepts.
    Falls back to raw text if JSON parsing fails.
    """
    # Try to extract JSON from the response
    text = response.strip()

    # Handle markdown code fences
    if text.startswith("```"):
        # Remove opening fence (possibly with language tag)
        first_newline = text.index("\n")
        text = text[first_newline + 1 :]
        if text.endswith("```"):
            text = text[:-3].strip()

    try:
        data = json.loads(text)
        # Parse relevant_facts — coerce to list of ints, ignore bad entries
        raw_relevant = data.get("relevant_facts", [])
        relevant_facts: list[int] = []
        if isinstance(raw_relevant, list):
            for item in raw_relevant:
                try:
                    relevant_facts.append(int(item))
                except (ValueError, TypeError):
                    continue
        return {
            "content": normalize_ai_links(str(data.get("content", response))),
            "confidence": float(data.get("confidence", 0.5)),
            "suggested_concepts": list(data.get("suggested_concepts", [])),
            "relevant_facts": relevant_facts,
        }
    except (json.JSONDecodeError, ValueError, TypeError):
        logger.warning("Failed to parse JSON from model %s, using raw response", model_id)
        return {
            "content": normalize_ai_links(response),
            "confidence": 0.5,
            "suggested_concepts": [],
            "relevant_facts": [],
        }


async def generate_dimensions(
    node: Node,
    facts: list[Fact],
    model_ids: list[str],
    gateway: ModelGateway,
    attractor: str | None = None,
    mode: str = "neutral",
) -> list[dict[str, object]]:
    """Generate dimensions for a node from multiple models.

    Each model receives the same fact base and produces its own analysis.

    Args:
        node: The node to generate dimensions for.
        facts: List of facts linked to the node.
        model_ids: List of model identifiers to call.
        gateway: The ModelGateway for making API calls.
        attractor: Optional perspective/attractor to guide analysis.
        mode: "neutral" (default, for concepts) or "credulous" (for perspectives).

    Returns:
        List of dicts with keys: model_id, content, confidence, suggested_concepts.
    """
    if not facts:
        return []

    user_message = _build_fact_prompt(node, facts, attractor=attractor)
    messages = [{"role": "user", "content": user_message}]

    mode_prompts = {
        "credulous": CREDULOUS_DIMENSION_PROMPT,
        "entity": ENTITY_DIMENSION_PROMPT,
        "event": EVENT_DIMENSION_PROMPT,
        "method": METHOD_DIMENSION_PROMPT,
        "inquiry": INQUIRY_DIMENSION_PROMPT,
    }
    system_prompt = mode_prompts.get(mode, DIMENSION_SYSTEM_PROMPT)

    responses = await gateway.generate_parallel(
        model_ids=model_ids,
        messages=messages,
        system_prompt=system_prompt,
        reasoning_effort=gateway.dimension_thinking_level or None,
    )

    dimensions: list[dict[str, object]] = []
    for model_id, response_text in responses.items():
        if response_text.startswith("Error: "):
            logger.error("Model %s failed: %s", model_id, response_text)
            continue

        parsed = _parse_dimension_response(response_text, model_id)
        dimensions.append(
            {
                "model_id": model_id,
                "content": parsed["content"],
                "confidence": parsed["confidence"],
                "suggested_concepts": parsed["suggested_concepts"],
                "relevant_facts": parsed["relevant_facts"],
            }
        )

    return dimensions
