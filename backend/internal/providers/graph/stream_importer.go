package graph

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/openktree/open-knowledge-tree/backend/internal/providers/storage"
	"github.com/openktree/open-knowledge-tree/backend/internal/qdrantstore"
	"github.com/openktree/open-knowledge-tree/backend/internal/store"
)

const streamImportBatchSize = 1000

// imgEntry / bodyEntry are temp entries buffered during the images/
// bodies map sections and consumed during source_images/source_bodies.
type imgEntry struct {
	key         string
	storageKey  string
	contentType string
}
type bodyEntry struct {
	storageKey  string
	contentType string
}

// vectorUpsert is the subset of *qdrantstore.Store the streaming
// importer uses to upsert fact/concept vectors. Defined as an interface
// so unit tests can substitute a fake without standing up Qdrant;
// *qdrantstore.Store satisfies it structurally.
type vectorUpsert interface {
	UpsertFactVectors(ctx context.Context, points []qdrantstore.FactPoint) error
	UpsertConceptVectors(ctx context.Context, points []qdrantstore.ConceptPoint) error
}

// StreamImporter applies a gzipped v2 graph bundle to a repository by
// streaming one entity at a time from the JSON, inserting in batches,
// and discarding each row after insert. Unlike BundleImporter (which
// decodes the entire bundle into a GraphBundle struct in RAM — OOM for
// 8+ GB bundles), StreamImporter holds only:
//   - Dense idx→UUID slices (one []pgtype.UUID per entity type; the
//     largest, factUUIDs, is ~160 MB for 10M facts).
//   - One batch buffer per section (~1000 rows).
//   - One Qdrant batch (~1000 vectors, ~12 MB).
//   - One image/body in flight (written to storage immediately).
//
// Peak memory for a 100 GB bundle: ~180 MB (vs ~400+ GB for the struct
// path). The v2 field order (images/bodies/source_images/source_bodies
// before facts) eliminates all forward references, so no deferred
// fixups are needed.
type StreamImporter struct {
	queries        *store.Queries
	qdrant         vectorUpsert
	storage        storage.FileStorage
	repoID         pgtype.UUID
	repoUUID       uuid.UUID
	repoSlug       string
	embeddingModel string

	// Dense idx→UUID slices. Indexed by the bundle's internal idx
	// (0, 1, 2, …). Grown via append as each section streams.
	sourceUUIDs        []pgtype.UUID
	sourceImageUUIDs   []pgtype.UUID
	sourceImageSources []pgtype.UUID // image idx → source UUID (for image_url remap)
	factUUIDs          []pgtype.UUID
	conceptUUIDs       []pgtype.UUID
	investigationUUIDs []pgtype.UUID
	reportUUIDs        []pgtype.UUID

	// Bundle metadata (read from the metadata section, small).
	embeddingsModel      string
	embeddingsDimensions int

	// Pending image/body entries (buffered during images/bodies maps,
	// resolved during source_images/source_bodies). The actual bytes
	// are written to temp storage keys during the map phase; the
	// source_images/source_bodies phase moves them to their final keys.
	pendingImages map[string]imgEntry
	pendingBodies map[string]bodyEntry
}

// NewStreamImporter constructs a StreamImporter. The arguments mirror
// NewBundleImporter so the caller (import_graph worker) can build
// either one from the same deps. qdrant may be a *qdrantstore.Store
// (the production implementation) or nil; nil is handled by the caller
// (streamEmbeddings returns needsReembed=true and skips vectors).
func NewStreamImporter(
	queries *store.Queries,
	qdrant *qdrantstore.Store,
	storageBackend storage.FileStorage,
	repoID pgtype.UUID,
	repoSlug string,
	embeddingModel string,
) *StreamImporter {
	return &StreamImporter{
		queries:        queries,
		qdrant:         qdrant,
		storage:        storageBackend,
		repoID:         repoID,
		repoUUID:       asUUID(repoID),
		repoSlug:       repoSlug,
		embeddingModel: embeddingModel,
	}
}

