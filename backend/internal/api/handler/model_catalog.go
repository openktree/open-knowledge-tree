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
	TaskKindRefinement        = "alias_generation"
	TaskKindSummarization     = "summarization"
	TaskKindSynthesis         = "synthesis"
	TaskKindReportAnnotation  = "report_annotation"
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
//
// ID is the full configured model id (e.g. "google/gemma-4-31b-it")
// used by the per-task model picker (SetModelSetting validates
// against it). BareID is the provider-prefix-stripped name (e.g.
// "gemma-4-31b-it") the allowed_models registry picker uses — the
// registry stores decompositions under the bare name (see
// contribute_source + IsAllowed's BareModelID normalization), so a
// whitelist entry of the bare name matches decompositions from any
// provider. The frontend RegistryPanel uses m.bare_id as the
// <option value>; ModelsPanel uses m.id for per-task assignments.
type CatalogModel struct {
	ID              string  `json:"id"`
	BareID          string  `json:"bare_id"`
	Provider        string  `json:"provider"`
	InputCostPer1M  float64 `json:"input_cost_per_1m"`
	OutputCostPer1M float64 `json:"output_cost_per_1m"`
}

// All returns every configured model, sorted by id for stable UI order.
func (c *ModelCatalog) All() []CatalogModel {
	out := make([]CatalogModel, 0, len(c.models))
	for _, m := range c.models {
		out = append(out, CatalogModel{
			ID:              m.ID,
			BareID:          bareModelID(m.ID),
			Provider:        m.Provider,
			InputCostPer1M:  m.InputCostPer1M,
			OutputCostPer1M: m.OutputCostPer1M,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// bareModelID strips the provider prefix from a model id. Mirrors
// registry.BareModelID but kept local to avoid importing the
// providers package into the handler's model_catalog (which only
// depends on config). The two must stay in lockstep.
func bareModelID(modelID string) string {
	for i := len(modelID) - 1; i >= 0; i-- {
		if modelID[i] == '/' {
			return modelID[i+1:]
		}
	}
	return modelID
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
