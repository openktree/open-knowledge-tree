//go:build e2e

package e2e

import (
	"context"
	"encoding/base64"
	"os"
	"testing"
	"time"

	"github.com/openktree/open-knowledge-tree/backend/internal/providers/ai"
)

// testPNGBase64 is a tiny 1x1 red PNG used as the image payload for
// the multimodal provider tests. It is a real, decodable PNG so a
// vision-capable model can process it. Inlining it as a base64
// constant keeps the tests dependency-free.
const testPNGBase64 = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8/5+hHgAHggJ/PchI7wAAAABJRU5ErkJggg=="

// mustDecodeTestPNG decodes the inline base64 PNG into bytes the
// provider helpers expect. A failure here means the constant is
// corrupt — the test cannot run.
func mustDecodeTestPNG(t *testing.T) []byte {
	t.Helper()
	b, err := base64.StdEncoding.DecodeString(testPNGBase64)
	if err != nil {
		t.Fatalf("decoding inline test PNG: %v", err)
	}
	return b
}

func TestOllamaCloudProvider_Chat(t *testing.T) {
	apiKey := os.Getenv("OLLAMA_API_KEY")
	if apiKey == "" {
		t.Skip("OLLAMA_API_KEY not set")
	}

	provider := ai.NewOllamaCloudProvider(apiKey, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	resp, err := provider.Chat(ctx, nil, ai.ChatRequest{
		Model: "nemotron-3-super",
		Messages: []ai.ChatMessage{
			{Role: "user", Content: "Say hello in exactly one sentence."},
		},
	})
	if err != nil {
		t.Fatalf("Chat failed: %v", err)
	}

	if len(resp.Messages) == 0 {
		t.Fatal("expected at least one message in response")
	}
	if resp.Messages[0].Content == "" {
		t.Fatal("expected non-empty message content")
	}
	if resp.Usage.TotalTokens == 0 {
		t.Fatal("expected non-zero token usage")
	}

	t.Logf("model: %s", resp.Model)
	t.Logf("finish_reason: %s", resp.FinishReason)
	t.Logf("usage: prompt=%d completion=%d total=%d",
		resp.Usage.PromptTokens, resp.Usage.CompletionTokens, resp.Usage.TotalTokens)
	t.Logf("response: %s", resp.Messages[0].Content)
}

func TestOllamaCloudProvider_Describe(t *testing.T) {
	apiKey := os.Getenv("OLLAMA_API_KEY")
	if apiKey == "" {
		t.Skip("OLLAMA_API_KEY not set")
	}

	provider := ai.NewOllamaCloudProvider(apiKey, nil)
	desc := provider.Describe()

	if desc.Name == "" {
		t.Fatal("expected non-empty provider name")
	}
	if !desc.Configured {
		t.Fatal("expected provider to be configured")
	}

	t.Logf("provider: %s", desc.Name)
	t.Logf("description: %s", desc.Description)
}

func TestOpenRouterProvider_Chat(t *testing.T) {
	apiKey := os.Getenv("OPENROUTER_API_KEY")
	if apiKey == "" {
		t.Skip("OPENROUTER_API_KEY not set")
	}

	provider := ai.NewOpenRouterProvider(apiKey, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	resp, err := provider.Chat(ctx, nil, ai.ChatRequest{
		Model: "openai/gpt-4o-mini",
		Messages: []ai.ChatMessage{
			{Role: "user", Content: "Say hello in exactly one sentence."},
		},
	})
	if err != nil {
		t.Fatalf("Chat failed: %v", err)
	}

	if len(resp.Messages) == 0 {
		t.Fatal("expected at least one message in response")
	}
	if resp.Messages[0].Content == "" {
		t.Fatal("expected non-empty message content")
	}
	if resp.Usage.TotalTokens == 0 {
		t.Fatal("expected non-zero token usage")
	}

	t.Logf("model: %s", resp.Model)
	t.Logf("finish_reason: %s", resp.FinishReason)
	t.Logf("usage: prompt=%d completion=%d total=%d",
		resp.Usage.PromptTokens, resp.Usage.CompletionTokens, resp.Usage.TotalTokens)
	t.Logf("response: %s", resp.Messages[0].Content)
}

func TestOpenRouterProvider_ChatWithThinking(t *testing.T) {
	apiKey := os.Getenv("OPENROUTER_API_KEY")
	if apiKey == "" {
		t.Skip("OPENROUTER_API_KEY not set")
	}

	provider := ai.NewOpenRouterProvider(apiKey, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	thinkingLevel := "low"
	resp, err := provider.Chat(ctx, nil, ai.ChatRequest{
		Model:         "openai/o3-mini",
		Messages:      []ai.ChatMessage{{Role: "user", Content: "What is 2+2? Answer in one word."}},
		ThinkingLevel: &thinkingLevel,
	})
	if err != nil {
		t.Fatalf("Chat with thinking failed: %v", err)
	}

	if len(resp.Messages) == 0 {
		t.Fatal("expected at least one message in response")
	}

	t.Logf("model: %s", resp.Model)
	t.Logf("finish_reason: %s", resp.FinishReason)
	t.Logf("usage: prompt=%d completion=%d total=%d",
		resp.Usage.PromptTokens, resp.Usage.CompletionTokens, resp.Usage.TotalTokens)
	t.Logf("response: %s", resp.Messages[0].Content)
}

func TestOpenRouterProvider_Describe(t *testing.T) {
	apiKey := os.Getenv("OPENROUTER_API_KEY")
	if apiKey == "" {
		t.Skip("OPENROUTER_API_KEY not set")
	}

	provider := ai.NewOpenRouterProvider(apiKey, nil)
	desc := provider.Describe()

	if desc.Name == "" {
		t.Fatal("expected non-empty provider name")
	}
	if !desc.Configured {
		t.Fatal("expected provider to be configured")
	}

	t.Logf("provider: %s", desc.Name)
	t.Logf("description: %s", desc.Description)
}

func TestOllamaProvider_Chat(t *testing.T) {
	baseURL := os.Getenv("OLLAMA_BASE_URL")
	if baseURL == "" {
		t.Skip("OLLAMA_BASE_URL not set (requires local Ollama instance)")
	}

	provider := ai.NewOllamaProvider(baseURL, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	resp, err := provider.Chat(ctx, nil, ai.ChatRequest{
		Model: "llama3.2",
		Messages: []ai.ChatMessage{
			{Role: "user", Content: "Say hello in exactly one sentence."},
		},
	})
	if err != nil {
		t.Fatalf("Chat failed: %v", err)
	}

	if len(resp.Messages) == 0 {
		t.Fatal("expected at least one message in response")
	}
	if resp.Messages[0].Content == "" {
		t.Fatal("expected non-empty message content")
	}

	t.Logf("model: %s", resp.Model)
	t.Logf("finish_reason: %s", resp.FinishReason)
	t.Logf("usage: prompt=%d completion=%d total=%d",
		resp.Usage.PromptTokens, resp.Usage.CompletionTokens, resp.Usage.TotalTokens)
	t.Logf("response: %s", resp.Messages[0].Content)
}

// TestOllamaCloudProvider_ChatWithImage exercises the multimodal
// path of the Ollama Cloud provider: a user message built with
// ai.NewImageMessage carries a 1x1 PNG as an `images` array entry
// (bare base64, per the Ollama /api/chat spec). The model must
// return a non-empty text response acknowledging the image.
//
// Skips when OLLAMA_API_KEY is unset, mirroring the text chat test.
// The model id must be a vision-capable model available on Ollama
// Cloud (gemma 4 is multimodal); swap it via
// OKT_TEST_OLLAMA_CLOUD_VISION_MODEL if the default errors.
func TestOllamaCloudProvider_ChatWithImage(t *testing.T) {
	apiKey := os.Getenv("OLLAMA_API_KEY")
	if apiKey == "" {
		t.Skip("OLLAMA_API_KEY not set")
	}
	model := os.Getenv("OKT_TEST_OLLAMA_CLOUD_VISION_MODEL")
	if model == "" {
		model = "gemma4:31b"
	}

	provider := ai.NewOllamaCloudProvider(apiKey, nil)
	pngBytes := mustDecodeTestPNG(t)

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	resp, err := provider.Chat(ctx, nil, ai.ChatRequest{
		Model: model,
		Messages: []ai.ChatMessage{
			ai.NewImageMessage("user", "What color is the single pixel in this image? Answer in one word.", []ai.ImageData{
				{Bytes: pngBytes, ContentType: "image/png"},
			}),
		},
	})
	if err != nil {
		t.Fatalf("Chat with image failed: %v", err)
	}
	if len(resp.Messages) == 0 {
		t.Fatal("expected at least one message in response")
	}
	if resp.Messages[0].Content == "" {
		t.Fatal("expected non-empty message content for image prompt")
	}

	t.Logf("model: %s", resp.Model)
	t.Logf("finish_reason: %s", resp.FinishReason)
	t.Logf("usage: prompt=%d completion=%d total=%d",
		resp.Usage.PromptTokens, resp.Usage.CompletionTokens, resp.Usage.TotalTokens)
	t.Logf("response: %s", resp.Messages[0].Content)
}

// TestOpenRouterProvider_ChatWithImage exercises the multimodal
// path of the OpenRouter provider: a user message built with
// ai.NewImageMessage carries a 1x1 PNG as an `image_url` content
// part (base64 data URL, per the OpenAI vision wire format that
// OpenRouter speaks). The model must return a non-empty text
// response.
//
// Skips when OPENROUTER_API_KEY is unset. The model id must be a
// vision-capable model on OpenRouter; override via
// OKT_TEST_OPENROUTER_VISION_MODEL if the default errors.
func TestOpenRouterProvider_ChatWithImage(t *testing.T) {
	apiKey := os.Getenv("OPENROUTER_API_KEY")
	if apiKey == "" {
		t.Skip("OPENROUTER_API_KEY not set")
	}
	model := os.Getenv("OKT_TEST_OPENROUTER_VISION_MODEL")
	if model == "" {
		model = "openai/gpt-4o-mini"
	}

	provider := ai.NewOpenRouterProvider(apiKey, nil)
	pngBytes := mustDecodeTestPNG(t)

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	resp, err := provider.Chat(ctx, nil, ai.ChatRequest{
		Model: model,
		Messages: []ai.ChatMessage{
			ai.NewImageMessage("user", "What color is the single pixel in this image? Answer in one word.", []ai.ImageData{
				{Bytes: pngBytes, ContentType: "image/png"},
			}),
		},
	})
	if err != nil {
		t.Fatalf("Chat with image failed: %v", err)
	}
	if len(resp.Messages) == 0 {
		t.Fatal("expected at least one message in response")
	}
	if resp.Messages[0].Content == "" {
		t.Fatal("expected non-empty message content for image prompt")
	}

	t.Logf("model: %s", resp.Model)
	t.Logf("finish_reason: %s", resp.FinishReason)
	t.Logf("usage: prompt=%d completion=%d total=%d",
		resp.Usage.PromptTokens, resp.Usage.CompletionTokens, resp.Usage.TotalTokens)
	t.Logf("response: %s", resp.Messages[0].Content)
}

// TestOllamaProvider_ChatWithImage exercises the multimodal path of
// the local Ollama provider against a locally-pulled vision model
// (default llava). Skips when OLLAMA_BASE_URL is unset.
//
// Override the model via OKT_TEST_OLLAMA_VISION_MODEL when the
// default is not pulled locally.
func TestOllamaProvider_ChatWithImage(t *testing.T) {
	baseURL := os.Getenv("OLLAMA_BASE_URL")
	if baseURL == "" {
		t.Skip("OLLAMA_BASE_URL not set (requires local Ollama instance with a vision model)")
	}
	model := os.Getenv("OKT_TEST_OLLAMA_VISION_MODEL")
	if model == "" {
		model = "llava"
	}

	provider := ai.NewOllamaProvider(baseURL, nil)
	pngBytes := mustDecodeTestPNG(t)

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	resp, err := provider.Chat(ctx, nil, ai.ChatRequest{
		Model: model,
		Messages: []ai.ChatMessage{
			ai.NewImageMessage("user", "What color is the single pixel in this image? Answer in one word.", []ai.ImageData{
				{Bytes: pngBytes, ContentType: "image/png"},
			}),
		},
	})
	if err != nil {
		t.Fatalf("Chat with image failed: %v", err)
	}
	if len(resp.Messages) == 0 {
		t.Fatal("expected at least one message in response")
	}
	if resp.Messages[0].Content == "" {
		t.Fatal("expected non-empty message content for image prompt")
	}

	t.Logf("model: %s", resp.Model)
	t.Logf("finish_reason: %s", resp.FinishReason)
	t.Logf("usage: prompt=%d completion=%d total=%d",
		resp.Usage.PromptTokens, resp.Usage.CompletionTokens, resp.Usage.TotalTokens)
	t.Logf("response: %s", resp.Messages[0].Content)
}