// StreamImport reads a gzipped v2 graph bundle from r (e.g. a temp
// file from FetchGraphPresignedToStream), processes each JSON section
// in field order, inserts entities into the repository, and returns
// the import result. The reader is consumed in a single pass; no
// section is buffered in full (except the small metadata struct).
func (s *StreamImporter) StreamImport(ctx context.Context, r io.Reader, mode ImportMode) (*ImportResult, error) {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return nil, fmt.Errorf("stream import: opening gzip: %w", err)
	}
	defer gz.Close()

	dec := json.NewDecoder(gz)

	// Read the opening '{'.
	t, err := dec.Token()
	if err != nil {
		return nil, fmt.Errorf("stream import: reading open token: %w", err)
	}
	if d, ok := t.(json.Delim); !ok || d != '{' {
		return nil, fmt.Errorf("stream import: expected object, got %v", t)
	}

	result := &ImportResult{}

	// Walk top-level keys in v2 field order.
	for dec.More() {
		t, err = dec.Token()
		if err != nil {
			return nil, fmt.Errorf("stream import: reading key: %w", err)
		}
		key, ok := t.(string)
		if !ok {
			return nil, fmt.Errorf("stream import: non-string key %v", t)
		}

		switch key {
		case "schema_version":
			var sv int
			if err := dec.Decode(&sv); err != nil {
				return nil, fmt.Errorf("stream import: decoding schema_version: %w", err)
			}
			if sv > SchemaVersion {
				return nil, fmt.Errorf("stream import: bundle schema_version %d is newer than this build supports (%d); upgrade OKT", sv, SchemaVersion)
			}
			if sv < 2 {
				return nil, fmt.Errorf("stream import: bundle schema_version %d is v1; this build only streams v2 bundles. Re-export the graph to produce a v2 bundle", sv)
			}

		case "metadata":
			var meta BundleMetadata
			if err := dec.Decode(&meta); err != nil {
				return nil, fmt.Errorf("stream import: decoding metadata: %w", err)
			}
			s.embeddingsModel = meta.EmbeddingModel
			s.embeddingsDimensions = meta.EmbeddingDimensions

		case "sources":
			n, err := s.streamSources(ctx, dec, mode)
			if err != nil {
				return nil, err
			}
			result.ImportedSources = n

		case "images":
			if err := s.streamImagesMap(ctx, dec); err != nil {
				return nil, err
			}

		case "bodies":
			if err := s.streamBodiesMap(ctx, dec); err != nil {
				return nil, err
			}

		case "source_images":
			if err := s.streamSourceImages(ctx, dec); err != nil {
				return nil, err
			}

		case "source_bodies":
			if err := s.streamSourceBodies(ctx, dec); err != nil {
				return nil, err
			}

		case "facts":
			n, err := s.streamFacts(ctx, dec, mode)
			if err != nil {
				return nil, err
			}
			result.ImportedFacts = n

		case "fact_sources":
			if err := s.streamFactSources(ctx, dec); err != nil {
				return nil, err
			}

		case "concepts":
			n, err := s.streamConcepts(ctx, dec)
			if err != nil {
				return nil, err
			}
			result.ImportedConcepts = n

		case "concept_aliases":
			if err := s.streamConceptAliases(ctx, dec); err != nil {
				return nil, err
			}

		case "fact_concepts":
			if err := s.streamFactConcepts(ctx, dec); err != nil {
				return nil, err
			}

		case "concept_summaries":
			n, err := s.streamSummaries(ctx, dec)
			if err != nil {
				return nil, err
			}
			result.ImportedSummaries = n

		case "concept_syntheses":
			n, err := s.streamSyntheses(ctx, dec)
			if err != nil {
				return nil, err
			}
			result.ImportedSyntheses = n

		case "investigations":
			n, err := s.streamInvestigations(ctx, dec)
			if err != nil {
				return nil, err
			}
			result.ImportedInvestigations = n

		case "investigation_sources":
			if err := s.streamInvestigationSources(ctx, dec); err != nil {
				return nil, err
			}

		case "reports":
			n, err := s.streamReports(ctx, dec)
			if err != nil {
				return nil, err
			}
			result.ImportedReports = n

		case "report_annotations":
			if err := s.streamReportAnnotations(ctx, dec); err != nil {
				return nil, err
			}

		case "embeddings":
			needsReembed, err := s.streamEmbeddings(ctx, dec)
			if err != nil {
				return nil, err
			}
			result.NeedsReembed = needsReembed

		default:
			// Unknown key: skip its value.
			var raw json.RawMessage
			if err := dec.Decode(&raw); err != nil {
				return nil, fmt.Errorf("stream import: skipping unknown key %q: %w", key, err)
			}
		}
	}

	return result, nil
}

// ── per-section handlers ──────────────────────────────────────────

// streamSources reads the sources array element-by-element, inserts
// each source, and populates sourceUUIDs[idx]. Row-by-row because
// CreateSource + UpdateSourcePublishedAt + MarkSourceParsed are
// per-row queries (sources are low-volume: thousands, not millions).
func (s *StreamImporter) streamSources(ctx context.Context, dec *json.Decoder, mode ImportMode) (int, error) {
	count := 0
	err := decodeArray(dec, func(idx int) error {
		var row SourceRow
		if err := dec.Decode(&row); err != nil {
			return fmt.Errorf("decoding source idx %d: %w", idx, err)
		}
		row.Idx = idx

		var doi *string
		if row.DOI != "" {
			d := row.DOI
			doi = &d
		}
		var publishedAt pgtype.Date
		if row.PublishedAt != "" {
			if t, err := time.Parse("2006-01-02", row.PublishedAt); err == nil {
				publishedAt = pgtype.Date{Valid: true, Time: t}
			}
		}

		if mode == ImportModeExisting {
			existing, err := s.queries.GetSourceByRepoAndURL(ctx, store.GetSourceByRepoAndURLParams{
				RepositoryID: s.repoID,
				Url:          row.URL,
			})
			if err == nil {
				s.growSourceUUIDs(idx, existing.ID)
				return nil
			}
		}

		srcID := pgtype.UUID{}
		if err := srcID.Scan(uuid.New().String()); err != nil {
			return fmt.Errorf("scanning source id: %w", err)
		}
		_, err := s.queries.CreateSource(ctx, store.CreateSourceParams{
			ID:           srcID,
			RepositoryID: s.repoID,
			Url:          row.URL,
			Kind:         row.Kind,
			Status:       row.Status,
			Doi:          doi,
		})
		if err != nil {
			existing, lookupErr := s.queries.GetSourceByRepoAndURL(ctx, store.GetSourceByRepoAndURLParams{
				RepositoryID: s.repoID,
				Url:          row.URL,
			})
			if lookupErr != nil {
				log.Printf("stream import: creating source idx %d (%s): %v", idx, row.URL, err)
				s.growSourceUUIDs(idx, pgtype.UUID{})
				return nil
			}
			srcID = existing.ID
		}
		s.growSourceUUIDs(idx, srcID)
		count++

		if publishedAt.Valid {
			if _, err := s.queries.UpdateSourcePublishedAt(ctx, store.UpdateSourcePublishedAtParams{
				ID:          srcID,
				PublishedAt: publishedAt,
			}); err != nil {
				log.Printf("stream import: setting source published_at idx %d: %v", idx, err)
			}
		}
		if row.ParsedText != "" || row.ParsedMarkdown != "" || row.ParsedTitle != "" {
			var title, text, md *string
			if row.ParsedTitle != "" {
				t := row.ParsedTitle
				title = &t
			}
			if row.ParsedText != "" {
				t := row.ParsedText
				text = &t
			}
			if row.ParsedMarkdown != "" {
				m := row.ParsedMarkdown
				md = &m
			}
			status := "ok"
			if _, err := s.queries.MarkSourceParsed(ctx, store.MarkSourceParsedParams{
				ID:             srcID,
				ParsedTitle:    title,
				ParsedText:     text,
				ParsedMarkdown: md,
				ParseStatus:    &status,
			}); err != nil {
				log.Printf("stream import: marking source parsed idx %d: %v", idx, err)
			}
		}
		return nil
	})
	return count, err
}

