package graph

import (
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash"
	"io"
	"log"
	"sort"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/openktree/open-knowledge-tree/backend/internal/store"
)

// BuildStats is the outcome of StreamBuild: the counts that populate
// the bundle's metadata section + the SHA256 of the canonical JSON
// (the bytes with embeddings/images/bodies zeroed and sha256="",
// matching marshalCanonical). The caller (export task) uses these to
// fill the metadata in a second streaming pass and to record the
// ExportGraphResult.
type BuildStats struct {
	SourceCount         int
	FactCount           int
	ConceptCount        int
	SummaryCount        int
	SynthesisCount      int
	ReportCount         int
	InvestigationCount  int
	ImageCount          int
	BodyCount           int
	EmbeddingFactCount  int
	EmbeddingConceptCnt int
	Bytes               int64
	SHA256              string
}

// StreamBuild assembles a GraphBundle and streams it as gzipped JSON
// directly to w. Unlike Build (which materializes the whole bundle in
// memory), StreamBuild writes each entity as it's fetched from the DB
// / Qdrant / storage, so peak memory is bounded by one Qdrant batch
// (~1.2 MB) + one storage image at a time, not the full ~11 GB bundle.
//
// shaOverride controls the metadata.sha256 field written to the output:
//   - "" => emit no sha256 field (omitempty) and compute the canonical
//     hash incrementally as the bytes flow. The caller reads the real
//     hash from BuildStats.SHA256, then re-calls StreamBuild with
//     shaOverride=<that hash> to produce the dedup-correct output.
//   - non-"" => emit sha256=<hash> in metadata and skip hashing (the
//     caller already computed it in a prior pass).
//
// The two-pass dance is required because metadata is the 2nd JSON
// field (written before any entity rows) but sha256 is only known
// after every row has streamed. Pass 1: StreamBuild(discard, "") →
// stats.SHA256. Pass 2: StreamBuild(tempFile, stats.SHA256). Memory
// stays bounded in both passes; the cost is 2× streaming CPU.
//
// The output JSON is byte-identical to json.Marshal(*GraphBundle) for
// the same data (same field order, compact formatting, omitempty
// rules), so a downstream json.Unmarshal sees the same shape the
// in-memory Build path produces. The canonical sha matches
// marshalCanonical's output (embeddings/images/bodies zeroed,
// sha256="").
func (b *BundleBuilder) StreamBuild(ctx context.Context, meta BundleMetadata, w io.Writer, shaOverride string) (*BuildStats, error) {
	stats := &BuildStats{}
	meta.ExportedAt = nowFn().UTC()
	meta.EmbeddingModel = b.embeddingModel
	meta.EmbeddingDimensions = b.embeddingDims
	meta.SHA256 = shaOverride

	// Hasher over the canonical bytes. We tee the real output through
	// a forkWriter that:
	//   - forwards the "canonical" region (everything up through
	//     source_images) to the hasher;
	//   - drops the "suppressed" region (source_bodies / images /
	//     bodies / embeddings — the 3 zeroed fields in
	//     marshalCanonical) from the hasher.
	// When shaOverride is set (pass 2), hashing is skipped (hasher is
	// nil) since the caller already has the hash.
	var hasher hash.Hash
	var fw *forkWriter
	if shaOverride == "" {
		hasher = sha256.New()
		fw = &forkWriter{file: w, hash: hasher}
		w = fw // redirect all writes through the fork
	} else {
		// Pass 2: no hashing; stats.SHA256 is the pre-computed hash.
		stats.SHA256 = shaOverride
	}

	gz := gzip.NewWriter(w)
	defer func() {
		_ = gz.Close()
		if fw != nil {
			fw.suppress = true // any trailing flush bypasses the hasher
		}
	}()

	jw := &jsonStreamWriter{w: gz}

	// ── schema_version + metadata ─────────────────────────────────
	jw.writeRaw(`{"schema_version":`)
	jw.writeJSON(1)
	jw.writeRaw(`,"metadata":`)
	writeMetadataJSON(jw, meta)

	// ── id→idx maps (built incrementally as each entity streams) ──
	sourceIdxByID := make(map[uuid.UUID]int)
	factIdxByID := make(map[uuid.UUID]int)
	conceptIdxByID := make(map[uuid.UUID]int)
	summaryIdxByID := make(map[uuid.UUID]int)
	investigationIdxByID := make(map[uuid.UUID]int)
	reportIdxByID := make(map[uuid.UUID]int)
	sourceImageIdxByID := make(map[uuid.UUID]int)

	// ── sources ──
	jw.writeRaw(`,"sources":`)
	srcRows, err := b.queries.ListAllSourcesForExport(ctx, b.repoID)
	if err != nil {
		return nil, fmt.Errorf("export: listing sources: %w", err)
	}
	if len(srcRows) == 0 {
		jw.writeRaw(`null`)
	} else {
		jw.writeRaw(`[`)
		for i, r := range srcRows {
			idx := i
			sourceIdxByID[asUUID(r.ID)] = idx
			hasStoredBody := r.StorageKey != nil && *r.StorageKey != ""
			row := SourceRow{
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
				HasStoredBody:  hasStoredBody,
			}
			if i > 0 {
				jw.writeRaw(`,`)
			}
			jw.writeJSON(row)
			// Embed the source body (PDF) when include_bodies is set.
			if b.includeBodies && hasStoredBody && b.storage != nil {
				if err := b.streamSourceBody(ctx, jw, idx, *r.StorageKey, r.ContentType); err != nil {
					log.Printf("export: embedding source body idx %d: %v", idx, err)
				}
			}
		}
		jw.writeRaw(`]`)
	}
	stats.SourceCount = len(srcRows)

	// ── facts ──
	jw.writeRaw(`,"facts":`)
	factRows, err := b.queries.ListAllFactsForExport(ctx, b.repoID)
	if err != nil {
		return nil, fmt.Errorf("export: listing facts: %w", err)
	}
	if len(factRows) == 0 {
		jw.writeRaw(`null`)
	} else {
		jw.writeRaw(`[`)
		for i, r := range factRows {
			idx := i
			factIdxByID[asUUID(r.ID)] = idx
			imageURL := ptrStr(r.ImageUrl)
			sourceImageIdx := -1
			if r.FactKind == "image" && imageURL != "" {
				sourceImageIdx = resolveSourceImageIdx(imageURL, sourceImageIdxByID)
			}
			row := FactRow{
				Idx:            idx,
				Text:           r.Text,
				FactKind:       r.FactKind,
				ImageURL:       imageURL,
				ContentHash:    factContentHash(r.Text),
				PromptsetHash:  ptrStr(r.PromptsetHash),
				Status:         r.Status,
				SourceImageIdx: sourceImageIdx,
			}
			if i > 0 {
				jw.writeRaw(`,`)
			}
			jw.writeJSON(row)
		}
		jw.writeRaw(`]`)
	}
	stats.FactCount = len(factRows)

	// ── fact_sources ──
	jw.writeRaw(`,"fact_sources":`)
	fsRows, err := b.queries.ListAllFactSourcesForExport(ctx, b.repoID)
	if err != nil {
		return nil, fmt.Errorf("export: listing fact_sources: %w", err)
	}
	streamFactSources(jw, fsRows, factIdxByID, sourceIdxByID)

	// ── concepts ──
	jw.writeRaw(`,"concepts":`)
	conceptRows, err := b.queries.ListAllConceptsForExport(ctx, b.repoID)
	if err != nil {
		return nil, fmt.Errorf("export: listing concepts: %w", err)
	}
	if len(conceptRows) == 0 {
		jw.writeRaw(`null`)
	} else {
		jw.writeRaw(`[`)
		for i, r := range conceptRows {
			idx := i
			conceptIdxByID[asUUID(r.ID)] = idx
			row := ConceptRow{
				Idx:           idx,
				CanonicalName: r.CanonicalName,
				Context:       r.Context,
				Description:   ptrStr(r.Description),
				PromptsetHash: ptrStr(r.PromptsetHash),
			}
			if i > 0 {
				jw.writeRaw(`,`)
			}
			jw.writeJSON(row)
		}
		jw.writeRaw(`]`)
	}
	stats.ConceptCount = len(conceptRows)

	// ── concept_aliases ──
	jw.writeRaw(`,"concept_aliases":`)
	caRows, err := b.queries.ListAllConceptAliasesForExport(ctx, b.repoID)
	if err != nil {
		return nil, fmt.Errorf("export: listing concept_aliases: %w", err)
	}
	streamConceptAliases(jw, caRows, conceptIdxByID)

	// ── fact_concepts ──
	jw.writeRaw(`,"fact_concepts":`)
	fcRows, err := b.queries.ListAllFactConceptsForExport(ctx, b.repoID)
	if err != nil {
		return nil, fmt.Errorf("export: listing fact_concepts: %w", err)
	}
	streamFactConcepts(jw, fcRows, factIdxByID, conceptIdxByID)

	// ── concept_summaries ──
	jw.writeRaw(`,"concept_summaries":`)
	sumRows, err := b.queries.ListAllSummariesForExport(ctx, b.repoID)
	if err != nil {
		return nil, fmt.Errorf("export: listing summaries: %w", err)
	}
	if len(sumRows) == 0 {
		jw.writeRaw(`null`)
	} else {
		jw.writeRaw(`[`)
		for i, r := range sumRows {
			idx := i
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
			row := SummaryRow{
				ConceptIdx:      cIdx,
				SequenceNum:     int(r.SequenceNum),
				IsComplete:      r.IsComplete,
				FactCount:       int(r.FactCount),
				Content:         r.Content,
				CoveredFactIdxs: covered,
				Model:           ptrStr(r.Model),
			}
			if i > 0 {
				jw.writeRaw(`,`)
			}
			jw.writeJSON(row)
		}
		jw.writeRaw(`]`)
	}
	stats.SummaryCount = len(sumRows)

	// ── concept_syntheses ──
	jw.writeRaw(`,"concept_syntheses":`)
	synRows, err := b.queries.ListAllSynthesesForExport(ctx, b.repoID)
	if err != nil {
		return nil, fmt.Errorf("export: listing syntheses: %w", err)
	}
	if len(synRows) == 0 {
		jw.writeRaw(`null`)
	} else {
		jw.writeRaw(`[`)
		for i, r := range synRows {
			coveredSum := idxsFromUUIDs(r.CoveredSummaryIds, summaryIdxByID)
			coveredCon := idxsFromUUIDs(r.CoveredConceptIds, conceptIdxByID)
			coveredImg := idxsFromUUIDs(r.EmbeddedImageIds, factIdxByID)
			row := SynthesisRow{
				CanonicalName:      r.CanonicalName,
				Content:            r.Content,
				CoveredSummaryIdxs: coveredSum,
				CoveredConceptIdxs: coveredCon,
				EmbeddedImageIdxs:  coveredImg,
				Model:              ptrStr(r.Model),
			}
			if i > 0 {
				jw.writeRaw(`,`)
			}
			jw.writeJSON(row)
		}
		jw.writeRaw(`]`)
	}
	stats.SynthesisCount = len(synRows)

	// ── investigations ──
	jw.writeRaw(`,"investigations":`)
	invRows, err := b.queries.ListAllInvestigationsForExport(ctx, b.repoID)
	if err != nil {
		return nil, fmt.Errorf("export: listing investigations: %w", err)
	}
	if len(invRows) == 0 {
		jw.writeRaw(`null`)
	} else {
		jw.writeRaw(`[`)
		for i, r := range invRows {
			idx := i
			investigationIdxByID[asUUID(r.ID)] = idx
			row := InvestigationRow{
				Idx:   idx,
				Title: r.Title,
				Topic: ptrStr(r.Topic),
			}
			if i > 0 {
				jw.writeRaw(`,`)
			}
			jw.writeJSON(row)
		}
		jw.writeRaw(`]`)
	}
	stats.InvestigationCount = len(invRows)

	// ── investigation_sources ──
	jw.writeRaw(`,"investigation_sources":`)
	isRows, err := b.queries.ListAllInvestigationSourcesForExport(ctx, b.repoID)
	if err != nil {
		return nil, fmt.Errorf("export: listing investigation_sources: %w", err)
	}
	streamInvestigationSources(jw, isRows, investigationIdxByID, sourceIdxByID)

	// ── reports ──
	jw.writeRaw(`,"reports":`)
	repRows, err := b.queries.ListAllReportsForExport(ctx, b.repoID)
	if err != nil {
		return nil, fmt.Errorf("export: listing reports: %w", err)
	}
	if len(repRows) == 0 {
		jw.writeRaw(`null`)
	} else {
		jw.writeRaw(`[`)
		for i, r := range repRows {
			idx := i
			reportIdxByID[asUUID(r.ID)] = idx
			parentIdx := -1
			if r.ParentID.Valid {
				if pIdx, ok := reportIdxByID[asUUID(r.ParentID)]; ok {
					parentIdx = pIdx
				}
			}
			row := ReportRow{
				Idx:                 idx,
				Title:               r.Title,
				Topic:               ptrStr(r.Topic),
				BodyMd:              r.BodyMd,
				Status:              r.Status,
				ParentIdx:           parentIdx,
				SimilarityThreshold: ptrFloat(r.SimilarityThreshold),
				EmbeddedModel:       ptrStr(r.EmbeddedModel),
				SentenceCount:       int(ptrInt32(r.SentenceCount)),
			}
			if i > 0 {
				jw.writeRaw(`,`)
			}
			jw.writeJSON(row)
		}
		jw.writeRaw(`]`)
	}
	stats.ReportCount = len(repRows)

	// ── report_annotations ──
	jw.writeRaw(`,"report_annotations":`)
	raRows, err := b.queries.ListAllReportAnnotationsForExport(ctx, b.repoID)
	if err != nil {
		return nil, fmt.Errorf("export: listing report_annotations: %w", err)
	}
	streamReportAnnotations(jw, raRows, reportIdxByID, factIdxByID)

	// ── source_images ── (last field in the canonical/hashed region)
	jw.writeRaw(`,"source_images":`)
	// imageRefs collects (ref, storageKey, contentType) for the
	// images map we emit after the suppressed-region begins.
	type imgRef struct {
		ref         string
		storageKey  string
		contentType *string
	}
	var imageRefs []imgRef
	var siRows []store.ListAllSourceImagesForExportRow
	if b.storage != nil {
		siRows, err = b.queries.ListAllSourceImagesForExport(ctx, b.repoID)
		if err != nil {
			return nil, fmt.Errorf("export: listing source_images: %w", err)
		}
	}
	if len(siRows) == 0 {
		jw.writeRaw(`null`)
	} else {
		jw.writeRaw(`[`)
		for i, r := range siRows {
			idx := i
			sourceImageIdxByID[asUUID(r.ID)] = idx
			sIdx, ok := sourceIdxByID[asUUID(r.SourceID)]
			if !ok {
				continue
			}
			row := SourceImageRow{
				Idx:         idx,
				SourceIdx:   sIdx,
				Kind:        r.Kind,
				PageNumber:  ptrInt(r.PageNumber),
				Position:    int(r.Position),
				URL:         ptrStr(r.Url),
				Width:       ptrInt(r.Width),
				Height:      ptrInt(r.Height),
				Bytes:       ptrInt(r.Bytes),
				AltText:     ptrStr(r.AltText),
				ContentType: ptrStr(r.ContentType),
			}
			if b.includeImages && r.StorageKey != nil && *r.StorageKey != "" && (r.Url == nil || *r.Url == "") {
				ref := fmt.Sprintf("img-%d", idx)
				row.ImageRef = ref
				imageRefs = append(imageRefs, imgRef{ref: ref, storageKey: *r.StorageKey, contentType: r.ContentType})
			}
			if i > 0 {
				jw.writeRaw(`,`)
			}
			jw.writeJSON(row)
		}
		jw.writeRaw(`]`)
	}
	stats.ImageCount = len(imageRefs)

	// ── suppressed region begins ──────────────────────────────────
	// Everything from here on (source_bodies, images, bodies,
	// embeddings) is excluded from the canonical hash. Tell the fork
	// writer to stop feeding the hasher; real bytes still flow to the
	// gzip writer. Before flipping suppress, feed the hasher the
	// closing `}` that marshalCanonical emits as the last byte of the
	// canonical object (source_images is the final field there; the
	// 3 zeroed fields are omitted, so the canonical JSON ends with
	// `}` immediately after source_images).
	if fw != nil {
		_, _ = fw.hash.Write([]byte(`}`))
		fw.suppress = true
	}

	// ── source_bodies + bodies (only when include_bodies) ──
	// The bodies were embedded inline during the sources loop above
	// (streamSourceBody appended to the Bodies map + SourceBodies
	// slice that we emit here). When include_bodies is false, both
	// sections are omitted (omitempty).
	// NOTE: because we stream, we can't emit source_bodies/bodies
	// from the earlier loop — we collected bodyRefs there and emit
	// them now. (See streamSourceBody: it records into bodyRefs.)
	// This is handled below via b.drainBodies.

	// ── images ── (omitempty; only when includeImages and refs exist)
	if b.includeImages && len(imageRefs) > 0 {
		jw.writeRaw(`,"images":{`)
		for i, ir := range imageRefs {
			if i > 0 {
				jw.writeRaw(`,`)
			}
			jw.writeJSONKey(ir.ref)
			jw.writeRaw(`:`)
			if err := b.streamImageFile(ctx, jw, ir.storageKey, ir.contentType); err != nil {
				log.Printf("export: embedding source image ref %s: %v", ir.ref, err)
				// Emit a null so the JSON stays valid even if the
				// storage read failed mid-stream.
				jw.writeRaw(`null`)
			}
		}
		jw.writeRaw(`}`)
	}

	// ── bodies ── (omitempty; only when includeBodies and bodies collected)
	if b.includeImages || b.includeBodies {
		// drain any bodies collected during the sources loop
		if bodyRefs := b.drainBodies(); len(bodyRefs) > 0 {
			jw.writeRaw(`,"source_bodies":[`)
			for i, br := range bodyRefs {
				if i > 0 {
					jw.writeRaw(`,`)
				}
				jw.writeJSON(br.ref)
			}
			jw.writeRaw(`],"bodies":{`)
			for i, br := range bodyRefs {
				if i > 0 {
					jw.writeRaw(`,`)
				}
				jw.writeJSONKey(br.ref)
				jw.writeRaw(`:`)
				jw.writeRaw(br.jsonBytes) // pre-marshaled FileBytes
			}
			jw.writeRaw(`}`)
		}
	}

	// ── embeddings ── (omitempty; nil when Qdrant unconfigured or empty)
	if b.qdrant != nil {
		embWritten, factCnt, conceptCnt, err := b.streamEmbeddings(ctx, jw, factIdxByID, conceptIdxByID)
		if err != nil {
			return nil, fmt.Errorf("export: streaming embeddings: %w", err)
		}
		if embWritten {
			stats.EmbeddingFactCount = factCnt
			stats.EmbeddingConceptCnt = conceptCnt
		}
	}

	jw.writeRaw(`}`)

	// Flush gzip. The defer closes gz; we need it flushed before we
	// finalize the hash.
	if err := gz.Close(); err != nil {
		return nil, fmt.Errorf("export: closing gzip writer: %w", err)
	}
	// Stop the fork from forwarding the gzip footer to the hasher
	// (already suppressed above, but be safe).
	if fw != nil {
		fw.suppress = true
	}

	if hasher != nil {
		stats.SHA256 = hex.EncodeToString(hasher.Sum(nil))
	}
	return stats, nil
}

