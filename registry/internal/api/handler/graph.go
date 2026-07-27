package handler

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"

	"github.com/openktree/knowledge-registry/internal/auth"
	"github.com/openktree/knowledge-registry/internal/model"
	"github.com/openktree/knowledge-registry/internal/service"
)

// GraphHandler exposes the shared knowledge graph CRUD surface.
// A graph bundle is a gzipped JSON document (see OKT's
// internal/providers/graph package); the registry treats the bundle
// as an opaque blob for storage but parses the embedded metadata
// section to populate the searchable graphs table.
//
// Auth follows the sources pattern: list/get are open under
// OptionalAuth (the auth_mode config gates writes); push/delete
// require authentication. The owner field on a pushed graph is
// populated from the authenticated user's email when available.
type GraphHandler struct {
	svc *service.Registry
}

func NewGraphHandler(svc *service.Registry) *GraphHandler {
	return &GraphHandler{svc: svc}
}

// graphMetaSection is the metadata section of a graph bundle. The
// registry only needs this slice to index the graph; the rest of the
// bundle (sources, facts, concepts, …) is stored verbatim as a
// gzipped blob. Defining a local struct avoids importing the OKT
// providers/graph package (which would create a module boundary
// violation: the registry must not depend on the OKT backend).
type graphMetaSection struct {
	Name           string   `json:"name"`
	Description    string   `json:"description"`
	Owner          string   `json:"owner"`
	Tags           []string `json:"tags"`
	SourceCount    int      `json:"source_count"`
	FactCount      int      `json:"fact_count"`
	ConceptCount   int      `json:"concept_count"`
	EmbeddingModel string   `json:"embedding_model"`
	OKTVersion     string   `json:"okt_version"`
	SHA256         string   `json:"sha256"`
}

// graphBundleEnvelope is the minimal top-level shape the handler
// peeks into to extract the metadata section. The full bundle has
// many more fields (sources, facts, concepts, …) but the registry
// only round-trips the bytes, so we decode just the metadata.
type graphBundleEnvelope struct {
	SchemaVersion int              `json:"schema_version"`
	Metadata      graphMetaSection `json:"metadata"`
}

