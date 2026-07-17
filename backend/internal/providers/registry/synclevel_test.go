package registry

import (
	"testing"
)

func TestSyncLevelFilter_PushConcepts(t *testing.T) {
	pkg := &DecompositionPackage{
		ModelID: "m",
		Facts:   []FactData{{Content: "f1", ContentHash: "h1"}},
		Concepts: []ConceptData{{CanonicalName: "C", Context: "ctx"}},
		Links:    []FactConceptLink{{FactContentHash: "h1", ConceptName: "C", ConceptContext: "ctx"}},
		Embeddings: &EmbeddingData{
			Model: "emb-model",
			Vectors: map[string][]float64{
				"fact:u1":    {0.1},
				"concept:u2": {0.2},
			},
		},
	}
	f := NewSyncLevelFilter(SyncLevelConcepts)
	got := f.FilterForPush(pkg)
	if got != pkg {
		t.Fatal("Concepts level should return the original pointer unchanged")
	}
	if !f.IncludeConcepts() {
		t.Fatal("IncludeConcepts() must be true at Concepts level")
	}
}

func TestSyncLevelFilter_PushFacts(t *testing.T) {
	pkg := &DecompositionPackage{
		ModelID: "m",
		Facts:   []FactData{{Content: "f1", ContentHash: "h1"}},
		Concepts: []ConceptData{{CanonicalName: "C", Context: "ctx"}},
		Links:    []FactConceptLink{{FactContentHash: "h1", ConceptName: "C", ConceptContext: "ctx"}},
		Embeddings: &EmbeddingData{
			Model: "emb-model",
			Vectors: map[string][]float64{
				"fact:u1":    {0.1},
				"concept:u2": {0.2},
			},
		},
	}
	f := NewSyncLevelFilter(SyncLevelFacts)
	if f.IncludeConcepts() {
		t.Fatal("IncludeConcepts() must be false at Facts level")
	}
	got := f.FilterForPush(pkg)
	if got == pkg {
		t.Fatal("Facts level should return a copy, not the original")
	}
	if len(got.Concepts) != 0 {
		t.Fatalf("Concepts should be nil at Facts level, got %d", len(got.Concepts))
	}
	if len(got.Links) != 0 {
		t.Fatalf("Links should be nil at Facts level, got %d", len(got.Links))
	}
	if len(got.Facts) != 1 {
		t.Fatalf("Facts should be preserved, got %d", len(got.Facts))
	}
	if got.Embeddings == nil || len(got.Embeddings.Vectors) != 1 {
		t.Fatalf("only fact embeddings should survive, got %+v", got.Embeddings)
	}
	if _, ok := got.Embeddings.Vectors["fact:u1"]; !ok {
		t.Fatal("fact:u1 should be in the filtered embeddings")
	}
	// Original must be untouched.
	if len(pkg.Concepts) != 1 || len(pkg.Links) != 1 || len(pkg.Embeddings.Vectors) != 2 {
		t.Fatal("original package was mutated")
	}
}

func TestSyncLevelFilter_EmptyDefaultsToConcepts(t *testing.T) {
	f := NewSyncLevelFilter("")
	if f.Level() != SyncLevelConcepts {
		t.Fatalf("empty level should default to Concepts, got %q", f.Level())
	}
	if !f.IncludeConcepts() {
		t.Fatal("empty level should include concepts")
	}
}

func TestParseSyncLevel(t *testing.T) {
	cases := map[string]SyncLevel{
		"facts":    SyncLevelFacts,
		"FACTS":    SyncLevelFacts,
		"  facts ": SyncLevelFacts,
		"concepts": SyncLevelConcepts,
		"Concepts": SyncLevelConcepts,
		"bogus":    "bogus", // ParseSyncLevel only normalizes; ValidSyncLevels rejects
		"":         "",
	}
	for in, want := range cases {
		if got := ParseSyncLevel(in); got != want {
			t.Errorf("ParseSyncLevel(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestValidSyncLevel(t *testing.T) {
	if !ValidSyncLevel("facts") {
		t.Error("facts should be valid")
	}
	if !ValidSyncLevel("CONCEPTS") {
		t.Error("CONCEPTS should be valid (case-insensitive)")
	}
	if ValidSyncLevel("bogus") {
		t.Error("bogus should be invalid")
	}
}

func TestFilterFactEmbeddings(t *testing.T) {
	if got := filterFactEmbeddings(nil); got != nil {
		t.Errorf("nil input should return nil, got %v", got)
	}
	ed := &EmbeddingData{
		Model: "emb",
		Vectors: map[string][]float64{
			"fact:u1":    {0.1},
			"concept:u2": {0.2},
			"fact:u3":    {0.3},
		},
	}
	got := filterFactEmbeddings(ed)
	if len(got.Vectors) != 2 {
		t.Errorf("expected 2 fact embeddings, got %d", len(got.Vectors))
	}
	if _, ok := got.Vectors["fact:u1"]; !ok {
		t.Error("fact:u1 missing")
	}
	if _, ok := got.Vectors["fact:u3"]; !ok {
		t.Error("fact:u3 missing")
	}
	allConcept := &EmbeddingData{
		Vectors: map[string][]float64{"concept:u1": {0.1}},
	}
	if filterFactEmbeddings(allConcept) != nil {
		t.Error("all-concept input should return nil")
	}
}