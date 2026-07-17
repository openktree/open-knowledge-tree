package handler

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/openktree/open-knowledge-tree/backend/internal/api/httputil"
	appmw "github.com/openktree/open-knowledge-tree/backend/internal/api/middleware"
	"github.com/openktree/open-knowledge-tree/backend/internal/store"
)

// Reports bundles the report HTTP handlers. A report is a user-
// authored markdown document that is automatically annotated with
// supporting facts from the repository: an async River job chunks
// the report into sentences, embeds each, and searches Qdrant for
// similar facts; matches persist in report_annotations so the UI
// can render each sentence alongside its auto-cited facts.
//
// All handlers are repo-scoped: they read the per-request pool and
// repository UUID from the context set by WithRepoQueries, the
// same way the source/investigation handlers do. Read endpoints
// require only authentication; mutations are gated by the `report`
// permission in the wiring layer.
type Reports struct {
	deps          Deps
	taskEnqueuer  TaskEnqueuer
}

func NewReports(d Deps) *Reports {
	return &Reports{deps: d}
}

// SetTaskEnqueuer attaches the background-task enqueuer the create/
// upload/annotate handlers use to insert annotate_report jobs.
// Optional: when nil, the handlers return 503. Idempotent.
func (r *Reports) SetTaskEnqueuer(eq TaskEnqueuer) {
	r.taskEnqueuer = eq
}

// createReportRequest is the wire shape for POST /{repoID}/reports
// (JSON path). Title is required; topic is optional (empty → NULL);
// text is the raw markdown body (required). Optional parent_id sets
// the parent report (must belong to the same repo); optional
// children_ids reparents existing reports under this new report
// (the meta-synthesis case where the parent is created after its
// children).
type createReportRequest struct {
	Title      string   `json:"title"`
	Topic      string   `json:"topic"`
	Text       string   `json:"text"`
	ParentID   string   `json:"parent_id"`
	ChildrenIDs []string `json:"children_ids"`
}

// updateReportRequest is the wire shape for PUT
// /{repoID}/reports/{reportID}. Topic is optional (empty → NULL).
// When body_md differs from the stored value the handler re-enqueues
// an annotate_report job so the annotations track the new text.
// Optional parent_id reparents this report; optional children_ids
// reparents those reports under this one. Reparenting alone does not
// re-enqueue annotation.
type updateReportRequest struct {
	Title       string   `json:"title"`
	Topic       string   `json:"topic"`
	Text        string   `json:"text"`
	ParentID    *string  `json:"parent_id"`
	ChildrenIDs []string `json:"children_ids"`
}

// uploadReportResponse is the wire shape for POST /reports and
// POST /reports/upload (202 Accepted).
type uploadReportResponse struct {
	ReportID string `json:"report_id"`
	JobID    string `json:"job_id"`
	Status   string `json:"status"`
}

// CreateReport handles POST /{repoID}/reports.
//
// Accepts JSON {title, topic, text}. Stores the report in `pending`
// status and enqueues an annotate_report job. Returns 202 with the
// report id + job id so the caller can poll status.
func (r *Reports) CreateReport(w http.ResponseWriter, req *http.Request) {
	pool := appmw.PoolFromContext(req.Context())
	if pool == nil {
		httputil.WriteError(w, http.StatusInternalServerError, "no per-repo pool on request context")
		return
	}
	queries := store.New(pool)

	repoID, err := repoIDFromURL(req)
	if err != nil {
		httputil.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}

	var body createReportRequest
	if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
		httputil.WriteError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if strings.TrimSpace(body.Title) == "" {
		httputil.WriteError(w, http.StatusBadRequest, "title is required")
		return
	}
	if strings.TrimSpace(body.Text) == "" {
		httputil.WriteError(w, http.StatusBadRequest, "text is required")
		return
	}

	r.createAndEnqueue(w, req, queries, repoID, strings.TrimSpace(body.Title), body.Topic, body.Text, body.ParentID, body.ChildrenIDs)
}

