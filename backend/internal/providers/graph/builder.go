package graph

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/openktree/open-knowledge-tree/backend/internal/qdrantstore"
	"github.com/openktree/open-knowledge-tree/backend/internal/store"
)

// BundleBuilder assembles a GraphBundle from a repository's derived
// layer. It reads every entity via the per-repo *store.Queries (the
// same one the per-repo chi middleware builds) and fetches Qdrant
// vectors for the embeddings section. The result is a complete,
// self-contained bundle the export task gzips + pushes to the
// registry.
//
// The builder is transport-agnostic: it knows nothing about HTTP or
// River. The export task constructs one per export job and calls
// Build, then hands the bundle to the registry client.
type BundleBuilder struct {
	queries        *store.Queries
	qdrant         *qdrantstore.Store
	repoID         pgtype.UUID
	repoUUID       uuid.UUID
	embeddingModel string
	embeddingDims  int
}

// NewBundleBuilder constructs a builder for the given repo. queries
// is the per-repo *store.Queries (the caller resolves the pool); qdrant
// may be nil (the bundle's embeddings section is left empty).
// embeddingModel/dims are stamped on the bundle so the import path
// can decide whether to upsert the vectors directly or re-embed.
func NewBundleBuilder(queries *store.Queries, qdrant *qdrantstore.Store, repoID pgtype.UUID, embeddingModel string, embeddingDims int) *BundleBuilder {
	return &BundleBuilder{
		queries:        queries,
		qdrant:         qdrant,
		repoID:         repoID,
		repoUUID:       asUUID(repoID),
		embeddingModel: embeddingModel,
		embeddingDims:  embeddingDims,
	}
}

