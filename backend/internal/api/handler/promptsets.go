package handler

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/openktree/open-knowledge-tree/backend/internal/api/httputil"
	"github.com/openktree/open-knowledge-tree/backend/internal/promptset"
	"github.com/openktree/open-knowledge-tree/backend/internal/rbac"
	"github.com/openktree/open-knowledge-tree/backend/internal/store"
)

// Promptsets is the HTTP handler bundle for the /api/v1/promptsets
// CRUD surface. It owns a promptset.Resolver (built-in + DB providers)
// for hash validation and a *store.Queries for persistence. The
// bundle is constructed in NewHandler and wired with the resolver in
// cmd/app/api.go (the same resolver the task workers use).
//
// Authorization:
//   - GET / (list) and GET /{hash} (read one) are open to any
//     authenticated user — they need to see the built-in + their own
//     custom promptsets to pick one for their repo.
//   - POST / (create), PUT /{hash} (update = create new), DELETE
//     /{hash} require the promptset.manage permission (granted to
//     every authenticated user by default — creating a custom
//     promptset is a user-scoped action, not an admin privilege).
//   - A user may only update/delete promptsets they own (or any if
//     sysadmin). The handler enforces ownership via the row's
//     owner_id.
type Promptsets struct {
	deps     Deps
	resolver *promptset.Resolver
}

// NewPromptsets constructs the bundle. deps.Store is the system-pool
// *store.Queries (promptsets live in okt_system). resolver may be nil
// — the bundle degrades to built-in-only (List returns just Default,
// Get returns only the built-in hash, Create/Update/Delete return
// 503). Wired by api.Handler.SetPromptsetResolver.
func NewPromptsets(d Deps) *Promptsets {
	return &Promptsets{deps: d}
}

// SetResolver wires the promptset resolver (built-in + DB). Called
// by the wiring layer after the resolver is built in cmd/app/api.go.
// Nil is safe — the bundle serves the built-in promptset only.
func (h *Promptsets) SetResolver(r *promptset.Resolver) {
	h.resolver = r
}

// List handles GET /api/v1/promptsets.
//
// Returns the built-in promptset plus every custom promptset the
// caller can see. A non-sysadmin sees the built-in + their own; a
// sysadmin sees the built-in + every custom promptset in the
// catalog. The built-in is always first so the UI can badge it as
// non-editable.
func (h *Promptsets) List(w http.ResponseWriter, r *http.Request) {
	uid := httputil.RequestUserID(r.Context())
	out := []promptset.Promptset{}
	if h.resolver != nil {
		if isSysadmin(r.Context(), h.deps.RBAC, uid) {
			out = append(out, h.resolver.List()...)
		} else {
			out = append(out, h.resolver.ListForOwner(uid.String())...)
		}
	} else {
		out = append(out, promptset.Default)
	}
	httputil.WriteJSON(w, http.StatusOK, out)
}

// Get handles GET /api/v1/promptsets/{hash}.
//
// Returns the promptset for the given hash (built-in or custom).
// A user may read any promptset they can see (their own + built-in);
// a sysadmin may read any. Returns 404 when the hash is unknown.
func (h *Promptsets) Get(w http.ResponseWriter, r *http.Request) {
	hash := strings.TrimSpace(chi.URLParam(r, "hash"))
	if hash == "" {
		httputil.WriteError(w, http.StatusBadRequest, "hash is required")
		return
	}
	if h.resolver == nil {
		httputil.WriteError(w, http.StatusServiceUnavailable, "promptset resolver not configured")
		return
	}
	ps, ok := h.resolver.Get(hash)
	if !ok {
		httputil.WriteError(w, http.StatusNotFound, "promptset not found")
		return
	}
	// Ownership check: a non-sysadmin may only read their own custom
	// promptsets (plus the built-in, which has no owner).
	if ps.Source == promptset.CustomSource {
		uid := httputil.RequestUserID(r.Context())
		if !isSysadmin(r.Context(), h.deps.RBAC, uid) && ps.OwnerID != uid.String() {
			httputil.WriteError(w, http.StatusNotFound, "promptset not found")
			return
		}
	}
	httputil.WriteJSON(w, http.StatusOK, ps)
}

// createBody is the POST /api/v1/promptsets request body. The hash is
// computed server-side from the 8 phase strings, so the client does
// not send it. An incomplete promptset (any empty phase) is rejected
// with a 400 naming the missing phases.
type createBody struct {
	Name                string `json:"name"`
	FactExtraction      string `json:"fact_extraction"`
	ImageFactExtraction string `json:"image_fact_extraction"`
	ConceptExtraction   string `json:"concept_extraction"`
	Refinement          string `json:"refinement"`
	Synthesis           string `json:"synthesis"`
	ImagePicker         string `json:"image_picker"`
	Summarization       string `json:"summarization"`
	Posture             string `json:"posture"`
}

