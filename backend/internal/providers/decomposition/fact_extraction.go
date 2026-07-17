package decomposition

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/openktree/open-knowledge-tree/backend/internal/providers/ai"
	"github.com/openktree/open-knowledge-tree/backend/internal/store"
)

const factExtractionPrompt = `You are a fact extraction and attribution system. Given the source text below, extract ALL knowledge worth preserving.

## Fact types

Each fact must be assigned one of the following types. Use the description and example to decide which type fits best.
- **claim** — A statement asserted by a person, institution, or source. May or may not be true. Use for opinions, observations, informal definitions, and any general assertion. (1-2 sentences)
  Example: "Kumar Patel at Bell Labs invented the carbon dioxide laser in 1964, which operates at a wavelength of 10.6 micrometers."
- **account** — A first-person or narrative retelling of events or experiences. Use for historical narratives, testimonies, stories. (can be a paragraph)
  Example: "In a 1959 speech at Caltech, Richard Feynman described to the audience his vision of manipulating individual atoms, a lecture that would later be recognized as the founding text of nanotechnology."
- **measurement** — A quantitative data point with units, as reported by a source. Not inherently verified. (1-2 sentences)
  Example: "Water purification using graphene oxide membranes achieves 99.9% removal of bacterial contaminants according to a 2021 study by MIT researchers."
- **formula** — A mathematical, logical, or formal statement that is true by definition within its formal system. Only use for provably correct formal statements. Always prefix the expression with what it represents (the named law, theorem, or relationship) so the fact is self-contained — bare expressions like "E = mc^2" alone are not allowed. (1-2 sentences)
  Example: "Einstein's mass-energy equivalence relates rest energy to mass via E = mc^2."
- **quote** — Verbatim text from a person, document, poem, speech, or book. Preserve exactly as written. (can be a paragraph)
  Example: 'Richard Feynman stated in his 1965 Nobel lecture: "The electron does anything it likes. It just goes in any direction at any speed, forward or backward in time, however it likes."'
- **procedure** — A step-by-step process, algorithm, recipe, or method. Preserve ordering and structure. (can be multi-step)
  Example: "The Sanger DNA sequencing method proceeds in four steps: (1) denature the double-stranded DNA template, (2) anneal a primer to the single-stranded template, (3) extend the primer using DNA polymerase with chain-terminating dideoxynucleotides, (4) separate the resulting fragments by gel electrophoresis to read the sequence."
- **reference** — An extract or summary from a book, paper, specification, or document. Preserves the source's framing. Use for architecture descriptions, detailed explanations, technical overviews. (can be a paragraph)
  Example: "The HTTP/1.1 specification (RFC 7230) defines a message as consisting of a start-line, zero or more header fields, an empty line indicating the end of the header section, and an optional message body."
- **code** — A code snippet, configuration, command, or technical artifact. Preserve formatting exactly. (can be multi-line)
  Example: "The shell command 'find . -name "*.go" -mtime -7' lists Go files modified in the last 7 days."
- **perspective** — An opinionated stance, viewpoint, or position on a topic. Use for editorial opinions, policy positions, ideological arguments, and any subjective take that a person, group, or institution holds. Must clearly state the position. (1-2 sentences)
  Example: "The Electronic Frontier Foundation argues that end-to-end encryption must remain legally protected because any government backdoor creates a vulnerability exploitable by all adversaries, not just law enforcement."

## Rules
- Extract ONLY information explicitly stated in the text. Never add knowledge from outside.
- For types marked "(1-2 sentences)", keep each fact atomic and self-contained.
- For types marked "(can be a paragraph/multi-step/multi-line)", preserve the full structure. Do NOT shred algorithms into individual steps or poems into individual lines. Capture the complete unit.
- A fact may be up to ~1000 characters. A full paragraph is acceptable when needed to make the fact self-contained and understandable on its own — do not truncate a quote, procedure, or reference just to hit a shorter length. Conversely, do not pad a fact beyond what the source states.
- Capture BOTH atomic facts (short claims, measurements) AND structured knowledge (code snippets, algorithms, detailed descriptions, quotes). Do not skip larger knowledge units just because they are long.
- Extract attribution (who said it, where it was published, when) and fold it into the fact text itself (e.g. "Richard Feynman stated in his 1965 Nobel lecture: ...").
- All facts should contain all relevant subjects; no fact should be ambiguous about what or who it is about.
- Do NOT repeat the same fact in different words.

## Structure of a fact

A fact is a **complete assertion** — it must contain at minimum:
- A **subject** — WHO or WHAT is this about (a named entity, concept, or thing)
- A **predicate** — what the subject DOES, IS, or HAS (a verb or verb phrase)
- A **claim or observation** — what is being asserted, measured, described, or quoted

A string that lacks any of these is not a fact. Titles, labels, headings, and bare noun phrases fail this test.

## CRITICAL: Every fact must be self-contained

Each fact will be stored in a knowledge graph and read WITHOUT the original source text. A reader seeing ONLY the fact must fully understand it.

The source text below is pre-segmented into numbered sentences, each prefixed with a [Sn] marker (e.g. "[S0] First sentence. [S1] Second sentence."). Use these markers to record which sentences each fact was derived from.

**Resolve all references.** Replace every pronoun, demonstrative ("this", "that", "these", "he", "she", "we", "they") and implicit subject with the explicit entity, person, concept, or topic name. The source text tells you who "he", "she", "they", "it", "the company", "the study", etc. refer to — substitute the actual name.

**Name the subject.** Every fact must explicitly state WHAT or WHO it is about. Never write "It was founded in 1998" — write "Google was founded in 1998." Never write "The algorithm runs in O(n log n)" — write "Merge sort runs in O(n log n)." Never write "He proposed the theory" — write "Albert Einstein proposed the theory of general relativity."

**Include the topic.** If a fact describes a property, event, or relationship, the fact must name both the subject and what it relates to. "The success rate was 94%" is useless — "The success rate of laparoscopic cholecystectomy was 94% in a 2019 meta-analysis" is a self-contained fact.

**Name specific events, places, and contexts.** "At the fair" is meaningless without knowing WHICH fair — write "at the 1893 World's Columbian Exposition in Chicago". "The technique improved performance" is useless — name the technique and what it improved. Every proper noun, named event, specific method, or location that the source text provides must appear in the fact. If the source doesn't name it, the fact cannot reference it.

**Reject incomplete fragments.** Hedging language ("may have had", "was allegedly", "has been linked to", "is reportedly") without an explicit subject. Either resolve who: "X may have had", "Y was allegedly", or skip.

**Discard unresolvable facts.** If the source text does not provide enough context to determine what a pronoun or vague reference points to, DO NOT extract that fact. A fact that cannot be understood on its own is worthless in the knowledge graph — skip it rather than store an ambiguous statement.

## What to SKIP — do NOT extract these

The source text comes from web pages and may contain noise that is NOT knowledge about the topic. Discard the following:
- **Platform metrics** — Upvote/downvote counts, like counts, share counts, view counts, comment counts, follower counts, star ratings of the page/post itself, karma scores. These are metadata about the container, not knowledge about the topic.
- **Navigation and UI chrome** — "Click here", "Read more", "Subscribe", breadcrumb trails, menu items, sidebar content, footer boilerplate, cookie notices.
- **Ephemeral page metadata** — "Last updated 2 hours ago", "Posted by user123", "5 min read", page word counts, reading time estimates.
- **Advertising and promotion** — Calls to action, coupon codes, "Buy now", affiliate disclaimers, sponsored content labels.
- **Self-referential framing** — "In this article we will discuss...", "This post covers...", "Let's dive in", "Thanks for reading". These describe the container, not the subject.
- **Search engine / aggregator artifacts** — Truncation markers ("..."), "Showing results for", "Related searches", "People also ask" headings without answers.
- **Empty, placeholder, or stub content** — Page numbers alone ("Page 495"), blank pages, "this page intentionally left blank", table of contents entries without content, chapter headings without body text, OCR artifacts with no readable text. If the source text has no substantive information to extract, return [].
- **Titles, headings, and labels** — Article titles, section headings, figure captions, and standalone labels name a topic but assert nothing about it. Extract the substantive content these headings introduce, not the headings themselves.
- **Incomplete fragments** — Bare noun phrases, prepositional phrases ("On knowing..."), and topic labels ("applications of X") are not facts. A fact must make an assertion that can be evaluated as true or false, informative or not.

**Rule of thumb**: If a piece of information would become meaningless or misleading when separated from the specific web page it appeared on, it is not a fact — skip it. A fact should be about the TOPIC, not about the page.

## Examples

BAD: "The Carbon Dioxide Laser"
  → No predicate, no assertion. This is a title/label.

BAD: "For The First Time, A plant that grows without light"
  → Headline framing, no concrete claim. Who made it? When? How?

BAD: "On knowing Marlan"
  → Fragment. No subject performing an action, no assertion.

BAD: "Carbon dioxide laser applications"
  → Bare noun phrase. No verb, no claim.

BAD: "He was good"
  → Vague subject, who is it about?

BAD: "A new approach to water purification"
  → Vague label. What approach? By whom? What makes it new?

BAD: "The role of inflammation in disease"
  → Topic description, not an assertion. What role specifically?

BAD: "Millions of visitors experienced electric lighting and saw AC motors, generators, and other equipment operating safely and reliably at the fair."
  → Which fair? A reader seeing this fact alone cannot identify the event. Write "...at the 1893 World's Columbian Exposition in Chicago" instead.

BAD: "The technique reduced error rates by 40% compared to the previous method."
  → Which technique? Which previous method? Name both explicitly: "Dropout regularization reduced neural network error rates by 40% compared to L2 weight decay in Srivastava et al. (2014)."

BAD: "E = mc^2"
  → Bare expression with no named context. Prefix with what it formalizes (see GOOD example below).

BAD: "x + Attn(LN1(x)) + FF(LN2(x))"
  → Bare math expression with no entity, no name, no relationship to a known concept. Prefix with the construct it represents.

GOOD: "Kumar Patel at Bell Labs invented the carbon dioxide laser in 1964, which operates at a wavelength of 10.6 micrometers."
  → Subject (Kumar Patel), predicate (invented), concrete claim with specifics.

GOOD: "Scientists at Arizona State University demonstrated the first white laser in 2015 by combining red, green, and blue semiconductor laser beams on a single chip."
  → Subject (scientists at ASU), predicate (demonstrated), specific result with method.

GOOD: "Marlan Scully's students described him as combining rigorous mathematical training with an intuitive approach to physics problems."
  → Subject (students), predicate (described), specific observation about a person.

GOOD: "Water purification using graphene oxide membranes achieves 99.9% removal of bacterial contaminants according to a 2021 study by MIT researchers."
  → Subject (purification method), predicate (achieves), measurable claim with attribution.

GOOD: "The Electronic Frontier Foundation argues that end-to-end encryption must remain legally protected because any government backdoor creates a vulnerability exploitable by all adversaries, not just law enforcement."
  → Clear stance holder (EFF), explicit position, reasoning included. This is a perspective, not a neutral claim.

GOOD: 'Richard Feynman stated in his 1965 Nobel lecture: "The electron does anything it likes. It just goes in any direction at any speed, forward or backward in time, however it likes."'
  → Verbatim quote with speaker, occasion, and date. Preserved exactly.

GOOD: "The Sanger DNA sequencing method proceeds in four steps: (1) denature the double-stranded DNA template, (2) anneal a primer to the single-stranded template, (3) extend the primer using DNA polymerase with chain-terminating dideoxynucleotides, (4) separate the resulting fragments by gel electrophoresis to read the sequence."
  → Named procedure (Sanger method), ordered steps preserved as a complete unit.

GOOD: "Einstein's mass-energy equivalence relates rest energy to mass via E = mc^2."
  → Named relationship prefixed before the expression so the fact is self-contained and entity-extractable.

GOOD: "The Pre-LayerNorm transformer block residual computation is x + Attn(LN1(x)) + FF(LN2(x))."
  → Named construct prefixed before the expression. Even abstract math becomes extractable when its referent is named.

**Test before extracting**: Read the candidate fact in isolation — imagine it printed on an index card with NO other context. If a reader would ask "what is this about?", "which one?", or "where?" — the fact is not self-contained. Resolve every implicit reference using information from the source text (name the event, place, person, method, etc.), or skip the fact entirely.

Source text:
"""
%s
"""

Respond with a JSON array of objects, like:
[{"text":"fact one","sentences":[0,1]},{"text":"fact two","sentences":[3]}]
The "sentences" array lists the global sentence indices ([Sn] markers) the fact was derived from. A fact derived from multiple sentences lists all of them. If no facts can be extracted, return []. Respond with ONLY the JSON array, no other text.`