// Build assembles the full GraphBundle. Entity order is sources →
// facts → fact_sources → concepts → concept_aliases → fact_concepts →
// summaries → syntheses → investigations → investigation_sources →
// reports → report_annotations → embeddings. Each entity is assigned
// a sequential idx as it streams in, and cross-references (fact_sources,
// fact_concepts, investigation_sources, report_annotations) are
// resolved to idxs via the id→idx maps built during the first pass.
//
// The bundle is built entirely in memory. For very large repos (tens
// of thousands of facts + embeddings) this can reach ~50MB before
// gzip; the export task marshals to gzip immediately to bound peak
// memory. A future streaming variant could write directly to a
// gzip.Writer if memory pressure becomes an issue.
func (b *BundleBuilder) Build(ctx context.Context, meta BundleMetadata) (*GraphBundle, error) {
	bundle := &GraphBundle{
		SchemaVersion: SchemaVersion,
		Metadata:      meta,
	}
	bundle.Metadata.ExportedAt = time.Now().UTC()
	bundle.Metadata.EmbeddingModel = b.embeddingModel
	bundle.Metadata.EmbeddingDimensions = b.embeddingDims

	// Sources. Build the id→idx map as we go so fact_sources /
	// investigation_sources can resolve to idxs.
	sourceIdxByID := make(map[uuid.UUID]int)
	srcRows, err := b.queries.ListAllSourcesForExport(ctx, b.repoID)
	if err != nil {
		return nil, fmt.Errorf("export: listing sources: %w", err)
	}
	for _, r := range srcRows {
		idx := len(bundle.Sources)
		sourceIdxByID[asUUID(r.ID)] = idx
		bundle.Sources = append(bundle.Sources, SourceRow{
			Idx:            idx,
			URL:            r.Url,
			DOI:            ptrStr(r.Doi),
			Kind:           r.Kind,
			Status:         r.Status,
			ParsedTitle:    ptrStr(r.ParsedTitle),
			ParsedText:     ptrStr(r.ParsedText),
			ParsedMarkdown: ptrStr(r.ParsedMarkdown),
			PublishedAt:    dateStr(r.PublishedAt),
			SHA256:         sourceSHA(r),
		})
	}
	bundle.Metadata.SourceCount = len(bundle.Sources)

	// Facts. Build id→idx map for fact_concepts / report_annotations.
	factIdxByID := make(map[uuid.UUID]int)
	factRows, err := b.queries.ListAllFactsForExport(ctx, b.repoID)
	if err != nil {
		return nil, fmt.Errorf("export: listing facts: %w", err)
	}
	for _, r := range factRows {
		idx := len(bundle.Facts)
		factIdxByID[asUUID(r.ID)] = idx
		bundle.Facts = append(bundle.Facts, FactRow{
			Idx:           idx,
			Text:          r.Text,
			FactKind:      r.FactKind,
			ImageURL:      ptrStr(r.ImageUrl),
			ContentHash:   factContentHash(r.Text),
			PromptsetHash: ptrStr(r.PromptsetHash),
			Status:        r.Status,
		})
	}
	bundle.Metadata.FactCount = len(bundle.Facts)

	// Fact sources.
	fsRows, err := b.queries.ListAllFactSourcesForExport(ctx, b.repoID)
	if err != nil {
		return nil, fmt.Errorf("export: listing fact_sources: %w", err)
	}
	for _, r := range fsRows {
		fIdx, ok := factIdxByID[asUUID(r.FactID)]
		if !ok {
			continue
		}
		sIdx, ok := sourceIdxByID[asUUID(r.SourceID)]
		if !ok {
			continue
		}
		bundle.FactSources = append(bundle.FactSources, FactSourceRow{
			FactIdx:    fIdx,
			SourceIdx:  sIdx,
			ChunkIndex: int(r.ChunkIndex),
		})
	}

	// Concepts. Build id→idx map for concept_aliases / fact_concepts.
	conceptIdxByID := make(map[uuid.UUID]int)
	conceptRows, err := b.queries.ListAllConceptsForExport(ctx, b.repoID)
	if err != nil {
		return nil, fmt.Errorf("export: listing concepts: %w", err)
	}
	for _, r := range conceptRows {
		idx := len(bundle.Concepts)
		conceptIdxByID[asUUID(r.ID)] = idx
		bundle.Concepts = append(bundle.Concepts, ConceptRow{
			Idx:           idx,
			CanonicalName: r.CanonicalName,
			Context:       r.Context,
			Description:   ptrStr(r.Description),
			PromptsetHash: ptrStr(r.PromptsetHash),
		})
	}
	bundle.Metadata.ConceptCount = len(bundle.Concepts)

	// Concept aliases.
	caRows, err := b.queries.ListAllConceptAliasesForExport(ctx, b.repoID)
	if err != nil {
		return nil, fmt.Errorf("export: listing concept_aliases: %w", err)
	}
	for _, r := range caRows {
		cIdx, ok := conceptIdxByID[asUUID(r.ConceptID)]
		if !ok {
			continue
		}
		bundle.ConceptAliases = append(bundle.ConceptAliases, ConceptAliasRow{
			ConceptIdx: cIdx,
			AliasText:  r.AliasText,
		})
	}

	// Fact concepts.
	fcRows, err := b.queries.ListAllFactConceptsForExport(ctx, b.repoID)
	if err != nil {
		return nil, fmt.Errorf("export: listing fact_concepts: %w", err)
	}
	for _, r := range fcRows {
		fIdx, ok := factIdxByID[asUUID(r.FactID)]
		if !ok {
			continue
		}
		cIdx, ok := conceptIdxByID[asUUID(r.ConceptID)]
		if !ok {
			continue
		}
		bundle.FactConcepts = append(bundle.FactConcepts, FactConceptRow{
			FactIdx:       fIdx,
			ConceptIdx:    cIdx,
			PromptsetHash: ptrStr(r.PromptsetHash),
		})
	}

	// Summaries. covered_fact_ids (UUIDs) → covered_fact_idxs (ints).
	summaryIdxByID := make(map[uuid.UUID]int)
	sumRows, err := b.queries.ListAllSummariesForExport(ctx, b.repoID)
	if err != nil {
		return nil, fmt.Errorf("export: listing summaries: %w", err)
	}
	for _, r := range sumRows {
		idx := len(bundle.ConceptSummaries)
		summaryIdxByID[asUUID(r.ID)] = idx
		cIdx, ok := conceptIdxByID[asUUID(r.ConceptID)]
		if !ok {
			continue
		}
		covered := make([]int, 0, len(r.CoveredFactIds))
		for _, fid := range r.CoveredFactIds {
			if fIdx, ok := factIdxByID[asUUID(fid)]; ok {
				covered = append(covered, fIdx)
			}
		}
		bundle.ConceptSummaries = append(bundle.ConceptSummaries, SummaryRow{
			ConceptIdx:      cIdx,
			SequenceNum:     int(r.SequenceNum),
			IsComplete:      r.IsComplete,
			FactCount:       int(r.FactCount),
			Content:         r.Content,
			CoveredFactIdxs: covered,
			Model:           ptrStr(r.Model),
		})
	}
	bundle.Metadata.SummaryCount = len(bundle.ConceptSummaries)

	// Syntheses. covered_summary_ids / covered_concept_ids /
	// embedded_image_ids (UUIDs) → idxs.
	synRows, err := b.queries.ListAllSynthesesForExport(ctx, b.repoID)
	if err != nil {
		return nil, fmt.Errorf("export: listing syntheses: %w", err)
	}
	for _, r := range synRows {
		coveredSum := idxsFromUUIDs(r.CoveredSummaryIds, summaryIdxByID)
		coveredCon := idxsFromUUIDs(r.CoveredConceptIds, conceptIdxByID)
		coveredImg := idxsFromUUIDs(r.EmbeddedImageIds, factIdxByID)
		bundle.ConceptSyntheses = append(bundle.ConceptSyntheses, SynthesisRow{
			CanonicalName:      r.CanonicalName,
			Content:            r.Content,
			CoveredSummaryIdxs: coveredSum,
			CoveredConceptIdxs: coveredCon,
			EmbeddedImageIdxs:  coveredImg,
			Model:              ptrStr(r.Model),
		})
	}
	bundle.Metadata.SynthesisCount = len(bundle.ConceptSyntheses)

	// Investigations.
	investigationIdxByID := make(map[uuid.UUID]int)
	invRows, err := b.queries.ListAllInvestigationsForExport(ctx, b.repoID)
	if err != nil {
		return nil, fmt.Errorf("export: listing investigations: %w", err)
	}
	for _, r := range invRows {
		idx := len(bundle.Investigations)
		investigationIdxByID[asUUID(r.ID)] = idx
		bundle.Investigations = append(bundle.Investigations, InvestigationRow{
			Idx:   idx,
			Title: r.Title,
			Topic: ptrStr(r.Topic),
		})
	}
	bundle.Metadata.InvestigationCount = len(bundle.Investigations)

	// Investigation sources.
	isRows, err := b.queries.ListAllInvestigationSourcesForExport(ctx, b.repoID)
	if err != nil {
		return nil, fmt.Errorf("export: listing investigation_sources: %w", err)
	}
	for _, r := range isRows {
		iIdx, ok := investigationIdxByID[asUUID(r.InvestigationID)]
		if !ok {
			continue
		}
		sIdx, ok := sourceIdxByID[asUUID(r.SourceID)]
		if !ok {
			continue
		}
		bundle.InvestigationSources = append(bundle.InvestigationSources, InvestigationSourceRow{
			InvestigationIdx: iIdx,
			SourceIdx:        sIdx,
		})
	}

	// Reports. parent_id (UUID) → parent_idx (int).
	reportIdxByID := make(map[uuid.UUID]int)
	repRows, err := b.queries.ListAllReportsForExport(ctx, b.repoID)
	if err != nil {
		return nil, fmt.Errorf("export: listing reports: %w", err)
	}
	for _, r := range repRows {
		idx := len(bundle.Reports)
		reportIdxByID[asUUID(r.ID)] = idx
		parentIdx := -1
		if r.ParentID.Valid {
			if pIdx, ok := reportIdxByID[asUUID(r.ParentID)]; ok {
				parentIdx = pIdx
			}
		}
		bundle.Reports = append(bundle.Reports, ReportRow{
			Idx:                 idx,
			Title:               r.Title,
			Topic:               ptrStr(r.Topic),
			BodyMd:              r.BodyMd,
			Status:              r.Status,
			ParentIdx:           parentIdx,
			SimilarityThreshold: ptrFloat(r.SimilarityThreshold),
			EmbeddedModel:       ptrStr(r.EmbeddedModel),
			SentenceCount:       int(ptrInt32(r.SentenceCount)),
		})
	}
	bundle.Metadata.ReportCount = len(bundle.Reports)

	// Report annotations.
	raRows, err := b.queries.ListAllReportAnnotationsForExport(ctx, b.repoID)
	if err != nil {
		return nil, fmt.Errorf("export: listing report_annotations: %w", err)
	}
	for _, r := range raRows {
		repIdx, ok := reportIdxByID[asUUID(r.ReportID)]
		if !ok {
			continue
		}
		fIdx, ok := factIdxByID[asUUID(r.FactID)]
		if !ok {
			continue
		}
		bundle.ReportAnnotations = append(bundle.ReportAnnotations, ReportAnnotationRow{
			ReportIdx:     repIdx,
			SentenceIndex: int(r.SentenceIndex),
			SentenceText:  r.SentenceText,
			FactIdx:       fIdx,
			Score:         r.Score,
			Posture:       ptrStr(r.Posture),
		})
	}

	// Embeddings. Fetch Qdrant vectors in batches of 1000 IDs so a
	// 50k-fact repo doesn't send one giant request. The vectors are
	// keyed by the stringified fact/concept idx (matching the bundle's
	// internal index scheme) so the import path can remap them to fresh
	// local UUIDs without a separate id map.
	if b.qdrant != nil {
		emb, err := b.fetchEmbeddings(ctx, factIdxByID, conceptIdxByID)
		if err != nil {
			return nil, fmt.Errorf("export: fetching embeddings: %w", err)
		}
		bundle.Embeddings = emb
	}

	// Compute the bundle sha256 (over the JSON, before gzip) so the
	// registry can dedup re-pushes of the same graph. The builder
	// sets it on the metadata; the export task reads it back when
	// pushing.
	jsonHash, err := bundleSHA(bundle)
	if err != nil {
		return nil, fmt.Errorf("export: computing bundle sha256: %w", err)
	}
	bundle.Metadata.SHA256 = jsonHash

	return bundle, nil
}

