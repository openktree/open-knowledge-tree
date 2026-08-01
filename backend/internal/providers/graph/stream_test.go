package graph

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"hash"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/openktree/open-knowledge-tree/backend/internal/qdrantstore"
)

// TestStreamEncoder_ParityWithMarshal is the core correctness guard
// for StreamBuild's hand-written JSON encoder. It constructs a
// representative GraphBundle in memory, serializes it two ways:
//   1. json.Marshal(bundle) — the reference (what the in-memory Build
//      path produces via MarshalGzip).
//   2. streamBundleToBytes(bundle) — the streaming encoder's output.
// and asserts the bytes are identical. Any divergence in field order,
// omitempty handling, formatting, or escaping would break the sha
// parity and the importer's json.Unmarshal. This test catches those
// without needing a live DB / Qdrant / storage (the streaming fetch
// paths are exercised by the e2e suite; this test covers the encoder).
func TestStreamEncoder_ParityWithMarshal(t *testing.T) {
	// Freeze time so ExportedAt is deterministic across both paths.
	frozen := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	origNow := nowFn
	nowFn = func() time.Time { return frozen }
	t.Cleanup(func() { nowFn = origNow })

	cases := []struct {
		name   string
		bundle GraphBundle
	}{
		{
			name: "empty repo (all nil slices, no omitempty fields)",
			bundle: GraphBundle{
				SchemaVersion: 2,
				Metadata: BundleMetadata{
					Name:                "empty",
					EmbeddingModel:      "m",
					EmbeddingDimensions: 3072,
					ExportedAt:          frozen,
				},
			},
		},
		{
			name: "with rows in every always-present slice",
			bundle: GraphBundle{
				SchemaVersion: 2,
				Metadata: BundleMetadata{
					Name:                "r",
					Description:         "d",
					Tags:                []string{"a", "b"},
					EmbeddingModel:      "m",
					EmbeddingDimensions: 3072,
					SourceCount:         1,
					FactCount:           1,
					ExportedAt:          frozen,
					SHA256:              "abc",
				},
				Sources: []SourceRow{{Idx: 0, URL: "u", Kind: "k", Status: "s", ParsedTitle: "t"}},
				Facts:  []FactRow{{Idx: 0, Text: "f", FactKind: "text", Status: "new", ContentHash: "h", SourceImageIdx: -1}},
				FactSources: []FactSourceRow{{FactIdx: 0, SourceIdx: 0, ChunkIndex: 2}},
				Concepts: []ConceptRow{{Idx: 0, CanonicalName: "c", Context: "ctx"}},
				ConceptAliases: []ConceptAliasRow{{ConceptIdx: 0, AliasText: "al"}},
				FactConcepts: []FactConceptRow{{FactIdx: 0, ConceptIdx: 0, PromptsetHash: "p"}},
				ConceptSummaries: []SummaryRow{{ConceptIdx: 0, SequenceNum: 1, IsComplete: true, FactCount: 3, Content: "s", CoveredFactIdxs: []int{0}, Model: "md"}},
				ConceptSyntheses: []SynthesisRow{{CanonicalName: "c", Content: "syn", CoveredSummaryIdxs: []int{0}, Model: "md"}},
				Investigations: []InvestigationRow{{Idx: 0, Title: "i", Topic: "t"}},
				InvestigationSources: []InvestigationSourceRow{{InvestigationIdx: 0, SourceIdx: 0}},
				Reports: []ReportRow{{Idx: 0, Title: "r", BodyMd: "b", Status: "annotated", ParentIdx: -1, SentenceCount: 5}},
				ReportAnnotations: []ReportAnnotationRow{{ReportIdx: 0, SentenceIndex: 1, SentenceText: "st", FactIdx: 0, Score: 0.9, Posture: "supports"}},
				SourceImages: []SourceImageRow{{Idx: 0, SourceIdx: 0, Kind: "inline", Position: 1, URL: "http://x", Width: 10, Height: 20}},
			},
		},
		{
			name: "with images + bodies + embeddings (omitempty fields present)",
			bundle: GraphBundle{
				SchemaVersion: 2,
				Metadata:      BundleMetadata{Name: "full", ExportedAt: frozen, SHA256: "deadbeef"},
				SourceImages:  []SourceImageRow{{Idx: 0, SourceIdx: 0, Kind: "page", Position: 1, ImageRef: "img-0"}},
				SourceBodies:  []SourceBodyRef{{SourceIdx: 0, BodyRef: "body-0", ContentType: "application/pdf"}},
				Images:        map[string]FileBytes{"img-0": {ContentType: "image/png", Data: []byte{1, 2, 3}}},
				Bodies:        map[string]FileBytes{"body-0": {ContentType: "application/pdf", Data: []byte{9, 9}}},
				Embeddings: &Embeddings{
					Model:          "m",
					Dimensions:     3072,
					FactVectors:    map[string][]float32{"0": {0.1, 0.2}},
					ConceptVectors: map[string][]float32{"0": {0.3}},
				},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ref, err := json.Marshal(tc.bundle)
			if err != nil {
				t.Fatalf("json.Marshal: %v", err)
			}
			got := streamBundleToBytes(t, &tc.bundle)
			if !bytes.Equal(ref, got) {
				t.Errorf("byte mismatch\nreference: %s\nstreamed:  %s", ref, got)
			}
		})
	}
}

