package handler

import (
	"encoding/json"
	"log"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/openktree/knowledge-registry/internal/auth"
	"github.com/openktree/knowledge-registry/internal/model"
	"github.com/openktree/knowledge-registry/internal/service"
)

// GraphHandler exposes the shared knowledge graph CRUD surface.
// A graph bundle is a gzipped JSON document (see OKT's
// internal/providers/graph package); the registry treats the body as
// an opaque blob streamed straight to S3. Indexing metadata
// (name, description, tags, counts, sha256, schema_version) arrives
// in X-Graph-* request headers, NOT parsed from the body — this keeps
// the body a pure byte pipe (worker temp file → HTTP → S3) with no
// tee/replay/spool, eliminating the "decode metadata, then re-stream
// the body" failure modes that previously broke multi-GB pushes.
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

// Push handles POST /api/v1/graphs. The request body is the raw
// gzipped graph bundle, streamed opaquely to storage. Indexing
// metadata is carried in X-Graph-* headers so the registry never
// needs to parse the body (the previous body-decode path was the
// source of the bufio buffer-full, metadata-too-large, and
// non-seekable-stream failures). Returns the resolved graph id.
//
// Required header: X-Graph-Name. Optional headers: X-Graph-Description,
// X-Graph-Tags (JSON array), X-Graph-Source-Count / X-Graph-Fact-Count
// / X-Graph-Concept-Count (ints), X-Graph-SHA256 (hex),
// X-Graph-Schema-Version (int), X-Graph-Owner (overridden by the
// authenticated email when present).
func (h *GraphHandler) Push(w http.ResponseWriter, r *http.Request) {
	name := r.Header.Get("X-Graph-Name")
	if name == "" {
		writeError(w, http.StatusBadRequest, "X-Graph-Name header is required")
		return
	}

	owner := r.Header.Get("X-Graph-Owner")
	if email := auth.RequestUserEmail(r.Context()); email != "" {
		owner = email
	}
	if owner == "" {
		owner = "anonymous"
	}

	tags := []string{}
	if tagsHeader := r.Header.Get("X-Graph-Tags"); tagsHeader != "" {
		if err := json.Unmarshal([]byte(tagsHeader), &tags); err != nil {
			writeError(w, http.StatusBadRequest, "X-Graph-Tags must be a JSON array of strings: "+err.Error())
			return
		}
	}

	meta := &model.GraphMeta{
		Name:          name,
		Description:   r.Header.Get("X-Graph-Description"),
		Owner:         owner,
		Tags:          tags,
		SourceCount:   headerInt(r, "X-Graph-Source-Count"),
		FactCount:     headerInt(r, "X-Graph-Fact-Count"),
		ConceptCount:  headerInt(r, "X-Graph-Concept-Count"),
		SHA256:        r.Header.Get("X-Graph-SHA256"),
		SchemaVersion: headerInt(r, "X-Graph-Schema-Version"),
	}

	// The body is a pure byte pipe to storage — no tee, no replay,
	// no buffering. Storage's multipart upload handles retries per
	// part without needing a seekable stream.
	result, err := h.svc.PushGraphStream(r.Context(), meta, r.Body)
	if err != nil {
		log.Printf("graph push: %v", err)
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, result)
}

// headerInt parses a request header as an int, returning 0 on
// absence or parse error. Used for the optional X-Graph-*-Count and
// X-Graph-Schema-Version headers.
func headerInt(r *http.Request, key string) int {
	v := r.Header.Get(key)
	if v == "" {
		return 0
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0
	}
	return n
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

// CleanupUploads handles POST /api/v1/admin/cleanup-uploads?max_age=1h.
// Lists in-progress multipart uploads in the storage backend and
// aborts the ones older than max_age (orphaned from failed pushes).
// In-flight pushes younger than max_age are left alone.
//
// The endpoint is admin-only (mounted under the admin group in the
// router). Non-S3 backends (filesystem/local dev) return 503 — only
// the S3 backend has multipart uploads.
//
// Query param: max_age — Go duration (e.g. "1h", "30m", "2h").
// Default "1h". 400 on parse error.
func (h *GraphHandler) CleanupUploads(w http.ResponseWriter, r *http.Request) {
	maxAge := 1 * time.Hour
	if v := r.URL.Query().Get("max_age"); v != "" {
		parsed, err := time.ParseDuration(v)
		if err != nil {
			writeError(w, http.StatusBadRequest, "max_age must be a Go duration (e.g. 1h, 30m): "+err.Error())
			return
		}
		if parsed <= 0 {
			writeError(w, http.StatusBadRequest, "max_age must be a positive duration")
			return
		}
		maxAge = parsed
	}

	listed, aborted, failed, err := h.svc.CleanupMultipartUploads(r.Context(), maxAge)
	if err != nil {
		// Non-S3 backend → 503; anything else → 500.
		if err.Error() == "cleanup not supported for this storage backend" {
			writeError(w, http.StatusServiceUnavailable, err.Error())
			return
		}
		log.Printf("cleanup-uploads: %v", err)
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"listed":  listed,
		"aborted": aborted,
		"failed":  len(failed),
		"max_age": maxAge.String(),
		"uploads": failed,
	})
}