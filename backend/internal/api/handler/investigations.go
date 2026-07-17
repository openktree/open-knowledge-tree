package handler

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/openktree/open-knowledge-tree/backend/internal/api/httputil"
	appmw "github.com/openktree/open-knowledge-tree/backend/internal/api/middleware"
	conceptpkg "github.com/openktree/open-knowledge-tree/backend/internal/concepts"
	"github.com/openktree/open-knowledge-tree/backend/internal/store"
)

// Investigations bundles the investigation HTTP handlers. An
// investigation is a lightweight, user-facing grouping of a subset
// of a repository's sources (and, transitively, their facts). The
// source/fact rows know nothing about investigations — only the
// investigation_sources junction records membership — so this
// handler is the sole owner of the grouping concept.
//
// All handlers are repo-scoped: they read the per-request pool and
// repository UUID from the context set by WithRepoQueries, the
// same way the source handlers do. Read endpoints require only
// authentication; mutations are gated by the `investigation`
// permission in the wiring layer.
type Investigations struct {
	deps Deps
}

func NewInvestigations(d Deps) *Investigations {
	return &Investigations{deps: d}
}

// createInvestigationRequest is the wire shape for POST
// /{repoID}/investigations. Topic is optional; an empty string is
// stored as NULL so the UI can distinguish "no topic" from "topic
// is empty".
type createInvestigationRequest struct {
	Title string `json:"title"`
	Topic string `json:"topic"`
}

// updateInvestigationRequest is the wire shape for PUT
// /{repoID}/investigations/{invID}. Topic is optional; an empty
// string clears the field (stored as NULL).
type updateInvestigationRequest struct {
	Title string `json:"title"`
	Topic string `json:"topic"`
}

// CreateInvestigation handles POST /{repoID}/investigations.
func (i *Investigations) CreateInvestigation(w http.ResponseWriter, r *http.Request) {
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

	var body createInvestigationRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		httputil.WriteError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if strings.TrimSpace(body.Title) == "" {
		httputil.WriteError(w, http.StatusBadRequest, "title is required")
		return
	}

	id := pgtype.UUID{}
	if err := id.Scan(uuid.New().String()); err != nil {
		httputil.WriteError(w, http.StatusInternalServerError, "generating id: "+err.Error())
		return
	}

	var topic *string
	if strings.TrimSpace(body.Topic) != "" {
		t := strings.TrimSpace(body.Topic)
		topic = &t
	}

	created, err := queries.CreateInvestigation(r.Context(), store.CreateInvestigationParams{
		ID:           id,
		RepositoryID: repoID,
		Title:        strings.TrimSpace(body.Title),
		Topic:        topic,
	})
	if err != nil {
		httputil.WriteError(w, http.StatusInternalServerError, "failed to create investigation")
		return
	}
	httputil.WriteJSON(w, http.StatusCreated, created)
}

// GetInvestigation handles GET /{repoID}/investigations/{invID}.
func (i *Investigations) GetInvestigation(w http.ResponseWriter, r *http.Request) {
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

	invID, err := investigationIDFromURL(r)
	if err != nil {
		httputil.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}

	inv, err := queries.GetInvestigationByID(r.Context(), invID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			httputil.WriteError(w, http.StatusNotFound, "investigation not found")
			return
		}
		httputil.WriteError(w, http.StatusInternalServerError, "failed to get investigation")
		return
	}
	if inv.RepositoryID != repoID {
		httputil.WriteError(w, http.StatusNotFound, "investigation not found")
		return
	}
	httputil.WriteJSON(w, http.StatusOK, inv)
}

// ListInvestigations handles GET /{repoID}/investigations.
func (i *Investigations) ListInvestigations(w http.ResponseWriter, r *http.Request) {
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

	limit, offset := parsePaging(r)
	search := strings.TrimSpace(r.URL.Query().Get("q"))

	inv, err := queries.ListInvestigationsByRepo(r.Context(), store.ListInvestigationsByRepoParams{
		RepositoryID: repoID,
		Column2:      search,
		Limit:        int32(limit),
		Offset:       int32(offset),
	})
	if err != nil {
		httputil.WriteError(w, http.StatusInternalServerError, "failed to list investigations")
		return
	}

	total, err := queries.CountInvestigationsByRepo(r.Context(), store.CountInvestigationsByRepoParams{
		RepositoryID: repoID,
		Column2:      search,
	})
	if err != nil {
		httputil.WriteError(w, http.StatusInternalServerError, "failed to count investigations")
		return
	}

	httputil.WriteJSON(w, http.StatusOK, pageEnvelope{
		Data:   inv,
		Total:  total,
		Limit:  limit,
		Offset: offset,
	})
}

