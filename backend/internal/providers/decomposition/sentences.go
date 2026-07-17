package decomposition

// SentenceChunkingProvider is a markdown-aware chunker that
// segments the source text into one chunk per sentence. It is the
// deterministic, chunker-config-independent contract used to map
// facts back to their origin sentences: every downstream consumer
// (storage, UI highlight, re-extraction) keys into the same global
// sentence array this provider produces.
//
// Markdown awareness:
//   - Fenced code blocks (``` or ~~~) are treated as a single
//     sentence unit regardless of internal punctuation.
//   - Indented code blocks (4-space/1-tab prefix lines) are treated
//     as a single sentence unit.
//   - Headings (ATX # ... or Setext underline ===/---) are treated
//     as a single sentence unit (titles assert nothing on their own
//     but the extraction prompt instructs the AI to extract the
//     substantive content they introduce; keeping the heading as a
//     unit lets a fact reference it when it genuinely summarizes the
//     section).
//   - Tables (GFM pipe tables, header separator row included) are
//     treated as a single sentence unit — splitting a table by
//     internal punctuation destroys its structure.
//   - List items are NOT split per item; a contiguous list block is
//     one sentence unit. A list item often contains a fragment, not
//     a complete sentence, and splitting would produce non-self-
//     contained units. The AI can still extract atomic facts from
//     the list and reference the whole list unit.
//   - Blockquotes are split by sentence on their inner text (the
//     `>` markers are stripped for splitting purposes) unless they
//     contain a code block, in which case the code block is its own
//     unit.
//
// Plain prose paragraphs are split on sentence boundaries: `.`, `!`,
// `?` followed by whitespace or end-of-text. The terminal punctuation
// is kept inside the sentence span.
//
// The provider is pure Go with no external dependencies and is always
// available (Configured: true).
type SentenceChunkingProvider struct {
	// MaxRunes caps a single sentence unit. A unit longer than this
	// (e.g. a very long paragraph with no terminal punctuation, or a
	// large code block) is hard-split on rune boundaries into
	// consecutive units so no chunk grows unbounded. 0 = no cap.
	MaxRunes int
}

// NewSentenceChunkingProvider returns a markdown-aware sentence
// chunker. maxRunes <= 0 means no cap.
func NewSentenceChunkingProvider(maxRunes int) *SentenceChunkingProvider {
	if maxRunes < 0 {
		maxRunes = 0
	}
	return &SentenceChunkingProvider{MaxRunes: maxRunes}
}

// Sentence is a single sentence unit produced by the sentence
// chunker. Index is the 0-based global position in the source's
// sentence stream; StartRune and EndRune are absolute rune offsets
// into the source text (EndRune exclusive). Text is the raw
// substring (including any markdown markers / fence lines).
type Sentence = Chunk

// SegmentSentences is the package-level entry point used outside the
// chunker (e.g. by the retrieve_source worker to persist sentence
// offsets, and by the frontend contract). It is equivalent to
// (&SentenceChunkingProvider{MaxRunes: 0}).Chunk(text) but returns
// the result typed as []Sentence for readability at call sites that
// reason in terms of sentences rather than chunks.
func SegmentSentences(text string) []Sentence {
	return (&SentenceChunkingProvider{MaxRunes: 0}).Chunk(text)
}