// Create handles POST /api/v1/promptsets.
//
// Creates a new custom promptset. The hash is computed server-side
// from the 8 phase strings (the client does not send it). An
// incomplete promptset (any empty phase) is rejected with a 400
// naming the missing phases. When the computed hash already exists
// (same 8 phase strings as an existing row), the upsert updates the
// name + owner + returns the existing row — two promptsets with the
// same prompts are the same philosophy.
func (h *Promptsets) Create(w http.ResponseWriter, r *http.Request) {
	if h.resolver == nil || h.deps.Store == nil {
		httputil.WriteError(w, http.StatusServiceUnavailable, "promptset resolver not configured")
		return
	}
	var body createBody
	if err := httputil.DecodeBody(r, &body); err != nil {
		httputil.WriteError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if strings.TrimSpace(body.Name) == "" {
		httputil.WriteError(w, http.StatusBadRequest, "name is required")
		return
	}
	ps := promptset.Promptset{
		Name:                body.Name,
		Source:              promptset.CustomSource,
		FactExtraction:      body.FactExtraction,
		ImageFactExtraction: body.ImageFactExtraction,
		ConceptExtraction:   body.ConceptExtraction,
		Refinement:          body.Refinement,
		Synthesis:           body.Synthesis,
		ImagePicker:         body.ImagePicker,
		Summarization:       body.Summarization,
		Posture:             body.Posture,
	}
	if !ps.IsComplete() {
		httputil.WriteError(w, http.StatusBadRequest, "promptset is incomplete; missing phases: "+joinPhases(ps.MissingPhases()))
		return
	}
	ps = ps.WithHash()
	uid := httputil.RequestUserID(r.Context())
	ownerID := pgtype.UUID{}
	if uid.Valid {
		ownerID = uid
	}
	row, err := h.deps.Store.UpsertPromptset(r.Context(), store.UpsertPromptsetParams{
		Hash:                ps.Hash,
		Name:                ps.Name,
		OwnerID:             ownerID,
		FactExtraction:      ps.FactExtraction,
		ImageFactExtraction: ps.ImageFactExtraction,
		ConceptExtraction:   ps.ConceptExtraction,
		Refinement:          ps.Refinement,
		Synthesis:           ps.Synthesis,
		ImagePicker:         ps.ImagePicker,
		Summarization:       ps.Summarization,
		Posture:             ps.Posture,
	})
	if err != nil {
		httputil.WriteError(w, http.StatusInternalServerError, "failed to create promptset")
		return
	}
	httputil.WriteJSON(w, http.StatusCreated, rowToPromptset(row))
}

// Update handles PUT /api/v1/promptsets/{hash}.
//
// Editing a custom promptset creates a NEW row (new hash), since the
// hash is the identity and a changed prompt is a new philosophy. The
// old row stays so repos pointing at it keep working. The response is
// the new promptset (with its new hash). The caller must own the
// old promptset (or be sysadmin) to edit it.
func (h *Promptsets) Update(w http.ResponseWriter, r *http.Request) {
	if h.resolver == nil || h.deps.Store == nil {
		httputil.WriteError(w, http.StatusServiceUnavailable, "promptset resolver not configured")
		return
	}
	oldHash := strings.TrimSpace(chi.URLParam(r, "hash"))
	if oldHash == "" {
		httputil.WriteError(w, http.StatusBadRequest, "hash is required")
		return
	}
	// The built-in promptset is immutable.
	if oldHash == promptset.DefaultHash {
		httputil.WriteError(w, http.StatusBadRequest, "the built-in promptset is immutable; create a custom promptset instead")
		return
	}
	// Load the old row to enforce ownership.
	old, err := h.deps.Store.GetPromptsetByHash(r.Context(), oldHash)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			httputil.WriteError(w, http.StatusNotFound, "promptset not found")
			return
		}
		httputil.WriteError(w, http.StatusInternalServerError, "failed to load promptset")
		return
	}
	uid := httputil.RequestUserID(r.Context())
	if !isSysadmin(r.Context(), h.deps.RBAC, uid) {
		if !old.OwnerID.Valid || old.OwnerID.String() != uid.String() {
			httputil.WriteError(w, http.StatusForbidden, "you do not own this promptset")
			return
		}
	}
	var body createBody
	if err := httputil.DecodeBody(r, &body); err != nil {
		httputil.WriteError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if strings.TrimSpace(body.Name) == "" {
		httputil.WriteError(w, http.StatusBadRequest, "name is required")
		return
	}
	ps := promptset.Promptset{
		Name:                body.Name,
		Source:              promptset.CustomSource,
		FactExtraction:      body.FactExtraction,
		ImageFactExtraction: body.ImageFactExtraction,
		ConceptExtraction:   body.ConceptExtraction,
		Refinement:          body.Refinement,
		Synthesis:           body.Synthesis,
		ImagePicker:         body.ImagePicker,
		Summarization:       body.Summarization,
		Posture:             body.Posture,
	}
	if !ps.IsComplete() {
		httputil.WriteError(w, http.StatusBadRequest, "promptset is incomplete; missing phases: "+joinPhases(ps.MissingPhases()))
		return
	}
	ps = ps.WithHash()
	ownerID := old.OwnerID
	row, err := h.deps.Store.UpsertPromptset(r.Context(), store.UpsertPromptsetParams{
		Hash:                ps.Hash,
		Name:                ps.Name,
		OwnerID:             ownerID,
		FactExtraction:      ps.FactExtraction,
		ImageFactExtraction: ps.ImageFactExtraction,
		ConceptExtraction:   ps.ConceptExtraction,
		Refinement:          ps.Refinement,
		Synthesis:           ps.Synthesis,
		ImagePicker:         ps.ImagePicker,
		Summarization:       ps.Summarization,
		Posture:             ps.Posture,
	})
	if err != nil {
		httputil.WriteError(w, http.StatusInternalServerError, "failed to create promptset")
		return
	}
	httputil.WriteJSON(w, http.StatusCreated, rowToPromptset(row))
}