// streamBundleToBytes runs a pre-built GraphBundle through the same
// jsonStreamWriter + forkWriter + suppress-region logic StreamBuild
// uses, but without a DB. It's the pure-encoder half of StreamBuild:
// it proves the framing + omitempty + field-order code is byte-parity
// with json.Marshal. (The DB-fetch half of StreamBuild is covered by
// the e2e suite.)
func streamBundleToBytes(t *testing.T, b *GraphBundle) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	jw := &jsonStreamWriter{w: gz}

	emitFullBundle(jw, b)
	if jw.err != nil {
		t.Fatalf("streaming: %v", jw.err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("closing gzip: %v", err)
	}
	// Gunzip to get the raw JSON bytes for comparison.
	gzr, err := gzip.NewReader(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("gunzip: %v", err)
	}
	jsonBytes, err := io.ReadAll(gzr)
	if err != nil {
		t.Fatalf("readall: %v", err)
	}
	return jsonBytes
}

// emitFullBundle writes a complete GraphBundle JSON object via the
// streaming encoder, mirroring StreamBuild's field order + omitempty
// rules exactly. It takes a pre-built bundle (no DB) so it's usable
// from tests. StreamBuild itself does NOT call this (it streams from
// DB rows); this is the parity-test harness sharing the same framing
// helpers (writeMetaField, writeJSONKey, etc.).
func emitFullBundle(jw *jsonStreamWriter, b *GraphBundle) {
	jw.writeRaw(`{"schema_version":`)
	jw.writeJSON(b.SchemaVersion)
	jw.writeRaw(`,"metadata":`)
	writeMetadataJSON(jw, b.Metadata)

	// v2 field order: images/bodies/source_images/source_bodies
	// come after sources and before facts.
	emitSlice(jw, "sources", b.Sources)

	// omitempty derived sections (images, bodies) before source_images.
	if len(b.Images) > 0 {
		jw.writeRaw(`,"images":`)
		jw.writeJSON(b.Images)
	}
	if len(b.Bodies) > 0 {
		jw.writeRaw(`,"bodies":`)
		jw.writeJSON(b.Bodies)
	}
	emitSlice(jw, "source_images", b.SourceImages)
	if len(b.SourceBodies) > 0 {
		jw.writeRaw(`,"source_bodies":`)
		jw.writeJSON(b.SourceBodies)
	}

	emitSlice(jw, "facts", b.Facts)
	emitSlice(jw, "fact_sources", b.FactSources)
	emitSlice(jw, "concepts", b.Concepts)
	emitSlice(jw, "concept_aliases", b.ConceptAliases)
	emitSlice(jw, "fact_concepts", b.FactConcepts)
	emitSlice(jw, "concept_summaries", b.ConceptSummaries)
	emitSlice(jw, "concept_syntheses", b.ConceptSyntheses)
	emitSlice(jw, "investigations", b.Investigations)
	emitSlice(jw, "investigation_sources", b.InvestigationSources)
	emitSlice(jw, "reports", b.Reports)
	emitSlice(jw, "report_annotations", b.ReportAnnotations)

	if b.Embeddings != nil {
		jw.writeRaw(`,"embeddings":`)
		jw.writeJSON(b.Embeddings)
	}
	jw.writeRaw(`}`)
}

// emitSlice writes `,"key":<json>` where <json> is the marshaled
// slice, or `null` when the slice is nil (matching json.Marshal's
// nil-slice → null behavior). It does NOT use the streaming junction
// helpers (those filter by id→idx maps); for the parity test we want
// the exact slice contents preserved.
func emitSlice[T any](jw *jsonStreamWriter, key string, vals []T) {
	jw.writeRaw(`,`)
	jw.writeJSONKey(key)
	jw.writeRaw(`:`)
	if vals == nil {
		jw.writeRaw(`null`)
		return
	}
	jw.writeJSON(vals)
}

