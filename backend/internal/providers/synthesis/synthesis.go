// Package synthesis provides the concept-synthesis LLM provider.
//
// A SynthesisProvider folds ALL of a canonical-name group's summary
// slices into ONE authoritative "definition" for that concept. It is
// the rewrite's analog of the production system's crystallization
// agent, but fed by incremental summaries (not child nodes) and scoped
// per canonical-name group (one synthesis row per
// (repository_id, lower(canonical_name))).
//
// The provider also exposes a separate image-picker call: given a
// concept name and a candidate list of image facts (id, text, alt),
// it returns up to N image fact_ids whose subject best illustrates the
// concept. The worker runs the picker first, then passes the chosen
// images to the synthesis call so the model can embed them via
// ![alt](<fact:fact_id>) markdown image syntax.
//
// The interface mirrors the summarization provider pattern
// (transport-agnostic strategy in internal/providers/synthesis/ with
// a concrete AI-backed implementation) so a future non-LLM synthesizer
// can slot in without the task worker changing shape.
package synthesis

import (
	"context"

	"github.com/openktree/open-knowledge-tree/backend/internal/providers/ai"
	"github.com/openktree/open-knowledge-tree/backend/internal/store"
)

// SliceInput is one summary slice presented to the synthesizer. ID is
// the slice's concept_summary id (UUID string); ConceptID is the
// per-context concept_id the slice belongs to; SequenceNum is the
// 1-based slice index within that concept_id; FactCount is the number
// of facts the slice folds; Content is the slice's markdown body (the
// summarizer's output, with [text](<fact:fact_id>) citations).
type SliceInput struct {
	ID          string
	ConceptID   string
	SequenceNum int32
	FactCount   int32
	Content     string
}

// ImageInput is one candidate image fact for the synthesis. FactID is
// the canonical UUID string so the LLM can emit ![alt](<fact:fact_id>)
// links; Text is the image description the extractor produced; Alt is
// a short alt-text label (currently derived from Text; a future
// extractor may supply a dedicated alt field).
type ImageInput struct {
	FactID string
	Text   string
	Alt    string
}

// RelatedContext is one per-context breakdown entry for a related
// concept: how many distinct facts the current concept group shares
// with this related concept within this specific context. Loaded by
// the worker via ListConceptRelationDetailsByConceptName.
type RelatedContext struct {
	Context         string
	SharedFactCount int32
}

// RelatedConceptInput is one related concept presented to the
// synthesizer as part of the Graph-Aware Reasoning context. CanonicalName
// is the related concept's canonical name; ConceptID is a
// representative concept_id (MIN(c.id::text)) the UI could link to;
// SharedFactCount is the aggregate count of distinct facts shared
// across ALL of the related concept's contexts; Contexts is the
// per-context breakdown (capped by the worker to keep the prompt
// bounded); Synthesis is the related concept's existing
// concept_syntheses.content attached VERBATIM for the top N2 relations
// only — empty when the related concept has no synthesis row yet OR
// when this relation's rank exceeds N2 (the worker decides; the
// provider treats empty as "no synthesis available").
type RelatedConceptInput struct {
	CanonicalName   string
	ConceptID       string
	SharedFactCount int32
	Contexts        []RelatedContext
	Synthesis       string
}

// SynthesisRequest bundles the inputs to one Synthesize call.
// CanonicalName and Context identify the concept group the synthesis
// covers (Context is the representative context — the first
// concept_id's context — and is informational; the synthesis is
// scoped by canonical name, not by context). Slices is the ordered
// list of summary slices to fold. CandidateImages is the list of
// image facts the model may embed (already narrowed by the picker
// when the candidate pool exceeded MaxImages). MaxImages caps how
// many the model may embed. RelatedConcepts is the optional
// graph-structure block: the top N1 related concept names with
// per-context shared_fact_counts, of which the top N2 also carry
// their existing synthesis text verbatim. The worker populates it
// from the concept_relations matview + the per-context detail query
// + GetSynthesisByGroup; nil/empty means no relation data was
// available (the prompt tells the model so). Model overrides the
// provider's default model id when non-empty. Attribution is the
// ai.Attribution the provider threads into the ChatRequest so the
// resulting ai_usage row is attributed to the repository and River
// job that triggered the synthesis. MaxTokens, when > 0, is passed
// through to the ChatRequest as the per-synthesis output token cap.
type SynthesisRequest struct {
	CanonicalName    string
	Context          string
	Slices           []SliceInput
	CandidateImages  []ImageInput
	MaxImages        int
	RelatedConcepts  []RelatedConceptInput
	Model            string
	MaxTokens        int
	// ThinkingLevel controls the model's reasoning effort ("low", "medium",
	// "high") when the provider supports it. nil means "no preference; let
	// the model decide". Set to "low" for synthesis since it's primarily a
	// prose-composition task where extended reasoning chains waste tokens.
	ThinkingLevel    *string
	TaskID           string
	Attribution      ai.Attribution
}

// ImagePickRequest bundles the inputs to one PickImages call.
// CanonicalName and Context identify the concept group; Candidates is
// the full candidate image pool (the worker loads up to
// MaxImageCandidates rows, then calls PickImages when len >
// MaxImages). MaxImages caps how many ids the picker may return.
type ImagePickRequest struct {
	CanonicalName string
	Context       string
	Candidates    []ImageInput
	MaxImages     int
	Model         string
	MaxTokens     int
	TaskID        string
	Attribution   ai.Attribution
}

// SynthesisProvider folds a concept's summary slices into one
// authoritative definition, and picks the most relevant image facts
// for the synthesis to embed. Synthesize returns the markdown body
// (no JSON envelope, no preamble); PickImages returns the chosen
// fact_id strings (subset of the candidates). Describe returns the
// operator-facing metadata for the /providers catalog.
type SynthesisProvider interface {
	Synthesize(ctx context.Context, db store.DBTX, req SynthesisRequest) (string, error)
	PickImages(ctx context.Context, db store.DBTX, req ImagePickRequest) ([]string, error)
	Describe() ProviderDescription
}

// ProviderDescription mirrors summarization.ProviderDescription so a
// single UI card component can render any provider tree.
type ProviderDescription struct {
	Name        string
	Description string
	Requires    string
	Configured  bool
	Supports    []string
	Notes       string
	Config      map[string]string
}