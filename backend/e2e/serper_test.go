//go:build e2e

package e2e

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/openktree/open-knowledge-tree/backend/internal/providers/search"
)

func TestSerperSearchProvider_Search(t *testing.T) {
	apiKey := os.Getenv("SERPER_API_KEY")
	if apiKey == "" {
		t.Skip("SERPER_API_KEY not set")
	}

	provider := search.NewSerperSearchProvider(apiKey)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	results, err := provider.Search(ctx, "Go programming language", search.SearchOptions{})
	if err != nil {
		t.Fatalf("Search failed: %v", err)
	}

	if len(results.Results) == 0 {
		t.Fatal("expected at least one search result, got none")
	}

	t.Logf("got %d results (total=%d, next_cursor=%q):", len(results.Results), results.Total, results.NextCursor)
	for i, r := range results.Results {
		t.Logf("  [%d] %s", i, r.Title)
		t.Logf("      %s", r.URL)
		t.Logf("      %s", r.Snippet)
	}
}
