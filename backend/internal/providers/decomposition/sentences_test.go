package decomposition

import (
	"reflect"
	"testing"
)

func sentencesText(s []Sentence) []string {
	out := make([]string, len(s))
	for i, c := range s {
		out[i] = trimRunes(c.Text)
	}
	return out
}

func TestSegmentSentences_BasicProse(t *testing.T) {
	text := "Hello world. This is a test! Is it working? Yes."
	got := sentencesText(SegmentSentences(text))
	want := []string{"Hello world.", "This is a test!", "Is it working?", "Yes."}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
}

func TestSegmentSentences_MarkdownStructuralUnits(t *testing.T) {
	text := "# Heading One\n\n" +
		"First sentence. Second sentence.\n\n" +
		"```go\n" +
		"func main() {\n" +
		"    fmt.Println(\"hi\")\n" +
		"}\n" +
		"```\n\n" +
		"| col1 | col2 |\n" +
		"|------|------|\n" +
		"| a    | b    |\n\n" +
		"- item one\n" +
		"- item two\n" +
		"  continuation line\n\n" +
		"Final sentence.\n"

	sents := SegmentSentences(text)
	got := sentencesText(sents)

	want := []string{
		"# Heading One",
		"First sentence.",
		"Second sentence.",
		"```go\nfunc main() {\n    fmt.Println(\"hi\")\n}\n```",
		"| col1 | col2 |\n|------|------|\n| a    | b    |",
		"- item one\n- item two\n  continuation line",
		"Final sentence.",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v want %#v", got, want)
	}
}

func TestSegmentSentences_AbsoluteRuneOffsets(t *testing.T) {
	text := "Hello. World."
	sents := SegmentSentences(text)
	if len(sents) != 2 {
		t.Fatalf("expected 2 sentences got %d", len(sents))
	}
	// Verify offsets index back into the source text correctly.
	for _, s := range sents {
		if string([]rune(text)[s.StartRune:s.EndRune]) != s.Text {
			t.Fatalf("offset mismatch: span %d:%d = %q not %q",
				s.StartRune, s.EndRune,
				string([]rune(text)[s.StartRune:s.EndRune]), s.Text)
		}
	}
}

func TestSegmentSentences_IndentedCodeBlock(t *testing.T) {
	text := "Intro sentence.\n\n    indented code line one\n    indented code line two\n\nOutro sentence.\n"
	got := sentencesText(SegmentSentences(text))
	want := []string{
		"Intro sentence.",
		"indented code line one\n    indented code line two",
		"Outro sentence.",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v want %#v", got, want)
	}
}

func TestSegmentSentences_SetextHeading(t *testing.T) {
	text := "Heading text\n===========\n\nBody sentence here.\n"
	got := sentencesText(SegmentSentences(text))
	want := []string{"Heading text\n===========", "Body sentence here."}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v want %#v", got, want)
	}
}