package search

import (
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/openktree/open-knowledge-tree/backend/internal/config"
)

func TestDeps_Available(t *testing.T) {
	base := func() Deps {
		return Deps{
			Qdrant:            nilQdrantStore(),
			EmbeddingProvider: nilEmbedder{},
			EmbeddingCfg:      config.EmbeddingConfig{Model: "test-model"},
			Hybrid:            config.SearchHybridConfig{Enabled: true, RRFK: 60, OverFetchMultiplier: 3},
		}
	}
	if !base().Available() {
		t.Error("expected Available=true when all deps wired")
	}
	if (Deps{}).Available() {
		t.Error("zero-value Deps should not be Available")
	}
	c := base()
	c.Hybrid.Enabled = false
	if c.Available() {
		t.Error("Enabled=false should disable")
	}
	c = base()
	c.Qdrant = nil
	if c.Available() {
		t.Error("nil Qdrant should disable")
	}
	c = base()
	c.EmbeddingProvider = nil
	if c.Available() {
		t.Error("nil EmbeddingProvider should disable")
	}
	c = base()
	c.EmbeddingCfg.Model = ""
	if c.Available() {
		t.Error("empty embedding model should disable")
	}
	c = base()
	c.Hybrid.OverFetchMultiplier = 0
	if c.Available() {
		t.Error("zero OverFetchMultiplier should disable")
	}
}

func TestValidateUUID(t *testing.T) {
	if err := validateUUID(pgtype.UUID{}); err == nil {
		t.Error("zero-value pgtype.UUID should be invalid")
	}
	u := uuid.New()
	if err := validateUUID(pgtype.UUID{Bytes: u, Valid: true}); err != nil {
		t.Errorf("valid UUID should pass: %v", err)
	}
}

func TestPgUUIDRoundTrip(t *testing.T) {
	u := uuid.New()
	pg := pgUUIDFromUUID(u)
	got := pgUUIDToUUID(pg)
	if got == nil || *got != u {
		t.Fatalf("round-trip failed: got %v", got)
	}
	if pgUUIDToUUID(pgtype.UUID{}) != nil {
		t.Error("invalid pgtype.UUID should return nil")
	}
}