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
		"- item one",
		"- item two\n  continuation line",
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

func TestSegmentSentences_ListSplitPerItem(t *testing.T) {
	// Tight bullet list: each item is its own sentence unit. The
	// whole-list-as-one-unit rule was changed so the
	// annotate_report worker can embed each item as a distinct
	// claim (a 6-item list of cross-scope bridges otherwise
	// becomes one ~5000-rune sentence whose embedding blends 6
	// distinct claims and retrieves no top-K hit for any
	// individual number). See sentences.go lineListItem case.
	text := "Intro sentence.\n\n" +
		"- item one is short\n" +
		"- item two is short\n" +
		"- item three is short\n\n" +
		"Outro sentence.\n"
	got := sentencesText(SegmentSentences(text))
	want := []string{
		"Intro sentence.",
		"- item one is short",
		"- item two is short",
		"- item three is short",
		"Outro sentence.",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v want %#v", got, want)
	}
}

func TestSegmentSentences_ListSplitOrdered(t *testing.T) {
	// Ordered list: each numbered item is its own sentence unit.
	text := "Lead sentence.\n\n" +
		"1. First point is long enough to stand alone.\n" +
		"2. Second point is long enough to stand alone.\n" +
		"3. Third point is long enough to stand alone.\n\n" +
		"Closing sentence.\n"
	got := sentencesText(SegmentSentences(text))
	want := []string{
		"Lead sentence.",
		"1. First point is long enough to stand alone.",
		"2. Second point is long enough to stand alone.",
		"3. Third point is long enough to stand alone.",
		"Closing sentence.",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v want %#v", got, want)
	}
}

func TestSegmentSentences_ListSplitLooseWithContinuation(t *testing.T) {
	// Loose list: a blank line between items is a continuation of
	// the list block, so item two's unit includes the blank line
	// that precedes it (the rune range spans from the marker to
	// the next marker). Continuation lines under an item stay with
	// that item.
	text := "Lead.\n\n" +
		"- item one\n" +
		"  continuation of one\n\n" +
		"- item two\n\n" +
		"Outro.\n"
	got := sentencesText(SegmentSentences(text))
	want := []string{
		"Lead.",
		"- item one\n  continuation of one",
		"- item two",
		"Outro.",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v want %#v", got, want)
	}
}

func TestSegmentSentences_ListSplitNested(t *testing.T) {
	// Nested list: each list item (top-level or nested) is its own
	// sentence unit. This is deliberate — a nested bullet is its
	// own claim and the annotate_report worker benefits from
	// embedding it as a distinct sentence rather than blending it
	// with the parent. The trade-off is that a parent item that's
	// just a heading ("Consider these points:") becomes a tiny
	// sentence of its own; that's acceptable because
	// min_sentence_runes filters it out at the worker level.
	text := "Lead.\n\n" +
		"- top level one\n" +
		"  - nested one a\n" +
		"  - nested one b\n" +
		"- top level two\n\n" +
		"Outro.\n"
	got := sentencesText(SegmentSentences(text))
	want := []string{
		"Lead.",
		"- top level one",
		"- nested one a",
		"- nested one b",
		"- top level two",
		"Outro.",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v want %#v", got, want)
	}
}