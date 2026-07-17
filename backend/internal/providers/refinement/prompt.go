package refinement

import (
	"fmt"
	"strings"
)

// refinementPrompt is the system message the refinement LLM receives.
// It asks the model to propose the full formal canonical name (never
// an alias or acronym), known aliases to add, and aliases to prune.
// The model is instructed that aliases must be interchangeable with
// the concept in context without changing the meaning, that it is OK
// to return no aliases, and that seed aliases must not be pruned.
const refinementPrompt = `You are a concept canonicalization system. Given a concept and its
context, propose the full formal canonical name, known aliases to add,
and aliases to prune.

## Rules
- The canonical name MUST be the most complete, formal, unambiguous
  form. Always choose a full name, NEVER an alias or acronym as
  canonical. (e.g. "Donald John Trump", not "Trump" or "DJT")
- Aliases are alternate surface forms that can replace the concept
  in a sentence without changing its meaning: short forms, initials,
  acronyms, common spellings, full names.
- Only return aliases you are confident are real references to this
  concept. Do not invent aliases. It is OK to return no aliases.
- For an acronym concept, include the full name as an alias if known.
- For a full name concept, include known acronyms/short forms as aliases.
- Aliases to prune: existing aliases that are wrong, misspelled, or
  refer to a different concept. Only prune if you are confident.
- Do not prune seed aliases (the original concept text or its short
  forms from extraction).

## Input
Concept: %s
Context: %s
Current aliases: [%s]
Seed aliases (protected): [%s]

Respond with JSON:
{"canonical_name":"...","aliases_to_add":[...],"aliases_to_prune":[...]}
Respond with ONLY the JSON object, no other text.`

// buildUserMessage formats the concept, context, current aliases, and
// seed aliases into the user message the refinement LLM receives.
func buildUserMessage(concept, context string, existingAliases, seedAliases []string) string {
	return fmt.Sprintf(refinementPrompt,
		concept,
		context,
		strings.Join(existingAliases, ", "),
		strings.Join(seedAliases, ", "),
	)
}