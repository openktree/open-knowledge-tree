package ai

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/openktree/open-knowledge-tree/backend/internal/config"
	"github.com/openktree/open-knowledge-tree/backend/internal/store"
)

const ollamaDefaultBaseURL = "http://localhost:11434"

type OllamaProvider struct {
	baseURL    string
	httpClient *http.Client
	models     []ModelInfo
}

func NewOllamaProvider(baseURL string, models []ModelInfo) *OllamaProvider {
	if baseURL == "" {
		baseURL = ollamaDefaultBaseURL
	}
	return &OllamaProvider{
		baseURL: baseURL,
		httpClient: &http.Client{
			Timeout: 120 * time.Second,
		},
		models: models,
	}
}

func NewOllamaProviderFromConfig(cfg config.OllamaProviderConfig, models []ModelInfo) *OllamaProvider {
	return NewOllamaProvider(cfg.BaseURL, models)
}

type ollamaChatRequest struct {
	Model    string          `json:"model"`
	Messages []ollamaMessage `json:"messages"`
	Stream   bool           `json:"stream"`
	Options  *ollamaOptions  `json:"options,omitempty"`
}

// ollamaMessage is the Ollama-native message shape. Ollama does NOT
// use the OpenAI content-parts array for multimodal input; instead
// it carries text in `content` and base64-encoded image bytes in a
// sibling `images` array. ollamaMessageFromChat translates a
// ChatMessage (which may be text-only or multimodal with Parts) into
// this shape so the Ollama providers can reuse the shared ChatMessage
// type without leaking the Ollama wire format into callers.
type ollamaMessage struct {
	Role    string   `json:"role"`
	Content string   `json:"content"`
	Images  []string `json:"images,omitempty"`
}

// ollamaMessageFromChat converts a ChatMessage into Ollama's native
// shape. Text-only messages map 1:1 (Content → content, no images).
// Multimodal messages concatenate the text parts into content and
// emit one base64 entry per image part (the raw bytes, without the
// data: prefix — Ollama expects bare base64). Image parts whose
// ImageURL.URL is not a data URL are skipped (Ollama does not fetch
// remote URLs).
func ollamaMessageFromChat(m ChatMessage) ollamaMessage {
	if !m.IsMultimodal() {
		return ollamaMessage{Role: m.Role, Content: m.Content}
	}
	om := ollamaMessage{Role: m.Role}
	for _, p := range m.Parts {
		switch p.Type {
		case "text":
			if om.Content != "" {
				om.Content += "\n"
			}
			om.Content += p.Text
		case "image_url":
			if p.ImageURL == nil {
				continue
			}
			b64 := dataURLBase64(p.ImageURL.URL)
			if b64 != "" {
				om.Images = append(om.Images, b64)
			}
		}
	}
	return om
}

// dataURLBase64 extracts the bare base64 payload from a data URL
// (data:<ct>;base64,<payload>). Returns "" when the input is not a
// data URL so the caller can skip non-fetchable image references.
func dataURLBase64(dataURL string) string {
	const prefix = ";base64,"
	idx := strings.Index(dataURL, prefix)
	if idx < 0 {
		return ""
	}
	return dataURL[idx+len(prefix):]
}

type ollamaOptions struct {
	Temperature *float64 `json:"temperature,omitempty"`
	NumPredict  *int     `json:"num_predict,omitempty"`
}

