package middleware

import (
	"context"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/openktree/open-knowledge-tree/backend/internal/api/httputil"
	"github.com/openktree/open-knowledge-tree/backend/internal/rbac"
	"github.com/openktree/open-knowledge-tree/backend/internal/store"
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

// keyAllows checks whether an API key on the request context authorizes
// the (resource, action) for the given repository domain. It returns:
//
//   - (true, nil) when no API key is on the context (session-authenticated
//     request — skip the per-key scope check, RBAC alone governs);
//   - (true, nil) when the key carries a matching "object:action" entry
//     (or a "object:*" / "*:action" / "*:*" wildcard) AND the key's
//     repository_id is either NULL (all-repos key) or matches the
//     effective repository domain;
//   - (false, nil) when the key does not cover the requested scope
//     (the middleware returns 403);
//   - (false, err) on an unexpected failure (the middleware returns 500).
//
// The domain argument is the effective repository domain for this
// request: the repo UUID for repo-scope routes, or "*" for system-scope
// routes. A NULL key.repository_id means "all repos the user can access"
// (RBAC still gates), so it passes the repo-match check unconditionally.
func keyAllows(ctx context.Context, resource, action, domain string) (bool, error) {
	key := httputil.RequestAPIKey(ctx)
	if key == nil {
		return true, nil
	}
	return keyCovers(key, resource, action, domain), nil
}

// keyCovers is the pure logic of keyAllows, extracted so tests can
// exercise it without constructing a context. See keyAllows for the
// matching rules.
func keyCovers(key *store.OktSystemApiKey, resource, action, domain string) bool {
	// Repository restriction. NULL = all repos; non-NULL = single
	// repo and the request's domain must match exactly.
	if key.RepositoryID.Valid {
		if !strings.EqualFold(key.RepositoryID.String(), domain) {
			return false
		}
	}

	// Permission scope. Each entry is "object:action"; "*" is a
	// wildcard on either side. Empty list = deny everything (a
	// key created with no permissions can do nothing).
	for _, entry := range key.Permissions {
		obj, act := splitScopeEntry(entry)
		if obj == "" {
			continue
		}
		objOK := obj == "*" || obj == resource
		actOK := act == "*" || act == action
		if objOK && actOK {
			return true
		}
	}
	return false
}

// splitScopeEntry splits "object:action" into its parts. Returns ("","")
// when the entry is malformed (the caller skips it).
func splitScopeEntry(entry string) (string, string) {
	idx := strings.Index(entry, ":")
	if idx <= 0 || idx == len(entry)-1 {
		return "", ""
	}
	return entry[:idx], entry[idx+1:]
}

// RequirePermission returns a middleware that enforces a (resource, action)
// permission on the request's user via the RBAC service. System admins
// always pass. The user ID is expected to already be on the context
// (typically set by AuthRequired). The domain (RBAC scope) is read from
// the X-Repository-ID header, defaulting to "*" (system scope) when
// absent. Used for system-scope routes (no /{repoID} in the URL).
//
// When the request authenticated via an API key (httputil.RequestAPIKey
// returns non-nil), the key's (object, action) scope and repository_id
// restriction are enforced in addition to RBAC. Session-authenticated
// requests skip the per-key check.
func RequirePermission(rbacSvc *rbac.Service, resource, action string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		uid := httputil.RequestUserID(r.Context())
		repoID := repositoryIDFromHeader(r)

		isSysAdmin, _ := rbacSvc.EnforceSystemAdmin(uid.String())
		if isSysAdmin {
			// Even sysadmins are bound by their API key's scope when
			// they use one — a key is a narrower credential than the
			// user's full RBAC. The whole point of a scoped key is
			// that it can't do what the user can do.
			if ok, _ := keyAllows(r.Context(), resource, action, repoID); !ok {
				httputil.WriteError(w, http.StatusForbidden, "api key lacks this scope")
				return
			}
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

		if ok, _ := keyAllows(r.Context(), resource, action, repoID); !ok {
			httputil.WriteError(w, http.StatusForbidden, "api key lacks this scope")
			return
		}

		next.ServeHTTP(w, r)
	}
}

// RequireRepoPermission returns a middleware that enforces a (resource, action)
// permission using the repository ID from the request context (set by
// WithRepoQueries). System admins always pass. Used for repo-scope routes
// (under /{repoID}) where the domain must come from the URL, not the header.
//
// API-key scope enforcement mirrors RequirePermission: the key's
// repository_id must be NULL (all repos) or match the route's repo, and
// the key's permissions must cover (resource, action).
func RequireRepoPermission(rbacSvc *rbac.Service, resource, action string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		uid := httputil.RequestUserID(r.Context())

		isSysAdmin, _ := rbacSvc.EnforceSystemAdmin(uid.String())
		if isSysAdmin {
			// Fall through to the key-scope check below — a sysadmin
			// using a scoped key is still bound by the key.
			repoID, ok := RepoIDFromContext(r.Context())
			if !ok {
				httputil.WriteError(w, http.StatusBadRequest, "could not resolve repository ID")
				return
			}
			if ok, _ := keyAllows(r.Context(), resource, action, repoID.String()); !ok {
				httputil.WriteError(w, http.StatusForbidden, "api key lacks this scope")
				return
			}
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

		if ok, _ := keyAllows(r.Context(), resource, action, repoID.String()); !ok {
			httputil.WriteError(w, http.StatusForbidden, "api key lacks this scope")
			return
		}

		next.ServeHTTP(w, r)
	}
}

// APIKeyRepoMatch is a convenience used by handlers that need to verify
// an API key's repository restriction outside the RequireRepoPermission
// middleware (e.g. for repo-scope list endpoints that don't carry a
// specific resource/action). Returns true when no key is on the context
// or the key's repository_id is NULL/matches repoID.
func APIKeyRepoMatch(ctx context.Context, repoID pgtype.UUID) bool {
	key := httputil.RequestAPIKey(ctx)
	if key == nil {
		return true
	}
	if !key.RepositoryID.Valid {
		return true
	}
	return strings.EqualFold(key.RepositoryID.String(), repoID.String())
}