// fetchEmbeddings pulls Qdrant fact + concept vectors in batches and
// keys them by the stringified bundle idx. Returns nil (no embeddings
// section) when Qdrant is unconfigured or returns nothing.
func (b *BundleBuilder) fetchEmbeddings(ctx context.Context, factIdxByID, conceptIdxByID map[uuid.UUID]int) (*Embeddings, error) {
	emb := &Embeddings{
		Model:      b.embeddingModel,
		Dimensions: b.embeddingDims,
	}
	// Facts.
	factIDs := make([]uuid.UUID, 0, len(factIdxByID))
	for id := range factIdxByID {
		factIDs = append(factIDs, id)
	}
	if len(factIDs) > 0 {
		factVecs := make(map[string][]float32, len(factIDs))
		for i := 0; i < len(factIDs); i += 1000 {
			end := i + 1000
			if end > len(factIDs) {
				end = len(factIDs)
			}
			batch := factIDs[i:end]
			points, err := b.qdrant.GetFactVectorsByIDs(ctx, batch)
			if err != nil {
				return nil, fmt.Errorf("fetching fact vectors batch %d: %w", i, err)
			}
			for id, p := range points {
				idx, ok := factIdxByID[id]
				if !ok {
					continue
				}
				factVecs[itoa(idx)] = p.Vector
			}
		}
		emb.FactVectors = factVecs
	}
	// Concepts.
	conceptIDs := make([]uuid.UUID, 0, len(conceptIdxByID))
	for id := range conceptIdxByID {
		conceptIDs = append(conceptIDs, id)
	}
	if len(conceptIDs) > 0 {
		conceptVecs := make(map[string][]float32, len(conceptIDs))
		for i := 0; i < len(conceptIDs); i += 1000 {
			end := i + 1000
			if end > len(conceptIDs) {
				end = len(conceptIDs)
			}
			batch := conceptIDs[i:end]
			points, err := b.qdrant.GetConceptVectorsByIDs(ctx, batch)
			if err != nil {
				return nil, fmt.Errorf("fetching concept vectors batch %d: %w", i, err)
			}
			for id, p := range points {
				idx, ok := conceptIdxByID[id]
				if !ok {
					continue
				}
				conceptVecs[itoa(idx)] = p.Vector
			}
		}
		emb.ConceptVectors = conceptVecs
	}
	if len(emb.FactVectors) == 0 && len(emb.ConceptVectors) == 0 {
		return nil, nil
	}
	return emb, nil
}

