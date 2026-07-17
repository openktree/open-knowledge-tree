//go:build e2e

package e2e_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/openktree/open-knowledge-tree/backend/e2e/testutil"
	"github.com/openktree/open-knowledge-tree/backend/internal/api/handler"
	"github.com/openktree/open-knowledge-tree/backend/internal/config"
	"github.com/openktree/open-knowledge-tree/backend/internal/providers/ai"
	"github.com/openktree/open-knowledge-tree/backend/internal/store"
)

// stubAIProvider is a test double for ai.AIProvider that also
// implements ai.EmbeddingProvider, so a test can exercise both
// the chat-side /ai/providers endpoint and the embedding-side
// /ai/embedding/providers endpoint without hitting a real model.
// Its Describe() returns the minimum the handler reads.
type stubAIProvider struct {
	name        string
	configured  bool
	embeddingOk bool
}

func (p *stubAIProvider) Chat(ctx context.Context, db store.DBTX, req ai.ChatRequest) (ai.ChatResponse, error) {
	return ai.ChatResponse{}, nil
}

func (p *stubAIProvider) Embed(ctx context.Context, db store.DBTX, req ai.EmbeddingRequest) (ai.EmbeddingResponse, error) {
	return ai.EmbeddingResponse{}, nil
}

func (p *stubAIProvider) Describe() ai.ProviderDescription {
	return ai.ProviderDescription{
		Name:       p.name,
		Configured: p.configured,
		Notes:      "stub provider for embedding endpoint test",
	}
}

// chatOnlyProvider is a chat-only AI provider (no Embed method).
// It exists so the embedding-capable filter (a type assertion to
// ai.EmbeddingProvider) can be tested: a chat-only provider must be
// excluded from the /ai/embedding/providers response while still
// appearing on /ai/providers.
type chatOnlyProvider struct {
	name       string
	configured bool
}

func (p *chatOnlyProvider) Chat(ctx context.Context, db store.DBTX, req ai.ChatRequest) (ai.ChatResponse, error) {
	return ai.ChatResponse{}, nil
}

func (p *chatOnlyProvider) Describe() ai.ProviderDescription {
	return ai.ProviderDescription{
		Name:       p.name,
		Configured: p.configured,
		Notes:      "chat-only stub (no Embed method)",
	}
}

// embeddingActiveConfig is the wire shape the /ai/embedding/providers
// endpoint returns under the "active" key. Declared locally so the
// test asserts on a stable contract; new fields are ignored until a
// test opts in.
type embeddingActiveConfig struct {
	Provider   string `json:"provider"`
	Model      string `json:"model"`
	Dimensions int    `json:"dimensions"`
	Configured bool   `json:"configured"`
}

type embeddingProvidersResponse struct {
	Active    embeddingActiveConfig `json:"active"`
	Providers []struct {
		ID               string `json:"id"`
		Name             string `json:"name"`
		Description      string `json:"description"`
		Requires         string `json:"requires"`
		Configured       bool   `json:"configured"`
		EmbeddingCapable bool   `json:"embedding_capable"`
		Timeout          string `json:"timeout"`
		Notes            string `json:"notes"`
	} `json:"providers"`
}

// TestEmbeddingProvidersEndpointRequiresAuth asserts that
// /ai/embedding/providers rejects unauthenticated callers (401).
// We wire an AI bundle so the authed route handler runs (when
// ai==nil the notConfigured fallback returns 503 without checking
// auth, which mirrors how /ai/providers behaves — that path is
// covered by TestEmbeddingProvidersEndpointNotConfigured below).
func TestEmbeddingProvidersEndpointRequiresAuth(t *testing.T) {
	aiBundle := handler.NewAI(nil, nil, config.EmbeddingConfig{}, nil)
	env := testutil.NewTestEnvWithAI(t, aiBundle)
	client := newAuthClient(env.BaseURL)

	resp, body := client.do("GET", "/api/v1/ai/embedding/providers", nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 for unauthenticated request, got %d: %s",
			resp.StatusCode, body)
	}
}

// TestEmbeddingProvidersEndpointNotConfigured covers the default
// test env: no AI bundle is wired, so the /ai group falls through
// to the notConfigured 503 handler. This mirrors how /ai/providers
// behaves in the default env and pins that the embedding route
// follows the same fallback shape (rather than 404 or panicking).
func TestEmbeddingProvidersEndpointNotConfigured(t *testing.T) {
	env := testutil.NewTestEnv(t)
	client := newAuthClient(env.BaseURL)
	client = registerTestUser(t, env, "emb-default@example.com", "password123", "Emb Default")

	resp, body := client.do("GET", "/api/v1/ai/embedding/providers", nil)
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 when AI bundle is not wired, got %d: %s",
			resp.StatusCode, body)
	}
}