// streamImagesMap reads the images map (idx→FileBytes) key-by-key,
// writes each image to storage, and records the storage_key for later
// use by streamSourceImages. One image in memory at a time.
func (s *StreamImporter) streamImagesMap(ctx context.Context, dec *json.Decoder) error {
	if s.storage == nil {
		return skipValue(dec)
	}
	// We need the storage keys when processing source_images. Store
	// them in a map keyed by image ref string.
	s.pendingImages = make(map[string]imgEntry)

	return decodeObject(dec, func(key string) error {
		var fb FileBytes
		if err := dec.Decode(&fb); err != nil {
			return fmt.Errorf("decoding image %q: %w", key, err)
		}
		// We don't know the source/image UUIDs yet (source_images
		// comes next). Store the bytes temporarily — we'll write to
		// storage during source_images processing when we know the
		// full key path. For now, just buffer the ref→bytes mapping.
		// To avoid buffering ALL images in memory, we write to a
		// temp storage key now and rename later. But since images
		// are typically << the bundle size (they're already
		// compressed), buffering the map of (ref → storageKey) is
		// fine. The actual bytes are written to storage immediately.
		tmpKey := fmt.Sprintf("tmp/import-%s/images/%s", s.repoUUID, key)
		if _, err := s.storage.Store(ctx, tmpKey, fb.ContentType, fb.Data); err != nil {
			log.Printf("stream import: storing image %q: %v", key, err)
			return nil
		}
		s.pendingImages[key] = imgEntry{key: key, storageKey: tmpKey, contentType: fb.ContentType}
		return nil
	})
}

// streamBodiesMap reads the bodies map (bodyRef→FileBytes) key-by-key,
// writes each body to a temp storage key, and records for later use
// by streamSourceBodies. One body in memory at a time.
func (s *StreamImporter) streamBodiesMap(ctx context.Context, dec *json.Decoder) error {
	if s.storage == nil {
		return skipValue(dec)
	}
	s.pendingBodies = make(map[string]bodyEntry)

	return decodeObject(dec, func(key string) error {
		var fb FileBytes
		if err := dec.Decode(&fb); err != nil {
			return fmt.Errorf("decoding body %q: %w", key, err)
		}
		tmpKey := fmt.Sprintf("tmp/import-%s/bodies/%s", s.repoUUID, key)
		if _, err := s.storage.Store(ctx, tmpKey, fb.ContentType, fb.Data); err != nil {
			log.Printf("stream import: storing body %q: %v", key, err)
			return nil
		}
		s.pendingBodies[key] = bodyEntry{storageKey: tmpKey, contentType: fb.ContentType}
		return nil
	})
}

// streamSourceImages reads the source_images array, inserts each row
// (resolving sourceImageRef → the temp storage key written during
// streamImagesMap), and populates sourceImageUUIDs + sourceImageSources.
func (s *StreamImporter) streamSourceImages(ctx context.Context, dec *json.Decoder) error {
	// Batch accumulators.
	var ids, srcIDs []pgtype.UUID
	var kinds, pageNums, urls, altTexts, storageKeys, contentTypes []string
	var positions, widths, heights, bytesVals []int32
	flush := func() error {
		if len(ids) == 0 {
			return nil
		}
		_, err := s.queries.BatchCreateSourceImages(ctx, store.BatchCreateSourceImagesParams{
			Column1: ids, Column2: srcIDs, Column3: kinds, Column4: pageNums,
			Column5: positions, Column6: urls, Column7: widths, Column8: heights,
			Column9: bytesVals, Column10: altTexts, Column11: storageKeys, Column12: contentTypes,
		})
		if err != nil {
			log.Printf("stream import: batch creating source_images: %v", err)
		}
		ids, srcIDs = nil, nil
		kinds, pageNums, urls, altTexts, storageKeys, contentTypes = nil, nil, nil, nil, nil, nil
		positions, widths, heights, bytesVals = nil, nil, nil, nil
		return nil
	}

	err := decodeArray(dec, func(idx int) error {
		var row SourceImageRow
		if err := dec.Decode(&row); err != nil {
			return fmt.Errorf("decoding source_image idx %d: %w", idx, err)
		}
		row.Idx = idx

		srcID, ok := s.getSourceUUID(row.SourceIdx)
		if !ok {
			return nil
		}
		imgID := pgtype.UUID{}
		_ = imgID.Scan(uuid.New().String())

		// Grow the UUID slices.
		s.growSourceImageUUIDs(idx, imgID, srcID)

		// Resolve image ref → storage key (if the image was embedded).
		var storageKey, contentType string
		if row.ImageRef != "" && s.pendingImages != nil {
			if entry, ok := s.pendingImages[row.ImageRef]; ok {
				// Move from temp key to the real key path.
				realKey := fmt.Sprintf("repositories/%s/sources/%s/images/%s",
					s.repoUUID, asUUID(srcID), asUUID(imgID))
				// Read from temp, write to real. (Storage backends
				// don't support rename; we re-store. For S3/Minio
				// this is a copy. For filesystem it's a read+write.
				// The temp key is cleaned up later.)
				f, err := s.storage.Get(ctx, entry.storageKey)
				if err == nil {
					data, _ := io.ReadAll(f.Body)
					_ = f.Body.Close()
					ct := entry.contentType
					if _, err := s.storage.Store(ctx, realKey, ct, data); err == nil {
						storageKey = realKey
						contentType = ct
					}
				}
				// Best-effort cleanup of the temp key.
				_ = s.storage.Delete(ctx, entry.storageKey)
				delete(s.pendingImages, row.ImageRef)
			}
		}

		ids = append(ids, imgID)
		srcIDs = append(srcIDs, srcID)
		kinds = append(kinds, row.Kind)
		if row.PageNumber > 0 {
			pageNums = append(pageNums, strconv.Itoa(row.PageNumber))
		} else {
			pageNums = append(pageNums, "")
		}
		positions = append(positions, int32(row.Position))
		urls = append(urls, row.URL)
		widths = append(widths, int32(row.Width))
		heights = append(heights, int32(row.Height))
		bytesVals = append(bytesVals, int32(row.Bytes))
		altTexts = append(altTexts, row.AltText)
		storageKeys = append(storageKeys, storageKey)
		contentTypes = append(contentTypes, contentType)

		if len(ids) >= streamImportBatchSize {
			return flush()
		}
		return nil
	})
	if err != nil {
		return err
	}
	return flush()
}

