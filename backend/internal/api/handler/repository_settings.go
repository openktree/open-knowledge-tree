package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/openktree/open-knowledge-tree/backend/internal/api/httputil"
	"github.com/openktree/open-knowledge-tree/backend/internal/config"
	"github.com/openktree/open-knowledge-tree/backend/internal/promptset"
	"github.com/openktree/open-knowledge-tree/backend/internal/providers/registry"
	"github.com/openktree/open-knowledge-tree/backend/internal/store"
)

// seedInput is the resolved set of providers + contexts the seeding
// helper inserts for a freshly-created repository. It is the
// post-resolution form of a create body / preset: provider ids have
// been intersected with the live registry (orphans dropped) and the
// "all" context sentinel has been expanded to the full embedded
// context vocabulary.
type seedInput struct {
	providers          []seedProvider
	contexts           []seedContext
	customContexts     []seedContext
	allowedContentTypes []string // nil = allow all (the default); non-nil = restrict to these kinds
}

type seedProvider struct {
	kind    string
	id      string
	enabled bool
}

type seedContext struct {
	label       string
	description string
}

// resolveSeed builds a seedInput from a create body (preset id +
// optional overrides). The resolution rules:
//
//   - providers: when body.Providers is non-empty, use it (per
//     kind); otherwise use the preset's. Each id is intersected
//     with the live registry; orphans are dropped (silently — the
//     caller can't seed a provider that isn't live in this
//     deployment). An absent kind in the preset means "no providers
//     of that kind enabled".
//   - contexts: when body.Contexts is non-empty, use it; otherwise
//     use the preset's. "all" expands to the full embedded
//     context vocabulary (is_custom=FALSE). An empty/absent list means "no
//     contexts allowed" (the admin fills custom ones — the
//     Enterprise pattern).
//   - custom_contexts: always come from the preset (the create
//     body doesn't override them; an admin adds custom contexts
//     from the settings page after creation).
//
// When no preset matches (and no default preset is configured), the
// seed is empty — the repo starts with no settings, and the gate
// denies everything until an admin configures it. This matches the
// "settings are the source of truth" model: a misconfigured create
// surfaces as a non-functional repo, not as silent "allow all".
// SeedDefaultRepositorySettings seeds the provider + context
// settings for a freshly-created repository using the same
// resolution path as CreateRepository (default preset → live
// registry fallback → embedded context vocabulary). It is the
// single entry point for callers that create a repository outside
// the HTTP CreateRepository flow — currently only
// bootstrap.EnsureDefaultRepository, which previously inserted the
// repo row directly and left it with no settings, triggering the
// "search provider not enabled for this repository" gate on every
// subsequent search.
//
// repoID is the string form of the repository UUID; it is scanned
// into a pgtype.UUID here so callers don't need to depend on pgx.
// A malformed repoID returns an error without writing anything.
//
// The seeding is idempotent (every insert is ON CONFLICT DO NOTHING
// at the sqlc layer) so a retry after a partial failure is safe.
// A deps with a nil ProviderRegistry / OntologySource degrades to
// the same fallbacks resolveSeed already uses: every live provider
// enabled, the full context vocabulary expanded from the embedded
// ontology source. When both are nil the seed is empty and the
// caller gets back a nil error with nothing written — the repo
// stays in the pre-fix "no settings" state, which is no worse than
// before.
//
// Exported (capitalised) so the bootstrap package, which lives in
// internal/bootstrap and cannot import the handler package
// directly without an import cycle, can take it as a callback
// parameter. The bootstrap package owns only the signature; the
// handler package owns the implementation.
func SeedDefaultRepositorySettings(ctx context.Context, deps Deps, repoID string) error {
	var id pgtype.UUID
	if err := id.Scan(repoID); err != nil {
		return fmt.Errorf("invalid repository_id: %w", err)
	}
	seed, err := resolveSeed(ctx, deps, createSeedBody{})
	if err != nil {
		return fmt.Errorf("resolving default seed: %w", err)
	}
	if err := seedRepositorySettings(ctx, deps, id, seed); err != nil {
		return fmt.Errorf("seeding default settings: %w", err)
	}
	return nil
}

func resolveSeed(ctx context.Context, deps Deps, body createSeedBody) (seedInput, error) {
	var in seedInput

	// Resolve the preset: explicit body.Preset → default preset →
	// none. When none matches, fall back to the "general"-style
	// default (all live providers + the full context vocabulary)
	// so a deployment that hasn't configured presets still seeds a
	// functional repo. This also covers the e2e test config, which
	// doesn't load the YAML presets.
	preset := (*config.RepositoryPreset)(nil)
	if body.Preset != "" {
		preset = deps.Config.PresetByID(body.Preset)
	} else {
		preset = deps.Config.DefaultPreset()
	}

	// ---- Providers ----
	prov := body.Providers
	if prov == nil && preset != nil {
		prov = preset.Providers
	}
	if prov == nil {
		// No preset providers: seed every live provider enabled.
		// This is the "general" default and the e2e fallback.
		prov = map[string][]string{}
		if deps.ProviderRegistry != nil {
			for _, lp := range deps.ProviderRegistry.LiveProviders() {
				prov[lp.Kind] = append(prov[lp.Kind], lp.ID)
			}
		}
	}
	if prov != nil && deps.ProviderRegistry != nil {
		live := deps.ProviderRegistry.LiveProviderIDs()
		for kind, ids := range prov {
			if kind != ProviderKindSearch && kind != ProviderKindResolution {
				continue
			}
			for _, id := range ids {
				if id == "" {
					continue
				}
				if !live[[2]string{kind, id}] {
					log.Printf("repository seeding: provider %s/%s not live in this deployment; dropping from seed", kind, id)
					continue
				}
				in.providers = append(in.providers, seedProvider{kind: kind, id: id, enabled: true})
			}
		}
	}

	// ---- Contexts ----
	labels := body.Contexts
	var customLabels []string
	var customDescs map[string]string
	if preset != nil {
		if labels == nil {
			labels = preset.Contexts
		}
		customLabels = preset.CustomContexts
		customDescs = preset.CustomContextDescriptions
	}
	if labels == nil {
		// No preset contexts: seed the full context vocabulary
		// (the "general" default). An explicit empty list (len==0) is
		// honored as "no contexts" (the Enterprise pattern); nil
		// means "no preference → full set".
		labels = []string{"all"}
	}
	expanded, err := expandContextList(ctx, deps, labels)
	if err != nil {
		return in, err
	}
	in.contexts = expanded
	for _, c := range customLabels {
		if c == "" {
			continue
		}
		desc := ""
		if customDescs != nil {
			desc = customDescs[c]
		}
		in.customContexts = append(in.customContexts, seedContext{label: c, description: desc})
	}
	// Per-repo allowed content types (migration 0049). nil = allow
	// all (the default); a non-empty array restricts to the listed
	// kinds. The "scientific" preset ships ["doi"] so a scientific
	// repo only accepts DOI-identified sources out of the box.
	if preset != nil {
		in.allowedContentTypes = preset.AllowedContentTypes
	}
	return in, nil
}

// expandContextList turns the preset's contexts slice into concrete
// seedContext rows. The "all" sentinel expands to the full embedded
// context vocabulary (is_custom=FALSE, with descriptions from the
// ontology file). Anything else is taken verbatim (validated
// against the vocabulary downstream only for the is_custom flag —
// a non-standard label in the explicit list is seeded is_custom=TRUE
// so the admin can still use it).
func expandContextList(ctx context.Context, deps Deps, labels []string) ([]seedContext, error) {
	if len(labels) == 0 {
		return nil, nil
	}
	// "all" expands to the full context vocabulary with descriptions.
	if len(labels) == 1 && labels[0] == "all" {
		if deps.OntologySource == nil {
			return nil, errors.New("repository seeding: ontology source not configured; cannot expand \"all\" contexts")
		}
		classes, err := deps.OntologySource.ContextClasses(ctx)
		if err != nil {
			return nil, fmt.Errorf("repository seeding: loading context classes: %w", err)
		}
		out := make([]seedContext, 0, len(classes))
		for _, c := range classes {
			out = append(out, seedContext{label: c.Label, description: c.Description})
		}
		return out, nil
	}
	// Explicit list: split into known (is_custom=FALSE) vs custom.
	var known, custom []string
	if deps.OntologySource != nil {
		classes, _ := deps.OntologySource.ContextClasses(ctx)
		set := make(map[string]bool, len(classes))
		for _, k := range classes {
			set[strings.ToLower(k.Label)] = true
		}
		for _, l := range labels {
			if set[strings.ToLower(l)] {
				known = append(known, l)
			} else {
				custom = append(custom, l)
			}
		}
	} else {
		custom = labels
	}
	// Build a lookup for descriptions from the known classes.
	var descLookup map[string]string
	if deps.OntologySource != nil {
		classes, _ := deps.OntologySource.ContextClasses(ctx)
		descLookup = make(map[string]string, len(classes))
		for _, c := range classes {
			descLookup[strings.ToLower(c.Label)] = c.Description
		}
	}
	out := make([]seedContext, 0, len(known)+len(custom))
	for _, l := range known {
		desc := ""
		if descLookup != nil {
			desc = descLookup[strings.ToLower(l)]
		}
		out = append(out, seedContext{label: l, description: desc})
	}
	// Explicit non-standard labels are seeded is_custom=TRUE so the
	// admin can still use them (the UI flags them as custom).
	for _, l := range custom {
		out = append(out, seedContext{label: l, description: ""})
	}
	return out, nil
}

// seedRepositorySettings writes the resolved seed rows for a repo.
// Called from CreateRepository after the repo row + role grant. All
// inserts are idempotent (ON CONFLICT DO NOTHING at the sqlc layer)
// so a retry after a partial failure is safe. Best-effort: a failure
// is logged and returned so the caller can decide whether to fail
// the whole create (we do — a repo with no settings is non-functional
// under the "settings are the source of truth" model).
func seedRepositorySettings(ctx context.Context, deps Deps, repoID pgtype.UUID, in seedInput) error {
	for _, p := range in.providers {
		if _, err := deps.Store.SeedRepositoryProviderSetting(ctx, store.SeedRepositoryProviderSettingParams{
			RepositoryID: repoID,
			ProviderKind: p.kind,
			ProviderID:   p.id,
			Enabled:      p.enabled,
		}); err != nil {
			return fmt.Errorf("seeding provider %s/%s: %w", p.kind, p.id, err)
		}
	}
	for _, c := range in.contexts {
		if _, err := deps.Store.SeedRepositoryContext(ctx, store.SeedRepositoryContextParams{
			RepositoryID: repoID,
			Context:      c.label,
			IsCustom:     false,
			Description:  c.description,
		}); err != nil {
			return fmt.Errorf("seeding context %q: %w", c.label, err)
		}
	}
	for _, c := range in.customContexts {
		if _, err := deps.Store.SeedRepositoryContext(ctx, store.SeedRepositoryContextParams{
			RepositoryID: repoID,
			Context:      c.label,
			IsCustom:     true,
			Description:  c.description,
		}); err != nil {
			return fmt.Errorf("seeding custom context %q: %w", c.label, err)
		}
	}
	// Per-repo allowed content types (migration 0049). Only write
	// when the preset specified a non-nil list; nil = allow all (the
	// default) and we leave the column NULL so the gate is a no-op.
	if in.allowedContentTypes != nil {
		if err := deps.Store.SetRepositoryAllowedContentTypes(ctx, store.SetRepositoryAllowedContentTypesParams{
			ID:                  repoID,
			AllowedContentTypes: in.allowedContentTypes,
		}); err != nil {
			return fmt.Errorf("seeding allowed_content_types: %w", err)
		}
	}
	return nil
}

