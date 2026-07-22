package decomposition

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/openktree/open-knowledge-tree/backend/internal/providers/ai"
	"github.com/openktree/open-knowledge-tree/backend/internal/promptset"
	"github.com/openktree/open-knowledge-tree/backend/internal/store"
)

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
	// promptset is the prompt set this provider uses for the
	// fact-extraction phase. Defaults to promptset.Default when the
	// zero value is used; a worker that resolved a per-repo
	// promptset constructs a fresh provider with that promptset via
	// WithPromptset. Only the FactExtraction phase field is read
	// here, but the whole Promptset is carried so the worker can
	// thread the same identity into every phase provider without
	// re-resolving.
	promptset promptset.Promptset
}

// NewAIFactExtractionProvider constructs an AI-backed fact
// extractor that uses the built-in promptset. A worker that needs a
// per-repo promptset calls WithPromptset on the returned provider
// (or constructs a fresh one) to swap in the resolved philosophy.
func NewAIFactExtractionProvider(aiProvider ai.AIProvider, model string) *AIFactExtractionProvider {
	return &AIFactExtractionProvider{
		AIProvider: aiProvider,
		Model:      model,
		promptset:  promptset.Default,
	}
}

// WithPromptset returns a copy of the provider that uses the given
// promptset's FactExtraction phase. The provider is otherwise
// immutable, so this is the only way to swap the active philosophy.
// Used by the source_decomposition worker after it resolves the
// repo's effective promptset hash.
func (p *AIFactExtractionProvider) WithPromptset(ps promptset.Promptset) *AIFactExtractionProvider {
	clone := *p
	clone.promptset = ps
	return &clone
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

	prompt := strings.Replace(p.promptset.FactExtraction, "%s", chunkText, 1)

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

// cleanJSONArray extracts the first valid JSON array from a raw
// model response. The model occasionally wraps the JSON in markdown
// fences (```json ... ```), prefixes it with chain-of-thought
// commentary, appends trailing junk, or emits multiple fenced
// blocks when it "reconsiders" mid-response. The historical
// implementation (first '[' to last ']') failed on the
// multi-block case because it spanned the prose between blocks.
//
// The new behavior: for each '[' position in the raw string, try
// to parse the substring from that '[' to each subsequent ']' as
// JSON. The first pair that parses cleanly wins. This handles:
//   - markdown fences (```json\n[...]\n```): the fence chars are
//     skipped because we start at the JSON's '[';
//   - preambles ("//C ..."): leading text before '[' is skipped;
//   - trailing junk (commentary after the array): text after ']'
//     is excluded;
//   - multiple blocks ("Wait, let me reconsider... ```json
//     [...]```"): the FIRST valid array is returned, ignoring
//     later blocks.
//
// Returns "" when no valid JSON array can be extracted — the caller
// then treats the response as a permanent parse failure (the
// soft-skip path handles it).
func cleanJSONArray(raw string) string {
	raw = strings.TrimSpace(raw)
	// Fast path: the whole response is already a clean array.
	if len(raw) > 0 && raw[0] == '[' {
		var probe []json.RawMessage
		if err := json.Unmarshal([]byte(raw), &probe); err == nil {
			return raw
		}
	}
	// Slow path: scan every '[' and try to parse from there to
	// every later ']'. O(n^2) over the positions, but the response
	// is bounded (typically a few KB) so this is cheap enough.
	for start := 0; start < len(raw); start++ {
		if raw[start] != '[' {
			continue
		}
		for end := len(raw) - 1; end > start; end-- {
			if raw[end] != ']' {
				continue
			}
			candidate := raw[start : end+1]
			var probe []json.RawMessage
			if err := json.Unmarshal([]byte(candidate), &probe); err == nil {
				return candidate
			}
		}
	}
	return ""
}
