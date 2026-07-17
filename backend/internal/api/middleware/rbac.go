package middleware

import (
	"net/http"
	"strings"

	"github.com/openktree/open-knowledge-tree/backend/internal/api/httputil"
	"github.com/openktree/open-knowledge-tree/backend/internal/rbac"
)

// repositoryIDFromHeader returns the repository ID for permission checks.
// It reads the X-Repository-ID header and falls back to "*" (system scope)
// when the header is missing or blank.
func repositoryIDFromHeader(r *http.Request) string {
	repoID := strings.TrimSpace(r.Header.Get("X-Repository-ID"))
	if repoID == "" {
		return "*"
	}
	return repoID
}

// RequirePermission returns a middleware that enforces a (resource, action)
// permission on the request's user via the RBAC service. System admins
// always pass. The user ID is expected to already be on the context
// (typically set by AuthRequired). The domain (RBAC scope) is read from
// the X-Repository-ID header, defaulting to "*" (system scope) when
// absent. Used for system-scope routes (no /{repoID} in the URL).
func RequirePermission(rbacSvc *rbac.Service, resource, action string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		uid := httputil.RequestUserID(r.Context())
		repoID := repositoryIDFromHeader(r)

		isSysAdmin, _ := rbacSvc.EnforceSystemAdmin(uid.String())
		if isSysAdmin {
			next.ServeHTTP(w, r)
			return
		}

		ok, err := rbacSvc.Enforce(uid.String(), repoID, resource, action)
		if err != nil {
			httputil.WriteError(w, http.StatusInternalServerError, "permission check failed")
			return
		}

		if !ok {
			httputil.WriteError(w, http.StatusForbidden, "insufficient permissions")
			return
		}

		next.ServeHTTP(w, r)
	}
}

// RequireRepoPermission returns a middleware that enforces a (resource, action)
// permission using the repository ID from the request context (set by
// WithRepoQueries). System admins always pass. Used for repo-scope routes
// (under /{repoID}) where the domain must come from the URL, not the header.
func RequireRepoPermission(rbacSvc *rbac.Service, resource, action string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		uid := httputil.RequestUserID(r.Context())

		isSysAdmin, _ := rbacSvc.EnforceSystemAdmin(uid.String())
		if isSysAdmin {
			next.ServeHTTP(w, r)
			return
		}

		repoID, ok := RepoIDFromContext(r.Context())
		if !ok {
			httputil.WriteError(w, http.StatusBadRequest, "could not resolve repository ID")
			return
		}

		enforceOK, err := rbacSvc.Enforce(uid.String(), repoID.String(), resource, action)
		if err != nil {
			httputil.WriteError(w, http.StatusInternalServerError, "permission check failed")
			return
		}

		if !enforceOK {
			httputil.WriteError(w, http.StatusForbidden, "insufficient permissions")
			return
		}

		next.ServeHTTP(w, r)
	}
}
