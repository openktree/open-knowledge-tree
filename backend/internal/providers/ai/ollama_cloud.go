package ai

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/openktree/open-knowledge-tree/backend/internal/config"
	"github.com/openktree/open-knowledge-tree/backend/internal/store"
)

const ollamaCloudURL = "https://ollama.com/api/chat"

type OllamaCloudProvider struct {
	apiKey     string
	httpClient *http.Client
	models     []ModelInfo
}

func NewOllamaCloudProvider(apiKey string, models []ModelInfo) *OllamaCloudProvider {
	return &OllamaCloudProvider{
		apiKey: apiKey,
		httpClient: &http.Client{
			Timeout: 120 * time.Second,
		},
		models: models,
	}
}

func NewOllamaCloudProviderFromConfig(cfg config.OllamaCloudProviderConfig, models []ModelInfo) *OllamaCloudProvider {
	return NewOllamaCloudProvider(cfg.APIKey, models)
}

type ollamaCloudChatRequest struct {
	Model    string          `json:"model"`
	Messages []ollamaMessage `json:"messages"`
	Stream   bool            `json:"stream"`
	Options  *ollamaOptions  `json:"options,omitempty"`
}

type ollamaCloudChatResponse struct {
	Model              string      `json:"model"`
	CreatedAt          string      `json:"created_at"`
	Message            ChatMessage `json:"message"`
	Done               bool        `json:"done"`
	TotalDuration      int64       `json:"total_duration"`
	LoadDuration       int64       `json:"load_duration"`
	PromptEvalCount    int         `json:"prompt_eval_count"`
	PromptEvalDuration int64       `json:"prompt_eval_duration"`
	EvalCount          int         `json:"eval_count"`
	EvalDuration       int64       `json:"eval_duration"`
	DoneReason         string      `json:"done_reason,omitempty"`
}

func (p *OllamaCloudProvider) Chat(ctx context.Context, db store.DBTX, req ChatRequest) (ChatResponse, error) {
	msgs := make([]ollamaMessage, len(req.Messages))
	for i, m := range req.Messages {
		msgs[i] = ollamaMessageFromChat(m)
	}
	ocReq := ollamaCloudChatRequest{
		Model:    req.Model,
		Messages: msgs,
		Stream:   false,
	}
	if req.Temperature != nil || req.MaxTokens != nil {
		ocReq.Options = &ollamaOptions{}
		if req.Temperature != nil {
			ocReq.Options.Temperature = req.Temperature
		}
		if req.MaxTokens != nil {
			ocReq.Options.NumPredict = req.MaxTokens
		}
	}

	body, err := json.Marshal(ocReq)
	if err != nil {
		return ChatResponse{}, fmt.Errorf("ollama_cloud: marshaling request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, ollamaCloudURL, bytes.NewReader(body))
	if err != nil {
		return ChatResponse{}, fmt.Errorf("ollama_cloud: creating request: %w", err)
	}
	httpReq.Header.Set("Authorization", "Bearer "+p.apiKey)
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := p.httpClient.Do(httpReq)
	if err != nil {
		return ChatResponse{}, fmt.Errorf("ollama_cloud: request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return ChatResponse{}, fmt.Errorf("ollama_cloud: status %d: %s", resp.StatusCode, string(b))
	}

	var ocResp ollamaCloudChatResponse
	if err := json.NewDecoder(resp.Body).Decode(&ocResp); err != nil {
		return ChatResponse{}, fmt.Errorf("ollama_cloud: decoding response: %w", err)
	}

	usage := Usage{
		PromptTokens:     ocResp.PromptEvalCount,
		CompletionTokens: ocResp.EvalCount,
		TotalTokens:      ocResp.PromptEvalCount + ocResp.EvalCount,
	}

	if db != nil {
		recordUsage(ctx, db, req.Model, "ollama_cloud", req.TaskID, req.Attribution, usage)
	}

	return ChatResponse{
		Model:        ocResp.Model,
		Messages:     []ChatMessage{ocResp.Message},
		Usage:        usage,
		FinishReason: ocResp.DoneReason,
	}, nil
}

func (p *OllamaCloudProvider) Describe() ProviderDescription {
	return ProviderDescription{
		Name:        "Ollama Cloud",
		Description: "Hosted Ollama API. Run open-source models on Ollama's cloud infrastructure via the native Ollama chat API.",
		Requires:    "OLLAMA_API_KEY or providers.ai.ollama_cloud.api_key",
		Configured:  true,
		Models:      p.models,
		Timeout:     "120s",
		Notes:       "Uses the native Ollama /api/chat endpoint. Models are configured in providers.ai.models.",
	}
}
