package qdrantstore

import (
	"context"
	"fmt"
	"log"

	"github.com/qdrant/go-client/qdrant"
)

// EnsureCollection creates the Qdrant collection if it does not
// exist, with a Cosine-distance vector at the given dimensions and
// a payload index on `repository_id` so the per-repo nearest-
// neighbor searches are fast. When the collection already exists
// and its configured dimension does not match `dimensions`:
//
//   - if the Store was built with `allow_recreate: true` (dev
//     affordance), the collection is dropped and recreated at the
//     new dimension. This re-embedding requirement is why
//     allow_recreate must never be enabled in production — at
//     millions of facts a drop+recreate means re-embedding
//     everything.
//   - otherwise the method returns an error so the operator sees
//     the mismatch at boot instead of a confusing 400 on the
//     first upsert.
//
// The payload index on `repository_id` is created unconditionally
// on a fresh collection; on an existing collection with the index
// already present the call is a no-op (Qdrant returns success).
func (s *Store) EnsureCollection(ctx context.Context, dimensions int) error {
	if s == nil || s.client == nil {
		return fmt.Errorf("qdrantstore: nil store")
	}
	if dimensions <= 0 {
		return fmt.Errorf("qdrantstore: dimensions must be > 0, got %d", dimensions)
	}

	exists, err := s.client.CollectionExists(ctx, s.collection)
	if err != nil {
		return fmt.Errorf("qdrantstore: checking collection %q: %w", s.collection, err)
	}

	if !exists {
		return s.createCollection(ctx, dimensions)
	}

	// Collection exists — verify the dimension matches.
	info, err := s.client.GetCollectionInfo(ctx, s.collection)
	if err != nil {
		return fmt.Errorf("qdrantstore: reading collection info for %q: %w", s.collection, err)
	}
	currentDim, err := collectionDimensions(info)
	if err != nil {
		// A named-vector collection (ParamsMap) would land here.
		// The application uses a single default vector, so treat
		// any other shape as a configuration error rather than
		// guessing which named vector to compare against.
		return fmt.Errorf("qdrantstore: %w", err)
	}
	if currentDim == dimensions {
		return nil
	}

	if !s.allowRecreate {
		return fmt.Errorf("qdrantstore: collection %q has dimension %d but config declares %d; set qdrant.allow_recreate: true (dev only) to drop+recreate, then re-embed all facts", s.collection, currentDim, dimensions)
	}
	log.Printf("qdrantstore: allow_recreate is true — dropping collection %q (dim %d) to recreate at dim %d", s.collection, currentDim, dimensions)
	if err := s.client.DeleteCollection(ctx, s.collection); err != nil {
		return fmt.Errorf("qdrantstore: dropping collection %q for dimension change: %w", s.collection, err)
	}
	return s.createCollection(ctx, dimensions)
}

func (s *Store) createCollection(ctx context.Context, dimensions int) error {
	err := s.client.CreateCollection(ctx, &qdrant.CreateCollection{
		CollectionName: s.collection,
		VectorsConfig: qdrant.NewVectorsConfig(&qdrant.VectorParams{
			Size:     uint64(dimensions),
			Distance: qdrant.Distance_Cosine,
		}),
	})
	if err != nil {
		return fmt.Errorf("qdrantstore: creating collection %q (dim %d): %w", s.collection, dimensions, err)
	}
	// Payload index on repository_id so the per-repo nearest-
	// neighbor searches filter cheaply. A keyword index is the
	// right choice for an exact-match UUID string filter.
	wait := true
	if _, err := s.client.CreateFieldIndex(ctx, &qdrant.CreateFieldIndexCollection{
		CollectionName: s.collection,
		FieldName:      PayloadRepositoryID,
		FieldType:      qdrant.FieldType_FieldTypeKeyword.Enum(),
		Wait:           &wait,
	}); err != nil {
		return fmt.Errorf("qdrantstore: creating repository_id payload index on %q: %w", s.collection, err)
	}
	log.Printf("qdrantstore: collection %q ready (dim %d, Cosine, repository_id payload index)", s.collection, dimensions)
	return nil
}

