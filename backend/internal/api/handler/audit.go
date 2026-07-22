package handler

import (
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/openktree/open-knowledge-tree/backend/internal/api/httputil"
	appmw "github.com/openktree/open-knowledge-tree/backend/internal/api/middleware"
	"github.com/openktree/open-knowledge-tree/backend/internal/store"
)

// Audit is the HTTP handler bundle for the audit-log read surface.
// It exposes two endpoints:
//
//   - GET /api/v1/admin/audit      (system scope; sysadmin only)
//   - GET /api/v1/repositories/{repoID}/audit (repo scope; repoadmin)
//
// Both accept the same query filters (from / to / action / object /
// actor_user_id / limit / offset) and return the same JSON shape:
//
//	{ "events": [...], "total": N, "actions": ["grant", "revoke", ...] }
//
// The system endpoint passes a NULL repository_id narg so the query
// returns every row; the repo endpoint passes the repo UUID from
// the URL context (set by WithRepoQueries) so the query is scoped
// to that repository. RBAC is enforced at the route (h.perm /
// h.repoPerm with ("audit","read")).
type Audit struct {
	deps Deps
}

func NewAudit(d Deps) *Audit { return &Audit{deps: d} }

// auditFilters parses the shared filter query params. from/to are
// RFC3339 timestamps; action/object/actor_user_id are optional
// exact-match filters. limit defaults to 100, max 200; offset
// defaults to 0. A malformed value is a 400.
type auditFilters struct {
	from         pgtype.Timestamptz
	to           pgtype.Timestamptz
	action       *string
	object       *string
	actorUserID  pgtype.UUID
	limitRows    int32
	pageOffset   int32
}

func parseAuditFilters(r *http.Request) (auditFilters, string, bool) {
	var f auditFilters
	if raw := r.URL.Query().Get("from"); raw != "" {
		t, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			return f, "invalid 'from' timestamp (use RFC3339)", false
		}
		f.from = pgtype.Timestamptz{Time: t, Valid: true}
	}
	if raw := r.URL.Query().Get("to"); raw != "" {
		t, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			return f, "invalid 'to' timestamp (use RFC3339)", false
		}
		f.to = pgtype.Timestamptz{Time: t, Valid: true}
	}
	if raw := r.URL.Query().Get("action"); raw != "" {
		f.action = &raw
	}
	if raw := r.URL.Query().Get("object"); raw != "" {
		f.object = &raw
	}
	if raw := r.URL.Query().Get("actor_user_id"); raw != "" {
		var u pgtype.UUID
		if err := u.Scan(raw); err != nil {
			return f, "invalid 'actor_user_id' (must be a UUID)", false
		}
		f.actorUserID = u
	}
	f.limitRows = 100
	if raw := r.URL.Query().Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 0 {
			return f, "invalid 'limit'", false
		}
		if n > 200 {
			n = 200
		}
		f.limitRows = int32(n)
	}
	if raw := r.URL.Query().Get("offset"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 0 {
			return f, "invalid 'offset'", false
		}
		f.pageOffset = int32(n)
	}
	return f, "", true
}

type auditEvent struct {
	ID           int64   `json:"id"`
	OccurredAt   string  `json:"occurred_at"`
	ActorUserID  *string `json:"actor_user_id"`
	ActorUsername string `json:"actor_username"`
	ActorEmail   *string `json:"actor_email"`
	Action       string  `json:"action"`
	Object       string  `json:"object"`
	RepositoryID *string `json:"repository_id"`
	Target       *string `json:"target"`
	Detail       any     `json:"detail"`
	SourceURL    *string `json:"source_url"`
}

// ListSystem handles GET /api/v1/admin/audit. Returns every audit
// event (no repository_id filter). Gated at the route by
// h.perm("audit","read") so only sysadmin reaches the handler.
func (a *Audit) ListSystem(w http.ResponseWriter, r *http.Request) {
	a.list(w, r, pgtype.UUID{})
}