// UploadReport handles POST /{repoID}/reports/upload.
//
// Accepts multipart/form-data with a `file` field (.md/.txt) or
// application/json {title, topic, text}. Mirrors Source.UploadSource's
// two-shape parsing. Stores the report in `pending` status and enqueues
// an annotate_report job. Returns 202 with the report id + job id.
func (r *Reports) UploadReport(w http.ResponseWriter, req *http.Request) {
	pool := appmw.PoolFromContext(req.Context())
	if pool == nil {
		httputil.WriteError(w, http.StatusInternalServerError, "no per-repo pool on request context")
		return
	}
	queries := store.New(pool)

	repoID, err := repoIDFromURL(req)
	if err != nil {
		httputil.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}

	var (
		title       string
		topic       string
		text        string
		parentID    string
		childrenIDs []string
	)
	ctype := req.Header.Get("Content-Type")
	if strings.HasPrefix(ctype, "multipart/form-data") {
		if err := req.ParseMultipartForm(maxUploadBytes); err != nil {
			httputil.WriteError(w, http.StatusBadRequest, "failed to parse multipart form: "+err.Error())
			return
		}
		title = strings.TrimSpace(req.FormValue("title"))
		topic = strings.TrimSpace(req.FormValue("topic"))
		parentID = strings.TrimSpace(req.FormValue("parent_id"))
		file, header, ferr := req.FormFile("file")
		if ferr != nil {
			httputil.WriteError(w, http.StatusBadRequest, "file field is required")
			return
		}
		defer file.Close()
		rawBytes := make([]byte, header.Size)
		if _, err := io.ReadFull(file, rawBytes); err != nil {
			httputil.WriteError(w, http.StatusBadRequest, "failed to read uploaded file: "+err.Error())
			return
		}
		ext := strings.ToLower(filepathExt(header.Filename))
		if ext != ".md" && ext != ".markdown" && ext != ".txt" {
			httputil.WriteError(w, http.StatusBadRequest, "unsupported file type: "+ext+" (use .md or .txt)")
			return
		}
		text = string(rawBytes)
	} else if strings.HasPrefix(ctype, "application/json") {
		var body createReportRequest
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			httputil.WriteError(w, http.StatusBadRequest, "invalid JSON body")
			return
		}
		title = strings.TrimSpace(body.Title)
		topic = strings.TrimSpace(body.Topic)
		text = body.Text
		parentID = body.ParentID
		childrenIDs = body.ChildrenIDs
	} else {
		httputil.WriteError(w, http.StatusUnsupportedMediaType, "Content-Type must be multipart/form-data or application/json")
		return
	}

	if title == "" {
		httputil.WriteError(w, http.StatusBadRequest, "title is required")
		return
	}
	if strings.TrimSpace(text) == "" {
		httputil.WriteError(w, http.StatusBadRequest, "text is required")
		return
	}

	r.createAndEnqueue(w, req, queries, repoID, title, topic, text, parentID, childrenIDs)
}

// createAndEnqueue is the shared tail of CreateReport + UploadReport:
// it inserts the report row in `pending` status, optionally validates
// and sets its parent / children, and enqueues the annotate_report
// job. Returns 202 with the report id + job id.
func (r *Reports) createAndEnqueue(w http.ResponseWriter, req *http.Request, queries *store.Queries, repoID pgtype.UUID, title, topic, text, parentIDStr string, childrenIDStrs []string) {
	if r.taskEnqueuer == nil {
		httputil.WriteError(w, http.StatusServiceUnavailable, "task manager not configured")
		return
	}

	id := pgtype.UUID{}
	if err := id.Scan(uuid.New().String()); err != nil {
		httputil.WriteError(w, http.StatusInternalServerError, "generating id: "+err.Error())
		return
	}

	var topicPtr *string
	if strings.TrimSpace(topic) != "" {
		t := strings.TrimSpace(topic)
		topicPtr = &t
	}

	var parentID pgtype.UUID
	if strings.TrimSpace(parentIDStr) != "" {
		if err := parentID.Scan(parentIDStr); err != nil {
			httputil.WriteError(w, http.StatusBadRequest, "invalid parent_id")
			return
		}
		if err := validateReportParent(req.Context(), queries, repoID, id, parentID); err != nil {
			httputil.WriteError(w, http.StatusBadRequest, err.Error())
			return
		}
	}

	created, err := queries.CreateReport(req.Context(), store.CreateReportParams{
		ID:           id,
		RepositoryID: repoID,
		Title:        title,
		Topic:        topicPtr,
		BodyMd:       text,
		Status:       "pending",
		ParentID:     parentID,
	})
	if err != nil {
		httputil.WriteError(w, http.StatusInternalServerError, "failed to create report")
		return
	}

	// Reparent existing reports under this new report (the meta-
	// synthesis case where the parent is created after its children).
	if len(childrenIDStrs) > 0 {
		if err := reparentChildren(req.Context(), queries, repoID, created.ID, childrenIDStrs); err != nil {
			httputil.WriteError(w, http.StatusBadRequest, err.Error())
			return
		}
	}

	repoIDStr := uuidFromPgtype(repoID)
	reportIDStr := uuidFromPgtype(created.ID)
	jobID, err := r.taskEnqueuer.EnqueueAnnotateReportFromHTTP(req.Context(), AnnotateReportArgs{
		ReportID:     reportIDStr,
		RepositoryID: repoIDStr,
	})
	if err != nil {
		httputil.WriteError(w, http.StatusInternalServerError, "failed to enqueue annotation: "+err.Error())
		return
	}

	httputil.WriteJSON(w, http.StatusAccepted, uploadReportResponse{
		ReportID: reportIDStr,
		JobID:    jobID,
		Status:   "queued",
	})
}