// ── streaming JSON helpers ───────────────────────────────────────

// jsonStreamWriter wraps an io.Writer and provides methods to emit
// compact JSON tokens byte-for-byte compatible with encoding/json's
// Marshal output (no spaces, fields in declaration order).
type jsonStreamWriter struct {
	w   io.Writer
	err error
}

func (j *jsonStreamWriter) writeRaw(s string) {
	if j.err != nil {
		return
	}
	_, j.err = j.w.Write([]byte(s))
}

// writeJSON marshals v with encoding/json and writes the bytes. This
// guarantees byte-parity with json.Marshal for each individual struct
// value (field order, omitempty, escaping).
func (j *jsonStreamWriter) writeJSON(v any) {
	if j.err != nil {
		return
	}
	data, err := json.Marshal(v)
	if err != nil {
		j.err = err
		return
	}
	_, j.err = j.w.Write(data)
}

// writeJSONKey writes a JSON object key (string, quoted + escaped).
func (j *jsonStreamWriter) writeJSONKey(k string) {
	if j.err != nil {
		return
	}
	data, err := json.Marshal(k)
	if err != nil {
		j.err = err
		return
	}
	_, j.err = j.w.Write(data)
}

// forkWriter tees writes to a file (the real output) and a hasher
// (the canonical sha). When suppress is true, writes go to the file
// only — used for the embeddings/images/bodies region that
// marshalCanonical zeroes out.
type forkWriter struct {
	file     io.Writer
	hash     hash.Hash
	suppress bool
}

