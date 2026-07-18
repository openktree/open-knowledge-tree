package refinement

import (
	"fmt"
	"strings"

	"github.com/openktree/open-knowledge-tree/backend/internal/promptset"
)


// buildUserMessage formats the concept, context, current aliases, and
// seed aliases into the user message the refinement LLM receives,
// using the given promptset's Refinement phase template.
func buildUserMessage(concept, context string, existingAliases, seedAliases []string, ps promptset.Promptset) string {
	return fmt.Sprintf(ps.Refinement,
		concept,
		context,
		strings.Join(existingAliases, ", "),
		strings.Join(seedAliases, ", "),
	)
}