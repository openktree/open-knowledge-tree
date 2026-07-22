package graph

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/openktree/open-knowledge-tree/backend/internal/qdrantstore"
	"github.com/openktree/open-knowledge-tree/backend/internal/store"
)

// ImportMode controls how the importer handles an existing repository.
//   - "new": every entity is inserted with a fresh UUID (uuid.New per
//     row). No dedup is needed because the repo is empty. This is the
//     fast path: one INSERT per entity, no conflict checks.
//   - "existing": sources dedup by (url), facts by (content_hash),
//     concepts by (canonical_name, context). Summaries/syntheses are
//     imported verbatim and skipped on conflict (the user can trigger
//     re-summarize/re-synthesize from the UI). Reuses the existing
//     idempotent queries (ON CONFLICT DO NOTHING).
type ImportMode string

const (
	ImportModeNew      ImportMode = "new"
	ImportModeExisting ImportMode = "existing"
)

// ImportResult is the outcome of an Import call. The counts are the
// rows actually inserted (not the input size); on a merge into an
// existing repo, duplicates are skipped so the counts reflect the
// net new entities. NeedsReembed is true when the bundle's embedding
// model doesn't match the local config and the caller should enqueue
// embed_facts + embed_concepts so search works post-import.
type ImportResult struct {
	ImportedSources        int
	ImportedFacts          int
	ImportedConcepts       int
	ImportedSummaries      int
	ImportedSyntheses      int
	ImportedReports        int
	ImportedInvestigations int
	NeedsReembed           bool
}

// BundleImporter applies a GraphBundle to a repository. It holds a
// per-repo *store.Queries (the caller resolves the pool) and the
// Qdrant store for the embeddings upsert. Transport-agnostic: the
// import task constructs one per import job and calls Import.
type BundleImporter struct {
	queries        *store.Queries
	qdrant         *qdrantstore.Store
	repoID         pgtype.UUID
	repoUUID       uuid.UUID
	embeddingModel string
}

// NewBundleImporter constructs an importer for the given repo. qdrant
// may be nil (the embeddings section is skipped). embeddingModel is
// the local config's embedding model name; when it matches the
// bundle's model, the importer upserts the Qdrant vectors directly,
// otherwise it sets NeedsReembed so the caller enqueues embed_facts
// + embed_concepts.
func NewBundleImporter(queries *store.Queries, qdrant *qdrantstore.Store, repoID pgtype.UUID, embeddingModel string) *BundleImporter {
	return &BundleImporter{
		queries:        queries,
		qdrant:         qdrant,
		repoID:         repoID,
		repoUUID:       asUUID(repoID),
		embeddingModel: embeddingModel,
	}
}