// ExtractedFact is one atomic claim returned by the fact extraction
// provider, together with the global sentence indices it was
// derived from. The indices key into the deterministic sentence
// array produced by SegmentSentences over the source's
// parsed_markdown (or parsed_text); the caller persists one
// fact_references row per index.
type ExtractedFact struct {
	Text      string `json:"text"`
	Sentences []int  `json:"sentences"`
}

type FactExtractionProvider interface {
	ExtractFacts(ctx context.Context, db store.DBTX, chunkText string, attr FactExtractionAttribution) ([]ExtractedFact, error)
	Describe() ProviderDescription
}

// FactExtractionAttribution carries the per-call context the
// fact extractor threads into the AI provider's ChatRequest so
// the resulting ai_usage row is attributed to the repository,
// source, and River job that triggered the extraction. All
// fields are strings (UUIDs in canonical form) so the value is
// cheap to pass through; the tracking helper parses them.
type FactExtractionAttribution struct {
	RepositoryID string
	SourceID     string
	TaskID       string
}

type AIFactExtractionProvider struct {
	AIProvider ai.AIProvider
	Model      string
}

func NewAIFactExtractionProvider(aiProvider ai.AIProvider, model string) *AIFactExtractionProvider {
	return &AIFactExtractionProvider{
		AIProvider: aiProvider,
		Model:      model,
	}
}

