package summarization

import (
	"context"
	"fmt"
	"strings"

	"github.com/openktree/open-knowledge-tree/backend/internal/providers/ai"
	"github.com/openktree/open-knowledge-tree/backend/internal/store"
)

// AISummarizationProvider is the LLM-backed SummarizationProvider.
// It wraps an ai.AIProvider (the same multi-provider gateway the
// decomposition extractors use) plus a model id, builds the
// summarization prompt, calls Chat via retryWithBackoff, and
// returns the trimmed markdown body. The provider records token
// usage into okt_system.ai_usage via the ai.Attribution the worker
// passes through.
type AISummarizationProvider struct {
	aiProvider ai.AIProvider
	model      string
}

// NewAISummarizationProvider constructs the provider. The aiProvider
// must be non-nil; the model is the chat model id the provider will
// send on every Summarize call (the worker may override it per-call
// via SummarizationRequest.Model, but that path is unused today).
func NewAISummarizationProvider(aiProvider ai.AIProvider, model string) *AISummarizationProvider {
	return &AISummarizationProvider{aiProvider: aiProvider, model: model}
}

func (p *AISummarizationProvider) Describe() ProviderDescription {
	aiDesc := p.aiProvider.Describe()
	configured := aiDesc.Configured && p.model != ""
	return ProviderDescription{
		Name:        "AI concept summarizer",
		Description: "Asks a configured chat model to summarize the facts linked to a (concept, context) pair into a credulous markdown summary that reflects every detail and perspective, citing central/key facts via [text](<fact_id>) links.",
		Requires:    "providers.summarization.{enabled,provider,model} and the underlying AI provider's API key",
		Configured:  configured,
		Supports:    []string{"summarization"},
		Notes:       "Provider is " + aiDesc.Name + ". Per-concept failures are logged and the concept is skipped; the summarization pass still completes.",
		Config: map[string]string{
			"ai_provider": aiDesc.Name,
			"model":       p.model,
		},
	}
}

// Summarize builds the prompt from req, calls the AI provider's
// Chat, and returns the trimmed markdown body. The db arg is passed
// to the AI provider so it can record token usage into ai_usage
// (mirrors every other AI-backed provider); pass nil to skip
// recording (tests).
//
// The response is treated as plain markdown: any ``` fence wrapper
// the model adds is stripped, then the body is trimmed. There is no
// JSON envelope to parse — the summarization provider returns a
// string, not a structured object.
func (p *AISummarizationProvider) Summarize(ctx context.Context, db store.DBTX, req SummarizationRequest) (string, error) {
	if len(req.Facts) == 0 {
		return "", nil
	}

	model := req.Model
	if model == "" {
		model = p.model
	}

	userMsg := buildUserMessage(req)

	var taskID *string
	if req.TaskID != "" {
		taskID = &req.TaskID
	}

	chatReq := ai.ChatRequest{
		Model:    model,
		Messages: []ai.ChatMessage{{Role: "system", Content: buildSystemPrompt(req.MaxTokens)}, {Role: "user", Content: userMsg}},
		TaskID:   taskID,
		Attribution: ai.Attribution{
			RepositoryID: req.Attribution.RepositoryID,
			SourceID:     req.Attribution.SourceID,
			Operation:    "concept_summarization",
		},
	}
	if req.MaxTokens > 0 {
		mt := req.MaxTokens
		chatReq.MaxTokens = &mt
	}

	resp, err := retryWithBackoff(ctx, retryConfig{}, "concept_summarization",
		func(callCtx context.Context) (ai.ChatResponse, error) {
			return p.aiProvider.Chat(callCtx, db, chatReq)
		},
	)
	if err != nil {
		return "", fmt.Errorf("concept summarization: ai chat failed: %w", err)
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