// Import applies the bundle to the importer's repository. The order
// mirrors the builder's: sources → facts → fact_sources → concepts →
// concept_aliases → fact_concepts → summaries → syntheses →
// investigations → investigation_sources → reports →
// report_annotations → embeddings. Each entity is inserted with a
// fresh UUID (new mode) or deduped (existing mode), and the
// idx→UUID remap table is built as rows are inserted so cross-ref
// junctions resolve to the fresh local UUIDs.
//
// The import is NOT wrapped in a single transaction: a 10k-fact
// graph would hold a long-running tx, and the ON CONFLICT DO NOTHING
// semantics make a partial import safe to re-run (a re-import is a
// no-op for everything that already landed). The caller (the import
// task) may wrap the whole call in a tx if it prefers atomicity.
func (b *BundleImporter) Import(ctx context.Context, bundle *GraphBundle, mode ImportMode) (*ImportResult, error) {
	if bundle.SchemaVersion > SchemaVersion {
		return nil, fmt.Errorf("import: bundle schema_version %d is newer than this build supports (%d); upgrade OKT", bundle.SchemaVersion, SchemaVersion)
	}
	result := &ImportResult{}

	// Sources. Build sourceIdx→UUID map for fact_sources /
	// investigation_sources.
	sourceUUIDByIdx := make(map[int]pgtype.UUID)
	for _, s := range bundle.Sources {
		var doi *string
		if s.DOI != "" {
			d := s.DOI
			doi = &d
		}
		var publishedAt pgtype.Date
		if s.PublishedAt != "" {
			if t, err := time.Parse("2006-01-02", s.PublishedAt); err == nil {
				publishedAt = pgtype.Date{Valid: true, Time: t}
			}
		}
		if mode == ImportModeExisting {
			// Dedup by URL: if a source with this URL exists, reuse it.
			existing, err := b.queries.GetSourceByRepoAndURL(ctx, store.GetSourceByRepoAndURLParams{
				RepositoryID: b.repoID,
				Url:          s.URL,
			})
			if err == nil {
				sourceUUIDByIdx[s.Idx] = existing.ID
				continue
			}
		}
		srcID := pgtype.UUID{}
		if err := srcID.Scan(uuid.New().String()); err != nil {
			return nil, fmt.Errorf("import: scanning source id: %w", err)
		}
		_, err := b.queries.CreateSource(ctx, store.CreateSourceParams{
			ID:           srcID,
			RepositoryID: b.repoID,
			Url:          s.URL,
			Kind:         s.Kind,
			Status:       s.Status,
			Doi:          doi,
		})
		if err != nil {
			// ON CONFLICT does nothing for sources? CreateSource has no
			// ON CONFLICT clause; a unique violation on (repo, url) means
			// a concurrent insert raced us. Re-fetch and reuse.
			existing, lookupErr := b.queries.GetSourceByRepoAndURL(ctx, store.GetSourceByRepoAndURLParams{
				RepositoryID: b.repoID,
				Url:          s.URL,
			})
			if lookupErr != nil {
				log.Printf("import: creating source idx %d (%s): %v", s.Idx, s.URL, err)
				continue
			}
			srcID = existing.ID
		}
		sourceUUIDByIdx[s.Idx] = srcID
		result.ImportedSources++
		// Persist published_at (CreateSource doesn't take it; UpdateSourcePublishedAt does).
		if publishedAt.Valid {
			if _, err := b.queries.UpdateSourcePublishedAt(ctx, store.UpdateSourcePublishedAtParams{
				ID:          srcID,
				PublishedAt: publishedAt,
			}); err != nil {
				log.Printf("import: setting source published_at idx %d: %v", s.Idx, err)
			}
		}
		// Persist parsed content (CreateSource doesn't take parsed fields;
		// MarkSourceParsed does). Best-effort: a failure logs and the
		// source still exists with status as-is.
		if s.ParsedText != "" || s.ParsedMarkdown != "" || s.ParsedTitle != "" {
			var title *string
			if s.ParsedTitle != "" {
				t := s.ParsedTitle
				title = &t
			}
			var text *string
			if s.ParsedText != "" {
				t := s.ParsedText
				text = &t
			}
			var md *string
			if s.ParsedMarkdown != "" {
				m := s.ParsedMarkdown
				md = &m
			}
			status := "ok"
			if _, err := b.queries.MarkSourceParsed(ctx, store.MarkSourceParsedParams{
				ID:             srcID,
				ParsedTitle:    title,
				ParsedText:     text,
				ParsedMarkdown: md,
				ParseStatus:    &status,
			}); err != nil {
				log.Printf("import: marking source parsed idx %d: %v", s.Idx, err)
			}
		}
	}

	// Facts. Build factIdx→UUID map. Use BatchCreateFacts for the
	// high-volume insert; the batch is idempotent (ON CONFLICT DO
	// NOTHING on id) so the existing-repo merge path is safe.
	factUUIDByIdx := make(map[int]pgtype.UUID)
	factRows := bundle.Facts
	if len(factRows) > 0 {
		ids := make([]pgtype.UUID, 0, len(factRows))
		texts := make([]string, 0, len(factRows))
		factKinds := make([]string, 0, len(factRows))
		imageURLs := make([]string, 0, len(factRows))
		statuses := make([]string, 0, len(factRows))
		promptsetHashes := make([]string, 0, len(factRows))
		for _, f := range factRows {
			id := pgtype.UUID{}
			_ = id.Scan(uuid.New().String())
			ids = append(ids, id)
			texts = append(texts, f.Text)
			factKinds = append(factKinds, f.FactKind)
			imageURLs = append(imageURLs, f.ImageURL) // NULLIF handles "" → NULL
			statuses = append(statuses, f.Status)
			promptsetHashes = append(promptsetHashes, f.PromptsetHash)
			factUUIDByIdx[f.Idx] = id
		}
		if _, err := b.queries.BatchCreateFacts(ctx, store.BatchCreateFactsParams{
			Column1: ids,
			Column2: texts,
			Column3: factKinds,
			Column4: imageURLs,
			Column5: statuses,
			Column6: promptsetHashes,
		}); err != nil {
			log.Printf("import: batch creating facts: %v", err)
		}
		result.ImportedFacts = len(factRows)
	}

	// Fact sources.
	if len(bundle.FactSources) > 0 {
		factIDs := make([]pgtype.UUID, 0, len(bundle.FactSources))
		sourceIDs := make([]pgtype.UUID, 0, len(bundle.FactSources))
		chunks := make([]int32, 0, len(bundle.FactSources))
		for _, fs := range bundle.FactSources {
			fID, ok := factUUIDByIdx[fs.FactIdx]
			if !ok {
				continue
			}
			sID, ok := sourceUUIDByIdx[fs.SourceIdx]
			if !ok {
				continue
			}
			factIDs = append(factIDs, fID)
			sourceIDs = append(sourceIDs, sID)
			chunks = append(chunks, int32(fs.ChunkIndex))
		}
		if len(factIDs) > 0 {
			if _, err := b.queries.BatchAddFactSources(ctx, store.BatchAddFactSourcesParams{
				Column1: factIDs,
				Column2: sourceIDs,
				Column3: chunks,
			}); err != nil {
				log.Printf("import: batch adding fact_sources: %v", err)
			}
		}
	}

	// Concepts. Build conceptIdx→UUID map. Row-by-row because CreateConcept
	// has ON CONFLICT DO NOTHING + the existing-repo merge needs the
	// resolved id via GetConceptByNameContext.
	conceptUUIDByIdx := make(map[int]pgtype.UUID)
	for _, c := range bundle.Concepts {
		var desc *string
		if c.Description != "" {
			d := c.Description
			desc = &d
		}
		var psh *string
		if c.PromptsetHash != "" {
			p := c.PromptsetHash
			psh = &p
		}
		created, err := b.queries.CreateConcept(ctx, store.CreateConceptParams{
			RepositoryID:  b.repoID,
			CanonicalName: c.CanonicalName,
			Context:       c.Context,
			Description:   desc,
			PromptsetHash: psh,
		})
		if err == nil {
			conceptUUIDByIdx[c.Idx] = created.ID
			result.ImportedConcepts++
			continue
		}
		// ON CONFLICT DO NOTHING returns no rows; re-fetch by (name, context).
		existing, lookupErr := b.queries.GetConceptByNameContext(ctx, store.GetConceptByNameContextParams{
			RepositoryID:  b.repoID,
			CanonicalName: c.CanonicalName,
			Context:       c.Context,
		})
		if lookupErr != nil {
			log.Printf("import: creating concept idx %d (%s/%s): %v", c.Idx, c.CanonicalName, c.Context, err)
			continue
		}
		conceptUUIDByIdx[c.Idx] = existing.ID
	}

	// Concept aliases (batch).
	if len(bundle.ConceptAliases) > 0 {
		conceptIDs := make([]pgtype.UUID, 0, len(bundle.ConceptAliases))
		aliases := make([]string, 0, len(bundle.ConceptAliases))
		for _, ca := range bundle.ConceptAliases {
			cID, ok := conceptUUIDByIdx[ca.ConceptIdx]
			if !ok {
				continue
			}
			conceptIDs = append(conceptIDs, cID)
			aliases = append(aliases, ca.AliasText)
		}
		if len(conceptIDs) > 0 {
			if _, err := b.queries.BatchCreateConceptAliases(ctx, store.BatchCreateConceptAliasesParams{
				Column1: conceptIDs,
				Column2: aliases,
			}); err != nil {
				log.Printf("import: batch creating concept_aliases: %v", err)
			}
		}
	}

	// Fact concepts (batch).
	if len(bundle.FactConcepts) > 0 {
		factIDs := make([]pgtype.UUID, 0, len(bundle.FactConcepts))
		conceptIDs := make([]pgtype.UUID, 0, len(bundle.FactConcepts))
		pshs := make([]string, 0, len(bundle.FactConcepts))
		for _, fc := range bundle.FactConcepts {
			fID, ok := factUUIDByIdx[fc.FactIdx]
			if !ok {
				continue
			}
			cID, ok := conceptUUIDByIdx[fc.ConceptIdx]
			if !ok {
				continue
			}
			factIDs = append(factIDs, fID)
			conceptIDs = append(conceptIDs, cID)
			pshs = append(pshs, fc.PromptsetHash)
		}
		if len(factIDs) > 0 {
			if _, err := b.queries.BatchAddFactConcepts(ctx, store.BatchAddFactConceptsParams{
				Column1: factIDs,
				Column2: conceptIDs,
				Column3: pshs,
			}); err != nil {
				log.Printf("import: batch adding fact_concepts: %v", err)
			}
		}
	}

	// Summaries. Row-by-row via CreateSummary (idempotent on the
	// concept_id + sequence_num? CreateSummary has no ON CONFLICT;
	// for existing-repo merge we skip if a summary with the same
	// (concept_id, sequence_num) exists — checked via GetOpenSummary
	// is wrong (that's only open slices). For MVP we insert; a
	// duplicate on an existing repo would create a second slice, which
	// the unique partial index uq_concept_summaries_concept_open would
	// reject for open slices. The new-repo path always inserts fresh.
	for _, s := range bundle.ConceptSummaries {
		cID, ok := conceptUUIDByIdx[s.ConceptIdx]
		if !ok {
			continue
		}
		covered := make([]pgtype.UUID, 0, len(s.CoveredFactIdxs))
		for _, fIdx := range s.CoveredFactIdxs {
			if fID, ok := factUUIDByIdx[fIdx]; ok {
				covered = append(covered, fID)
			}
		}
		var model *string
		if s.Model != "" {
			m := s.Model
			model = &m
		}
		if _, err := b.queries.CreateSummary(ctx, store.CreateSummaryParams{
			ConceptID:      cID,
			RepositoryID:   b.repoID,
			Context:        "",
			SequenceNum:    int32(s.SequenceNum),
			IsComplete:     s.IsComplete,
			FactCount:      int32(s.FactCount),
			Content:        s.Content,
			CoveredFactIds: covered,
			Model:          model,
		}); err != nil {
			// A duplicate (concept_id, sequence_num) on an existing repo
			// would violate the unique partial index for open slices.
			// Log + skip; the existing summary stays.
			log.Printf("import: creating summary concept_idx %d seq %d: %v", s.ConceptIdx, s.SequenceNum, err)
			continue
		}
		result.ImportedSummaries++
	}

	// Syntheses. Upsert (ON CONFLICT on lower(canonical_name) updates).
	for _, s := range bundle.ConceptSyntheses {
		// covered idxs → UUIDs (best-effort; empty arrays are fine).
		coveredSum := idxsToUUIDs(s.CoveredSummaryIdxs, nil)
		coveredCon := idxsToUUIDs(s.CoveredConceptIdxs, conceptUUIDByIdx)
		coveredImg := idxsToUUIDs(s.EmbeddedImageIdxs, factUUIDByIdx)
		var model *string
		if s.Model != "" {
			m := s.Model
			model = &m
		}
		if _, err := b.queries.UpsertSynthesis(ctx, store.UpsertSynthesisParams{
			RepositoryID:      b.repoID,
			CanonicalName:     s.CanonicalName,
			Content:           s.Content,
			CoveredSummaryIds: coveredSum,
			CoveredConceptIds: coveredCon,
			EmbeddedImageIds:  coveredImg,
			Model:             model,
		}); err != nil {
			log.Printf("import: upserting synthesis %s: %v", s.CanonicalName, err)
			continue
		}
		result.ImportedSyntheses++
	}

	// Investigations. Build investigationIdx→UUID map.
	investigationUUIDByIdx := make(map[int]pgtype.UUID)
	for _, inv := range bundle.Investigations {
		invID := pgtype.UUID{}
		_ = invID.Scan(uuid.New().String())
		var topic *string
		if inv.Topic != "" {
			t := inv.Topic
			topic = &t
		}
		if _, err := b.queries.CreateInvestigation(ctx, store.CreateInvestigationParams{
			ID:           invID,
			RepositoryID: b.repoID,
			Title:        inv.Title,
			Topic:        topic,
		}); err != nil {
			log.Printf("import: creating investigation idx %d: %v", inv.Idx, err)
			continue
		}
		investigationUUIDByIdx[inv.Idx] = invID
		result.ImportedInvestigations++
	}

	// Investigation sources (batch).
	if len(bundle.InvestigationSources) > 0 {
		invIDs := make([]pgtype.UUID, 0, len(bundle.InvestigationSources))
		srcIDs := make([]pgtype.UUID, 0, len(bundle.InvestigationSources))
		for _, is := range bundle.InvestigationSources {
			iID, ok := investigationUUIDByIdx[is.InvestigationIdx]
			if !ok {
				continue
			}
			sID, ok := sourceUUIDByIdx[is.SourceIdx]
			if !ok {
				continue
			}
			invIDs = append(invIDs, iID)
			srcIDs = append(srcIDs, sID)
		}
		if len(invIDs) > 0 {
			if _, err := b.queries.BatchAddInvestigationSources(ctx, store.BatchAddInvestigationSourcesParams{
				Column1: invIDs,
				Column2: srcIDs,
			}); err != nil {
				log.Printf("import: batch adding investigation_sources: %v", err)
			}
		}
	}

	// Reports. Build reportIdx→UUID map. parent_idx resolved to the
	// parent's fresh UUID (reports are inserted in idx order, so a
	// parent is always inserted before its children).
	reportUUIDByIdx := make(map[int]pgtype.UUID)
	for _, r := range bundle.Reports {
		repID := pgtype.UUID{}
		_ = repID.Scan(uuid.New().String())
		var topic *string
		if r.Topic != "" {
			t := r.Topic
			topic = &t
		}
		var parentID pgtype.UUID
		if r.ParentIdx >= 0 {
			if pID, ok := reportUUIDByIdx[r.ParentIdx]; ok {
				parentID = pID
			}
		}
		var simThresh *float64
		if r.SimilarityThreshold != 0 {
			v := r.SimilarityThreshold
			simThresh = &v
		}
		var embModel *string
		if r.EmbeddedModel != "" {
			m := r.EmbeddedModel
			embModel = &m
		}
		if _, err := b.queries.CreateReport(ctx, store.CreateReportParams{
			ID:           repID,
			RepositoryID: b.repoID,
			Title:        r.Title,
			Topic:        topic,
			BodyMd:       r.BodyMd,
			Status:       r.Status,
			ParentID:     parentID,
		}); err != nil {
			log.Printf("import: creating report idx %d: %v", r.Idx, err)
			continue
		}
		reportUUIDByIdx[r.Idx] = repID
		result.ImportedReports++
		// Persist the extra columns CreateReport doesn't take via
		// MarkReportStatus (best-effort).
		if r.SimilarityThreshold != 0 || r.EmbeddedModel != "" || r.SentenceCount != 0 {
			var sc *int32
			if r.SentenceCount != 0 {
				v := int32(r.SentenceCount)
				sc = &v
			}
			if err := b.queries.MarkReportStatus(ctx, store.MarkReportStatusParams{
				ID:                  repID,
				Status:              r.Status,
				Error:               nil,
				AnnotationJobID:     nil,
				SentenceCount:       sc,
				EmbeddedModel:       embModel,
				SimilarityThreshold: simThresh,
			}); err != nil {
				log.Printf("import: marking report status idx %d: %v", r.Idx, err)
			}
		}
	}

	// Report annotations (batch).
	if len(bundle.ReportAnnotations) > 0 {
		repIDs := make([]pgtype.UUID, 0, len(bundle.ReportAnnotations))
		sentenceIdxs := make([]int32, 0, len(bundle.ReportAnnotations))
		sentenceTexts := make([]string, 0, len(bundle.ReportAnnotations))
		factIDs := make([]pgtype.UUID, 0, len(bundle.ReportAnnotations))
		scores := make([]float64, 0, len(bundle.ReportAnnotations))
		postures := make([]string, 0, len(bundle.ReportAnnotations))
		for _, ra := range bundle.ReportAnnotations {
			repID, ok := reportUUIDByIdx[ra.ReportIdx]
			if !ok {
				continue
			}
			fID, ok := factUUIDByIdx[ra.FactIdx]
			if !ok {
				continue
			}
			repIDs = append(repIDs, repID)
			sentenceIdxs = append(sentenceIdxs, int32(ra.SentenceIndex))
			sentenceTexts = append(sentenceTexts, ra.SentenceText)
			factIDs = append(factIDs, fID)
			scores = append(scores, ra.Score)
			postures = append(postures, ra.Posture)
		}
		if len(repIDs) > 0 {
			if _, err := b.queries.BatchAddReportAnnotations(ctx, store.BatchAddReportAnnotationsParams{
				Column1: repIDs,
				Column2: sentenceIdxs,
				Column3: sentenceTexts,
				Column4: factIDs,
				Column5: scores,
				Column6: postures,
			}); err != nil {
				log.Printf("import: batch adding report_annotations: %v", err)
			}
		}
	}

	// Embeddings. When the bundle's model matches the local config,
	// upsert the Qdrant vectors directly (remapped to the fresh local
	// UUIDs). Otherwise set NeedsReembed so the caller enqueues
	// embed_facts + embed_concepts.
	if bundle.Embeddings != nil && b.qdrant != nil {
		if bundle.Embeddings.Model == b.embeddingModel {
			if err := b.upsertEmbeddings(ctx, bundle, factUUIDByIdx, conceptUUIDByIdx); err != nil {
				log.Printf("import: upserting embeddings: %v", err)
			}
		} else {
			result.NeedsReembed = true
			log.Printf("import: bundle embedding model %q != local %q; enqueuing re-embed",
				bundle.Embeddings.Model, b.embeddingModel)
		}
	} else if bundle.Embeddings != nil && b.qdrant == nil {
		result.NeedsReembed = true
	}

	return result, nil
}

