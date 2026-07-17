package summarization

import (
	"fmt"
	"strings"
)

// buildSystemPrompt returns the system message the LLM receives for a
// summarization call. The prompt is parameterized by the per-slice
// output token cap (MaxTokens) so the model is told the concrete word
// budget it must fit within, rather than a fixed "~400-500 words"
// heuristic. When maxTokens <= 0 (no cap configured), a sensible
// fallback word guidance is used.
//
// The prompt instructs the model to reason ONLY from the provided
// facts, reflect what the facts say without judging them (credulous),
// preserve the variety of distinct perspectives present in the facts
// (without curating by "relevance" or "correctness"), and condense
// repetition so the word budget covers the breadth of ideas rather
// than being spent re-stating the same point. Structure is
// inverted-pyramid (core ideas first, nuances last) so that, even if
// the model is truncated by the token cap, the most important content
// survives. Central/key facts are cited via [text](<fact:fact_id>)
// links; citation is selective (load-bearing facts only), not
// exhaustive. The "fact:" prefix is a kind discriminator: OKT stores
// facts and concepts in two separate UUID tables, so the prefix
// tells the frontend which route a citation resolves to
// (/facts/{id} for facts, /concepts/{id} for concepts).
//
// The prompt deliberately does NOT request JSON, confidence, or
// suggested concepts — the production dimensions.py envelope is
// dropped here. The summarization provider returns plain markdown.
func buildSystemPrompt(maxTokens int) string {
	var maxWords int
	if maxTokens > 0 {
		// 1 token ≈ 0.75 words for English prose.
		maxWords = int(float64(maxTokens) * 0.75)
	} else {
		maxWords = 450
	}
	return fmt.Sprintf(`You are a reasoning engine. You must reason ONLY from the provided facts.
Do NOT use your training data or prior knowledge. Base your summary
exclusively on the facts given.

================ WORD BUDGET — STRICT ================
HARD LIMIT: Your summary MUST NOT exceed approximately %d words.
This is a strict cap. Count your words as you write. If you approach
the limit, CONDENSE and wrap up — never trail off mid-sentence. The
most important content MUST come first so a reader gets the core even
if the end is cut. Finishing within the budget is mandatory; an
unfinished, cut-off summary is a failure.
======================================================

Your task is to summarize the facts given for the concept. You are
credulous: your job is to reflect what the facts say, not to judge
them. When facts conflict, reflect BOTH sides — do not pick a
position. Do not rank sources by prestige or correctness; attribute
each claim to who makes it and on what basis.

PRIORITIZE VARIETY, NOT EXHAUSTION: Identify the DISTINCT ideas and
perspectives present in the facts. Give each distinct perspective a
representative mention, preserving the full variety of viewpoints.
Do NOT decide which perspective is "correct", "more relevant", or
"more important" — represent what the facts say. When several facts
make the same point, MERGE them into one representative statement.
This is neutral: you are not judging the content, only avoiding
repetition so the word budget covers the breadth of ideas rather
than re-stating one point many times.

INVERTED PYRAMID: Open with 1-2 sentences capturing the central
idea(s). Then supporting detail. Then nuances and edge cases LAST.
This front-loads the core so truncation never cuts the most
important content. A reader should be able to absorb the core in
the first few sentences.

When facts include source attribution (shown in parentheses after
the fact), mention the key sources, organizations, or people behind
the claims in your summary. For example, instead of "studies show X
is effective", write "according to research published by
[organization], X is effective".

Treat every source's claims equally regardless of institutional
prestige, mainstream acceptance, or the source's reputation. A claim
from a government agency, corporation, or peer-reviewed journal is
NOT inherently more reliable than one from an independent researcher
or lesser-known source. Attribute each claim to who makes it and on
what basis; do not grant any source the privilege of bare,
unattributed assertion.

CITATION SYNTAX: Cite central and key facts using markdown links of
the form [text](<fact:fact_id>) where fact_id is the UUID shown
before each fact (the "fact:" prefix identifies the citation kind —
facts and concepts share the UUID space but live in separate
tables). Use citations to support and strengthen your summary —
anchor important claims to the facts that establish them. Not every
fact needs to be cited; cite the ones that are load-bearing for the
summary. A fact may be cited more than once.

Return ONLY the markdown summary. No JSON, no preamble. Remember:
stay within %d words — finish the summary, do not trail off.`, maxWords, maxWords)
}

// buildUserMessage formats the concept header and the fact list
// into the user message the LLM receives. Each fact is rendered as
//
//	N. [<fact:fact_id>] <text> (<attribution>)
//
// with the attribution omitted when empty. The fact_id is the
// canonical UUID string so the model can embed [text](<fact:fact_id>)
// links verbatim.
func buildUserMessage(req SummarizationRequest) string {
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Concept: %q\n", req.ConceptCanonicalName))
	sb.WriteString("Context: ")
	sb.WriteString(req.Context)
	sb.WriteString("\n\nFacts:\n")
	for i, f := range req.Facts {
		sb.WriteString(fmt.Sprintf("  %d. [<fact:%s>] %s", i+1, f.ID, f.Text))
		if f.Attribution != "" {
			sb.WriteString(" ")
			sb.WriteString(f.Attribution)
		}
		sb.WriteString("\n")
	}
	sb.WriteString("\nBased ONLY on the facts above, synthesize the distinct ideas, representative details, and notable perspectives into a self-contained summary that fits within the word budget.\n")
	return sb.String()
}
