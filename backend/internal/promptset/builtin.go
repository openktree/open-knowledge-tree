package promptset

// BuiltinSource is the Source value the built-in promptset carries so
// the UI can badge it as non-editable and the resolver can report
// where a promptset came from.
const BuiltinSource = "builtin"

// CustomSource is the Source value for a user-defined promptset
// stored in okt_system.promptsets.
const CustomSource = "custom"

// BuiltinName is the human-readable name of the built-in promptset.
// It is NOT part of the hash input, so changing it would not change
// the promptset's identity.
const BuiltinName = "Built-in (default)"

// Default is the built-in Promptset, populated from the canonical
// prompt strings in builtin_prompts.go (the single source of truth).
// Its Hash is computed once at init via WithHash and exposed as
// DefaultHash below.
//
// The summarization phase is special: the provider builds the actual
// per-call system prompt from a template + a per-call word budget.
// The built-in promptset stores the template (with two %d verbs) so
// the hash captures the prompt text, not a specific budget. The
// provider's FormatSystemPrompt renders it at call time.
var Default = Promptset{
	Name:                BuiltinName,
	Source:              BuiltinSource,
	FactExtraction:      builtinFactExtractionPrompt,
	ImageFactExtraction: builtinImageFactExtractionPrompt,
	ConceptExtraction:   builtinConceptExtractionPrompt,
	Refinement:          builtinRefinementPrompt,
	Synthesis:           builtinSynthesisSystemPrompt,
	ImagePicker:         builtinImagePickerSystemPrompt,
	Summarization:       builtinSummarizationSystemPromptTemplate,
	Posture:             builtinPostureSystemPrompt,
}

// DefaultHash is the canonical hash of the built-in Promptset,
// computed once at package init. Migrations and config that need a
// "this is the built-in" sentinel use this value (or, equivalently,
// the empty string, which the resolver treats as "use Default").
var DefaultHash string

func init() {
	Default = Default.WithHash()
	DefaultHash = Default.Hash
}