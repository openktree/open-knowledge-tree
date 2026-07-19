package handler

import (
	"log"
	"net/http"
	"net/url"
	"strconv"

	"github.com/go-chi/chi/v5"

	"github.com/openktree/knowledge-registry/internal/model"
	"github.com/openktree/knowledge-registry/internal/service"
)

type SourceHandler struct {
	svc *service.Registry
}

func NewSourceHandler(svc *service.Registry) *SourceHandler {
	return &SourceHandler{svc: svc}
}

func (h *SourceHandler) Search(w http.ResponseWriter, r *http.Request) {
	q := model.SearchQuery{
		URL:   r.URL.Query().Get("url"),
		DOI:   r.URL.Query().Get("doi"),
		SHA256: r.URL.Query().Get("sha256"),
	}
	if q.URL == "" && q.DOI == "" && q.SHA256 == "" {
		writeError(w, http.StatusBadRequest, "provide url, doi, or sha256 query parameter")
		return
	}
	result, err := h.svc.SearchSource(r.Context(), q)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (h *SourceHandler) Push(w http.ResponseWriter, r *http.Request) {
	var data model.SourceData
	if err := decodeBody(r, &data); err != nil {
		log.Printf("push source: decode body: %v", err)
		writeError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}
	result, err := h.svc.PushSource(r.Context(), &data)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, result)
}

func (h *SourceHandler) PushDecomposition(w http.ResponseWriter, r *http.Request) {
	sourceID := chi.URLParam(r, "sid")
	modelID := decodeModelParam(chi.URLParam(r, "model"))
	var decomp model.DecompositionPackage
	if err := decodeBody(r, &decomp); err != nil {
		log.Printf("push decomposition: decode body: %v", err)
		writeError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}
	if decomp.ModelID == "" {
		decomp.ModelID = modelID
	}
	result, err := h.svc.PushDecomposition(r.Context(), sourceID, &decomp)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, result)
}

func (h *SourceHandler) PullSource(w http.ResponseWriter, r *http.Request) {
	sourceID := chi.URLParam(r, "sid")
	pkg, err := h.svc.PullSource(r.Context(), sourceID)
	if err != nil {
		writeError(w, http.StatusNotFound, "source not found")
		return
	}
	writeJSON(w, http.StatusOK, pkg)
}

func (h *SourceHandler) PullDecomposition(w http.ResponseWriter, r *http.Request) {
	sourceID := chi.URLParam(r, "sid")
	modelID := decodeModelParam(chi.URLParam(r, "model"))
	decomp, err := h.svc.PullDecomposition(r.Context(), sourceID, modelID)
	if err != nil {
		writeError(w, http.StatusNotFound, "decomposition not found")
		return
	}
	writeJSON(w, http.StatusOK, decomp)
}

func (h *SourceHandler) ListSources(w http.ResponseWriter, r *http.Request) {
	limit := parseIntParam(r, "limit", 20)
	offset := parseIntParam(r, "offset", 0)
	q := r.URL.Query().Get("q")

	if q != "" {
		sources, total, err := h.svc.SearchSourcesText(r.Context(), q, limit, offset)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"sources": sources,
			"total":   total,
		})
		return
	}

	sources, total, err := h.svc.ListSources(r.Context(), limit, offset)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"sources": sources,
		"total":   total,
	})
}

func (h *SourceHandler) ListDecompositions(w http.ResponseWriter, r *http.Request) {
	sourceID := chi.URLParam(r, "sid")
	decomps, err := h.svc.ListDecompositions(r.Context(), sourceID)
	if err != nil {
		writeError(w, http.StatusNotFound, "source not found")
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"decompositions": decomps})
}

func (h *SourceHandler) PresignedUploadURL(w http.ResponseWriter, r *http.Request) {
	sourceID := chi.URLParam(r, "sid")
	assetType := r.URL.Query().Get("type")
	assetID := r.URL.Query().Get("asset_id")
	url, err := h.svc.PresignedUploadURL(r.Context(), sourceID, assetType, assetID)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"url": url})
}

func parseIntParam(r *http.Request, name string, defaultVal int) int {
	raw := r.URL.Query().Get(name)
	if raw == "" {
		return defaultVal
	}
	v, err := strconv.Atoi(raw)
	if err != nil || v < 0 {
		return defaultVal
	}
	return v
}

func (h *SourceHandler) PresignedDownloadURL(w http.ResponseWriter, r *http.Request) {
	sourceID := chi.URLParam(r, "sid")
	assetType := r.URL.Query().Get("type")
	assetID := r.URL.Query().Get("asset_id")
	url, err := h.svc.PresignedDownloadURL(r.Context(), sourceID, assetType, assetID)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"url": url})
}

// decodeModelParam URL-decodes the model path parameter. Chi v5
// does not decode %2F (/) in path params by default to prevent
// path traversal, so model IDs like "google%2Fgemma-4-31b-it"
// arrive still-encoded. The registry needs the decoded form
// ("google/gemma-4-31b-it") to sanitize it for the S3 key and
// to match against the metadata DB.
func decodeModelParam(raw string) string {
	decoded, err := url.PathUnescape(raw)
	if err != nil {
		return raw
	}
	return decoded
}
