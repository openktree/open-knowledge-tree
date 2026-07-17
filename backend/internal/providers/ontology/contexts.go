// Package ontology supplies the controlled vocabulary the concept-
// extraction prompt uses to assign a context (an ontology class) to
// every extracted concept. The vocabulary is a curated set of 88
// context categories derived from the DBpedia ontology's L3 class
// set by merging semantically similar labels and cross-referencing
// with the DBpedia L2 hierarchy. The full derivation is in
// scripts/experiments/merge_labels.py and the manual curation in
// scripts/experiments/manual_select.json.
//
// The package exposes a single interface — L3Source — and one
// implementation: EmbeddedL3Source, which serves the go:embedded
// contexts.json snapshot compiled into the binary. There is no
// live SPARQL fetch — the embedded file is the single source of
// truth. An operator refreshes it by editing contexts.json and
// rebuilding (the experiments scripts can help derive a new list).
package ontology

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
)

// ContextClass is one entry in the curated context vocabulary: a
// human-readable label the concept-extraction prompt offers the
// model, plus an optional description used as a hint when the label
// alone is ambiguous (e.g. "Activity — practices, exercises,
// healing methods, and things people do").
type ContextClass struct {
	Label       string `json:"label"`
	Description string `json:"description,omitempty"`
}

// L3Source is the contract the concept-extraction worker and the
// repository-seeding logic consume. ContextClasses returns the
// curated context labels (with optional descriptions) the prompt
// offers the model as the allowed context vocabulary. The slice is
// ordered as committed in the embedded file so the prompt is stable
// across runs.
type L3Source interface {
	ContextClasses(ctx context.Context) ([]ContextClass, error)
}

// embeddedContexts holds the JSON snapshot compiled into the binary.
// The file lives next to this .go file; go:embed pulls it in at
// build time so the server boots offline.
//
//go:embed contexts.json
var embeddedContexts []byte

// EmbeddedL3Source serves the committed snapshot. It is the only
// implementation; there is no live SPARQL fetch.
type EmbeddedL3Source struct {
	classes []ContextClass
}

// NewEmbeddedL3Source parses the embedded snapshot once and returns
// a source that serves the parsed slice for the lifetime of the
// process.
func NewEmbeddedL3Source() (*EmbeddedL3Source, error) {
	var raw struct {
		Contexts []ContextClass `json:"contexts"`
	}
	if err := json.Unmarshal(embeddedContexts, &raw); err != nil {
		return nil, fmt.Errorf("ontology: parsing embedded contexts.json: %w", err)
	}
	if len(raw.Contexts) == 0 {
		return nil, fmt.Errorf("ontology: embedded contexts.json is empty")
	}
	return &EmbeddedL3Source{classes: raw.Contexts}, nil
}

func (s *EmbeddedL3Source) ContextClasses(_ context.Context) ([]ContextClass, error) {
	return s.classes, nil
}

// Labels returns just the labels (no descriptions), useful for the
// seeding logic that needs to check membership.
func Labels(classes []ContextClass) []string {
	out := make([]string, len(classes))
	for i, c := range classes {
		out[i] = c.Label
	}
	return out
}