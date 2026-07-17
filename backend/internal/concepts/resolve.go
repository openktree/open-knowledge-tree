// Package concepts holds the domain logic for concept grouping,
// disambiguation, and routing. This file implements the per-fact
// alias-match disambiguation: when a (name, context) lookup matches
// multiple existing concepts (because an alias is shared, e.g. "N"
// on both Nitrogen and Neutron), the helper picks, for one specific
// fact, the concept whose Qdrant vector is cosine-closest to that
// fact's vector.
//
// The helper is transport-agnostic and worker-agnostic: both the
// extract_concepts and refine_concepts workers route through it so
// there is exactly one call site for alias routing in the codebase.
// The decision is always per fact — the question being answered is
// "to which concept does THIS fact belong?", never "to which concept
// does this candidate/text belong?". A candidate bundling many facts
// routes each fact independently; some may go to one concept, others
// to another.
package concepts

import (
	"context"
	"fmt"
	"log"
	"math"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/openktree/open-knowledge-tree/backend/internal/providers/ai"
	"github.com/openktree/open-knowledge-tree/backend/internal/qdrantstore"
	"github.com/openktree/open-knowledge-tree/backend/internal/store"
)

// ResolveAliasMatchForFact picks the best existing concept for ONE
// fact when (name, context) matches concepts via canonical name or
// alias. It returns the winning concept and true, or the zero value
// and false when:
//   - 0 matches (no concept to link to), or
//   - >1 matches but the fact's Qdrant vector is unavailable (the
//     caller should defer the fact — leave it on its candidate / do
//     not link it — and retry once embed_facts has vectorized it).
//
// Strategy:
//   - FindConceptsByAlias (:many, no LIMIT 1, deterministic ORDER BY).
//   - 0 matches  -> (zero, false): caller treats as a miss.
//   - 1 match    -> that concept (no embedding work needed).
//   - >1 matches -> embedding tie-break:
//     1. Fetch the fact's vector (qdrant.GetFactVectorsByIDs).
//     2. Fetch the candidate concepts' vectors (GetConceptVectorsByIDs).
//     3. For any candidate missing a Qdrant vector, embed it in
//        place (Embed canonical_name+" "+context, UpsertConceptVectors,
//        MarkConceptEmbedded) so every candidate has a vector.
//     4. cosine-compare the fact vector against each candidate's
//        vector; return the highest-similarity concept.
//     5. If the fact vector is missing -> (zero, false) so the
//        caller defers the fact (embed_facts owns fact embedding;
//        we do not embed facts here).
//
// nil qdrant or embeddingProvider is treated as "no vectors
// available": a single match still resolves (no embedding needed),
// but a multi-match returns (zero, false) so the caller defers. This
// keeps Qdrant-less deployments safe — they never mis-route on a
// shared alias, they just defer until embeddings exist.
func ResolveAliasMatchForFact(
	ctx context.Context,
	queries *store.Queries,
	qdrant *qdrantstore.Store,
	embeddingProvider ai.EmbeddingProvider,
	embeddingModel string,
	repoID pgtype.UUID,
	conceptContext, name string,
	factID pgtype.UUID,
) (store.OktRepositoryConcept, bool) {
	matches, err := queries.FindConceptsByAlias(ctx, store.FindConceptsByAliasParams{
		RepositoryID: repoID,
		Context:      conceptContext,
		Name:         name,
	})
	if err != nil {
		log.Printf("concepts.ResolveAliasMatchForFact: FindConceptsByAlias: %v", err)
		return store.OktRepositoryConcept{}, false
	}
	switch len(matches) {
	case 0:
		return store.OktRepositoryConcept{}, false
	case 1:
		return matches[0], true
	}

	// >1 match: ambiguous alias — disambiguate by embedding distance.
	winner, ok := disambiguateByEmbedding(ctx, queries, qdrant, embeddingProvider, embeddingModel, repoID, matches, factID)
	if !ok {
		return store.OktRepositoryConcept{}, false
	}
	return winner, true
}