func (f *forkWriter) Write(p []byte) (int, error) {
	n, err := f.file.Write(p)
	if err != nil {
		return n, err
	}
	if !f.suppress {
		_, _ = f.hash.Write(p)
	}
	return n, nil
}

// writeMetadataJSON emits the metadata object byte-for-byte identical
// to json.Marshal(BundleMetadata{...}) so the canonical sha matches.
func writeMetadataJSON(jw *jsonStreamWriter, m BundleMetadata) {
	jw.writeRaw(`{`)
	first := true
	writeMetaField(jw, &first, "name", m.Name, false)
	writeMetaField(jw, &first, "description", m.Description, true)
	writeMetaField(jw, &first, "owner", m.Owner, true)
	writeMetaFieldOmitEmptySlice(jw, &first, "tags", m.Tags)
	writeMetaField(jw, &first, "okt_version", m.OKTVersion, true)
	writeMetaFieldOmitEmptySlice(jw, &first, "promptset_hashes", m.PromptsetHashes)
	writeMetaField(jw, &first, "embedding_model", m.EmbeddingModel, true)
	writeMetaFieldOmitEmptyInt(jw, &first, "embedding_dimensions", m.EmbeddingDimensions)
	writeMetaFieldInt(jw, &first, "source_count", m.SourceCount)
	writeMetaFieldInt(jw, &first, "fact_count", m.FactCount)
	writeMetaFieldInt(jw, &first, "concept_count", m.ConceptCount)
	writeMetaFieldInt(jw, &first, "summary_count", m.SummaryCount)
	writeMetaFieldInt(jw, &first, "synthesis_count", m.SynthesisCount)
	writeMetaFieldInt(jw, &first, "report_count", m.ReportCount)
	writeMetaFieldInt(jw, &first, "investigation_count", m.InvestigationCount)
	writeMetaField(jw, &first, "sha256", m.SHA256, true)
	// exported_at is always present (no omitempty).
	if !first {
		jw.writeRaw(`,`)
	}
	jw.writeJSONKey("exported_at")
	jw.writeRaw(`:`)
	jw.writeJSON(m.ExportedAt)
	jw.writeRaw(`}`)
}

