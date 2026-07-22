package handler

import (
	"net/http"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/openktree/open-knowledge-tree/backend/internal/api/httputil"
	appmw "github.com/openktree/open-knowledge-tree/backend/internal/api/middleware"
	"github.com/openktree/open-knowledge-tree/backend/internal/audit"
	"github.com/openktree/open-knowledge-tree/backend/internal/rbac"
)

// recordAudit is the shared helper every handler call site uses to
// emit an audit event. It resolves the actor (user id + email) from
// the request context, the optional repository scope from the URL
// (set by WithRepoQueries), and dispatches RecordAsync on the
// recorder. It is a no-op when deps.Audit is nil (tests that don't
// wire the recorder) so call sites can call it unconditionally.
//
// The email lookup is best-effort: a fetch failure is swallowed and
// the email is left empty, so an audit write never fails the request
// even if the user store is briefly unreachable. The actor_user_id
// is always set (it comes from the authed context).
func recordAudit(deps Deps, r *http.Request, action, object string, target string, detail map[string]any) {
	if deps.Audit == nil {
		return
	}
	uid := httputil.RequestUserID(r.Context())
	username := ""
	if deps.Users != nil && uid.Valid {
		if u, err := deps.Users.GetUser(r.Context(), rbac.UserID(uid.String())); err == nil {
			username = u.Email
		}
	}
	var repoID pgtype.UUID
	if id, ok := appmw.RepoIDFromContext(r.Context()); ok && id.Valid {
		repoID = id
	}
	deps.Audit.RecordAsync(audit.Event{
		UserID:       uid,
		Username:     username,
		Action:       action,
		Object:       object,
		RepositoryID: repoID,
		Target:       target,
		Detail:       detail,
	})
}

// recordAuditWithRepo is the variant for call sites that already
// hold a resolved repository UUID (e.g. CreateRepository, which
// knows the new repo's id after the insert). It lets the caller
// pin the audit row to that repo even when the request didn't go
// through WithRepoQueries (system-scope POST /repositories).
func recordAuditWithRepo(deps Deps, r *http.Request, action, object string, repoID pgtype.UUID, target string, detail map[string]any) {
	if deps.Audit == nil {
		return
	}
	uid := httputil.RequestUserID(r.Context())
	username := ""
	if deps.Users != nil && uid.Valid {
		if u, err := deps.Users.GetUser(r.Context(), rbac.UserID(uid.String())); err == nil {
			username = u.Email
		}
	}
	deps.Audit.RecordAsync(audit.Event{
		UserID:       uid,
		Username:     username,
		Action:       action,
		Object:       object,
		RepositoryID: repoID,
		Target:       target,
		Detail:       detail,
	})
}