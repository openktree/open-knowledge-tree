// Package summarization provides the concept-summary LLM provider.
//
// A SummarizationProvider receives a concept (canonical name +
// context) and the facts linked to it, and returns a credulous
// markdown summary that reflects every detail and perspective
// present in the facts. The summary cites central and key facts via
// standard markdown links of the form [text](<fact_id>); citation is
// selective (load-bearing facts only), not exhaustive.
//
// The interface mirrors the decomposition provider pattern
// (transport-agnostic strategy in internal/providers/summarization/
// with a concrete AI-backed implementation) so a future non-LLM
// summarizer (e.g. an extractive summarizer) can slot in without the
// task worker changing shape.
package summarization

import (
	"context"

	"github.com/openktree/open-knowledge-tree/backend/internal/providers/ai"
	"github.com/openktree/open-knowledge-tree/backend/internal/store"
)

// FactInput is one fact presented to the summarizer. ID is the
// canonical UUID string so the LLM can emit [text](<fact_id>) links;
// Text is the fact body; Attribution is an optional "(BBC; J. Smith)"
// parenthetical the worker builds from the fact's sources so the
// summary can name the sources behind the claims.
type FactInput struct {
	ID          string
	Text        string
	Attribution string
}

// SummarizationRequest bundles the inputs to one Summarize call.
// ConceptCanonicalName and Context identify the (concept, context)
// pair the summary covers; Facts is the ordered list of facts to
// summarize (the worker passes the union of an open slice's covered
// set and any newly arrived facts). Model overrides the provider's
// default model id when non-empty. Attribution is the ai.Attribution
// the provider threads into the ChatRequest so the resulting ai_usage
// row is attributed to the repository and River job that triggered
// the summary. MaxTokens, when > 0, is passed through to the
// ChatRequest as the per-slice output token cap (hard backstop on the
// prompt's length-budget instruction); 0 means "leave the provider
// default".
type SummarizationRequest struct {
	ConceptCanonicalName string
	Context              string
	Facts                []FactInput
	Model                string
	MaxTokens            int
	TaskID               string
	Attribution          ai.Attribution
}

// SummarizationProvider summarizes the facts linked to a (concept,
// context) pair. Summarize returns the markdown body (no JSON
// envelope, no preamble); Describe returns the operator-facing
// metadata for the /providers catalog.
type SummarizationProvider interface {
	Summarize(ctx context.Context, db store.DBTX, req SummarizationRequest) (string, error)
	Describe() ProviderDescription
}

// ProviderDescription mirrors decomposition.ProviderDescription so
// a single UI card component can render any provider tree.
type ProviderDescription struct {
	Name        string
	Description string
	Requires    string
	Configured  bool
	Supports    []string
	Notes       string
	Config      map[string]string
}