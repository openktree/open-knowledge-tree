package ai

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"

	"github.com/openktree/open-knowledge-tree/backend/internal/config"
	"github.com/openktree/open-knowledge-tree/backend/internal/store"
)

// ChatMessage is one message in a chat conversation. It supports
// both the plain-text shape (Role + Content, the original fast path
// used by every text-only caller) and the multimodal shape
// (Role + Parts, used by the image fact extractor and any future
// vision caller).
//
// Wire shape (OpenAI-compatible, used by OpenRouter and most model
// providers):
//
//   - text-only:    {"role": "...", "content": "the text"}
//   - multimodal:   {"role": "...", "content": [
//                       {"type": "text", "text": "..."},
//                       {"type": "image_url", "image_url": {"url": "data:image/png;base64,..."}}
//                   ]}
//
// The MarshalJSON / UnmarshalJSON implementations below map between
// the Go struct and this wire shape. The Go struct keeps Content and
// Parts as separate exported fields so text-only callers stay
// simple (`ChatMessage{Role: "user", Content: "..."}`) and multimodal
// callers use NewImageMessage. The json tags are "-" so the custom
// marshaler is the only path.
//
// Ollama's native API uses a sibling `images` field instead of
// content parts; the Ollama providers translate Parts into that
// shape locally (see ollama.go / ollama_cloud.go).
type ChatMessage struct {
	Role    string        `json:"-"`
	Content string        `json:"-"`
	Parts   []ContentPart `json:"-"`
}

// IsMultimodal reports whether the message carries image parts.
// Providers use it to decide between the text-only and multimodal
// request body shapes.
func (m ChatMessage) IsMultimodal() bool { return len(m.Parts) > 0 }

// ContentPart is one element of a multimodal message's content
// array. Type is "text" or "image_url". For "text", Text holds the
// text. For "image_url", ImageURL.URL holds a data URL
// (data:<content-type>;base64,<base64-bytes>) — base64 keeps the
// payload self-contained so it works against providers that do not
// fetch remote URLs and avoids leaking the image URL to the model
// (the source URL is passed as text context instead).
type ContentPart struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	ImageURL *struct {
		URL string `json:"url"`
	} `json:"image_url,omitempty"`
}

// ImageData is one image to embed in a multimodal message. Bytes
// is the raw image payload (PNG, JPEG, etc.); ContentType is the
// MIME type (e.g. "image/png"). NewImageMessage base64-encodes the
// bytes into a data URL so the wire payload is self-contained.
type ImageData struct {
	Bytes       []byte
	ContentType string
}

// NewImageMessage builds a multimodal user message with one text
// part followed by one image part per supplied image. The text part
// carries the prompt; each image part is a base64 data URL so the
// payload works against any OpenAI-compatible vision endpoint
// without the provider having to fetch the image. Callers that only
// need text should use ChatMessage{Role: ..., Content: ...}
// directly.
func NewImageMessage(role, text string, images []ImageData) ChatMessage {
	parts := make([]ContentPart, 0, 1+len(images))
	parts = append(parts, ContentPart{Type: "text", Text: text})
	for _, img := range images {
		if len(img.Bytes) == 0 || img.ContentType == "" {
			continue
		}
		b64 := base64.StdEncoding.EncodeToString(img.Bytes)
		parts = append(parts, ContentPart{
			Type:     "image_url",
			ImageURL: &struct {
				URL string `json:"url"`
			}{
				URL: "data:" + img.ContentType + ";base64," + b64,
			},
		})
	}
	return ChatMessage{Role: role, Parts: parts}
}

// MarshalJSON implements the OpenAI-compatible wire shape: a string
// `content` for text-only messages, or an array of content parts for
// multimodal messages.
func (m ChatMessage) MarshalJSON() ([]byte, error) {
	if len(m.Parts) == 0 {
		return json.Marshal(struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		}{Role: m.Role, Content: m.Content})
	}
	return json.Marshal(struct {
		Role  string        `json:"role"`
		Parts []ContentPart `json:"content"`
	}{Role: m.Role, Parts: m.Parts})
}

// UnmarshalJSON accepts both the string-content shape (from model
// responses, which are always text) and the parts-array shape (for
// round-tripping a multimodal message). The text path is the common
// one; the array path exists so a caller could decode a multimodal
// request it built earlier.
func (m *ChatMessage) UnmarshalJSON(data []byte) error {
	var raw struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	m.Role = raw.Role
	if len(raw.Content) == 0 {
		return nil
	}
	// String content → text-only message.
	var s string
	if err := json.Unmarshal(raw.Content, &s); err == nil {
		m.Content = s
		return nil
	}
	// Array content → multimodal message.
	var parts []ContentPart
	if err := json.Unmarshal(raw.Content, &parts); err != nil {
		return fmt.Errorf("ai.ChatMessage: content is neither string nor parts array: %w", err)
	}
	m.Parts = parts
	return nil
}