// ListRepo handles GET /api/v1/repositories/{repoID}/audit. The
// repository UUID is read from the request context (set by
// WithRepoQueries); the query is scoped to that repository. Gated
// at the route by h.repoPerm("audit","read") so repoadmin (and
// sysadmin) reach the handler, but only for repos they administer.
func (a *Audit) ListRepo(w http.ResponseWriter, r *http.Request) {
	repoID, ok := appmw.RepoIDFromContext(r.Context())
	if !ok || !repoID.Valid {
		httputil.WriteError(w, http.StatusBadRequest, "repository id missing from context")
		return
	}
	a.list(w, r, repoID)
}

// list is the shared query path. repoIDFilter is the narg value for
// repository_id: a zero pgtype.UUID (Valid=false) means "no filter"
// (system view); a valid UUID scopes to that repo.
func (a *Audit) list(w http.ResponseWriter, r *http.Request, repoIDFilter pgtype.UUID) {
	f, msg, ok := parseAuditFilters(r)
	if !ok {
		httputil.WriteError(w, http.StatusBadRequest, msg)
		return
	}
	params := store.ListAuditEventsParams{
		RepositoryID: repoIDFilter,
		Action:       f.action,
		ActorUserID:  f.actorUserID,
		Object:       f.object,
		From:         f.from,
		To:           f.to,
		LimitRows:    f.limitRows,
		PageOffset:   f.pageOffset,
	}
	rows, err := a.deps.Store.ListAuditEvents(r.Context(), params)
	if err != nil {
		httputil.WriteError(w, http.StatusInternalServerError, err.Error())
		return
	}
	total, err := a.deps.Store.CountAuditEvents(r.Context(), store.CountAuditEventsParams{
		RepositoryID: repoIDFilter,
		Action:       f.action,
		ActorUserID:  f.actorUserID,
		Object:       f.object,
		From:         f.from,
		To:           f.to,
	})
	if err != nil {
		httputil.WriteError(w, http.StatusInternalServerError, err.Error())
		return
	}
	actions, err := a.deps.Store.ListAuditActions(r.Context())
	if err != nil {
		httputil.WriteError(w, http.StatusInternalServerError, err.Error())
		return
	}

	events := make([]auditEvent, 0, len(rows))
	for _, row := range rows {
		events = append(events, rowToEvent(row))
	}
	httputil.WriteJSON(w, http.StatusOK, map[string]any{
		"events":  events,
		"total":   total,
		"actions": actions,
	})
}

// rowToEvent maps a sqlc row to the JSON shape. Timestamps are
// rendered as RFC3339 strings; NULLable UUID/string columns are
// rendered as null when invalid/empty. Detail is decoded from JSONB
// into a generic map so the client sees a JSON object, not a base64
// blob (pgx returns JSONB as []byte).
func rowToEvent(row store.ListAuditEventsRow) auditEvent {
	var occurred string
	if row.OccurredAt.Valid {
		occurred = row.OccurredAt.Time.Format(time.RFC3339)
	}
	var actorID *string
	if row.ActorUserID.Valid {
		s := row.ActorUserID.String()
		actorID = &s
	}
	var repoID *string
	if row.RepositoryID.Valid {
		s := row.RepositoryID.String()
		repoID = &s
	}
	var detail any
	if len(row.Detail) > 0 {
		var decoded any
		if err := json.Unmarshal(row.Detail, &decoded); err == nil {
			detail = decoded
		} else {
			detail = row.Detail
		}
	}
	return auditEvent{
		ID:            row.ID,
		OccurredAt:    occurred,
		ActorUserID:   actorID,
		ActorUsername: row.ActorUsername,
		ActorEmail:    row.ActorEmail,
		Action:        row.Action,
		Object:        row.Object,
		RepositoryID:  repoID,
		Target:        row.Target,
		Detail:        detail,
		SourceURL:     row.SourceUrl,
	}
}