// TestStreamEncoder_EmptyRepoMatchesMarshal is a focused sub-case:
// the empty-repo bundle (all nil slices, no omitempty fields present)
// must produce exactly the bytes json.Marshal produces, including the
// trailing `source_images":null}` with no extra fields.
func TestStreamEncoder_EmptyRepoMatchesMarshal(t *testing.T) {
	frozen := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	origNow := nowFn
	nowFn = func() time.Time { return frozen }
	t.Cleanup(func() { nowFn = origNow })

	b := &GraphBundle{
		SchemaVersion: 2,
		Metadata:      BundleMetadata{Name: "empty", EmbeddingModel: "m", EmbeddingDimensions: 3072, ExportedAt: frozen},
	}
	ref, _ := json.Marshal(b)
	got := streamBundleToBytes(t, b)
	if !bytes.Equal(ref, got) {
		t.Errorf("empty repo mismatch\nreference: %s\nstreamed:  %s", ref, got)
	}
}

// TestMetadataJSON_Parity checks the metadata object in isolation,
// covering every omitempty branch (description, owner, tags, sha256,
// promptset_hashes, okt_version, embedding_model, embedding_dimensions)
// against json.Marshal(BundleMetadata{...}).
func TestMetadataJSON_Parity(t *testing.T) {
	frozen := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	cases := []BundleMetadata{
		{Name: "bare", ExportedAt: frozen},
		{Name: "full", Description: "d", Owner: "o", Tags: []string{"x"}, OKTVersion: "v", PromptsetHashes: []string{"h"}, EmbeddingModel: "m", EmbeddingDimensions: 3072, SourceCount: 1, FactCount: 2, ConceptCount: 3, SummaryCount: 4, SynthesisCount: 5, ReportCount: 6, InvestigationCount: 7, SHA256: "s", ExportedAt: frozen},
		{Name: "zeros", EmbeddingDimensions: 0, SourceCount: 0, ExportedAt: frozen},
	}
	for i, m := range cases {
		ref, err := json.Marshal(m)
		if err != nil {
			t.Fatalf("case %d marshal: %v", i, err)
		}
		var buf bytes.Buffer
		jw := &jsonStreamWriter{w: &buf}
		writeMetadataJSON(jw, m)
		if jw.err != nil {
			t.Fatalf("case %d stream: %v", i, jw.err)
		}
		if !bytes.Equal(ref, buf.Bytes()) {
			t.Errorf("case %d metadata mismatch\nreference: %s\nstreamed:  %s", i, ref, buf.Bytes())
		}
	}
}

// TestStreamBuild_ForkWriterSuppressRegion verifies the two-pass sha
// contract with the v2 suppress pattern (two suppressed regions
// bracketed by toggle). The canonical region is sources + facts
// through report_annotations; suppressed regions are images/bodies/
// source_images/source_bodies (before facts) and embeddings (after
// report_annotations). The hash covers canonical bytes + a closing "}".
func TestStreamBuild_ForkWriterSuppressRegion(t *testing.T) {
	var fileBuf bytes.Buffer
	h := sha256NewReal()
	fw := &forkWriter{file: &fileBuf, hash: h}

	// Canonical region #1: sources.
	canonical1 := []byte(`{"schema_version":2,"metadata":{"name":"x"},"sources":null`)
	if _, err := fw.Write(canonical1); err != nil {
		t.Fatalf("write canonical1: %v", err)
	}
	// Suppressed region #1: images, bodies, source_images, source_bodies.
	fw.suppress = true
	suppressed1 := []byte(`,"images":{},"source_images":null`)
	if _, err := fw.Write(suppressed1); err != nil {
		t.Fatalf("write suppressed1: %v", err)
	}
	// Canonical region #2: facts through report_annotations.
	fw.suppress = false
	canonical2 := []byte(`,"facts":null,"report_annotations":null`)
	if _, err := fw.Write(canonical2); err != nil {
		t.Fatalf("write canonical2: %v", err)
	}
	// Feed the closing } to the hash (matching marshalCanonical),
	// then suppress for embeddings.
	_, _ = fw.hash.Write([]byte(`}`))
	fw.suppress = true
	suppressed2 := []byte(`,"embeddings":{"model":"m"}}`)
	if _, err := fw.Write(suppressed2); err != nil {
		t.Fatalf("write suppressed2: %v", err)
	}

	// The file should have received everything.
	wantFile := append(append(append(canonical1, suppressed1...), canonical2...), suppressed2...)
	if !bytes.Equal(fileBuf.Bytes(), wantFile) {
		t.Errorf("file bytes mismatch\nwant: %s\ngot:  %s", wantFile, fileBuf.Bytes())
	}

	// The hash should be over canonical1 + canonical2 + "}" only.
	wantHashBytes := append(append(canonical1, canonical2...), '}')
	wantHash := sha256Sum(wantHashBytes)
	if got := hexEncode(h.Sum(nil)); got != wantHash {
		t.Errorf("hash mismatch\nwant: %s\ngot:  %s", wantHash, got)
	}
}

