// Package promptset models a "promptset": the complete set of phase
// prompt strings an OKT deployment uses to decompose, refine,
// summarize, synthesize, and classify facts/concepts. A promptset has
// TWO identity hashes:
//
//   - Hash: the SHA-256 over the canonical JSON of all 8 phase fields.
//     This is the CATALOG identity: two promptsets with the same 8
//     prompts share a row in okt_system.promptsets (UpsertPromptset
//     keys on Hash) and a row in the UI list. Renaming, owner, and
//     source are deliberately excluded.
//
//   - RegistryHash: the SHA-256 over only the 4 REGISTRY-SHARED
//     phases (fact_extraction, image_fact_extraction,
//     concept_extraction, refinement). The other 4 phases
//     (synthesis, image_picker, summarization, posture) run
//     locally only — their output is never pushed to the registry
//     (contribute_source pushes facts + concepts + links + embeddings,
//     never summaries or synthesized reports). Two promptsets that
//     differ only in the local phases share a RegistryHash and are
//     "registry-compatible": a pulling repo accepts decompositions
//     tagged with any promptset in the same compatibility class, so
//     tweaking the summarizer no longer fractures the registry graph.
//
// The split is what lets the registry keep decompositions from
// different *philosophies* separate (a different fact-extraction
// prompt is a new philosophy) without penalizing operators who only
// customized the non-shared phases. The catalog Hash keeps the
// upsert/UI identity stable so a user can still keep "same
// philosophy, different summarizer" as distinct user-visible
// promptsets.
//
// A Promptset is an object with one field per phase. The phases are
// fixed by the codebase (fact extraction, image fact extraction,
// concept extraction, refinement, synthesis, image picker,
// summarization, posture). Adding a phase is a schema change: the
// struct grows a field, the hash inputs grow, and old hashes no
// longer match — which is the correct behavior, since a promptset
// with a new phase is a new philosophy.
//
// The package also defines a Provider strategy (built-in + DB) so the
// server can resolve a hash to a Promptset the same way it resolves a
// search or resolution provider. The built-in provider returns the
// Default promptset (compiled from the existing prompt constants); a
// DB provider returns user-defined promptsets stored in
// okt_system.promptsets. A Resolver chains them.
package promptset

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
)

// PhaseLabel is the human-readable name of a prompt phase. The set of
// labels is fixed and matches the Promptset struct fields. Used by
// the UI and by validation errors so a missing phase is named
// clearly.
type PhaseLabel string

const (
	PhaseFactExtraction       PhaseLabel = "fact_extraction"
	PhaseImageFactExtraction  PhaseLabel = "image_fact_extraction"
	PhaseConceptExtraction    PhaseLabel = "concept_extraction"
	PhaseRefinement           PhaseLabel = "refinement"
	PhaseSynthesis            PhaseLabel = "synthesis"
	PhaseImagePicker          PhaseLabel = "image_picker"
	PhaseSummarization        PhaseLabel = "summarization"
	PhasePosture              PhaseLabel = "posture"
)

// NumPhases is the number of phase fields on a Promptset. Kept as a
// constant so validation can assert "all phases present" without
// hardcoding 8 everywhere.
const NumPhases = 8

// RegistrySharedPhases is the ordered list of phases that feed the
// RegistryHash — the compatibility identifier the registry wire
// format carries and the pull filter compares against. Only the
// phases whose output is actually pushed to the registry
// (fact_extraction, image_fact_extraction, concept_extraction,
// refinement) belong here; synthesis / image_picker / summarization
// / posture run locally only and changes to them must NOT fracture
// the registry graph.
var RegistrySharedPhases = []PhaseLabel{
	PhaseFactExtraction,
	PhaseImageFactExtraction,
	PhaseConceptExtraction,
	PhaseRefinement,
}

// RegistryLocalPhases is the ordered list of phases that are NOT
// pushed to the registry and therefore NOT part of the RegistryHash.
// Changes to any of these produce a new catalog Hash but keep the
// same RegistryHash — i.e. a "compatible" promptset.
var RegistryLocalPhases = []PhaseLabel{
	PhaseSynthesis,
	PhaseImagePicker,
	PhaseSummarization,
	PhasePosture,
}

