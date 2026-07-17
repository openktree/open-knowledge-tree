package handler

import (
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/openktree/open-knowledge-tree/backend/internal/api/httputil"
	"github.com/openktree/open-knowledge-tree/backend/internal/rbac"
)

// User bundles the user-profile and self-permissions HTTP handlers.
type User struct {
	deps Deps
}

// NewUser constructs a User handler bundle.
func NewUser(d Deps) *User {
	return &User{deps: d}
}

// GetOwnPermissions handles GET /permissions (current user's effective
// permissions).
func (u *User) GetOwnPermissions(w http.ResponseWriter, r *http.Request) {
	uid := httputil.RequestUserID(r.Context())

	isSysAdmin, _ := u.deps.RBAC.EnforceSystemAdmin(uid.String())

	var permList []rbac.Permission
	if isSysAdmin {
		permList = append(permList, rbac.Permission{Resource: "*", Action: "*"})
	} else {
		perms, _ := u.deps.RBAC.GetPermissionsForUser(uid.String(), "*")
		seen := make(map[string]bool)
		for _, p := range perms {
			if len(p) >= 4 {
				key := p[2] + ":" + p[3]
				if !seen[key] {
					seen[key] = true
					permList = append(permList, rbac.Permission{Resource: p[2], Action: p[3]})
				}
			}
		}
	}

	httputil.WriteJSON(w, http.StatusOK, map[string]interface{}{
		"user_id":      uid.String(),
		"permissions":  permList,
		"system_admin": isSysAdmin,
	})
}

// GetMe handles GET /users/me.
func (u *User) GetMe(w http.ResponseWriter, r *http.Request) {
	uid := httputil.RequestUserID(r.Context())
	user, err := u.deps.Users.GetUser(r.Context(), rbac.UserID(uid.String()))
	if err != nil {
		httputil.WriteError(w, http.StatusNotFound, "user not found")
		return
	}

	httputil.WriteJSON(w, http.StatusOK, user)
}

// GetProfile handles GET /users/{userID}.
func (u *User) GetProfile(w http.ResponseWriter, r *http.Request) {
	userID := chi.URLParam(r, "userID")

	var uid pgtype.UUID
	if err := uid.Scan(userID); err != nil {
		httputil.WriteError(w, http.StatusBadRequest, "invalid user id")
		return
	}

	user, err := u.deps.Users.GetUser(r.Context(), rbac.UserID(userID))
	if err != nil {
		httputil.WriteError(w, http.StatusNotFound, "user not found")
		return
	}

	httputil.WriteJSON(w, http.StatusOK, user)
}

// UpdateProfile handles PUT /users/{userID}.
func (u *User) UpdateProfile(w http.ResponseWriter, r *http.Request) {
	userID := chi.URLParam(r, "userID")
	uid := httputil.RequestUserID(r.Context())

	// Sysadmins can update any user's profile (not just their own).
	isSysAdmin, _ := u.deps.RBAC.EnforceSystemAdmin(uid.String())
	if !isSysAdmin && uid.String() != userID {
		httputil.WriteError(w, http.StatusForbidden, "cannot update another user's profile")
		return
	}

	var body struct {
		DisplayName string `json:"display_name"`
	}
	_ = httputil.DecodeBody(r, &body)

	user, err := u.deps.Users.UpdateUser(r.Context(), rbac.UserID(userID), body.DisplayName)
	if err != nil {
		httputil.WriteError(w, http.StatusInternalServerError, "failed to update profile")
		return
	}

	httputil.WriteJSON(w, http.StatusOK, user)
}