// Attribution bundles the per-call context that gets recorded
// into okt_system.ai_usage alongside the token counts. Every
// field is optional — interactive chat may carry none, a
// background task carries all of them. Providers pass the
// whole struct through to recordUsage / recordEmbeddingUsage so
// the tracking helper writes one row with full attribution.
//
// RepositoryID / SourceID are UUID strings (canonical form); the
// tracking helper scans them into pgtype.UUID. Operation is one
// of "chat", "fact_extraction", "embedding" (an empty Operation
// defaults to "chat" at the recordUsage site and "embedding" at
// the recordEmbeddingUsage site, so callers that don't care keep
// working). The task id is carried separately on ChatRequest /
// EmbeddingRequest because the chat endpoint exposes it as a
// JSON field and the embedding path picks it up from the River
// job; Attribution stays focused on the repo/source/operation
// axis.
type Attribution struct {
	RepositoryID string
	SourceID     string
	Operation    string
}

type ChatRequest struct {
	Model         string        `json:"model"`
	Messages      []ChatMessage `json:"messages"`
	Temperature   *float64      `json:"temperature,omitempty"`
	MaxTokens     *int          `json:"max_tokens,omitempty"`
	ThinkingLevel *string       `json:"thinking_level,omitempty"`
	TaskID        *string       `json:"task_id,omitempty"`
	// RepositoryID / SourceID are the optional JSON-facing
	// attribution fields the interactive chat endpoint accepts.
	// The handler folds them into Attribution (the value type
	// the tracking helper reads) before invoking the provider;
	// they live on the struct so the standard JSON decode picks
	// them up. Background tasks set Attribution directly on the
	// Go struct and leave these blank.
	RepositoryID string `json:"repository_id,omitempty"`
	SourceID     string `json:"source_id,omitempty"`
	// Attribution carries the per-call context the tracking
	// helper writes into ai_usage. The handler copies the
	// JSON-decoded RepositoryID / SourceID into it; background
	// tasks set it directly. It is not JSON-exposed to avoid a
	// duplicate of the fields above.
	Attribution Attribution `json:"-"`
}

type ChatResponse struct {
	Model        string        `json:"model"`
	Messages     []ChatMessage `json:"messages"`
	Usage        Usage         `json:"usage"`
	FinishReason string        `json:"finish_reason,omitempty"`
}

type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

type AIProvider interface {
	Chat(ctx context.Context, db store.DBTX, req ChatRequest) (ChatResponse, error)
	Describe() ProviderDescription
}

// EmbeddingProvider is the interface a provider implements to
// supply bulk text embeddings. It is separate from AIProvider so
// chat-only providers (and chat-only configs) stay unaffected; a
// provider that ships both implements both. The `db store.DBTX`
// argument is passed so implementations can record token usage
// into okt_system.ai_usage the same way Chat does (mirrors
// tracking.go); pass nil to skip recording (e.g. in tests).
type EmbeddingProvider interface {
	Embed(ctx context.Context, db store.DBTX, req EmbeddingRequest) (EmbeddingResponse, error)
	Describe() ProviderDescription
}

// EmbeddingRequest is the bulk-embed input. Inputs is the list of
// strings to embed in a single round-trip; providers that cap the
// batch size (OpenAI/OpenRouter limit input count) chunk it
// themselves and concatenate the responses.
type EmbeddingRequest struct {
	Model  string   `json:"model"`
	Inputs []string `json:"inputs"`
	// TaskID is the River job id of the embed_facts worker that
	// issued the call, so usage rows can be tied back to the job
	// that caused them. Not JSON-exposed (embedding requests are
	// only built server-side by the embed worker, never decoded
	// from a client body).
	TaskID *string `json:"-"`
	// Attribution mirrors ChatRequest.Attribution: optional
	// repo/source context the tracking helper records. The embed
	// worker sets RepositoryID; SourceID is carried when the
	// embed was chained from a specific source's decomposition.
	Attribution Attribution `json:"-"`
}

// EmbeddingResponse is the bulk-embed output. Embeddings[i] is the
// vector for Inputs[i]. Usage is the summed token usage across all
// batches (for tracking).
type EmbeddingResponse struct {
	Model      string         `json:"model"`
	Embeddings [][]float32    `json:"embeddings"`
	Usage      EmbeddingUsage `json:"usage"`
}

type EmbeddingUsage struct {
	PromptTokens int `json:"prompt_tokens"`
	TotalTokens  int `json:"total_tokens"`
}

type ModelInfo struct {
	ID              string  `json:"id"`
	InputCostPer1M  float64 `json:"input_cost_per_1m"`
	OutputCostPer1M float64 `json:"output_cost_per_1m"`
	ThinkingLevel   *string `json:"thinking_level,omitempty"`
}

type ProviderDescription struct {
	Name        string      `json:"name"`
	Description string      `json:"description"`
	Requires    string      `json:"requires"`
	Configured  bool        `json:"configured"`
	Models      []ModelInfo `json:"models"`
	Timeout     string      `json:"timeout"`
	Notes       string      `json:"notes"`
}

func ModelsForProvider(cfg *config.Config, provider string) []ModelInfo {
	var models []ModelInfo
	for _, m := range cfg.Providers.AI.Models {
		if m.Provider == provider {
			models = append(models, ModelInfo{
				ID:              m.ID,
				InputCostPer1M:  m.InputCostPer1M,
				OutputCostPer1M: m.OutputCostPer1M,
				ThinkingLevel:   m.ThinkingLevel,
			})
		}
	}
	return models
}

func LookupModel(cfg *config.Config, modelID string) (config.AIModelConfig, bool) {
	for _, m := range cfg.Providers.AI.Models {
		if m.ID == modelID {
			return m, true
		}
	}
	return config.AIModelConfig{}, false
}
