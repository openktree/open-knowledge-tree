package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/google/uuid"

	"github.com/openktree/knowledge-registry/internal/model"
	"github.com/openktree/knowledge-registry/internal/storage"
	"github.com/openktree/knowledge-registry/internal/store"
)

const schemaVersion = 1

// defaultRepoID is the single namespace used by the registry.
// Each registry instance serves one logical scope; spin up
// another instance for isolation.
const defaultRepoID = "default"

// Storage is the interface the Registry needs from the object
// store. *storage.S3Store satisfies it. Extracted so tests can
// inject a mock without a real MinIO/S3 instance.
type Storage interface {
	StoreJSON(ctx context.Context, key string, data []byte) error
	ReadAll(ctx context.Context, key string) ([]byte, string, error)
	PresignedURL(ctx context.Context, key string, ttl time.Duration) (string, error)
	PresignedPUTURL(ctx context.Context, key string, ttl time.Duration) (string, error)
}

type Registry struct {
	store      store.MetadataStore
	storage    Storage
	presignTTL time.Duration
}

func New(mstore store.MetadataStore, s3store Storage, presignTTL int) *Registry {
	ttl := time.Duration(presignTTL) * time.Second
	if ttl <= 0 {
		ttl = 1 * time.Hour
	}
	return &Registry{
		store:      mstore,
		storage:    s3store,
		presignTTL: ttl,
	}
}

// EnsureDefaultRepo creates the "default" repository if it doesn't
// exist. Each registry instance serves one logical scope, so a
// single default repo covers the common case.
func (r *Registry) EnsureDefaultRepo(ctx context.Context) error {
	if _, err := r.store.GetRepository(ctx, defaultRepoID); err != nil {
		now := time.Now().UTC()
		return r.store.CreateRepository(ctx, &model.Repository{
			ID:        defaultRepoID,
			Name:      "Default Repository",
			Owner:     "registry",
			CreatedAt: now,
			UpdatedAt: now,
		})
	}
	return nil
}

func SourceKey(sourceID string) string {
	return fmt.Sprintf("sources/%s.json", sourceID)
}

func DecompKey(sourceID, modelID string) string {
	return fmt.Sprintf("sources/%s/decompositions/%s.json", sourceID, sanitizeModelID(modelID))
}

func BodyKey(sourceID string) string {
	return fmt.Sprintf("sources/%s/body.pdf", sourceID)
}

func ImageKey(sourceID, imageID string) string {
	return fmt.Sprintf("sources/%s/images/%s", sourceID, imageID)
}

func sanitizeModelID(mid string) string {
	b := []byte(mid)
	for i, c := range b {
		if c == '/' || c == ':' || c == '\\' || c == ' ' {
			b[i] = '_'
		}
	}
	return string(b)
}

func (r *Registry) SearchSource(ctx context.Context, q model.SearchQuery) (*model.SearchResult, error) {
	var sources []model.SourceMeta
	var err error

	switch {
	case q.SHA256 != "":
		sources, err = r.store.SearchBySHA256(ctx, defaultRepoID, q.SHA256)
	case q.DOI != "":
		sources, err = r.store.SearchByDOI(ctx, defaultRepoID, q.DOI)
	case q.URL != "":
		sources, err = r.store.SearchByURL(ctx, defaultRepoID, q.URL)
	default:
		return &model.SearchResult{Found: false}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("searching: %w", err)
	}
	if len(sources) == 0 {
		return &model.SearchResult{Found: false}, nil
	}

	s := sources[0]
	presigned, err := r.presignedForSource(ctx, s.ID)
	if err != nil {
		return nil, err
	}

	decomps, err := r.store.ListDecompositions(ctx, s.ID)
	if err != nil {
		return nil, err
	}

	result := &model.SearchResult{
		Found:    true,
		SourceID: s.ID,
		S3Key:    s.S3Key,
		Presigned: presigned,
	}
	for _, d := range decomps {
		ref := model.DecompRef{
			ModelID:        d.ModelID,
			FactCount:      d.FactCount,
			HasEmbeddings:  d.HasEmbeddings,
			EmbeddingModel: d.EmbeddingModel,
		}
		if d.S3Key != "" {
			if pu, err := r.storage.PresignedURL(ctx, d.S3Key, r.presignTTL); err == nil {
				ref.PresignedURL = pu
			} else if !errors.Is(err, storage.ErrPresignDisabled) {
				log.Printf("registry: presigning decomposition %s/%s: %v", s.ID, d.ModelID, err)
			}
		}
		result.Decomps = append(result.Decomps, ref)
	}
	return result, nil
}