type ollamaChatResponse struct {
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

func (p *OllamaProvider) Chat(ctx context.Context, db store.DBTX, req ChatRequest) (ChatResponse, error) {
	msgs := make([]ollamaMessage, len(req.Messages))
	for i, m := range req.Messages {
		msgs[i] = ollamaMessageFromChat(m)
	}
	ollamaReq := ollamaChatRequest{
		Model:    req.Model,
		Messages: msgs,
		Stream:   false,
	}
	if req.Temperature != nil || req.MaxTokens != nil {
		ollamaReq.Options = &ollamaOptions{}
		if req.Temperature != nil {
			ollamaReq.Options.Temperature = req.Temperature
		}
		if req.MaxTokens != nil {
			ollamaReq.Options.NumPredict = req.MaxTokens
		}
	}

	body, err := json.Marshal(ollamaReq)
	if err != nil {
		return ChatResponse{}, fmt.Errorf("ollama: marshaling request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.baseURL+"/api/chat", bytes.NewReader(body))
	if err != nil {
		return ChatResponse{}, fmt.Errorf("ollama: creating request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := p.httpClient.Do(httpReq)
	if err != nil {
		return ChatResponse{}, fmt.Errorf("ollama: request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return ChatResponse{}, fmt.Errorf("ollama: status %d: %s", resp.StatusCode, string(b))
	}

	var ollamaResp ollamaChatResponse
	if err := json.NewDecoder(resp.Body).Decode(&ollamaResp); err != nil {
		return ChatResponse{}, fmt.Errorf("ollama: decoding response: %w", err)
	}

	usage := Usage{
		PromptTokens:     ollamaResp.PromptEvalCount,
		CompletionTokens: ollamaResp.EvalCount,
		TotalTokens:      ollamaResp.PromptEvalCount + ollamaResp.EvalCount,
	}

	if db != nil {
		recordUsage(ctx, db, req.Model, "ollama", req.TaskID, req.Attribution, usage)
	}

	return ChatResponse{
		Model:        ollamaResp.Model,
		Messages:     []ChatMessage{ollamaResp.Message},
		Usage:        usage,
		FinishReason: ollamaResp.DoneReason,
	}, nil
}

func (p *OllamaProvider) Describe() ProviderDescription {
	return ProviderDescription{
		Name:        "Ollama",
		Description: "Local LLM inference via Ollama. Supports any model pulled into the local Ollama instance.",
		Requires:    "OLLAMA_BASE_URL or providers.ai.ollama.base_url",
		Configured:  true,
		Models:      p.models,
		Timeout:     "120s",
		Notes:       "Base URL defaults to http://localhost:11434. Models are discovered at runtime via the Ollama API.",
	}
}

// ollamaEmbedRequest is the body for Ollama POST /api/embed.
// `Input` accepts a single string or an array of strings; we send
// an array so one request embeds the whole batch.
type ollamaEmbedRequest struct {
	Model string   `json:"model"`
	Input []string `json:"input"`
}

type ollamaEmbedResponse struct {
	Model      string      `json:"model"`
	Embeddings [][]float32 `json:"embeddings"`
	// Ollama reports per-request prompt token counts under
	// prompt_eval_count (single value, not per-input). Total
	// duration / load duration mirror the chat response.
	PromptEvalCount int `json:"prompt_eval_count"`
}

// Embed calls Ollama POST /api/embed with the batch of inputs and
// returns the vectors in order. The Ollama embeddings endpoint
// accepts an array of inputs in a single request, so we send the
// whole batch in one round-trip. Token usage is recorded into
// okt_system.ai_usage when `db` is non-nil, mirroring Chat.
func (p *OllamaProvider) Embed(ctx context.Context, db store.DBTX, req EmbeddingRequest) (EmbeddingResponse, error) {
	ollamaReq := ollamaEmbedRequest{
		Model: req.Model,
		Input: req.Inputs,
	}
	body, err := json.Marshal(ollamaReq)
	if err != nil {
		return EmbeddingResponse{}, fmt.Errorf("ollama: marshaling embed request: %w", err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.baseURL+"/api/embed", bytes.NewReader(body))
	if err != nil {
		return EmbeddingResponse{}, fmt.Errorf("ollama: creating embed request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := p.httpClient.Do(httpReq)
	if err != nil {
		return EmbeddingResponse{}, fmt.Errorf("ollama: embed request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return EmbeddingResponse{}, fmt.Errorf("ollama: embed status %d: %s", resp.StatusCode, string(b))
	}

	var ollamaResp ollamaEmbedResponse
	if err := json.NewDecoder(resp.Body).Decode(&ollamaResp); err != nil {
		return EmbeddingResponse{}, fmt.Errorf("ollama: decoding embed response: %w", err)
	}

	usage := EmbeddingUsage{
		PromptTokens: ollamaResp.PromptEvalCount,
		TotalTokens:  ollamaResp.PromptEvalCount,
	}
	if db != nil {
		recordEmbeddingUsage(ctx, db, req.Model, "ollama", req.TaskID, req.Attribution, usage)
	}
	return EmbeddingResponse{
		Model:      ollamaResp.Model,
		Embeddings: ollamaResp.Embeddings,
		Usage:      usage,
	}, nil
}