// UpdateInvestigation handles PUT /{repoID}/investigations/{invID}.
func (i *Investigations) UpdateInvestigation(w http.ResponseWriter, r *http.Request) {
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

	invID, err := investigationIDFromURL(r)
	if err != nil {
		httputil.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}

	var body updateInvestigationRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		httputil.WriteError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if strings.TrimSpace(body.Title) == "" {
		httputil.WriteError(w, http.StatusBadRequest, "title is required")
		return
	}

	// Verify ownership before the update so a cross-repo id is a
	// 404, not a silent update on another repo's investigation.
	existing, err := queries.GetInvestigationByID(r.Context(), invID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			httputil.WriteError(w, http.StatusNotFound, "investigation not found")
			return
		}
		httputil.WriteError(w, http.StatusInternalServerError, "failed to get investigation")
		return
	}
	if existing.RepositoryID != repoID {
		httputil.WriteError(w, http.StatusNotFound, "investigation not found")
		return
	}

	var topic *string
	if strings.TrimSpace(body.Topic) != "" {
		t := strings.TrimSpace(body.Topic)
		topic = &t
	}

	updated, err := queries.UpdateInvestigation(r.Context(), store.UpdateInvestigationParams{
		ID:    invID,
		Title: strings.TrimSpace(body.Title),
		Topic: topic,
	})
	if err != nil {
		httputil.WriteError(w, http.StatusInternalServerError, "failed to update investigation")
		return
	}
	httputil.WriteJSON(w, http.StatusOK, updated)
}

