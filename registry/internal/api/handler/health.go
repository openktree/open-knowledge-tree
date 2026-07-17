package handler

import (
	"net/http"

	"github.com/openktree/knowledge-registry/internal/service"
)

type HealthHandler struct {
	svc *service.Registry
}

func NewHealthHandler(svc *service.Registry) *HealthHandler {
	return &HealthHandler{svc: svc}
}

func (h *HealthHandler) Health(w http.ResponseWriter, r *http.Request) {
	repoCount, sourceCount, err := h.svc.Stats(r.Context())
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"status": "ok",
			"error":  err.Error(),
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"status":          "ok",
		"repositories":    repoCount,
		"sources":         sourceCount,
	})
}
