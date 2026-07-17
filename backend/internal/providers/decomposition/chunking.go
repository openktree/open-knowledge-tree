package decomposition

import "strconv"

// Chunk is one window of the source text. The Index is the
// chunk's position in the source's chunk stream (0-based, dense)
// and is what gets persisted on fact_source.chunk_index so the
// UI can scroll back to a fact's origin.
//
// StartRune and EndRune are the absolute rune offsets of the chunk
// text within the source text that was chunked (EndRune is
// exclusive). They let callers map a chunk back to overlapping
// global sentences without re-running the chunker. The simple
// sliding-window chunker sets them directly; other chunkers may
// leave them as zero when the mapping is not meaningful.
type Chunk struct {
	Index     int    `json:"index"`
	Text      string `json:"text"`
	StartRune int    `json:"start_rune"`
	EndRune   int    `json:"end_rune"`
}

// ProviderDescription is the static metadata a decomposition
// provider exposes to operators. It mirrors the shape used by
// the search, fetch, ai, and content_parsing trees so a single
// UI card component can render any of them.
type ProviderDescription struct {
	Name        string   // human-friendly label, e.g. "Simple (sliding-window)"
	Description string   // one-paragraph summary of what the provider does
	Requires    string   // env var / config key needed ("" when always on)
	Configured  bool     // true when the provider is currently usable
	Supports    []string // capabilities handled, e.g. ["chunking", "fact_extraction"]
	Notes       string   // free-form follow-up (e.g. "splits on rune boundary")
	Config      map[string]string // read-only key/value view of the current config
}

type ChunkingProvider interface {
	Chunk(text string) []Chunk
	Describe() ProviderDescription
}

type SimpleChunkingProvider struct {
	ChunkSize    int
	ChunkOverlap int
}

func NewSimpleChunkingProvider(chunkSize, chunkOverlap int) *SimpleChunkingProvider {
	if chunkSize <= 0 {
		chunkSize = 2000
	}
	if chunkOverlap < 0 {
		chunkOverlap = 0
	}
	if chunkOverlap >= chunkSize {
		chunkOverlap = chunkSize / 10
	}
	return &SimpleChunkingProvider{
		ChunkSize:    chunkSize,
		ChunkOverlap: chunkOverlap,
	}
}

func (p *SimpleChunkingProvider) Chunk(text string) []Chunk {
	if len(text) == 0 {
		return nil
	}
	runes := []rune(text)
	total := len(runes)
	if total <= p.ChunkSize {
		return []Chunk{{Index: 0, Text: text, StartRune: 0, EndRune: total}}
	}

	var chunks []Chunk
	step := p.ChunkSize - p.ChunkOverlap
	if step <= 0 {
		step = p.ChunkSize
	}
	for start := 0; start < total; start += step {
		end := start + p.ChunkSize
		if end > total {
			end = total
		}
		chunks = append(chunks, Chunk{
			Index:     len(chunks),
			Text:      string(runes[start:end]),
			StartRune: start,
			EndRune:   end,
		})
		if end >= total {
			break
		}
	}
	return chunks
}

// Describe returns the static metadata for the simple chunker.
// The provider is always available (it is pure Go with no
// external dependencies) so Configured is true unconditionally.
// The chunk_size / chunk_overlap read back from the struct so
// the UI can surface the effective values even when the config
// defaults kicked in.
func (p *SimpleChunkingProvider) Describe() ProviderDescription {
	size := p.ChunkSize
	overlap := p.ChunkOverlap
	return ProviderDescription{
		Name:        "Simple (sliding-window chunker)",
		Description: "Splits parsed text into fixed-size overlapping windows measured in Unicode runes. Pure Go; no external dependencies.",
		Requires:    "",
		Configured:  true,
		Supports:    []string{"chunking"},
		Notes:       "Chunks larger than chunk_size are split with a sliding window; chunk_overlap is capped to chunk_size/10 by NewSimpleChunkingProvider.",
		Config: map[string]string{
			"chunk_size":    intToString(size),
			"chunk_overlap": intToString(overlap),
		},
	}
}

func intToString(n int) string {
	return strconv.Itoa(n)
}
