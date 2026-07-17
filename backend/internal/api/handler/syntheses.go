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

// Syntheses bundles the concept-synthesis ("definition") HTTP
// handlers. A concept_synthesis is the single authoritative definition
// the synthesize_concept worker produces for a canonical-name group,
// folding ALL of the group's summary slices into one markdown body.
// Synthesis is task-driven (the synthesize_concept worker, chained
// from summarize_concepts), not HTTP-driven; this handler is
// read-only.
//
// All handlers are repo-scoped: they read the per-request pool from
// the context set by WithRepoQueries. The definition endpoint
// resolves the route's concept_id to its canonical-name group and
// returns the group's single synthesis row plus the eager-loaded
// image facts (id, image_url, text) for every image the synthesis
// embeds, so the frontend can resolve storage URLs to authenticated
// blob URLs without N extra calls.
type Syntheses struct {
	deps Deps
}

func NewSyntheses(deps Deps) *Syntheses {
	return &Syntheses{deps: deps}
}

// definitionResponse is the JSON body returned by GetDefinition. The
// frontend renders synthesis.content as markdown, rewriting
// [text](<fact:fact_id>) citations to fact-detail links and
// ![alt](<fact:fact_id>) image citations to renderable image URLs
// resolved from the images array.
type definitionResponse struct {
	Synthesis store.OktRepositoryConceptSynthesis `json:"synthesis"`
	Images    []definitionImage                   `json:"images"`
}

// definitionImage is one eager-loaded image fact the synthesis
// embeds. ImageURL is the service-routable URL the frontend resolves
// to a blob URL via the storage endpoint when it points at our own
// storage, or passes through to <img> when it is an external URL.
type definitionImage struct {
	ID       string `json:"id"`
	Text     string `json:"text"`
	ImageURL string `json:"image_url"`
	FactKind string `json:"fact_kind"`
}

// GetDefinition handles GET /{repoID}/concepts/{conceptID}/definition.
// Returns the single synthesis for the concept_id's canonical-name
// group, plus the eager-loaded image facts the synthesis embeds. A
// cross-repo conceptID is a 404 (the concept must belong to the
// route's repo). 404 when no synthesis exists yet (the
// synthesize_concept worker hasn't run, or the concept has no
// summary slices).
func (s *Syntheses) GetDefinition(w http.ResponseWriter, r *http.Request) {
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
	// a 404, not a silent return of another repo's definition.
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

	syn, err := queries.GetSynthesisByGroup(r.Context(), store.GetSynthesisByGroupParams{
		RepositoryID:  repoID,
		CanonicalName: concept.CanonicalName,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			httputil.WriteError(w, http.StatusNotFound, "no definition yet for this concept")
			return
		}
		httputil.WriteError(w, http.StatusInternalServerError, "failed to get definition")
		return
	}

	// Eager-load the embedded image facts so the frontend can resolve
	// storage URLs without N extra calls. A synthesis with no embedded
	// images (embedded_image_ids = '{}') yields an empty array.
	var images []definitionImage
	if len(syn.EmbeddedImageIds) > 0 {
		rows, err := queries.ListImageFactsByIDs(r.Context(), syn.EmbeddedImageIds)
		if err != nil {
			httputil.WriteError(w, http.StatusInternalServerError, "failed to load definition images")
			return
		}
		images = make([]definitionImage, 0, len(rows))
		for _, row := range rows {
			url := ""
			if row.ImageUrl != nil {
				url = *row.ImageUrl
			}
			images = append(images, definitionImage{
				ID:       pgUUIDToStringHandler(row.ID),
				Text:     row.Text,
				ImageURL: url,
				FactKind: row.FactKind,
			})
		}
	}

	httputil.WriteJSON(w, http.StatusOK, definitionResponse{
		Synthesis: syn,
		Images:    images,
	})
}

// pgUUIDToStringHandler renders a pgtype.UUID as the canonical
// lowercase 36-char string. Mirrors the tasks.pgUUIDToString helper
// but lives in the handler package to avoid an import cycle.
func pgUUIDToStringHandler(u pgtype.UUID) string {
	if !u.Valid {
		return ""
	}
	return u.String()
}

// guard against unused imports if the file is edited.
var _ = chi.URLParam