// createSeedBody is the subset of the create-repository body that
// drives settings seeding. Embedded in createRepositoryRequest so
// the existing JSON shape gains the fields without a separate type.
type createSeedBody struct {
	Preset    string              `json:"preset"`
	Providers map[string][]string `json:"providers"`
	Contexts  []string             `json:"contexts"`
}

// RepositorySettings bundles the per-repository settings HTTP
// handlers (provider toggles + context management). All handlers
// operate on the system pool (repo metadata, not per-tenant data),
// reading the repoID from the path. The gate helpers + seeding
// logic live in this file so CreateRepository can reuse seedRepositorySettings.
// (The struct + constructor are defined near the bottom of the file
// alongside the migrate-enqueuer wiring.)

// SetRegistry attaches the live provider registry (wired after
// NewHandler, since the registry is built from env-gated provider
// maps). Called by api.Handler.SetProviderRegistry.
func (h *RepositorySettings) SetRegistry(r *ProviderRegistry) {
	h.registry = r
}

// ListPresets handles GET /repositories/presets.
//
// Returns the configured repository presets so the create UI can
// render the "Repository type" dropdown. Each entry includes the
// id, label, description, and a summary of which providers it
// enables and how many contexts it allows (with an `all_contexts`
// flag for the "all" sentinel). Authed-only (any logged-in user
// can create a repo and pick a preset).
func (h *RepositorySettings) ListPresets(w http.ResponseWriter, r *http.Request) {
	presets := h.deps.Config.RepositoryPresets
	out := make([]map[string]interface{}, 0, len(presets))
	for _, p := range presets {
		allContexts := false
		ctxCount := len(p.Contexts)
		if len(p.Contexts) == 1 && p.Contexts[0] == "all" {
			allContexts = true
			if h.deps.OntologySource != nil {
				if classes, err := h.deps.OntologySource.ContextClasses(r.Context()); err == nil {
					ctxCount = len(classes)
				}
			}
		}
		out = append(out, map[string]interface{}{
			"id":            p.ID,
			"label":         p.Label,
			"description":   p.Description,
			"providers":      p.Providers,
			"contexts":       p.Contexts,
			"all_contexts":   allContexts,
			"context_count":  ctxCount,
			"custom_contexts": p.CustomContexts,
			"custom_context_descriptions": p.CustomContextDescriptions,
		})
	}
	httputil.WriteJSON(w, http.StatusOK, map[string]interface{}{
		"presets":          out,
		"default_preset":   h.deps.Config.DefaultRepositoryPreset,
	})
}

// GetSettings handles GET /repositories/{repoID}/settings.
//
// Returns the live provider catalog (each tagged with whether it's
// stored + enabled for this repo, and whether it's orphaned) and
// the repo's context list (each with is_custom, description, and
// concept_count so the UI can block delete-while-populated).
func (h *RepositorySettings) GetSettings(w http.ResponseWriter, r *http.Request) {
	repoID, ok := parseRepoID(w, r)
	if !ok {
		return
	}

	providers, err := h.deps.Store.ListRepositoryProviderSettings(r.Context(), repoID)
	if err != nil {
		httputil.WriteError(w, http.StatusInternalServerError, "failed to list provider settings")
		return
	}
	// Index stored rows by [2]string{kind, id}.
	stored := make(map[[2]string]store.OktSystemRepositoryProviderSetting, len(providers))
	for _, p := range providers {
		stored[[2]string{p.ProviderKind, p.ProviderID}] = p
	}

	// Build the live list, tagging each with stored/enabled/orphaned.
	liveList := []map[string]interface{}{}
	if h.registry != nil {
		for _, lp := range h.registry.LiveProviders() {
			key := [2]string{lp.Kind, lp.ID}
			entry := map[string]interface{}{
				"kind":    lp.Kind,
				"id":      lp.ID,
				"name":    lp.Name,
				"stored":  false,
				"enabled": false,
				"orphaned": false,
			}
			if s, ok := stored[key]; ok {
				entry["stored"] = true
				entry["enabled"] = s.Enabled
			}
			liveList = append(liveList, entry)
		}
	}
	// Orphans: stored rows not in the live registry. The UI greys them.
	liveSet := make(map[[2]string]bool)
	if h.registry != nil {
		liveSet = h.registry.LiveProviderIDs()
	}
	orphanList := []map[string]interface{}{}
	for _, p := range providers {
		key := [2]string{p.ProviderKind, p.ProviderID}
		if !liveSet[key] {
			orphanList = append(orphanList, map[string]interface{}{
				"kind":    p.ProviderKind,
				"id":      p.ProviderID,
				"enabled": p.Enabled,
				"orphaned": true,
			})
		}
	}

	ctxRows, err := h.deps.Store.ListRepositoryContexts(r.Context(), repoID)
	if err != nil {
		httputil.WriteError(w, http.StatusInternalServerError, "failed to list contexts")
		return
	}
	contexts := make([]map[string]interface{}, 0, len(ctxRows))
	for _, c := range ctxRows {
		contexts = append(contexts, map[string]interface{}{
			"context":       c.Context,
			"is_custom":     c.IsCustom,
			"description":   c.Description,
			"created_at":    c.CreatedAt,
			"updated_at":    c.UpdatedAt,
			"concept_count": h.conceptCount(r.Context(), repoID, c.Context),
		})
	}

	registryConfigured := h.deps.Config.Providers.AnyRegistryConfigured()

	autoContribute, err := h.deps.Store.GetRepositoryAutoContribute(r.Context(), repoID)
	if err != nil {
		httputil.WriteError(w, http.StatusInternalServerError, "failed to read auto_contribute flag")
		return
	}

	// Per-repo registry integration: which configured registry this
	// repo uses and whether the cache/remote integration is on.
	// registry_id defaults to "default" (migration 0037); when the
	// column is still NULL on a backfilled row we surface "default"
	// so the UI's dropdown always has a selection.
	regCfg, err := h.deps.Store.GetRepositoryRegistryConfig(r.Context(), repoID)
	if err != nil {
		httputil.WriteError(w, http.StatusInternalServerError, "failed to read registry config")
		return
	}
	regID := "default"
	if regCfg.RegistryID != nil && *regCfg.RegistryID != "" {
		regID = *regCfg.RegistryID
	}
	// The dropdown options are the configured registry ids. When
	// the stored registry_id isn't in the configured list (e.g. the
	// operator removed a registry from config), we still surface
	// the stored value as the first option so the admin sees what's
	// set and can change it.
	options := h.deps.Config.Providers.RegistryIDs()
	if !registryIDInList(regID, options) {
		options = append([]string{regID}, options...)
	}

	// Context mapping summary (the dedicated GET /context-mappings
	// endpoint is the panel's refetch path; this is the page-load
	// view). Best-effort: a failure here doesn't fail the whole
	// settings read — the panel degrades to empty.
	mapRows, mapErr := h.deps.Store.ListRepositoryContextMappings(r.Context(), repoID)
	contextMappings := make([]map[string]interface{}, 0)
	mappedLocal := make(map[string]bool)
	if mapErr == nil {
		for _, m := range mapRows {
			contextMappings = append(contextMappings, map[string]interface{}{
				"local_context":    m.LocalContext,
				"registry_context": m.RegistryContext,
				"updated_at":       m.UpdatedAt,
			})
			mappedLocal[strings.ToLower(m.LocalContext)] = true
		}
	} else {
		log.Printf("repository_settings: listing context mappings for GetSettings: %v", mapErr)
	}

	policy, policyErr := h.deps.Store.GetUnmappedContextPolicy(r.Context(), repoID)
	unmappedPolicy := "skip"
	var catchAllCtx interface{}
	if policyErr == nil {
		unmappedPolicy = policy.UnmappedContextPolicy
		if policy.CatchAllContext != nil {
			catchAllCtx = *policy.CatchAllContext
		}
	} else {
		log.Printf("repository_settings: reading unmapped policy for GetSettings: %v", policyErr)
	}

	registryContexts := h.registryContextsForRepo(r.Context(), repoID)
	registrySet := make(map[string]bool, len(registryContexts))
	for _, c := range registryContexts {
		registrySet[strings.ToLower(c)] = true
	}
	unmappedLocal := make([]string, 0)
	for _, c := range ctxRows {
		lc := strings.ToLower(c.Context)
		if mappedLocal[lc] || registrySet[lc] {
			continue
		}
		unmappedLocal = append(unmappedLocal, c.Context)
	}

	// Per-repo model selection per task (migration 0039). Surface
	// each task_kind's selected model (override or NULL = inherit
	// global default) and the global default for display.
	modelRows, modelErr := h.deps.Store.ListRepositoryModelSettings(r.Context(), repoID)
	modelByKind := make(map[string]string, len(AllTaskKinds))
	if modelErr == nil {
		for _, m := range modelRows {
			if m.ModelID != nil {
				modelByKind[m.TaskKind] = *m.ModelID
			}
		}
	} else {
		log.Printf("repository_settings: listing model settings for GetSettings: %v", modelErr)
	}
	taskModelDefaults := map[string]string{
		TaskKindFactExtraction:    h.deps.Config.Providers.Decomposition.FactExtraction.Model,
		TaskKindImageExtraction:   h.deps.Config.Providers.Decomposition.ImageExtraction.Model,
		TaskKindConceptExtraction: h.deps.Config.Providers.Decomposition.ConceptExtraction.Model,
		TaskKindRefinement:       h.deps.Config.Providers.Refinement.Model,
		TaskKindSummarization:     h.deps.Config.Providers.Summarization.Model,
		TaskKindSynthesis:         h.deps.Config.Providers.Synthesis.Model,
		TaskKindReportAnnotation:  h.deps.Config.Providers.Reports.PostureClassifier.Model,
	}
	modelsSection := make([]map[string]interface{}, 0, len(AllTaskKinds))
	for _, kind := range AllTaskKinds {
		selected := modelByKind[kind]
		modelsSection = append(modelsSection, map[string]interface{}{
			"task_kind":  kind,
			"selected":   selected,
			"default":     taskModelDefaults[kind],
		})
	}
	modelCatalog := []CatalogModel{}
	if h.modelCatalog != nil {
		modelCatalog = h.modelCatalog.All()
	}

	// Per-repo allowed_models whitelist (migration 0040). NULL =
	// inherit global config; we surface the per-repo value (or nil)
	// and the global fallback for display.
	allowedModels, allowedErr := h.deps.Store.GetRepositoryAllowedModels(r.Context(), repoID)
	var allowedModelsVal interface{}
	var allowedModelsDefault []string
	if allowedErr == nil {
		if allowedModels != nil {
			allowedModelsVal = allowedModels
		} else {
			allowedModelsVal = nil
		}
	} else {
		log.Printf("repository_settings: reading allowed_models for GetSettings: %v", allowedErr)
		allowedModelsVal = nil
	}
	// The global fallback is the first configured registry's
	// allowed_models (the per-repo replaces global semantics).
	if regs := h.deps.Config.Providers.ResolveRegistries(); len(regs) > 0 {
		allowedModelsDefault = regs[0].AllowedModels
	}

	// Per-repo report annotation settings (migration 0041). NULL
	// similarity_threshold = inherit global default (0.84). Absent
	// row = inherit both threshold and posture_classifier_enabled.
	reportsSection := map[string]interface{}{
		"similarity_threshold":       nil,
		"posture_classifier_enabled": h.deps.Config.Providers.Reports.PostureClassifier.Enabled,
		"default_threshold":          h.deps.Config.Providers.Reports.SimilarityThresholdOr(0.84),
	}
	if repRow, repErr := h.deps.Store.GetRepositoryReportSettings(r.Context(), repoID); repErr == nil {
		if repRow.SimilarityThreshold != nil {
			reportsSection["similarity_threshold"] = *repRow.SimilarityThreshold
		}
		reportsSection["posture_classifier_enabled"] = repRow.PostureClassifierEnabled
	} else if !errors.Is(repErr, pgx.ErrNoRows) {
		log.Printf("repository_settings: reading report settings for GetSettings: %v", repErr)
	}

	// Per-repo push/pull sync levels (migration 0044). Controls how
	// much of a decomposition the repo contributes to and imports
	// from the registry. Defaults to "concepts" (full sync).
	syncLevels, syncErr := h.deps.Store.GetRepositorySyncLevels(r.Context(), repoID)
	pushLevel := string(registry.SyncLevelConcepts)
	pullLevel := string(registry.SyncLevelConcepts)
	if syncErr == nil {
		pushLevel = syncLevels.RegistryPushLevel
		pullLevel = syncLevels.RegistryPullLevel
	} else {
		log.Printf("repository_settings: reading sync levels for GetSettings: %v", syncErr)
	}

	// Per-repo allowed content types gate (migration 0049). NULL =
	// allow all (the default, backward compatible for existing
	// repos); a non-NULL array restricts to the listed kinds
	// ("document", "url", "doi"). Surfaced so the UI can render the
	// toggle and the ingestion handlers can 403-reject disallowed
	// types.
	allowedContentTypes, contentTypesErr := h.deps.Store.GetRepositoryAllowedContentTypes(r.Context(), repoID)
	var allowedContentTypesVal interface{}
	if contentTypesErr == nil {
		if allowedContentTypes != nil {
			allowedContentTypesVal = allowedContentTypes
		} else {
			allowedContentTypesVal = nil
		}
	} else {
		log.Printf("repository_settings: reading allowed_content_types for GetSettings: %v", contentTypesErr)
		allowedContentTypesVal = nil
	}

	// Per-repo contributor identity (migration 0050). Surfaced so
	// the settings page load shows the current state without a
	// second round-trip. Defaults to (nil, true) — anonymous — for
	// repos that haven't configured attribution.
	contributor, contributorErr := h.deps.Store.GetRepositoryContributor(r.Context(), repoID)
	var contributorDisplayName interface{}
	contributorAnonymous := true
	if contributorErr == nil {
		contributorAnonymous = contributor.ContributorAnonymous
		if contributor.ContributorDisplayName != nil {
			contributorDisplayName = *contributor.ContributorDisplayName
		} else {
			contributorDisplayName = nil
		}
	} else {
		log.Printf("repository_settings: reading contributor for GetSettings: %v", contributorErr)
	}

	httputil.WriteJSON(w, http.StatusOK, map[string]interface{}{
		"providers":             liveList,
		"orphaned_providers":    orphanList,
		"contexts":              contexts,
		"registry_configured":   registryConfigured,
		"registry_id":           regID,
		"registry_enabled":      regCfg.RegistryEnabled,
		"registry_options":      options,
		"auto_contribute":       autoContribute,
		"context_mappings":      contextMappings,
		"unmapped_policy":       unmappedPolicy,
		"catch_all_context":     catchAllCtx,
		"registry_contexts":     registryContexts,
		"unmapped_local":        unmappedLocal,
		"task_models":           modelsSection,
		"model_catalog":         modelCatalog,
		"allowed_models":        allowedModelsVal,
		"allowed_models_default": allowedModelsDefault,
		"reports":               reportsSection,
		"registry_push_level":   pushLevel,
		"registry_pull_level":   pullLevel,
		"allowed_content_types": allowedContentTypesVal,
		"contributor_display_name": contributorDisplayName,
		"contributor_anonymous":    contributorAnonymous,
	})
}