// Describe returns the static metadata for the AI-backed fact
// extractor. The provider's "configured" status tracks whether
// the underlying AI provider it was constructed with is itself
// configured (no API key, no provider instance -> the worker
// logs "fact extraction provider not configured" and skips
// extraction).
func (p *AIFactExtractionProvider) Describe() ProviderDescription {
	aiDesc := p.AIProvider.Describe()
	configured := aiDesc.Configured && p.Model != ""
	return ProviderDescription{
		Name: "AI fact extractor",
		Description: "Asks a configured chat model to enumerate the atomic, self-contained factual claims in a chunk of parsed text. Uses a constrained prompt that requires a JSON array of strings; non-JSON responses are run through a bracket-extraction fallback before being rejected.",
		Requires:    "providers.decomposition.fact_extraction.{provider,model} and the underlying AI provider's API key",
		Configured:  configured,
		Supports:    []string{"fact_extraction"},
		Notes:       "Provider is " + aiDesc.Name + ". Per-chunk failures are logged and the chunk is skipped; the source is still marked processed (with 0 facts from that chunk).",
		Config: map[string]string{
			"ai_provider": aiDesc.Name,
			"model":       p.Model,
		},
	}
}

func (p *AIFactExtractionProvider) ExtractFacts(ctx context.Context, db store.DBTX, chunkText string, attr FactExtractionAttribution) ([]ExtractedFact, error) {
	if strings.TrimSpace(chunkText) == "" {
		return nil, nil
	}

	prompt := strings.Replace(factExtractionPrompt, "%s", chunkText, 1)

	var taskID *string
	if attr.TaskID != "" {
		taskID = &attr.TaskID
	}
	resp, err := retryWithBackoff(ctx, retryConfig{}, "fact_extraction",
		func(callCtx context.Context) (ai.ChatResponse, error) {
			return p.AIProvider.Chat(callCtx, db, ai.ChatRequest{
				Model: p.Model,
				Messages: []ai.ChatMessage{
					{Role: "user", Content: prompt},
				},
				TaskID: taskID,
				Attribution: ai.Attribution{
					RepositoryID: attr.RepositoryID,
					SourceID:     attr.SourceID,
					Operation:    "fact_extraction",
				},
			})
		},
	)
	if err != nil {
		return nil, fmt.Errorf("fact extraction: ai chat failed: %w", err)
	}

	if len(resp.Messages) == 0 {
		return nil, nil
	}

	content := resp.Messages[0].Content
	content = strings.TrimSpace(content)

	if content == "" || content == "[]" {
		return nil, nil
	}

	var facts []ExtractedFact
	if err := json.Unmarshal([]byte(content), &facts); err != nil {
		cleaned := cleanJSONArray(content)
		if cleaned != "" {
			if err2 := json.Unmarshal([]byte(cleaned), &facts); err2 != nil {
				return nil, fmt.Errorf("fact extraction: failed to parse response as JSON array: %w (raw: %s)", err, content)
			}
		} else {
			return nil, fmt.Errorf("fact extraction: failed to parse response as JSON array: %w (raw: %s)", err, content)
		}
	}

	return facts, nil
}

func cleanJSONArray(raw string) string {
	raw = strings.TrimSpace(raw)
	start := strings.Index(raw, "[")
	end := strings.LastIndex(raw, "]")
	if start == -1 || end == -1 || end <= start {
		return ""
	}
	return raw[start : end+1]
}
