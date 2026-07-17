package model

import "testing"

func TestNormalizeEmbeddingModel(t *testing.T) {
	cases := []struct{ in, want string }{
		{"gemini-embedding-2", "gemini-embedding-2"},
		{"google/gemini-embedding-2", "gemini-embedding-2"},
		{"google/gemini-embedding-2:free", "gemini-embedding-2"},
		{"gemini-embedding-2", "gemini-embedding-2"},
		{"openai/text-embedding-3-large", "text-embedding-3-large"},
		{"qwen/qwen3-embedding", "qwen3-embedding"},
		{"gemma-4-31b-it", "gemma-4-31b-it"},
		{"ollama/gemma4:31b", "gemma4:31b"},
		{"gemma4:31b", "gemma4:31b"},
		{"  Google/Gemini-Embedding-2  ", "gemini-embedding-2"},
		{"", ""},
		{"   ", ""},
	}
	for _, c := range cases {
		got := NormalizeEmbeddingModel(c.in)
		if got != c.want {
			t.Errorf("NormalizeEmbeddingModel(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestNormalizeEmbeddingModel_Idempotent(t *testing.T) {
	for _, c := range []string{"google/gemini-embedding-2:free", "qwen3-embedding", ""} {
		once := NormalizeEmbeddingModel(c)
		twice := NormalizeEmbeddingModel(once)
		if once != twice {
			t.Errorf("not idempotent: %q → %q → %q", c, once, twice)
		}
	}
}