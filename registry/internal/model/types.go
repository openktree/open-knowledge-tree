package model

import "time"

type Repository struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	Description string    `json:"description,omitempty"`
	Owner       string    `json:"owner,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

type RepoUpdate struct {
	Name        *string `json:"name,omitempty"`
	Description *string `json:"description,omitempty"`
}

type SourceMeta struct {
	ID        string    `json:"id"`
	RepoID    string    `json:"repo_id"`
	URL       string    `json:"url,omitempty"`
	DOI       string    `json:"doi,omitempty"`
	SHA256    string    `json:"sha256,omitempty"`
	Title     string    `json:"title,omitempty"`
	S3Key     string    `json:"s3_key"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

type SourceRef struct {
	SourceID    string                `json:"source_id"`
	S3Key       string                `json:"s3_key"`
	Presigned   PresignedURLs         `json:"presigned"`
	Decomps     []DecompRef           `json:"decompositions,omitempty"`
}

type PresignedURLs struct {
	Source string            `json:"source"`
	Body   string            `json:"body,omitempty"`
	Images map[string]string `json:"images,omitempty"`
}

type DecompRef struct {
	ModelID        string `json:"model_id"`
	FactCount      int    `json:"fact_count"`
	HasEmbeddings  bool   `json:"has_embeddings"`
	EmbeddingModel string `json:"embedding_model,omitempty"`
	PresignedURL   string `json:"presigned_url"`
}

type DecompMeta struct {
	ID              string    `json:"id"`
	SourceID        string    `json:"source_id"`
	ModelID         string    `json:"model_id"`
	DecomposedBy    string    `json:"decomposed_by,omitempty"`
	DecomposedAt    time.Time `json:"decomposed_at,omitempty"`
	FactCount       int       `json:"fact_count"`
	SummaryCount    int       `json:"summary_count"`
	HasEmbeddings   bool      `json:"has_embeddings"`
	EmbeddingModel  string    `json:"embedding_model,omitempty"`
	EmbeddingDims   int       `json:"embedding_dimensions,omitempty"`
	S3Key           string    `json:"s3_key"`
	CreatedAt       time.Time `json:"created_at"`
}

type SourcePackage struct {
	SchemaVersion int               `json:"schema_version"`
	Source        SourceData        `json:"source"`
	Content       ContentData       `json:"content"`
	Decompositions []DecompRef      `json:"decompositions"`
}

type SourceData struct {
	ID          string     `json:"id"`
	URL         string     `json:"url,omitempty"`
	DOI         string     `json:"doi,omitempty"`
	SHA256      string     `json:"sha256,omitempty"`
	Title       string     `json:"title,omitempty"`
	PublishedAt *time.Time `json:"published_at,omitempty"`
	Content     ContentData `json:"content,omitempty"`
}

type ContentData struct {
	Text        string          `json:"text"`
	Markdown    string          `json:"markdown,omitempty"`
	ContentType string          `json:"content_type,omitempty"`
	HasBody     bool            `json:"has_body"`
	Images      []ImageRef      `json:"images,omitempty"`
}

type ImageRef struct {
	ID          string `json:"id"`
	Filename    string `json:"filename"`
	AltText     string `json:"alt_text,omitempty"`
	Description string `json:"description,omitempty"`
}

type DecompositionPackage struct {
	SchemaVersion    int                    `json:"schema_version"`
	ModelID          string                 `json:"model_id"`
	DecomposedBy     string                 `json:"decomposed_by,omitempty"`
	DecomposedAt     time.Time              `json:"decomposed_at"`
	Facts            []FactData             `json:"facts"`
	Concepts         []ConceptData          `json:"concepts"`
	Summaries        []SummaryData          `json:"summaries,omitempty"`
	Embeddings       *EmbeddingData         `json:"embeddings,omitempty"`
	ConceptEmbeddings *EmbeddingData        `json:"concept_embeddings,omitempty"`
}

type FactData struct {
	ID          string   `json:"id"`
	Content     string   `json:"content"`
	ContentHash string   `json:"content_hash"`
	SourceText  string   `json:"source_text,omitempty"`
	ImageID     string   `json:"image_id,omitempty"`
	ConceptIDs  []string `json:"concept_ids,omitempty"`
}

type ConceptData struct {
	ID            string   `json:"id"`
	CanonicalName string   `json:"canonical_name"`
	Context       string   `json:"context"`
	OntologyClass string   `json:"ontology_class,omitempty"`
	Aliases       []string `json:"aliases,omitempty"`
}

type SummaryData struct {
	ID         string   `json:"id"`
	ConceptID  string   `json:"concept_id"`
	SliceNum   int      `json:"slice_number"`
	IsOpen     bool     `json:"is_open"`
	Content    string   `json:"content"`
	FactIDs    []string `json:"fact_ids,omitempty"`
}

type EmbeddingData struct {
	Model      string               `json:"model"`
	Dimensions int                  `json:"dimensions"`
	Vectors    map[string][]float32 `json:"vectors"`
}

type PushResult struct {
	SourceID     string `json:"source_id"`
	FactsNew     int    `json:"facts_new"`
	FactsLinked  int    `json:"facts_linked"`
}

// FactHashEntry is the per-fact input to the batch fact-hash upsert.
// ContentHash is the sha256 of the fact text; FactID is the remote
// fact's UUID (empty for link-only entries). Used by BatchUpsertFactHashes.
type FactHashEntry struct {
	ContentHash string
	FactID      string
}

// BatchFactHashResult is the outcome of a batch fact-hash upsert.
// New is the count of hashes inserted for the first time; Linked is
// the count of hashes that already existed and were re-linked to the
// decomposition.
type BatchFactHashResult struct {
	New    int
	Linked int
}

type SearchQuery struct {
	URL   string `json:"url,omitempty"`
	DOI   string `json:"doi,omitempty"`
	SHA256 string `json:"sha256,omitempty"`
	Text  string `json:"text,omitempty"`
	Limit int    `json:"limit,omitempty"`
}

type User struct {
	ID           string    `json:"id"`
	Email        string    `json:"email"`
	PasswordHash string    `json:"-"`
	DisplayName  string    `json:"display_name"`
	Role         string    `json:"role"` // "viewer" | "editor" | "admin"
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

type APIToken struct {
	ID        string     `json:"id"`
	UserID    string     `json:"user_id"`
	Name      string     `json:"name"`
	TokenHash string     `json:"-"`
	Scope     string     `json:"scope"` // "read" | "write" | "readwrite"
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
	CreatedAt time.Time  `json:"created_at"`
}

type SearchResult struct {
	Found      bool         `json:"found"`
	SourceID   string       `json:"source_id,omitempty"`
	S3Key      string       `json:"s3_key,omitempty"`
	Presigned  PresignedURLs `json:"presigned,omitempty"`
	Decomps    []DecompRef `json:"decompositions,omitempty"`
}
