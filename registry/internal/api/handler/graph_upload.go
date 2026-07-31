package handler

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"mime"
	"mime/multipart"
	"net/http"
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
// "owner" form fields.
//
// Streaming path (no full-disk spool): the handler drives
// multipart.Reader directly — reading text form fields as small parts,
// then the "bundle" file part as an io.Reader. It peeks the bundle's
// leading bytes via ExtractGraphMetaFromReader (gzip + json, stops
// after the metadata section — the first few KB), captures the
// consumed head as a replay buffer, and streams the full bundle to
// storage via PushGraphFromReader(io.MultiReader(replayHead, part)).
// The 100s-of-GB bulk never touches disk; peak memory is the 1 MB
// metadata peek + one 64 MB S3 multipart part.
//
// Admin-only via the router's RequireRole("admin") group. The owner
// field defaults to the authenticated admin's email (falling back to
// the bundle's metadata.owner or "anonymous"), mirroring Push.
// Returns 201 + {graph_id, new}.
func (h *GraphHandler) UploadFromFile(w http.ResponseWriter, r *http.Request) {
	result, status, errMsg := processStreamedUpload(h.svc, h.uploadCfg, r)
	if errMsg != "" {
		log.Printf("graph upload: %s", errMsg)
		writeError(w, status, errMsg)
		return
	}
	writeJSON(w, http.StatusCreated, result)
}

// processStreamedUpload is the shared streaming-upload core used by
// both the API handler (GraphHandler.UploadFromFile) and the UI
// handler (UIHandler.GraphUploadPage). It drives multipart.Reader,
// extracts the bundle metadata from the leading bytes without
// spooling the full file to disk, and streams the bundle to storage.
// Returns (result, 0, "") on success — the caller writes the response
// shape (JSON vs redirect) appropriate to its surface. On failure
// returns (nil, status, errMsg) and the caller renders the error. The
// caller is responsible for logging errMsg when non-empty.
func processStreamedUpload(svc *service.Registry, cfg *config.GraphUploadConfig, r *http.Request) (*model.GraphPushResult, int, string) {
	if cfg == nil {
		return nil, http.StatusServiceUnavailable, "graph upload not configured"
	}
	maxBytes := cfg.MaxSizeBytes

	// Parse the multipart boundary from the Content-Type header and
	// build a streaming multipart.Reader. Unlike ParseMultipartForm
	// (which buffers file parts to the parser's own temp file and
	// forces a re-spool), multipart.Reader.NextPart returns each part
	// as an io.Reader that streams the body without buffering it —
	// the single-spool-free path.
	_, mr, err := parseMultipartStream(r)
	if err != nil {
		return nil, http.StatusBadRequest, "parsing multipart boundary: " + err.Error()
	}

	overrides := uploadOverrides{}
	var bundlePart io.Reader
	for {
		part, perr := mr.NextPart()
		if perr == io.EOF {
			break
		}
		if perr != nil {
			return nil, http.StatusBadRequest, "reading multipart part: " + perr.Error()
		}
		formName := part.FormName()
		if formName == "" {
			continue
		}
		if formName == "bundle" {
			// The bundle file part — keep the reader and stop iterating
			// parts. multipart.Reader.NextPart skips past the current
			// part's body when called again, which would discard the
			// bundle bytes before we stream them to storage. The
			// metadata extraction + storage stream reads from this
			// part reader directly; any remaining text fields after
			// the bundle are not supported (the upload form puts text
			// fields first, matching the convention).
			bundlePart = part
			break
		}
		// Text form field — read the (small) value into the overrides.
		val, verr := io.ReadAll(io.LimitReader(part, 1<<20))
		if verr != nil {
			return nil, http.StatusBadRequest, "reading form field " + formName + ": " + verr.Error()
		}
		applyField(&overrides, formName, strings.TrimSpace(string(val)))
	}
	if bundlePart == nil {
		return nil, http.StatusBadRequest, "bundle file is required"
	}

	// Cap the bundle read at maxBytes (0 = unlimited). A limitedReader
	// returns ErrUploadTooLarge when exceeded; the metadata parse and
	// the storage stream both read through it so the cap is enforced
	// regardless of bundle size.
	bodyReader := bundlePart
	if maxBytes > 0 {
		bodyReader = &limitedUploadReader{R: bundlePart, N: maxBytes}
	}

	// Extract the bundle's metadata from the leading bytes (gzip +
	// json, stops after the metadata object). The TeeReader inside
	// captures the consumed head so the caller can replay it to
	// storage. The rest of the bundle (the 100s-of-GB bulk) stays in
	// bodyReader, unread.
	bundleMeta, replayHead, err := service.ExtractGraphMetaFromReader(r.Context(), bodyReader)
	if err != nil {
		if errors.Is(err, service.ErrUploadTooLarge) {
			return nil, http.StatusRequestEntityTooLarge, "upload exceeds the configured size limit"
		}
		if errors.Is(err, service.ErrMetadataNotFound) {
			return nil, http.StatusBadRequest, err.Error()
		}
		return nil, http.StatusBadRequest, "reading bundle metadata: " + err.Error()
	}

	applyOverrides(bundleMeta, overrides)
	if email := auth.RequestUserEmail(r.Context()); email != "" {
		bundleMeta.Owner = email
	}
	if bundleMeta.Owner == "" {
		bundleMeta.Owner = "anonymous"
	}
	if bundleMeta.Name == "" {
		return nil, http.StatusBadRequest, "name is required (set it in the bundle metadata or the name form field)"
	}

	// Stream the full bundle to storage: the replayed head (the
	// leading bytes the metadata parse consumed) + the remaining
	// unread body. Storage's multipart upload reads one 64 MB part at
	// a time; the bundle never lands on disk.
	result, err := svc.PushGraphFromReader(r.Context(), bundleMeta, io.MultiReader(replayHead, bodyReader))
	if err != nil {
		// The size cap (limitedUploadReader) surfaces as
		// ErrUploadTooLarge wrapped inside the storage error. Map
		// it to 413 so the client gets a clear "too large" signal
		// rather than a generic 500.
		if errors.Is(err, service.ErrUploadTooLarge) {
			return nil, http.StatusRequestEntityTooLarge, "upload exceeds the configured size limit"
		}
		return nil, http.StatusInternalServerError, "Failed to upload graph: " + err.Error()
	}
	return result, 0, ""
}