// GetReport handles GET /{repoID}/reports/{reportID}.
//
// Returns the report row + its annotations grouped by sentence_index.
// Cross-repo → 404 (not a leak).
func (r *Reports) GetReport(w http.ResponseWriter, req *http.Request) {
	pool := appmw.PoolFromContext(req.Context())
	if pool == nil {
		httputil.WriteError(w, http.StatusInternalServerError, "no per-repo pool on request context")
		return
	}
	queries := store.New(pool)

	repoID, err := repoIDFromURL(req)
	if err != nil {
		httputil.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}
	reportID, err := reportIDFromURL(req)
	if err != nil {
		httputil.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}

	report, err := queries.GetReportByID(req.Context(), reportID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			httputil.WriteError(w, http.StatusNotFound, "report not found")
			return
		}
		httputil.WriteError(w, http.StatusInternalServerError, "failed to get report")
		return
	}
	if report.RepositoryID != repoID {
		httputil.WriteError(w, http.StatusNotFound, "report not found")
		return
	}

	annotations, err := queries.ListReportAnnotationsByReport(req.Context(), reportID)
	if err != nil {
		httputil.WriteError(w, http.StatusInternalServerError, "failed to list report annotations")
		return
	}

	httputil.WriteJSON(w, http.StatusOK, map[string]interface{}{
		"report":      report,
		"annotations": annotations,
	})
}

// ListReports handles GET /{repoID}/reports.
//
// Paginated (pageEnvelope). Optional filters: q (search title/topic),
// status (pending/processing/annotated/failed).
func (r *Reports) ListReports(w http.ResponseWriter, req *http.Request) {
	pool := appmw.PoolFromContext(req.Context())
	if pool == nil {
		httputil.WriteError(w, http.StatusInternalServerError, "no per-repo pool on request context")
		return
	}
	queries := store.New(pool)

	repoID, err := repoIDFromURL(req)
	if err != nil {
		httputil.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}

	limit, offset := parsePaging(req)
	search := strings.TrimSpace(req.URL.Query().Get("q"))
	status := strings.TrimSpace(req.URL.Query().Get("status"))

	reports, err := queries.ListReportsByRepo(req.Context(), store.ListReportsByRepoParams{
		RepositoryID: repoID,
		Column2:      search,
		Column3:      status,
		Limit:        int32(limit),
		Offset:       int32(offset),
	})
	if err != nil {
		httputil.WriteError(w, http.StatusInternalServerError, "failed to list reports")
		return
	}

	total, err := queries.CountReportsByRepo(req.Context(), store.CountReportsByRepoParams{
		RepositoryID: repoID,
		Column2:      search,
		Column3:      status,
	})
	if err != nil {
		httputil.WriteError(w, http.StatusInternalServerError, "failed to count reports")
		return
	}

	httputil.WriteJSON(w, http.StatusOK, pageEnvelope{
		Data:   reports,
		Total:  total,
		Limit:  limit,
		Offset: offset,
	})
}

