// Package graph contains the transport-agnostic graph bundle format
// plus the builder (repo → bundle) and importer (bundle → repo).
//
// A "graph" is the serializable form of an entire repository's
// derived layer: sources + facts + concepts + summaries + syntheses +
// investigations + reports + embeddings. Bundles are gzipped JSON
// documents pushed to the shared knowledge registry so other OKT
// instances can import them into a fresh repository in a single
// task — no re-decomposition, no re-summarization, no LLM cost.
//
// The package knows nothing about HTTP or River. The HTTP handler
// (internal/api/handler/graph.go) enqueues an export/import task;
// the River worker (internal/taskmanager/tasks/export_graph.go /
// import_graph.go) calls this package's Builder/Importer against a
// per-repo *store.Queries. Keeping the heavy logic here matches the
// existing provider split (search/fetch/storage are transport-agnostic
// packages the handlers and workers both depend on).
package graph

import "time"

// SchemaVersion is the bundle format version. Bumped when the bundle
// shape changes in a backward-incompatible way; the import path
// checks this and refuses bundles newer than it understands.
const SchemaVersion = 1

// GraphBundle is the on-wire graph format. Internal indices (not
// UUIDs) are used for every cross-reference so the importer can remap
// each idx to a fresh local UUID (uuid.New) without collisions. The
// metadata section carries the human-readable name/description/tags
// the registry indexes for search, plus counts for the UI.
type GraphBundle struct {
	SchemaVersion        int                      `json:"schema_version"`
	Metadata             BundleMetadata           `json:"metadata"`
	Sources              []SourceRow              `json:"sources"`
	Facts                []FactRow                `json:"facts"`
	FactSources          []FactSourceRow          `json:"fact_sources"`
	Concepts             []ConceptRow             `json:"concepts"`
	ConceptAliases       []ConceptAliasRow        `json:"concept_aliases"`
	FactConcepts         []FactConceptRow         `json:"fact_concepts"`
	ConceptSummaries     []SummaryRow             `json:"concept_summaries"`
	ConceptSyntheses     []SynthesisRow           `json:"concept_syntheses"`
	Investigations       []InvestigationRow       `json:"investigations"`
	InvestigationSources []InvestigationSourceRow `json:"investigation_sources"`
	Reports              []ReportRow              `json:"reports"`
	ReportAnnotations    []ReportAnnotationRow    `json:"report_annotations"`
	SourceImages         []SourceImageRow         `json:"source_images"`
	SourceBodies         []SourceBodyRef          `json:"source_bodies,omitempty"`
	Images               map[string]FileBytes     `json:"images,omitempty"`
	Bodies               map[string]FileBytes     `json:"bodies,omitempty"`
	Embeddings           *Embeddings              `json:"embeddings,omitempty"`
}

// BundleMetadata is the human-readable header the registry indexes for
// search + the UI displays. Counts are populated by the builder; the
// registry echoes them on read so the browse UI can show graph size
// without downloading the bundle.
type BundleMetadata struct {
	Name                string    `json:"name"`
	Description         string    `json:"description,omitempty"`
	Owner               string    `json:"owner,omitempty"`
	Tags                []string  `json:"tags,omitempty"`
	OKTVersion          string    `json:"okt_version,omitempty"`
	PromptsetHashes     []string  `json:"promptset_hashes,omitempty"`
	EmbeddingModel      string    `json:"embedding_model,omitempty"`
	EmbeddingDimensions int       `json:"embedding_dimensions,omitempty"`
	SourceCount         int       `json:"source_count"`
	FactCount           int       `json:"fact_count"`
	ConceptCount        int       `json:"concept_count"`
	SummaryCount        int       `json:"summary_count"`
	SynthesisCount      int       `json:"synthesis_count"`
	ReportCount         int       `json:"report_count"`
	InvestigationCount  int       `json:"investigation_count"`
	SHA256              string    `json:"sha256,omitempty"`
	ExportedAt          time.Time `json:"exported_at"`
}

