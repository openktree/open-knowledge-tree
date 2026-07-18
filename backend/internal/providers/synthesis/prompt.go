package synthesis

import (
	"fmt"
	"strings"
)

// buildSynthesisUserMessage formats the concept header, the summary
// slices, and the candidate image list into the user message the
// synthesis LLM receives. Each slice is rendered as:
//
//   N. [concept_id=<concept:id>, seq=<n>, facts=<count>]
//   <content>
//
// so the model can see which slice a citation originated in. The
// candidate images are rendered as:
//
//   - [<fact:id>] alt: <alt> (text: <text>)
//
// The fact_id is the canonical UUID string so the model can emit
// ![alt](<fact:fact_id>) links verbatim. The related-concepts block
// exposes concept_ids as <concept:id> so the model can emit
// [name](<concept:concept_id>) concept links verbatim. When there
// are no candidates, the image section is omitted and the prompt
// tells the model to embed no images (the system prompt already
// covers the empty-list case, but the explicit reminder reduces
// hallucinated image ids).
func buildSynthesisUserMessage(req SynthesisRequest) string {
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Concept: %q\n", req.CanonicalName))
	sb.WriteString("Context: ")
	sb.WriteString(req.Context)
	sb.WriteString("\n\n")

	sb.WriteString("--- Summary slices ---\n")
	for i, s := range req.Slices {
		sb.WriteString(fmt.Sprintf("\n%d. [concept_id=<concept:%s>, seq=%d, facts=%d]\n",
			i+1, s.ConceptID, s.SequenceNum, s.FactCount))
		sb.WriteString(s.Content)
		sb.WriteString("\n")
	}

	// Related concepts block (Graph-Aware Reasoning context). The
	// system prompt tells the model to treat this as relational
	// evidence; an empty list is announced so the model doesn't
	// hallucinate relations.
	if len(req.RelatedConcepts) > 0 {
		sb.WriteString("\n\n--- Related concepts (top by shared facts; strongest carry their synthesis text) ---\n")
		for _, rc := range req.RelatedConcepts {
			sb.WriteString(fmt.Sprintf("\n- %s (shared facts: %d, concept_id: <concept:%s>)\n",
				rc.CanonicalName, rc.SharedFactCount, rc.ConceptID))
			if len(rc.Contexts) > 0 {
				for _, c := range rc.Contexts {
					sb.WriteString(fmt.Sprintf("  · %s: %d shared\n", c.Context, c.SharedFactCount))
				}
			}
			if rc.Synthesis != "" {
				sb.WriteString("\n  Related synthesis:\n")
				sb.WriteString(rc.Synthesis)
				if !strings.HasSuffix(rc.Synthesis, "\n") {
					sb.WriteString("\n")
				}
			} else {
				sb.WriteString("  (no related synthesis available)\n")
			}
		}
	} else {
		sb.WriteString("\n\nNo related concept data available.\n")
	}

	if len(req.CandidateImages) > 0 {
		sb.WriteString("\n\n--- Candidate images (embed up to ")
		sb.WriteString(fmt.Sprintf("%d", req.MaxImages))
		sb.WriteString(") ---\n")
		for _, img := range req.CandidateImages {
			alt := img.Alt
			if alt == "" {
				alt = img.Text
				if len(alt) > 80 {
					alt = alt[:80] + "..."
				}
			}
			sb.WriteString(fmt.Sprintf("- [<fact:%s>] alt: %s (text: %s)\n",
				img.FactID, alt, truncate(img.Text, 160)))
		}
	} else {
		sb.WriteString("\n\nNo candidate images provided; embed no images.\n")
	}

	sb.WriteString("\nFold the slices above into the single authoritative definition for this concept.\n")
	return sb.String()
}

// buildImagePickerUserMessage formats the concept header and the
// candidate image list into the user message the image-picker LLM
// receives. Each candidate is rendered as:
//
//   - [<fact:fact_id>] alt: <alt> (text: <text>)
//
// The final line states the max number to return.
func buildImagePickerUserMessage(req ImagePickRequest) string {
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Concept: %q\n", req.CanonicalName))
	sb.WriteString("Context: ")
	sb.WriteString(req.Context)
	sb.WriteString("\n\n")

	sb.WriteString("--- Candidate images ---\n")
	for _, img := range req.Candidates {
		alt := img.Alt
		if alt == "" {
			alt = img.Text
			if len(alt) > 80 {
				alt = alt[:80] + "..."
			}
		}
		sb.WriteString(fmt.Sprintf("- [<fact:%s>] alt: %s (text: %s)\n",
			img.FactID, alt, truncate(img.Text, 160)))
	}

	sb.WriteString(fmt.Sprintf("\nReturn up to %d fact_ids, one per line.\n", req.MaxImages))
	return sb.String()
}

// truncate shortens s to at most n runes, appending "..." when it
// truncates. Used to keep image descriptions in the prompt bounded.
func truncate(s string, n int) string {
	if n <= 0 || len([]rune(s)) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n]) + "..."
}