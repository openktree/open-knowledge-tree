package summarization

import (
	"fmt"
	"strings"
)

// FormatSystemPrompt renders a summarization system-prompt template
// (carrying two %d verbs) with the per-call word budget derived from
// maxTokens. Exported so a custom promptset's template can be
// formatted by the provider without re-implementing the word-budget
// arithmetic. When maxTokens <= 0 a sensible fallback (450 words) is
// used, matching the historical default.
func FormatSystemPrompt(template string, maxTokens int) string {
	var maxWords int
	if maxTokens > 0 {
		// 1 token ≈ 0.75 words for English prose.
		maxWords = int(float64(maxTokens) * 0.75)
	} else {
		maxWords = 450
	}
	return fmt.Sprintf(template, maxWords, maxWords)
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