// TestEmbeddingProvidersEndpointShape wires a custom AI bundle
// with two embedding-capable stubs (ollama, openrouter) and one
// chat-only stub (ollama_cloud), then asserts:
//   - the active block reflects the configured EmbeddingConfig
//     (provider=openrouter, model, dimensions, configured=true),
//   - the providers slice contains exactly the two embedding-
//     capable stubs (chat-only ollama_cloud is excluded),
//   - each provider entry has embedding_capable=true and the
//     Describe() metadata round-trips.
//
// This guards both the type-assertion filter (chat-only providers
// must not appear) and the active-config read path.
func TestEmbeddingProvidersEndpointShape(t *testing.T) {
	aiProviders := map[string]ai.AIProvider{
		"ollama":       &stubAIProvider{name: "Ollama (stub)", configured: true, embeddingOk: true},
		"ollama_cloud": &chatOnlyProvider{name: "Ollama Cloud (stub chat-only)", configured: true},
		"openrouter":   &stubAIProvider{name: "OpenRouter (stub)", configured: true, embeddingOk: true},
	}
	embCfg := config.EmbeddingConfig{
		Provider:   "openrouter",
		Model:      "google/gemini-embedding-2",
		Dimensions: 3072,
	}
	// The active provider in aiProviders is "openrouter"; it
	// implements EmbeddingProvider, so the wiring would resolve
	// it. We pass it directly as the resolved EmbeddingProvider
	// so active.configured=true (mirroring what cmd/app/api.go
	// does after the type-assertion succeeds).
	var embeddingProvider ai.EmbeddingProvider
	if ep, ok := aiProviders["openrouter"].(ai.EmbeddingProvider); ok {
		embeddingProvider = ep
	}
	aiBundle := handler.NewAI(aiProviders, embeddingProvider, embCfg, nil)

	env := testutil.NewTestEnvWithAI(t, aiBundle)
	client := newAuthClient(env.BaseURL)
	client = registerTestUser(t, env, "emb@example.com", "password123", "Emb User")

	resp, body := client.do("GET", "/api/v1/ai/embedding/providers", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, body)
	}

	var out embeddingProvidersResponse
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("unmarshal: %v: %s", err, body)
	}

	// Active config must round-trip the EmbeddingConfig exactly.
	if out.Active.Provider != "openrouter" {
		t.Errorf("expected active.provider=openrouter, got %q", out.Active.Provider)
	}
	if out.Active.Model != "google/gemini-embedding-2" {
		t.Errorf("expected active.model=google/gemini-embedding-2, got %q", out.Active.Model)
	}
	if out.Active.Dimensions != 3072 {
		t.Errorf("expected active.dimensions=3072, got %d", out.Active.Dimensions)
	}
	if !out.Active.Configured {
		t.Error("expected active.configured=true (EmbeddingProvider was resolved)")
	}

	// The providers slice must contain exactly the two embedding-
	// capable stubs (ollama, openrouter); the chat-only ollama_cloud
	// must be filtered out by the type assertion.
	if len(out.Providers) != 2 {
		t.Fatalf("expected 2 embedding-capable providers, got %d: %+v",
			len(out.Providers), out.Providers)
	}
	// Sort order is by id (handler sorts), so [ollama, openrouter].
	if out.Providers[0].ID != "ollama" {
		t.Errorf("expected providers[0].id=ollama, got %q", out.Providers[0].ID)
	}
	if out.Providers[1].ID != "openrouter" {
		t.Errorf("expected providers[1].id=openrouter, got %q", out.Providers[1].ID)
	}
	for _, p := range out.Providers {
		if !p.EmbeddingCapable {
			t.Errorf("provider %q: expected embedding_capable=true", p.ID)
		}
		if p.Name == "" {
			t.Errorf("provider %q: expected non-empty name", p.ID)
		}
		if !p.Configured {
			t.Errorf("provider %q: expected configured=true (stub)", p.ID)
		}
	}
}

// TestEmbeddingProvidersEndpointUnconfigured covers the case
// where the named embedding provider doesn't resolve to a real
// EmbeddingProvider (e.g. misconfigured name or chat-only provider
// pointed at). The active.configured flag must be false, and the
// providers slice still lists the embedding-capable stubs so the
// operator can see what's available.
func TestEmbeddingProvidersEndpointUnconfigured(t *testing.T) {
	aiProviders := map[string]ai.AIProvider{
		"ollama": &stubAIProvider{name: "Ollama (stub)", configured: true},
	}
	// Point embedding config at a provider that isn't registered
	// in the map — mirrors a misconfigured deployment. The wiring
	// layer would leave embeddingProvider nil.
	embCfg := config.EmbeddingConfig{
		Provider:   "nonexistent",
		Model:      "some-model",
		Dimensions: 1024,
	}
	aiBundle := handler.NewAI(aiProviders, nil, embCfg, nil)

	env := testutil.NewTestEnvWithAI(t, aiBundle)
	client := newAuthClient(env.BaseURL)
	client = registerTestUser(t, env, "emb-unset@example.com", "password123", "Emb Unset")

	resp, body := client.do("GET", "/api/v1/ai/embedding/providers", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, body)
	}

	var out embeddingProvidersResponse
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("unmarshal: %v: %s", err, body)
	}
	if out.Active.Configured {
		t.Error("expected active.configured=false when EmbeddingProvider is nil")
	}
	if out.Active.Provider != "nonexistent" {
		t.Errorf("expected active.provider=nonexistent (echoed from config), got %q", out.Active.Provider)
	}
	// The one embedding-capable stub still appears in providers.
	if len(out.Providers) != 1 || out.Providers[0].ID != "ollama" {
		t.Errorf("expected providers=[ollama], got %+v", out.Providers)
	}
}