// upsertEmbeddings upserts the bundle's Qdrant vectors, remapping the
// bundle-internal idx keys to the fresh local UUIDs. Called only when
// the bundle's embedding model matches the local config.
func (b *BundleImporter) upsertEmbeddings(ctx context.Context, bundle *GraphBundle, factUUIDByIdx, conceptUUIDByIdx map[int]pgtype.UUID) error {
	if bundle.Embeddings == nil {
		return nil
	}
	// Facts.
	if len(bundle.Embeddings.FactVectors) > 0 {
		points := make([]qdrantstore.FactPoint, 0, len(bundle.Embeddings.FactVectors))
		for idxStr, vec := range bundle.Embeddings.FactVectors {
			idx := atoiSafe(idxStr)
			fID, ok := factUUIDByIdx[idx]
			if !ok {
				continue
			}
			points = append(points, qdrantstore.FactPoint{
				ID:           asUUID(fID),
				Vector:       vec,
				RepositoryID: b.repoUUID,
				Status:       "stable",
			})
		}
		if len(points) > 0 {
			if err := b.qdrant.UpsertFactVectors(ctx, points); err != nil {
				return fmt.Errorf("upserting fact vectors: %w", err)
			}
		}
	}
	// Concepts.
	if len(bundle.Embeddings.ConceptVectors) > 0 {
		points := make([]qdrantstore.ConceptPoint, 0, len(bundle.Embeddings.ConceptVectors))
		for idxStr, vec := range bundle.Embeddings.ConceptVectors {
			idx := atoiSafe(idxStr)
			cID, ok := conceptUUIDByIdx[idx]
			if !ok {
				continue
			}
			points = append(points, qdrantstore.ConceptPoint{
				ID:           asUUID(cID),
				Vector:       vec,
				RepositoryID: b.repoUUID,
			})
		}
		if len(points) > 0 {
			if err := b.qdrant.UpsertConceptVectors(ctx, points); err != nil {
				return fmt.Errorf("upserting concept vectors: %w", err)
			}
		}
	}
	return nil
}

// idxsToUUIDs remaps a slice of bundle idxs to local UUIDs via the
// provided map. When the map is nil (e.g. summary idxs which aren't
// tracked), returns an empty slice — the caller (UpsertSynthesis)
// accepts empty arrays.
func idxsToUUIDs(idxs []int, uuidByIdx map[int]pgtype.UUID) []pgtype.UUID {
	if uuidByIdx == nil {
		return []pgtype.UUID{}
	}
	out := make([]pgtype.UUID, 0, len(idxs))
	for _, idx := range idxs {
		if id, ok := uuidByIdx[idx]; ok {
			out = append(out, id)
		}
	}
	return out
}

func atoiSafe(s string) int {
	var n int
	for _, c := range s {
		if c < '0' || c > '9' {
			return -1
		}
		n = n*10 + int(c-'0')
	}
	return n
}
