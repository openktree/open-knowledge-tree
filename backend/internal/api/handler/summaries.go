package handler

import (
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/openktree/open-knowledge-tree/backend/internal/api/httputil"
	appmw "github.com/openktree/open-knowledge-tree/backend/internal/api/middleware"
	"github.com/openktree/open-knowledge-tree/backend/internal/store"
)

// Summaries bundles the concept-summary HTTP handlers. A
// concept_summary is an incremental LLM-produced synthesis of the
// facts linked to a (concept, context) pair, produced by the
// summarize_concepts worker (which fans out from extract_concepts in
// parallel with embed_concepts). Summaries are sliced into
// BatchSize-fact chunks; the oldest slice stays "open" (regenerated
// as new facts arrive) until it reaches BatchSize and freezes.
//
// All handlers are repo-scoped: they read the per-request pool from
// the context set by WithRepoQueries, the same way the concepts and
// source handlers do. Read endpoints require only authentication;
// summary creation is task-driven (the summarize_concepts worker),
// not HTTP-driven.
type Summaries struct {
	deps Deps
}

func NewSummaries(deps Deps) *Summaries {
	return &Summaries{deps: deps}
}

// ListByConcept handles GET /{repoID}/concepts/{conceptID}/summaries.
// Returns every summary slice for the concept, ordered by
// sequence_num (oldest slice first). Each slice carries its
// is_complete flag (FALSE = the open accumulator still being
// regenerated as new facts arrive; TRUE = a frozen slice covering
// exactly BatchSize facts) and its covered_fact_ids array so the
// client can map a fact to the slice that cites it. A cross-repo
// conceptID is a 404 (the concept must belong to the route's repo).
func (s *Summaries) ListByConcept(w http.ResponseWriter, r *http.Request) {
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

	conceptID, err := conceptIDFromURL(r)
	if err != nil {
		httputil.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}

	// Verify the concept belongs to the repo so a cross-repo id is
	// a 404, not a silent listing of another repo's summaries.
	concept, err := queries.GetConceptByID(r.Context(), conceptID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			httputil.WriteError(w, http.StatusNotFound, "concept not found")
			return
		}
		httputil.WriteError(w, http.StatusInternalServerError, "failed to get concept")
		return
	}
	if concept.RepositoryID != repoID {
		httputil.WriteError(w, http.StatusNotFound, "concept not found")
		return
	}

	summaries, err := queries.ListSummariesByConcept(r.Context(), conceptID)
	if err != nil {
		httputil.WriteError(w, http.StatusInternalServerError, "failed to list summaries for concept")
		return
	}

	total, err := queries.CountSummariesByConcept(r.Context(), conceptID)
	if err != nil {
		httputil.WriteError(w, http.StatusInternalServerError, "failed to count summaries for concept")
		return
	}

	// No pagination: a concept's summary count is bounded by
	// ceil(fact_count / BatchSize), which is small even for large
	// concepts. The page envelope is still used so the response shape
	// matches the other list endpoints.
	limit := len(summaries)
	if limit == 0 {
		limit = 1
	}
	httputil.WriteJSON(w, http.StatusOK, pageEnvelope{
		Data:   summaries,
		Total:  total,
		Limit:  limit,
		Offset: 0,
	})
}

// guard against unused imports if the file is edited.
var _ = chi.URLParam
var _ pgtype.UUID