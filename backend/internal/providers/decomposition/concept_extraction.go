package decomposition

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/openktree/open-knowledge-tree/backend/internal/providers/ai"
	"github.com/openktree/open-knowledge-tree/backend/internal/promptset"
	"github.com/openktree/open-knowledge-tree/backend/internal/store"
)


// FactInput is one fact in a batch submitted to the concept-
// extraction provider. Index is the 0-based position of the fact
// within the batch; the provider echoes it back as FactIndex on
// every ExtractedConcept it returns for that fact, so the worker
// can map concepts back to their originating fact without relying
// on output order.
type FactInput struct {
	Index int    `json:"index"`
	Text  string `json:"text"`
}

// ExtractedConcept is one concept returned by the concept-extraction
// provider for a single fact in the batch. FactIndex is the 0-based
// position of the originating fact within the input batch (matches
// FactInput.Index); Concept is the 1-2 word surface form extracted
// from the fact text; Context is the L3 ontology class label
// assigned from the prompt's allowed list; SeedAliases are 2-3
// short forms generated inline to boost text-search match chance
// against existing concepts (the worker merges them into the
// matched concept's alias set for free, no LLM call).
type ExtractedConcept struct {
	FactIndex   int      `json:"fact_index"`
	Concept     string   `json:"concept"`
	Context     string   `json:"context"`
	SeedAliases []string `json:"seed_aliases,omitempty"`
}

// ContextEntry is one allowed context label the concept-extraction
// prompt offers the model, optionally with a short description used
// as a hint. The per-repository settings feature builds this list
// from repository_contexts (the curated context vocabulary + admin-
// custom contexts); the legacy global path wraps bare labels
// with an empty description.
type ContextEntry struct {
	Label       string
	Description string
}

type ConceptExtractionProvider interface {
	ExtractConcepts(ctx context.Context, db store.DBTX, facts []FactInput, contexts []ContextEntry, attr ConceptExtractionAttribution) ([]ExtractedConcept, error)
	Describe() ProviderDescription
}

// ConceptExtractionAttribution carries the per-call context the
// concept extractor threads into the AI provider's ChatRequest so
// the resulting ai_usage row is attributed to the repository,
// source, and River job that triggered the extraction. Mirrors
// FactExtractionAttribution so the tracking layer sees the same
// shape; the Operation string distinguishes concept extraction
// from fact extraction in the ai_usage.operation column.
type ConceptExtractionAttribution struct {
	RepositoryID string
	SourceID     string
	TaskID       string
}

type AIConceptExtractionProvider struct {
	AIProvider ai.AIProvider
	Model      string
	// promptset is the prompt set this provider uses for the
	// concept-extraction phase. Defaults to promptset.Default; a
	// worker swaps in the per-repo philosophy via WithPromptset.
	promptset promptset.Promptset
}

func NewAIConceptExtractionProvider(aiProvider ai.AIProvider, model string) *AIConceptExtractionProvider {
	return &AIConceptExtractionProvider{
		AIProvider: aiProvider,
		Model:      model,
		promptset:  promptset.Default,
	}
}

// WithPromptset returns a copy of the provider that uses the given
// promptset's ConceptExtraction phase.
func (p *AIConceptExtractionProvider) WithPromptset(ps promptset.Promptset) *AIConceptExtractionProvider {
	clone := *p
	clone.promptset = ps
	return &clone
}

func (p *AIConceptExtractionProvider) Describe() ProviderDescription {
	aiDesc := p.AIProvider.Describe()
	configured := aiDesc.Configured && p.Model != ""
	return ProviderDescription{
		Name:        "AI concept extractor",
		Description: "Asks a configured chat model to enumerate the concepts (people, places, molecules, organizations, ideas) a fact mentions, assigning each a context from the embedded context vocabulary and emitting seed aliases to boost text-search matching.",
		Requires:    "providers.decomposition.concept_extraction.{enabled,provider,model} and the underlying AI provider's API key",
		Configured:  configured,
		Supports:    []string{"concept_extraction"},
		Notes:       "Provider is " + aiDesc.Name + ". Per-fact failures are logged and the fact is skipped; the repo pass still completes.",
		Config: map[string]string{
			"ai_provider": aiDesc.Name,
			"model":       p.Model,
		},
	}
}

