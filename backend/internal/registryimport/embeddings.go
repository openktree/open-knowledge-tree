// Package registryimport is the single source of truth for importing
// pre-computed embeddings (fact + concept vectors) from a registry
// decomposition package into the local Qdrant store + Postgres
// embedded_at/embedded_model columns.
//
// It is shared by the three registry-pull paths so they stay in
// lockstep:
//   - handler.PullOneRemoteSource (the sync "Pull" button + the
//     pull_remote_batch worker) — remote_pull.go
//   - tasks.RetrieveSourceWorker (the cache-hit shortcut on fetch) —
//     retrieve_source.go
//   - tasks.PullAllFromRegistryWorker (the "Pull All" button) —
//     pull_all_from_registry.go
//
// The helper is a pure function: given the decomposition package, the
// factIDByHash + conceptIDByKey maps (built by each caller during
// fact/concept import), the Qdrant store, the per-repo store.Queries,
// and the repo ID, it upserts the vectors into Qdrant and marks the
// rows embedded in Postgres. It does NOT decide whether to skip the
// embed_facts enqueue — the caller uses the returned vector count to
// decide.
//
// Dependency graph: registryimport imports qdrantstore, store, and the
// registry providers package — all low-level, no cycles. Both the
// handler and tasks packages can import it.
package registryimport

import (
	"context"
	"log"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/openktree/open-knowledge-tree/backend/internal/providers/registry"
	"github.com/openktree/open-knowledge-tree/backend/internal/qdrantstore"
	"github.com/openktree/open-knowledge-tree/backend/internal/store"
)