func (r *Registry) presignedForSource(ctx context.Context, sourceID string) (model.PresignedURLs, error) {
	srcKey := SourceKey(sourceID)
	srcURL, err := r.storage.PresignedURL(ctx, srcKey, r.presignTTL)
	if err != nil {
		if errors.Is(err, storage.ErrPresignDisabled) {
			return model.PresignedURLs{}, nil
		}
		return model.PresignedURLs{}, fmt.Errorf("presigning source: %w", err)
	}
	p := model.PresignedURLs{Source: srcURL}

	bodyKey := BodyKey(sourceID)
	if pu, err := r.storage.PresignedURL(ctx, bodyKey, r.presignTTL); err == nil {
		p.Body = pu
	}

	return p, nil
}

func (r *Registry) PushSource(ctx context.Context, data *model.SourceData) (*model.PushResult, error) {
	sourceID := data.ID

	// Dedup: when the client doesn't supply an explicit ID, search
	// for an existing source by URL → DOI → SHA256. Reusing the
	// existing ID means the sources table stays one-row-per-source
	// and decompositions/fact_hashes link to the right row.
	if sourceID == "" {
		existing, err := r.findExistingSource(ctx, data)
		if err != nil {
			return nil, fmt.Errorf("searching for existing source: %w", err)
		}
		if existing != nil {
			sourceID = existing.ID
		} else {
			sourceID = uuid.New().String()
		}
	}

	// Write the resolved ID back into the source data so the S3
	// object is self-consistent (the client may have sent an empty
	// ID, in which case we generated or dedup'd one above).
	data.ID = sourceID

	s3Key := SourceKey(sourceID)
	pkg := &model.SourcePackage{
		SchemaVersion: schemaVersion,
		Source:        *data,
		Content:       data.Content,
	}

	body, err := json.Marshal(pkg)
	if err != nil {
		return nil, fmt.Errorf("marshaling source: %w", err)
	}
	// Fire-and-forget: the S3 object is an overwrite-safe blob (the
	// next push writes the same key). A failure here logs and the
	// object is stale until the next push; the metadata DB is
	// already consistent so a search will find the source. A pull
	// in the ~50–200ms window before S3 catches up gets a 404,
	// which the backend's pull path already handles gracefully.
	go func() {
		if err := r.storage.StoreJSON(context.Background(), s3Key, body); err != nil {
			log.Printf("registry: async StoreJSON for source %s: %v", sourceID, err)
		}
	}()

	now := time.Now().UTC()
	meta := &model.SourceMeta{
		ID:        sourceID,
		RepoID:    defaultRepoID,
		URL:       data.URL,
		DOI:       data.DOI,
		SHA256:    data.SHA256,
		Title:     data.Title,
		S3Key:     s3Key,
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := r.store.IndexSource(ctx, meta); err != nil {
		return nil, fmt.Errorf("indexing source: %w", err)
	}

	return &model.PushResult{SourceID: sourceID}, nil
}

// findExistingSource searches the store for a source matching the
// given data by URL, then DOI, then SHA256. Returns nil when no
// match is found. The first non-empty identifier wins; subsequent
// identifiers are not checked once a match is found.
func (r *Registry) findExistingSource(ctx context.Context, data *model.SourceData) (*model.SourceMeta, error) {
	if data.URL != "" {
		sources, err := r.store.SearchByURL(ctx, defaultRepoID, data.URL)
		if err != nil {
			return nil, fmt.Errorf("searching by url: %w", err)
		}
		if len(sources) > 0 {
			return &sources[0], nil
		}
	}
	if data.DOI != "" {
		sources, err := r.store.SearchByDOI(ctx, defaultRepoID, data.DOI)
		if err != nil {
			return nil, fmt.Errorf("searching by doi: %w", err)
		}
		if len(sources) > 0 {
			return &sources[0], nil
		}
	}
	if data.SHA256 != "" {
		sources, err := r.store.SearchBySHA256(ctx, defaultRepoID, data.SHA256)
		if err != nil {
			return nil, fmt.Errorf("searching by sha256: %w", err)
		}
		if len(sources) > 0 {
			return &sources[0], nil
		}
	}
	return nil, nil
}

func (r *Registry) PushDecomposition(ctx context.Context, sourceID string, decomp *model.DecompositionPackage) (*model.PushResult, error) {
	now := time.Now().UTC()
	originalModelID := decomp.ModelID
	sanitizedModelID := sanitizeModelID(originalModelID)
	s3Key := DecompKey(sourceID, sanitizedModelID)

	// Dedup: reuse the existing decomposition row for this
	// (sourceID, modelID) pair instead of creating a new one on
	// every push. The S3 object key is already deterministic, so
	// the JSON is overwritten either way; the dedup here keeps the
	// decompositions metadata table one-row-per-(source, model).
	// We dedup on the SANITIZED model id so a push of
	// "google/gemma-4-31b-it" and a later push of the same
	// (after sanitization) reuses the same row. But we store the
	// ORIGINAL model id in the metadata DB so downstream clients
	// can match it against their config without needing to know
	// the sanitization rules.
	decompID := uuid.New().String()
	if existing, err := r.store.GetDecompositionBySourceAndModel(ctx, sourceID, originalModelID); err == nil && existing != nil {
		decompID = existing.ID
	}

	// Batch fact-hash upsert: replaces the per-fact
	// FactHashExists + InsertFactHash/LinkFactHash loop (2N
	// auto-committed queries) with one transaction (one SELECT
	// IN + N INSERTs, one fsync). For 200 facts this cuts ~400
	// queries down to 2.
	entries := make([]model.FactHashEntry, 0, len(decomp.Facts))
	for i := range decomp.Facts {
		f := &decomp.Facts[i]
		if f.ContentHash == "" {
			h := sha256.Sum256([]byte(f.Content))
			f.ContentHash = hex.EncodeToString(h[:])
		}
		entries = append(entries, model.FactHashEntry{
			ContentHash: f.ContentHash,
			FactID:      f.ID,
		})
	}
	batchResult, err := r.store.BatchUpsertFactHashes(ctx, sourceID, decompID, entries)
	if err != nil {
		return nil, fmt.Errorf("batch upserting fact hashes: %w", err)
	}

	body, err := json.Marshal(decomp)
	if err != nil {
		return nil, fmt.Errorf("marshaling decomposition: %w", err)
	}
	// Fire-and-forget: the S3 object is an overwrite-safe blob (the
	// next push writes the same key). A failure here logs and the
	// object is stale until the next push; the metadata DB is
	// already consistent so a pull will find the decomposition
	// metadata. A pull in the ~50–200ms window before S3 catches up
	// gets a 404, which the backend's pull path handles gracefully
	// (it logs and skips to the next model).
	go func() {
		if err := r.storage.StoreJSON(context.Background(), s3Key, body); err != nil {
			log.Printf("registry: async StoreJSON for decomposition %s/%s: %v", sourceID, sanitizedModelID, err)
		}
	}()

	var embModel string
	var embDims int
	if decomp.Embeddings != nil {
		embModel = decomp.Embeddings.Model
		embDims = decomp.Embeddings.Dimensions
	}

	meta := &model.DecompMeta{
		ID:              decompID,
		SourceID:        sourceID,
		ModelID:         originalModelID,
		DecomposedBy:    decomp.DecomposedBy,
		DecomposedAt:    decomp.DecomposedAt,
		FactCount:       len(decomp.Facts),
		SummaryCount:    len(decomp.Summaries),
		HasEmbeddings:   decomp.Embeddings != nil,
		EmbeddingModel:  embModel,
		EmbeddingDims:   embDims,
		S3Key:           s3Key,
		CreatedAt:       now,
	}
	if err := r.store.IndexDecomposition(ctx, meta); err != nil {
		return nil, fmt.Errorf("indexing decomposition: %w", err)
	}

	return &model.PushResult{
		SourceID:    sourceID,
		FactsNew:    batchResult.New,
		FactsLinked: batchResult.Linked,
	}, nil
}

func (r *Registry) PullSource(ctx context.Context, sourceID string) (*model.SourcePackage, error) {
	s3Key := SourceKey(sourceID)
	data, _, err := r.storage.ReadAll(ctx, s3Key)
	if err != nil {
		return nil, fmt.Errorf("reading source %s: %w", sourceID, err)
	}
	var pkg model.SourcePackage
	if err := json.Unmarshal(data, &pkg); err != nil {
		return nil, fmt.Errorf("unmarshaling source: %w", err)
	}
	// Ensure the source ID is populated even if the S3 object was
	// written by an older version of PushSource that didn't write
	// the resolved ID back into the source data.
	if pkg.Source.ID == "" {
		pkg.Source.ID = sourceID
	}
	// Embed the decompositions list from the metadata DB. The S3
	// object is written at source-push time (before any
	// decompositions exist), so it always has an empty
	// decompositions array. The authoritative list lives in the
	// metadata DB (populated by PushDecomposition →
	// IndexDecomposition). Each decomposition gets a presigned S3
	// URL so clients can fetch the full decomposition package
	// directly from object storage without proxying through the
	// registry service.
	decomps, err := r.store.ListDecompositions(ctx, sourceID)
	if err != nil {
		log.Printf("registry: listing decompositions for source %s: %v", sourceID, err)
	} else {
		pkg.Decompositions = make([]model.DecompRef, 0, len(decomps))
		for _, d := range decomps {
			ref := model.DecompRef{
				ModelID:        d.ModelID,
				FactCount:      d.FactCount,
				HasEmbeddings:  d.HasEmbeddings,
				EmbeddingModel: d.EmbeddingModel,
			}
			if d.S3Key != "" {
				if pu, err := r.storage.PresignedURL(ctx, d.S3Key, r.presignTTL); err == nil {
					ref.PresignedURL = pu
				} else if !errors.Is(err, storage.ErrPresignDisabled) {
					log.Printf("registry: presigning decomposition %s/%s: %v", sourceID, d.ModelID, err)
				}
			}
			pkg.Decompositions = append(pkg.Decompositions, ref)
		}
	}
	return &pkg, nil
}

func (r *Registry) PullDecomposition(ctx context.Context, sourceID, modelID string) (*model.DecompositionPackage, error) {
	s3Key := DecompKey(sourceID, sanitizeModelID(modelID))
	data, _, err := r.storage.ReadAll(ctx, s3Key)
	if err != nil {
		return nil, fmt.Errorf("reading decomposition %s/%s: %w", sourceID, modelID, err)
	}
	var pkg model.DecompositionPackage
	if err := json.Unmarshal(data, &pkg); err != nil {
		return nil, fmt.Errorf("unmarshaling decomposition: %w", err)
	}
	return &pkg, nil
}

func (r *Registry) ListSources(ctx context.Context, limit, offset int) ([]model.SourceMeta, int, error) {
	sources, err := r.store.ListAllSources(ctx, limit, offset)
	if err != nil {
		return nil, 0, err
	}
	total, err := r.store.CountAllSources(ctx)
	if err != nil {
		return nil, 0, err
	}
	return sources, total, nil
}

func (r *Registry) SearchSourcesText(ctx context.Context, query string, limit, offset int) ([]model.SourceMeta, int, error) {
	sources, err := r.store.SearchByText(ctx, defaultRepoID, query, limit, offset)
	if err != nil {
		return nil, 0, err
	}
	total, err := r.store.CountByText(ctx, defaultRepoID, query)
	if err != nil {
		return nil, 0, err
	}
	return sources, total, nil
}

func (r *Registry) ListDecompositions(ctx context.Context, sourceID string) ([]model.DecompMeta, error) {
	return r.store.ListDecompositions(ctx, sourceID)
}

func (r *Registry) Stats(ctx context.Context) (repoCount, sourceCount int, err error) {
	return r.store.Stats(ctx)
}

// SeedContexts populates the contexts table from the embedded
// contexts.json snapshot. Idempotent: ON CONFLICT DO UPDATE refreshes
// the description + updated_at for already-stored labels and inserts
// new ones. Called once at boot so the registry publishes a fresh
// canonical vocabulary every time it starts (seed-on-boot,
// mutable-via-file-change-and-restart).
func (r *Registry) SeedContexts(ctx context.Context) (int, error) {
	classes, err := model.LoadContextClasses()
	if err != nil {
		return 0, fmt.Errorf("loading embedded contexts: %w", err)
	}
	for _, c := range classes {
		if err := r.store.UpsertContext(ctx, c.Label, c.Description); err != nil {
			return 0, fmt.Errorf("upserting context %q: %w", c.Label, err)
		}
	}
	return len(classes), nil
}

// ListContexts returns the canonical context vocabulary from the
// store (the seeded table). The handler exposes this as
// GET /api/v1/contexts.
func (r *Registry) ListContexts(ctx context.Context) ([]model.ContextClass, error) {
	return r.store.ListContexts(ctx)
}

func (r *Registry) PresignedUploadURL(ctx context.Context, sourceID, assetType, assetID string) (string, error) {
	var key string
	switch assetType {
	case "body":
		key = BodyKey(sourceID)
	case "image":
		key = ImageKey(sourceID, assetID)
	default:
		return "", fmt.Errorf("unknown asset type: %s", assetType)
	}
	return r.storage.PresignedPUTURL(ctx, key, r.presignTTL)
}

func (r *Registry) PresignedDownloadURL(ctx context.Context, sourceID, assetType, assetID string) (string, error) {
	var key string
	switch assetType {
	case "body":
		key = BodyKey(sourceID)
	case "image":
		if assetID == "" {
			return "", fmt.Errorf("image id is required")
		}
		key = ImageKey(sourceID, assetID)
	default:
		return "", fmt.Errorf("unknown asset type: %s", assetType)
	}
	return r.storage.PresignedURL(ctx, key, r.presignTTL)
}
