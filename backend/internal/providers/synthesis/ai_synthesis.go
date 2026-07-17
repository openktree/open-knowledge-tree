package synthesis

import (
	"context"
	"fmt"
	"strings"

	"github.com/openktree/open-knowledge-tree/backend/internal/providers/ai"
	"github.com/openktree/open-knowledge-tree/backend/internal/store"
)

// AISynthesisProvider is the LLM-backed SynthesisProvider. It wraps
// an ai.AIProvider (the same multi-provider gateway the summarization
// and decomposition extractors use) plus two model ids — one for the
// main synthesis call and one for the image-picker call (which may be
// a cheaper/faster model). It builds the prompts, calls Chat via
// retryWithBackoff, and returns the trimmed markdown body (synthesis)
// or the parsed fact_id list (picker). The provider records token
// usage into okt_system.ai_usage via the ai.Attribution the worker
// passes through.
type AISynthesisProvider struct {
	aiProvider   ai.AIProvider
	model        string // synthesis model id
	pickerModel  string // image-picker model id; defaults to model when empty
}

// NewAISynthesisProvider constructs the provider. aiProvider must be
// non-nil; model is the chat model id the provider sends on every
// Synthesize call (the worker may override per-call via
// SynthesisRequest.Model, but that path is unused today). pickerModel
// is the model id for the PickImages call; when empty, Synthesize's
// model is used.
func NewAISynthesisProvider(aiProvider ai.AIProvider, model, pickerModel string) *AISynthesisProvider {
	return &AISynthesisProvider{aiProvider: aiProvider, model: model, pickerModel: pickerModel}
}

func (p *AISynthesisProvider) Describe() ProviderDescription {
	aiDesc := p.aiProvider.Describe()
	configured := aiDesc.Configured && p.model != ""
	supports := []string{"synthesis"}
	if p.pickerModel != "" {
		supports = append(supports, "image_picking")
	}
	return ProviderDescription{
		Name:        "AI concept synthesizer",
		Description: "Folds a concept's summary slices into one authoritative definition, and picks the most relevant image facts for the definition to embed.",
		Requires:    "providers.synthesis.{enabled,provider,model} and the underlying AI provider's API key",
		Configured:  configured,
		Supports:    supports,
		Notes:       "Provider is " + aiDesc.Name + ". Per-concept failures are logged and the concept is skipped; the synthesis pass still completes.",
		Config: map[string]string{
			"ai_provider":  aiDesc.Name,
			"model":        p.model,
			"picker_model": p.pickerModel,
		},
	}
}

// Synthesize builds the synthesis prompt from req, calls the AI
// provider's Chat, and returns the trimmed markdown body. The db arg
// is passed to the AI provider so it can record token usage into
// ai_usage (mirrors every other AI-backed provider); pass nil to skip
// recording (tests).
//
// The response is treated as plain markdown: any ``` fence wrapper
// the model adds is stripped, then the body is trimmed. There is no
// JSON envelope to parse — the synthesis provider returns a string.
func (p *AISynthesisProvider) Synthesize(ctx context.Context, db store.DBTX, req SynthesisRequest) (string, error) {
	if len(req.Slices) == 0 {
		return "", nil
	}

	model := req.Model
	if model == "" {
		model = p.model
	}

	userMsg := buildSynthesisUserMessage(req)

	var taskID *string
	if req.TaskID != "" {
		taskID = &req.TaskID
	}

	chatReq := ai.ChatRequest{
		Model:    model,
		Messages: []ai.ChatMessage{{Role: "system", Content: synthesisSystemPrompt}, {Role: "user", Content: userMsg}},
		TaskID:   taskID,
		Attribution: ai.Attribution{
			RepositoryID: req.Attribution.RepositoryID,
			SourceID:     req.Attribution.SourceID,
			Operation:    "concept_synthesis",
		},
	}
	if req.MaxTokens > 0 {
		mt := req.MaxTokens
		chatReq.MaxTokens = &mt
	}
	if req.ThinkingLevel != nil {
		chatReq.ThinkingLevel = req.ThinkingLevel
	}

	resp, err := retryWithBackoff(ctx, retryConfig{}, "concept_synthesis",
		func(callCtx context.Context) (ai.ChatResponse, error) {
			return p.aiProvider.Chat(callCtx, db, chatReq)
		},
	)
	if err != nil {
		return "", fmt.Errorf("concept synthesis: ai chat failed: %w", err)
	}
	if len(resp.Messages) == 0 {
		return "", nil
	}
	content := strings.TrimSpace(resp.Messages[0].Content)
	if content == "" {
		return "", nil
	}
	return trimMarkdownFences(content), nil
}

