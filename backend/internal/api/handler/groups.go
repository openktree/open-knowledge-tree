// Package handler contains the HTTP handler implementations for the API,
// grouped by domain (auth, user, admin, repository, source, group).
// Handlers are exposed as plain functions and structs that receive only
// the dependencies they need, so they are easy to compose and test.
package handler

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/openktree/open-knowledge-tree/backend/internal/api/httputil"
	"github.com/openktree/open-knowledge-tree/backend/internal/rbac"
)

// Groups bundles the HTTP handlers for the groups domain.
//
// Authorization model (per the phase-1.5 plan):
//
//   - All endpoints require an authenticated session.
//   - Mutations (create, update, delete, member add/remove,
//     role grant/revoke) require `groups:manage`, which
//     is satisfied by `sysadmin` (the only role that has
//     it in the seed).
//   - Reads (list, get, members, roles, "my groups") are
//     open to any authenticated user. The repoadmin /
//     editor / viewer / curator roles do NOT have
//     `groups:read` in the seed, so a regular user can
//     still see their own membership via
//     `GET /users/{id}/groups` only when the {id} is
//     themselves (the handler enforces the self-only
//     rule explicitly).
type Groups struct {
	deps Deps
}

// NewGroups constructs a Groups handler bundle.
func NewGroups(d Deps) *Groups {
	return &Groups{deps: d}
}

// listGroupsResponse is the JSON envelope for
// `GET /api/v1/groups`.
type listGroupsResponse struct {
	Groups []groupJSON `json:"groups"`
}

type groupJSON struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	CreatedAt   string `json:"created_at"`
	UpdatedAt   string `json:"updated_at"`
}

func toGroupJSON(g rbac.Group) groupJSON {
	return groupJSON{
		ID:          string(g.ID),
		Name:        g.Name,
		Description: g.Description,
	}
}

// ListGroups handles GET /api/v1/groups.
func (g *Groups) ListGroups(w http.ResponseWriter, r *http.Request) {
	groups, err := g.deps.Groups.ListGroups(r.Context())
	if err != nil {
		httputil.WriteError(w, http.StatusInternalServerError, "failed to list groups")
		return
	}
	out := make([]groupJSON, 0, len(groups))
	for _, gr := range groups {
		out = append(out, toGroupJSON(gr))
	}
	httputil.WriteJSON(w, http.StatusOK, listGroupsResponse{Groups: out})
}

// createGroupRequest is the body shape for
// `POST /api/v1/groups`.
type createGroupRequest struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

// CreateGroup handles POST /api/v1/groups.
func (g *Groups) CreateGroup(w http.ResponseWriter, r *http.Request) {
	var req createGroupRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httputil.WriteError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	gr, err := g.deps.Groups.CreateGroup(r.Context(), req.Name, req.Description)
	if err != nil {
		if errors.Is(err, rbac.ErrGroupNotFound) {
			httputil.WriteError(w, http.StatusNotFound, "group not found")
			return
		}
		// Unique violation surfaces as 409.
		if isUniqueViolation(err) {
			httputil.WriteError(w, http.StatusConflict, "group name already exists")
			return
		}
		httputil.WriteError(w, http.StatusInternalServerError, "failed to create group")
		return
	}
	httputil.WriteJSON(w, http.StatusCreated, toGroupJSON(gr))
}

// GetGroup handles GET /api/v1/groups/{groupID}.
func (g *Groups) GetGroup(w http.ResponseWriter, r *http.Request) {
	id := rbac.GroupID(chi.URLParam(r, "groupID"))
	gr, err := g.deps.Groups.GetGroup(r.Context(), id)
	if err != nil {
		if errors.Is(err, rbac.ErrGroupNotFound) {
			httputil.WriteError(w, http.StatusNotFound, "group not found")
			return
		}
		httputil.WriteError(w, http.StatusInternalServerError, "failed to get group")
		return
	}
	httputil.WriteJSON(w, http.StatusOK, toGroupJSON(gr))
}

// updateGroupRequest is the body shape for
// `PATCH /api/v1/groups/{groupID}`.
type updateGroupRequest struct {
	Name        *string `json:"name"`
	Description *string `json:"description"`
}