// Push handles POST /api/v1/graphs. The request body is a gzipped
// JSON graph bundle. The handler streams r.Body through a gzip reader
// and a json.Decoder just long enough to decode the leading
// schema_version + metadata fields (the only fields the registry
// indexes), then hands the full original gzipped stream — the bytes
// the gzip reader consumed (captured via a tee) plus the unread
// remainder of r.Body — to PushGraphStream for storage. No 2 GB cap,
// no full-body buffer; the only in-memory state is the small
// compressed prefix consumed while decoding the metadata object.
// Returns the resolved graph id.
//
// The metadata section is the 2nd JSON field (right after
// schema_version), so the gzip + json layers pull only a few KB of
// compressed bytes in the common case. A 16 MB backstop rejects
// pathological bundles whose metadata alone exceeds that, preserving
// the previous maxPeek safety bound.
func (h *GraphHandler) Push(w http.ResponseWriter, r *http.Request) {
	const maxMetadataCompressedBytes = 16 << 20 // 16 MB — backstop for pathological metadata

	// replay captures the compressed bytes the gzip reader pulls from
	// r.Body, so we can prepend them to the unread tail of r.Body and
	// hand storage a complete gzip stream starting at byte 0.
	var replay bytes.Buffer
	// A bounded wrapper around replay enforces the backstop: if the
	// gzip reader asks for more compressed bytes than the metadata
	// object should ever need, we abort with a clear error rather
	// than buffering unbounded data.
	bounded := &limitedReader{r: io.TeeReader(r.Body, &replay), max: maxMetadataCompressedBytes}

	gz, err := gzip.NewReader(bounded)
	if err != nil {
		writeError(w, http.StatusBadRequest, "body is not valid gzip: "+err.Error())
		return
	}
	dec := json.NewDecoder(gz)
	var env graphBundleEnvelope
	// Walk the top-level JSON object token-by-token and decode only
	// the "schema_version" and "metadata" values, then stop. A
	// straight dec.Decode(&env) would read to the top-level closing
	// '}' to validate trailing fields it doesn't care about — pulling
	// the entire bundle (sources, facts, embeddings, …) through the
	// gzip reader and tripping the 16 MB backstop on any real bundle.
	// The token walk pulls only as many compressed bytes as the
	// metadata prefix requires, leaving the rest of the bundle in
	// r.Body for storage to stream.
	if err := decodeMetadata(dec, &env); err != nil {
		if errors.Is(err, errLimitExceeded) {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("bundle metadata too large (exceeds %d byte compressed limit)", maxMetadataCompressedBytes))
			return
		}
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	// Close the gzip reader so its underlying read is fully flushed
	// into replay. We discard the reader (do not read its remaining
	// uncompressed bytes — those belong to the rest of the bundle,
	// which storage will re-decompress from the replayed stream).
	_ = gz.Close()

	if env.Metadata.Name == "" {
		writeError(w, http.StatusBadRequest, "bundle metadata.name is required")
		return
	}

	// Owner: prefer the authenticated user's email; fall back to the
	// bundle's declared owner; finally "anonymous".
	owner := env.Metadata.Owner
	if email := auth.RequestUserEmail(r.Context()); email != "" {
		owner = email
	}
	if owner == "" {
		owner = "anonymous"
	}
	tags := env.Metadata.Tags
	if tags == nil {
		tags = []string{}
	}

	meta := &model.GraphMeta{
		Name:          env.Metadata.Name,
		Description:   env.Metadata.Description,
		Owner:         owner,
		Tags:          tags,
		SourceCount:   env.Metadata.SourceCount,
		FactCount:     env.Metadata.FactCount,
		ConceptCount:  env.Metadata.ConceptCount,
		SHA256:        env.Metadata.SHA256,
		SchemaVersion: env.SchemaVersion,
	}

	// Storage gets: the compressed bytes the gzip reader already
	// consumed (replayed from the tee buffer) + the unread remainder
	// of r.Body. Together this is the complete original gzipped
	// bundle from byte 0.
	bodyStream := io.MultiReader(bytes.NewReader(replay.Bytes()), r.Body)
	result, err := h.svc.PushGraphStream(r.Context(), meta, bodyStream)
	if err != nil {
		log.Printf("graph push: %v", err)
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, result)
}

// errLimitExceeded is returned by limitedReader.Read when the wrapped
// reader has produced more than max bytes. It is distinct from any
// gzip/json error so the Push handler can map it to a clear 400.
var errLimitExceeded = errors.New("metadata compressed size exceeds backstop limit")

// decodeMetadata walks the leading JSON tokens of dec and populates
// env.SchemaVersion + env.Metadata only, then returns. It exists to
// avoid the json.Decoder.Decode(&struct) behavior of reading to the
// top-level closing '}' (which would pull the whole bundle through
// the gzip reader). The top-level object is expected to be of the
// shape {"schema_version": N, "metadata": {...}, ...}; we read
// keys in order and stop as soon as "metadata" is decoded. Fields
// appearing before "metadata" (only "schema_version" in the current
// bundle schema) are decoded into env; any other pre-metadata key
// is skipped via a json.RawMessage decode. Returns an error wrapping
// errLimitExceeded if the limitedReader backstop trips, or the raw
// json/gzip error otherwise.
func decodeMetadata(dec *json.Decoder, env *graphBundleEnvelope) error {
	t, err := dec.Token()
	if err != nil {
		return fmt.Errorf("reading bundle open token: %w", err)
	}
	if d, ok := t.(json.Delim); !ok || d != '{' {
		return fmt.Errorf("bundle is not a JSON object (got %v)", t)
	}
	for dec.More() {
		t, err = dec.Token()
		if err != nil {
			return fmt.Errorf("reading bundle field key: %w", err)
		}
		key, ok := t.(string)
		if !ok {
			return fmt.Errorf("unexpected non-string bundle field key: %v", t)
		}
		switch key {
		case "schema_version":
			if err := dec.Decode(&env.SchemaVersion); err != nil {
				return fmt.Errorf("decoding bundle schema_version: %w", err)
			}
		case "metadata":
			if err := dec.Decode(&env.Metadata); err != nil {
				return fmt.Errorf("decoding bundle metadata: %w", err)
			}
			return nil
		default:
			// Skip the value of any field we don't need that appears
			// before "metadata". A json.RawMessage decode consumes
			// exactly one JSON value without surfacing its shape.
			var raw json.RawMessage
			if err := dec.Decode(&raw); err != nil {
				return fmt.Errorf("skipping bundle field %q: %w", key, err)
			}
		}
	}
	return fmt.Errorf("bundle metadata field not found (reached end of object)")
}

