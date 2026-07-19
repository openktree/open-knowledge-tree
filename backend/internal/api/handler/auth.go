package handler

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/openktree/open-knowledge-tree/backend/internal/api/httputil"
	"github.com/openktree/open-knowledge-tree/backend/internal/auth"
	"github.com/openktree/open-knowledge-tree/backend/internal/rbac"
	"github.com/openktree/open-knowledge-tree/backend/internal/store"
)

type registerRequest struct {
	Email       string `json:"email"`
	Password    string `json:"password"`
	DisplayName string `json:"display_name"`
}

type loginRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

// Auth bundles the authentication-related HTTP handlers.
type Auth struct {
	deps Deps
}

// NewAuth constructs an Auth handler bundle.
func NewAuth(d Deps) *Auth {
	return &Auth{deps: d}
}

// Register handles POST /auth/register.
func (a *Auth) Register(w http.ResponseWriter, r *http.Request) {
	var req registerRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httputil.WriteError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	if req.Email == "" || req.Password == "" {
		httputil.WriteError(w, http.StatusBadRequest, "email and password are required")
		return
	}

	hash, err := auth.HashPassword(req.Password)
	if err != nil {
		httputil.WriteError(w, http.StatusInternalServerError, "failed to hash password")
		return
	}

	user, err := a.deps.Users.CreateUser(r.Context(), req.Email, hash, req.DisplayName)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			httputil.WriteError(w, http.StatusConflict, "email already exists")
			return
		}
		httputil.WriteError(w, http.StatusInternalServerError, "failed to create user")
		return
	}

	// First-user autopromotion: when the bootstrap flag is on and
	// the users table was empty just before this insert (so the row
	// we just wrote is the only one), grant sysadmin on the system
	// domain so a fresh `docker compose up` + register yields a
	// usable admin with no env vars or psql surgery. The count==1
	// check (rather than count==0 pre-insert) avoids a race between
	// two concurrent first-registrations: only the insert that
	// landed first sees count==1, the second sees count==2 and is
	// not promoted. If EnsureDefaultAdmin already seeded a user at
	// boot, count will be >=2 here and the guard never trips, so
	// the explicit default_admin path wins when configured. A log
	// line is emitted so an operator notices if it fires
	// unexpectedly on a public deployment.
	if a.deps.Config.Bootstrap.AutoPromoteFirstUser {
		count, err := a.deps.Store.CountUsers(r.Context())
		if err != nil {
			log.Printf("warn: autopromote CountUsers failed for %s: %v", user.ID, err)
		} else if count == 1 {
			if err := a.deps.RBAC.AddRoleForUser(string(user.ID), rbac.RoleSysAdmin, rbac.DomainSystem); err != nil {
				log.Printf("warn: autopromote AddRoleForUser failed for %s: %v", user.ID, err)
			} else {
				log.Printf("bootstrap: autopromoted first registered user %q to sysadmin (system). Set bootstrap.auto_promote_first_user=false (or OKT_BOOTSTRAP_AUTO_PROMOTE=false) on a public deployment.", user.Email)
			}
		}
	}

	httputil.WriteJSON(w, http.StatusCreated, user)
}

// Login handles POST /auth/login.
func (a *Auth) Login(w http.ResponseWriter, r *http.Request) {
	var req loginRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httputil.WriteError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	user, err := a.deps.Store.GetUserByEmail(r.Context(), req.Email)
	if err != nil {
		httputil.WriteError(w, http.StatusUnauthorized, "invalid credentials")
		return
	}

	if !auth.CheckPassword(user.PasswordHash, req.Password) {
		httputil.WriteError(w, http.StatusUnauthorized, "invalid credentials")
		return
	}

	rawToken, err := auth.GenerateSessionToken()
	if err != nil {
		httputil.WriteError(w, http.StatusInternalServerError, "failed to generate session")
		return
	}

	tokenHash := auth.HashToken(rawToken)
	_, err = a.deps.Store.CreateSession(r.Context(), store.CreateSessionParams{
		UserID:    user.ID,
		TokenHash: tokenHash,
		ExpiresAt: httputil.PgTimestamptz(time.Now().Add(a.deps.Config.Auth.TokenTTL)),
	})
	if err != nil {
		httputil.WriteError(w, http.StatusInternalServerError, "failed to create session")
		return
	}

	jwtToken, err := auth.GenerateToken(a.deps.Config.Auth.JWTSecret, a.deps.Config.Auth.TokenTTL, httputil.UUIDToString(user.ID), user.Email)
	if err != nil {
		httputil.WriteError(w, http.StatusInternalServerError, "failed to generate token")
		return
	}

	httputil.WriteJSON(w, http.StatusOK, map[string]string{
		"token":        rawToken,
		"jwt":          jwtToken,
		"display_name": user.DisplayName,
		"email":        user.Email,
	})
}

// Logout handles POST /auth/logout.
func (a *Auth) Logout(w http.ResponseWriter, r *http.Request) {
	tokenStr := r.Header.Get("Authorization")
	if tokenStr == "" || len(tokenStr) < 8 || tokenStr[:7] != "Bearer " {
		httputil.WriteError(w, http.StatusUnauthorized, "missing token")
		return
	}

	tokenHash := auth.HashToken(tokenStr[7:])
	session, err := a.deps.Store.GetSessionByTokenHash(r.Context(), tokenHash)
	if err != nil {
		httputil.WriteJSON(w, http.StatusOK, map[string]string{"message": "logged out"})
		return
	}

	_ = a.deps.Store.DeleteSession(r.Context(), session.ID)
	httputil.WriteJSON(w, http.StatusOK, map[string]string{"message": "logged out"})
}

// RefreshToken handles POST /auth/refresh.
func (a *Auth) RefreshToken(w http.ResponseWriter, r *http.Request) {
	tokenStr := r.Header.Get("Authorization")
	if tokenStr == "" || len(tokenStr) < 8 || tokenStr[:7] != "Bearer " {
		httputil.WriteError(w, http.StatusUnauthorized, "missing token")
		return
	}

	tokenHash := auth.HashToken(tokenStr[7:])
	session, err := a.deps.Store.GetSessionByTokenHash(r.Context(), tokenHash)
	if err != nil {
		httputil.WriteError(w, http.StatusUnauthorized, "invalid session")
		return
	}

	_ = a.deps.Store.DeleteSession(r.Context(), session.ID)

	rawToken, err := auth.GenerateSessionToken()
	if err != nil {
		httputil.WriteError(w, http.StatusInternalServerError, "failed to generate session")
		return
	}

	newHash := auth.HashToken(rawToken)
	_, err = a.deps.Store.CreateSession(r.Context(), store.CreateSessionParams{
		UserID:    session.UserID,
		TokenHash: newHash,
		ExpiresAt: httputil.PgTimestamptz(time.Now().Add(a.deps.Config.Auth.TokenTTL)),
	})
	if err != nil {
		httputil.WriteError(w, http.StatusInternalServerError, "failed to create session")
		return
	}

	httputil.WriteJSON(w, http.StatusOK, map[string]string{"token": rawToken})
}