// UpdateGroup handles PATCH /api/v1/groups/{groupID}.
// We do a read-modify-write for partial updates because
// the sqlc-generated UpdateGroup is a full update; the
// alternative is a sqlc partial update, but for two
// fields the read-modify-write is clearer and the
// concurrency risk is negligible (sysadmin only).
func (g *Groups) UpdateGroup(w http.ResponseWriter, r *http.Request) {
	id := rbac.GroupID(chi.URLParam(r, "groupID"))
	var req updateGroupRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httputil.WriteError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	current, err := g.deps.Groups.GetGroup(r.Context(), id)
	if err != nil {
		if errors.Is(err, rbac.ErrGroupNotFound) {
			httputil.WriteError(w, http.StatusNotFound, "group not found")
			return
		}
		httputil.WriteError(w, http.StatusInternalServerError, "failed to load group")
		return
	}
	name := current.Name
	if req.Name != nil {
		name = *req.Name
	}
	description := current.Description
	if req.Description != nil {
		description = *req.Description
	}
	updated, err := g.deps.Groups.UpdateGroup(r.Context(), id, name, description)
	if err != nil {
		if errors.Is(err, rbac.ErrGroupNotFound) {
			httputil.WriteError(w, http.StatusNotFound, "group not found")
			return
		}
		if isUniqueViolation(err) {
			httputil.WriteError(w, http.StatusConflict, "group name already exists")
			return
		}
		httputil.WriteError(w, http.StatusInternalServerError, "failed to update group")
		return
	}
	httputil.WriteJSON(w, http.StatusOK, toGroupJSON(updated))
}

// DeleteGroup handles DELETE /api/v1/groups/{groupID}.
func (g *Groups) DeleteGroup(w http.ResponseWriter, r *http.Request) {
	id := rbac.GroupID(chi.URLParam(r, "groupID"))
	if err := g.deps.Groups.DeleteGroup(r.Context(), id); err != nil {
		if errors.Is(err, rbac.ErrGroupNotFound) {
			httputil.WriteError(w, http.StatusNotFound, "group not found")
			return
		}
		httputil.WriteError(w, http.StatusInternalServerError, "failed to delete group")
		return
	}
	httputil.WriteJSON(w, http.StatusOK, map[string]string{"message": "group deleted"})
}

// addMemberRequest is the body shape for
// `POST /api/v1/groups/{groupID}/members`.
type addMemberRequest struct {
	UserID string `json:"user_id"`
}

// groupMembersResponse is the envelope for
// `GET /api/v1/groups/{groupID}/members`.
type groupMembersResponse struct {
	Members []groupMemberJSON `json:"members"`
}

type groupMemberJSON struct {
	UserID      string `json:"user_id"`
	Email       string `json:"email"`
	DisplayName string `json:"display_name"`
}

// AddMember handles POST /api/v1/groups/{groupID}/members.
func (g *Groups) AddMember(w http.ResponseWriter, r *http.Request) {
	id := rbac.GroupID(chi.URLParam(r, "groupID"))
	var req addMemberRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httputil.WriteError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.UserID == "" {
		httputil.WriteError(w, http.StatusBadRequest, "user_id is required")
		return
	}
	if err := g.deps.Groups.AddMember(r.Context(), id, req.UserID); err != nil {
		if errors.Is(err, rbac.ErrGroupNotFound) {
			httputil.WriteError(w, http.StatusNotFound, "group not found")
			return
		}
		httputil.WriteError(w, http.StatusInternalServerError, "failed to add member")
		return
	}
	httputil.WriteJSON(w, http.StatusOK, map[string]string{"message": "member added"})
}

// RemoveMember handles
// DELETE /api/v1/groups/{groupID}/members/{userID}.
func (g *Groups) RemoveMember(w http.ResponseWriter, r *http.Request) {
	id := rbac.GroupID(chi.URLParam(r, "groupID"))
	userID := chi.URLParam(r, "userID")
	if err := g.deps.Groups.RemoveMember(r.Context(), id, userID); err != nil {
		httputil.WriteError(w, http.StatusInternalServerError, "failed to remove member")
		return
	}
	httputil.WriteJSON(w, http.StatusOK, map[string]string{"message": "member removed"})
}

// ListMembers handles GET /api/v1/groups/{groupID}/members.
func (g *Groups) ListMembers(w http.ResponseWriter, r *http.Request) {
	id := rbac.GroupID(chi.URLParam(r, "groupID"))
	members, err := g.deps.Groups.ListMembers(r.Context(), id)
	if err != nil {
		if errors.Is(err, rbac.ErrGroupNotFound) {
			httputil.WriteError(w, http.StatusNotFound, "group not found")
			return
		}
		httputil.WriteError(w, http.StatusInternalServerError, "failed to list members")
		return
	}
	out := make([]groupMemberJSON, 0, len(members))
	for _, m := range members {
		out = append(out, groupMemberJSON{
			UserID:      m.UserID,
			Email:       m.Email,
			DisplayName: m.DisplayName,
		})
	}
	httputil.WriteJSON(w, http.StatusOK, groupMembersResponse{Members: out})
}

