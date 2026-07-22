package handler

import (
	"compress/gzip"
	"encoding/json"
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
// JSON graph bundle. The handler ungzips, peeks at the metadata
// section to populate the searchable index, then stores the raw
// gzipped bytes in S3 (so pulls can stream the original bytes
// without re-gzipping). Returns the resolved graph id.
func (h *GraphHandler) Push(w http.ResponseWriter, r *http.Request) {
	// Read the raw gzipped bytes first; these are stored verbatim.
	bodyBytes, err := io.ReadAll(io.LimitReader(r.Body, 2<<30)) // 2GB cap
	if err != nil {
		writeError(w, http.StatusBadRequest, "reading request body: "+err.Error())
		return
	}
	if len(bodyBytes) == 0 {
		writeError(w, http.StatusBadRequest, "empty request body")
		return
	}

	// Ungzip to peek at the metadata section. We re-gzip on the other
	// side is NOT needed — we store the original gzipped bytes.
	gz, err := gzip.NewReader(bytesReader(bodyBytes))
	if err != nil {
		writeError(w, http.StatusBadRequest, "body is not valid gzip: "+err.Error())
		return
	}
	jsonBytes, err := io.ReadAll(gz)
	if err != nil {
		writeError(w, http.StatusBadRequest, "gunzipping bundle: "+err.Error())
		return
	}
	gz.Close()

	var env graphBundleEnvelope
	if err := json.Unmarshal(jsonBytes, &env); err != nil {
		writeError(w, http.StatusBadRequest, "bundle is not valid JSON: "+err.Error())
		return
	}
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
	result, err := h.svc.PushGraph(r.Context(), meta, bodyBytes)
	if err != nil {
		log.Printf("graph push: %v", err)
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, result)
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

// bytesReader wraps a []byte as an io.Reader without bytes.NewReader
// (kept here to avoid an extra import alias collision; the stdlib
// bytes.NewReader would work too, but this local helper keeps the
// push handler self-contained).
func bytesReader(b []byte) io.Reader {
	return &byteReader{b: b}
}

type byteReader struct {
	b []byte
	i int
}

func (r *byteReader) Read(p []byte) (int, error) {
	if r.i >= len(r.b) {
		return 0, io.EOF
	}
	n := copy(p, r.b[r.i:])
	r.i += n
	return n, nil
}
