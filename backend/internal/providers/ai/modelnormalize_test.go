package ai

import "testing"

func TestNormalizeEmbeddingModel(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		// Bare names pass through unchanged.
		{"gemini-embedding-2", "gemini-embedding-2"},
		{"qwen3-embedding", "qwen3-embedding"},
		{"gemma-4-31b-it", "gemma-4-31b-it"},
		{"text-embedding-3-large", "text-embedding-3-large"},

		// Vendor prefix stripped (OpenRouter-style).
		{"google/gemini-embedding-2", "gemini-embedding-2"},
		{"openai/text-embedding-3-large", "text-embedding-3-large"},
		{"qwen/qwen3-embedding", "qwen3-embedding"},
		{"z-ai/glm-5.2", "glm-5.2"},
		{"meta/llama-3.1", "llama-3.1"},

		// OpenRouter variant suffix stripped (vendor-prefixed only).
		{"google/gemini-embedding-2:free", "gemini-embedding-2"},
		{"google/gemini-embedding-2:nitro", "gemini-embedding-2"},
		{"openai/text-embedding-3-large:beta", "text-embedding-3-large"},

		// Bare-name ":tag" preserved (no vendor prefix → not an
		// OpenRouter routing tag). Ollama's "gemma4:31b" keeps ":31b"
		// because it's part of the model identity.
		{"gemma4:31b", "gemma4:31b"},
		{"qwen3:8b", "qwen3:8b"},

		// Whitespace + case normalization.
		{"  Google/Gemini-Embedding-2  ", "gemini-embedding-2"},
		{"  Qwen3-Embedding  ", "qwen3-embedding"},

		// Empty stays empty.
		{"", ""},
		{"   ", ""},

		// Idempotency: a normalized input is unchanged.
		{"gemini-embedding-2", "gemini-embedding-2"},

		// Model names with ":tag" that are NOT OpenRouter routing
		// tiers are preserved (":31b", ":8b", ":instruct" are model
		// variants, not routing tiers).
		{"ollama/gemma4:31b", "gemma4:31b"},
		{"gemma4:31b", "gemma4:31b"},
		{"qwen3:8b", "qwen3:8b"},

		// Cross-provider equivalence: same model, different routing.
		{"google/gemini-embedding-2", "gemini-embedding-2"},
		{"gemini-embedding-2", "gemini-embedding-2"},
	}
	for _, c := range cases {
		got := NormalizeEmbeddingModel(c.in)
		if got != c.want {
			t.Errorf("NormalizeEmbeddingModel(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestNormalizeEmbeddingModel_Idempotent(t *testing.T) {
	// Running normalize on its own output must be stable.
	cases := []string{
		"google/gemini-embedding-2:free",
		"openai/text-embedding-3-large",
		"qwen3-embedding",
		"",
	}
	for _, c := range cases {
		once := NormalizeEmbeddingModel(c)
		twice := NormalizeEmbeddingModel(once)
		if once != twice {
			t.Errorf("not idempotent: %q → %q → %q", c, once, twice)
		}
	}
}