// parseMultipartStream extracts the boundary from the request's
// Content-Type and returns a streaming *multipart.Reader. Unlike
// (*http.Request).ParseMultipartForm, it does not buffer file parts
// to disk — NextPart returns each part as an io.Reader.
func parseMultipartStream(r *http.Request) (string, *multipart.Reader, error) {
	ct := r.Header.Get("Content-Type")
	if !strings.HasPrefix(ct, "multipart/form-data") {
		return "", nil, errors.New("request is not multipart/form-data")
	}
	_, params, err := mime.ParseMediaType(ct)
	if err != nil {
		return "", nil, fmt.Errorf("parsing content-type: %w", err)
	}
	boundary, ok := params["boundary"]
	if !ok || boundary == "" {
		return "", nil, errors.New("missing multipart boundary")
	}
	return boundary, multipart.NewReader(r.Body, boundary), nil
}

// applyField sets one override field by form-name. Used by
// processStreamedUpload as it reads text form fields from the
// multipart stream.
func applyField(ov *uploadOverrides, name, value string) {
	switch name {
	case "name":
		ov.Name = value
	case "description":
		ov.Description = value
	case "owner":
		ov.Owner = value
	case "tags":
		ov.TagsJSON = value
	}
}

// limitedUploadReader is an io.Reader that returns ErrUploadTooLarge
// once more than N bytes have been read, instead of silently
// truncating (io.LimitReader's behavior). Used to enforce the upload
// size cap on the streaming bundle read without buffering the whole
// file.
type limitedUploadReader struct {
	R io.Reader
	N int64
}

func (l *limitedUploadReader) Read(p []byte) (int, error) {
	if l.N <= 0 {
		return 0, service.ErrUploadTooLarge
	}
	if int64(len(p)) > l.N {
		p = p[:l.N]
	}
	n, err := l.R.Read(p)
	l.N -= int64(n)
	return n, err
}

// uploadOverrides carries the optional form-field overrides the admin
// can supply alongside the bundle file. Non-empty fields win over the
// bundle-extracted metadata.
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