// streamSourceBodies reads the source_bodies array, resolves each
// bodyRef to the temp storage key written during streamBodiesMap,
// writes the body to the real storage path, and marks the source.
func (s *StreamImporter) streamSourceBodies(ctx context.Context, dec *json.Decoder) error {
	if s.storage == nil {
		return skipValue(dec)
	}
	return decodeArray(dec, func(_ int) error {
		var sb SourceBodyRef
		if err := dec.Decode(&sb); err != nil {
			return fmt.Errorf("decoding source_body: %w", err)
		}
		if sb.BodyRef == "" {
			return nil
		}
		srcID, ok := s.getSourceUUID(sb.SourceIdx)
		if !ok {
			return nil
		}
		entry, ok := s.pendingBodies[sb.BodyRef]
		if !ok {
			return nil
		}
		// Read from temp, write to real path.
		key := fmt.Sprintf("repositories/%s/sources/%s/body.pdf", s.repoUUID, asUUID(srcID))
		f, err := s.storage.Get(ctx, entry.storageKey)
		if err != nil {
			log.Printf("stream import: reading body temp %s: %v", entry.storageKey, err)
			return nil
		}
		data, _ := io.ReadAll(f.Body)
		_ = f.Body.Close()
		ct := entry.contentType
		if _, err := s.storage.Store(ctx, key, ct, data); err != nil {
			log.Printf("stream import: storing body for source idx %d: %v", sb.SourceIdx, err)
			return nil
		}
		lp := key
		if _, err := s.queries.MarkSourceBodyStored(ctx, store.MarkSourceBodyStoredParams{
			ID:          srcID,
			StorageKey:  &key,
			ContentType: &ct,
			LocalPath:   &lp,
		}); err != nil {
			log.Printf("stream import: marking source body stored idx %d: %v", sb.SourceIdx, err)
		}
		_ = s.storage.Delete(ctx, entry.storageKey)
		delete(s.pendingBodies, sb.BodyRef)
		return nil
	})
}