// Delete handles DELETE /api/v1/promptsets/{hash}.
//
// Deletes a custom promptset the caller owns (or any if sysadmin).
// The built-in promptset cannot be deleted. Deleting a promptset
// does NOT cascade to repositories that reference it — their
// active_promptset_hash / accepted_promptset_hashes are plain text
// with no FK, so a repo pointing at a deleted promptset falls back
// to the global default at resolve time.
func (h *Promptsets) Delete(w http.ResponseWriter, r *http.Request) {
	if h.deps.Store == nil {
		httputil.WriteError(w, http.StatusServiceUnavailable, "promptset store not configured")
		return
	}
	hash := strings.TrimSpace(chi.URLParam(r, "hash"))
	if hash == "" {
		httputil.WriteError(w, http.StatusBadRequest, "hash is required")
		return
	}
	if hash == promptset.DefaultHash {
		httputil.WriteError(w, http.StatusBadRequest, "the built-in promptset cannot be deleted")
		return
	}
	row, err := h.deps.Store.GetPromptsetByHash(r.Context(), hash)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			httputil.WriteError(w, http.StatusNotFound, "promptset not found")
			return
		}
		httputil.WriteError(w, http.StatusInternalServerError, "failed to load promptset")
		return
	}
	uid := httputil.RequestUserID(r.Context())
	if !isSysadmin(r.Context(), h.deps.RBAC, uid) {
		if !row.OwnerID.Valid || row.OwnerID.String() != uid.String() {
			httputil.WriteError(w, http.StatusForbidden, "you do not own this promptset")
			return
		}
	}
	if err := h.deps.Store.DeletePromptset(r.Context(), hash); err != nil {
		httputil.WriteError(w, http.StatusInternalServerError, "failed to delete promptset")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// rowToPromptset converts a sqlc row into a Promptset for the JSON
// response. Mirrors promptset.rowToPromptset but lives in the
// handler package to avoid an import cycle (handler imports
// promptset; promptset can't import handler).
func rowToPromptset(r store.OktSystemPromptset) promptset.Promptset {
	owner := ""
	if r.OwnerID.Valid {
		owner = r.OwnerID.String()
	}
	return promptset.Promptset{
		Hash:                r.Hash,
		Name:                r.Name,
		OwnerID:             owner,
		Source:              promptset.CustomSource,
		FactExtraction:      r.FactExtraction,
		ImageFactExtraction: r.ImageFactExtraction,
		ConceptExtraction:   r.ConceptExtraction,
		Refinement:          r.Refinement,
		Synthesis:           r.Synthesis,
		ImagePicker:         r.ImagePicker,
		Summarization:       r.Summarization,
		Posture:             r.Posture,
	}
}

// joinPhases renders a []PhaseLabel as a comma-separated string for
// 400 error messages.
func joinPhases(ps []promptset.PhaseLabel) string {
	out := make([]string, 0, len(ps))
	for _, p := range ps {
		out = append(out, string(p))
	}
	return strings.Join(out, ", ")
}

// isSysadmin reports whether the user has the sysadmin role
// (granted the wildcard */* policy). Used to gate the sysadmin-only
// views (list all promptsets, update/delete any). Falls back to
// false when RBAC is nil.
func isSysadmin(ctx context.Context, rbacSvc *rbac.Service, uid pgtype.UUID) bool {
	if rbacSvc == nil || !uid.Valid {
		return false
	}
	ok, _ := rbacSvc.EnforceSystemAdmin(uid.String())
	return ok
}