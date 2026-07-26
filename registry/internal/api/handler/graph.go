package handler

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"encoding/json"
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
// JSON graph bundle. The handler buffers a head chunk, gunzips just
// enough to decode the metadata section (for the searchable index),
// then streams the full original gzipped body (buffered head + the
// unread remainder of r.Body) straight to storage — no 2 GB cap, no
// full-body buffer. Returns the resolved graph id.
//
// The metadata section is the 2nd JSON field (right after
// schema_version), so a small head buffer (256 KB) is enough to
// decode it for any realistic bundle. If the head is too small to
// contain the full metadata object (pathological case: a multi-MB
// description), the handler falls back to growing the buffer up to
// maxPeekBytes (16 MB) before giving up with a 400.
func (h *GraphHandler) Push(w http.ResponseWriter, r *http.Request) {
	const (
		initialPeek = 256 << 10  // 256 KB — covers schema_version + metadata
		maxPeek     = 16 << 20   // 16 MB — backstop for pathological metadata
	)

	// Buffer the head so we can (a) gunzip + decode metadata from it
	// and (b) replay it into the storage stream. We read greedily up
	// to initialPeek, then more if needed.
	br := bufio.NewReaderSize(r.Body, initialPeek)
	head, err := br.Peek(initialPeek)
	if err != nil && err != io.EOF {
		writeError(w, http.StatusBadRequest, "reading request body head: "+err.Error())
		return
	}
	// If Peek returned less than initialPeek (short body or EOF),
	// head is what's available; br holds nothing extra. If Peek
	// returned the full initialPeek, br still holds those bytes
	// (Peek doesn't consume) — we'll consume via the MultiReader
	// below.

	// Gunzip the head + decode the metadata envelope. We feed the
	// head bytes to a gzip reader; if the gzip stream needs more
	// bytes than the head to decode the metadata object, we grow.
	env, consumedHead, err := peekMetadata(head, br, initialPeek, maxPeek)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
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

	// The storage stream is: the original gzipped bytes from the
	// very start (we consumed `consumedHead` bytes from br while
	// peeking; br still holds the unconsumed remainder). We replay
	// the consumed head + the rest of br. bufio.Reader.Peek doesn't
	// consume, but our peekMetadata may have Consumed via Discard;
	// the simplest correct replay is: bytes.NewReader(head[:nConsumed])
	// + br (which still has the unconsumed peeked bytes + unread
	// body). Since Peek keeps bytes in the buffer, br.Read() yields
	// the full original stream from byte 0 — so we don't even need
	// to replay the head. But peekMetadata may have called Discard
	// to advance past gzip-frame boundaries. To be safe, replay the
	// consumed bytes explicitly.
	bodyStream := io.MultiReader(bytes.NewReader(consumedHead), br)
	result, err := h.svc.PushGraphStream(r.Context(), meta, bodyStream)
	if err != nil {
		log.Printf("graph push: %v", err)
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, result)
}

// peekMetadata gunzips the head bytes and decodes the
// graphBundleEnvelope (schema_version + metadata). If the gzip
// stream needs more bytes than head to fully decode the metadata
// object, it reads more from br (the bufio reader over r.Body),
// growing the buffer up to maxPeek. Returns the envelope, the raw
// gzipped bytes consumed so far (to replay into the storage stream),
// and an error.
//
// The consumed bytes are the head bytes that were fed to the gzip
// reader; the caller replays them via io.MultiReader so storage gets
// the full original gzipped stream.
func peekMetadata(head []byte, br *bufio.Reader, initialPeek, maxPeek int) (graphBundleEnvelope, []byte, error) {
	// Try decoding from the head first.
	gz, err := gzip.NewReader(bytes.NewReader(head))
	if err != nil {
		return graphBundleEnvelope{}, nil, fmt.Errorf("body is not valid gzip: %w", err)
	}
	dec := json.NewDecoder(gz)
	var env graphBundleEnvelope
	// Read the opening '{' + "schema_version" + metadata object.
	if err := dec.Decode(&env); err == nil {
		// Success — head was enough. The consumed gzipped bytes are
		// the whole head (the gzip reader may have read less, but
		// replaying the full head + the rest of br is safe: the
		// storage path gunzips from the start, so extra head bytes
		// it already consumed are part of the gzip stream). We need
		// to know how many bytes gz consumed to avoid replaying
		// beyond them. gzip.Reader doesn't expose that, but since
		// we feed head and the storage path re-reads from byte 0
		// via br (which still has the peeked bytes), we return head
		// as consumed and the caller uses io.MultiReader(head, br).
		// BUT br still contains the head bytes (Peek doesn't
		// consume), so replaying head + br would duplicate them.
		// Fix: discard the peeked head from br so br starts after it.
		_, _ = br.Discard(len(head))
		return env, head, nil
	}
	// Head wasn't enough. Grow the buffer up to maxPeek, feeding the
	// accumulating bytes to a fresh gzip reader each attempt (cheap
	// for the small metadata prefix).
	total := head
	for len(total) < maxPeek {
		chunk, err := br.Peek(len(total) + initialPeek)
		if err != nil && err != io.EOF {
			return graphBundleEnvelope{}, nil, fmt.Errorf("reading request body: %w", err)
		}
		total = chunk
		gz, err := gzip.NewReader(bytes.NewReader(total))
		if err != nil {
			return graphBundleEnvelope{}, nil, fmt.Errorf("body is not valid gzip: %w", err)
		}
		dec := json.NewDecoder(gz)
		if err := dec.Decode(&env); err == nil {
			_, _ = br.Discard(len(total))
			return env, total, nil
		}
		if len(chunk) < len(total)+initialPeek {
			// EOF — no more bytes; the bundle is truncated.
			return graphBundleEnvelope{}, nil, fmt.Errorf("bundle metadata truncated (could not decode within %d bytes)", len(total))
		}
	}
	return graphBundleEnvelope{}, nil, fmt.Errorf("bundle metadata too large (exceeds %d byte peek limit)", maxPeek)
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