// UpdateReport handles PUT /{repoID}/reports/{reportID}.
//
// Updates title, topic, and body_md. When body_md differs from the
// stored value the handler re-enqueues an annotate_report job (auto-
// re-annotation) and resets status to `pending`. Optional parent_id
// reparents this report (with cycle detection); optional children_ids
// reparents those reports under this one. Reparenting alone does not
// re-enqueue annotation. Returns the updated report row.
func (r *Reports) UpdateReport(w http.ResponseWriter, req *http.Request) {
	pool := appmw.PoolFromContext(req.Context())
	if pool == nil {
		httputil.WriteError(w, http.StatusInternalServerError, "no per-repo pool on request context")
		return
	}
	queries := store.New(pool)

	repoID, err := repoIDFromURL(req)
	if err != nil {
		httputil.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}
	reportID, err := reportIDFromURL(req)
	if err != nil {
		httputil.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}

	var body updateReportRequest
	if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
		httputil.WriteError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if strings.TrimSpace(body.Title) == "" {
		httputil.WriteError(w, http.StatusBadRequest, "title is required")
		return
	}
	if strings.TrimSpace(body.Text) == "" {
		httputil.WriteError(w, http.StatusBadRequest, "text is required")
		return
	}

	existing, err := queries.GetReportByID(req.Context(), reportID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			httputil.WriteError(w, http.StatusNotFound, "report not found")
			return
		}
		httputil.WriteError(w, http.StatusInternalServerError, "failed to get report")
		return
	}
	if existing.RepositoryID != repoID {
		httputil.WriteError(w, http.StatusNotFound, "report not found")
		return
	}

	// Reparent this report if a parent_id was supplied. An explicit
	// empty string clears the parent (back to top-level). Cycle
	// detection: the new parent must not be this report or any of
	// its descendants.
	if body.ParentID != nil {
		newParentIDStr := strings.TrimSpace(*body.ParentID)
		if newParentIDStr == "" {
			if err := queries.SetReportsParent(req.Context(), store.SetReportsParentParams{
				ParentID:     pgtype.UUID{},
				Column2:      []pgtype.UUID{reportID},
				RepositoryID: repoID,
			}); err != nil {
				httputil.WriteError(w, http.StatusInternalServerError, "failed to clear parent")
				return
			}
		} else {
			var newParentID pgtype.UUID
			if err := newParentID.Scan(newParentIDStr); err != nil {
				httputil.WriteError(w, http.StatusBadRequest, "invalid parent_id")
				return
			}
			if err := validateReportParent(req.Context(), queries, repoID, reportID, newParentID); err != nil {
				httputil.WriteError(w, http.StatusBadRequest, err.Error())
				return
			}
			if err := queries.SetReportsParent(req.Context(), store.SetReportsParentParams{
				ParentID:     newParentID,
				Column2:      []pgtype.UUID{reportID},
				RepositoryID: repoID,
			}); err != nil {
				httputil.WriteError(w, http.StatusInternalServerError, "failed to reparent report")
				return
			}
		}
	}

	// Reparent existing reports under this one.
	if len(body.ChildrenIDs) > 0 {
		if err := reparentChildren(req.Context(), queries, repoID, reportID, body.ChildrenIDs); err != nil {
			httputil.WriteError(w, http.StatusBadRequest, err.Error())
			return
		}
	}

	var topicPtr *string
	if strings.TrimSpace(body.Topic) != "" {
		t := strings.TrimSpace(body.Topic)
		topicPtr = &t
	}

	updated, err := queries.UpdateReport(req.Context(), store.UpdateReportParams{
		ID:     reportID,
		Title:  strings.TrimSpace(body.Title),
		Topic:  topicPtr,
		BodyMd: body.Text,
	})
	if err != nil {
		httputil.WriteError(w, http.StatusInternalServerError, "failed to update report")
		return
	}

	// Auto-re-enqueue annotation when the body changed. The new job
	// will clear stale annotations and re-derive them from the new
	// text. We reset status to `pending` so the UI shows the report
	// is being re-annotated. Reparenting alone does not re-enqueue.
	bodyChanged := updated.BodyMd != existing.BodyMd
	if bodyChanged && r.taskEnqueuer != nil {
		repoIDStr := uuidFromPgtype(repoID)
		reportIDStr := uuidFromPgtype(updated.ID)
		jobID, err := r.taskEnqueuer.EnqueueAnnotateReportFromHTTP(req.Context(), AnnotateReportArgs{
			ReportID:     reportIDStr,
			RepositoryID: repoIDStr,
		})
		if err != nil {
			httputil.WriteError(w, http.StatusInternalServerError, "failed to enqueue re-annotation: "+err.Error())
			return
		}
		jobIDPtr := jobID
		_ = queries.MarkReportStatus(req.Context(), store.MarkReportStatusParams{
			ID:              reportID,
			Status:          "pending",
			AnnotationJobID: &jobIDPtr,
		})
	}

	httputil.WriteJSON(w, http.StatusOK, updated)
}

