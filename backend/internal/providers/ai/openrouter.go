package ai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/openktree/open-knowledge-tree/backend/internal/config"
	"github.com/openktree/open-knowledge-tree/backend/internal/store"
)

const openrouterURL = "https://openrouter.ai/api/v1/chat/completions"

type OpenRouterProvider struct {
	apiKey     string
	httpClient *http.Client
	models     []ModelInfo
	// embedBatchSize is the max number of inputs sent in a single
	// POST /v1/embeddings request. OpenRouter (and the OpenAI-
	// compatible endpoint it proxies) rejects large batches — both
	// input-count and total-token limits apply — returning an
	// empty `data` array. Chunking client-side keeps each request
	// within the provider's limits; the results are concatenated
	// in input order. 32 is a conservative default that stays well
	// under OpenAI's 2048-input hard cap while keeping the per-
	// request token count reasonable for 3072-dim models. When the
	// provider still returns an empty `data` array at this size,
	// embedBatchRecursive halves the batch and retries, down to a
	// single input — so the pipeline self-heals instead of dying
	// on the first oversized batch.
	embedBatchSize int
}

const defaultOpenRouterEmbedBatchSize = 32

func NewOpenRouterProvider(apiKey string, models []ModelInfo) *OpenRouterProvider {
	return &OpenRouterProvider{
		apiKey:          apiKey,
		httpClient:      &http.Client{Timeout: 120 * time.Second},
		models:          models,
		embedBatchSize:  defaultOpenRouterEmbedBatchSize,
	}
}

// WithEmbedBatchSize returns a copy of the provider with the embed
// batch size overridden. Values <=0 fall back to the default. This is
// the hook for config-driven tuning when a configured embedding model
// rejects the default 64-input batch with an empty `data` array.
func (p *OpenRouterProvider) WithEmbedBatchSize(n int) *OpenRouterProvider {
	if n <= 0 {
		n = defaultOpenRouterEmbedBatchSize
	}
	p.embedBatchSize = n
	return p
}

func NewOpenRouterProviderFromConfig(cfg config.OpenRouterProviderConfig, models []ModelInfo) *OpenRouterProvider {
	return NewOpenRouterProvider(cfg.APIKey, models).WithEmbedBatchSize(cfg.EmbedBatchSize)
}

type openRouterRequest struct {
	Model           string        `json:"model"`
	Messages        []ChatMessage `json:"messages"`
	Temperature     *float64      `json:"temperature,omitempty"`
	MaxTokens       *int          `json:"max_tokens,omitempty"`
	ReasoningEffort *string       `json:"reasoning_effort,omitempty"`
}