// Promptset is the complete set of phase prompts an OKT instance (or
// a repository) uses. Two identity hashes are carried:
//
//   - Hash is the CATALOG identity — sha256 over the JSON marshalling
//     of all 8 phase fields in struct declaration order. Two
//     Promptsets with the same 8 strings have the same Hash, share a
//     row in okt_system.promptsets (UpsertPromptset keys on it), and
//     appear as one entry in the catalog. This is what the UI shows
//     as "the hash".
//
//   - RegistryHash is the COMPATIBILITY identity — sha256 over only
//     the 4 registry-shared phases (see RegistrySharedPhases). The
//     registry wire format carries this hash on each decomposition
//     and the pull filter compares against it, so two promptsets
//     that differ only in the local phases (synthesis, image_picker,
//     summarization, posture) share a RegistryHash and can exchange
//     decompositions.
//
// The Name field is a human-readable label carried for the UI; it is
// NOT part of either hash input (renaming a promptset does not change
// its identity). OwnerID is the user who created a custom promptset;
// empty for the built-in. Source is "builtin" or "custom" so the UI
// can badge them.
type Promptset struct {
	Hash                string `json:"hash"`
	RegistryHash        string `json:"registry_hash"`
	Name                string `json:"name"`
	OwnerID             string `json:"owner_id,omitempty"`
	Source              string `json:"source"` // "builtin" | "custom"
	FactExtraction      string `json:"fact_extraction"`
	ImageFactExtraction string `json:"image_fact_extraction"`
	ConceptExtraction   string `json:"concept_extraction"`
	Refinement          string `json:"refinement"`
	Synthesis           string `json:"synthesis"`
	ImagePicker         string `json:"image_picker"`
	Summarization       string `json:"summarization"`
	Posture             string `json:"posture"`
}

// Phases returns the 8 phase strings in a fixed order, paired with
// their labels. Used by HashPromptset and by validation. The order
// matches the struct field declaration order so JSON marshalling and
// hashing are deterministic without a custom MarshalJSON.
func (p Promptset) Phases() []struct {
	Label PhaseLabel
	Value string
} {
	return []struct {
		Label PhaseLabel
		Value string
	}{
		{PhaseFactExtraction, p.FactExtraction},
		{PhaseImageFactExtraction, p.ImageFactExtraction},
		{PhaseConceptExtraction, p.ConceptExtraction},
		{PhaseRefinement, p.Refinement},
		{PhaseSynthesis, p.Synthesis},
		{PhaseImagePicker, p.ImagePicker},
		{PhaseSummarization, p.Summarization},
		{PhasePosture, p.Posture},
	}
}

// hashInput is the exact set of fields hashed to produce a
// Promptset's CATALOG identity (the .Hash field). It MUST stay in
// sync with Promptset's phase fields — adding a phase means adding a
// field here and regenerating every existing hash (which is the
// point: a new phase is a new philosophy). Name, OwnerID, Source,
// Hash, and RegistryHash are deliberately excluded so identity is
// purely "what prompts run".
type hashInput struct {
	FactExtraction      string `json:"fact_extraction"`
	ImageFactExtraction string `json:"image_fact_extraction"`
	ConceptExtraction   string `json:"concept_extraction"`
	Refinement          string `json:"refinement"`
	Synthesis           string `json:"synthesis"`
	ImagePicker         string `json:"image_picker"`
	Summarization       string `json:"summarization"`
	Posture             string `json:"posture"`
}

// registryHashInput is the subset of fields hashed to produce a
// Promptset's REGISTRY compatibility identity (the .RegistryHash
// field). Only the phases whose output is pushed to the registry
// belong here (see RegistrySharedPhases); changes to synthesis /
// image_picker / summarization / posture produce a new catalog Hash
// but keep the same RegistryHash — i.e. a "compatible" promptset
// whose decompositions can be pulled by repos that only know the
// original. The order matches RegistrySharedPhases for determinism.
type registryHashInput struct {
	FactExtraction      string `json:"fact_extraction"`
	ImageFactExtraction string `json:"image_fact_extraction"`
	ConceptExtraction   string `json:"concept_extraction"`
	Refinement          string `json:"refinement"`
}

