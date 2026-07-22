package handler

import (
	"encoding/json"
	"net/http"

	"github.com/openktree/open-knowledge-tree/backend/internal/api/httputil"
	"github.com/openktree/open-knowledge-tree/backend/internal/rbac"
)

type assignRoleRequest struct {
	UserID       string `json:"user_id"`
	Role         string `json:"role"`
	RepositoryID string `json:"repository_id"`
}

type userWithRoles struct {
	ID          string          `json:"id"`
	Email       string          `json:"email"`
	DisplayName string          `json:"display_name"`
	Roles       []rbac.UserRole `json:"roles"`
	SystemAdmin bool            `json:"system_admin"`
}

type removeRoleRequest struct {
	UserID       string `json:"user_id"`
	Role         string `json:"role"`
	RepositoryID string `json:"repository_id"`
}

type userPermissionsResponse struct {
	Permissions []rbac.Permission `json:"permissions"`
}

type usersListResponse struct {
	Users       []userWithRoles   `json:"users"`
	Permissions []rbac.Permission `json:"available_permissions"`
}

// Admin bundles the admin-only HTTP handlers.
type Admin struct {
	deps Deps
	// repoPoolResolver resolves a repository UUID-or-slug string to
	// its per-repo *pgxpool.Pool + parsed UUID. Set via
	// SetRepoPoolResolver from the wiring layer; nil disables the
	// concept-reextract and source-reprocess endpoints (503).
	repoPoolResolver RepoPoolResolver
	// taskEnqueuer inserts background jobs (extract_concepts,
	// source_decomposition). Set via SetTaskEnqueuer from the
	// wiring layer; nil disables the reextract/reprocess endpoints.
	taskEnqueuer TaskEnqueuer
}

// NewAdmin constructs an Admin handler bundle.
func NewAdmin(d Deps) *Admin {
	return &Admin{deps: d}
}

// ListUsers handles GET /admin/users.
func (a *Admin) ListUsers(w http.ResponseWriter, r *http.Request) {
	users, err := a.deps.Users.ListUsers(r.Context())
	if err != nil {
		httputil.WriteError(w, http.StatusInternalServerError, "failed to list users")
		return
	}

	result := make([]userWithRoles, 0, len(users))
	for _, u := range users {
		uid := string(u.ID)
		roles, _ := a.deps.RBAC.GetRolesForUser(uid, "*")
		isSysAdmin, _ := a.deps.RBAC.EnforceSystemAdmin(uid)

		var userRoles []rbac.UserRole
		for _, role := range roles {
			userRoles = append(userRoles, rbac.UserRole{
				Role:         role,
				RepositoryID: "*",
			})
		}

		if isSysAdmin {
			userRoles = append(userRoles, rbac.UserRole{
				Role:         rbac.RoleSysAdmin,
				RepositoryID: rbac.DomainSystem,
			})
		}

		result = append(result, userWithRoles{
			ID:          uid,
			Email:       u.Email,
			DisplayName: u.DisplayName,
			Roles:       userRoles,
			SystemAdmin: isSysAdmin,
		})
	}

	perms, _ := a.deps.RBAC.GetAllPermissions()

	var permList []rbac.Permission
	seen := make(map[string]bool)
	for _, p := range perms {
		if len(p) >= 4 {
			key := p[2] + ":" + p[3]
			if !seen[key] {
				seen[key] = true
				permList = append(permList, rbac.Permission{
					Resource: p[2],
					Action:   p[3],
				})
			}
		}
	}

	httputil.WriteJSON(w, http.StatusOK, usersListResponse{
		Users:       result,
		Permissions: permList,
	})
}

// AssignRole handles PUT /admin/users/roles.
func (a *Admin) AssignRole(w http.ResponseWriter, r *http.Request) {
	var req assignRoleRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httputil.WriteError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	if req.UserID == "" || req.Role == "" || req.RepositoryID == "" {
		httputil.WriteError(w, http.StatusBadRequest, "user_id, role, and repository_id are required")
		return
	}

	if !rbac.IsValidRole(req.Role) {
		httputil.WriteError(w, http.StatusBadRequest, "invalid role: must be one of sysadmin, repoadmin, editor, viewer, curator, user, admin, viewer (legacy)")
		return
	}

	if req.Role == rbac.RoleSysAdmin {
		if err := a.deps.Users.AssignRole(r.Context(), req.UserID, rbac.RoleSysAdmin, rbac.DomainSystem); err != nil {
			httputil.WriteError(w, http.StatusInternalServerError, "failed to assign sysadmin role")
			return
		}
		recordAudit(a.deps, r, rbac.AuditActionRoleAssign, rbac.Objects.Users, req.UserID, map[string]any{
			"role":          req.Role,
			"repository_id": req.RepositoryID,
		})
		httputil.WriteJSON(w, http.StatusOK, map[string]string{"message": "role assigned"})
		return
	}

	if err := a.deps.Users.AssignRole(r.Context(), req.UserID, req.Role, req.RepositoryID); err != nil {
		httputil.WriteError(w, http.StatusInternalServerError, "failed to assign role")
		return
	}

	recordAudit(a.deps, r, rbac.AuditActionRoleAssign, rbac.Objects.Users, req.UserID, map[string]any{
		"role":          req.Role,
		"repository_id": req.RepositoryID,
	})
	httputil.WriteJSON(w, http.StatusOK, map[string]string{"message": "role assigned"})
}

// RemoveRole handles DELETE /admin/users/roles.
func (a *Admin) RemoveRole(w http.ResponseWriter, r *http.Request) {
	var req removeRoleRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httputil.WriteError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	if req.UserID == "" || req.Role == "" || req.RepositoryID == "" {
		httputil.WriteError(w, http.StatusBadRequest, "user_id, role, and repository_id are required")
		return
	}

	if err := a.deps.Users.RemoveRole(r.Context(), req.UserID, req.Role, req.RepositoryID); err != nil {
		httputil.WriteError(w, http.StatusInternalServerError, "failed to remove role")
		return
	}

	recordAudit(a.deps, r, rbac.AuditActionRoleRemove, rbac.Objects.Users, req.UserID, map[string]any{
		"role":          req.Role,
		"repository_id": req.RepositoryID,
	})
	httputil.WriteJSON(w, http.StatusOK, map[string]string{"message": "role removed"})
}

// ListPermissions handles GET /admin/permissions.
func (a *Admin) ListPermissions(w http.ResponseWriter, r *http.Request) {
	perms, err := a.deps.RBAC.GetAllPermissions()
	if err != nil {
		httputil.WriteError(w, http.StatusInternalServerError, "failed to list permissions")
		return
	}

	var permList []rbac.Permission
	seen := make(map[string]bool)
	for _, p := range perms {
		if len(p) >= 4 {
			key := p[2] + ":" + p[3]
			if !seen[key] {
				seen[key] = true
				permList = append(permList, rbac.Permission{
					Resource: p[2],
					Action:   p[3],
				})
			}
		}
	}

	httputil.WriteJSON(w, http.StatusOK, userPermissionsResponse{Permissions: permList})
}