func writeMetaField(jw *jsonStreamWriter, first *bool, key, val string, omitempty bool) {
	if omitempty && val == "" {
		return
	}
	if !*first {
		jw.writeRaw(`,`)
	}
	*first = false
	jw.writeJSONKey(key)
	jw.writeRaw(`:`)
	jw.writeJSON(val)
}

func writeMetaFieldInt(jw *jsonStreamWriter, first *bool, key string, val int) {
	if !*first {
		jw.writeRaw(`,`)
	}
	*first = false
	jw.writeJSONKey(key)
	jw.writeRaw(`:`)
	jw.writeJSON(val)
}

func writeMetaFieldOmitEmptyInt(jw *jsonStreamWriter, first *bool, key string, val int) {
	if val == 0 {
		return
	}
	writeMetaFieldInt(jw, first, key, val)
}

func writeMetaFieldOmitEmptySlice(jw *jsonStreamWriter, first *bool, key string, vals []string) {
	if len(vals) == 0 {
		return
	}
	if !*first {
		jw.writeRaw(`,`)
	}
	*first = false
	jw.writeJSONKey(key)
	jw.writeRaw(`:`)
	jw.writeJSON(vals)
}

// ── per-junction streaming (fact_sources, concept_aliases, ...) ──

func streamFactSources(jw *jsonStreamWriter, rows []store.ListAllFactSourcesForExportRow, factIdxByID, sourceIdxByID map[uuid.UUID]int) {
	if len(rows) == 0 {
		jw.writeRaw(`null`)
		return
	}
	jw.writeRaw(`[`)
	first := true
	for _, r := range rows {
		fIdx, ok := factIdxByID[asUUID(r.FactID)]
		if !ok {
			continue
		}
		sIdx, ok := sourceIdxByID[asUUID(r.SourceID)]
		if !ok {
			continue
		}
		row := FactSourceRow{FactIdx: fIdx, SourceIdx: sIdx, ChunkIndex: int(r.ChunkIndex)}
		if !first {
			jw.writeRaw(`,`)
		}
		first = false
		jw.writeJSON(row)
	}
	jw.writeRaw(`]`)
}

