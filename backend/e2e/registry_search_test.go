//go:build e2e

package e2e_test

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/openktree/open-knowledge-tree/backend/e2e/testutil"
	"github.com/openktree/open-knowledge-tree/backend/internal/config"
	"github.com/openktree/open-knowledge-tree/backend/internal/providers/registry"
	"github.com/openktree/open-knowledge-tree/backend/internal/providers/search"
)

// TestRegistrySearchProvider_Search exercises the registry search
// provider against a live registry. Skips gracefully when
// OKT_TEST_REGISTRY_URL is unset (mirrors the serper/openalex
// env-gated pattern) so the suite stays green in a keyless dev env.
func TestRegistrySearchProvider_Search(t *testing.T) {
	regURL := os.Getenv("OKT_TEST_REGISTRY_URL")
	if regURL == "" {
		t.Skip("OKT_TEST_REGISTRY_URL not set; skipping live registry search test")
	}

	cfg := config.RegistryConfig{ID: "default", URL: regURL}
	client := registry.New(cfg)
	services := registry.NewServiceMap(registry.NewClientMap(config.ProvidersConfig{
		Registries: []config.RegistryConfig{cfg},
	}))
	provider := search.NewRegistrySearchProvider(services, config.SearchRegistryProviderConfig{})

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	// Inject the "default" registry id so the ServiceMap resolves it.
	ctx = registry.WithRegistryID(ctx, "default")

	resp, err := provider.Search(ctx, "machine learning", search.SearchOptions{})
	if err != nil {
		t.Fatalf("Search failed: %v", err)
	}

	t.Logf("got %d results (total=%d, next_cursor=%q)", len(resp.Results), resp.Total, resp.NextCursor)
	for i, r := range resp.Results {
		if i >= 5 {
			break
		}
		t.Logf("  [%d] %s", i, r.Title)
		t.Logf("      %s", r.URL)
	}
}

// TestRegistrySearchProvider_KeylessConfig verifies that a deployment
// with no SERPER_API_KEY and no OPENALEX_EMAIL still gets the
// registry search provider registered when a registry is configured,
// so the keyless default works. This is the core of the "low-budget
// user can run with only the registry" goal.
func TestRegistrySearchProvider_KeylessConfig(t *testing.T) {
	regURL := os.Getenv("OKT_TEST_REGISTRY_URL")
	if regURL == "" {
		t.Skip("OKT_TEST_REGISTRY_URL not set; skipping live registry keyless config test")
	}

	// Build a search providers map the same way cmd/app/api.go does,
	// but with no Serper/OpenAlex keys — only the registry.
	searchProviders := make(map[string]search.SearchProvider)
	registryClients := registry.NewClientMap(config.ProvidersConfig{
		Registries: []config.RegistryConfig{{ID: "default", URL: regURL}},
	})
	registryServices := registry.NewServiceMap(registryClients)
	if registryServices.IsConfigured() {
		searchProviders["registry"] = search.NewRegistrySearchProvider(registryServices, config.SearchRegistryProviderConfig{})
	}

	if len(searchProviders) != 1 {
		t.Fatalf("expected exactly 1 search provider (registry) in keyless config, got %d", len(searchProviders))
	}
	if _, ok := searchProviders["registry"]; !ok {
		t.Fatalf("expected registry provider in keyless config, got %v", searchProviders)
	}

	// The provider should be usable for a search.
	provider := searchProviders["registry"]
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	ctx = registry.WithRegistryID(ctx, "default")
	resp, err := provider.Search(ctx, "test", search.SearchOptions{})
	if err != nil {
		t.Fatalf("keyless registry Search failed: %v", err)
	}
	t.Logf("keyless config: got %d results (total=%d)", len(resp.Results), resp.Total)
}

// TestRegistrySearchProvider_ListProviders verifies the
// /sources/providers endpoint lists the registry search provider
// when it's registered. This is the REST surface an agent or the
// settings UI reads to discover available providers.
func TestRegistrySearchProvider_ListProviders(t *testing.T) {
	env := testutil.NewTestEnv(t)
	defer env.Server.Close()
	admin := bootstrapSysAdmin(t, env, "registry-search@example.com")

	resp, raw := admin.do("GET", "/api/v1/sources/providers", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /sources/providers: status %d, body %s", resp.StatusCode, string(raw))
	}
	var out struct {
		Search []map[string]interface{} `json:"search"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode providers: %v", err)
	}
	// The e2e env may or may not have a registry configured; assert
	// only that the response is well-formed (the search array exists).
	// When a registry is configured, "registry" should appear in the
	// list with the human name "OKT Knowledge Registry".
	for _, p := range out.Search {
		if p["id"] == "registry" {
			if p["name"] != "OKT Knowledge Registry" {
				t.Errorf("registry provider name: expected \"OKT Knowledge Registry\", got %v", p["name"])
			}
			return
		}
	}
	// Not found — acceptable when no registry is configured in the
	// e2e env. No assertion failure.
}