package handler

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"os"
	"strings"

	"github.com/openktree/knowledge-registry/internal/auth"
	"github.com/openktree/knowledge-registry/internal/config"
	"github.com/openktree/knowledge-registry/internal/model"
	"github.com/openktree/knowledge-registry/internal/service"
)

// SetUploadConfig wires the graph-upload config. Called by the router
// builder; the upload handler returns 503 when nil (so a registry that
// hasn't opted into file uploads just doesn't expose the path).
func (h *GraphHandler) SetUploadConfig(cfg *config.GraphUploadConfig) {
	h.uploadCfg = cfg
}

// UploadFromFile handles POST /api/v1/admin/graphs/upload — the
// admin-only file-upload path for graph bundles. A multipart form with
// a "bundle" file part (a .json.gz graph bundle produced by OKT's
// export) plus optional "name", "description", "tags" (JSON array),
// "owner" form fields. The registry spools the file to a temp file,
// extracts the bundle's metadata section without buffering the whole
// file in memory, streams the temp file to storage, and indexes the
// metadata row. Returns 201 + {graph_id, new}.
//
// Memory is bounded regardless of bundle size: the bundle body is
// streamed straight to a single temp file (32 KB copy buffer), the
// metadata parse reads only the small metadata section, and storage's
// multipart upload reads one 64 MB part at a time. The 100s-of-GB bulk
// lives only on disk.
//
// Admin-only via the router's RequireRole("admin") group. The owner
// field defaults to the authenticated admin's email (falling back to
// the bundle's metadata.owner or "anonymous"), mirroring Push.
//
// Config: cfg.GraphUpload.MaxSizeBytes (0 = unlimited) bounds the
// spool; cfg.GraphUpload.TempDir controls where the temp file lives
// (operators point this at a mounted volume for large uploads).
func (h *GraphHandler) UploadFromFile(w http.ResponseWriter, r *http.Request) {
	if h.uploadCfg == nil {
		writeError(w, http.StatusServiceUnavailable, "graph upload not configured")
		return
	}
	maxBytes := h.uploadCfg.MaxSizeBytes
	tempDir := h.uploadCfg.TempDir

	// Parse the multipart form. ParseMultipartForm with a 1 MB
	// in-memory threshold spills the bundle file to the parser's own
	// temp file and exposes it via FileHeader.Open. We re-spool that
	// to our own temp file below; the double-spool is acceptable (the
	// alternative — driving NextPart manually — complicates the text-
	// field parsing and gains ~one disk write for a 100 GB upload).
	// A single spool would be ideal, but KISS: the parser's temp file
	// lives in the OS temp dir, ours lives in the configured volume.
	if maxBytes > 0 {
		r.Body = http.MaxBytesReader(w, r.Body, maxBytes)
	}
	if err := r.ParseMultipartForm(1 << 20); err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, http.ErrMissingBoundary) {
			status = http.StatusBadRequest
		}
		if strings.Contains(err.Error(), "too large") {
			status = http.StatusRequestEntityTooLarge
		}
		writeError(w, status, "parsing multipart form: "+err.Error())
		return
	}
	fhs, ok := r.MultipartForm.File["bundle"]
	if !ok || len(fhs) == 0 {
		writeError(w, http.StatusBadRequest, "bundle file is required")
		return
	}
	file, err := fhs[0].Open()
	if err != nil {
		writeError(w, http.StatusBadRequest, "opening bundle file part: "+err.Error())
		return
	}
	defer file.Close()
	// RemoveAll cleans up the parser's temp file for the bundle
	// part. Best-effort: a failure here leaks a temp file but the
	// upload itself has already succeeded by the time this runs.
	defer func() { _ = r.MultipartForm.RemoveAll() }()

	// Optional form-field overrides. Non-empty fields win over the
	// bundle's metadata; empty fields fall back to the bundle.
	overrides := uploadOverrides{
		Name:        strings.TrimSpace(formValue(r.MultipartForm.Value, "name")),
		Description: strings.TrimSpace(formValue(r.MultipartForm.Value, "description")),
		Owner:       strings.TrimSpace(formValue(r.MultipartForm.Value, "owner")),
		TagsJSON:    strings.TrimSpace(formValue(r.MultipartForm.Value, "tags")),
	}

	// Spool the bundle to a temp file on the configured volume. The
	// spool is the single on-disk copy the rest of the path reads
	// from (metadata parse + storage stream). Cleanup is deferred.
	tmpPath, err := service.SpoolUploadToTempFile(file, tempDir, maxBytes)
	if err != nil {
		if errors.Is(err, service.ErrUploadTooLarge) {
			writeError(w, http.StatusRequestEntityTooLarge, err.Error())
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer func() { _ = os.Remove(tmpPath) }()

	// Extract the bundle's metadata section (name/description/tags/
	// counts/sha256/schema_version) by streaming the temp file
	// through gzip + json.Decoder and stopping after the metadata
	// object. The rest of the bundle (the 100s-of-GB bulk) is never
	// touched. Peak parse memory is the small metadata struct.
	bundleMeta, err := service.ExtractGraphMetaFromTempFile(r.Context(), tmpPath)
	if err != nil {
		if errors.Is(err, service.ErrMetadataNotFound) {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeError(w, http.StatusBadRequest, "reading bundle metadata: "+err.Error())
		return
	}

	// Apply caller overrides. The admin can rename a bundle on
	// upload; non-empty fields win, empty fields fall back to the
	// bundle's own metadata.
	applyOverrides(bundleMeta, overrides)

	// Owner: admin email from the session wins; otherwise the
	// bundle's owner; otherwise "anonymous". Mirrors Push.
	if email := auth.RequestUserEmail(r.Context()); email != "" {
		bundleMeta.Owner = email
	}
	if bundleMeta.Owner == "" {
		bundleMeta.Owner = "anonymous"
	}

	// Require a name (the registry indexes by name). The bundle's
	// metadata.name is the source; an override may empty it.
	if bundleMeta.Name == "" {
		writeError(w, http.StatusBadRequest, "name is required (set it in the bundle metadata or the name form field)")
		return
	}

	result, err := h.svc.PushGraphFromFile(r.Context(), bundleMeta, tmpPath)
	if err != nil {
		log.Printf("graph upload: %v", err)
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, result)
}

// uploadOverrides carries the optional form-field overrides the admin
// can supply alongside the bundle file. Non-empty fields win over the
// bundle's extracted metadata.
type uploadOverrides struct {
	Name        string
	Description string
	Owner       string
	TagsJSON    string // raw JSON array string from the form; parsed in applyOverrides
}

// applyOverrides merges non-empty overrides into the bundle-extracted
// metadata. Empty override fields fall back to the bundle's value.
// A malformed tags JSON is silently ignored (the bundle's tags survive)
// — the upload form is friendlier than the API's strict X-Graph-Tags
// 400 path, since the admin sees the result on the next page.
func applyOverrides(meta *model.GraphMeta, ov uploadOverrides) {
	if ov.Name != "" {
		meta.Name = ov.Name
	}
	if ov.Description != "" {
		meta.Description = ov.Description
	}
	if ov.Owner != "" {
		meta.Owner = ov.Owner
	}
	if ov.TagsJSON != "" {
		var tags []string
		if err := json.Unmarshal([]byte(ov.TagsJSON), &tags); err == nil {
			meta.Tags = tags
		}
	}
}

// formValue returns the first value for key from a multipart form's
// Value map (map[string][]string has no .Get). Empty when the key is
// absent.
func formValue(values map[string][]string, key string) string {
	if vs, ok := values[key]; ok && len(vs) > 0 {
		return vs[0]
	}
	return ""
}