// DeleteInvestigation handles DELETE /{repoID}/investigations/{invID}.
func (i *Investigations) DeleteInvestigation(w http.ResponseWriter, r *http.Request) {
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

	invID, err := investigationIDFromURL(r)
	if err != nil {
		httputil.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}

	existing, err := queries.GetInvestigationByID(r.Context(), invID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			httputil.WriteError(w, http.StatusNotFound, "investigation not found")
			return
		}
		httputil.WriteError(w, http.StatusInternalServerError, "failed to get investigation")
		return
	}
	if existing.RepositoryID != repoID {
		httputil.WriteError(w, http.StatusNotFound, "investigation not found")
		return
	}

	if err := queries.DeleteInvestigation(r.Context(), invID); err != nil {
		httputil.WriteError(w, http.StatusInternalServerError, "failed to delete investigation")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// addInvestigationSourceRequest is the wire shape for POST
// /{repoID}/investigations/{invID}/sources. The caller may pass
// either an existing source_id (the common case: the user picks a
// source from the repo-level list or a search result) or a URL to
// retrieve (the "add a new source to this investigation" flow).
// When a URL is passed without a source_id, the handler enqueues a
// retrieve_source job and, on success, records the membership once
// the source row exists — but for the v1 we require source_id and
// treat the retrieve flow as a separate step the frontend drives.
type addInvestigationSourceRequest struct {
	SourceID string `json:"source_id"`
}

// AddSource handles POST /{repoID}/investigations/{invID}/sources.
//
// Adds an existing source to the investigation. Idempotent: re-adding
// a source is a no-op (ON CONFLICT DO NOTHING). The source must
// belong to the same repository as the investigation; a cross-repo
// source_id is a 404.
func (i *Investigations) AddSource(w http.ResponseWriter, r *http.Request) {
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

	invID, err := investigationIDFromURL(r)
	if err != nil {
		httputil.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}

	// Verify investigation ownership.
	inv, err := queries.GetInvestigationByID(r.Context(), invID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			httputil.WriteError(w, http.StatusNotFound, "investigation not found")
			return
		}
		httputil.WriteError(w, http.StatusInternalServerError, "failed to get investigation")
		return
	}
	if inv.RepositoryID != repoID {
		httputil.WriteError(w, http.StatusNotFound, "investigation not found")
		return
	}

	var body addInvestigationSourceRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		httputil.WriteError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if body.SourceID == "" {
		httputil.WriteError(w, http.StatusBadRequest, "source_id is required")
		return
	}
	var sourceID pgtype.UUID
	if err := sourceID.Scan(body.SourceID); err != nil {
		httputil.WriteError(w, http.StatusBadRequest, "invalid source_id")
		return
	}

	// Verify the source belongs to the same repo.
	src, err := queries.GetSourceByID(r.Context(), sourceID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			httputil.WriteError(w, http.StatusNotFound, "source not found")
			return
		}
		httputil.WriteError(w, http.StatusInternalServerError, "failed to get source")
		return
	}
	if src.RepositoryID != repoID {
		httputil.WriteError(w, http.StatusNotFound, "source not found")
		return
	}

	if err := queries.AddInvestigationSource(r.Context(), store.AddInvestigationSourceParams{
		InvestigationID: invID,
		SourceID:        sourceID,
	}); err != nil {
		httputil.WriteError(w, http.StatusInternalServerError, "failed to add source to investigation")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// RemoveSource handles DELETE
// /{repoID}/investigations/{invID}/sources/{sourceID}.
func (i *Investigations) RemoveSource(w http.ResponseWriter, r *http.Request) {
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

	invID, err := investigationIDFromURL(r)
	if err != nil {
		httputil.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}
	sourceID, err := sourceIDFromURL(r)
	if err != nil {
		httputil.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}

	// Verify investigation ownership before removing.
	inv, err := queries.GetInvestigationByID(r.Context(), invID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			httputil.WriteError(w, http.StatusNotFound, "investigation not found")
			return
		}
		httputil.WriteError(w, http.StatusInternalServerError, "failed to get investigation")
		return
	}
	if inv.RepositoryID != repoID {
		httputil.WriteError(w, http.StatusNotFound, "investigation not found")
		return
	}

	if err := queries.RemoveInvestigationSource(r.Context(), store.RemoveInvestigationSourceParams{
		InvestigationID: invID,
		SourceID:        sourceID,
	}); err != nil {
		httputil.WriteError(w, http.StatusInternalServerError, "failed to remove source from investigation")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ListSources handles GET /{repoID}/investigations/{invID}/sources.
//
// Returns the investigation's source rows (the subset the user has
// added), with the source's status/parse_status so the Sources phase
// can show ingestion/decomposition state. Paginated and searchable
// against the source's search_tsv, mirroring the repo-level source
// list.
func (i *Investigations) ListSources(w http.ResponseWriter, r *http.Request) {
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
	invID, err := investigationIDFromURL(r)
	if err != nil {
		httputil.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}

	inv, err := queries.GetInvestigationByID(r.Context(), invID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			httputil.WriteError(w, http.StatusNotFound, "investigation not found")
			return
		}
		httputil.WriteError(w, http.StatusInternalServerError, "failed to get investigation")
		return
	}
	if inv.RepositoryID != repoID {
		httputil.WriteError(w, http.StatusNotFound, "investigation not found")
		return
	}

	limit, offset := parsePaging(r)
	search := strings.TrimSpace(r.URL.Query().Get("q"))

	sources, err := queries.ListInvestigationSources(r.Context(), store.ListInvestigationSourcesParams{
		InvestigationID: invID,
		Column2:         search,
		Limit:           int32(limit),
		Offset:          int32(offset),
	})
	if err != nil {
		httputil.WriteError(w, http.StatusInternalServerError, "failed to list investigation sources")
		return
	}

	total, err := queries.CountInvestigationSources(r.Context(), store.CountInvestigationSourcesParams{
		InvestigationID: invID,
		Column2:         search,
	})
	if err != nil {
		httputil.WriteError(w, http.StatusInternalServerError, "failed to count investigation sources")
		return
	}

	httputil.WriteJSON(w, http.StatusOK, pageEnvelope{
		Data:   sources,
		Total:  total,
		Limit:  limit,
		Offset: offset,
	})
}

// ListFacts handles GET /{repoID}/investigations/{invID}/facts.
//
// Returns facts contributed by the investigation's sources (via
// fact_sources → investigation_sources), deduped by fact_id with a
// computed source_count over all sources in the repo. Mirrors the
// repo-level fact list's status/sort/search filters and pagination.
func (i *Investigations) ListFacts(w http.ResponseWriter, r *http.Request) {
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
	invID, err := investigationIDFromURL(r)
	if err != nil {
		httputil.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}

	inv, err := queries.GetInvestigationByID(r.Context(), invID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			httputil.WriteError(w, http.StatusNotFound, "investigation not found")
			return
		}
		httputil.WriteError(w, http.StatusInternalServerError, "failed to get investigation")
		return
	}
	if inv.RepositoryID != repoID {
		httputil.WriteError(w, http.StatusNotFound, "investigation not found")
		return
	}

	statusFilter := r.URL.Query().Get("status")
	if statusFilter == "" {
		statusFilter = "stable"
	} else if statusFilter == "all" {
		statusFilter = ""
	}
	sortParam := r.URL.Query().Get("sort")
	search := strings.TrimSpace(r.URL.Query().Get("q"))
	limit, offset := parsePaging(r)

	facts, err := queries.ListInvestigationFacts(r.Context(), store.ListInvestigationFactsParams{
		InvestigationID: invID,
		Column2:         statusFilter,
		Column3:         search,
		Column4:         sortParam,
		Limit:           int32(limit),
		Offset:          int32(offset),
	})
	if err != nil {
		httputil.WriteError(w, http.StatusInternalServerError, "failed to list investigation facts")
		return
	}

	total, err := queries.CountInvestigationFacts(r.Context(), store.CountInvestigationFactsParams{
		InvestigationID: invID,
		Column2:         statusFilter,
		Column3:         search,
	})
	if err != nil {
		httputil.WriteError(w, http.StatusInternalServerError, "failed to count investigation facts")
		return
	}

	httputil.WriteJSON(w, http.StatusOK, pageEnvelope{
		Data:   facts,
		Total:  total,
		Limit:  limit,
		Offset: offset,
	})
}