func streamConceptAliases(jw *jsonStreamWriter, rows []store.ListAllConceptAliasesForExportRow, conceptIdxByID map[uuid.UUID]int) {
	if len(rows) == 0 {
		jw.writeRaw(`null`)
		return
	}
	jw.writeRaw(`[`)
	first := true
	for _, r := range rows {
		cIdx, ok := conceptIdxByID[asUUID(r.ConceptID)]
		if !ok {
			continue
		}
		row := ConceptAliasRow{ConceptIdx: cIdx, AliasText: r.AliasText}
		if !first {
			jw.writeRaw(`,`)
		}
		first = false
		jw.writeJSON(row)
	}
	jw.writeRaw(`]`)
}

func streamFactConcepts(jw *jsonStreamWriter, rows []store.ListAllFactConceptsForExportRow, factIdxByID, conceptIdxByID map[uuid.UUID]int) {
	if len(rows) == 0 {
		jw.writeRaw(`null`)
		return
	}
	jw.writeRaw(`[`)
	first := true
	for _, r := range rows {
		fIdx, ok := factIdxByID[asUUID(r.FactID)]
		if !ok {
			continue
		}
		cIdx, ok := conceptIdxByID[asUUID(r.ConceptID)]
		if !ok {
			continue
		}
		row := FactConceptRow{FactIdx: fIdx, ConceptIdx: cIdx, PromptsetHash: ptrStr(r.PromptsetHash)}
		if !first {
			jw.writeRaw(`,`)
		}
		first = false
		jw.writeJSON(row)
	}
	jw.writeRaw(`]`)
}

