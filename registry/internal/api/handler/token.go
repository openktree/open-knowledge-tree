package handler

import (
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/openktree/knowledge-registry/internal/auth"
	"github.com/openktree/knowledge-registry/internal/config"
	"github.com/openktree/knowledge-registry/internal/model"
	"github.com/openktree/knowledge-registry/internal/store"
)

type TokenHandler struct {
	store     store.MetadataStore
	authCfg   *config.AuthConfig
	jwtSecret string
}

func NewTokenHandler(store store.MetadataStore, cfg *config.AuthConfig) *TokenHandler {
	return &TokenHandler{store: store, authCfg: cfg, jwtSecret: cfg.JWTSecret}
}

type createTokenRequest struct {
	Name   string `json:"name"`
	Scope  string `json:"scope"`
	Expiry int    `json:"expires_in_days"`
}

type createTokenResponse struct {
	ID        string  `json:"id"`
	Name      string  `json:"name"`
	Token     string  `json:"token"`
	Scope     string  `json:"scope"`
	ExpiresAt *string `json:"expires_at,omitempty"`
	CreatedAt string  `json:"created_at"`
}

func (h *TokenHandler) List(w http.ResponseWriter, r *http.Request) {
	userID := auth.RequestUser(r.Context())
	if userID == "" {
		writeError(w, http.StatusUnauthorized, "not authenticated")
		return
	}
	tokens, err := h.store.ListAPITokens(r.Context(), userID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list tokens")
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"tokens": tokens})
}

func (h *TokenHandler) Create(w http.ResponseWriter, r *http.Request) {
	userID := auth.RequestUser(r.Context())
	if userID == "" {
		writeError(w, http.StatusUnauthorized, "not authenticated")
		return
	}

	var req createTokenRequest
	if err := decodeBody(r, &req); err != nil || req.Name == "" {
		writeError(w, http.StatusBadRequest, "name is required")
		return
	}
	if req.Scope == "" {
		req.Scope = "read"
	}
	if req.Scope != "read" && req.Scope != "write" && req.Scope != "readwrite" {
		writeError(w, http.StatusBadRequest, "scope must be read, write, or readwrite")
		return
	}

	rawToken, err := auth.GenerateAPIToken()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to generate token")
		return
	}

	now := time.Now()
	var expiresAt *time.Time
	if req.Expiry > 0 {
		t := now.Add(time.Duration(req.Expiry) * 24 * time.Hour)
		expiresAt = &t
	}

	tok := &model.APIToken{
		ID:        uuid.New().String(),
		UserID:    userID,
		Name:      req.Name,
		TokenHash: auth.HashToken(rawToken),
		Scope:     req.Scope,
		ExpiresAt: expiresAt,
		CreatedAt: now,
	}

	if err := h.store.CreateAPIToken(r.Context(), tok); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to create token")
		return
	}

	writeJSON(w, http.StatusCreated, createTokenResponse{
		ID:        tok.ID,
		Name:      tok.Name,
		Token:     rawToken,
		Scope:     tok.Scope,
		CreatedAt: tok.CreatedAt.UTC().Format(time.RFC3339),
	})
}

func (h *TokenHandler) Revoke(w http.ResponseWriter, r *http.Request) {
	userID := auth.RequestUser(r.Context())
	tokenID := chi.URLParam(r, "id")
	role := auth.RequestUserRole(r.Context())

	if role == "admin" {
		if err := h.store.RevokeAPIToken(r.Context(), tokenID, ""); err != nil {
			writeError(w, http.StatusNotFound, "token not found")
			return
		}
	} else {
		if err := h.store.RevokeAPIToken(r.Context(), tokenID, userID); err != nil {
			writeError(w, http.StatusNotFound, "token not found")
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]string{"message": "token revoked"})
}
