package refinement

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/openktree/open-knowledge-tree/backend/internal/providers/ai"
	"github.com/openktree/open-knowledge-tree/backend/internal/store"
)

// AIRefineProvider is the LLM-backed RefineProvider. It wraps an
// ai.AIProvider (the same multi-provider gateway the summarization
// and decomposition extractors use) plus a model id, builds the
// refinement prompt, calls Chat via retryWithBackoff, and returns the
// parsed RefinementResult. The provider records token usage into
// okt_system.ai_usage via the ai.Attribution the worker passes
// through.
type AIRefineProvider struct {
	aiProvider ai.AIProvider
	model       string
}

// NewAIRefineProvider constructs the provider. aiProvider must be
// non-nil; model is the chat model id the provider will send on
// every Refine call (the worker may override per-call via
// RefinementRequest.Model, but that path is unused today).
func NewAIRefineProvider(aiProvider ai.AIProvider, model string) *AIRefineProvider {
	return &AIRefineProvider{aiProvider: aiProvider, model: model}
}

func (p *AIRefineProvider) Describe() ProviderDescription {
	aiDesc := p.aiProvider.Describe()
	configured := aiDesc.Configured && p.model != ""
	return ProviderDescription{
		Name:        "AI concept refiner",
		Description: "Asks a configured chat model to propose the full formal canonical name and known aliases for a (concept, context) pair, plus aliases to prune. Runs once per unresolved candidate concept.",
		Requires:    "providers.refinement.{provider,model} and the underlying AI provider's API key",
		Configured:  configured,
		Supports:    []string{"refinement"},
		Notes:       "Provider is " + aiDesc.Name + ". Runs only for unresolved candidates (resolved candidates route via cache; matched candidates route via pre-LLM DB queries).",
		Config: map[string]string{
			"ai_provider": aiDesc.Name,
			"model":       p.model,
		},
	}
}

// Refine builds the prompt from req, calls the AI provider's Chat,
// and returns the parsed RefinementResult. The db arg is passed to
// the AI provider so it can record token usage into ai_usage (mirrors
// every other AI-backed provider); pass nil to skip recording
// (tests).
func (p *AIRefineProvider) Refine(ctx context.Context, db store.DBTX, req RefinementRequest) (RefinementResult, error) {
	if strings.TrimSpace(req.Concept) == "" {
		return RefinementResult{}, fmt.Errorf("refinement: concept is required")
	}

	userMsg := buildUserMessage(req.Concept, req.Context, req.ExistingAliases, req.SeedAliases)

	model := req.Model
	if model == "" {
		model = p.model
	}

	var taskID *string
	if req.TaskID != "" {
		taskID = &req.TaskID
	}

	chatReq := ai.ChatRequest{
		Model:    model,
		Messages: []ai.ChatMessage{{Role: "user", Content: userMsg}},
		TaskID:   taskID,
		Attribution: ai.Attribution{
			RepositoryID: req.Attribution.RepositoryID,
			SourceID:     req.Attribution.SourceID,
			Operation:    "alias_generation",
		},
	}
	if req.MaxTokens > 0 {
		mt := req.MaxTokens
		chatReq.MaxTokens = &mt
	}

	resp, err := retryWithBackoff(ctx, retryConfig{}, "concept_refinement",
		func(callCtx context.Context) (ai.ChatResponse, error) {
			return p.aiProvider.Chat(callCtx, db, chatReq)
		},
	)
	if err != nil {
		return RefinementResult{}, fmt.Errorf("refinement: ai chat failed: %w", err)
	}

	if len(resp.Messages) == 0 {
		return RefinementResult{}, fmt.Errorf("refinement: empty response from model")
	}
	content := strings.TrimSpace(resp.Messages[0].Content)
	if content == "" {
		return RefinementResult{}, fmt.Errorf("refinement: empty content from model")
	}

	cleaned := cleanJSONObject(content)
	if cleaned == "" {
		return RefinementResult{}, fmt.Errorf("refinement: no JSON object in response (raw: %s)", content)
	}
	var result RefinementResult
	if err := json.Unmarshal([]byte(cleaned), &result); err != nil {
		return RefinementResult{}, fmt.Errorf("refinement: failed to parse response as JSON object: %w (raw %s)", err, content)
	}
	if strings.TrimSpace(result.CanonicalName) == "" {
		result.CanonicalName = req.Concept
	}
	return result, nil
}