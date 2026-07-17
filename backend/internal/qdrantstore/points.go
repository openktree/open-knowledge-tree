package qdrantstore

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/qdrant/go-client/qdrant"
)

// Payload keys. Centralized so a typo at one call-site can't
// diverge from the rest. The payload carries repository_id and
// status only — no fact text, no source_id (Postgres is the
// single source of truth for everything except the vector).
const (
	PayloadRepositoryID = "repository_id"
	PayloadStatus       = "status"
)

// FactPoint is the upsert payload for a single fact vector. The
// point id is the fact UUID (Qdrant accepts UUID point ids
// natively). RepositoryID is stored in the payload so per-repo
// searches can filter on it; Status is stored so dedup searches
// can exclude `to_delete` facts and the catchup job can target
// stale points by payload without a separate index.
type FactPoint struct {
	ID           uuid.UUID
	Vector       []float32
	RepositoryID uuid.UUID
	Status       string
}

// Hit is a nearest-neighbor search result. ID is the fact UUID
// (the caller uses it to load the fact row from Postgres); Score
// is the cosine similarity Qdrant returned (higher = more
// similar, range [-1, 1] for Cosine distance — Qdrant surfaces
// similarity as score so 1.0 = identical).
type Hit struct {
	ID    uuid.UUID
	Score float32
}

// UpsertFactVectors upserts a batch of fact vectors into the
// collection. Point id = FactPoint.ID (UUID). Payload =
// `{repository_id, status}`. The caller passes the full vector
// (already at the collection's dimension); Qdrant validates the
// dimension on upsert and returns an error on mismatch.
func (s *Store) UpsertFactVectors(ctx context.Context, points []FactPoint) error {
	if s == nil || s.client == nil {
		return fmt.Errorf("qdrantstore: nil store")
	}
	if len(points) == 0 {
		return nil
	}
	upsertPoints := make([]*qdrant.PointStruct, 0, len(points))
	for _, p := range points {
		repoIDStr := p.RepositoryID.String()
		status := p.Status
		upsertPoints = append(upsertPoints, &qdrant.PointStruct{
			Id:      qdrant.NewIDUUID(p.ID.String()),
			Vectors: qdrant.NewVectorsDense(p.Vector),
			Payload: qdrant.NewValueMap(map[string]any{
				PayloadRepositoryID: repoIDStr,
				PayloadStatus:       status,
			}),
		})
	}
	wait := true
	if _, err := s.client.Upsert(ctx, &qdrant.UpsertPoints{
		CollectionName: s.collection,
		Wait:           &wait,
		Points:         upsertPoints,
	}); err != nil {
		return fmt.Errorf("qdrantstore: upserting %d points: %w", len(points), err)
	}
	return nil
}

// SearchSimilar searches the collection for the nearest neighbors
// of `vec` within the repository `repositoryID`, excluding the
// fact `excludeFactID` (the caller's own id), with a minimum
// cosine similarity score of `minScore`, returning at most `limit`
// hits. The repository filter is a `must` condition so Qdrant
// only searches the repo's points; the self-exclusion is a `must
// not` (has_id) so the caller's own vector is never returned as
// its own nearest neighbor.
func (s *Store) SearchSimilar(ctx context.Context, vec []float32, repositoryID uuid.UUID, excludeFactID uuid.UUID, minScore float32, limit int) ([]Hit, error) {
	if s == nil || s.client == nil {
		return nil, fmt.Errorf("qdrantstore: nil store")
	}
	if limit <= 0 {
		limit = 1
	}
	repoFilter := qdrant.NewMatchKeyword(PayloadRepositoryID, repositoryID.String())
	filter := &qdrant.Filter{
		Must: []*qdrant.Condition{repoFilter},
		MustNot: []*qdrant.Condition{
			qdrant.NewHasID(qdrant.NewIDUUID(excludeFactID.String())),
		},
	}
	lim := uint64(limit)
	scoreThreshold := minScore
	res, err := s.client.Query(ctx, &qdrant.QueryPoints{
		CollectionName: s.collection,
		Query:          qdrant.NewQueryDense(vec),
		Filter:         filter,
		ScoreThreshold: &scoreThreshold,
		Limit:          &lim,
	})
	if err != nil {
		return nil, fmt.Errorf("qdrantstore: searching collection %q: %w", s.collection, err)
	}
	return scoredPointsToHits(res)
}