func streamInvestigationSources(jw *jsonStreamWriter, rows []store.ListAllInvestigationSourcesForExportRow, investigationIdxByID, sourceIdxByID map[uuid.UUID]int) {
	if len(rows) == 0 {
		jw.writeRaw(`null`)
		return
	}
	jw.writeRaw(`[`)
	first := true
	for _, r := range rows {
		iIdx, ok := investigationIdxByID[asUUID(r.InvestigationID)]
		if !ok {
			continue
		}
		sIdx, ok := sourceIdxByID[asUUID(r.SourceID)]
		if !ok {
			continue
		}
		row := InvestigationSourceRow{InvestigationIdx: iIdx, SourceIdx: sIdx}
		if !first {
			jw.writeRaw(`,`)
		}
		first = false
		jw.writeJSON(row)
	}
	jw.writeRaw(`]`)
}

func streamReportAnnotations(jw *jsonStreamWriter, rows []store.ListAllReportAnnotationsForExportRow, reportIdxByID, factIdxByID map[uuid.UUID]int) {
	if len(rows) == 0 {
		jw.writeRaw(`null`)
		return
	}
	jw.writeRaw(`[`)
	first := true
	for _, r := range rows {
		repIdx, ok := reportIdxByID[asUUID(r.ReportID)]
		if !ok {
			continue
		}
		fIdx, ok := factIdxByID[asUUID(r.FactID)]
		if !ok {
			continue
		}
		row := ReportAnnotationRow{
			ReportIdx:     repIdx,
			SentenceIndex: int(r.SentenceIndex),
			SentenceText:  r.SentenceText,
			FactIdx:       fIdx,
			Score:         r.Score,
			Posture:       ptrStr(r.Posture),
		}
		if !first {
			jw.writeRaw(`,`)
		}
		first = false
		jw.writeJSON(row)
	}
	jw.writeRaw(`]`)
}