func registryIDInList(id string, list []string) bool {
	for _, x := range list {
		if x == id {
			return true
		}
	}
	return false
}

// SetProviderEnabled handles PUT /repositories/{repoID}/settings/providers.
//
// Upserts the enabled flag for a (kind, id) triple. The kind must
// be 'search' or 'resolution'; the id must be in the live registry
// (rejecting an orphan id with 400 keeps the settings table clean —
// the admin can only toggle providers that actually exist).
//
// After the upsert, the per-repo provider gate cache is invalidated
// (via the gateInvalidator callback wired by the wiring layer) so
// the toggle takes effect immediately on the next TestSearch /
// EnqueueRetrieveSource call instead of waiting for the 5-min TTL.
func (h *RepositorySettings) SetProviderEnabled(w http.ResponseWriter, r *http.Request) {
	repoID, ok := parseRepoID(w, r)
	if !ok {
		return
	}
	var body struct {
		ProviderKind string `json:"provider_kind"`
		ProviderID   string `json:"provider_id"`
		Enabled      bool   `json:"enabled"`
	}
	if err := httputil.DecodeBody(r, &body); err != nil {
		httputil.WriteError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if body.ProviderKind != ProviderKindSearch && body.ProviderKind != ProviderKindResolution {
		httputil.WriteError(w, http.StatusBadRequest, "provider_kind must be 'search' or 'resolution'")
		return
	}
	if body.ProviderID == "" {
		httputil.WriteError(w, http.StatusBadRequest, "provider_id is required")
		return
	}
	if h.registry != nil {
		if !h.registry.LiveProviderIDs()[[2]string{body.ProviderKind, body.ProviderID}] {
			httputil.WriteError(w, http.StatusBadRequest, "provider is not live in this deployment")
			return
		}
	}
	row, err := h.deps.Store.SetRepositoryProviderEnabled(r.Context(), store.SetRepositoryProviderEnabledParams{
		RepositoryID: repoID,
		ProviderKind: body.ProviderKind,
		ProviderID:   body.ProviderID,
		Enabled:      body.Enabled,
	})
	if err != nil {
		httputil.WriteError(w, http.StatusInternalServerError, "failed to update provider setting")
		return
	}
	if h.gateInvalidator != nil {
		h.gateInvalidator(repoID.String())
	}
	httputil.WriteJSON(w, http.StatusOK, row)
}

// SetModelSetting handles PUT /repositories/{repoID}/settings/models.
//
// Upserts the per-repo model override for a task kind. The body is
// { "task_kind": string, "model_id": string }. An empty model_id
// clears the override (revert to inheriting the global default).
// The task_kind must be one of the six generation tasks; model_id
// (when non-empty) must be in the model catalog.
func (h *RepositorySettings) SetModelSetting(w http.ResponseWriter, r *http.Request) {
	repoID, ok := parseRepoID(w, r)
	if !ok {
		return
	}
	var body struct {
		TaskKind string `json:"task_kind"`
		ModelID  string `json:"model_id"`
	}
	if err := httputil.DecodeBody(r, &body); err != nil {
		httputil.WriteError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	validKind := false
	for _, k := range AllTaskKinds {
		if k == body.TaskKind {
			validKind = true
			break
		}
	}
	if !validKind {
		httputil.WriteError(w, http.StatusBadRequest, "task_kind must be one of: fact_extraction, image_extraction, concept_extraction, alias_generation, summarization, synthesis, report_annotation")
		return
	}
	if body.ModelID != "" {
		if h.modelCatalog == nil || !h.modelCatalog.IsValid(body.ModelID) {
			httputil.WriteError(w, http.StatusBadRequest, "model_id is not in the configured model catalog")
			return
		}
	}
	if body.ModelID == "" {
		if err := h.deps.Store.DeleteRepositoryModelSetting(r.Context(), store.DeleteRepositoryModelSettingParams{
			RepositoryID: repoID,
			TaskKind:     body.TaskKind,
		}); err != nil {
			httputil.WriteError(w, http.StatusInternalServerError, "failed to clear model setting")
			return
		}
		httputil.WriteJSON(w, http.StatusOK, map[string]interface{}{
			"task_kind": body.TaskKind,
			"model_id":  nil,
		})
		return
	}
	row, err := h.deps.Store.UpsertRepositoryModelSetting(r.Context(), store.UpsertRepositoryModelSettingParams{
		RepositoryID: repoID,
		TaskKind:     body.TaskKind,
		ModelID:      &body.ModelID,
	})
	if err != nil {
		httputil.WriteError(w, http.StatusInternalServerError, "failed to update model setting")
		return
	}
	httputil.WriteJSON(w, http.StatusOK, row)
}

// GetReportSettings handles GET /repositories/{repoID}/settings/reports.
//
// Returns the per-repo report annotation settings (similarity_threshold,
// posture_classifier_enabled, max_facts_per_sentence,
// lexical_similarity_floor). Absent row = inherit global defaults
// (threshold 0.84, classifier on, max facts 5, lexical floor 0.6),
// surfaced as null overrides + the global default values.
func (h *RepositorySettings) GetReportSettings(w http.ResponseWriter, r *http.Request) {
	repoID, ok := parseRepoID(w, r)
	if !ok {
		return
	}
	globalMaxFacts := int32(h.deps.Config.Providers.Reports.MaxFactsPerSentenceOr(5))
	globalLexicalFloor := h.deps.Config.Providers.Reports.LexicalSimilarityFloorOr(0.6)
	out := map[string]interface{}{
		"similarity_threshold":       nil,
		"posture_classifier_enabled": h.deps.Config.Providers.Reports.PostureClassifier.Enabled,
		"default_threshold":          h.deps.Config.Providers.Reports.SimilarityThresholdOr(0.84),
		"max_facts_per_sentence":     nil,
		"default_max_facts":          globalMaxFacts,
		"lexical_similarity_floor":  nil,
		"default_lexical_floor":     globalLexicalFloor,
	}
	row, err := h.deps.Store.GetRepositoryReportSettings(r.Context(), repoID)
	if err == nil {
		if row.SimilarityThreshold != nil {
			out["similarity_threshold"] = *row.SimilarityThreshold
		}
		out["posture_classifier_enabled"] = row.PostureClassifierEnabled
		if row.MaxFactsPerSentence != nil {
			out["max_facts_per_sentence"] = *row.MaxFactsPerSentence
		}
		if row.LexicalSimilarityFloor != nil {
			out["lexical_similarity_floor"] = *row.LexicalSimilarityFloor
		}
	} else if !errors.Is(err, pgx.ErrNoRows) {
		httputil.WriteError(w, http.StatusInternalServerError, "failed to read report settings")
		return
	}
	httputil.WriteJSON(w, http.StatusOK, out)
}

// SetReportSettings handles PUT /repositories/{repoID}/settings/reports.
//
// Upserts the per-repo report annotation settings. The body is
// { "similarity_threshold": float64|null,
//   "posture_classifier_enabled": bool,
//   "max_facts_per_sentence": int|null,
//   "lexical_similarity_floor": float64|null }.
// A null for any numeric field clears that override (inherit global
// default). Ranges: similarity_threshold (0, 1]; max_facts_per_sentence
// [1, 50]; lexical_similarity_floor [0, 1] (0 disables the semantic
// gate on lexical hits entirely).
func (h *RepositorySettings) SetReportSettings(w http.ResponseWriter, r *http.Request) {
	repoID, ok := parseRepoID(w, r)
	if !ok {
		return
	}
	var body struct {
		SimilarityThreshold     *float64 `json:"similarity_threshold"`
		PostureClassifierEnabled *bool   `json:"posture_classifier_enabled"`
		MaxFactsPerSentence     *int32   `json:"max_facts_per_sentence"`
		LexicalSimilarityFloor  *float64 `json:"lexical_similarity_floor"`
	}
	if err := httputil.DecodeBody(r, &body); err != nil {
		httputil.WriteError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	var threshold *float64
	if body.SimilarityThreshold != nil {
		v := *body.SimilarityThreshold
		if v <= 0 || v > 1 {
			httputil.WriteError(w, http.StatusBadRequest, "similarity_threshold must be in (0, 1]")
			return
		}
		threshold = &v
	}
	var maxFacts *int32
	if body.MaxFactsPerSentence != nil {
		v := *body.MaxFactsPerSentence
		if v < 1 || v > 50 {
			httputil.WriteError(w, http.StatusBadRequest, "max_facts_per_sentence must be in [1, 50]")
			return
		}
		maxFacts = &v
	}
	var lexicalFloor *float64
	if body.LexicalSimilarityFloor != nil {
		v := *body.LexicalSimilarityFloor
		if v < 0 || v > 1 {
			httputil.WriteError(w, http.StatusBadRequest, "lexical_similarity_floor must be in [0, 1]")
			return
		}
		lexicalFloor = &v
	}
	enabled := h.deps.Config.Providers.Reports.PostureClassifier.Enabled
	if body.PostureClassifierEnabled != nil {
		enabled = *body.PostureClassifierEnabled
	}
	// When all four fields are at their inherit-null state (threshold
	// nil + enabled == global default + max_facts nil + lexical_floor
	// nil), drop the override row so the repo inherits cleanly.
	// Otherwise upsert.
	globalDefault := h.deps.Config.Providers.Reports.PostureClassifier.Enabled
	globalMaxFacts := int32(h.deps.Config.Providers.Reports.MaxFactsPerSentenceOr(5))
	globalLexicalFloor := h.deps.Config.Providers.Reports.LexicalSimilarityFloorOr(0.6)
	if threshold == nil && body.PostureClassifierEnabled == nil && maxFacts == nil && lexicalFloor == nil {
		_ = h.deps.Store.DeleteRepositoryReportSettings(r.Context(), repoID)
		httputil.WriteJSON(w, http.StatusOK, map[string]interface{}{
			"similarity_threshold":       nil,
			"posture_classifier_enabled": globalDefault,
			"default_threshold":          h.deps.Config.Providers.Reports.SimilarityThresholdOr(0.84),
			"max_facts_per_sentence":     nil,
			"default_max_facts":          globalMaxFacts,
			"lexical_similarity_floor":   nil,
			"default_lexical_floor":      globalLexicalFloor,
		})
		return
	}
	row, err := h.deps.Store.UpsertRepositoryReportSettings(r.Context(), store.UpsertRepositoryReportSettingsParams{
		RepositoryID:            repoID,
		SimilarityThreshold:     threshold,
		PostureClassifierEnabled: enabled,
		MaxFactsPerSentence:     maxFacts,
		LexicalSimilarityFloor:  lexicalFloor,
	})
	if err != nil {
		httputil.WriteError(w, http.StatusInternalServerError, "failed to update report settings")
		return
	}
	out := map[string]interface{}{
		"posture_classifier_enabled": row.PostureClassifierEnabled,
		"default_threshold":          h.deps.Config.Providers.Reports.SimilarityThresholdOr(0.84),
		"default_max_facts":          globalMaxFacts,
		"default_lexical_floor":      globalLexicalFloor,
	}
	if row.SimilarityThreshold != nil {
		out["similarity_threshold"] = *row.SimilarityThreshold
	} else {
		out["similarity_threshold"] = nil
	}
	if row.MaxFactsPerSentence != nil {
		out["max_facts_per_sentence"] = *row.MaxFactsPerSentence
	} else {
		out["max_facts_per_sentence"] = nil
	}
	if row.LexicalSimilarityFloor != nil {
		out["lexical_similarity_floor"] = *row.LexicalSimilarityFloor
	} else {
		out["lexical_similarity_floor"] = nil
	}
	httputil.WriteJSON(w, http.StatusOK, out)
}
//
// Adds a custom context (is_custom=TRUE) with a description. The
// label must not duplicate an existing context (case-insensitive).
// Standard contexts are seeded at creation, not added here;
// an admin who wants a standard context that wasn't in the preset
// can still add it (it lands is_custom=TRUE since the admin chose
// it, which is fine — the flag is informational, the gate only
// checks membership).
func (h *RepositorySettings) AddContext(w http.ResponseWriter, r *http.Request) {
	repoID, ok := parseRepoID(w, r)
	if !ok {
		return
	}
	var body struct {
		Context     string `json:"context"`
		Description string `json:"description"`
	}
	if err := httputil.DecodeBody(r, &body); err != nil {
		httputil.WriteError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if strings.TrimSpace(body.Context) == "" {
		httputil.WriteError(w, http.StatusBadRequest, "context is required")
		return
	}
	row, err := h.deps.Store.SeedRepositoryContext(r.Context(), store.SeedRepositoryContextParams{
		RepositoryID: repoID,
		Context:      strings.TrimSpace(body.Context),
		IsCustom:     true,
		Description:  body.Description,
	})
	if err != nil {
		httputil.WriteError(w, http.StatusInternalServerError, "failed to add context")
		return
	}
	if row.Context == "" {
		// ON CONFLICT DO NOTHING returns no rows; re-fetch for the
		// response so the UI gets the existing row's description.
		existing, gerr := h.deps.Store.GetRepositoryContext(r.Context(), store.GetRepositoryContextParams{RepositoryID: repoID, Context: strings.TrimSpace(body.Context)})
		if gerr != nil {
			httputil.WriteJSON(w, http.StatusOK, map[string]interface{}{"context": strings.TrimSpace(body.Context), "is_custom": true, "description": body.Description, "duplicate": true})
			return
		}
		row = existing
	}
	httputil.WriteJSON(w, http.StatusCreated, row)
}

// UpdateContext handles PUT /repositories/{repoID}/settings/contexts/{context}.
//
// Edits the description only (renaming a context is add-new +
// migrate + delete-old). The {context} URL param is the label
// (case-insensitive match); the body carries the new description.
func (h *RepositorySettings) UpdateContext(w http.ResponseWriter, r *http.Request) {
	repoID, ok := parseRepoID(w, r)
	if !ok {
		return
	}
	ctxLabel := chi.URLParam(r, "context")
	if ctxLabel == "" {
		httputil.WriteError(w, http.StatusBadRequest, "context is required")
		return
	}
	existing, err := h.deps.Store.GetRepositoryContext(r.Context(), store.GetRepositoryContextParams{RepositoryID: repoID, Context: ctxLabel})
	if err != nil {
		httputil.WriteError(w, http.StatusNotFound, "context not found")
		return
	}
	var body struct {
		Description string `json:"description"`
	}
	if err := httputil.DecodeBody(r, &body); err != nil {
		httputil.WriteError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	row, err := h.deps.Store.UpsertRepositoryContext(r.Context(), store.UpsertRepositoryContextParams{
		RepositoryID: repoID,
		Context:      existing.Context,
		IsCustom:     existing.IsCustom,
		Description:  body.Description,
	})
	if err != nil {
		httputil.WriteError(w, http.StatusInternalServerError, "failed to update context")
		return
	}
	httputil.WriteJSON(w, http.StatusOK, row)
}

// MigrateContext handles POST /repositories/{repoID}/settings/contexts/{context}/migrate.
//
// Enqueues a migrate_context job that re-assigns every concept under
// {context} to {target_context}, merging into an existing
// (canonical_name, target_context) concept where one exists. Returns
// 202 + job_id so the UI can poll for completion. The target must be
// a member of repository_contexts (the admin picks it from the
// dropdown); the old context is removed from repository_contexts
// after the merge completes.
func (h *RepositorySettings) MigrateContext(w http.ResponseWriter, r *http.Request) {
	repoID, ok := parseRepoID(w, r)
	if !ok {
		return
	}
	ctxLabel := chi.URLParam(r, "context")
	if ctxLabel == "" {
		httputil.WriteError(w, http.StatusBadRequest, "context is required")
		return
	}
	var body struct {
		TargetContext string `json:"target_context"`
	}
	if err := httputil.DecodeBody(r, &body); err != nil {
		httputil.WriteError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if strings.TrimSpace(body.TargetContext) == "" {
		httputil.WriteError(w, http.StatusBadRequest, "target_context is required")
		return
	}
	if strings.EqualFold(ctxLabel, body.TargetContext) {
		httputil.WriteError(w, http.StatusBadRequest, "target_context must differ from the source context")
		return
	}
	// Both source and target must exist in repository_contexts.
	if _, err := h.deps.Store.GetRepositoryContext(r.Context(), store.GetRepositoryContextParams{RepositoryID: repoID, Context: ctxLabel}); err != nil {
		httputil.WriteError(w, http.StatusNotFound, "source context not found")
		return
	}
	if _, err := h.deps.Store.GetRepositoryContext(r.Context(), store.GetRepositoryContextParams{RepositoryID: repoID, Context: body.TargetContext}); err != nil {
		httputil.WriteError(w, http.StatusBadRequest, "target_context is not an allowed context for this repository")
		return
	}
	// Enqueue via the task enqueuer. The handler doesn't import
	// taskmanager; it goes through the TaskEnqueuer interface the
	// wiring layer wires onto the source handler. We reach it via
	// a dedicated setter (SetMigrateEnqueuer) wired in wiring.go.
	if h.migrateEnqueuer == nil {
		httputil.WriteError(w, http.StatusServiceUnavailable, "task manager not configured")
		return
	}
	jobID, err := h.migrateEnqueuer.EnqueueMigrateContext(r.Context(), MigrateContextArgs{
		RepositoryID: repoID.String(),
		OldContext:   ctxLabel,
		NewContext:   body.TargetContext,
	})
	if err != nil {
		httputil.WriteError(w, http.StatusInternalServerError, err.Error())
		return
	}
	httputil.WriteJSON(w, http.StatusAccepted, map[string]interface{}{
		"job_id":        jobID,
		"old_context":   ctxLabel,
		"new_context":   body.TargetContext,
		"status":        "queued",
	})
}

// DeleteContext handles DELETE /repositories/{repoID}/settings/contexts/{context}.
//
// Refuses (409) when the context still has concepts — the admin must
// migrate first. After migrate, concept_count is 0 and the delete
// succeeds, removing the row from repository_contexts.
func (h *RepositorySettings) DeleteContext(w http.ResponseWriter, r *http.Request) {
	repoID, ok := parseRepoID(w, r)
	if !ok {
		return
	}
	ctxLabel := chi.URLParam(r, "context")
	if ctxLabel == "" {
		httputil.WriteError(w, http.StatusBadRequest, "context is required")
		return
	}
	count := h.conceptCount(r.Context(), repoID, ctxLabel)
	if count > 0 {
		httputil.WriteJSON(w, http.StatusConflict, map[string]interface{}{
			"error":         "context still has concepts; migrate them to another context first",
			"concept_count": count,
		})
		return
	}
	if err := h.deps.Store.DeleteRepositoryContext(r.Context(), store.DeleteRepositoryContextParams{
		RepositoryID: repoID,
		Context:      ctxLabel,
	}); err != nil {
		httputil.WriteError(w, http.StatusInternalServerError, "failed to delete context")
		return
	}
	// Cascade: drop any context mapping that referenced the deleted
	// local context so the mapping table doesn't dangle. Best-effort
	// (a failure here doesn't un-delete the context); the orphaned
	// mapping would surface as an unmapped local in the UI anyway.
	if err := h.deps.Store.DeleteRepositoryContextMapping(r.Context(), store.DeleteRepositoryContextMappingParams{
		RepositoryID: repoID,
		Lower:        ctxLabel,
	}); err != nil {
		log.Printf("repository_settings: deleting context mapping for %q: %v", ctxLabel, err)
	}
	httputil.WriteJSON(w, http.StatusOK, map[string]string{"message": "context deleted"})
}

// ──────────────────────────────────────────────────────────────
// Context mappings (local ↔ registry). See migration 0038.
// ──────────────────────────────────────────────────────────────

// registryContextsForRepo fetches the registry's canonical context
// vocabulary via the repo's configured registry client. Best-effort:
// returns an empty list (not an error) when the registry is
// unconfigured, the integration is off, or the fetch fails. The
// caller surfaces the empty list to the UI so the dropdown degrades
// gracefully; the admin can still store mappings (the handler
// accepts any non-empty registry_context when the vocab is empty).
func (h *RepositorySettings) registryContextsForRepo(ctx context.Context, repoID pgtype.UUID) []string {
	if h.registryClients == nil || !h.registryClients.IsConfigured() {
		return nil
	}
	regCfg, err := h.deps.Store.GetRepositoryRegistryConfig(ctx, repoID)
	if err != nil || !regCfg.RegistryEnabled {
		return nil
	}
	regID := "default"
	if regCfg.RegistryID != nil && *regCfg.RegistryID != "" {
		regID = *regCfg.RegistryID
	}
	rc, _, ok := h.registryClients.Client(regID)
	if !ok || !rc.IsConfigured() {
		return nil
	}
	labels, err := rc.ListContexts(ctx)
	if err != nil {
		log.Printf("repository_settings: fetching registry contexts for repo %s: %v", repoID, err)
		return nil
	}
	return labels
}

// ListContextMappings handles GET /repositories/{repoID}/settings/context-mappings.
//
// Returns the repo's local→registry mapping rows, the per-repo
// unmapped-context pull policy, the catch-all context (when set),
// the live registry context vocabulary (best-effort; empty when the
// registry is down/unconfigured), and the list of local contexts
// that have no mapping AND are not in the registry vocab (the
// "unmapped" list the UI surfaces for the admin to pick targets for).
func (h *RepositorySettings) ListContextMappings(w http.ResponseWriter, r *http.Request) {
	repoID, ok := parseRepoID(w, r)
	if !ok {
		return
	}
	rows, err := h.deps.Store.ListRepositoryContextMappings(r.Context(), repoID)
	if err != nil {
		httputil.WriteError(w, http.StatusInternalServerError, "failed to list context mappings")
		return
	}
	mappings := make([]map[string]interface{}, 0, len(rows))
	mappedLocal := make(map[string]bool, len(rows))
	for _, m := range rows {
		mappings = append(mappings, map[string]interface{}{
			"local_context":    m.LocalContext,
			"registry_context": m.RegistryContext,
			"updated_at":       m.UpdatedAt,
		})
		mappedLocal[strings.ToLower(m.LocalContext)] = true
	}

	policy, err := h.deps.Store.GetUnmappedContextPolicy(r.Context(), repoID)
	if err != nil {
		httputil.WriteError(w, http.StatusInternalServerError, "failed to read unmapped context policy")
		return
	}

	registryContexts := h.registryContextsForRepo(r.Context(), repoID)
	registrySet := make(map[string]bool, len(registryContexts))
	for _, c := range registryContexts {
		registrySet[strings.ToLower(c)] = true
	}

	// Compute the unmapped local list: local contexts with no
	// mapping row AND whose label is not in the registry vocab
	// (a local label that matches a registry label is pushed
	// verbatim, so it's not "unmapped" in the drift sense).
	ctxRows, err := h.deps.Store.ListRepositoryContexts(r.Context(), repoID)
	if err != nil {
		httputil.WriteError(w, http.StatusInternalServerError, "failed to list contexts")
		return
	}
	unmapped := make([]string, 0)
	for _, c := range ctxRows {
		lc := strings.ToLower(c.Context)
		if mappedLocal[lc] {
			continue
		}
		if registrySet[lc] {
			continue
		}
		unmapped = append(unmapped, c.Context)
	}

	resp := map[string]interface{}{
		"mappings":               mappings,
		"unmapped_policy":        policy.UnmappedContextPolicy,
		"registry_contexts":      registryContexts,
		"unmapped_local":         unmapped,
		"registry_configured":    h.registryClients != nil && h.registryClients.IsConfigured(),
	}
	if policy.CatchAllContext != nil {
		resp["catch_all_context"] = *policy.CatchAllContext
	}
	httputil.WriteJSON(w, http.StatusOK, resp)
}

// UpsertContextMapping handles PUT /repositories/{repoID}/settings/context-mappings.
//
// Adds or updates a local→registry mapping. The local context must
// be a member of repository_contexts (400 otherwise). When the
// registry is configured and its vocab is non-empty, the registry
// target must be in the vocab (400 otherwise, so the admin can't map
// to a phantom label). When the registry is unconfigured or its vocab
// is empty (e.g. the endpoint isn't live yet), any non-empty registry
// target is accepted — the mapping is stored for later validation.
func (h *RepositorySettings) UpsertContextMapping(w http.ResponseWriter, r *http.Request) {
	repoID, ok := parseRepoID(w, r)
	if !ok {
		return
	}
	var body struct {
		LocalContext    string `json:"local_context"`
		RegistryContext string `json:"registry_context"`
	}
	if err := httputil.DecodeBody(r, &body); err != nil {
		httputil.WriteError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	body.LocalContext = strings.TrimSpace(body.LocalContext)
	body.RegistryContext = strings.TrimSpace(body.RegistryContext)
	if body.LocalContext == "" {
		httputil.WriteError(w, http.StatusBadRequest, "local_context is required")
		return
	}
	if body.RegistryContext == "" {
		httputil.WriteError(w, http.StatusBadRequest, "registry_context is required")
		return
	}
	// The local context must be a member of repository_contexts.
	if _, err := h.deps.Store.GetRepositoryContext(r.Context(), store.GetRepositoryContextParams{
		RepositoryID: repoID, Context: body.LocalContext,
	}); err != nil {
		httputil.WriteError(w, http.StatusBadRequest, "local_context is not an allowed context for this repository")
		return
	}
	// When the registry is configured and its vocab is non-empty,
	// validate the target. An empty vocab (registry endpoint not
	// live, or registry genuinely has no contexts yet) means
	// validation is off — accept any non-empty target so the admin
	// can curate mappings before the registry is reachable.
	if registryContexts := h.registryContextsForRepo(r.Context(), repoID); len(registryContexts) > 0 {
		if !containsCI(registryContexts, body.RegistryContext) {
			httputil.WriteError(w, http.StatusBadRequest, "registry_context is not in the registry's vocabulary")
			return
		}
	}
	row, err := h.deps.Store.UpsertRepositoryContextMapping(r.Context(), store.UpsertRepositoryContextMappingParams{
		RepositoryID:    repoID,
		LocalContext:    body.LocalContext,
		RegistryContext: body.RegistryContext,
	})
	if err != nil {
		httputil.WriteError(w, http.StatusInternalServerError, "failed to upsert context mapping")
		return
	}
	httputil.WriteJSON(w, http.StatusOK, row)
}

// DeleteContextMappingHandler handles DELETE /repositories/{repoID}/settings/context-mappings/{localContext}.
//
// Removes a mapping by local context (case-insensitive). 404 when
// no row matches (the admin deleted something that wasn't there).
func (h *RepositorySettings) DeleteContextMappingHandler(w http.ResponseWriter, r *http.Request) {
	repoID, ok := parseRepoID(w, r)
	if !ok {
		return
	}
	localContext := chi.URLParam(r, "localContext")
	if localContext == "" {
		httputil.WriteError(w, http.StatusBadRequest, "localContext is required")
		return
	}
	// Check existence for a clean 404 (DeleteRepositoryContextMapping
	// is :exec and returns no rows).
	if _, err := h.deps.Store.GetRepositoryContextMappingByLocal(r.Context(), store.GetRepositoryContextMappingByLocalParams{
		RepositoryID: repoID, Lower: localContext,
	}); err != nil {
		httputil.WriteError(w, http.StatusNotFound, "mapping not found")
		return
	}
	if err := h.deps.Store.DeleteRepositoryContextMapping(r.Context(), store.DeleteRepositoryContextMappingParams{
		RepositoryID: repoID, Lower: localContext,
	}); err != nil {
		httputil.WriteError(w, http.StatusInternalServerError, "failed to delete context mapping")
		return
	}
	httputil.WriteJSON(w, http.StatusOK, map[string]string{"message": "mapping deleted"})
}

// SetUnmappedPolicy handles PUT /repositories/{repoID}/settings/unmapped-policy.
//
// Sets the per-repo pull policy for unmapped registry contexts.
// 'skip' drops the concept; 'auto_add' seeds a new
// repository_contexts row named after the registry label; 'catch_all'
// routes unmapped contexts to catch_all_context, which must be a
// member of repository_contexts (400 otherwise).
func (h *RepositorySettings) SetUnmappedPolicy(w http.ResponseWriter, r *http.Request) {
	repoID, ok := parseRepoID(w, r)
	if !ok {
		return
	}
	var body struct {
		Policy          string  `json:"policy"`
		CatchAllContext *string `json:"catch_all_context"`
	}
	if err := httputil.DecodeBody(r, &body); err != nil {
		httputil.WriteError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	switch body.Policy {
	case "skip", "auto_add", "catch_all":
	default:
		httputil.WriteError(w, http.StatusBadRequest, "policy must be 'skip', 'auto_add', or 'catch_all'")
		return
	}
	var catchAll *string
	if body.Policy == "catch_all" {
		if body.CatchAllContext == nil || strings.TrimSpace(*body.CatchAllContext) == "" {
			httputil.WriteError(w, http.StatusBadRequest, "catch_all_context is required when policy is 'catch_all'")
			return
		}
		label := strings.TrimSpace(*body.CatchAllContext)
		if _, err := h.deps.Store.GetRepositoryContext(r.Context(), store.GetRepositoryContextParams{
			RepositoryID: repoID, Context: label,
		}); err != nil {
			httputil.WriteError(w, http.StatusBadRequest, "catch_all_context is not an allowed context for this repository")
			return
		}
		catchAll = &label
	}
	if err := h.deps.Store.SetUnmappedContextPolicy(r.Context(), store.SetUnmappedContextPolicyParams{
		ID:              repoID,
		UnmappedContextPolicy: body.Policy,
		CatchAllContext: catchAll,
	}); err != nil {
		httputil.WriteError(w, http.StatusInternalServerError, "failed to update unmapped context policy")
		return
	}
	resp := map[string]interface{}{"unmapped_policy": body.Policy}
	if catchAll != nil {
		resp["catch_all_context"] = *catchAll
	}
	httputil.WriteJSON(w, http.StatusOK, resp)
}

// containsCI reports whether s is in list (case-insensitive).
func containsCI(list []string, s string) bool {
	for _, x := range list {
		if strings.EqualFold(x, s) {
			return true
		}
	}
	return false
}

// checkRegistryConfigured returns an error when no registry is
// configured globally. Called by ContributeAll and PullAllFromRegistry
// so the endpoints return 400 before enqueuing jobs that would fail.
func (h *RepositorySettings) checkRegistryConfigured() error {
	if h.deps.Config == nil {
		return fmt.Errorf("config not loaded")
	}
	if !h.deps.Config.Providers.AnyRegistryConfigured() {
		return fmt.Errorf("registry not configured")
	}
	return nil
}

// checkRepoRegistryEnabled returns an error when the per-repo
// registry integration is off. Used by ContributeAll,
// PullAllFromRegistry, and SetAutoContribute (enabling) to refuse
// operations that would silently no-op because the repo has turned
// the integration off.
func (h *RepositorySettings) checkRepoRegistryEnabled(ctx context.Context, repoID pgtype.UUID) error {
	cfg, err := h.deps.Store.GetRepositoryRegistryConfig(ctx, repoID)
	if err != nil {
		return fmt.Errorf("reading repository registry config: %w", err)
	}
	if !cfg.RegistryEnabled {
		return fmt.Errorf("registry integration is disabled for this repository")
	}
	return nil
}

// ContributeAll handles POST /repositories/{repoID}/settings/contribute-all.
// Enqueues a contribute_all job that pushes every processed source in the
// repo to the registry. Returns 202 + job_id.
func (h *RepositorySettings) ContributeAll(w http.ResponseWriter, r *http.Request) {
	repoID, ok := parseRepoID(w, r)
	if !ok {
		return
	}
	if err := h.checkRegistryConfigured(); err != nil {
		httputil.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := h.checkRepoRegistryEnabled(r.Context(), repoID); err != nil {
		httputil.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}
	if h.registrySyncEnqueuer == nil {
		httputil.WriteError(w, http.StatusServiceUnavailable, "task manager not configured")
		return
	}
	jobID, err := h.registrySyncEnqueuer.EnqueueContributeAll(r.Context(), ContributeAllArgs{
		RepositoryID: repoID.String(),
	})
	if err != nil {
		httputil.WriteError(w, http.StatusInternalServerError, err.Error())
		return
	}
	httputil.WriteJSON(w, http.StatusAccepted, map[string]interface{}{
		"job_id": jobID,
		"status": "queued",
	})
}

// PullAllFromRegistry handles POST /repositories/{repoID}/settings/pull-all.
// Enqueues a pull_all_from_registry job that checks every source in the
// repo against the registry and imports any available decompositions.
// Returns 202 + job_id.
func (h *RepositorySettings) PullAllFromRegistry(w http.ResponseWriter, r *http.Request) {
	repoID, ok := parseRepoID(w, r)
	if !ok {
		return
	}
	if err := h.checkRegistryConfigured(); err != nil {
		httputil.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := h.checkRepoRegistryEnabled(r.Context(), repoID); err != nil {
		httputil.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}
	if h.registrySyncEnqueuer == nil {
		httputil.WriteError(w, http.StatusServiceUnavailable, "task manager not configured")
		return
	}
	jobID, err := h.registrySyncEnqueuer.EnqueuePullAll(r.Context(), PullAllArgs{
		RepositoryID: repoID.String(),
	})
	if err != nil {
		httputil.WriteError(w, http.StatusInternalServerError, err.Error())
		return
	}
	httputil.WriteJSON(w, http.StatusAccepted, map[string]interface{}{
		"job_id": jobID,
		"status": "queued",
	})
}

// SetAutoContribute handles PUT /repositories/{repoID}/settings/auto-contribute.
//
// Toggles the per-repo auto_contribute flag. When true, the
// cleanup_facts worker chains a contribute_source job for every
// source that finishes the ingestion pipeline, pushing its
// decomposition to the registry automatically. When false (the
// default), sources are only pushed via the manual "Push All to
// Registry" endpoint.
//
// Enabling requires the registry to be configured (URL set); the
// endpoint returns 400 otherwise so the toggle can't be left in a
// state where the auto-chain would no-op silently.
func (h *RepositorySettings) SetAutoContribute(w http.ResponseWriter, r *http.Request) {
	repoID, ok := parseRepoID(w, r)
	if !ok {
		return
	}
	var body struct {
		Enabled bool `json:"enabled"`
	}
	if err := httputil.DecodeBody(r, &body); err != nil {
		httputil.WriteError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if body.Enabled {
		if err := h.checkRegistryConfigured(); err != nil {
			httputil.WriteError(w, http.StatusBadRequest, err.Error())
			return
		}
		if err := h.checkRepoRegistryEnabled(r.Context(), repoID); err != nil {
			httputil.WriteError(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	if err := h.deps.Store.SetRepositoryAutoContribute(r.Context(), store.SetRepositoryAutoContributeParams{
		ID:             repoID,
		AutoContribute:  body.Enabled,
	}); err != nil {
		httputil.WriteError(w, http.StatusInternalServerError, "failed to update auto_contribute")
		return
	}
	httputil.WriteJSON(w, http.StatusOK, map[string]interface{}{
		"auto_contribute": body.Enabled,
	})
}

// SetRegistrySettings handles PUT /repositories/{repoID}/settings/registry.
//
// Updates the per-repo registry selector and the on/off toggle in
// one call. The body is { "registry_id": string, "enabled": bool }.
// Both fields are optional; omitting one leaves it unchanged.
//
// Validation:
//   - When `enabled` is true, `registry_id` (if provided) must be in
//     the configured registries list. When `enabled` is true and
//     `registry_id` is omitted, the existing value is kept.
//   - When `enabled` is true, at least one registry must be
//     configured globally (the toggle can't be on with no registry
//     to talk to).
//   - Enabling auto_contribute requires registry_enabled=true, so
//     turning the integration off also turns auto_contribute off
//     (defense-in-depth — the cleanup_facts worker also checks).
//
// After the upsert, the per-repo provider gate cache is invalidated
// so the change takes effect immediately on the next request.
func (h *RepositorySettings) SetRegistrySettings(w http.ResponseWriter, r *http.Request) {
	repoID, ok := parseRepoID(w, r)
	if !ok {
		return
	}
	var body struct {
		RegistryID    *string   `json:"registry_id"`
		Enabled       *bool     `json:"enabled"`
		AllowedModels *[]string `json:"allowed_models"`
	}
	// Read the raw body first so we can detect whether allowed_models
	// was explicitly present (even as null) vs absent. Go's *[]string
	// can't distinguish these two cases.
	rawBody, rawErr := io.ReadAll(r.Body)
	if rawErr != nil {
		httputil.WriteError(w, http.StatusBadRequest, "failed to read request body")
		return
	}
	if err := json.Unmarshal(rawBody, &body); err != nil {
		httputil.WriteError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	var rawPresent struct {
		AllowedModels json.RawMessage `json:"allowed_models"`
	}
	_ = json.Unmarshal(rawBody, &rawPresent)
	amPresent := len(rawPresent.AllowedModels) > 0
	if body.RegistryID == nil && body.Enabled == nil && !amPresent {
		httputil.WriteError(w, http.StatusBadRequest, "must set registry_id, enabled, or allowed_models")
		return
	}

	// Resolve the current values so we can compute the new ones and
	// validate the combination.
	cur, err := h.deps.Store.GetRepositoryRegistryConfig(r.Context(), repoID)
	if err != nil {
		httputil.WriteError(w, http.StatusInternalServerError, "failed to read current registry config")
		return
	}
	curID := "default"
	if cur.RegistryID != nil && *cur.RegistryID != "" {
		curID = *cur.RegistryID
	}
	newID := curID
	if body.RegistryID != nil {
		newID = strings.TrimSpace(*body.RegistryID)
		if newID == "" {
			newID = "default"
		}
	}
	newEnabled := cur.RegistryEnabled
	if body.Enabled != nil {
		newEnabled = *body.Enabled
	}

	if newEnabled {
		if !h.deps.Config.Providers.AnyRegistryConfigured() {
			httputil.WriteError(w, http.StatusBadRequest, "no registry is configured; cannot enable the integration")
			return
		}
		// When enabling, the selected id must be a configured
		// registry. (When disabling we accept any stored id so the
		// admin can turn the integration off even if the configured
		// list changed.)
		if _, ok := h.deps.Config.Providers.RegistryByID(newID); !ok {
			httputil.WriteError(w, http.StatusBadRequest, "registry_id is not in the configured registries list")
			return
		}
	}

	// Persist the registry_id (only when non-empty — the column is
	// TEXT, not NOT NULL, but we never want to write NULL back).
	if newID != "" {
		if err := h.deps.Store.SetRepositoryRegistryID(r.Context(), store.SetRepositoryRegistryIDParams{
			ID:         repoID,
			RegistryID: &newID,
		}); err != nil {
			httputil.WriteError(w, http.StatusInternalServerError, "failed to update registry_id")
			return
		}
	}
	if body.Enabled != nil {
		if err := h.deps.Store.SetRepositoryRegistryEnabled(r.Context(), store.SetRepositoryRegistryEnabledParams{
			ID:              repoID,
			RegistryEnabled: newEnabled,
		}); err != nil {
			httputil.WriteError(w, http.StatusInternalServerError, "failed to update registry_enabled")
			return
		}
		// Turning the integration off also turns auto_contribute off
		// — the cleanup_facts worker checks registry_enabled too,
		// but we keep the flag consistent so the UI doesn't show
		// "auto-contribute enabled" while the integration is off.
		if !newEnabled {
			if err := h.deps.Store.SetRepositoryAutoContribute(r.Context(), store.SetRepositoryAutoContributeParams{
				ID:             repoID,
				AutoContribute: false,
			}); err != nil {
				log.Printf("repository_settings: clearing auto_contribute on registry disable: %v", err)
			}
		}
	}

	if amPresent {
		var am []string
		if body.AllowedModels != nil {
			am = *body.AllowedModels
		}
		if err := h.deps.Store.SetRepositoryAllowedModels(r.Context(), store.SetRepositoryAllowedModelsParams{
			ID:            repoID,
			AllowedModels: am,
		}); err != nil {
			httputil.WriteError(w, http.StatusInternalServerError, "failed to update allowed_models")
			return
		}
	}

	if h.gateInvalidator != nil {
		h.gateInvalidator(repoID.String())
	}

	httputil.WriteJSON(w, http.StatusOK, map[string]interface{}{
		"registry_id":      newID,
		"registry_enabled": newEnabled,
		"allowed_models":   body.AllowedModels,
	})
}

// SetSyncLevels handles PUT /repositories/{repoID}/settings/sync-levels.
//
// Updates the per-repo push and pull sync levels in one call. The body
// is { "push_level": string, "pull_level": string }. Both fields are
// optional; omitting one leaves it unchanged. Each level must be one
// of "facts" or "concepts" (case-insensitive); any other value is a
// 400. The levels are cumulative: "facts" includes sources + facts +
// fact embeddings; "concepts" adds concepts, links, and concept
// embeddings.
//
// After the update, the per-repo provider gate cache is invalidated
// so the change takes effect on the next contribute/pull job.
func (h *RepositorySettings) SetSyncLevels(w http.ResponseWriter, r *http.Request) {
	repoID, ok := parseRepoID(w, r)
	if !ok {
		return
	}
	var body struct {
		PushLevel *string `json:"push_level"`
		PullLevel *string `json:"pull_level"`
	}
	if err := httputil.DecodeBody(r, &body); err != nil {
		httputil.WriteError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if body.PushLevel == nil && body.PullLevel == nil {
		httputil.WriteError(w, http.StatusBadRequest, "must set push_level or pull_level")
		return
	}

	// Resolve current values so an omitted field keeps its value.
	cur, err := h.deps.Store.GetRepositorySyncLevels(r.Context(), repoID)
	if err != nil {
		httputil.WriteError(w, http.StatusInternalServerError, "failed to read current sync levels")
		return
	}
	newPush := cur.RegistryPushLevel
	newPull := cur.RegistryPullLevel
	if body.PushLevel != nil {
		p := registry.ParseSyncLevel(*body.PushLevel)
		if !registry.ValidSyncLevels[p] {
			httputil.WriteError(w, http.StatusBadRequest, "push_level must be 'facts' or 'concepts'")
			return
		}
		newPush = string(p)
	}
	if body.PullLevel != nil {
		p := registry.ParseSyncLevel(*body.PullLevel)
		if !registry.ValidSyncLevels[p] {
			httputil.WriteError(w, http.StatusBadRequest, "pull_level must be 'facts' or 'concepts'")
			return
		}
		newPull = string(p)
	}

	if err := h.deps.Store.SetRepositorySyncLevels(r.Context(), store.SetRepositorySyncLevelsParams{
		ID:                repoID,
		RegistryPushLevel: newPush,
		RegistryPullLevel: newPull,
	}); err != nil {
		httputil.WriteError(w, http.StatusInternalServerError, "failed to update sync levels")
		return
	}

	if h.gateInvalidator != nil {
		h.gateInvalidator(repoID.String())
	}

	httputil.WriteJSON(w, http.StatusOK, map[string]interface{}{
		"registry_push_level": newPush,
		"registry_pull_level": newPull,
	})
}

// SetContentTypes handles PUT /repositories/{repoID}/settings/content-types.
//
// Updates the per-repo allowed content types gate (migration 0049).
// The body is { "allowed_content_types": ["document","url","doi"] | null }.
// Pass null (or omit the field) to clear the override and allow all
// content types (the default, backward compatible). Pass an array
// to restrict: a repo with ["doi"] only accepts DOI-identified
// sources; ["document","url"] accepts uploads and URLs but not DOIs.
//
// Validation:
//   - Each value must be one of "document", "url", "doi" (the only
//     members of the column's CHECK constraint). Any other value is
//     a 400.
//   - An empty array is rejected (use null to reset to allow-all).
//   - Duplicates are rejected.
//
// After the update, the per-repo provider gate cache is invalidated
// so the change takes effect on the next ingestion request.
func (h *RepositorySettings) SetContentTypes(w http.ResponseWriter, r *http.Request) {
	repoID, ok := parseRepoID(w, r)
	if !ok {
		return
	}
	// Read the raw body first so we can detect whether
	// allowed_content_types was explicitly present (even as null)
	// vs absent. Mirrors the SetRegistrySettings pattern.
	rawBody, rawErr := io.ReadAll(r.Body)
	if rawErr != nil {
		httputil.WriteError(w, http.StatusBadRequest, "failed to read request body")
		return
	}
	var body struct {
		AllowedContentTypes *[]string `json:"allowed_content_types"`
	}
	if err := json.Unmarshal(rawBody, &body); err != nil {
		httputil.WriteError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	var rawPresent struct {
		AllowedContentTypes json.RawMessage `json:"allowed_content_types"`
	}
	_ = json.Unmarshal(rawBody, &rawPresent)
	present := len(rawPresent.AllowedContentTypes) > 0
	if !present {
		httputil.WriteError(w, http.StatusBadRequest, "must set allowed_content_types (array or null)")
		return
	}

	// null = clear the override (allow all). A non-null array is
	// validated: each value must be a known content kind, no
	// duplicates, no empty array.
	var types []string
	if body.AllowedContentTypes != nil {
		types = *body.AllowedContentTypes
		seen := map[string]bool{}
		for _, v := range types {
			if !ValidContentKinds[v] {
				httputil.WriteError(w, http.StatusBadRequest, "allowed_content_types: invalid value "+v+" (must be one of document, url, doi)")
				return
			}
			if seen[v] {
				httputil.WriteError(w, http.StatusBadRequest, "allowed_content_types: duplicate value "+v)
				return
			}
			seen[v] = true
		}
		if len(types) == 0 {
			httputil.WriteError(w, http.StatusBadRequest, "allowed_content_types: empty array is not allowed (use null to allow all)")
			return
		}
	}

	if err := h.deps.Store.SetRepositoryAllowedContentTypes(r.Context(), store.SetRepositoryAllowedContentTypesParams{
		ID:                  repoID,
		AllowedContentTypes: types,
	}); err != nil {
		httputil.WriteError(w, http.StatusInternalServerError, "failed to update allowed_content_types")
		return
	}

	if h.gateInvalidator != nil {
		h.gateInvalidator(repoID.String())
	}

	httputil.WriteJSON(w, http.StatusOK, map[string]interface{}{
		"allowed_content_types": body.AllowedContentTypes,
	})
}
// enqueue. The taskmanager package mirrors it in a River JobArgs
// type; this struct stays in handler to keep the HTTP layer
// independent of River internals (same pattern as RetrieveSourceArgs).
type MigrateContextArgs struct {
	RepositoryID string `json:"repository_id"`
	OldContext   string `json:"old_context"`
	NewContext    string `json:"new_context"`
}

// MigrateEnqueuer is the minimal contract the settings handler
// needs from the task manager to enqueue a migrate_context job.
// taskmanager.MigrateEnqueuerAdapter satisfies it.
type MigrateEnqueuer interface {
	EnqueueMigrateContext(ctx context.Context, args MigrateContextArgs) (string, error)
}

// SetMigrateEnqueuer wires the migrate-context enqueuer. Called by
// api.Handler.SetTaskEnqueuer (the same wiring path that wires the
// source handler's enqueuer) so there's one place in cmd/app/api.go
// that wires task enqueueing.
func (h *RepositorySettings) SetMigrateEnqueuer(eq MigrateEnqueuer) {
	h.migrateEnqueuer = eq
}

// ContributeAllArgs is the wire shape for the contribute-all enqueue.
type ContributeAllArgs struct {
	RepositoryID string `json:"repository_id"`
}

// PullAllArgs is the wire shape for the pull-all-from-registry enqueue.
type PullAllArgs struct {
	RepositoryID string `json:"repository_id"`
}

// RegistrySyncEnqueuer is the minimal contract the settings handler
// needs from the task manager to enqueue contribute-all and pull-all
// jobs. taskmanager.RegistrySyncEnqueuerAdapter satisfies it.
type RegistrySyncEnqueuer interface {
	EnqueueContributeAll(ctx context.Context, args ContributeAllArgs) (string, error)
	EnqueuePullAll(ctx context.Context, args PullAllArgs) (string, error)
}

// SetRegistrySyncEnqueuer wires the registry-sync enqueuer. Called
// by api.Handler.SetRegistrySyncEnqueuer in wiring.go, which itself
// is called from cmd/app/api.go.
func (h *RepositorySettings) SetRegistrySyncEnqueuer(eq RegistrySyncEnqueuer) {
	h.registrySyncEnqueuer = eq
}

// SetGateInvalidator wires the callback that drops the per-repo
// provider gate cache after a successful toggle. Called by
// api.Handler.SetGateInvalidator in wiring.go, which forwards
// (*Handler).InvalidateProviderGate. Idempotent and safe to call
// with nil (the toggle handler checks before calling).
func (h *RepositorySettings) SetGateInvalidator(fn func(string)) {
	h.gateInvalidator = fn
}

// conceptCount returns the number of concepts in a repo assigned to a
// given context. Used by GetSettings (to show the count) and
// DeleteContext (to refuse while populated). Best-effort: a query
// error logs and returns 0 so a transient DB issue doesn't wedge the
// settings UI (the delete endpoint re-checks, so a false 0 just means
// the delete will 409 server-side if there really are concepts).
func (h *RepositorySettings) conceptCount(ctx context.Context, repoID pgtype.UUID, contextLabel string) int64 {
	// The CountConceptsByContext query runs against the per-repo
	// pool (concepts live in okt_repository on the repo's database).
	// Resolve the pool the same way the per-repo middleware does.
	if h.deps.Registry == nil {
		return 0
	}
	dbName, err := h.deps.Store.GetRepositoryDatabaseName(ctx, repoID)
	if err != nil {
		log.Printf("repository_settings: resolving database for concept count: %v", err)
		return 0
	}
	pool := h.deps.Registry.Get(dbName)
	if pool == nil || pool.Pool == nil {
		return 0
	}
	count, err := store.New(pool.Pool).CountConceptsByContext(ctx, store.CountConceptsByContextParams{
		RepositoryID: repoID,
		Context:      contextLabel,
	})
	if err != nil {
		log.Printf("repository_settings: counting concepts for context %q: %v", contextLabel, err)
		return 0
	}
	return count
}

// parseRepoID reads the {repoID} URL param and parses it to a UUID.
// Shared by all settings handlers since they all start with the same
// repoID resolution + validation.
func parseRepoID(w http.ResponseWriter, r *http.Request) (pgtype.UUID, bool) {
	repoID := chi.URLParam(r, "repoID")
	var uid pgtype.UUID
	if err := uid.Scan(repoID); err != nil {
		httputil.WriteError(w, http.StatusBadRequest, "invalid repository id")
		return uid, false
	}
	return uid, true
}

// migrateEnqueuer is set by SetMigrateEnqueuer; nil when the task
// manager isn't wired (tests). The MigrateContext handler checks
// for nil and returns 503.
type RepositorySettings struct {
	deps                Deps
	registry            *ProviderRegistry
	registryClients     *registry.ClientMap
	migrateEnqueuer     MigrateEnqueuer
	registrySyncEnqueuer RegistrySyncEnqueuer
	// gateInvalidator is called after a successful SetProviderEnabled
	// to drop the per-repo gate cache entry so the toggle takes effect
	// immediately. Wired by the wiring layer via SetGateInvalidator.
	gateInvalidator     func(string)
	// modelCatalog is the runtime catalog of configured AI models.
	// Built from cfg.Providers.AI.Models at wiring time and set via
	// SetModelCatalog. Nil is safe — the models section of GetSettings
	// returns an empty catalog and SetModelSetting returns 400.
	modelCatalog         *ModelCatalog
	// promptsetResolver validates the active + accepted promptset
	// hashes against the live catalog (built-in + DB). Set via
	// SetPromptsetResolver. Nil is safe — GetPromptset returns the
	// stored values and SetPromptset returns 503.
	promptsetResolver    *promptset.Resolver
}

func NewRepositorySettings(d Deps) *RepositorySettings {
	return &RepositorySettings{deps: d, registry: d.ProviderRegistry}
}

// SetModelCatalog wires the AI model catalog (built from
// cfg.Providers.AI.Models). Called by api.Handler.SetModelCatalog
// in wiring.go. Nil is safe — the models section of GetSettings
// returns an empty catalog and SetModelSetting returns 400.
func (h *RepositorySettings) SetModelCatalog(c *ModelCatalog) {
	h.modelCatalog = c
}

// SetRegistryClients wires the per-registry client map (built from
// cfg.Providers.ResolveRegistries). Called by api.Handler.SetRemote
// in wiring.go after the client map is constructed. Nil is safe —
// the registry settings handlers treat a nil map as "no registries
// configured" and the gate returns 400 on enable/selector changes.
func (h *RepositorySettings) SetRegistryClients(m *registry.ClientMap) {
	h.registryClients = m
}

// SetPromptsetResolver wires the promptset resolver (built-in + DB)
// used by GetPromptset / SetPromptset to validate the active +
// accepted hashes against the live catalog. Called by
// api.Handler.SetPromptsetResolver in wiring.go. Nil is safe —
// GetPromptset returns the stored values unchanged and SetPromptset
// returns 503.
func (h *RepositorySettings) SetPromptsetResolver(r *promptset.Resolver) {
	h.promptsetResolver = r
}

// GetPromptset handles GET /repositories/{repoID}/settings/promptset.
//
// Returns the repo's active_promptset_hash (NULL = inherit global
// default) and accepted_promptset_hashes (the cache-admit set for
// registry pull). The response also includes the resolved
// effective_hash (what the repo actually runs under after the
// global-default fallback) so the UI can show the current
// philosophy without re-resolving.
func (h *RepositorySettings) GetPromptset(w http.ResponseWriter, r *http.Request) {
	repoID, ok := parseRepoID(w, r)
	if !ok {
		return
	}
	row, err := h.deps.Store.GetRepositoryPromptset(r.Context(), repoID)
	out := map[string]interface{}{
		"active_hash":              nil,
		"accepted_hashes":          []string{},
		"effective_hash":           promptset.DefaultHash,
		"global_default_hash":      globalPromptsetDefault(h, promptset.DefaultHash),
	}
	if err == nil {
		if row.ActivePromptsetHash != nil {
			out["active_hash"] = *row.ActivePromptsetHash
			out["effective_hash"] = *row.ActivePromptsetHash
		}
		if row.AcceptedPromptsetHashes != nil {
			out["accepted_hashes"] = row.AcceptedPromptsetHashes
		}
	} else if !errors.Is(err, pgx.ErrNoRows) {
		httputil.WriteError(w, http.StatusInternalServerError, "failed to read repository promptset")
		return
	}
	httputil.WriteJSON(w, http.StatusOK, out)
}

// SetPromptset handles PUT /repositories/{repoID}/settings/promptset.
//
// Updates the repo's active_promptset_hash and
// accepted_promptset_hashes. The body is
// { "active_hash": string|null, "accepted_hashes": []string|null }.
// A null active_hash clears the override (inherit global default);
// a null accepted_hashes clears the accepted set (only the active
// hash is accepted). Every non-empty hash must be known to the
// resolver (built-in or a promptset the caller owns / is public).
func (h *RepositorySettings) SetPromptset(w http.ResponseWriter, r *http.Request) {
	repoID, ok := parseRepoID(w, r)
	if !ok {
		return
	}
	if h.promptsetResolver == nil {
		httputil.WriteError(w, http.StatusServiceUnavailable, "promptset resolver not configured")
		return
	}
	var body struct {
		ActiveHash     *string  `json:"active_hash"`
		AcceptedHashes *[]string `json:"accepted_hashes"`
	}
	if err := httputil.DecodeBody(r, &body); err != nil {
		httputil.WriteError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	// Validate active_hash.
	var active *string
	if body.ActiveHash != nil && *body.ActiveHash != "" {
		hash := strings.TrimSpace(*body.ActiveHash)
		if !h.promptsetResolver.Has(hash) {
			httputil.WriteError(w, http.StatusBadRequest, "active_hash is not a known promptset")
			return
		}
		active = &hash
	}
	// Validate accepted_hashes.
	var accepted []string
	if body.AcceptedHashes != nil {
		accepted = make([]string, 0, len(*body.AcceptedHashes))
		for _, h2 := range *body.AcceptedHashes {
			h2 = strings.TrimSpace(h2)
			if h2 == "" {
				continue
			}
			if !h.promptsetResolver.Has(h2) {
				httputil.WriteError(w, http.StatusBadRequest, "accepted_hashes contains an unknown promptset: "+h2)
				return
			}
			accepted = append(accepted, h2)
		}
	}
	if err := h.deps.Store.SetRepositoryPromptset(r.Context(), store.SetRepositoryPromptsetParams{
		ID:                    repoID,
		ActivePromptsetHash:   active,
		AcceptedPromptsetHashes: accepted,
	}); err != nil {
		httputil.WriteError(w, http.StatusInternalServerError, "failed to update repository promptset")
		return
	}
	httputil.WriteJSON(w, http.StatusOK, map[string]interface{}{
		"active_hash":     active,
		"accepted_hashes": accepted,
	})
}

// globalPromptsetDefault returns the configured global default
// promptset hash (cfg.Providers.PromptsetDefault), falling back to
// the built-in default when unset. Used by GetPromptset to surface
// the inherited value in the UI.
func globalPromptsetDefault(h *RepositorySettings, fallback string) string {
	if h.deps.Config != nil && h.deps.Config.Providers.PromptsetDefault != "" {
		return h.deps.Config.Providers.PromptsetDefault
	}
	return fallback
}

// maxContributorDisplayNameLen is the longest display name the
// SetContributor handler accepts. The column is TEXT (no DB-level
// cap); this is the application-level guard against a 1MB payload.
const maxContributorDisplayNameLen = 120

// GetContributor handles GET /repositories/{repoID}/settings/contributor.
//
// Returns the repo's contributor identity: display_name (string or
// null) and anonymous (bool). Defaults to (null, true) — anonymous
// — for repos that haven't configured attribution. The
// contribute_source worker reads this (via the store query, not
// this HTTP endpoint) to decide what to send on PushSource.
func (h *RepositorySettings) GetContributor(w http.ResponseWriter, r *http.Request) {
	repoID, ok := parseRepoID(w, r)
	if !ok {
		return
	}
	row, err := h.deps.Store.GetRepositoryContributor(r.Context(), repoID)
	if err != nil {
		// The row must exist (the repo was created via
		// CreateRepository, which inserted it); treat any error
		// as a 500 rather than fabricating a default.
		httputil.WriteError(w, http.StatusInternalServerError, "failed to read contributor identity")
		return
	}
	var name interface{}
	if row.ContributorDisplayName != nil {
		name = *row.ContributorDisplayName
	}
	httputil.WriteJSON(w, http.StatusOK, map[string]interface{}{
		"display_name": name,
		"anonymous":    row.ContributorAnonymous,
	})
}

// SetContributor handles PUT /repositories/{repoID}/settings/contributor.
//
// Updates the repo's contributor identity. The body is
// { "display_name": string|null, "anonymous": bool }. Both fields
// are optional; omitting one leaves it unchanged.
//
// Validation:
//   - When anonymous=true, display_name MUST be null or empty. The
//     handler clears the stored name (writes NULL) so the column
//     stays clean and the contribute worker sends the canonical
//     "anonymous" marker (display_name="" + anonymous=true).
//   - When anonymous=false, display_name MUST be a non-empty
//     trimmed string of <=120 chars.
//   - At least one field must be present.
func (h *RepositorySettings) SetContributor(w http.ResponseWriter, r *http.Request) {
	repoID, ok := parseRepoID(w, r)
	if !ok {
		return
	}
	// Read the raw body first so we can detect field presence
	// (Go's *string / *bool can't distinguish absent from null).
	rawBody, rawErr := io.ReadAll(r.Body)
	if rawErr != nil {
		httputil.WriteError(w, http.StatusBadRequest, "failed to read request body")
		return
	}
	var body struct {
		DisplayName *string `json:"display_name"`
		Anonymous   *bool   `json:"anonymous"`
	}
	if err := json.Unmarshal(rawBody, &body); err != nil {
		httputil.WriteError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	var rawPresent struct {
		DisplayName json.RawMessage `json:"display_name"`
		Anonymous   json.RawMessage `json:"anonymous"`
	}
	_ = json.Unmarshal(rawBody, &rawPresent)
	namePresent := len(rawPresent.DisplayName) > 0
	anonPresent := len(rawPresent.Anonymous) > 0
	if !namePresent && !anonPresent {
		httputil.WriteError(w, http.StatusBadRequest, "must set display_name or anonymous")
		return
	}

	// Resolve current values so an omitted field keeps its value.
	cur, err := h.deps.Store.GetRepositoryContributor(r.Context(), repoID)
	if err != nil {
		httputil.WriteError(w, http.StatusInternalServerError, "failed to read current contributor identity")
		return
	}
	newAnonymous := cur.ContributorAnonymous
	if anonPresent {
		newAnonymous = *body.Anonymous
	}
	// Resolve the new display_name. When namePresent, the client
	// either sent a string or null. When absent, keep the current
	// stored value (a *string we read back from the row).
	var newNamePtr *string
	if namePresent {
		if body.DisplayName != nil {
			trimmed := strings.TrimSpace(*body.DisplayName)
			newNamePtr = &trimmed
		} else {
			newNamePtr = nil // explicit null
		}
	} else {
		newNamePtr = cur.ContributorDisplayName
	}

	// Combination validation.
	if newAnonymous {
		// anonymous=true ⇒ display_name must be empty/null. Clear
		// it so the column stays clean.
		newNamePtr = nil
	} else {
		// anonymous=false ⇒ display_name must be non-empty and
		// within the length cap.
		if newNamePtr == nil || *newNamePtr == "" {
			httputil.WriteError(w, http.StatusBadRequest, "display_name is required when anonymous is false")
			return
		}
		if len(*newNamePtr) > maxContributorDisplayNameLen {
			httputil.WriteError(w, http.StatusBadRequest, fmt.Sprintf("display_name must be at most %d characters", maxContributorDisplayNameLen))
			return
		}
	}

	if err := h.deps.Store.SetRepositoryContributor(r.Context(), store.SetRepositoryContributorParams{
		ID:                     repoID,
		ContributorDisplayName: newNamePtr,
		ContributorAnonymous:   newAnonymous,
	}); err != nil {
		httputil.WriteError(w, http.StatusInternalServerError, "failed to update contributor identity")
		return
	}

	var respName interface{}
	if newNamePtr != nil {
		respName = *newNamePtr
	}
	httputil.WriteJSON(w, http.StatusOK, map[string]interface{}{
		"display_name": respName,
		"anonymous":    newAnonymous,
	})
}