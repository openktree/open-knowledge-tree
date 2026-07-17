//go:build e2e

package e2e_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/openktree/open-knowledge-tree/backend/e2e/testutil"
)

// decompositionProvider is the wire shape the
// /sources/decomposition/providers endpoint returns for one
// provider entry. We declare it locally so the test asserts
// on a stable contract; new fields the handler adds are
// ignored here until a test opts in.
type decompositionProvider struct {
	ID          string            `json:"id"`
	Name        string            `json:"name"`
	Description string            `json:"description"`
	Requires    string            `json:"requires"`
	Configured  bool              `json:"configured"`
	Supports    []string          `json:"supports"`
	Notes       string            `json:"notes"`
	Config      map[string]string `json:"config"`
}

type decompositionProvidersResponse struct {
	Chunking        []decompositionProvider `json:"chunking"`
	FactExtraction  []decompositionProvider `json:"fact_extraction"`
	ImageExtraction []decompositionProvider `json:"image_extraction"`
}

// TestDecompositionProvidersEndpointRequiresAuth asserts that
// /sources/decomposition/providers rejects unauthenticated
// callers (401). A future regression that flips this endpoint
// to public would leak the deployment's provider inventory.
func TestDecompositionProvidersEndpointRequiresAuth(t *testing.T) {
	env := testutil.NewTestEnv(t)
	client := newAuthClient(env.BaseURL)

	resp, body := client.do("GET", "/api/v1/sources/decomposition/providers", nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 for unauthenticated request, got %d: %s",
			resp.StatusCode, body)
	}
}

// TestDecompositionProvidersEndpointShape covers the default
// test env: the wiring currently has no chunkers and no fact
// extractors (the test util passes nil for both maps), so the
// response should be 200 with two empty arrays. This pins the
// response shape — any field the handler drops or renames
// will surface here.
func TestDecompositionProvidersEndpointShape(t *testing.T) {
	env := testutil.NewTestEnv(t)
	client := registerTestUser(t, env, "dec@example.com", "password123", "Decomp User")

	resp, body := client.do("GET", "/api/v1/sources/decomposition/providers", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, body)
	}

	var out decompositionProvidersResponse
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("unmarshal: %v: %s", err, body)
	}

	// Default test env: no chunking/fact-extraction/image-extraction
	// providers are registered. The endpoint must still return the
	// arrays (not omit them) so the UI's empty-state kicks in
	// predictably.
	if out.Chunking == nil {
		t.Error("expected chunking array to be present (possibly empty), got null")
	}
	if out.FactExtraction == nil {
		t.Error("expected fact_extraction array to be present (possibly empty), got null")
	}
	if out.ImageExtraction == nil {
		t.Error("expected image_extraction array to be present (possibly empty), got null")
	}
	if len(out.Chunking) != 0 {
		t.Errorf("expected 0 chunking providers in test env, got %d: %+v",
			len(out.Chunking), out.Chunking)
	}
	if len(out.FactExtraction) != 0 {
		t.Errorf("expected 0 fact_extraction providers in test env, got %d: %+v",
			len(out.FactExtraction), out.FactExtraction)
	}
	if len(out.ImageExtraction) != 0 {
		t.Errorf("expected 0 image_extraction providers in test env, got %d: %+v",
			len(out.ImageExtraction), out.ImageExtraction)
	}
}

// TestDecompositionProvidersEndpointWithChunker wires a
// custom env that registers a SimpleChunkingProvider, then
// asserts the response surfaces it with the expected id,
// name, supports=["chunking"], configured=true, and a
// non-empty Config map. A future regression in
// ChunkingProvider.Describe() or the handler's
// ListDecompositionProviders() would break this test.
func TestDecompositionProvidersEndpointWithChunker(t *testing.T) {
	env := testutil.NewTestEnvWithChunker(t, 1500, 150)
	client := registerTestUser(t, env, "chunker@example.com", "password123", "Chunker User")

	resp, body := client.do("GET", "/api/v1/sources/decomposition/providers", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, body)
	}

	var out decompositionProvidersResponse
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("unmarshal: %v: %s", err, body)
	}

	if len(out.Chunking) != 1 {
		t.Fatalf("expected 1 chunking provider, got %d: %+v",
			len(out.Chunking), out.Chunking)
	}
	p := out.Chunking[0]

	if p.ID != "simple" {
		t.Errorf("expected id=simple, got %q", p.ID)
	}
	if p.Name == "" {
		t.Error("expected non-empty name")
	}
	if !p.Configured {
		t.Error("expected Configured=true (simple chunker is always-on)")
	}
	if p.Requires != "" {
		t.Errorf("expected empty requires for always-on chunker, got %q", p.Requires)
	}
	if len(p.Supports) != 1 || p.Supports[0] != "chunking" {
		t.Errorf("expected Supports=[chunking], got %v", p.Supports)
	}
	// The simple chunker always surfaces its effective
	// chunk_size / chunk_overlap through the Describe()
	// Config map. We don't pin the exact values here (a
	// future default tweak would need a parallel update),
	// only that the keys are present.
	if _, ok := p.Config["chunk_size"]; !ok {
		t.Error("expected chunk_size in config map")
	}
	if _, ok := p.Config["chunk_overlap"]; !ok {
		t.Error("expected chunk_overlap in config map")
	}
}
