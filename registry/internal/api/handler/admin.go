package handler

import (
	"net/http"

	"github.com/openktree/knowledge-registry/internal/auth"
	"github.com/openktree/knowledge-registry/internal/store"
)

type AdminHandler struct {
	store store.MetadataStore
}

func NewAdminHandler(store store.MetadataStore) *AdminHandler {
	return &AdminHandler{store: store}
}

type updateRoleRequest struct {
	Role string `json:"role"`
}

func (h *AdminHandler) ListUsers(w http.ResponseWriter, r *http.Request) {
	users, err := h.store.ListUsers(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list users")
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"users": users})
}

func (h *AdminHandler) UpdateRole(w http.ResponseWriter, r *http.Request) {
	userID := r.PathValue("id")
	if userID == "" {
		writeError(w, http.StatusBadRequest, "user id is required")
		return
	}

	var req updateRoleRequest
	if err := decodeBody(r, &req); err != nil || req.Role == "" {
		writeError(w, http.StatusBadRequest, "role is required")
		return
	}

	if req.Role != "viewer" && req.Role != "editor" && req.Role != "admin" {
		writeError(w, http.StatusBadRequest, "role must be viewer, editor, or admin")
		return
	}

	actingRole := auth.RequestUserRole(r.Context())
	if actingRole == "admin" {
		if err := h.store.UpdateUserRole(r.Context(), userID, req.Role); err != nil {
			writeError(w, http.StatusNotFound, "user not found")
			return
		}
	} else {
		if err := h.store.UpdateUserRole(r.Context(), userID, req.Role); err != nil {
			writeError(w, http.StatusNotFound, "user not found")
			return
		}
	}

	writeJSON(w, http.StatusOK, map[string]string{"message": "role updated"})
}