// ListConcepts returns the concepts derived from the investigation's
// sources, grouped by canonical name so the UI presents "one
// concept, many contexts" instead of one row per
// (canonical_name, context). Concepts are reached via
// fact_concepts → fact_sources → investigation_sources, so only
// concepts tied to facts that came from the investigation's own
// sources are returned — a brand-new investigation with no processed
// sources returns an empty list. fact_count is the total across the
// group's contexts (computed in Go from the per-context rows);
// cross-confirmation still shows because the membership filter is
// only on which concepts appear, not on the count. Mirrors ListFacts
// for inv resolution and paging (paging is by group, in Go after
// grouping).
func (i *Investigations) ListConcepts(w http.ResponseWriter, r *http.Request) {
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
	invID, err := investigationIDFromURL(r)
	if err != nil {
		httputil.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}

	inv, err := queries.GetInvestigationByID(r.Context(), invID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			httputil.WriteError(w, http.StatusNotFound, "investigation not found")
			return
		}
		httputil.WriteError(w, http.StatusInternalServerError, "failed to get investigation")
		return
	}
	if inv.RepositoryID != repoID {
		httputil.WriteError(w, http.StatusNotFound, "investigation not found")
		return
	}

	limit, offset := parsePaging(r)

	rows, err := queries.ListGroupedInvestigationConcepts(r.Context(), invID)
	if err != nil {
		httputil.WriteError(w, http.StatusInternalServerError, "failed to list investigation concepts")
		return
	}

	total, err := queries.CountGroupedInvestigationConcepts(r.Context(), invID)
	if err != nil {
		httputil.WriteError(w, http.StatusInternalServerError, "failed to count investigation concepts")
		return
	}

	// Group per-context rows by lower(canonical_name). The
	// investigation list omits per-context aliases (the repo-level
	// detail endpoint returns them), so we pass a nil aliases map.
	groupRows := make([]conceptpkg.GroupRow, 0, len(rows))
	for _, r := range rows {
		groupRows = append(groupRows, conceptpkg.FromListGroupedInvestigationConceptsRow(r))
	}
	groups := conceptpkg.BuildGroups(groupRows, nil)
	page := conceptpkg.Paginate(groups, offset, limit)

	httputil.WriteJSON(w, http.StatusOK, pageEnvelope{
		Data:   page,
		Total:  total,
		Limit:  limit,
		Offset: offset,
	})
}

// investigationIDFromURL extracts the {invID} chi URL param and
// parses it into a pgtype.UUID. Mirrors sourceIDFromURL.
func investigationIDFromURL(r *http.Request) (pgtype.UUID, error) {
	var id pgtype.UUID
	raw := chi.URLParam(r, "invID")
	if raw == "" {
		return id, errors.New("invID is required")
	}
	if err := id.Scan(raw); err != nil {
		return id, errors.New("invalid investigation id")
	}
	return id, nil
}
