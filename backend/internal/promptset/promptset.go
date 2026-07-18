// Package promptset models a "promptset": the complete set of phase
// prompt strings an OKT deployment uses to decompose, refine,
// summarize, synthesize, and classify facts/concepts. A promptset is
// identified by the SHA-256 hash of the canonical JSON of its phase
// fields, so two promptsets with identical prompts share an identity
// even across instances. This is what lets the registry keep
// decompositions from different promptsets separate: the hash is the
// "philosophy" identifier, and a repository declares which hashes it
// accepts so foreign decompositions with a different philosophy never
// contaminate its graph.
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

// Promptset is the complete set of phase prompts an OKT instance (or
// a repository) uses. The Hash field is the canonical identifier —
// sha256 over the JSON marshalling of the 8 phase fields in struct
// declaration order. Two Promptsets with the same 8 strings have the
// same hash and are the same philosophy.
//
// The Name field is a human-readable label carried for the UI; it is
// NOT part of the hash input (renaming a promptset does not change
// its identity). OwnerID is the user who created a custom promptset;
// empty for the built-in. Source is "builtin" or "custom" so the UI
// can badge them.
type Promptset struct {
	Hash                string `json:"hash"`
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
// Promptset's identity. It MUST stay in sync with Promptset's phase
// fields — adding a phase means adding a field here and regenerating
// every existing hash (which is the point: a new phase is a new
// philosophy). Name, OwnerID, Source, and Hash itself are deliberately
// excluded so identity is purely "what prompts run".
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

// HashPromptset computes the canonical SHA-256 hash of a Promptset's
// phase fields. The input is the JSON marshalling of hashInput (a
// struct, so field order is the declaration order and deterministic),
// so two promptsets with the same 8 phase strings always hash to the
// same value across instances and across processes. The returned
// string is the lowercase hex digest, 64 chars.
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

// WithHash returns a copy of p with the Hash field set to the canonical
// hash of its phase fields. Convenience for the create handler: build
// the Promptset from the request body, call WithHash, persist. The
// returned Hash is the authoritative identity.
func (p Promptset) WithHash() Promptset {
	p.Hash = HashPromptset(p)
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