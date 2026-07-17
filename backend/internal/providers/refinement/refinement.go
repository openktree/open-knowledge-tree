// Package refinement provides the concept-refinement LLM provider.
//
// A RefineProvider receives a candidate concept (raw extracted text +
// context) and its current aliases, and returns the full formal
// canonical name, aliases to add, and aliases to prune. The
// refine_concepts task worker calls it once per unresolved candidate
// after the pre-LLM routing (exact canonical / alias / seed-alias
// match) misses; the post-LLM routing uses the returned canonical name
// and aliases to check for collisions with existing concepts before
// creating a new one or merging.
//
// The interface mirrors the summarization / decomposition provider
// pattern (transport-agnostic strategy in internal/providers/ with a
// concrete AI-backed implementation) so a future non-LLM refiner can
// slot in without the task worker changing shape.
package refinement

import (
	"context"

	"github.com/openktree/open-knowledge-tree/backend/internal/providers/ai"
	"github.com/openktree/open-knowledge-tree/backend/internal/store"
)

// RefinementRequest bundles the inputs to one Refine call. Concept is
// the raw extracted concept text (e.g. "trump"); Context is the L3
// ontology class label (e.g. "Politician"); ExistingAliases are the
// concept's current aliases (empty for a new candidate — the prompt
// still asks the model to propose new ones); SeedAliases are the
// short forms the concept extractor emitted inline (the worker adds
// them regardless, but listing them helps the model avoid
// re-proposing them). Model overrides the provider's default model
// id when non-empty. Attribution is the ai.Attribution the provider
// threads into the ChatRequest so the resulting ai_usage row is
// attributed to the repository and River job that triggered the
// refinement. MaxTokens, when > 0, is passed through to the
// ChatRequest as the per-call output token cap.
type RefinementRequest struct {
	Concept         string
	Context         string
	ExistingAliases []string
	SeedAliases     []string
	Model           string
	MaxTokens       int
	TaskID          string
	Attribution     ai.Attribution
}

// RefinementResult is the output of the refinement call. CanonicalName
// is the full formal canonical name (never an alias or acronym); the
// worker uses it to check for collisions with existing concepts before
// creating a new one or merging. AliasesToAdd are known alternate
// surface forms to insert as concept_aliases rows (ON CONFLICT DO
// NOTHING). AliasesToPrune are existing aliases to remove (the worker
// deletes matching concept_aliases rows); empty when there is nothing
// to prune (e.g. a new candidate with no existing aliases).
type RefinementResult struct {
	CanonicalName  string   `json:"canonical_name"`
	AliasesToAdd   []string `json:"aliases_to_add"`
	AliasesToPrune []string `json:"aliases_to_prune"`
}

// RefineProvider refines a candidate concept into a canonical name +
// aliases to add + aliases to prune. Refine returns the structured
// result; Describe returns the operator-facing metadata for the
// /providers catalog.
type RefineProvider interface {
	Refine(ctx context.Context, db store.DBTX, req RefinementRequest) (RefinementResult, error)
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