// streamFacts reads the facts array in batches, inserts via
// BatchCreateFacts, and populates factUUIDs[idx]. Image facts get
// their image_url remapped using sourceImageUUIDs + sourceImageSources
// (both already built from the source_images section that precedes
// facts in v2 order).
func (s *StreamImporter) streamFacts(ctx context.Context, dec *json.Decoder, mode ImportMode) (int, error) {
	count := 0
	var ids []pgtype.UUID
	var texts, factKinds, imageURLs, statuses, promptsetHashes []string
	flush := func() error {
		if len(ids) == 0 {
			return nil
		}
		_, err := s.queries.BatchCreateFacts(ctx, store.BatchCreateFactsParams{
			Column1: ids, Column2: texts, Column3: factKinds,
			Column4: imageURLs, Column5: statuses, Column6: promptsetHashes,
		})
		if err != nil {
			log.Printf("stream import: batch creating facts: %v", err)
		}
		ids, texts, factKinds, imageURLs, statuses, promptsetHashes = nil, nil, nil, nil, nil, nil
		return nil
	}

	err := decodeArray(dec, func(idx int) error {
		var row FactRow
		if err := dec.Decode(&row); err != nil {
			return fmt.Errorf("decoding fact idx %d: %w", idx, err)
		}
		row.Idx = idx

		id := pgtype.UUID{}
		_ = id.Scan(uuid.New().String())

		// Remap image_url for storage-backed image facts (v2: sourceImageUUIDs is already built).
		imageURL := row.ImageURL
		if row.FactKind == "image" && row.SourceImageIdx >= 0 {
			if newImgID, ok := s.getSourceImageUUID(row.SourceImageIdx); ok {
				if newSrcID, ok := s.getSourceImageSource(row.SourceImageIdx); ok {
					imageURL = fmt.Sprintf("/api/v1/repositories/%s/sources/%s/images/%s",
						s.repoSlug, asUUID(newSrcID), asUUID(newImgID))
				}
			}
		}

		// Grow factUUIDs.
		s.growFactUUIDs(idx, id)

		ids = append(ids, id)
		texts = append(texts, row.Text)
		factKinds = append(factKinds, row.FactKind)
		imageURLs = append(imageURLs, imageURL)
		statuses = append(statuses, row.Status)
		promptsetHashes = append(promptsetHashes, row.PromptsetHash)
		count++

		if len(ids) >= streamImportBatchSize {
			return flush()
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	if err := flush(); err != nil {
		return 0, err
	}
	return count, nil
}

// streamFactSourcesRows reads the fact_sources array in batches.
func (s *StreamImporter) streamFactSources(ctx context.Context, dec *json.Decoder) error {
	var factIDs, sourceIDs []pgtype.UUID
	var chunks []int32
	flush := func() error {
		if len(factIDs) == 0 {
			return nil
		}
		_, err := s.queries.BatchAddFactSources(ctx, store.BatchAddFactSourcesParams{
			Column1: factIDs, Column2: sourceIDs, Column3: chunks,
		})
		if err != nil {
			log.Printf("stream import: batch adding fact_sources: %v", err)
		}
		factIDs, sourceIDs, chunks = nil, nil, nil
		return nil
	}

	err := decodeArray(dec, func(_ int) error {
		var row FactSourceRow
		if err := dec.Decode(&row); err != nil {
			return fmt.Errorf("decoding fact_source: %w", err)
		}
		fID, ok := s.getFactUUID(row.FactIdx)
		if !ok {
			return nil
		}
		sID, ok := s.getSourceUUID(row.SourceIdx)
		if !ok {
			return nil
		}
		factIDs = append(factIDs, fID)
		sourceIDs = append(sourceIDs, sID)
		chunks = append(chunks, int32(row.ChunkIndex))
		if len(factIDs) >= streamImportBatchSize {
			return flush()
		}
		return nil
	})
	if err != nil {
		return err
	}
	return flush()
}

// streamConcepts reads concepts row-by-row (CreateConcept is per-row
// with ON CONFLICT DO NOTHING), populates conceptUUIDs[idx].
func (s *StreamImporter) streamConcepts(ctx context.Context, dec *json.Decoder) (int, error) {
	count := 0
	err := decodeArray(dec, func(idx int) error {
		var row ConceptRow
		if err := dec.Decode(&row); err != nil {
			return fmt.Errorf("decoding concept idx %d: %w", idx, err)
		}
		row.Idx = idx

		var desc *string
		if row.Description != "" {
			d := row.Description
			desc = &d
		}
		var psh *string
		if row.PromptsetHash != "" {
			p := row.PromptsetHash
			psh = &p
		}
		created, err := s.queries.CreateConcept(ctx, store.CreateConceptParams{
			RepositoryID:  s.repoID,
			CanonicalName: row.CanonicalName,
			Context:       row.Context,
			Description:   desc,
			PromptsetHash: psh,
		})
		if err == nil {
			s.growConceptUUIDs(idx, created.ID)
			count++
			return nil
		}
		existing, lookupErr := s.queries.GetConceptByNameContext(ctx, store.GetConceptByNameContextParams{
			RepositoryID:  s.repoID,
			CanonicalName: row.CanonicalName,
			Context:       row.Context,
		})
		if lookupErr != nil {
			log.Printf("stream import: creating concept idx %d (%s/%s): %v", idx, row.CanonicalName, row.Context, err)
			s.growConceptUUIDs(idx, pgtype.UUID{})
			return nil
		}
		s.growConceptUUIDs(idx, existing.ID)
		return nil
	})
	return count, err
}

func (s *StreamImporter) streamConceptAliases(ctx context.Context, dec *json.Decoder) error {
	var conceptIDs []pgtype.UUID
	var aliases []string
	flush := func() error {
		if len(conceptIDs) == 0 {
			return nil
		}
		_, err := s.queries.BatchCreateConceptAliases(ctx, store.BatchCreateConceptAliasesParams{
			Column1: conceptIDs, Column2: aliases,
		})
		if err != nil {
			log.Printf("stream import: batch creating concept_aliases: %v", err)
		}
		conceptIDs, aliases = nil, nil
		return nil
	}
	err := decodeArray(dec, func(_ int) error {
		var row ConceptAliasRow
		if err := dec.Decode(&row); err != nil {
			return fmt.Errorf("decoding concept_alias: %w", err)
		}
		cID, ok := s.getConceptUUID(row.ConceptIdx)
		if !ok {
			return nil
		}
		conceptIDs = append(conceptIDs, cID)
		aliases = append(aliases, row.AliasText)
		if len(conceptIDs) >= streamImportBatchSize {
			return flush()
		}
		return nil
	})
	if err != nil {
		return err
	}
	return flush()
}

func (s *StreamImporter) streamFactConcepts(ctx context.Context, dec *json.Decoder) error {
	var factIDs, conceptIDs []pgtype.UUID
	var pshs []string
	flush := func() error {
		if len(factIDs) == 0 {
			return nil
		}
		_, err := s.queries.BatchAddFactConcepts(ctx, store.BatchAddFactConceptsParams{
			Column1: factIDs, Column2: conceptIDs, Column3: pshs,
		})
		if err != nil {
			log.Printf("stream import: batch adding fact_concepts: %v", err)
		}
		factIDs, conceptIDs, pshs = nil, nil, nil
		return nil
	}
	err := decodeArray(dec, func(_ int) error {
		var row FactConceptRow
		if err := dec.Decode(&row); err != nil {
			return fmt.Errorf("decoding fact_concept: %w", err)
		}
		fID, ok := s.getFactUUID(row.FactIdx)
		if !ok {
			return nil
		}
		cID, ok := s.getConceptUUID(row.ConceptIdx)
		if !ok {
			return nil
		}
		factIDs = append(factIDs, fID)
		conceptIDs = append(conceptIDs, cID)
		pshs = append(pshs, row.PromptsetHash)
		if len(factIDs) >= streamImportBatchSize {
			return flush()
		}
		return nil
	})
	if err != nil {
		return err
	}
	return flush()
}

func (s *StreamImporter) streamSummaries(ctx context.Context, dec *json.Decoder) (int, error) {
	count := 0
	err := decodeArray(dec, func(_ int) error {
		var row SummaryRow
		if err := dec.Decode(&row); err != nil {
			return fmt.Errorf("decoding summary: %w", err)
		}
		cID, ok := s.getConceptUUID(row.ConceptIdx)
		if !ok {
			return nil
		}
		covered := make([]pgtype.UUID, 0, len(row.CoveredFactIdxs))
		for _, fIdx := range row.CoveredFactIdxs {
			if fID, ok := s.getFactUUID(fIdx); ok {
				covered = append(covered, fID)
			}
		}
		var model *string
		if row.Model != "" {
			m := row.Model
			model = &m
		}
		if _, err := s.queries.CreateSummary(ctx, store.CreateSummaryParams{
			ConceptID:      cID,
			RepositoryID:   s.repoID,
			Context:        "",
			SequenceNum:    int32(row.SequenceNum),
			IsComplete:     row.IsComplete,
			FactCount:      int32(row.FactCount),
			Content:        row.Content,
			CoveredFactIds: covered,
			Model:          model,
		}); err != nil {
			log.Printf("stream import: creating summary concept_idx %d seq %d: %v", row.ConceptIdx, row.SequenceNum, err)
			return nil
		}
		count++
		return nil
	})
	return count, err
}

func (s *StreamImporter) streamSyntheses(ctx context.Context, dec *json.Decoder) (int, error) {
	count := 0
	err := decodeArray(dec, func(_ int) error {
		var row SynthesisRow
		if err := dec.Decode(&row); err != nil {
			return fmt.Errorf("decoding synthesis: %w", err)
		}
		coveredCon := s.idxsToUUIDs(row.CoveredConceptIdxs, s.conceptUUIDs)
		coveredImg := s.idxsToUUIDs(row.EmbeddedImageIdxs, s.factUUIDs)
		var model *string
		if row.Model != "" {
			m := row.Model
			model = &m
		}
		if _, err := s.queries.UpsertSynthesis(ctx, store.UpsertSynthesisParams{
			RepositoryID:        s.repoID,
			CanonicalName:       row.CanonicalName,
			Content:             row.Content,
			CoveredSummaryIds:   []pgtype.UUID{},
			CoveredConceptIds:   coveredCon,
			EmbeddedImageIds:    coveredImg,
			Model:               model,
		}); err != nil {
			log.Printf("stream import: upserting synthesis %s: %v", row.CanonicalName, err)
			return nil
		}
		count++
		return nil
	})
	return count, err
}

func (s *StreamImporter) streamInvestigations(ctx context.Context, dec *json.Decoder) (int, error) {
	count := 0
	err := decodeArray(dec, func(idx int) error {
		var row InvestigationRow
		if err := dec.Decode(&row); err != nil {
			return fmt.Errorf("decoding investigation idx %d: %w", idx, err)
		}
		row.Idx = idx
		invID := pgtype.UUID{}
		_ = invID.Scan(uuid.New().String())
		var topic *string
		if row.Topic != "" {
			t := row.Topic
			topic = &t
		}
		if _, err := s.queries.CreateInvestigation(ctx, store.CreateInvestigationParams{
			ID:           invID,
			RepositoryID: s.repoID,
			Title:        row.Title,
			Topic:        topic,
		}); err != nil {
			log.Printf("stream import: creating investigation idx %d: %v", idx, err)
			return nil
		}
		s.growInvestigationUUIDs(idx, invID)
		count++
		return nil
	})
	return count, err
}

func (s *StreamImporter) streamInvestigationSources(ctx context.Context, dec *json.Decoder) error {
	var invIDs, srcIDs []pgtype.UUID
	flush := func() error {
		if len(invIDs) == 0 {
			return nil
		}
		_, err := s.queries.BatchAddInvestigationSources(ctx, store.BatchAddInvestigationSourcesParams{
			Column1: invIDs, Column2: srcIDs,
		})
		if err != nil {
			log.Printf("stream import: batch adding investigation_sources: %v", err)
		}
		invIDs, srcIDs = nil, nil
		return nil
	}
	err := decodeArray(dec, func(_ int) error {
		var row InvestigationSourceRow
		if err := dec.Decode(&row); err != nil {
			return fmt.Errorf("decoding investigation_source: %w", err)
		}
		iID, ok := s.getInvestigationUUID(row.InvestigationIdx)
		if !ok {
			return nil
		}
		sID, ok := s.getSourceUUID(row.SourceIdx)
		if !ok {
			return nil
		}
		invIDs = append(invIDs, iID)
		srcIDs = append(srcIDs, sID)
		if len(invIDs) >= streamImportBatchSize {
			return flush()
		}
		return nil
	})
	if err != nil {
		return err
	}
	return flush()
}

func (s *StreamImporter) streamReports(ctx context.Context, dec *json.Decoder) (int, error) {
	count := 0
	err := decodeArray(dec, func(idx int) error {
		var row ReportRow
		if err := dec.Decode(&row); err != nil {
			return fmt.Errorf("decoding report idx %d: %w", idx, err)
		}
		row.Idx = idx
		repID := pgtype.UUID{}
		_ = repID.Scan(uuid.New().String())
		var topic *string
		if row.Topic != "" {
			t := row.Topic
			topic = &t
		}
		var parentID pgtype.UUID
		if row.ParentIdx >= 0 {
			if pID, ok := s.getReportUUID(row.ParentIdx); ok {
				parentID = pID
			}
		}
		var simThresh *float64
		if row.SimilarityThreshold != 0 {
			v := row.SimilarityThreshold
			simThresh = &v
		}
		var embModel *string
		if row.EmbeddedModel != "" {
			m := row.EmbeddedModel
			embModel = &m
		}
		if _, err := s.queries.CreateReport(ctx, store.CreateReportParams{
			ID:           repID,
			RepositoryID: s.repoID,
			Title:        row.Title,
			Topic:        topic,
			BodyMd:       row.BodyMd,
			Status:       row.Status,
			ParentID:     parentID,
		}); err != nil {
			log.Printf("stream import: creating report idx %d: %v", idx, err)
			return nil
		}
		s.growReportUUIDs(idx, repID)
		count++
		if row.SimilarityThreshold != 0 || row.EmbeddedModel != "" || row.SentenceCount != 0 {
			var sc *int32
			if row.SentenceCount != 0 {
				v := int32(row.SentenceCount)
				sc = &v
			}
			if err := s.queries.MarkReportStatus(ctx, store.MarkReportStatusParams{
				ID:                  repID,
				Status:              row.Status,
				Error:               nil,
				AnnotationJobID:     nil,
				SentenceCount:       sc,
				EmbeddedModel:       embModel,
				SimilarityThreshold: simThresh,
			}); err != nil {
				log.Printf("stream import: marking report status idx %d: %v", idx, err)
			}
		}
		return nil
	})
	return count, err
}

func (s *StreamImporter) streamReportAnnotations(ctx context.Context, dec *json.Decoder) error {
	var repIDs []pgtype.UUID
	var sentenceIdxs []int32
	var sentenceTexts []string
	var factIDs []pgtype.UUID
	var scores []float64
	var postures []string
	flush := func() error {
		if len(repIDs) == 0 {
			return nil
		}
		_, err := s.queries.BatchAddReportAnnotations(ctx, store.BatchAddReportAnnotationsParams{
			Column1: repIDs, Column2: sentenceIdxs, Column3: sentenceTexts,
			Column4: factIDs, Column5: scores, Column6: postures,
		})
		if err != nil {
			log.Printf("stream import: batch adding report_annotations: %v", err)
		}
		repIDs, sentenceIdxs, sentenceTexts, factIDs, scores, postures = nil, nil, nil, nil, nil, nil
		return nil
	}
	err := decodeArray(dec, func(_ int) error {
		var row ReportAnnotationRow
		if err := dec.Decode(&row); err != nil {
			return fmt.Errorf("decoding report_annotation: %w", err)
		}
		repID, ok := s.getReportUUID(row.ReportIdx)
		if !ok {
			return nil
		}
		fID, ok := s.getFactUUID(row.FactIdx)
		if !ok {
			return nil
		}
		repIDs = append(repIDs, repID)
		sentenceIdxs = append(sentenceIdxs, int32(row.SentenceIndex))
		sentenceTexts = append(sentenceTexts, row.SentenceText)
		factIDs = append(factIDs, fID)
		scores = append(scores, row.Score)
		postures = append(postures, row.Posture)
		if len(repIDs) >= streamImportBatchSize {
			return flush()
		}
		return nil
	})
	if err != nil {
		return err
	}
	return flush()
}

// streamEmbeddings reads the embeddings object, streams fact_vectors
// and concept_vectors in batches of 1000 to Qdrant. Returns
// needsReembed=true when the bundle's model doesn't match the local
// config (or Qdrant isn't configured).
func (s *StreamImporter) streamEmbeddings(ctx context.Context, dec *json.Decoder) (bool, error) {
	if s.qdrant == nil {
		return true, skipValue(dec)
	}

	// Decode the embeddings object token by token.
	t, err := dec.Token()
	if err != nil {
		return false, fmt.Errorf("stream import: reading embeddings open: %w", err)
	}
	if d, ok := t.(json.Delim); !ok || d != '{' {
		return false, fmt.Errorf("stream import: embeddings is not an object")
	}

	needsReembed := false
	for dec.More() {
		t, err = dec.Token()
		if err != nil {
			return false, fmt.Errorf("stream import: reading embeddings key: %w", err)
		}
		key, _ := t.(string)
		switch key {
		case "model":
			var m string
			if err := dec.Decode(&m); err != nil {
				return false, fmt.Errorf("stream import: decoding embeddings model: %w", err)
			}
			if m != s.embeddingModel {
				needsReembed = true
			}
		case "dimensions":
			var d int
			_ = dec.Decode(&d)
		case "fact_vectors":
			if needsReembed {
				if err := skipValue(dec); err != nil {
					return false, err
				}
				continue
			}
			if err := s.streamVectors(ctx, dec, "fact"); err != nil {
				return false, err
			}
		case "concept_vectors":
			if needsReembed {
				if err := skipValue(dec); err != nil {
					return false, err
				}
				continue
			}
			if err := s.streamVectors(ctx, dec, "concept"); err != nil {
				return false, err
			}
		default:
			if err := skipValue(dec); err != nil {
				return false, err
			}
		}
	}
	// Read the closing '}'.
	_, _ = dec.Token()
	return needsReembed, nil
}

// streamVectors reads a vectors map (idx→[]float32) key-by-key,
// accumulates batches of 1000, and upserts to Qdrant.
//
// The opening '{' of the vectors object is read by decodeObject itself
// (not here) — reading it here would consume the first map key and make
// decodeObject see a stringified idx (e.g. "100246") where it expects
// '{', surfacing as "expected object, got <idx>" on repos with ≥100k
// facts/concepts.
func (s *StreamImporter) streamVectors(ctx context.Context, dec *json.Decoder, kind string) error {
	if kind == "fact" {
		var batch []qdrantstore.FactPoint
		err := decodeObject(dec, func(key string) error {
			idx := atoiSafe(key)
			var vec []float32
			if err := dec.Decode(&vec); err != nil {
				return fmt.Errorf("decoding fact vector %q: %w", key, err)
			}
			fID, ok := s.getFactUUID(idx)
			if !ok {
				return nil
			}
			batch = append(batch, qdrantstore.FactPoint{
				ID:           asUUID(fID),
				Vector:       vec,
				RepositoryID: s.repoUUID,
				Status:       "stable",
			})
			if len(batch) >= streamImportBatchSize {
				if err := s.qdrant.UpsertFactVectors(ctx, batch); err != nil {
					return fmt.Errorf("upserting fact vectors: %w", err)
				}
				batch = batch[:0]
			}
			return nil
		})
		if err != nil {
			return err
		}
		if len(batch) > 0 {
			if err := s.qdrant.UpsertFactVectors(ctx, batch); err != nil {
				return fmt.Errorf("upserting fact vectors (final): %w", err)
			}
		}
	} else {
		var batch []qdrantstore.ConceptPoint
		err := decodeObject(dec, func(key string) error {
			idx := atoiSafe(key)
			var vec []float32
			if err := dec.Decode(&vec); err != nil {
				return fmt.Errorf("decoding concept vector %q: %w", key, err)
			}
			cID, ok := s.getConceptUUID(idx)
			if !ok {
				return nil
			}
			batch = append(batch, qdrantstore.ConceptPoint{
				ID:           asUUID(cID),
				Vector:       vec,
				RepositoryID: s.repoUUID,
			})
			if len(batch) >= streamImportBatchSize {
				if err := s.qdrant.UpsertConceptVectors(ctx, batch); err != nil {
					return fmt.Errorf("upserting concept vectors: %w", err)
				}
				batch = batch[:0]
			}
			return nil
		})
		if err != nil {
			return err
		}
		if len(batch) > 0 {
			if err := s.qdrant.UpsertConceptVectors(ctx, batch); err != nil {
				return fmt.Errorf("upserting concept vectors (final): %w", err)
			}
		}
	}
	return nil
}

// ── helpers ──────────────────────────────────────────────────────

// pendingImages / pendingBodies are set during the images/bodies
// sections and consumed during source_images/source_bodies.
func (s *StreamImporter) growSourceUUIDs(idx int, id pgtype.UUID) {
	for len(s.sourceUUIDs) <= idx {
		s.sourceUUIDs = append(s.sourceUUIDs, pgtype.UUID{})
	}
	s.sourceUUIDs[idx] = id
}
func (s *StreamImporter) getSourceUUID(idx int) (pgtype.UUID, bool) {
	if idx < 0 || idx >= len(s.sourceUUIDs) {
		return pgtype.UUID{}, false
	}
	id := s.sourceUUIDs[idx]
	return id, id.Valid
}
func (s *StreamImporter) growSourceImageUUIDs(idx int, imgID, srcID pgtype.UUID) {
	for len(s.sourceImageUUIDs) <= idx {
		s.sourceImageUUIDs = append(s.sourceImageUUIDs, pgtype.UUID{})
		s.sourceImageSources = append(s.sourceImageSources, pgtype.UUID{})
	}
	s.sourceImageUUIDs[idx] = imgID
	s.sourceImageSources[idx] = srcID
}
func (s *StreamImporter) getSourceImageUUID(idx int) (pgtype.UUID, bool) {
	if idx < 0 || idx >= len(s.sourceImageUUIDs) {
		return pgtype.UUID{}, false
	}
	id := s.sourceImageUUIDs[idx]
	return id, id.Valid
}
func (s *StreamImporter) getSourceImageSource(idx int) (pgtype.UUID, bool) {
	if idx < 0 || idx >= len(s.sourceImageSources) {
		return pgtype.UUID{}, false
	}
	id := s.sourceImageSources[idx]
	return id, id.Valid
}
func (s *StreamImporter) growFactUUIDs(idx int, id pgtype.UUID) {
	for len(s.factUUIDs) <= idx {
		s.factUUIDs = append(s.factUUIDs, pgtype.UUID{})
	}
	s.factUUIDs[idx] = id
}
func (s *StreamImporter) getFactUUID(idx int) (pgtype.UUID, bool) {
	if idx < 0 || idx >= len(s.factUUIDs) {
		return pgtype.UUID{}, false
	}
	id := s.factUUIDs[idx]
	return id, id.Valid
}
func (s *StreamImporter) growConceptUUIDs(idx int, id pgtype.UUID) {
	for len(s.conceptUUIDs) <= idx {
		s.conceptUUIDs = append(s.conceptUUIDs, pgtype.UUID{})
	}
	s.conceptUUIDs[idx] = id
}
func (s *StreamImporter) getConceptUUID(idx int) (pgtype.UUID, bool) {
	if idx < 0 || idx >= len(s.conceptUUIDs) {
		return pgtype.UUID{}, false
	}
	id := s.conceptUUIDs[idx]
	return id, id.Valid
}
func (s *StreamImporter) growInvestigationUUIDs(idx int, id pgtype.UUID) {
	for len(s.investigationUUIDs) <= idx {
		s.investigationUUIDs = append(s.investigationUUIDs, pgtype.UUID{})
	}
	s.investigationUUIDs[idx] = id
}
func (s *StreamImporter) getInvestigationUUID(idx int) (pgtype.UUID, bool) {
	if idx < 0 || idx >= len(s.investigationUUIDs) {
		return pgtype.UUID{}, false
	}
	id := s.investigationUUIDs[idx]
	return id, id.Valid
}
func (s *StreamImporter) growReportUUIDs(idx int, id pgtype.UUID) {
	for len(s.reportUUIDs) <= idx {
		s.reportUUIDs = append(s.reportUUIDs, pgtype.UUID{})
	}
	s.reportUUIDs[idx] = id
}
func (s *StreamImporter) getReportUUID(idx int) (pgtype.UUID, bool) {
	if idx < 0 || idx >= len(s.reportUUIDs) {
		return pgtype.UUID{}, false
	}
	id := s.reportUUIDs[idx]
	return id, id.Valid
}

// idxsToUUIDs remaps a slice of bundle idxs to local UUIDs via the
// dense slice. Returns an empty slice for nil maps (e.g. summary idxs).
func (s *StreamImporter) idxsToUUIDs(idxs []int, uuids []pgtype.UUID) []pgtype.UUID {
	out := make([]pgtype.UUID, 0, len(idxs))
	for _, idx := range idxs {
		if idx >= 0 && idx < len(uuids) && uuids[idx].Valid {
			out = append(out, uuids[idx])
		}
	}
	return out
}

// decodeArray reads a JSON array, calling fn for each element. The
// caller decodes the element inside fn via dec.Decode.
func decodeArray(dec *json.Decoder, fn func(idx int) error) error {
	t, err := dec.Token()
	if err != nil {
		return fmt.Errorf("reading array open: %w", err)
	}
	if d, ok := t.(json.Delim); !ok || d != '[' {
		// null or non-array — skip the value.
		if t == nil {
			return nil
		}
		return fmt.Errorf("expected array, got %v", t)
	}
	idx := 0
	for dec.More() {
		if err := fn(idx); err != nil {
			return err
		}
		idx++
	}
	// Read the closing ']'.
	_, _ = dec.Token()
	return nil
}

// decodeObject reads a JSON object, calling fn for each key. The
// caller decodes the value inside fn via dec.Decode.
func decodeObject(dec *json.Decoder, fn func(key string) error) error {
	t, err := dec.Token()
	if err != nil {
		return fmt.Errorf("reading object open: %w", err)
	}
	if d, ok := t.(json.Delim); !ok || d != '{' {
		if t == nil {
			return nil
		}
		return fmt.Errorf("expected object, got %v", t)
	}
	for dec.More() {
		t, err = dec.Token()
		if err != nil {
			return fmt.Errorf("reading object key: %w", err)
		}
		key, ok := t.(string)
		if !ok {
			return fmt.Errorf("non-string object key: %v", t)
		}
		if err := fn(key); err != nil {
			return err
		}
	}
	// Read the closing '}'.
	_, _ = dec.Token()
	return nil
}

// skipValue consumes and discards one JSON value from the decoder.
func skipValue(dec *json.Decoder) error {
	var raw json.RawMessage
	return dec.Decode(&raw)
}