// SourceRow is one source in the bundle. Idx is the bundle-internal
// index fact_sources / investigation_sources reference. SHA256 is
// computed by the builder from the parsed content (so the registry can
// dedup re-pushes of the same graph); it is NOT a column on the
// sources table.
type SourceRow struct {
	Idx            int    `json:"idx"`
	URL            string `json:"url"`
	DOI            string `json:"doi,omitempty"`
	Kind           string `json:"kind"`
	Status         string `json:"status"`
	ParsedTitle    string `json:"parsed_title,omitempty"`
	ParsedText     string `json:"parsed_text,omitempty"`
	ParsedMarkdown string `json:"parsed_markdown,omitempty"`
	PublishedAt    string `json:"published_at,omitempty"` // RFC3339 date
	SHA256         string `json:"sha256,omitempty"`
	HasStoredBody  bool   `json:"has_stored_body,omitempty"`
}

// FactRow is one fact. Idx is the bundle-internal index fact_sources /
// fact_concepts / report_annotations reference. ContentHash is the
// dedup key the import path uses to merge facts into an existing repo.
type FactRow struct {
	Idx            int    `json:"idx"`
	Text           string `json:"text"`
	FactKind       string `json:"fact_kind"`
	ImageURL       string `json:"image_url,omitempty"`
	ContentHash    string `json:"content_hash,omitempty"`
	PromptsetHash  string `json:"promptset_hash,omitempty"`
	Status         string `json:"status"`
	SourceImageIdx int    `json:"source_image_idx,omitempty"` // -1 = none; for image_url remapping on import
}

// FactSourceRow links a fact idx to a source idx.
type FactSourceRow struct {
	FactIdx    int `json:"fact_idx"`
	SourceIdx  int `json:"source_idx"`
	ChunkIndex int `json:"chunk_index"`
}

// ConceptRow is one concept. Idx is the bundle-internal index
// concept_aliases / fact_concepts reference.
type ConceptRow struct {
	Idx           int    `json:"idx"`
	CanonicalName string `json:"canonical_name"`
	Context       string `json:"context"`
	Description   string `json:"description,omitempty"`
	PromptsetHash string `json:"promptset_hash,omitempty"`
}

// ConceptAliasRow links a concept idx to an alias string.
type ConceptAliasRow struct {
	ConceptIdx int    `json:"concept_idx"`
	AliasText  string `json:"alias_text"`
}

// FactConceptRow links a fact idx to a concept idx.
type FactConceptRow struct {
	FactIdx       int    `json:"fact_idx"`
	ConceptIdx    int    `json:"concept_idx"`
	PromptsetHash string `json:"promptset_hash,omitempty"`
}

// SummaryRow is one concept_summary slice. CoveredFactIdxs references
// fact idxs; the importer remaps them to fresh local UUIDs.
type SummaryRow struct {
	ConceptIdx      int    `json:"concept_idx"`
	SequenceNum     int    `json:"sequence_num"`
	IsComplete      bool   `json:"is_complete"`
	FactCount       int    `json:"fact_count"`
	Content         string `json:"content"`
	CoveredFactIdxs []int  `json:"covered_fact_idxs,omitempty"`
	Model           string `json:"model,omitempty"`
}

// SynthesisRow is one concept_synthesis. CanonicalName is the group
// key (one synthesis per lower(canonical_name) per repo). The covered
// idxs reference summary idxs / concept idxs / fact idxs (for images).
type SynthesisRow struct {
	CanonicalName      string `json:"canonical_name"`
	Content            string `json:"content"`
	CoveredSummaryIdxs []int  `json:"covered_summary_idxs,omitempty"`
	CoveredConceptIdxs []int  `json:"covered_concept_idxs,omitempty"`
	EmbeddedImageIdxs  []int  `json:"embedded_image_idxs,omitempty"`
	Model              string `json:"model,omitempty"`
}