// disambiguateByEmbedding is the multi-match branch. It returns the
// cosine-closest concept to the fact, or (zero, false) when the
// fact's vector is unavailable (caller defers). Missing candidate
// concept vectors are embedded in place so every candidate
// participates in the comparison.
func disambiguateByEmbedding(
	ctx context.Context,
	queries *store.Queries,
	qdrant *qdrantstore.Store,
	embeddingProvider ai.EmbeddingProvider,
	embeddingModel string,
	repoID pgtype.UUID,
	candidates []store.OktRepositoryConcept,
	factID pgtype.UUID,
) (store.OktRepositoryConcept, bool) {
	if qdrant == nil {
		log.Printf("concepts.disambiguateByEmbedding: qdrant store is nil; cannot tie-break %d matches for fact %s, deferring",
			len(candidates), pgUUIDToString(factID))
		return store.OktRepositoryConcept{}, false
	}

	// 1. Fact vector.
	factUUID, ok := pgUUIDToUUID(factID)
	if !ok {
		return store.OktRepositoryConcept{}, false
	}
	factVecs, err := qdrant.GetFactVectorsByIDs(ctx, []uuid.UUID{factUUID})
	if err != nil {
		log.Printf("concepts.disambiguateByEmbedding: fetching fact vector for %s: %v", pgUUIDToString(factID), err)
		return store.OktRepositoryConcept{}, false
	}
	factPt, hasFactVec := factVecs[factUUID]
	if !hasFactVec || len(factPt.Vector) == 0 {
		// Fact not embedded yet (embed_facts failed/was skipped). Defer
		// — we do not embed facts here (embed_facts owns that).
		return store.OktRepositoryConcept{}, false
	}

	// 2. Candidate concept vectors.
	candidateUUIDs := make([]uuid.UUID, 0, len(candidates))
	for _, c := range candidates {
		if u, ok := pgUUIDToUUID(c.ID); ok {
			candidateUUIDs = append(candidateUUIDs, u)
		}
	}
	conceptVecs, err := qdrant.GetConceptVectorsByIDs(ctx, candidateUUIDs)
	if err != nil {
		log.Printf("concepts.disambiguateByEmbedding: fetching concept vectors: %v", err)
		return store.OktRepositoryConcept{}, false
	}

	// 3. Embed any candidate missing a vector, in place.
	repoUUID, repoOK := pgUUIDToUUID(repoID)
	if !repoOK {
		return store.OktRepositoryConcept{}, false
	}
	for _, c := range candidates {
		cu, ok := pgUUIDToUUID(c.ID)
		if !ok {
			continue
		}
		if pt, present := conceptVecs[cu]; present && len(pt.Vector) > 0 {
			continue
		}
		if embeddingProvider == nil {
			log.Printf("concepts.disambiguateByEmbedding: concept %s has no vector and no embedding provider; skipping candidate",
				pgUUIDToString(c.ID))
			continue
		}
		vec, err := embedConceptInPlace(ctx, embeddingProvider, embeddingModel, qdrant, repoUUID, c)
		if err != nil {
			log.Printf("concepts.disambiguateByEmbedding: embedding concept %s in place: %v",
				pgUUIDToString(c.ID), err)
			continue
		}
		conceptVecs[cu] = qdrantstore.ConceptPoint{ID: cu, Vector: vec}
		if err := markConceptEmbedded(ctx, queries, c.ID, embeddingModel); err != nil {
			log.Printf("concepts.disambiguateByEmbedding: marking concept %s embedded: %v",
				pgUUIDToString(c.ID), err)
		}
	}

	// 4. Cosine compare; pick the highest-similarity candidate that
	// actually has a vector.
	var best store.OktRepositoryConcept
	bestScore := float32(-2)
	found := false
	for _, c := range candidates {
		cu, ok := pgUUIDToUUID(c.ID)
		if !ok {
			continue
		}
		pt, present := conceptVecs[cu]
		if !present || len(pt.Vector) == 0 {
			continue
		}
		score := cosineSim(factPt.Vector, pt.Vector)
		if !found || score > bestScore {
			best = c
			bestScore = score
			found = true
		}
	}
	if !found {
		log.Printf("concepts.disambiguateByEmbedding: no candidate had a usable vector for fact %s; deferring",
			pgUUIDToString(factID))
		return store.OktRepositoryConcept{}, false
	}
	return best, true
}