// ── helpers ──────────────────────────────────────────────────────────

func asUUID(p pgtype.UUID) uuid.UUID {
	if !p.Valid {
		return uuid.Nil
	}
	return p.Bytes
}

func ptrStr(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func ptrFloat(f *float64) float64 {
	if f == nil {
		return 0
	}
	return *f
}

func ptrInt32(i *int32) int32 {
	if i == nil {
		return 0
	}
	return *i
}

func dateStr(d pgtype.Date) string {
	if !d.Valid {
		return ""
	}
	return d.Time.Format("2006-01-02")
}

func itoa(i int) string {
	return fmt.Sprintf("%d", i)
}

func idxsFromUUIDs(ids []pgtype.UUID, idxByID map[uuid.UUID]int) []int {
	out := make([]int, 0, len(ids))
	for _, id := range ids {
		if idx, ok := idxByID[asUUID(id)]; ok {
			out = append(out, idx)
		}
	}
	return out
}

func factContentHash(text string) string {
	h := sha256.Sum256([]byte(text))
	return hex.EncodeToString(h[:])
}

func sourceSHA(r store.ListAllSourcesForExportRow) string {
	h := sha256.New()
	h.Write([]byte(r.Url))
	if r.ParsedText != nil {
		h.Write([]byte(*r.ParsedText))
	}
	if r.ParsedMarkdown != nil {
		h.Write([]byte(*r.ParsedMarkdown))
	}
	if r.ParsedTitle != nil {
		h.Write([]byte(*r.ParsedTitle))
	}
	return hex.EncodeToString(h.Sum(nil))
}

func bundleSHA(b *GraphBundle) (string, error) {
	saved := b.Metadata.SHA256
	b.Metadata.SHA256 = ""
	data, err := marshalCanonical(b)
	b.Metadata.SHA256 = saved
	if err != nil {
		return "", err
	}
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:]), nil
}

func marshalCanonical(b *GraphBundle) ([]byte, error) {
	// json.Marshal is deterministic for structs (fields in declaration
	// order). Maps (embeddings vectors) are NOT deterministic, so we
	// skip them in the hash computation by zeroing them temporarily.
	savedEmb := b.Embeddings
	b.Embeddings = nil
	data, err := json.Marshal(b)
	b.Embeddings = savedEmb
	return data, err
}
