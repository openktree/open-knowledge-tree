package decomposition

import (
	"strings"
	"testing"

	"github.com/openktree/open-knowledge-tree/backend/internal/promptset"
)

// TestBuildImageFactExtractionPrompt_Substitutions verifies the
// three %s placeholders (source URL, source title, image alt text)
// are substituted in order, with the documented fallbacks when a
// value is empty.
func TestBuildImageFactExtractionPrompt_Substitutions(t *testing.T) {
	got := buildImageFactExtractionPrompt(
		"https://example.com/article",
		"Acme Revenue 2023",
		"bar chart of revenue",
		false, // sourceHasText — base prompt only, no scope note
		promptset.Default,
	)
	if !strings.Contains(got, "Source URL: https://example.com/article") {
		t.Errorf("prompt missing substituted Source URL; got:\n%s", got)
	}
	if !strings.Contains(got, "Source title: Acme Revenue 2023") {
		t.Errorf("prompt missing substituted Source title; got:\n%s", got)
	}
	if !strings.Contains(got, "Image alt text (provided by the source page, may be empty): bar chart of revenue") {
		t.Errorf("prompt missing substituted Image alt text; got:\n%s", got)
	}
}

// TestBuildImageFactExtractionPrompt_EmptyFallbacks checks the
// (unknown) / (none) fallbacks fire when the source URL / title /
// alt text are empty, so the model never sees a bare "Source URL: "
// line.
func TestBuildImageFactExtractionPrompt_EmptyFallbacks(t *testing.T) {
	got := buildImageFactExtractionPrompt("", "", "", false, promptset.Default)
	if !strings.Contains(got, "Source URL: (unknown)") {
		t.Errorf("expected (unknown) fallback for Source URL; got:\n%s", got)
	}
	if !strings.Contains(got, "Source title: (unknown)") {
		t.Errorf("expected (unknown) fallback for Source title; got:\n%s", got)
	}
	if !strings.Contains(got, "Image alt text (provided by the source page, may be empty): (none)") {
		t.Errorf("expected (none) fallback for Image alt text; got:\n%s", got)
	}
}

// TestBuildImageFactExtractionPrompt_FocusFiguresNoteAppendedWhenSourceHasText
// asserts the focus-figures scope note is appended when the source
// had parsed text (the text-chunk loop already processed the body).
// The model must be steered toward visual information and away from
// re-transcribing text the text pass already captured.
func TestBuildImageFactExtractionPrompt_FocusFiguresNoteAppendedWhenSourceHasText(t *testing.T) {
	got := buildImageFactExtractionPrompt(
		"https://example.com/article",
		"Acme Revenue 2023",
		"bar chart",
		true, // sourceHasText — scope note appended
		promptset.Default,
	)
	if !strings.Contains(got, "## Scope note") {
		t.Errorf("expected '## Scope note' section when sourceHasText=true; got:\n%s", got)
	}
	if !strings.Contains(got, "text-extraction pass") {
		t.Errorf("expected scope note to mention the text-extraction pass; got:\n%s", got)
	}
	if !strings.Contains(got, "Focus on the visual information") {
		t.Errorf("expected scope note to instruct focus on visual information; got:\n%s", got)
	}
}

// TestBuildImageFactExtractionPrompt_NoFocusFiguresNoteWhenSourceHasNoText
// asserts the scope note is NOT appended for image-only sources
// (e.g. scanned PDFs with no text layer). In that case the image is
// the primary content and the model should transcribe everything,
// including rendered text.
func TestBuildImageFactExtractionPrompt_NoFocusFiguresNoteWhenSourceHasNoText(t *testing.T) {
	got := buildImageFactExtractionPrompt(
		"https://example.com/scanned.pdf",
		"Scanned Document",
		"",
		false, // sourceHasText — no scope note
		promptset.Default,
	)
	if strings.Contains(got, "## Scope note") {
		t.Errorf("expected NO '## Scope note' section when sourceHasText=false; got:\n%s", got)
	}
	if strings.Contains(got, "text-extraction pass") {
		t.Errorf("expected NO mention of text-extraction pass when sourceHasText=false; got:\n%s", got)
	}
}

// TestBuildImageFactExtractionPrompt_LiteralPercentPreserved
// guards against regressions in the strings.Replace-based
// substitution: the prompt body contains literal '%' characters
// (e.g. "42%", "50%"), and a switch to fmt.Sprintf would interpret
// them as format verbs. We assert a literal "42%" survives the
// build unchanged. This mirrors the existing contract documented
// inline at the call site.
func TestBuildImageFactExtractionPrompt_LiteralPercentPreserved(t *testing.T) {
	got := buildImageFactExtractionPrompt("https://example.com", "Title", "alt", false, promptset.Default)
	if !strings.Contains(got, "42%") {
		t.Errorf("expected literal '42%%' to survive prompt build; got:\n%s", got)
	}
	if strings.Contains(got, "%!s") {
		t.Errorf("detected fmt-style '%%!s' formatting artifact in prompt; got:\n%s", got)
	}
}