// embedConceptInPlace embeds one concept's canonical_name + " " +
// context and upserts the vector into Qdrant, returning the vector.
// Used when a matched concept has no Qdrant vector (reset by
// refine/migrate, or Qdrant was down during its embed_concepts pass).
// The caller also marks the concept embedded_at so the later
// embed_concepts pass skips it.
func embedConceptInPlace(
	ctx context.Context,
	provider ai.EmbeddingProvider,
	model string,
	qdrant *qdrantstore.Store,
	repoUUID uuid.UUID,
	c store.OktRepositoryConcept,
) ([]float32, error) {
	if provider == nil {
		return nil, fmt.Errorf("embedding provider is nil")
	}
	input := c.CanonicalName + " " + c.Context
	resp, err := provider.Embed(ctx, nil, ai.EmbeddingRequest{
		Model:  model,
		Inputs: []string{input},
	})
	if err != nil {
		return nil, fmt.Errorf("embedding concept %s: %w", pgUUIDToString(c.ID), err)
	}
	if len(resp.Embeddings) != 1 {
		return nil, fmt.Errorf("embedding provider returned %d vectors for 1 input", len(resp.Embeddings))
	}
	vec := resp.Embeddings[0]
	cu, ok := pgUUIDToUUID(c.ID)
	if !ok {
		return nil, fmt.Errorf("concept id is not a valid uuid")
	}
	if err := qdrant.UpsertConceptVectors(ctx, []qdrantstore.ConceptPoint{{
		ID:           cu,
		Vector:       vec,
		RepositoryID: repoUUID,
	}}); err != nil {
		return nil, fmt.Errorf("upserting concept vector: %w", err)
	}
	return vec, nil
}

// markConceptEmbedded wraps MarkConceptEmbedded so this package
// doesn't inline store params at the call site.
func markConceptEmbedded(ctx context.Context, queries *store.Queries, conceptID pgtype.UUID, model string) error {
	var modelArg *string
	if model != "" {
		modelArg = &model
	}
	if _, err := queries.MarkConceptEmbedded(ctx, store.MarkConceptEmbeddedParams{
		ID:            conceptID,
		EmbeddedModel: modelArg,
	}); err != nil {
		return err
	}
	return nil
}

// cosineSim computes the cosine similarity between two vectors. Pure
// Go, no dependencies. Returns -1 when either vector is empty or
// zero-length (so it never wins against a real score, which is in
// [-1, 1]).
func cosineSim(a, b []float32) float32 {
	if len(a) == 0 || len(b) == 0 || len(a) != len(b) {
		return -1
	}
	var dot, na, nb float32
	for i := range a {
		dot += a[i] * b[i]
		na += a[i] * a[i]
		nb += b[i] * b[i]
	}
	if na == 0 || nb == 0 {
		return -1
	}
	// float32 sqrt is fine for similarity ranking.
	return dot / (float32sqrt(na) * float32sqrt(nb))
}

// float32sqrt is a tiny helper because math.Sqrt takes float64.
func float32sqrt(x float32) float32 {
	return float32(math.Sqrt(float64(x)))
}

// pgUUIDToUUID converts a pgtype.UUID to a google/uuid.UUID. Returns
// false when the pgtype.UUID is not valid (so callers can branch
// without a separate error).
func pgUUIDToUUID(u pgtype.UUID) (uuid.UUID, bool) {
	if !u.Valid {
		return uuid.Nil, false
	}
	// pgtype.UUID stores Bytes as [16]byte in network/big-endian
	// order, which is exactly what google/uuid expects.
	return uuid.UUID(u.Bytes), true
}

// pgUUIDToString is a local helper to avoid pulling the tasks-package
// version into this domain package. Matches the format used in logs
// elsewhere (canonical UUID string).
func pgUUIDToString(u pgtype.UUID) string {
	if !u.Valid {
		return "(invalid)"
	}
	return uuid.UUID(u.Bytes).String()
}