// HashPromptset computes the canonical SHA-256 hash of a Promptset's
// 8 phase fields (the CATALOG identity). The input is the JSON
// marshalling of hashInput (a struct, so field order is the
// declaration order and deterministic), so two promptsets with the
// same 8 phase strings always hash to the same value across instances
// and across processes. The returned string is the lowercase hex
// digest, 64 chars.
//
// Use this to compute the hash for a newly-created custom promptset
// (the handler ignores any client-supplied hash) and to verify a
// stored promptset's hash matches its content (a tamper check).
func HashPromptset(p Promptset) string {
	in := hashInput{
		FactExtraction:      p.FactExtraction,
		ImageFactExtraction: p.ImageFactExtraction,
		ConceptExtraction:   p.ConceptExtraction,
		Refinement:          p.Refinement,
		Synthesis:           p.Synthesis,
		ImagePicker:         p.ImagePicker,
		Summarization:       p.Summarization,
		Posture:             p.Posture,
	}
	b, _ := json.Marshal(in)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// RegistryHashPromptset computes the canonical SHA-256 hash over
// only the 4 registry-shared phase fields (fact_extraction,
// image_fact_extraction, concept_extraction, refinement). This is
// the COMPATIBILITY identity the registry wire format carries on
// each decomposition and the pull filter compares against — two
// promptsets that differ only in the local phases (synthesis,
// image_picker, summarization, posture) share a RegistryHash and can
// exchange decompositions. The returned string is the lowercase hex
// digest, 64 chars.
//
// Because the hash is purely a function of the 4 shared strings,
// any custom promptset whose shared fields equal the built-in
// Default's collapses to DefaultRegistryHash automatically — no
// special-casing needed. This is what makes "tweaked only the
// summarizer" promptsets stay registry-compatible with the default.
func RegistryHashPromptset(p Promptset) string {
	in := registryHashInput{
		FactExtraction:      p.FactExtraction,
		ImageFactExtraction: p.ImageFactExtraction,
		ConceptExtraction:   p.ConceptExtraction,
		Refinement:          p.Refinement,
	}
	b, _ := json.Marshal(in)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// WithHash returns a copy of p with the Hash field set to the
// canonical catalog hash of all 8 phase fields AND the RegistryHash
// field set to the canonical compatibility hash of the 4 shared
// fields. Convenience for the create handler: build the Promptset
// from the request body, call WithHash, persist. The returned Hash
// is the authoritative catalog identity; the returned RegistryHash
// is the authoritative compatibility identity.
func (p Promptset) WithHash() Promptset {
	p.Hash = HashPromptset(p)
	p.RegistryHash = RegistryHashPromptset(p)
	return p
}

// WithRegistryHash returns a copy of p with only the RegistryHash
// field populated. Rarely needed directly — WithHash populates both
// — but useful when a caller already has the catalog hash and only
// needs the compatibility hash recomputed (e.g. verifying a stored
// row).
func (p Promptset) WithRegistryHash() Promptset {
	p.RegistryHash = RegistryHashPromptset(p)
	return p
}

// IsComplete reports whether every phase field is non-empty. Used by
// the create/update handler to reject promptsets missing a phase
// (which would silently fall back to the built-in at runtime and
// produce a hash that doesn't match the actual prompts used).
func (p Promptset) IsComplete() bool {
	for _, ph := range p.Phases() {
		if ph.Value == "" {
			return false
		}
	}
	return true
}

// MissingPhases returns the labels of any phase fields that are
// empty, so the create handler can name them in a 400 error.
func (p Promptset) MissingPhases() []PhaseLabel {
	var missing []PhaseLabel
	for _, ph := range p.Phases() {
		if ph.Value == "" {
			missing = append(missing, ph.Label)
		}
	}
	return missing
}