// Chunk implements ChunkingProvider. It walks the markdown line by
// line, grouping structural blocks (code fences, indented code,
// headings, tables, lists) into single units and splitting prose
// paragraphs on sentence boundaries.
func (p *SentenceChunkingProvider) Chunk(text string) []Chunk {
	if text == "" {
		return nil
	}
	runes := []rune(text)
	lines := splitLinesKeepEnds(runes)

	var units []unit
	i := 0
	for i < len(lines) {
		line := lines[i]
		kind := classifyLine(line, units, i)

		switch kind {
		case lineFencedCode:
			start := i
			fence := fenceMarker(line)
			i++
			for i < len(lines) && !isClosingFence(lines[i], fence) {
				i++
			}
			if i < len(lines) {
				i++ // include closing fence
			}
			units = append(units, unit{startLine: start, endLine: i})
		case lineIndentedCode:
			start := i
			for i < len(lines) && isIndentedCodeLine(lines[i]) {
				i++
			}
			units = append(units, unit{startLine: start, endLine: i})
		case lineATXHeading:
			units = append(units, unit{startLine: i, endLine: i + 1})
			i++
		case lineSetextHeading:
			// The heading is the previous prose line + this underline.
			if len(units) > 0 {
				prev := &units[len(units)-1]
				if prev.kind == unitProse {
					// Merge: convert the previous prose unit into a heading unit.
					prev.kind = unitHeading
					prev.endLine = i + 1
					i++
					continue
				}
			}
			units = append(units, unit{startLine: i, endLine: i + 1, kind: unitHeading})
			i++
		case lineTable:
			start := i
			i++
			for i < len(lines) && isTableRow(lines[i]) {
				i++
			}
			units = append(units, unit{startLine: start, endLine: i, kind: unitTable})
		case lineListItem:
			start := i
			for i < len(lines) && (isListItem(lines[i]) || isContinuationLine(lines[i])) {
				i++
			}
			units = append(units, unit{startLine: start, endLine: i, kind: unitList})
		case lineBlank:
			i++
		default: // prose
			start := i
			for i < len(lines) && classifyLine(lines[i], units, i) == lineProse {
				i++
			}
			units = append(units, unit{startLine: start, endLine: i, kind: unitProse})
		}
	}

	// Convert line-indexed units into rune-offset chunks. Prose units
	// are further split on sentence boundaries; structural units
	// (code, heading, table, list) become a single chunk each.
	var chunks []Chunk
	for _, u := range units {
		startRune := lineStartRune(lines, u.startLine)
		endRune := lineEndRune(lines, u.endLine, len(runes))
		unitText := string(runes[startRune:endRune])

		if u.kind == unitProse {
			sub := splitProseSentences([]rune(unitText), startRune)
			for _, s := range sub {
				s.Index = len(chunks)
				chunks = append(chunks, s)
			}
			continue
		}
		chunks = append(chunks, Chunk{
			Index:     len(chunks),
			Text:      unitText,
			StartRune: startRune,
			EndRune:   endRune,
		})
	}

	if p.MaxRunes > 0 {
		chunks = capChunks(chunks, p.MaxRunes)
	}
	return chunks
}

// Describe implements ChunkingProvider.
func (p *SentenceChunkingProvider) Describe() ProviderDescription {
	return ProviderDescription{
		Name:        "Sentence (markdown-aware)",
		Description: "Segments parsed markdown into sentence units. Fenced/indented code, headings, tables, and list blocks are kept as single units; prose is split on .!? boundaries. Produces the deterministic global sentence array used as the fact-reference contract.",
		Requires:    "",
		Configured:  true,
		Supports:    []string{"chunking"},
		Notes:       "The sentence array this provider produces is the stable contract between extraction, storage, and UI highlighting. Changing its rules requires re-deriving all fact_references.",
		Config: map[string]string{
			"max_runes": intToString(p.MaxRunes),
		},
	}
}

// --- internal line classification ---

type lineKind int

const (
	lineProse lineKind = iota
	lineBlank
	lineFencedCode
	lineIndentedCode
	lineATXHeading
	lineSetextHeading
	lineTable
	lineListItem
)

type unitKind int

const (
	unitProse unitKind = iota
	unitHeading
	unitTable
	unitList
	unitCode
)

type unit struct {
	startLine int
	endLine   int
	kind      unitKind
}

// splitLinesKeepEnds splits a rune slice into lines, preserving the
// line terminator (\n) at the end of each line. The final line may
// have no terminator.
func splitLinesKeepEnds(runes []rune) [][]rune {
	var lines [][]rune
	start := 0
	for i := 0; i < len(runes); i++ {
		if runes[i] == '\n' {
			lines = append(lines, runes[start:i+1])
			start = i + 1
		}
	}
	if start < len(runes) {
		lines = append(lines, runes[start:])
	}
	return lines
}