// DeleteReport handles DELETE /{repoID}/reports/{reportID}.
func (r *Reports) DeleteReport(w http.ResponseWriter, req *http.Request) {
	pool := appmw.PoolFromContext(req.Context())
	if pool == nil {
		httputil.WriteError(w, http.StatusInternalServerError, "no per-repo pool on request context")
		return
	}
	queries := store.New(pool)

	repoID, err := repoIDFromURL(req)
	if err != nil {
		httputil.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}
	reportID, err := reportIDFromURL(req)
	if err != nil {
		httputil.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}

	existing, err := queries.GetReportByID(req.Context(), reportID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			httputil.WriteError(w, http.StatusNotFound, "report not found")
			return
		}
		httputil.WriteError(w, http.StatusInternalServerError, "failed to get report")
		return
	}
	if existing.RepositoryID != repoID {
		httputil.WriteError(w, http.StatusNotFound, "report not found")
		return
	}

	if err := queries.DeleteReport(req.Context(), reportID); err != nil {
		httputil.WriteError(w, http.StatusInternalServerError, "failed to delete report")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// AnnotateReport handles POST /{repoID}/reports/{reportID}/annotate.
//
// Manually re-runs the annotation pass (e.g. after new facts were
// added to the repo). Returns 202 with the new job id. Resets status
// to `pending`.
func (r *Reports) AnnotateReport(w http.ResponseWriter, req *http.Request) {
	pool := appmw.PoolFromContext(req.Context())
	if pool == nil {
		httputil.WriteError(w, http.StatusInternalServerError, "no per-repo pool on request context")
		return
	}
	queries := store.New(pool)

	if r.taskEnqueuer == nil {
		httputil.WriteError(w, http.StatusServiceUnavailable, "task manager not configured")
		return
	}

	repoID, err := repoIDFromURL(req)
	if err != nil {
		httputil.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}
	reportID, err := reportIDFromURL(req)
	if err != nil {
		httputil.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}

	existing, err := queries.GetReportByID(req.Context(), reportID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			httputil.WriteError(w, http.StatusNotFound, "report not found")
			return
		}
		httputil.WriteError(w, http.StatusInternalServerError, "failed to get report")
		return
	}
	if existing.RepositoryID != repoID {
		httputil.WriteError(w, http.StatusNotFound, "report not found")
		return
	}

	repoIDStr := uuidFromPgtype(repoID)
	reportIDStr := uuidFromPgtype(reportID)
	jobID, err := r.taskEnqueuer.EnqueueAnnotateReportFromHTTP(req.Context(), AnnotateReportArgs{
		ReportID:     reportIDStr,
		RepositoryID: repoIDStr,
	})
	if err != nil {
		httputil.WriteError(w, http.StatusInternalServerError, "failed to enqueue annotation: "+err.Error())
		return
	}

	jobIDPtr := jobID
	_ = queries.MarkReportStatus(req.Context(), store.MarkReportStatusParams{
		ID:              reportID,
		Status:          "pending",
		AnnotationJobID: &jobIDPtr,
	})

	httputil.WriteJSON(w, http.StatusAccepted, uploadReportResponse{
		ReportID: reportIDStr,
		JobID:    jobID,
		Status:   "queued",
	})
}

// ListAnnotations handles GET /{repoID}/reports/{reportID}/annotations.
//
// Returns the flat annotation rows (sentence_index, sentence_text,
// fact_id, score, fact fields) so the UI can group by sentence and
// render the autocitation view.
func (r *Reports) ListAnnotations(w http.ResponseWriter, req *http.Request) {
	pool := appmw.PoolFromContext(req.Context())
	if pool == nil {
		httputil.WriteError(w, http.StatusInternalServerError, "no per-repo pool on request context")
		return
	}
	queries := store.New(pool)

	repoID, err := repoIDFromURL(req)
	if err != nil {
		httputil.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}
	reportID, err := reportIDFromURL(req)
	if err != nil {
		httputil.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}

	existing, err := queries.GetReportByID(req.Context(), reportID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			httputil.WriteError(w, http.StatusNotFound, "report not found")
			return
		}
		httputil.WriteError(w, http.StatusInternalServerError, "failed to get report")
		return
	}
	if existing.RepositoryID != repoID {
		httputil.WriteError(w, http.StatusNotFound, "report not found")
		return
	}

	annotations, err := queries.ListReportAnnotationsByReport(req.Context(), reportID)
	if err != nil {
		httputil.WriteError(w, http.StatusInternalServerError, "failed to list report annotations")
		return
	}
	httputil.WriteJSON(w, http.StatusOK, map[string]interface{}{
		"annotations": annotations,
		"count":       len(annotations),
	})
}

