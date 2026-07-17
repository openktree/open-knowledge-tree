//go:build e2e

package e2e

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/openktree/open-knowledge-tree/backend/internal/providers/search"
)

func TestOpenAlexSearchProvider_Search(t *testing.T) {
	email := os.Getenv("OPENALEX_EMAIL")
	if email == "" {
		email = "carlosgomezsoza@gmail.com"
	}

	provider := search.NewOpenAlexSearchProvider(email)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	results, err := provider.Search(ctx, "machine learning", search.SearchOptions{})
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