// ImportEmbeddings imports the fact + concept vectors from a registry
// decomposition package into the local Qdrant store and marks the
// corresponding facts + concepts as embedded in Postgres.
//
// Parameters:
//   - decomp: the pulled decomposition package. When decomp.Embeddings
//     is nil the function is a no-op (returns 0).
//   - factIDByHash: maps the registry's fact content_hash → the local
//     fact UUID (built by the caller during fact import). Used to
//     resolve "fact:<content_hash>" embedding keys to local UUIDs.
//   - conceptIDByKey: maps "canonical_name\x00context" → the local
//     concept UUID (built by the caller during concept import). Used
//     to mark concepts embedded in Postgres.
//   - qdrant: the Qdrant store. When nil, vectors are not upserted
//     (but facts/concepts are still NOT marked embedded — the caller
//     should let embed_facts re-embed).
//   - expectedEmbModel: the local embedding config's model id (bare
//     or prefixed). When the decomposition's embedding model doesn't
//     match (after BareModelID normalization), the import is skipped
//     (the vectors are in a foreign space) and the function returns 0
//     so the caller lets embed_facts re-embed with the correct model.
//   - logPrefix: the caller's log prefix (e.g. "remote:",
//     "retrieve_source:", "pull_all_from_registry:") so log lines
//     attribute to the right call site.
//
// Returns the number of fact + concept vectors upserted to Qdrant.
// The caller uses a non-zero return to skip the embed_facts enqueue
// (the facts are already embedded + marked).
func ImportEmbeddings(
	ctx context.Context,
	decomp *registry.DecompositionPackage,
	factIDByHash map[string]pgtype.UUID,
	conceptIDByKey map[string]pgtype.UUID,
	qdrant *qdrantstore.Store,
	queries *store.Queries,
	repoID pgtype.UUID,
	expectedEmbModel string,
	logPrefix string,
) int {
	if decomp == nil || decomp.Embeddings == nil || len(decomp.Embeddings.Vectors) == 0 {
		return 0
	}

	// Embedding model compatibility guard: skip import when the
	// decomposition's embedding model doesn't match the local config
	// (after bare-name normalization). The vectors would be in a
	// different space; let embed_facts re-embed with the correct model.
	embModel := decomp.Embeddings.Model
	if expectedEmbModel != "" && registry.BareModelID(embModel) != registry.BareModelID(expectedEmbModel) {
		log.Printf("%s skipping embedding import: decomposition model %q != local %q (different vector space; embed_facts will re-embed)",
			logPrefix, embModel, expectedEmbModel)
		return 0
	}

	if qdrant == nil {
		return 0
	}

	// Resolve embedding keys to local UUIDs. The push path keys fact
	// embeddings by "fact:<content_hash>" (resolved via factIDByHash)
	// and concept embeddings by "concept:<uuid>" (best-effort match
	// via conceptIDByKey).
	localUUIDByEmbKey := make(map[string]pgtype.UUID, len(decomp.Embeddings.Vectors))
	for embKey := range decomp.Embeddings.Vectors {
		parts := strings.SplitN(embKey, ":", 2)
		if len(parts) != 2 {
			continue
		}
		if parts[0] == "fact" {
			if fID, ok := factIDByHash[parts[1]]; ok {
				localUUIDByEmbKey[embKey] = fID
			}
		}
	}

	// Build Qdrant points using local UUIDs.
	var factPoints []qdrantstore.FactPoint
	var conceptPoints []qdrantstore.ConceptPoint
	for embKey, values := range decomp.Embeddings.Vectors {
		localID, ok := localUUIDByEmbKey[embKey]
		if !ok {
			continue
		}
		localUUID, err := uuid.Parse(pgUUIDToString(localID))
		if err != nil {
			continue
		}
		vec := make([]float32, len(values))
		for i, v := range values {
			vec[i] = float32(v)
		}
		parts := strings.SplitN(embKey, ":", 2)
		switch parts[0] {
		case "fact":
			factPoints = append(factPoints, qdrantstore.FactPoint{
				ID:           localUUID,
				Vector:       vec,
				RepositoryID: pgtypeToUUID(repoID),
				Status:       "new",
			})
		case "concept":
			conceptPoints = append(conceptPoints, qdrantstore.ConceptPoint{
				ID:           localUUID,
				Vector:       vec,
				RepositoryID: pgtypeToUUID(repoID),
			})
		}
	}

	if len(factPoints) > 0 {
		if err := qdrant.UpsertFactVectors(ctx, factPoints); err != nil {
			log.Printf("%s upserting fact vectors: %v", logPrefix, err)
		}
	}
	if len(conceptPoints) > 0 {
		if err := qdrant.UpsertConceptVectors(ctx, conceptPoints); err != nil {
			log.Printf("%s upserting concept vectors: %v", logPrefix, err)
		}
	}

	// Mark facts + concepts as embedded in Postgres. Use the actual
	// embedding model (decomp.Embeddings.Model), not the generation
	// model (decomp.ModelID), so the reconciler can detect a mismatch
	// with the local embedding config.
	embModelPtr := strPtrOrNil(embModel)
	for _, f := range decomp.Facts {
		if fID, ok := factIDByHash[f.ContentHash]; ok {
			if _, err := queries.MarkFactEmbedded(ctx, store.MarkFactEmbeddedParams{
				ID:            fID,
				EmbeddedModel: embModelPtr,
			}); err != nil {
				log.Printf("%s marking fact embedded: %v", logPrefix, err)
			}
		}
	}
	for _, c := range decomp.Concepts {
		if c.CanonicalName == "" {
			continue
		}
		conceptKey := c.CanonicalName + "\x00" + c.Context
		conceptID, ok := conceptIDByKey[conceptKey]
		if !ok {
			continue
		}
		if _, err := queries.MarkConceptEmbedded(ctx, store.MarkConceptEmbeddedParams{
			ID:            conceptID,
			EmbeddedModel: embModelPtr,
		}); err != nil {
			log.Printf("%s marking concept embedded: %v", logPrefix, err)
		}
	}

	return len(factPoints) + len(conceptPoints)
}

func strPtrOrNil(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func pgUUIDToString(id pgtype.UUID) string {
	if !id.Valid {
		return ""
	}
	return uuid.UUID(id.Bytes).String()
}

func pgtypeToUUID(id pgtype.UUID) uuid.UUID {
	return uuid.UUID(id.Bytes)
}