// SearchSimilarByID searches for the nearest neighbors of the
// fact `factID` (Qdrant fetches the vector server-side from the
// point id via a recommend-by-id query), within the repository
// `repositoryID`, excluding `excludeFactID` (usually == factID
// so a fact is never its own nearest neighbor), with a minimum
// cosine similarity score of `minScore`, returning at most
// `limit` hits. This is the dedup worker's entry point: it has
// the fact UUID in hand, not the vector, and avoids a separate
// "read vector by id" round-trip by letting Qdrant look it up.
func (s *Store) SearchSimilarByID(ctx context.Context, factID uuid.UUID, repositoryID uuid.UUID, excludeFactID uuid.UUID, minScore float32, limit int) ([]Hit, error) {
	if s == nil || s.client == nil {
		return nil, fmt.Errorf("qdrantstore: nil store")
	}
	if limit <= 0 {
		limit = 1
	}
	repoFilter := qdrant.NewMatchKeyword(PayloadRepositoryID, repositoryID.String())
	filter := &qdrant.Filter{
		Must: []*qdrant.Condition{repoFilter},
		MustNot: []*qdrant.Condition{
			qdrant.NewHasID(qdrant.NewIDUUID(excludeFactID.String())),
		},
	}
	lim := uint64(limit)
	scoreThreshold := minScore
	res, err := s.client.Query(ctx, &qdrant.QueryPoints{
		CollectionName: s.collection,
		// NewQueryID builds a recommend-by-id query: Qdrant
		// reads the vector for factID server-side and finds
		// its nearest neighbors.
		Query:          qdrant.NewQueryID(qdrant.NewIDUUID(factID.String())),
		Filter:         filter,
		ScoreThreshold: &scoreThreshold,
		Limit:          &lim,
	})
	if err != nil {
		return nil, fmt.Errorf("qdrantstore: searching collection %q by id: %w", s.collection, err)
	}
	return scoredPointsToHits(res)
}

// DeleteFactVectors deletes the given fact UUIDs from the
// collection. Idempotent: deleting a point that doesn't exist is
// a no-op (Qdrant returns success). Used by cleanup_facts and
// fact_catchup to mirror Postgres deletes.
func (s *Store) DeleteFactVectors(ctx context.Context, ids []uuid.UUID) error {
	if s == nil || s.client == nil {
		return fmt.Errorf("qdrantstore: nil store")
	}
	if len(ids) == 0 {
		return nil
	}
	pointIDs := make([]*qdrant.PointId, 0, len(ids))
	for _, id := range ids {
		pointIDs = append(pointIDs, qdrant.NewIDUUID(id.String()))
	}
	wait := true
	if _, err := s.client.Delete(ctx, &qdrant.DeletePoints{
		CollectionName: s.collection,
		Wait:           &wait,
		Points:         qdrant.NewPointsSelectorIDs(pointIDs),
	}); err != nil {
		return fmt.Errorf("qdrantstore: deleting %d points: %w", len(ids), err)
	}
	return nil
}

// UpdateFactStatusPayload updates the `status` payload field on a
// single point. Called by deduplicate_facts when a fact transitions
// `new → stable` (survivor) or `new → to_delete` (loser) so future
// searches can filter by status (e.g. exclude `to_delete`).
func (s *Store) UpdateFactStatusPayload(ctx context.Context, factID uuid.UUID, status string) error {
	if s == nil || s.client == nil {
		return fmt.Errorf("qdrantstore: nil store")
	}
	wait := true
	if _, err := s.client.SetPayload(ctx, &qdrant.SetPayloadPoints{
		CollectionName: s.collection,
		Wait:           &wait,
		Payload: qdrant.NewValueMap(map[string]any{
			PayloadStatus: status,
		}),
		PointsSelector: qdrant.NewPointsSelector(qdrant.NewIDUUID(factID.String())),
	}); err != nil {
		return fmt.Errorf("qdrantstore: updating status payload for fact %s: %w", factID, err)
	}
	return nil
}

// pointIDToUUID extracts the UUID from a Qdrant PointId. Returns
// ok=false when the point id is a numeric id (which the
// application never writes) or when the UUID string is invalid.
func pointIDToUUID(p *qdrant.PointId) (uuid.UUID, bool) {
	if p == nil {
		return uuid.Nil, false
	}
	if u := p.GetUuid(); u != "" {
		id, err := uuid.Parse(u)
		if err != nil {
			return uuid.Nil, false
		}
		return id, true
	}
	return uuid.Nil, false
}

// scoredPointsToHits converts Qdrant's Query response into the
// application's Hit shape. Non-UUID point ids (which the
// application never writes) are skipped rather than aborting the
// whole search, so a polluted collection degrades gracefully.
func scoredPointsToHits(res []*qdrant.ScoredPoint) ([]Hit, error) {
	hits := make([]Hit, 0, len(res))
	for _, sp := range res {
		id, ok := pointIDToUUID(sp.Id)
		if !ok {
			continue
		}
		hits = append(hits, Hit{ID: id, Score: sp.Score})
	}
	return hits, nil
}

