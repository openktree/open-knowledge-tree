package handler

import (
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/openktree/open-knowledge-tree/backend/internal/api/httputil"
	appmw "github.com/openktree/open-knowledge-tree/backend/internal/api/middleware"
	"github.com/openktree/open-knowledge-tree/backend/internal/providers/storage"
	"github.com/openktree/open-knowledge-tree/backend/internal/store"
)

// Storage bundles the HTTP handlers that serve stored source assets
// (inline images, PDF page renders, full PDF source bodies). The
// bundle owns no state beyond the storage backend; per-repo pool
// resolution comes from the WithRepoQueries middleware the route is
// mounted under, exactly like the source detail handlers.
//
// Authorization: the routes are mounted inside the per-repo route
// group, so WithRepoQueries has already resolved the slug to a
// repository UUID (404 when the slug is unknown) and AuthRequired
// has validated the session. A logged-in non-member can still hit
// these endpoints today, mirroring the existing source-detail
// posture; tightening to a repo-membership check is a follow-up.
type Storage struct {
	backend storage.FileStorage
}

// Backend returns the underlying FileStorage. Exported so the wiring
// layer can hand the same backend to the graph handler (for the
// upload-graph-bundle air-gapped import path) without rebuilding it.
func (s *Storage) Backend() storage.FileStorage { return s.backend }

// NewStorage constructs a Storage handler bundle. `backend` may be
// nil — the handlers return 503 in that case so a deployment that
// disabled storage still serves the rest of the API.
func NewStorage(backend storage.FileStorage) *Storage {
	return &Storage{backend: backend}
}

// ServeSourceImage handles
// GET /api/v1/repositories/{repoID}/sources/{sourceID}/images/{imageID}.
//
// It looks up the source_images row by {imageID}, verifies the row's
// source_id matches the route's {sourceID} (defense in depth against
// cross-source image access via ID guessing), and streams the stored
// bytes back with the recorded Content-Type and ETag. Honors
// If-None-Match for 304 short-circuits.
func (s *Storage) ServeSourceImage(w http.ResponseWriter, r *http.Request) {
	if s.backend == nil {
		httputil.WriteError(w, http.StatusServiceUnavailable, "storage not configured")
		return
	}

	pool := appmw.PoolFromContext(r.Context())
	if pool == nil {
		httputil.WriteError(w, http.StatusInternalServerError, "no per-repo pool on request context")
		return
	}
	queries := store.New(pool)

	sourceID, err := sourceIDFromURL(r)
	if err != nil {
		httputil.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}

	imageIDRaw := chi.URLParam(r, "imageID")
	if imageIDRaw == "" {
		httputil.WriteError(w, http.StatusBadRequest, "imageID is required")
		return
	}
	var imageID pgtype.UUID
	if err := imageID.Scan(imageIDRaw); err != nil {
		httputil.WriteError(w, http.StatusBadRequest, "invalid image id")
		return
	}

	img, err := queries.GetSourceImageByID(r.Context(), imageID)
	if err != nil {
		httputil.WriteError(w, http.StatusNotFound, "image not found")
		return
	}
	// Defense in depth: the image must belong to the source in the
	// URL. A request with a valid imageID from another source is
	// rejected as 404 so an attacker gains no signal from probing.
	if img.SourceID != sourceID {
		httputil.WriteError(w, http.StatusNotFound, "image not found")
		return
	}
	if img.StorageKey == nil || *img.StorageKey == "" {
		// Not yet mirrored — the frontend should fall back to the
		// external url. 404 tells it to do that.
		httputil.WriteError(w, http.StatusNotFound, "image not stored")
		return
	}

	file, err := s.backend.Get(r.Context(), *img.StorageKey)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			httputil.WriteError(w, http.StatusNotFound, "image file missing")
			return
		}
		httputil.WriteError(w, http.StatusInternalServerError, "storage error")
		return
	}
	defer file.Body.Close()

	// Prefer the content-type recorded at Store time (sniffed from
	// the upstream response); fall back to what the backend sniffed
	// on Get. Either way set a Content-Type so browsers render
	// instead of downloading.
	ct := ""
	if img.ContentType != nil && *img.ContentType != "" {
		ct = *img.ContentType
	}
	if ct == "" {
		ct = file.ContentType
	}
	if ct == "" {
		ct = "application/octet-stream"
	}

	w.Header().Set("Content-Type", ct)
	if file.Size > 0 {
		w.Header().Set("Content-Length", strconv.FormatInt(file.Size, 10))
	}
	if file.ETag != "" {
		w.Header().Set("ETag", file.ETag)
	}
	if !file.ModTime.IsZero() {
		w.Header().Set("Last-Modified", file.ModTime.UTC().Format(time.RFC1123))
	}
	// Cache stored assets for a short window on the client. The
	// content is immutable (a new store overwrites the key with
	// new bytes and bumps the row's mirrored_at, but the URL stays
	// stable so a stale cache is at worst a few minutes behind a
	// re-mirror).
	w.Header().Set("Cache-Control", "private, max-age=300")

	// If-None-Match short-circuit. Browsers send this when they
	// already have the ETag cached; replying 304 with no body saves
	// the bandwidth and the round-trip to disk.
	if file.ETag != "" {
		if match := r.Header.Get("If-None-Match"); match != "" {
			for _, t := range strings.Split(match, ",") {
				if strings.TrimSpace(t) == file.ETag {
					w.WriteHeader(http.StatusNotModified)
					return
				}
			}
		}
	}

	w.WriteHeader(http.StatusOK)
	// Stream the body. http.ServeContent would also work and would
	// add range support, but stored assets are small (images, page
	// renders) and we don't need range today. A plain Copy keeps
	// the dependency surface narrow.
	_, _ = io.Copy(w, file.Body)
}

