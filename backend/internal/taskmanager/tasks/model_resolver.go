package tasks

import (
	"context"
	"log"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/openktree/open-knowledge-tree/backend/internal/config"
	"github.com/openktree/open-knowledge-tree/backend/internal/providers/ai"
	"github.com/openktree/open-knowledge-tree/backend/internal/store"
)

// Task kind constants used by the ModelResolver. These match the
// CHECK constraint values in migration 0039 and the handler package's
// AllTaskKinds. Duplicated here to avoid an import cycle (handler
// imports tasks; tasks can't import handler).
const (
	TaskKindFactExtraction    = "fact_extraction"
	TaskKindImageExtraction   = "image_extraction"
	TaskKindConceptExtraction = "concept_extraction"
	TaskKindRefinement       = "alias_generation"
	TaskKindSummarization      = "summarization"
	TaskKindSynthesis          = "synthesis"
	TaskKindReportAnnotation  = "report_annotation"
	TaskKindClaimExtraction    = "claim_extraction"
)

// ModelResolver resolves a per-repo model override for a task kind
// into the AI provider instance that serves that model + the model id
// to use. When the repo has no override for this task kind (or the
// override's model is not in the catalog / the provider isn't live),
// the resolver returns the global default (nil provider, global model
// id from the config) so the caller falls back to the baked-in
// behavior.
//
// The nil-provider fallback is intentional: the caller already holds
// a default provider instance built at boot from the global config.
// When the resolver returns (nil, ""), the caller uses that default
// instance + its baked-in model. When the resolver returns
// (provider, modelID), the caller uses the per-repo provider +
// modelID instead.
//
// The resolver is built once in cmd/app/api.go where the full
// config + aiProviders map + systemQueries are all in scope, and
// threaded into the 6 AI-using workers.
type ModelResolver struct {
	cfg          *config.Config
	aiProviders  map[string]ai.AIProvider
	systemQueries *store.Queries
}

// NewModelResolver builds a resolver. All three arguments are
// required; nil aiProviders or systemQueries makes the resolver
// always return the global default (safe for tests that don't
// test per-repo overrides).
func NewModelResolver(cfg *config.Config, aiProviders map[string]ai.AIProvider, systemQueries *store.Queries) *ModelResolver {
	return &ModelResolver{cfg: cfg, aiProviders: aiProviders, systemQueries: systemQueries}
}

// ResolveResult is the output of Resolve. When Provider is non-nil,
// the caller should use it + ModelID for this job. When Provider is
// nil, the caller should use its default (baked-in) provider +
// model — the per-repo override was absent or unresolvable.
type ResolveResult struct {
	Provider ai.AIProvider
	ModelID  string
}

// Resolve reads the per-repo model override for the given task kind
// and resolves it to (AIProvider, modelID). When no override exists,
// or the override's model isn't in the catalog, or the provider
// isn't live, it returns ResolveResult{} (nil provider, empty model)
// so the caller falls back to its default.
func (r *ModelResolver) Resolve(ctx context.Context, repoID pgtype.UUID, taskKind string) ResolveResult {
	if r.systemQueries == nil || r.cfg == nil {
		return ResolveResult{}
	}
	setting, err := r.systemQueries.GetRepositoryModelSetting(ctx, store.GetRepositoryModelSettingParams{
		RepositoryID: repoID,
		TaskKind:     taskKind,
	})
	if err != nil {
		return ResolveResult{}
	}
	if setting.ModelID == nil || *setting.ModelID == "" {
		return ResolveResult{}
	}
	modelID := *setting.ModelID
	// Resolve model_id → provider via the catalog.
	modelCfg, ok := ai.LookupModel(r.cfg, modelID)
	if !ok {
		log.Printf("model_resolver: model %q not in catalog; falling back to default for task %s", modelID, taskKind)
		return ResolveResult{}
	}
	if r.aiProviders == nil {
		return ResolveResult{}
	}
	provider, ok := r.aiProviders[modelCfg.Provider]
	if !ok || provider == nil {
		log.Printf("model_resolver: provider %q for model %q not live; falling back to default for task %s", modelCfg.Provider, modelID, taskKind)
		return ResolveResult{}
	}
	return ResolveResult{Provider: provider, ModelID: modelID}
}