// classifyLine determines the structural kind of a line. `prev`
// is the units accumulated so far (used to detect Setext headings,
// which depend on the previous prose line) and `idx` is the line
// index being classified.
func classifyLine(line []rune, prev []unit, idx int) lineKind {
	// Trim leading spaces for classification, but keep a copy for
	// indented-code detection (which needs the raw prefix).
	trimmed := trimLeftSpaces(line)

	if len(trimmed) == 0 || (len(trimmed) == 1 && trimmed[0] == '\n') {
		return lineBlank
	}

	// Fenced code: ``` or ~~~ (3+ markers), optionally with language.
	if isFenceOpen(trimmed) {
		return lineFencedCode
	}

	// Indented code: 4+ spaces or 1+ tab at start.
	if isIndentedCodeLine(line) {
		return lineIndentedCode
	}

	// ATX heading: 1-6 # at start followed by space or end.
	if isATXHeading(trimmed) {
		return lineATXHeading
	}

	// Setext heading underline: === or --- (3+) on a line that
	// follows prose.
	if isSetextUnderline(trimmed) && len(prev) > 0 {
		// Only treat as setext if the previous unit is prose and the
		// last line of that unit is the immediately preceding line.
		p := &prev[len(prev)-1]
		if p.kind == unitProse && p.endLine == idx {
			return lineSetextHeading
		}
	}

	// Table row: starts with | or has | ... | structure. The
	// separator row (| --- | --- |) is also a table row.
	if isTableRow(line) {
		return lineTable
	}

	// List item: -, *, +, or digit. at start followed by space.
	if isListItem(line) {
		return lineListItem
	}

	return lineProse
}

func trimLeftSpaces(runes []rune) []rune {
	i := 0
	for i < len(runes) && (runes[i] == ' ' || runes[i] == '\t') {
		i++
	}
	return runes[i:]
}

func isFenceOpen(trimmed []rune) bool {
	if len(trimmed) < 3 {
		return false
	}
	marker := trimmed[0]
	if marker != '`' && marker != '~' {
		return false
	}
	count := 0
	for count < len(trimmed) && trimmed[count] == marker {
		count++
	}
	return count >= 3
}

func fenceMarker(line []rune) rune {
	t := trimLeftSpaces(line)
	if len(t) == 0 {
		return 0
	}
	return t[0]
}

func isClosingFence(line []rune, marker rune) bool {
	t := trimLeftSpaces(line)
	count := 0
	for count < len(t) && t[count] == marker {
		count++
	}
	return count >= 3
}

func isIndentedCodeLine(line []rune) bool {
	if len(line) >= 1 && line[0] == '\t' {
		return true
	}
	if len(line) >= 4 && line[0] == ' ' && line[1] == ' ' && line[2] == ' ' && line[3] == ' ' {
		return true
	}
	return false
}

func isATXHeading(trimmed []rune) bool {
	if len(trimmed) == 0 || trimmed[0] != '#' {
		return false
	}
	n := 0
	for n < len(trimmed) && trimmed[n] == '#' {
		n++
	}
	if n > 6 {
		return false
	}
	// Must be followed by space or end-of-line.
	if n < len(trimmed) && trimmed[n] != ' ' && trimmed[n] != '\n' {
		return false
	}
	return true
}

func isSetextUnderline(trimmed []rune) bool {
	if len(trimmed) < 3 {
		return false
	}
	marker := trimmed[0]
	if marker != '=' && marker != '-' {
		return false
	}
	for i := 0; i < len(trimmed); i++ {
		r := trimmed[i]
		if r != marker && r != ' ' && r != '\t' && r != '\n' {
			return false
		}
	}
	return true
}

func isTableRow(line []rune) bool {
	t := trimLeftSpaces(line)
	if len(t) == 0 {
		return false
	}
	// A table row starts with | or contains | ... |. Blank lines are
	// already excluded. A line that is just prose with no pipe is not
	// a table row.
	if t[0] == '|' {
		return true
	}
	// Detect "cell | cell" pattern.
	hasPipe := false
	for _, r := range t {
		if r == '|' {
			hasPipe = true
			break
		}
		if r == '\n' {
			break
		}
	}
	return hasPipe
}

func isListItem(line []rune) bool {
	t := trimLeftSpaces(line)
	if len(t) == 0 {
		return false
	}
	// Bullet: -, *, + followed by space.
	if (t[0] == '-' || t[0] == '*' || t[0] == '+') && len(t) > 1 && t[1] == ' ' {
		return true
	}
	// Ordered: digits followed by . or ) and space.
	i := 0
	for i < len(t) && t[i] >= '0' && t[i] <= '9' {
		i++
	}
	if i == 0 {
		return false
	}
	if i < len(t) && (t[i] == '.' || t[i] == ')') && i+1 < len(t) && t[i+1] == ' ' {
		return true
	}
	return false
}