func (p *AIConceptExtractionProvider) ExtractConcepts(ctx context.Context, db store.DBTX, facts []FactInput, contexts []ContextEntry, attr ConceptExtractionAttribution) ([]ExtractedConcept, error) {
	// Drop empty/whitespace-only facts; they would waste prompt tokens
	// and the model cannot extract anything from them.
	var filtered []FactInput
	for _, f := range facts {
		if strings.TrimSpace(f.Text) == "" {
			continue
		}
		filtered = append(filtered, f)
	}
	if len(filtered) == 0 {
		return nil, nil
	}
	if len(contexts) == 0 {
		return nil, fmt.Errorf("concept extraction: allowed context list is empty; repository has no contexts configured")
	}

	prompt := buildConceptExtractionPrompt(filtered, contexts, p.promptset)

	var taskID *string
	if attr.TaskID != "" {
		taskID = &attr.TaskID
	}
	resp, err := retryWithBackoff(ctx, retryConfig{}, "concept_extraction",
		func(callCtx context.Context) (ai.ChatResponse, error) {
			return p.AIProvider.Chat(callCtx, db, ai.ChatRequest{
				Model: p.Model,
				Messages: []ai.ChatMessage{
					{Role: "user", Content: prompt},
				},
				TaskID: taskID,
				Attribution: ai.Attribution{
					RepositoryID: attr.RepositoryID,
					SourceID:     attr.SourceID,
					Operation:    "concept_extraction",
				},
			})
		},
	)
	if err != nil {
		return nil, fmt.Errorf("concept extraction: ai chat failed: %w", err)
	}

	if len(resp.Messages) == 0 {
		return nil, nil
	}
	content := strings.TrimSpace(resp.Messages[0].Content)
	if content == "" || content == "[]" {
		return nil, nil
	}

	var concepts []ExtractedConcept
	if err := json.Unmarshal([]byte(content), &concepts); err != nil {
		cleaned := cleanJSONArray(content)
		if cleaned != "" {
			if err2 := json.Unmarshal([]byte(cleaned), &concepts); err2 != nil {
				return nil, fmt.Errorf("concept extraction: failed to parse response as JSON array: %w (raw: %s)", err, content)
			}
		} else {
			return nil, fmt.Errorf("concept extraction: failed to parse response as JSON array: %w (raw: %s)", err, content)
		}
	}
	return concepts, nil
}

// buildConceptExtractionPrompt substitutes the allowed-context list
// and the batch of facts into the prompt template. Each context is
// rendered as a bullet list entry; when an entry carries a
// description, it's appended after an em-dash as a hint. The list is
// ordered as given (the caller sorts standard vs custom) so
// the prompt is stable across runs within a repo. Each fact in the
// batch is rendered as its own block prefixed by [fact_index N] so
// the model can echo the index back on every concept it extracts
// from that fact.
func buildConceptExtractionPrompt(facts []FactInput, contexts []ContextEntry, ps promptset.Promptset) string {
	var ctxSB strings.Builder
	for _, c := range contexts {
		ctxSB.WriteString("- ")
		ctxSB.WriteString(c.Label)
		if c.Description != "" {
			ctxSB.WriteString(" — ")
			ctxSB.WriteString(c.Description)
		}
		ctxSB.WriteString("\n")
	}
	renderedContexts := ctxSB.String()

	var factsSB strings.Builder
	for _, f := range facts {
		fmt.Fprintf(&factsSB, "[fact_index %d]\n\"\"\"\n%s\n\"\"\"\n\n", f.Index, f.Text)
	}
	renderedFacts := factsSB.String()
	return fmt.Sprintf(ps.ConceptExtraction, renderedContexts, renderedFacts)
}