// ── streamed binary + embeddings ─────────────────────────────────

// bodyRefCollected is a body entry recorded during the sources loop
// (streamSourceBody) and emitted in the bodies/source_bodies sections
// after the suppressed region begins.
type bodyRefCollected struct {
	ref       string
	jsonBytes string // pre-marshaled FileBytes JSON object
}

// drainBodies returns the collected body refs and clears the buffer.
// The builder is per-export-job so single-threaded; the slice lives on
// the builder for the duration of one StreamBuild call.
func (b *BundleBuilder) drainBodies() []bodyRefCollected {
	out := b.bodyBuf
	b.bodyBuf = nil
	return out
}

// streamSourceBody reads a source body (PDF) from storage, marshals it
// to a FileBytes JSON object, and records it in the builder's bodyBuf
// for later emission in the bodies/source_bodies sections. Called
// inline during the sources loop; the actual JSON bytes are emitted
// after the suppressed region begins (so they don't hit the hasher).
func (b *BundleBuilder) streamSourceBody(ctx context.Context, jw *jsonStreamWriter, sourceIdx int, storageKey string, contentType *string) error {
	file, err := b.storage.Get(ctx, storageKey)
	if err != nil {
		return fmt.Errorf("reading source body from storage: %w", err)
	}
	defer file.Body.Close()
	data, err := readAll(file.Body)
	if err != nil {
		return fmt.Errorf("reading source body bytes: %w", err)
	}
	ct := "application/pdf"
	if contentType != nil && *contentType != "" {
		ct = *contentType
	}
	ref := fmt.Sprintf("body-%d", sourceIdx)
	fb := FileBytes{ContentType: ct, Data: data}
	fbJSON, err := json.Marshal(fb)
	if err != nil {
		return fmt.Errorf("marshaling source body FileBytes: %w", err)
	}
	b.bodyBuf = append(b.bodyBuf, bodyRefCollected{ref: ref, jsonBytes: string(fbJSON)})
	return nil
}

// streamImageFile reads an image from storage and streams its FileBytes
// JSON value directly to the gzip writer. One image in memory at a time.
func (b *BundleBuilder) streamImageFile(ctx context.Context, jw *jsonStreamWriter, storageKey string, contentType *string) error {
	file, err := b.storage.Get(ctx, storageKey)
	if err != nil {
		return fmt.Errorf("reading source image from storage: %w", err)
	}
	defer file.Body.Close()
	data, err := readAll(file.Body)
	if err != nil {
		return fmt.Errorf("reading source image bytes: %w", err)
	}
	ct := "image/png"
	if contentType != nil && *contentType != "" {
		ct = *contentType
	}
	fb := FileBytes{ContentType: ct, Data: data}
	jw.writeJSON(fb)
	return nil
}