// grantGroupRoleRequest is the body shape for
// `PUT /api/v1/groups/{groupID}/roles`.
type grantGroupRoleRequest struct {
	Role   string `json:"role"`
	Domain string `json:"domain"`
}

// groupRolesResponse is the envelope for
// `GET /api/v1/groups/{groupID}/roles`.
type groupRolesResponse struct {
	Roles []rbac.GroupRole `json:"roles"`
}

// GrantGroupRole handles PUT /api/v1/groups/{groupID}/roles.
func (g *Groups) GrantGroupRole(w http.ResponseWriter, r *http.Request) {
	id := rbac.GroupID(chi.URLParam(r, "groupID"))
	var req grantGroupRoleRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httputil.WriteError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if !rbac.IsValidRole(req.Role) {
		httputil.WriteError(w, http.StatusBadRequest, "invalid role")
		return
	}
	if req.Domain == "" {
		req.Domain = rbac.DomainAll
	}
	if err := g.deps.Groups.GrantRole(r.Context(), id, req.Role, req.Domain); err != nil {
		if errors.Is(err, rbac.ErrGroupNotFound) {
			httputil.WriteError(w, http.StatusNotFound, "group not found")
			return
		}
		httputil.WriteError(w, http.StatusInternalServerError, "failed to grant role")
		return
	}
	httputil.WriteJSON(w, http.StatusOK, map[string]string{"message": "role granted"})
}

// revokeGroupRoleRequest is the body shape for
// `DELETE /api/v1/groups/{groupID}/roles`.
type revokeGroupRoleRequest struct {
	Role   string `json:"role"`
	Domain string `json:"domain"`
}

// RevokeGroupRole handles DELETE /api/v1/groups/{groupID}/roles.
func (g *Groups) RevokeGroupRole(w http.ResponseWriter, r *http.Request) {
	id := rbac.GroupID(chi.URLParam(r, "groupID"))
	var req revokeGroupRoleRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httputil.WriteError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Domain == "" {
		req.Domain = rbac.DomainAll
	}
	if err := g.deps.Groups.RevokeRole(r.Context(), id, req.Role, req.Domain); err != nil {
		httputil.WriteError(w, http.StatusInternalServerError, "failed to revoke role")
		return
	}
	httputil.WriteJSON(w, http.StatusOK, map[string]string{"message": "role revoked"})
}

// ListGroupRoles handles GET /api/v1/groups/{groupID}/roles.
func (g *Groups) ListGroupRoles(w http.ResponseWriter, r *http.Request) {
	id := rbac.GroupID(chi.URLParam(r, "groupID"))
	roles, err := g.deps.Groups.ListGroupRoles(r.Context(), id)
	if err != nil {
		httputil.WriteError(w, http.StatusInternalServerError, "failed to list group roles")
		return
	}
	httputil.WriteJSON(w, http.StatusOK, groupRolesResponse{Roles: roles})
}

// ListUserGroups handles GET /api/v1/users/{userID}/groups.
//
// Authorization: a non-sysadmin caller may only query
// their own group memberships. This keeps the "who am
// I in a group with?" UI working for regular users
// without leaking the group list of other users.
func (g *Groups) ListUserGroups(w http.ResponseWriter, r *http.Request) {
	uid := httputil.RequestUserID(r.Context())
	targetUser := chi.URLParam(r, "userID")
	isSysAdmin, _ := g.deps.RBAC.EnforceSystemAdmin(uid.String())
	if !isSysAdmin && uid.String() != targetUser {
		httputil.WriteError(w, http.StatusForbidden, "can only view your own group memberships")
		return
	}
	groups, err := g.deps.Groups.ListForUser(r.Context(), targetUser)
	if err != nil {
		httputil.WriteError(w, http.StatusInternalServerError, "failed to list user groups")
		return
	}
	out := make([]groupJSON, 0, len(groups))
	for _, gr := range groups {
		out = append(out, toGroupJSON(gr))
	}
	httputil.WriteJSON(w, http.StatusOK, listGroupsResponse{Groups: out})
}

// isUniqueViolation reports whether err is a Postgres
// unique-constraint violation. The store package does
// not re-export a typed error for this, so we match by
// the SQLSTATE code (23505) the same way bootstrap.go
// does. We avoid importing pgx in this handler by
// checking the SQLSTATE string pgx embeds in its
// errors.
func isUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	return errContains(err, "SQLSTATE 23505")
}

// errContains walks an error chain looking for a
// substring. Wrapping errors in pgx (and any library
// that implements Unwrap) is honored.
func errContains(err error, sub string) bool {
	for err != nil {
		if strings.Contains(err.Error(), sub) {
			return true
		}
		type unwrapper interface{ Unwrap() error }
		u, ok := err.(unwrapper)
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}
