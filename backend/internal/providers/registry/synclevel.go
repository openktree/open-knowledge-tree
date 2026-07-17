package registry

import "strings"

// SyncLevel controls how much of a source's decomposition the backend
// contributes to the registry (push) and imports from the registry
// (pull). Levels are cumulative: each level includes the data of the
// previous one.
//
//   - SyncLevelFacts: sources + facts + fact embeddings. No concepts,
//     no fact_concept links, no concept embeddings. Pulling at this
//     level leaves fact_concepts empty so the extract_concepts worker
//     regenerates concepts from the stable facts.
//   - SyncLevelConcepts: everything in Facts plus concepts, concept
//     aliases, fact_concept links, and concept embeddings. This is
//     the default and the legacy full-sync behavior.
type SyncLevel string

const (
	SyncLevelFacts    SyncLevel = "facts"
	SyncLevelConcepts SyncLevel = "concepts"
)

// ValidSyncLevels is the set of accepted level strings. Used by the
// settings handler to validate PUT .../settings/sync-levels input
// before persisting via SetRepositorySyncLevels.
var ValidSyncLevels = map[SyncLevel]bool{
	SyncLevelFacts:    true,
	SyncLevelConcepts: true,
}

// ValidSyncLevel reports whether s is one of the accepted level
// strings (case-insensitive). The caller should normalize with
// ParseSyncLevel before persisting.
func ValidSyncLevel(s string) bool {
	_, ok := ValidSyncLevels[ParseSyncLevel(s)]
	return ok
}

// ParseSyncLevel normalizes a level string to its canonical form
// (lowercased). Unknown strings map to the empty SyncLevel (which
// ValidSyncLevels rejects), so callers can chain:
//
//	level := registry.ParseSyncLevel(raw)
//	if !registry.ValidSyncLevels[level] { /* 400 */ }
func ParseSyncLevel(s string) SyncLevel {
	return SyncLevel(strings.ToLower(strings.TrimSpace(s)))
}

// SyncLevelFilter is the single source of truth for what a given
// sync level includes. Both the push worker (contribute_source) and
// the pull paths (pull_all_from_registry, retrieve_source,
// remote_pull) consult it so the "facts-only" definition lives in one
// place and the four call sites stay in lockstep.
//
// Construct one per job/request from the repo's stored level column
// (GetRepositorySyncLevels) and pass it to the relevant gate:
//
//   - Push: use IncludeConcepts() to skip loading concepts/links/
//     concept-embeddings entirely (avoids wasted DB + Qdrant reads).
//   - Pull: call FilterForPull on the pulled DecompositionPackage so
//     the concept/link/concept-embedding import loops iterate zero
//     items when the level is Facts.
type SyncLevelFilter struct {
	level SyncLevel
}

// NewSyncLevelFilter constructs a filter for the given level. The
// level should come from the repo's registry_push_level or
// registry_pull_level column; an empty level defaults to Concepts
// (the migration default) so a misread never silently strips data.
func NewSyncLevelFilter(level SyncLevel) *SyncLevelFilter {
	if level == "" {
		level = SyncLevelConcepts
	}
	return &SyncLevelFilter{level: level}
}

// Level returns the configured sync level.
func (f *SyncLevelFilter) Level() SyncLevel { return f.level }

// IncludeConcepts reports whether concepts, fact_concept links, and
// concept embeddings should be pushed/pulled. True for Concepts, false
// for Facts. The push worker uses this to decide whether to load
// concepts/links/concept-embeddings at all (skipping the DB + Qdrant
// reads when false).
func (f *SyncLevelFilter) IncludeConcepts() bool {
	return f.level == SyncLevelConcepts
}

// FilterForPush returns a shallow copy of pkg with the concept-level
// fields nilled when the level is Facts. The original package is not
// mutated. When the level is Concepts the original pointer is
// returned unchanged (no allocation).
//
// Used by the contribute_source worker after it has assembled the
// full package (or, more efficiently, after skipping the concept
// loads via IncludeConcepts) so the pushed JSON omits the concept
// fields entirely.
func (f *SyncLevelFilter) FilterForPush(pkg *DecompositionPackage) *DecompositionPackage {
	if f.IncludeConcepts() {
		return pkg
	}
	out := *pkg
	out.Concepts = nil
	out.Links = nil
	out.ConceptEmbeddings = nil
	out.Embeddings = filterFactEmbeddings(pkg.Embeddings)
	return &out
}

// FilterForPull strips a pulled decomposition the same way
// FilterForPush does. The pull import loops then iterate zero
// concept/link items unchanged, so the existing loop code needs no
// per-level branching — a single FilterForPull call after
// PullDecomposition is the only wiring each pull path needs.
func (f *SyncLevelFilter) FilterForPull(decomp *DecompositionPackage) *DecompositionPackage {
	return f.FilterForPush(decomp)
}

// filterFactEmbeddings returns a copy of the EmbeddingData with
// only the "fact:"-prefixed keys retained (dropping "concept:"
// vectors). Used when stripping concept-level data for a Facts-level
// push/pull. nil input returns nil so the omitempty JSON tag drops
// the field entirely.
func filterFactEmbeddings(ed *EmbeddingData) *EmbeddingData {
	if ed == nil || len(ed.Vectors) == 0 {
		return nil
	}
	out := &EmbeddingData{
		Model:      ed.Model,
		Dimensions: ed.Dimensions,
		Vectors:    make(map[string][]float64),
	}
	for k, v := range ed.Vectors {
		if strings.HasPrefix(k, "fact:") {
			out.Vectors[k] = v
		}
	}
	if len(out.Vectors) == 0 {
		return nil
	}
	return out
}