// reportIDFromURL extracts the {reportID} chi URL param and parses
// it into a pgtype.UUID. Mirrors investigationIDFromURL.
func reportIDFromURL(r *http.Request) (pgtype.UUID, error) {
	var id pgtype.UUID
	raw := chi.URLParam(r, "reportID")
	if raw == "" {
		return id, errors.New("reportID is required")
	}
	if err := id.Scan(raw); err != nil {
		return id, errors.New("invalid report id")
	}
	return id, nil
}

// validateReportParent checks that parentID refers to an existing
// report in the same repo and that making it the parent of childID
// would not create a cycle (parentID must not equal childID and must
// not be a descendant of childID). For a brand-new report (childID
// zero-value) only the existence + same-repo check runs.
func validateReportParent(ctx context.Context, queries *store.Queries, repoID, childID, parentID pgtype.UUID) error {
	if uuidFromPgtype(parentID) == uuidFromPgtype(childID) && childID.Valid {
		return errors.New("a report cannot be its own parent")
	}
	parent, err := queries.GetReportByID(ctx, parentID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return errors.New("parent report not found")
		}
		return fmt.Errorf("looking up parent report: %w", err)
	}
	if parent.RepositoryID != repoID {
		return errors.New("parent report not found")
	}
	// Cycle check: walk ancestors of the new parent; childID must
	// not appear (if it did, the new parent is a descendant of the
	// report being reparented → cycle). For a brand-new report
	// (childID zero-value) this is skipped.
	if !childID.Valid {
		return nil
	}
	ancestors, err := queries.GetReportAncestors(ctx, parentID)
	if err != nil {
		return fmt.Errorf("checking report ancestry: %w", err)
	}
	childIDStr := uuidFromPgtype(childID)
	for _, a := range ancestors {
		if uuidFromPgtype(a.ID) == childIDStr {
			return errors.New("cannot reparent: target is a descendant of this report (cycle)")
		}
	}
	return nil
}

// reparentChildren sets parentID on every id in childIDStrs after
// validating each belongs to the same repo and is not the parent
// itself. Does not cycle-check individual children against their own
// subtrees (a child that is an ancestor of the new parent would
// create a cycle); callers setting children_ids on an existing report
// should guard against that, but the common meta-synthesis flow
// creates a fresh parent so children cannot be its ancestors.
func reparentChildren(ctx context.Context, queries *store.Queries, repoID, parentID pgtype.UUID, childIDStrs []string) error {
	parentIDStr := uuidFromPgtype(parentID)
	childIDs := make([]pgtype.UUID, 0, len(childIDStrs))
	for _, raw := range childIDStrs {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		var cid pgtype.UUID
		if err := cid.Scan(raw); err != nil {
			return errors.New("invalid children_ids entry: " + raw)
		}
		if uuidFromPgtype(cid) == parentIDStr {
			return errors.New("a report cannot be its own child")
		}
		// Verify the child exists and belongs to the same repo.
		child, err := queries.GetReportByID(ctx, cid)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return errors.New("child report not found: " + raw)
			}
			return fmt.Errorf("looking up child report: %w", err)
		}
		if child.RepositoryID != repoID {
			return errors.New("child report not found: " + raw)
		}
		childIDs = append(childIDs, cid)
	}
	if len(childIDs) == 0 {
		return nil
	}
	if err := queries.SetReportsParent(ctx, store.SetReportsParentParams{
		ParentID:     parentID,
		Column2:      childIDs,
		RepositoryID: repoID,
	}); err != nil {
		return fmt.Errorf("reparenting children: %w", err)
	}
	return nil
}

// filepathExt is a thin wrapper over path/filepath.Ext so the
// reports handler (which doesn't otherwise need filepath) stays
// focused. Kept local so the import is path/filepath only for
// this helper.
func filepathExt(name string) string {
	// avoid importing path/filepath at the package level just for
	// this one call; the logic is the same.
	for i := len(name) - 1; i >= 0 && name[i] != '/'; i-- {
		if name[i] == '.' {
			return name[i:]
		}
	}
	return ""
}

// hashText is a tiny helper kept for symmetry with Source.UploadSource
// (which derives a synthetic URL from a hash of the raw text). Reports
// don't have a UNIQUE URL constraint, so this is unused today but kept
// as a placeholder for a future dedup-by-hash feature.
func hashText(text string) string {
	h := sha256.Sum256([]byte(text))
	return hex.EncodeToString(h[:8])
}

var _ = hashText // referenced for the placeholder above