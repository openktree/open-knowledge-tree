package search

import (
	"context"

	"github.com/openktree/open-knowledge-tree/backend/internal/providers/ai"
	"github.com/openktree/open-knowledge-tree/backend/internal/qdrantstore"
	"github.com/openktree/open-knowledge-tree/backend/internal/store"
)

// nilQdrantStore returns a non-nil *qdrantstore.Store whose client
// is nil. The Store methods guard against a nil client, so this is
// safe for the Available() check (which only tests nil-ness of the
// pointer) without making a real gRPC connection. Used by unit tests
// that exercise Deps.Available without booting Qdrant.
func nilQdrantStore() *qdrantstore.Store {
	return &qdrantstore.Store{}
}

// nilEmbedder is a no-op ai.EmbeddingProvider for unit tests that
// only exercise Deps.Available. Its Embed method is never called
// by those tests; if it is, it returns a zero-length embedding to
// trigger the hybrid service's fail-open path.
type nilEmbedder struct{}

func (nilEmbedder) Embed(_ context.Context, _ store.DBTX, _ ai.EmbeddingRequest) (ai.EmbeddingResponse, error) {
	return ai.EmbeddingResponse{Model: "test"}, nil
}

func (nilEmbedder) Describe() ai.ProviderDescription {
	return ai.ProviderDescription{Name: "test-embedder"}
}