// --- Concept collection methods ---
//
// The concept collection (okt_concepts by default) stores one
// vector per concept: the embedding of canonical_name + " " +
// context. Point id = concept UUID; payload = {repository_id}.
// No status field (concepts don't have a lifecycle like facts);
// the caller filters by repository_id at query time.

// ConceptPoint is the upsert payload for a single concept vector.
// Point id = ConceptPoint.ID (UUID). RepositoryID is stored in the
// payload so per-repo searches can filter on it.
type ConceptPoint struct {
	ID           uuid.UUID
	Vector       []float32
	RepositoryID uuid.UUID
}

// UpsertConceptVectors upserts a batch of concept vectors into the
// concept collection. Mirrors UpsertFactVectors but targets
// Store.conceptCollection and writes only the repository_id payload
// (no status — concepts don't have a lifecycle).
func (s *Store) UpsertConceptVectors(ctx context.Context, points []ConceptPoint) error {
	if s == nil || s.client == nil {
		return fmt.Errorf("qdrantstore: nil store")
	}
	if len(points) == 0 {
		return nil
	}
	upsertPoints := make([]*qdrant.PointStruct, 0, len(points))
	for _, p := range points {
		repoIDStr := p.RepositoryID.String()
		upsertPoints = append(upsertPoints, &qdrant.PointStruct{
			Id:      qdrant.NewIDUUID(p.ID.String()),
			Vectors: qdrant.NewVectorsDense(p.Vector),
			Payload: qdrant.NewValueMap(map[string]any{
				PayloadRepositoryID: repoIDStr,
			}),
		})
	}
	wait := true
	if _, err := s.client.Upsert(ctx, &qdrant.UpsertPoints{
		CollectionName: s.conceptCollection,
		Wait:           &wait,
		Points:         upsertPoints,
	}); err != nil {
		return fmt.Errorf("qdrantstore: upserting %d concept points: %w", len(points), err)
	}
	return nil
}

// SearchSimilarConcepts searches the concept collection for the
// nearest neighbors of `vec` within the repository `repositoryID`,
// with a minimum cosine similarity score of `minScore`, returning
// at most `limit` hits. Mirrors SearchSimilar but targets the
// concept collection and omits the self-exclusion filter (concepts
// don't have a "find my nearest neighbor excluding myself" use
// case today; the caller passes excludeID when it needs it).
func (s *Store) SearchSimilarConcepts(ctx context.Context, vec []float32, repositoryID uuid.UUID, minScore float32, limit int) ([]Hit, error) {
	if s == nil || s.client == nil {
		return nil, fmt.Errorf("qdrantstore: nil store")
	}
	if limit <= 0 {
		limit = 1
	}
	repoFilter := qdrant.NewMatchKeyword(PayloadRepositoryID, repositoryID.String())
	filter := &qdrant.Filter{
		Must: []*qdrant.Condition{repoFilter},
	}
	lim := uint64(limit)
	scoreThreshold := minScore
	res, err := s.client.Query(ctx, &qdrant.QueryPoints{
		CollectionName: s.conceptCollection,
		Query:          qdrant.NewQueryDense(vec),
		Filter:         filter,
		ScoreThreshold: &scoreThreshold,
		Limit:          &lim,
	})
	if err != nil {
		return nil, fmt.Errorf("qdrantstore: searching concept collection %q: %w", s.conceptCollection, err)
	}
	return scoredPointsToHits(res)
}

// DeleteConceptVectors deletes the given concept UUIDs from the
// concept collection. Idempotent. Used when a concept is deleted
// (Phase 2 may add concept deletion; Phase 1 leaves concepts in
// place).
func (s *Store) DeleteConceptVectors(ctx context.Context, ids []uuid.UUID) error {
	if s == nil || s.client == nil {
		return fmt.Errorf("qdrantstore: nil store")
	}
	if len(ids) == 0 {
		return nil
	}
	pointIDs := make([]*qdrant.PointId, 0, len(ids))
	for _, id := range ids {
		pointIDs = append(pointIDs, qdrant.NewIDUUID(id.String()))
	}
	wait := true
	if _, err := s.client.Delete(ctx, &qdrant.DeletePoints{
		CollectionName: s.conceptCollection,
		Wait:           &wait,
		Points:         qdrant.NewPointsSelectorIDs(pointIDs),
	}); err != nil {
		return fmt.Errorf("qdrantstore: deleting %d concept points: %w", len(ids), err)
	}
	return nil
}