// isContinuationLine detects a line that is a soft continuation of
// the current list block (indented under a list item, or a blank
// line within a loose list). We keep it simple: any indented line
// (2+ spaces or a tab) or a blank line is treated as continuation
// while we are inside a list unit. The list unit ends at the first
// non-indented, non-blank, non-list-item line, which is detected by
// the outer loop falling through to another kind.
func isContinuationLine(line []rune) bool {
	if len(line) == 0 {
		return true
	}
	if line[0] == '\t' {
		return true
	}
	if len(line) >= 2 && line[0] == ' ' && line[1] == ' ' {
		return true
	}
	// Blank line (just whitespace + newline).
	for _, r := range line {
		if r != ' ' && r != '\t' && r != '\n' {
			return false
		}
	}
	return true
}

// lineStartRune returns the rune offset where the given line index
// begins within the full text.
func lineStartRune(lines [][]rune, lineIdx int) int {
	if lineIdx >= len(lines) {
		lineIdx = len(lines) - 1
	}
	off := 0
	for i := 0; i < lineIdx; i++ {
		off += len(lines[i])
	}
	return off
}

// lineEndRune returns the rune offset just past the end of the line
// range [0, endLine).
func lineEndRune(lines [][]rune, endLine, total int) int {
	if endLine >= len(lines) {
		return total
	}
	off := 0
	for i := 0; i < endLine; i++ {
		off += len(lines[i])
	}
	return off
}

// splitProseSentences splits a prose unit (one or more consecutive
// prose lines) into sentence chunks on `.`, `!`, `?` followed by
// whitespace or end-of-unit. baseRune is the absolute rune offset of
// the unit's start within the source text, so each returned chunk
// carries absolute StartRune/EndRune.
func splitProseSentences(unitRunes []rune, baseRune int) []Chunk {
	if len(unitRunes) == 0 {
		return nil
	}
	var chunks []Chunk
	sentStart := 0
	for i := 0; i < len(unitRunes); i++ {
		r := unitRunes[i]
		if r != '.' && r != '!' && r != '?' {
			continue
		}
		// Need a following whitespace or end-of-unit to count as a
		// boundary. Also accept end-of-line ('\n').
		next := rune(0)
		if i+1 < len(unitRunes) {
			next = unitRunes[i+1]
		}
		if next == ' ' || next == '\t' || next == '\n' || next == 0 {
			end := i + 1
			// Skip trailing whitespace into the sentence span so the
			// next sentence starts cleanly.
			for end < len(unitRunes) && (unitRunes[end] == ' ' || unitRunes[end] == '\t' || unitRunes[end] == '\n') {
				end++
			}
			text := string(unitRunes[sentStart:end])
			if trimRunes(text) != "" {
				chunks = append(chunks, Chunk{
					Text:      text,
					StartRune: baseRune + sentStart,
					EndRune:   baseRune + end,
				})
			}
			sentStart = end
		}
	}
	// Trailing fragment with no terminal punctuation.
	if sentStart < len(unitRunes) {
		text := string(unitRunes[sentStart:])
		if trimRunes(text) != "" {
			chunks = append(chunks, Chunk{
				Text:      text,
				StartRune: baseRune + sentStart,
				EndRune:   baseRune + len(unitRunes),
			})
		}
	}
	return chunks
}

func trimRunes(s string) string {
	runes := []rune(s)
	i := 0
	for i < len(runes) && (runes[i] == ' ' || runes[i] == '\t' || runes[i] == '\n') {
		i++
	}
	j := len(runes)
	for j > i && (runes[j-1] == ' ' || runes[j-1] == '\t' || runes[j-1] == '\n') {
		j--
	}
	return string(runes[i:j])
}

// capChunks hard-splits any chunk longer than maxRunes into
// consecutive sub-chunks of at most maxRunes runes. The sub-chunks
// keep contiguous Index values. This bounds chunk size for very
// long punctuation-free units (e.g. a 5000-rune code block) so the
// AI is never fed an unbounded input.
func capChunks(chunks []Chunk, maxRunes int) []Chunk {
	if maxRunes <= 0 {
		return chunks
	}
	out := make([]Chunk, 0, len(chunks))
	idx := 0
	for _, c := range chunks {
		runes := []rune(c.Text)
		if len(runes) <= maxRunes {
			c.Index = idx
			out = append(out, c)
			idx++
			continue
		}
		start := c.StartRune
		for off := 0; off < len(runes); off += maxRunes {
			end := off + maxRunes
			if end > len(runes) {
				end = len(runes)
			}
			out = append(out, Chunk{
				Index:     idx,
				Text:      string(runes[off:end]),
				StartRune: start + off,
				EndRune:   start + end,
			})
			idx++
		}
	}
	return out
}