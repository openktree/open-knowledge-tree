package model

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"sync"
)

// ContextClass is one entry in the canonical context vocabulary the
// registry publishes to OKT instances so they can map their local
// contexts to the registry's canonical set. The vocabulary is a
// curated set of 88 context categories derived from the DBpedia
// ontology's L3 class set (the same snapshot OKT embeds for its
// concept-extraction prompt), embedded into the registry binary at
// build time. The canonical list only changes when the embedded
// file is edited and the registry image is rebuilt; the contexts
// table is seeded from it on every boot (idempotent upsert).
type ContextClass struct {
	Label       string `json:"label"`
	Description string `json:"description,omitempty"`
}

//go:embed contexts.json
var embeddedContexts []byte

// LoadContextClasses parses the embedded snapshot once and returns
// the canonical context list. The result is cached for the lifetime
// of the process; the embedded file is the single source of truth
// (seed-on-boot, mutable-via-file-change-and-restart).
var (
	contextClassesOnce sync.Once
	contextClasses     []ContextClass
	contextClassesErr  error
)

func LoadContextClasses() ([]ContextClass, error) {
	contextClassesOnce.Do(func() {
		var raw struct {
			Contexts []ContextClass `json:"contexts"`
		}
		if err := json.Unmarshal(embeddedContexts, &raw); err != nil {
			contextClassesErr = fmt.Errorf("ontology: parsing embedded contexts.json: %w", err)
			return
		}
		if len(raw.Contexts) == 0 {
			contextClassesErr = fmt.Errorf("ontology: embedded contexts.json is empty")
			return
		}
		contextClasses = raw.Contexts
	})
	return contextClasses, contextClassesErr
}

// ContextLabels returns just the labels, in embedded order.
func ContextLabels() ([]string, error) {
	classes, err := LoadContextClasses()
	if err != nil {
		return nil, err
	}
	out := make([]string, len(classes))
	for i, c := range classes {
		out[i] = c.Label
	}
	return out, nil
}

// ContextLabelsIfReady is a context-aware accessor used by the
// service layer; the context is accepted for future live-fetch
// parity with OKT's L3Source interface but is currently unused.
func ContextLabelsIfReady(_ context.Context) ([]ContextClass, error) {
	return LoadContextClasses()
}