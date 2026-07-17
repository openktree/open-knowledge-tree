// Package posture provides the autocite posture classifier.
//
// After the annotate_report worker retrieves candidate facts from
// Qdrant for each report sentence, a Classifier assigns every
// (sentence, fact) pair one of four postures — related, supports,
// contradicts, irrelevant — so the worker can drop irrelevant
// matches before persisting report_annotations. The AI-backed
// implementation wraps an ai.AIProvider the same way the
// summarization/synthesis providers do; a nil/unconfigured
// classifier makes the worker fall back to the legacy keep-all
// behavior (posture = NULL).
package posture

import (
	"context"

	"github.com/google/uuid"
	"github.com/openktree/open-knowledge-tree/backend/internal/providers/ai"
	"github.com/openktree/open-knowledge-tree/backend/internal/store"
)

// Posture is the LLM-assigned relationship label for a
// (sentence, fact) pair. The four values are persisted as the
// report_annotations.posture TEXT column (with irrelevant never
// persisted — the worker drops those pairs before insert).
type Posture string

const (
	// Related: the fact is topically relevant to the sentence but
	// neither supports nor contradicts its claim.
	Related Posture = "related"
	// Supports: the fact provides evidence for the sentence's claim.
	Supports Posture = "supports"
	// Contradicts: the fact provides evidence against the claim.
	Contradicts Posture = "contradicts"
	// Irrelevant: the fact is not meaningfully related to the
	// sentence. The worker drops these pairs instead of persisting
	// them; the value is never written to the database.
	Irrelevant Posture = "irrelevant"
)

// FactCandidate is one retrieved fact the classifier must judge
// against a sentence. ID is the fact UUID (string form); Text is
// the fact body. Score is the Qdrant cosine similarity (0..1) the
// worker carried through; the classifier does not use it but the
// worker threads it so the result row can be matched back to the
// original hit.
type FactCandidate struct {
	ID   uuid.UUID
	Text string
}

// SentenceFacts is a single sentence plus the candidate facts
// Qdrant returned for it. One or more of these forms a batch.
type SentenceFacts struct {
	SentenceIndex int
	SentenceText  string
	Facts         []FactCandidate
}

// Classification is the per-pair label the classifier returns.
// SentenceIndex + FactID mirror the SentenceFacts/FactCandidate
// fields so the caller can join results back to the original hits
// without keeping a parallel index.
type Classification struct {
	SentenceIndex int
	FactID        uuid.UUID
	Posture       Posture
}

// ClassifyRequest bundles the inputs to one Classify call. Sentences
// is the batch (typically 8 sentences per call); Model overrides the
// provider's default model id when non-empty (the worker sets it
// from the per-repo ModelResolver override). MaxTokens, when > 0,
// is passed through to the ChatRequest as the output token cap.
// TaskID + Attribution are threaded into the ChatRequest so the
// resulting ai_usage row is attributed to the annotate_report job
// and the repository it serves.
type ClassifyRequest struct {
	Sentences   []SentenceFacts
	Model       string
	MaxTokens   int
	TaskID      string
	Attribution ai.Attribution
}

// Classifier assigns postures to a batch of (sentence, fact) pairs.
// Classify returns one Classification per (SentenceFacts[i], Facts[j])
// pair that received a non-irrelevant posture; irrelevant pairs may
// be omitted from the returned slice (the caller drops them either
// way). Describe returns the operator-facing metadata for the
// /providers catalog.
type Classifier interface {
	Classify(ctx context.Context, db store.DBTX, req ClassifyRequest) ([]Classification, error)
	Describe() ProviderDescription
	// Configured reports whether the classifier is ready to run
	// (provider instance + model both present). The worker checks
	// this before invoking Classify so a deployment without a chat
	// model falls back to the keep-all path.
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