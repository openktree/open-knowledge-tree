package handler

import (
	"sort"

	"github.com/openktree/open-knowledge-tree/backend/internal/config"
)

// TaskKindFactExtraction etc. are the task_kind values stored in
// repository_model_settings and surfaced in the settings UI. They
// match the CHECK constraint in migration 0039. Embedding is
// deliberately excluded (dimension-specific, mixing breaks vector
// search).
const (
	TaskKindFactExtraction    = "fact_extraction"
	TaskKindImageExtraction   = "image_extraction"
	TaskKindConceptExtraction = "concept_extraction"
	TaskKindRefinement       = "alias_generation"
	TaskKindSummarization      = "summarization"
	TaskKindSynthesis          = "synthesis"
	TaskKindReportAnnotation   = "report_annotation"
)

// AllTaskKinds is the ordered list shown in the UI.
var AllTaskKinds = []string{
	TaskKindFactExtraction,
	TaskKindImageExtraction,
	TaskKindConceptExtraction,
	TaskKindRefinement,
	TaskKindSummarization,
	TaskKindSynthesis,
	TaskKindReportAnnotation,
}

// ModelCatalog is the runtime catalog of configured AI models,
// built from cfg.Providers.AI.Models at wiring time. It is the
// model-selection equivalent of ProviderRegistry: the settings UI
// lists it, the SetModelSetting handler validates against it, and
// the ModelResolver resolves model_id → provider via it.
type ModelCatalog struct {
	models []config.AIModelConfig
}

// NewModelCatalog builds a catalog from the config's model list.
func NewModelCatalog(models []config.AIModelConfig) *ModelCatalog {
	return &ModelCatalog{models: models}
}

// CatalogModel is one entry in the model catalog exposed to the UI.
type CatalogModel struct {
	ID            string  `json:"id"`
	Provider      string  `json:"provider"`
	InputCostPer1M float64 `json:"input_cost_per_1m"`
	OutputCostPer1M float64 `json:"output_cost_per_1m"`
}

// All returns every configured model, sorted by id for stable UI order.
func (c *ModelCatalog) All() []CatalogModel {
	out := make([]CatalogModel, 0, len(c.models))
	for _, m := range c.models {
		out = append(out, CatalogModel{
			ID:              m.ID,
			Provider:        m.Provider,
			InputCostPer1M:  m.InputCostPer1M,
			OutputCostPer1M: m.OutputCostPer1M,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// IsValid reports whether a model id is in the catalog.
func (c *ModelCatalog) IsValid(modelID string) bool {
	for _, m := range c.models {
		if m.ID == modelID {
			return true
		}
	}
	return false
}

// ProviderOf returns the provider id for a model id, or "" when not found.
func (c *ModelCatalog) ProviderOf(modelID string) string {
	for _, m := range c.models {
		if m.ID == modelID {
			return m.Provider
		}
	}
	return ""
}