// limitedReader wraps an io.Reader and returns errLimitExceeded once
// more than max bytes have been read. Used by Push to bound the
// compressed-prefix buffer captured while decoding the bundle metadata.
type limitedReader struct {
	r   io.Reader
	n   int64
	max int64
}

func (l *limitedReader) Read(p []byte) (int, error) {
	if l.n >= l.max {
		return 0, errLimitExceeded
	}
	remaining := l.max - l.n
	if int64(len(p)) > remaining {
		p = p[:remaining]
	}
	n, err := l.r.Read(p)
	l.n += int64(n)
	if err == nil && l.n >= l.max {
		// Don't surface the limit as an error mid-read; let the
		// caller ask for one more byte so the next Read returns
		// errLimitExceeded cleanly. This avoids truncating a valid
		// read that happens to land exactly on the boundary.
		return n, nil
	}
	return n, err
}

// List handles GET /api/v1/graphs. Supports ?limit=&offset=&q=&tag=.
// The q and tag filters are mutually exclusive (q wins when both set).
func (h *GraphHandler) List(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
	if offset < 0 {
		offset = 0
	}
	q := r.URL.Query().Get("q")
	tag := r.URL.Query().Get("tag")
	result, err := h.svc.ListGraphs(r.Context(), model.GraphSearchQuery{
		Query:  q,
		Tag:    tag,
		Limit:  limit,
		Offset: offset,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, result)
}

// Get handles GET /api/v1/graphs/{id}. Returns the graph metadata
// with a presigned download URL for the bundle.
func (h *GraphHandler) Get(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "graph id is required")
		return
	}
	meta, err := h.svc.PullGraph(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusNotFound, "graph not found")
		return
	}
	writeJSON(w, http.StatusOK, meta)
}

// PullBundle handles GET /api/v1/graphs/{id}/bundle. Streams the raw
// gzipped bundle bytes straight from storage (or via the registry
// when no presigned URL is available). Used by the OKT import path's
// fallback when the presigned fast path is unavailable.
func (h *GraphHandler) PullBundle(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "graph id is required")
		return
	}
	data, contentType, err := h.svc.PullGraphBundle(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusNotFound, "graph bundle not found")
		return
	}
	w.Header().Set("Content-Type", contentType)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

// Delete handles DELETE /api/v1/graphs/{id}. Owner-or-admin only:
// a non-admin user may only delete graphs they pushed (matched on
// the owner field, which the push handler populated from the
// authenticated email).
func (h *GraphHandler) Delete(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "graph id is required")
		return
	}
	role := auth.RequestUserRole(r.Context())
	email := auth.RequestUserEmail(r.Context())
	// Fetch the meta to check ownership when the caller isn't an admin.
	if role != "admin" {
		meta, err := h.svc.PullGraph(r.Context(), id)
		if err != nil {
			writeError(w, http.StatusNotFound, "graph not found")
			return
		}
		if meta.Owner != email || email == "" {
			writeError(w, http.StatusForbidden, "only the owner or an admin can delete this graph")
			return
		}
	}
	if err := h.svc.DeleteGraph(r.Context(), id); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"message": "graph deleted"})
}
