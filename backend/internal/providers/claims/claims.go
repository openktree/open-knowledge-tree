// Package claims provides the report-claim extraction pass.
//
// Before the annotate_report worker retrieves candidate facts for a
// report sentence, a claim Extractor reads the sentence and emits
// every verifiable assertion it makes — numeric values, causal
// claims, comparisons, quotations, definitions. Each claim carries
// a Term (the verbatim surface form the LLM picked out of the
// sentence) plus a Type so the worker can route numeric claims to
// the tsvector lexical retrieval path and prose claims to the
// embedding path.
//
// The claims are ephemeral: they are computed inside the
// annotate_report worker and threaded into the posture-classifier
// prompt so the LLM judges each candidate fact against the specific
// claim it could verify, not just the sentence's broad topic. They
// are never persisted — re-annotation recomputes them from scratch.
//
// The AI-backed implementation wraps an ai.AIProvider the same way
// the posture classifier and the summarization providers do; a
// nil/unconfigured extractor makes the worker skip the claim-driven
// retrieval path (the legacy embedding-only retrieval still runs).
package claims

import (
	"context"

	"github.com/openktree/open-knowledge-tree/backend/internal/providers/ai"
	"github.com/openktree/open-knowledge-tree/backend/internal/store"
)

// ClaimType is the category of a verifiable claim. The value drives
// the retrieval path the worker uses to find candidate facts:
//   - "numeric"   → tsvector search on the Term (the value + unit);
//   - "causal", "comparison", "quotation", "definition", "other"
//     → embedding similarity search on the Term.
type ClaimType string

const (
	ClaimNumeric     ClaimType = "numeric"
	ClaimCausal      ClaimType = "causal"
	ClaimComparison  ClaimType = "comparison"
	ClaimQuotation   ClaimType = "quotation"
	ClaimDefinition  ClaimType = "definition"
	ClaimOther       ClaimType = "other"
)

// Claim is one verifiable assertion extracted from a sentence. Term
// is the verbatim surface form the extractor picked out of the
// sentence (the value, entity, or short phrase that should be
// verifiable against a fact). Context is the surrounding clause so
// the classifier can disambiguate when the Term alone is ambiguous.
type Claim struct {
	Type    ClaimType `json:"type"`
	Term    string    `json:"term"`
	Context string    `json:"context"`
}

// SentenceClaims is the set of claims extracted from one sentence.
// SentenceIndex matches the Chunk.Index the chunker produced so the
// worker can join claims back to the candidate sentence array.
type SentenceClaims struct {
	SentenceIndex int     `json:"sentence_index"`
	Claims        []Claim `json:"claims"`
}

// ExtractRequest bundles the inputs to one Extract call. Sentences
// is the batch (typically 16 sentences per call); Model overrides
// the provider's default model id when non-empty (the worker sets
// it from the per-repo ModelResolver override). MaxTokens, when > 0,
// is passed through to the ChatRequest as the output token cap.
// TaskID + Attribution are threaded into the ChatRequest so the
// resulting ai_usage row is attributed to the annotate_report job
// and the repository it serves.
type ExtractRequest struct {
	Sentences   []SentenceInput
	Model       string
	MaxTokens   int
	TaskID      string
	Attribution ai.Attribution
}

// SentenceInput is one sentence the extractor must read. Index is
// the chunker's sentence_index; Text is the sentence body.
type SentenceInput struct {
	Index int    `json:"sentence_index"`
	Text  string `json:"sentence"`
}

// Extractor reads a batch of sentences and emits the verifiable
// claims each one makes. Extract returns one SentenceClaims per
// input sentence that has at least one claim; sentences with no
// claims are omitted (the worker treats absence as "no
// claim-driven retrieval for this sentence"). Describe returns the
// operator-facing metadata for the /providers catalog. Configured
// reports whether the extractor is ready to run.
type Extractor interface {
	Extract(ctx context.Context, db store.DBTX, req ExtractRequest) ([]SentenceClaims, error)
	Describe() ProviderDescription
	Configured() bool
}

// ProviderDescription mirrors the shape used by the other provider
// packages so a single UI card component can render any provider
// tree.
type ProviderDescription struct {
	Name        string
	Description string
	Requires    string
	Configured  bool
	Supports    []string
	Notes       string
	Config      map[string]string
}