// collectionDimensions extracts the vector size from a single-
// vector (Params) collection info. Returns an error for named-
// vector (ParamsMap) collections, which the application does not
// use.
func collectionDimensions(info *qdrant.CollectionInfo) (int, error) {
	if info == nil || info.Config == nil || info.Config.Params == nil || info.Config.Params.VectorsConfig == nil {
		return 0, fmt.Errorf("collection config is missing vectors_config")
	}
	if params := info.Config.Params.VectorsConfig.GetParams(); params != nil {
		return int(params.GetSize()), nil
	}
	return 0, fmt.Errorf("collection uses a named-vector map (ParamsMap), which the application does not support; recreate the collection with a single default vector")
}

// EnsureConceptCollection creates the concept collection (the
// okt_concepts collection by default) at the given dimensions if it
// does not exist, and verifies the dimension matches when it does.
// Mirrors EnsureCollection but targets Store.conceptCollection.
// Called once at boot by the wiring layer (cmd/app/api.go) after
// EnsureCollection so both collections are ready before the task
// manager starts.
func (s *Store) EnsureConceptCollection(ctx context.Context, dimensions int) error {
	if s == nil || s.client == nil {
		return fmt.Errorf("qdrantstore: nil store")
	}
	if dimensions <= 0 {
		return fmt.Errorf("qdrantstore: dimensions must be > 0, got %d", dimensions)
	}

	exists, err := s.client.CollectionExists(ctx, s.conceptCollection)
	if err != nil {
		return fmt.Errorf("qdrantstore: checking concept collection %q: %w", s.conceptCollection, err)
	}

	if !exists {
		return s.createConceptCollection(ctx, dimensions)
	}

	info, err := s.client.GetCollectionInfo(ctx, s.conceptCollection)
	if err != nil {
		return fmt.Errorf("qdrantstore: reading collection info for concept collection %q: %w", s.conceptCollection, err)
	}
	currentDim, err := collectionDimensions(info)
	if err != nil {
		return fmt.Errorf("qdrantstore: %w", err)
	}
	if currentDim == dimensions {
		return nil
	}

	if !s.allowRecreate {
		return fmt.Errorf("qdrantstore: concept collection %q has dimension %d but config declares %d; set qdrant.allow_recreate: true (dev only) to drop+recreate, then re-embed all concepts", s.conceptCollection, currentDim, dimensions)
	}
	log.Printf("qdrantstore: allow_recreate is true — dropping concept collection %q (dim %d) to recreate at dim %d", s.conceptCollection, currentDim, dimensions)
	if err := s.client.DeleteCollection(ctx, s.conceptCollection); err != nil {
		return fmt.Errorf("qdrantstore: dropping concept collection %q for dimension change: %w", s.conceptCollection, err)
	}
	return s.createConceptCollection(ctx, dimensions)
}

func (s *Store) createConceptCollection(ctx context.Context, dimensions int) error {
	err := s.client.CreateCollection(ctx, &qdrant.CreateCollection{
		CollectionName: s.conceptCollection,
		VectorsConfig: qdrant.NewVectorsConfig(&qdrant.VectorParams{
			Size:     uint64(dimensions),
			Distance: qdrant.Distance_Cosine,
		}),
	})
	if err != nil {
		return fmt.Errorf("qdrantstore: creating concept collection %q (dim %d): %w", s.conceptCollection, dimensions, err)
	}
	wait := true
	if _, err := s.client.CreateFieldIndex(ctx, &qdrant.CreateFieldIndexCollection{
		CollectionName: s.conceptCollection,
		FieldName:      PayloadRepositoryID,
		FieldType:      qdrant.FieldType_FieldTypeKeyword.Enum(),
		Wait:           &wait,
	}); err != nil {
		return fmt.Errorf("qdrantstore: creating repository_id payload index on concept collection %q: %w", s.conceptCollection, err)
	}
	log.Printf("qdrantstore: concept collection %q ready (dim %d, Cosine, repository_id payload index)", s.conceptCollection, dimensions)
	return nil
}