// InvestigationRow is one investigation. Idx is the bundle-internal
// index investigation_sources references.
type InvestigationRow struct {
	Idx   int    `json:"idx"`
	Title string `json:"title"`
	Topic string `json:"topic,omitempty"`
}

// InvestigationSourceRow links an investigation idx to a source idx.
type InvestigationSourceRow struct {
	InvestigationIdx int `json:"investigation_idx"`
	SourceIdx        int `json:"source_idx"`
}

// ReportRow is one report. Idx is the bundle-internal index
// report_annotations + parent reference. ParentIdx is -1 (or absent)
// for a root report.
type ReportRow struct {
	Idx                 int     `json:"idx"`
	Title               string  `json:"title"`
	Topic               string  `json:"topic,omitempty"`
	BodyMd              string  `json:"body_md"`
	Status              string  `json:"status"`
	ParentIdx           int     `json:"parent_idx,omitempty"` // -1 = none
	SimilarityThreshold float64 `json:"similarity_threshold,omitempty"`
	EmbeddedModel       string  `json:"embedded_model,omitempty"`
	SentenceCount       int     `json:"sentence_count,omitempty"`
}

// ReportAnnotationRow is one report_annotation. FactIdx references a
// fact idx; the importer remaps it to the fresh local UUID.
type ReportAnnotationRow struct {
	ReportIdx     int     `json:"report_idx"`
	SentenceIndex int     `json:"sentence_index"`
	SentenceText  string  `json:"sentence_text"`
	FactIdx       int     `json:"fact_idx"`
	Score         float64 `json:"score"`
	Posture       string  `json:"posture,omitempty"`
}

// Embeddings carries the Qdrant vectors for the bundle. The import
// path upserts them directly when the model matches the local
// embedding config; otherwise it enqueues embed_facts / embed_concepts
// so the importing repo re-vectorizes with its own model. Vectors are
// keyed by the stringified fact/concept idx (matching the bundle's
// internal index scheme).
type Embeddings struct {
	Model          string               `json:"model"`
	Dimensions     int                  `json:"dimensions"`
	FactVectors    map[string][]float32 `json:"fact_vectors,omitempty"`
	ConceptVectors map[string][]float32 `json:"concept_vectors,omitempty"`
}

// SourceImageRow is one source_images row. Idx is the bundle-internal
// index; SourceIdx references the parent source. ImageRef is the key
// into the Images map; empty for remote-URL inline images (no bytes
// embedded — the importing repo re-fetches from the remote URL).
type SourceImageRow struct {
	Idx         int    `json:"idx"`
	SourceIdx   int    `json:"source_idx"`
	Kind        string `json:"kind"` // "inline" | "page"
	PageNumber  int    `json:"page_number,omitempty"`
	Position    int    `json:"position"`
	URL         string `json:"url,omitempty"` // remote URL for inline; empty for page
	Width       int    `json:"width,omitempty"`
	Height      int    `json:"height,omitempty"`
	Bytes       int    `json:"bytes,omitempty"`
	AltText     string `json:"alt_text,omitempty"`
	ContentType string `json:"content_type,omitempty"`
	ImageRef    string `json:"image_ref,omitempty"` // key into Images map
}

// SourceBodyRef is one entry per source with a stored body (today:
// PDFs). BodyRef is the key into the Bodies map. Only populated when
// the export was requested with include_bodies=true; otherwise the
// section is absent and the importing repo re-fetches the URL (or
// loses the body for upload:// sources).
type SourceBodyRef struct {
	SourceIdx   int    `json:"source_idx"`
	BodyRef     string `json:"body_ref,omitempty"`
	ContentType string `json:"content_type,omitempty"`
}

// FileBytes carries the raw bytes for an embedded image or source
// body. base64 in JSON; gzip compresses (poorly for already-compressed
// PDFs/PNGs, but the bundle is self-contained).
type FileBytes struct {
	ContentType string `json:"content_type"`
	Data        []byte `json:"data"`
}