type openRouterResponse struct {
	ID      string `json:"id"`
	Model   string `json:"model"`
	Choices []struct {
		Message      ChatMessage `json:"message"`
		FinishReason string      `json:"finish_reason"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
		TotalTokens      int `json:"total_tokens"`
	} `json:"usage"`
}

func (p *OpenRouterProvider) Chat(ctx context.Context, db store.DBTX, req ChatRequest) (ChatResponse, error) {
	orReq := openRouterRequest{
		Model:       req.Model,
		Messages:    req.Messages,
		Temperature: req.Temperature,
		MaxTokens:   req.MaxTokens,
	}
	if req.ThinkingLevel != nil {
		orReq.ReasoningEffort = req.ThinkingLevel
	}

	body, err := json.Marshal(orReq)
	if err != nil {
		return ChatResponse{}, fmt.Errorf("openrouter: marshaling request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, openrouterURL, bytes.NewReader(body))
	if err != nil {
		return ChatResponse{}, fmt.Errorf("openrouter: creating request: %w", err)
	}
	httpReq.Header.Set("Authorization", "Bearer "+p.apiKey)
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := p.httpClient.Do(httpReq)
	if err != nil {
		return ChatResponse{}, fmt.Errorf("openrouter: request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return ChatResponse{}, fmt.Errorf("openrouter: status %d: %s", resp.StatusCode, string(b))
	}

	var orResp openRouterResponse
	if err := json.NewDecoder(resp.Body).Decode(&orResp); err != nil {
		return ChatResponse{}, fmt.Errorf("openrouter: decoding response: %w", err)
	}

	if len(orResp.Choices) == 0 {
		return ChatResponse{}, fmt.Errorf("openrouter: no choices in response")
	}

	usage := Usage{
		PromptTokens:     orResp.Usage.PromptTokens,
		CompletionTokens: orResp.Usage.CompletionTokens,
		TotalTokens:      orResp.Usage.TotalTokens,
	}

	if db != nil {
		recordUsage(ctx, db, req.Model, "openrouter", req.TaskID, req.Attribution, usage)
	}

	return ChatResponse{
		Model:        orResp.Model,
		Messages:     []ChatMessage{orResp.Choices[0].Message},
		Usage:        usage,
		FinishReason: orResp.Choices[0].FinishReason,
	}, nil
}

func (p *OpenRouterProvider) Describe() ProviderDescription {
	return ProviderDescription{
		Name:        "OpenRouter",
		Description: "Multi-provider LLM gateway. Routes to hundreds of models from OpenAI, Anthropic, Google, Meta, and others through a single API.",
		Requires:    "OPENROUTER_API_KEY or providers.ai.openrouter.api_key",
		Configured:  true,
		Models:      p.models,
		Timeout:     "120s",
		Notes:       "Models are configured in providers.ai.models. Supports reasoning_effort for thinking models.",
	}
}

const openrouterEmbeddingsURL = "https://openrouter.ai/api/v1/embeddings"

// openRouterEmbedRequest is the OpenAI-compatible body for
// OpenRouter POST /api/v1/embeddings. `Input` is an array of
// strings to embed in one request.
type openRouterEmbedRequest struct {
	Model string   `json:"model"`
	Input []string `json:"input"`
}

type openRouterEmbedResponse struct {
	Model string `json:"model"`
	Data  []struct {
		Index     int       `json:"index"`
		Embedding []float32 `json:"embedding"`
	} `json:"data"`
	Usage struct {
		PromptTokens int `json:"prompt_tokens"`
		TotalTokens  int `json:"total_tokens"`
	} `json:"usage"`
	// OpenRouter wraps upstream 4xx errors in a 200 response
	// with an `error` object instead of a `data` array. When this
	// field is non-nil the call failed even though the HTTP
	// status was 200.
	Error *struct {
		Message string `json:"message"`
		Code    int    `json:"code"`
	} `json:"error"`
}

// Embed calls OpenRouter POST /api/v1/embeddings (OpenAI-compatible
// shape: `model` + `input` array, response `data[].embedding`).
// Inputs are chunked into batches of embedBatchSize (default 32)
// before posting: OpenRouter rejects large single requests with an
// empty `data` array (the input-count / total-token limits vary by
// underlying model), so client-side batching is required to embed a
// full source's fact set. Each batch is a separate HTTP request;
// the per-batch responses are concatenated in input order so callers
// get one vector per input as if the request were single-shot. Token
// usage is summed across batches and recorded into ai_usage when
// `db` is non-nil.
//
// When a batch comes back with an empty `data` array (the provider's
// signal that the batch exceeded the model's input-count or total-
// token limit), embedBatchRecursive halves the batch and retries
// each half independently, down to a single input. This makes the
// pipeline self-healing: the configured embedBatchSize is a starting
// point, not a hard requirement, so a tight model limit on one
// chunk of long facts no longer kills the whole embed_facts job.
func (p *OpenRouterProvider) Embed(ctx context.Context, db store.DBTX, req EmbeddingRequest) (EmbeddingResponse, error) {
	if len(req.Inputs) == 0 {
		return EmbeddingResponse{Model: req.Model}, nil
	}

	batchSize := p.embedBatchSize
	if batchSize <= 0 {
		batchSize = defaultOpenRouterEmbedBatchSize
	}

	var (
		embeddings [][]float32
		usage      EmbeddingUsage
		model      string
	)
	for start := 0; start < len(req.Inputs); start += batchSize {
		end := start + batchSize
		if end > len(req.Inputs) {
			end = len(req.Inputs)
		}
		batch := req.Inputs[start:end]

		batchResp, err := p.embedBatchRecursive(ctx, req.Model, batch)
		if err != nil {
			return EmbeddingResponse{}, fmt.Errorf("openrouter: embedding batch [%d:%d]: %w", start, end, err)
		}
		if model == "" {
			model = batchResp.Model
		}
		embeddings = append(embeddings, batchResp.Embeddings...)
		usage.PromptTokens += batchResp.Usage.PromptTokens
		usage.TotalTokens += batchResp.Usage.TotalTokens
	}

	if db != nil {
		recordEmbeddingUsage(ctx, db, req.Model, "openrouter", req.TaskID, req.Attribution, usage)
	}
	return EmbeddingResponse{
		Model:      model,
		Embeddings: embeddings,
		Usage:      usage,
	}, nil
}

// errOpenRouterEmbedNoData is the sentinel returned by embedBatch
// when OpenRouter responds 200 OK with an empty `data` array — the
// provider's signal that the batch exceeded the underlying model's
// input-count or total-token limit. embedBatchRecursive matches on
// this to decide whether to halve and retry or surface the error.
var errOpenRouterEmbedNoData = errors.New("openrouter: embed response has no data")

// embedBatchRecursive sends inputs as a single POST /v1/embeddings
// request. When OpenRouter returns an empty `data` array (the
// errOpenRouterEmbedNoData sentinel) and the batch has more than one
// input, it halves the batch and recurses on each half, concatenating
// the results in input order. A single-input batch that still returns
// no data is a real error (the input itself is too large for the
// model, or a transient provider glitch) and is returned to the
// caller. Recursion depth is bounded by log2(batchSize) — for the
// default 32 that's at most 5 levels, so this is safe.
func (p *OpenRouterProvider) embedBatchRecursive(ctx context.Context, model string, inputs []string) (struct {
	Model      string
	Embeddings [][]float32
	Usage      EmbeddingUsage
}, error) {
	resp, err := p.embedBatch(ctx, model, inputs)
	if err == nil {
		return resp, nil
	}
	// Only the no-data sentinel is halvable; HTTP / decode / count
	// mismatch errors are surfaced immediately.
	if !errors.Is(err, errOpenRouterEmbedNoData) {
		return resp, err
	}
	// Single input that still returned no data: can't halve further.
	if len(inputs) <= 1 {
		return resp, fmt.Errorf("%w (batch of %d inputs; reduce embed_batch_size)", errOpenRouterEmbedNoData, len(inputs))
	}
	mid := len(inputs) / 2
	left, err := p.embedBatchRecursive(ctx, model, inputs[:mid])
	if err != nil {
		return left, err
	}
	right, err := p.embedBatchRecursive(ctx, model, inputs[mid:])
	if err != nil {
		return right, err
	}
	left.Embeddings = append(left.Embeddings, right.Embeddings...)
	left.Usage.PromptTokens += right.Usage.PromptTokens
	left.Usage.TotalTokens += right.Usage.TotalTokens
	return left, nil
}

// embedBatch is the single-request embedding call against
// POST /v1/embeddings. It sends one batch of inputs (already
// chunked by the caller) and returns the vectors in input order.
// The response's `data[].index` is trusted to match the batch
// order; when the server omits indices or returns them out of
// order we reorder by index defensively.
func (p *OpenRouterProvider) embedBatch(ctx context.Context, model string, inputs []string) (struct {
	Model      string
	Embeddings [][]float32
	Usage      EmbeddingUsage
}, error) {
	// OpenRouter (and the underlying Gemini embedding model) reject
	// empty-string inputs with "EmbedContentRequest.content contains
	// an empty Part". Replace any empty / whitespace-only input with
	// a single space so the request succeeds; the resulting vector
	// is a valid (if uninformative) embedding for that fact.
	sanitized := make([]string, len(inputs))
	for i, in := range inputs {
		if strings.TrimSpace(in) == "" {
			sanitized[i] = " "
		} else {
			sanitized[i] = in
		}
	}
	orReq := openRouterEmbedRequest{
		Model: model,
		Input: sanitized,
	}
	body, err := json.Marshal(orReq)
	if err != nil {
		return struct {
			Model      string
			Embeddings [][]float32
			Usage      EmbeddingUsage
		}{}, fmt.Errorf("openrouter: marshaling embed request: %w", err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, openrouterEmbeddingsURL, bytes.NewReader(body))
	if err != nil {
		return struct {
			Model      string
			Embeddings [][]float32
			Usage      EmbeddingUsage
		}{}, fmt.Errorf("openrouter: creating embed request: %w", err)
	}
	httpReq.Header.Set("Authorization", "Bearer "+p.apiKey)
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := p.httpClient.Do(httpReq)
	if err != nil {
		return struct {
			Model      string
			Embeddings [][]float32
			Usage      EmbeddingUsage
		}{}, fmt.Errorf("openrouter: embed request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return struct {
			Model      string
			Embeddings [][]float32
			Usage      EmbeddingUsage
		}{}, fmt.Errorf("openrouter: embed status %d: %s", resp.StatusCode, string(b))
	}

	rawBody, _ := io.ReadAll(resp.Body)
	var orResp openRouterEmbedResponse
	if err := json.Unmarshal(rawBody, &orResp); err != nil {
		return struct {
			Model      string
			Embeddings [][]float32
			Usage      EmbeddingUsage
		}{}, fmt.Errorf("openrouter: decoding embed response: %w (raw: %s)", err, truncate(string(rawBody), 500))
	}
	if orResp.Error != nil {
		return struct {
			Model      string
			Embeddings [][]float32
			Usage      EmbeddingUsage
		}{}, fmt.Errorf("openrouter: embed upstream error (code %d): %s", orResp.Error.Code, orResp.Error.Message)
	}
	if len(orResp.Data) == 0 {
		log.Printf("openrouter: embed empty data debug: status=%d, inputs=%d, content-type=%s, raw_body=%s", resp.StatusCode, len(inputs), resp.Header.Get("Content-Type"), truncate(string(rawBody), 500))
		return struct {
			Model      string
			Embeddings [][]float32
			Usage      EmbeddingUsage
		}{}, fmt.Errorf("%w (batch of %d inputs)", errOpenRouterEmbedNoData, len(inputs))
	}
	if len(orResp.Data) != len(inputs) {
		return struct {
			Model      string
			Embeddings [][]float32
			Usage      EmbeddingUsage
		}{}, fmt.Errorf("openrouter: embed response returned %d vectors for %d inputs", len(orResp.Data), len(inputs))
	}

	// Reorder by index in case the server shuffled the response
	// (it shouldn't, but the contract allows it).
	embeddings := make([][]float32, len(orResp.Data))
	for _, d := range orResp.Data {
		if d.Index < 0 || d.Index >= len(embeddings) {
			return struct {
				Model      string
				Embeddings [][]float32
				Usage      EmbeddingUsage
			}{}, fmt.Errorf("openrouter: embed response index %d out of range", d.Index)
		}
		embeddings[d.Index] = d.Embedding
	}

	return struct {
		Model      string
		Embeddings [][]float32
		Usage      EmbeddingUsage
	}{
		Model:      orResp.Model,
		Embeddings: embeddings,
		Usage: EmbeddingUsage{
			PromptTokens: orResp.Usage.PromptTokens,
			TotalTokens:  orResp.Usage.TotalTokens,
		},
	}, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