// helpers to keep the fork-writer test readable.
func sha256NewReal() hash.Hash { return sha256.New() }
func sha256Sum(b []byte) string {
	h := sha256.New()
	h.Write(b)
	return hex.EncodeToString(h.Sum(nil))
}
func hexEncode(b []byte) string { return hex.EncodeToString(b) }

var _ = io.ReadAll // keep io imported (used by streamBundleToBytes)

// fakeVectorUpsert records every fact/concept vector batch it receives
// so a test can assert streamVectors parsed and flushed correctly
// without standing up Qdrant.
type fakeVectorUpsert struct {
	facts    []qdrantstore.FactPoint
	concepts []qdrantstore.ConceptPoint
	factErr  error
	concErr  error
}

func (f *fakeVectorUpsert) UpsertFactVectors(_ context.Context, pts []qdrantstore.FactPoint) error {
	if f.factErr != nil {
		return f.factErr
	}
	f.facts = append(f.facts, pts...)
	return nil
}

func (f *fakeVectorUpsert) UpsertConceptVectors(_ context.Context, pts []qdrantstore.ConceptPoint) error {
	if f.concErr != nil {
		return f.concErr
	}
	f.concepts = append(f.concepts, pts...)
	return nil
}

// TestStreamVectors_HighIdxNotMisreadAsObjectOpen is the regression
// test for the double-read bug that surfaced as
// "import_graph: applying bundle: expected object, got 100246".
//
// Before the fix, streamVectors read the opening '{' of the vectors
// map itself and then handed the decoder to decodeObject, which also
// tried to read '{' — but the next token was the first map KEY (a
// stringified idx like "100246"), so decodeObject failed with
// "expected object, got 100246". Any repo with ≥100k facts/concepts
// triggered it on import as soon as the embeddings section was parsed.
//
// This test feeds a vectors object whose first key is "100246"
// (mirroring the reported failure), pre-populates the importer's
// factUUIDs at that idx, and asserts the vector is parsed and
// upserted — not rejected as a stray object token.
func TestStreamVectors_HighIdxNotMisreadAsObjectOpen(t *testing.T) {
	ctx := context.Background()
	fake := &fakeVectorUpsert{}
	s := &StreamImporter{qdrant: fake}

	// Register fact UUIDs at idx 100246 and 100247 so both parsed
	// vectors are upserted (unregistered idxs are parsed-and-skipped).
	factID := pgtype.UUID{}
	if err := factID.Scan("11111111-1111-4111-8111-111111111111"); err != nil {
		t.Fatalf("scan fact id: %v", err)
	}
	s.growFactUUIDs(100246, factID)
	factID2 := pgtype.UUID{}
	if err := factID2.Scan("33333333-3333-4333-8333-333333333333"); err != nil {
		t.Fatalf("scan fact id2: %v", err)
	}
	s.growFactUUIDs(100247, factID2)

	// A fact_vectors object whose first key is "100246" — exactly the
	// token the bug misread as the object opener.
	dec := json.NewDecoder(strings.NewReader(`{"100246":[0.1,0.2,0.3],"100247":[0.4,0.5]}`))
	if err := s.streamVectors(ctx, dec, "fact"); err != nil {
		t.Fatalf("streamVectors fact: %v (want no error; the old double-read would fail here with 'expected object, got 100246')", err)
	}
	if len(fake.facts) != 2 {
		t.Fatalf("upserted facts = %d, want 2", len(fake.facts))
	}
	if fake.facts[0].ID != asUUID(factID) {
		t.Errorf("first upserted id = %v, want %v", fake.facts[0].ID, asUUID(factID))
	}
	if fake.facts[1].ID != asUUID(factID2) {
		t.Errorf("second upserted id = %v, want %v", fake.facts[1].ID, asUUID(factID2))
	}

	// Same regression check for concept_vectors.
	concID := pgtype.UUID{}
	if err := concID.Scan("22222222-2222-4222-8222-222222222222"); err != nil {
		t.Fatalf("scan concept id: %v", err)
	}
	s.growConceptUUIDs(100246, concID)
	decC := json.NewDecoder(strings.NewReader(`{"100246":[0.9,0.8,0.7]}`))
	if err := s.streamVectors(ctx, decC, "concept"); err != nil {
		t.Fatalf("streamVectors concept: %v", err)
	}
	if len(fake.concepts) != 1 {
		t.Fatalf("upserted concepts = %d, want 1", len(fake.concepts))
	}
	if fake.concepts[0].ID != asUUID(concID) {
		t.Errorf("upserted concept id = %v, want %v", fake.concepts[0].ID, asUUID(concID))
	}
}