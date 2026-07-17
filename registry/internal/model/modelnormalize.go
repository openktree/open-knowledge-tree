// Package model: modelnormalize.go
//
// This is a deliberate DUPLICATE of
// backend/internal/providers/ai/modelnormalize.go's
// NormalizeEmbeddingModel. The registry is a separate Go module
// (registry/go.mod) and cannot import the backend's internal
// packages, so the pure function is duplicated here for the
// registry-side backfill CLI. Keep both copies in sync — the parity
// test in this file guards against drift.
package model

import "strings"

// NormalizeEmbeddingModel collapses provider-routing differences in an
// embedding model identifier so the same underlying model stored by
// different OKT instances (via different providers) is recognized as
// identical. See the backend copy for the full rationale + rules.
//
// Rules:
//  1. Trim whitespace and lowercase.
//  2. Strip a trailing OpenRouter routing-tier tag (":free", ":nitro",
//     ":beta", ":on-demand", ":fp8"). Ollama variant tags like ":31b"
//     are model identity and preserved.
//  3. Strip the first "/"-delimited segment (the vendor routing
//     prefix, e.g. "google/" in "google/gemini-embedding-2").
//  4. Preserve everything else verbatim.
//
// Idempotent: a normalized input is returned unchanged.
func NormalizeEmbeddingModel(model string) string {
	s := strings.TrimSpace(model)
	if s == "" {
		return ""
	}
	s = strings.ToLower(s)

	if i := strings.LastIndexByte(s, ':'); i >= 0 {
		tail := s[i+1:]
		if isOpenRouterRoutingTag(tail) {
			s = s[:i]
		}
	}

	if i := strings.IndexByte(s, '/'); i >= 0 {
		s = s[i+1:]
	}

	return s
}

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