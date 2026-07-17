package tasks

import (
	"context"
	"fmt"
	"log"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/openktree/open-knowledge-tree/backend/internal/providers/registry"
	"github.com/openktree/open-knowledge-tree/backend/internal/store"
)

// OutboundContextMapper translates local concept contexts to the
// registry's canonical vocabulary on contribute. Built once per
// contribute_source Work call from the repo's mapping table + the
// registry's published vocab.
//
// Rules (locked from the design conversation):
//   - A local context with a mapping row → its registry_context.
//   - A local context with no mapping row whose label exists in the
//     registry vocab → the local label verbatim (push as-is).
//   - A local context with no mapping row and absent from the
//     registry vocab → skip (the concept is not contributed).
//   - When the registry vocab is empty (registry endpoint not live
//     or genuinely has no contexts), validation is off: every
//     unmapped local context is pushed verbatim so a no-registry
//     deployment doesn't silently drop custom-context contributions.
type OutboundContextMapper struct {
	localToRegistry map[string]string // lower(local) → registry
	registrySet     map[string]bool   // lower(registry) → exists
	registryEmpty   bool              // true when ListContexts returned []
}

// NewOutboundContextMapper loads the repo's mapping table + the
// registry's vocab and returns a mapper. Returns a no-op mapper
// (map everything verbatim) when the registry client is nil or the
// vocab fetch fails AND there's no cached value — the contribute
// path degrades to current behavior rather than failing the job.
func NewOutboundContextMapper(ctx context.Context, systemQueries *store.Queries, rc *registry.Client, repoID pgtype.UUID) (*OutboundContextMapper, error) {
	m := &OutboundContextMapper{
		localToRegistry: map[string]string{},
		registrySet:     map[string]bool{},
	}
	rows, err := systemQueries.ListRepositoryContextMappings(ctx, repoID)
	if err != nil {
		return nil, fmt.Errorf("listing context mappings: %w", err)
	}
	for _, r := range rows {
		m.localToRegistry[strings.ToLower(r.LocalContext)] = r.RegistryContext
	}
	if rc != nil && rc.IsConfigured() {
		labels, err := rc.ListContexts(ctx)
		if err != nil {
			// fallbackContexts already returned the cached value
			// if there was one; if that's also empty, treat as
			// "validation off" so we don't drop contributions on
			// a transient outage.
			log.Printf("contribute_source: loading registry vocab (proceeding with validation off): %v", err)
			m.registryEmpty = true
			return m, nil
		}
		m.registryEmpty = len(labels) == 0
		for _, l := range labels {
			m.registrySet[strings.ToLower(l)] = true
		}
	} else {
		// No registry client (unconfigured): validation off.
		m.registryEmpty = true
	}
	return m, nil
}

// mapContext returns the registry-side context label for a local
// one and whether the concept should be contributed. When the second
// return is false, the caller skips the concept (and any link to it).
func (m *OutboundContextMapper) MapContext(localContext string) (string, bool) {
	if localContext == "" {
		return "", false
	}
	lc := strings.ToLower(localContext)
	if regCtx, ok := m.localToRegistry[lc]; ok {
		return regCtx, true
	}
	// Unmapped. If the registry vocab is empty (endpoint not live or
	// genuinely empty), validation is off → push verbatim.
	if m.registryEmpty {
		return localContext, true
	}
	// Unmapped but the label exists in the registry vocab → push verbatim.
	if m.registrySet[lc] {
		return localContext, true
	}
	// Unmapped and absent → skip.
	return "", false
}

// NewOutboundContextMapperForTest builds an outbound mapper from
// explicit fields so tests can exercise the MapContext logic without
// a database or a registry client. Not used by production code.
func NewOutboundContextMapperForTest(localToRegistry map[string]string, registrySet map[string]bool, registryEmpty bool) *OutboundContextMapper {
	return &OutboundContextMapper{
		localToRegistry: localToRegistry,
		registrySet:     registrySet,
		registryEmpty:   registryEmpty,
	}
}

// InboundContextMapper translates registry concept contexts to the
// repo's local vocabulary on pull. Built once per pull_all Work call.
//
// Unmapped registry contexts are handled per the repo's
// unmapped_context_policy (skip | auto_add | catch_all).
type InboundContextMapper struct {
	registryToLocal map[string]string // lower(registry) → local
	policy          string            // skip | auto_add | catch_all
	catchAll        string            // local context for catch_all ("" when not set)
}