// PickImages builds the image-picker prompt from req, calls the AI
// provider's Chat with the picker model, and returns the parsed
// fact_id list. The response is expected to be one UUID per line (no
// prose); the parser splits on whitespace/newlines, keeps only
// well-formed UUIDs, and deduplicates. The caller (worker) further
// filters to ids that are actually in the candidate set, so a
// hallucinated id is dropped before it reaches the synthesis.
func (p *AISynthesisProvider) PickImages(ctx context.Context, db store.DBTX, req ImagePickRequest) ([]string, error) {
	if len(req.Candidates) == 0 {
		return nil, nil
	}

	model := req.Model
	if model == "" {
		model = p.pickerModel
		if model == "" {
			model = p.model
		}
	}

	userMsg := buildImagePickerUserMessage(req)

	var taskID *string
	if req.TaskID != "" {
		taskID = &req.TaskID
	}

	chatReq := ai.ChatRequest{
		Model:    model,
		Messages: []ai.ChatMessage{{Role: "system", Content: imagePickerSystemPrompt}, {Role: "user", Content: userMsg}},
		TaskID:   taskID,
		Attribution: ai.Attribution{
			RepositoryID: req.Attribution.RepositoryID,
			SourceID:     req.Attribution.SourceID,
			Operation:    "concept_image_picking",
		},
	}
	if req.MaxTokens > 0 {
		mt := req.MaxTokens
		chatReq.MaxTokens = &mt
	}

	resp, err := retryWithBackoff(ctx, retryConfig{}, "concept_image_picking",
		func(callCtx context.Context) (ai.ChatResponse, error) {
			return p.aiProvider.Chat(callCtx, db, chatReq)
		},
	)
	if err != nil {
		return nil, fmt.Errorf("concept image picking: ai chat failed: %w", err)
	}
	if len(resp.Messages) == 0 {
		return nil, nil
	}
	return parseImagePickerResponse(resp.Messages[0].Content, req.MaxImages), nil
}

// parseImagePickerResponse extracts the fact_ids from the picker's
// raw text response. The model is instructed to return one UUID per
// line, but in practice it may wrap them in markdown, add stray
// prose, or comma-separate. The parser:
//   1. Strips markdown bullets and backticks.
//   2. Splits on any whitespace or comma.
//   3. Keeps only tokens that match the canonical UUID shape.
//   4. Deduplicates, preserving order.
//   5. Caps at maxN (0 means no cap).
func parseImagePickerResponse(raw string, maxN int) []string {
	// Strip common markdown wrappers the model adds despite the
	// "no markdown" instruction: bullets (-, *), code fences, and
	// surrounding backticks.
	s := strings.ReplaceAll(raw, "```", "")
	s = strings.ReplaceAll(s, "`", "")
	s = strings.TrimSpace(s)

	// Split on any combination of newlines, commas, and spaces.
	tokens := strings.FieldsFunc(s, func(r rune) bool {
		return r == '\n' || r == '\r' || r == ',' || r == ' ' || r == '\t' || r == ';' || r == '|'
	})

	const hex = "0123456789abcdefABCDEF"
	isUUID := func(t string) bool {
		if len(t) != 36 {
			return false
		}
		for i, c := range t {
			switch i {
			case 8, 13, 18, 23:
				if c != '-' {
					return false
				}
			default:
				if !strings.ContainsRune(hex, c) {
					return false
				}
			}
		}
		return true
	}

	seen := make(map[string]bool, len(tokens))
	out := make([]string, 0, len(tokens))
	for _, t := range tokens {
		t = strings.TrimSpace(t)
		if !isUUID(t) || seen[t] {
			continue
		}
		seen[t] = true
		out = append(out, t)
		if maxN > 0 && len(out) >= maxN {
			break
		}
	}
	return out
}