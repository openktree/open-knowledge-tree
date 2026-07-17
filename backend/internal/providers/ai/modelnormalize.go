package ai

import "strings"

// NormalizeEmbeddingModel collapses provider-routing differences in an
// embedding model identifier so the registry cache reconciler can
// compare model *identity* rather than routing strings.
//
// Background: the embedding model string recorded on
// facts.embedded_model (and sent to the registry on push) comes from
// the provider's response, not the configured value. OpenRouter
// prefixes model IDs with a vendor segment ("google/", "openai/",
// "z-ai/") and appends routing-tier tags (":free", ":nitro"). Ollama
// uses bare names. Two OKT instances embedding with the *same*
// underlying model but different providers therefore store different
// strings ("google/gemini-embedding-2:free" vs "gemini-embedding-2"),
// which made the reconciler's exact-string mismatch check trigger a
// needless full re-embed on pull.
//
// Normalization rules (deterministic, no config dependency):
//
//  1. Trim whitespace and lowercase.
//  2. Strip OpenRouter variant suffixes — a trailing ":<word>" tag
//     from a known set of OpenRouter routing tiers (":free",
//     ":nitro", ":beta", ":on-demand", ":fp8"). Ollama variant tags
//     like ":31b" or ":8b" are model identity, not routing tiers,
//     and are preserved.
//  3. Strip the first "/"-delimited segment when present — the
//     vendor routing prefix (e.g. "google/gemini-embedding-2" →
//     "gemini-embedding-2"). Bare names are unchanged.
//  4. Preserve everything else verbatim, including version/variant
//     markers that are part of model identity ("-it", "-instruct",
//     "-8b", dimension hints).
//
// Examples:
//
//	"google/gemini-embedding-2"       → "gemini-embedding-2"
//	"google/gemini-embedding-2:free"  → "gemini-embedding-2"
//	"gemini-embedding-2"              → "gemini-embedding-2"
//	"openai/text-embedding-3-large"   → "text-embedding-3-large"
//	"qwen/qwen3-embedding"            → "qwen3-embedding"
//	"gemma-4-31b-it"                  → "gemma-4-31b-it" (preserved)
//	""                                 → ""
//
// NormalizeEmbeddingModel is idempotent: a normalized input is
// returned unchanged.
//
// NOTE: a mirror copy lives in registry/internal/model for the
// registry-side backfill CLI (the registry is a separate Go module
// and cannot import this package). Keep both copies in sync; the
// parity test in registry/internal/model/modelnormalize_test.go
// guards against drift.
func NormalizeEmbeddingModel(model string) string {
	s := strings.TrimSpace(model)
	if s == "" {
		return ""
	}
	s = strings.ToLower(s)

	// Strip a trailing OpenRouter routing-tier tag (":free", ":nitro",
	// ":beta", ":on-demand", ":fp8"). These are OpenRouter's routing
	// tiers for the same underlying model, not part of model identity.
	// Ollama's ":31b" / ":8b" are model variants and are preserved.
	if i := strings.LastIndexByte(s, ':'); i >= 0 {
		tail := s[i+1:]
		if isOpenRouterRoutingTag(tail) {
			s = s[:i]
		}
	}

	// Strip the first path segment (the vendor routing prefix).
	// "google/gemini-embedding-2" → "gemini-embedding-2".
	// "z-ai/glm-5.2"               → "glm-5.2".
	// Bare names (no "/") are unchanged.
	if i := strings.IndexByte(s, '/'); i >= 0 {
		s = s[i+1:]
	}

	return s
}

// isOpenRouterRoutingTag reports whether tag is a known OpenRouter
// routing-tier suffix. These are appended by OpenRouter to
// differentiate routing tiers of the same model (free vs paid vs
// low-latency) and are not part of the model's identity. Kept as a
// small, explicit set so Ollama variant tags like "31b" or "instruct"
// are never misclassified.
var openRouterRoutingTags = map[string]bool{
	"free":      true,
	"nitro":     true,
	"beta":      true,
	"on-demand": true,
	"fp8":       true,
}

func isOpenRouterRoutingTag(tag string) bool {
	return openRouterRoutingTags[tag]
}