// NewInboundContextMapper loads the repo's reverse mapping + the
// unmapped-context policy. Returns a mapper ready for mapContext.
func NewInboundContextMapper(ctx context.Context, systemQueries *store.Queries, repoID pgtype.UUID) (*InboundContextMapper, error) {
	m := &InboundContextMapper{
		registryToLocal: map[string]string{},
		policy:          "skip",
	}
	rows, err := systemQueries.ListRepositoryContextMappings(ctx, repoID)
	if err != nil {
		return nil, fmt.Errorf("listing context mappings: %w", err)
	}
	for _, r := range rows {
		m.registryToLocal[strings.ToLower(r.RegistryContext)] = r.LocalContext
	}
	policy, err := systemQueries.GetUnmappedContextPolicy(ctx, repoID)
	if err != nil {
		return nil, fmt.Errorf("reading unmapped context policy: %w", err)
	}
	m.policy = policy.UnmappedContextPolicy
	if policy.CatchAllContext != nil {
		m.catchAll = *policy.CatchAllContext
	}
	return m, nil
}

// mapContext returns the local context label for a registry one and
// whether the concept should be imported. When the second return is
// false, the caller skips the concept (and any link to it). The
// autoAdd callback is called (with the registry label) when the
// policy is auto_add and the label isn't already a local context;
// the caller seeds a repository_contexts row so the import can land.
func (m *InboundContextMapper) MapContext(registryContext string, autoAdd func(string)) (string, bool) {
	if registryContext == "" {
		return "", false
	}
	rc := strings.ToLower(registryContext)
	if localCtx, ok := m.registryToLocal[rc]; ok {
		return localCtx, true
	}
	switch m.policy {
	case "skip":
		return "", false
	case "auto_add":
		if autoAdd != nil {
			autoAdd(registryContext)
		}
		return registryContext, true
	case "catch_all":
		if m.catchAll == "" {
			return "", false
		}
		return m.catchAll, true
	default:
		return "", false
	}
}

// ApplyOutboundConcepts rewrites the context field of each concept
// via the outbound mapper, dropping concepts whose local context is
// unmapped-and-absent. The conceptIDs slice is filtered in parallel
// (the two slices are index-aligned by loadConcepts). skippedNames
// is populated with the lower(canoname, localctx) of dropped
// concepts so FilterLinksByContext can drop their links too.
func ApplyOutboundConcepts(concepts []registry.ConceptData, conceptIDs []uuid.UUID, mapper *OutboundContextMapper, skippedNames map[string]bool) ([]registry.ConceptData, []uuid.UUID) {
	out := make([]registry.ConceptData, 0, len(concepts))
	outIDs := make([]uuid.UUID, 0, len(conceptIDs))
	for i, c := range concepts {
		mapped, ok := mapper.MapContext(c.Context)
		if !ok {
			skippedNames[strings.ToLower(c.CanonicalName+"\x00"+c.Context)] = true
			continue
		}
		c.Context = mapped
		out = append(out, c)
		if i < len(conceptIDs) {
			outIDs = append(outIDs, conceptIDs[i])
		}
	}
	return out, outIDs
}

// FilterLinksByContext rewrites the concept_context of each
// fact_concept link via the outbound mapper, dropping links whose
// concept was skipped (by name+context) or whose context maps to
// skip. This keeps the registry's link set consistent with the
// pushed concept set — no dangling links.
func FilterLinksByContext(links []registry.FactConceptLink, mapper *OutboundContextMapper, skippedConceptNames map[string]bool) []registry.FactConceptLink {
	out := make([]registry.FactConceptLink, 0, len(links))
	for _, l := range links {
		// Drop links to concepts that were skipped by name+context.
		if skippedConceptNames[strings.ToLower(l.ConceptName+"\x00"+l.ConceptContext)] {
			continue
		}
		mapped, ok := mapper.MapContext(l.ConceptContext)
		if !ok {
			continue
		}
		l.ConceptContext = mapped
		out = append(out, l)
	}
	return out
}