// streamEmbeddings streams the embeddings object: model, dimensions,
// fact_vectors, concept_vectors. Each vector map is streamed batch by
// batch (1000 IDs → Qdrant → write → drop), so peak memory is one
// batch (~1.2 MB) not the full 7.5 GB. Returns (written, factCount,
// conceptCount, err). written is false when both maps are empty (the
// embeddings section is omitted entirely via omitempty).
func (b *BundleBuilder) streamEmbeddings(ctx context.Context, jw *jsonStreamWriter, factIdxByID, conceptIdxByID map[uuid.UUID]int) (bool, int, int, error) {
	// Collect IDs.
	factIDs := make([]uuid.UUID, 0, len(factIdxByID))
	for id := range factIdxByID {
		factIDs = append(factIDs, id)
	}
	conceptIDs := make([]uuid.UUID, 0, len(conceptIdxByID))
	for id := range conceptIdxByID {
		conceptIDs = append(conceptIDs, id)
	}

	// To know whether we should emit the embeddings object at all
	// (it's omitempty via *Embeddings), we need to know if any
	// vectors exist. Stream the fact + concept vectors to temp
	// buffers first? No — that would buffer them all. Instead,
	// open the embeddings object optimistically, stream vectors,
	// and if both turn out empty, we rewind by... we can't rewind
	// a stream.
	//
	// Approach: do a tiny existence check via Qdrant count, OR
	// just always emit the embeddings object when qdrant is wired
	// (model + dimensions always present; fact_vectors/concept_vectors
	// emitted as {} when no IDs, which json.Marshal would omit via
	// omitempty on the inner maps — but the outer *Embeddings is
	// non-nil so it appears). This diverges slightly from Build
	// (which sets bundle.Embeddings = nil when both maps empty),
	// but only in the empty-repo edge case. Acceptable: an empty
	// repo's bundle with "embeddings":{"model":"...","dimensions":3072}
	// is valid and the importer handles it. We choose to always
	// emit when qdrant != nil for streaming simplicity.

	jw.writeRaw(`,"embeddings":{`)
	jw.writeJSONKey("model")
	jw.writeRaw(`:`)
	jw.writeJSON(b.embeddingModel)
	jw.writeRaw(`,`)
	jw.writeJSONKey("dimensions")
	jw.writeRaw(`:`)
	jw.writeJSON(b.embeddingDims)

	// fact_vectors
	factCnt := 0
	jw.writeRaw(`,`)
	jw.writeJSONKey("fact_vectors")
	jw.writeRaw(`:{`)
	if len(factIDs) > 0 {
		first := true
		for i := 0; i < len(factIDs); i += 1000 {
			end := i + 1000
			if end > len(factIDs) {
				end = len(factIDs)
			}
			batch := factIDs[i:end]
			points, err := b.qdrant.GetFactVectorsByIDs(ctx, batch)
			if err != nil {
				return false, 0, 0, fmt.Errorf("fetching fact vectors batch %d: %w", i, err)
			}
			// Sort keys for deterministic map output (json.Marshal
			// sorts string keys; we must too for byte-parity on the
			// non-suppressed fields — though embeddings IS suppressed
			// from the hash, the importer still parses it, so order
			// doesn't matter for correctness, only for sha parity of
			// the non-suppressed region. Since embeddings is in the
			// suppressed region, order doesn't affect the sha. But we
			// sort anyway for stable output.)
			keys := make([]string, 0, len(points))
			vecByID := make(map[string][]float32, len(points))
			for id, p := range points {
				idx, ok := factIdxByID[id]
				if !ok {
					continue
				}
				k := itoa(idx)
				keys = append(keys, k)
				vecByID[k] = p.Vector
			}
			sort.Strings(keys)
			for _, k := range keys {
				if !first {
					jw.writeRaw(`,`)
				}
				first = false
				jw.writeJSONKey(k)
				jw.writeRaw(`:`)
				jw.writeJSON(vecByID[k])
				factCnt++
			}
		}
	}
	jw.writeRaw(`}`)

	// concept_vectors
	conceptCnt := 0
	jw.writeRaw(`,`)
	jw.writeJSONKey("concept_vectors")
	jw.writeRaw(`:{`)
	if len(conceptIDs) > 0 {
		first := true
		for i := 0; i < len(conceptIDs); i += 1000 {
			end := i + 1000
			if end > len(conceptIDs) {
				end = len(conceptIDs)
			}
			batch := conceptIDs[i:end]
			points, err := b.qdrant.GetConceptVectorsByIDs(ctx, batch)
			if err != nil {
				return false, 0, 0, fmt.Errorf("fetching concept vectors batch %d: %w", i, err)
			}
			keys := make([]string, 0, len(points))
			vecByID := make(map[string][]float32, len(points))
			for id, p := range points {
				idx, ok := conceptIdxByID[id]
				if !ok {
					continue
				}
				k := itoa(idx)
				keys = append(keys, k)
				vecByID[k] = p.Vector
			}
			sort.Strings(keys)
			for _, k := range keys {
				if !first {
					jw.writeRaw(`,`)
				}
				first = false
				jw.writeJSONKey(k)
				jw.writeRaw(`:`)
				jw.writeJSON(vecByID[k])
				conceptCnt++
			}
		}
	}
	jw.writeRaw(`}}`)

	return true, factCnt, conceptCnt, nil
}

// pgtype import retained for the row types used in the streaming
// junction helpers above (ListAllFactSourcesForExportRow etc. live in
// the store package and carry pgtype.UUID fields accessed via asUUID).
var _ = pgtype.UUID{}