// ServeSourceBody handles
// GET /api/v1/repositories/{repoID}/sources/{sourceID}/body.
//
// It serves the stored full source body (today: PDFs only). HTML /
// text sources are NOT stored this way — their content preview lives
// in the sources.content column — so this endpoint returns 404 for
// any source whose storage_key is NULL.
func (s *Storage) ServeSourceBody(w http.ResponseWriter, r *http.Request) {
	if s.backend == nil {
		httputil.WriteError(w, http.StatusServiceUnavailable, "storage not configured")
		return
	}

	pool := appmw.PoolFromContext(r.Context())
	if pool == nil {
		httputil.WriteError(w, http.StatusInternalServerError, "no per-repo pool on request context")
		return
	}
	queries := store.New(pool)

	repoID, err := repoIDFromURL(r)
	if err != nil {
		httputil.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}
	sourceID, err := sourceIDFromURL(r)
	if err != nil {
		httputil.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}

	src, err := queries.GetSourceByID(r.Context(), sourceID)
	if err != nil {
		httputil.WriteError(w, http.StatusNotFound, "source not found")
		return
	}
	if src.RepositoryID != repoID {
		httputil.WriteError(w, http.StatusNotFound, "source not found")
		return
	}
	if src.StorageKey == nil || *src.StorageKey == "" {
		httputil.WriteError(w, http.StatusNotFound, "source body not stored")
		return
	}

	file, err := s.backend.Get(r.Context(), *src.StorageKey)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			httputil.WriteError(w, http.StatusNotFound, "source body file missing")
			return
		}
		httputil.WriteError(w, http.StatusInternalServerError, "storage error")
		return
	}
	defer file.Body.Close()

	ct := ""
	if src.ContentType != nil && *src.ContentType != "" {
		ct = *src.ContentType
	}
	if ct == "" {
		ct = file.ContentType
	}
	if ct == "" {
		ct = "application/octet-stream"
	}

	w.Header().Set("Content-Type", ct)
	if file.Size > 0 {
		w.Header().Set("Content-Length", strconv.FormatInt(file.Size, 10))
	}
	if file.ETag != "" {
		w.Header().Set("ETag", file.ETag)
	}
	if !file.ModTime.IsZero() {
		w.Header().Set("Last-Modified", file.ModTime.UTC().Format(time.RFC1123))
	}
	// PDFs can be larger; allow client caching for the same reason
	// as images (immutable content, stable URL).
	w.Header().Set("Cache-Control", "private, max-age=300")

	if file.ETag != "" {
		if match := r.Header.Get("If-None-Match"); match != "" {
			for _, t := range strings.Split(match, ",") {
				if strings.TrimSpace(t) == file.ETag {
					w.WriteHeader(http.StatusNotModified)
					return
				}
			}
		}
	}

	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, file.Body)
}