// GetFactVectorsByIDs fetches specific fact vectors by their point
// IDs from the fact collection. Uses Qdrant's Get by ID API, which
// is more efficient than scrolling the entire collection. Returns a
// map from UUID to FactPoint for the caller's convenience. Silently
// skips IDs that don't exist in Qdrant (not-yet-embedded facts).
func (s *Store) GetFactVectorsByIDs(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]FactPoint, error) {
	if s == nil || s.client == nil {
		return nil, fmt.Errorf("qdrantstore: nil store")
	}
	if len(ids) == 0 {
		return nil, nil
	}
	result := make(map[uuid.UUID]FactPoint, len(ids))
	// Batch the Get() calls so each response stays under the gRPC
	// max message size (4MB default). Each 768-dim float32 vector
	// is ~3KB, so 100 vectors ≈ 300KB + payload overhead — well
	// under the limit. Sources with thousands of facts would
	// otherwise exceed it in a single call.
	const batchSize = 100
	for start := 0; start < len(ids); start += batchSize {
		end := start + batchSize
		if end > len(ids) {
			end = len(ids)
		}
		batch := ids[start:end]
		pointIDs := make([]*qdrant.PointId, 0, len(batch))
		for _, id := range batch {
			pointIDs = append(pointIDs, qdrant.NewIDUUID(id.String()))
		}
		res, err := s.client.Get(ctx, &qdrant.GetPoints{
			CollectionName: s.collection,
			Ids:            pointIDs,
			WithVectors:    qdrant.NewWithVectors(true),
			WithPayload:    qdrant.NewWithPayload(true),
		})
		if err != nil {
			return nil, fmt.Errorf("qdrantstore: getting %d fact vectors (batch %d-%d): %w", len(ids), start, end, err)
		}
		for _, sp := range res {
			id, ok := pointIDToUUID(sp.GetId())
			if !ok {
				continue
			}
			vo := sp.GetVectors()
			if vo == nil {
				continue
			}
			vectorOut := vo.GetVector()
			if vectorOut == nil {
				continue
			}
			// Qdrant v1.18+ returns the actual vector data in
			// the Dense oneof field, not the deprecated top-level
			// Data field. Use GetDense().GetData() to read it.
			denseVec := vectorOut.GetDense()
			var vec []float32
			if denseVec != nil {
				vec = denseVec.GetData()
			} else {
				vec = vectorOut.GetData()
			}
			if len(vec) == 0 {
				continue
			}
			payload := sp.GetPayload()
			repoIDStr := payload[PayloadRepositoryID].GetStringValue()
			repoUUID, err := uuid.Parse(repoIDStr)
			if err != nil {
				continue
			}
			status := payload[PayloadStatus].GetStringValue()
			result[id] = FactPoint{
				ID:           id,
				Vector:       vec,
				RepositoryID: repoUUID,
				Status:       status,
			}
		}
	}
	return result, nil
}

// GetConceptVectorsByIDs fetches specific concept vectors by their
// point IDs from the concept collection. Mirrors
// GetFactVectorsByIDs but targets the concept collection. Returns a
// map from UUID to ConceptPoint. Silently skips IDs that don't exist
// (not-yet-embedded concepts).
func (s *Store) GetConceptVectorsByIDs(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]ConceptPoint, error) {
	if s == nil || s.client == nil {
		return nil, fmt.Errorf("qdrantstore: nil store")
	}
	if len(ids) == 0 {
		return nil, nil
	}
	result := make(map[uuid.UUID]ConceptPoint, len(ids))
	const batchSize = 100
	for start := 0; start < len(ids); start += batchSize {
		end := start + batchSize
		if end > len(ids) {
			end = len(ids)
		}
		batch := ids[start:end]
		pointIDs := make([]*qdrant.PointId, 0, len(batch))
		for _, id := range batch {
			pointIDs = append(pointIDs, qdrant.NewIDUUID(id.String()))
		}
		res, err := s.client.Get(ctx, &qdrant.GetPoints{
			CollectionName: s.conceptCollection,
			Ids:            pointIDs,
			WithVectors:    qdrant.NewWithVectors(true),
			WithPayload:    qdrant.NewWithPayload(true),
		})
		if err != nil {
			return nil, fmt.Errorf("qdrantstore: getting %d concept vectors (batch %d-%d): %w", len(ids), start, end, err)
		}
		for _, sp := range res {
			id, ok := pointIDToUUID(sp.GetId())
			if !ok {
				continue
			}
			vo := sp.GetVectors()
			if vo == nil {
				continue
			}
			vectorOut := vo.GetVector()
			if vectorOut == nil {
				continue
			}
			denseVec := vectorOut.GetDense()
			var vec []float32
			if denseVec != nil {
				vec = denseVec.GetData()
			} else {
				vec = vectorOut.GetData()
			}
			if len(vec) == 0 {
				continue
			}
			result[id] = ConceptPoint{
				ID:     id,
				Vector: vec,
			}
		}
	}
	return result, nil
}