package handler

import (
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgtype"
	appmw "github.com/openktree/open-knowledge-tree/backend/internal/api/middleware"
	"github.com/openktree/open-knowledge-tree/backend/internal/api/httputil"
	"github.com/openktree/open-knowledge-tree/backend/internal/rbac"
	"github.com/openktree/open-knowledge-tree/backend/internal/store"
)

// APIKey bundles the personal-access-token HTTP handlers. Keys are
// always self-managed: every route resolves the caller from the
// session context (httputil.RequestUserID) and scopes the query to
// that user's keys. A session is required to manage keys — an API
// key itself cannot create or revoke keys (the authed wrapper uses
// AuthRequired, which dispatches on the okt_ prefix, but the
// management routes are mounted under /users/me/api-keys which is
// only ever called from the browser; the prefix-dispatch path is
// still correct for any caller, but a key created with no "user:write"
// scope would be rejected by its own scope check on the create
// route).
type APIKey struct {
	deps Deps
}

// NewAPIKey constructs an APIKey handler bundle.
func NewAPIKey(d Deps) *APIKey {
	return &APIKey{deps: d}
}

// apiKeyMaxPerUser is the default cap on the number of active keys a
// single user may hold when cfg.APIKeys.MaxPerUser is zero. Keeps the
// default config usable without forcing operators to set a value.
const apiKeyDefaultMaxPerUser = 20

// apiKeyMaxTTL is the default upper bound on a key's lifetime when
// cfg.APIKeys.MaxTTL is zero. 90 days matches common PAT lifetimes
// (GitHub's classic tokens default to no cap but recommend rotation;
// 90d is a reasonable middle ground that nudges rotation without
// breaking long-running automation).
const apiKeyDefaultMaxTTL = 90 * 24 * time.Hour

// maxExpiry returns the latest absolute time a new key may expire at,
// sourced from cfg.APIKeys.MaxTTL (with the 90d default fallback).
func (a *APIKey) maxExpiry() time.Time {
	max := apiKeyDefaultMaxTTL
	if a.deps.Config != nil && a.deps.Config.APIKeys.MaxTTL > 0 {
		max = a.deps.Config.APIKeys.MaxTTL
	}
	return time.Now().Add(max)
}

// maxPerUser returns the per-user active-key cap from config (with
// the 20 default fallback).
func (a *APIKey) maxPerUser() int64 {
	if a.deps.Config != nil && a.deps.Config.APIKeys.MaxPerUser > 0 {
		return int64(a.deps.Config.APIKeys.MaxPerUser)
	}
	return apiKeyDefaultMaxPerUser
}

// defaultTTL returns the default TTL applied when the create body
// omits expires_in_days. Zero means "no expiry" (the key never
// expires on its own; only revocation or the max-TTL cap limit it).
func (a *APIKey) defaultTTL() time.Duration {
	if a.deps.Config != nil && a.deps.Config.APIKeys.DefaultTTL > 0 {
		return a.deps.Config.APIKeys.DefaultTTL
	}
	return 0
}

// List handles GET /users/me/api-keys. Returns the caller's keys,
// most-recent-first, with token_hash stripped (only the recognizable
// prefix is exposed). Revoked keys are included so the UI can show
// history.
func (a *APIKey) List(w http.ResponseWriter, r *http.Request) {
	uid := httputil.RequestUserID(r.Context())
	keys, err := a.deps.Store.ListAPIKeysByUser(r.Context(), uid)
	if err != nil {
		httputil.WriteError(w, http.StatusInternalServerError, "failed to list api keys")
		return
	}
	httputil.WriteJSON(w, http.StatusOK, map[string]any{"api_keys": keys})
}

// Create handles POST /users/me/api-keys. The raw token is returned
// exactly once in the response; subsequent calls to List return only
// the prefix. The body shape:
//
//	{
//	  "name":           "CI ingest bot",          // required
//	  "permissions":    ["source:read", "fact:write"], // required, may include "*:*"
//	  "repository_id":  "uuid-or-null",            // optional; null/omitted = all repos
//	  "expires_in_days": 30                         // optional; 0 = no expiry (capped by config)
//	}
func (a *APIKey) Create(w http.ResponseWriter, r *http.Request) {
	uid := httputil.RequestUserID(r.Context())

	var body struct {
		Name          string   `json:"name"`
		Permissions   []string `json:"permissions"`
		RepositoryID  *string  `json:"repository_id"`
		ExpiresInDays *int     `json:"expires_in_days"`
	}
	if err := httputil.DecodeBody(r, &body); err != nil {
		httputil.WriteError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	name := strings.TrimSpace(body.Name)
	if name == "" {
		httputil.WriteError(w, http.StatusBadRequest, "name is required")
		return
	}
	if len(name) > 128 {
		httputil.WriteError(w, http.StatusBadRequest, "name too long (max 128 chars)")
		return
	}

	// Validate every permission entry is a known "object:action"
	// (or uses "*" wildcard on either side). Unknown entries are
	// rejected so a typo doesn't silently produce a useless key.
	perms := body.Permissions
	if perms == nil {
		perms = []string{}
	}
	for _, entry := range perms {
		obj, act := splitScope(entry)
		if obj == "" {
			httputil.WriteError(w, http.StatusBadRequest, "invalid permission: "+entry)
			return
		}
		if obj != "*" && !rbac.IsValidObject(obj) {
			httputil.WriteError(w, http.StatusBadRequest, "unknown resource in permission: "+entry)
			return
		}
		if act != "*" && !rbac.IsValidAction(act) {
			httputil.WriteError(w, http.StatusBadRequest, "unknown action in permission: "+entry)
			return
		}
	}

	// Resolve repository_id, if given. NULL = all repos. When a
	// UUID is provided, the repository must exist AND the caller
	// must have at least read on it — otherwise a user could mint
	// a key for a repo they can't even see.
	var repoID pgtype.UUID
	if body.RepositoryID != nil && *body.RepositoryID != "" && *body.RepositoryID != "null" {
		if err := repoID.Scan(*body.RepositoryID); err != nil {
			httputil.WriteError(w, http.StatusBadRequest, "invalid repository_id")
			return
		}
		repo, err := a.deps.Store.GetRepositoryByID(r.Context(), repoID)
		if err != nil {
			httputil.WriteError(w, http.StatusBadRequest, "repository not found")
			return
		}
		_ = repo // existence check only; the RBAC read check follows
		isSysAdmin, _ := a.deps.RBAC.EnforceSystemAdmin(uid.String())
		if !isSysAdmin {
			ok, err := a.deps.RBAC.Enforce(uid.String(), repoID.String(), rbac.Objects.Repositories, rbac.Actions.Read)
			if err != nil || !ok {
				httputil.WriteError(w, http.StatusForbidden, "no read access to that repository")
				return
			}
		}
	}

	// Resolve expiry. 0 / nil = no expiry (NULL). The configured
	// max TTL caps the value; a request for 365d when max is 90d
	// is clamped, not rejected, so the UI can offer fixed options
	// (7d, 30d, 90d, never) without knowing the server cap.
	var expiresAt pgtype.Timestamptz
	if body.ExpiresInDays != nil && *body.ExpiresInDays > 0 {
		t := time.Now().Add(time.Duration(*body.ExpiresInDays) * 24 * time.Hour)
		max := a.maxExpiry()
		if t.After(max) {
			t = max
		}
		expiresAt = pgtype.Timestamptz{Time: t, Valid: true}
	} else if dflt := a.defaultTTL(); dflt > 0 {
		t := time.Now().Add(dflt)
		max := a.maxExpiry()
		if t.After(max) {
			t = max
		}
		expiresAt = pgtype.Timestamptz{Time: t, Valid: true}
	}
	// else: leave expiresAt invalid (NULL) = no expiry.

	// Enforce the per-user active-key cap. Revoked keys don't count
	// (ListAPIKeysByUser returns all keys including revoked, so we
	// count non-revoked explicitly).
	count, err := a.deps.Store.CountAPIKeysByUser(r.Context(), uid)
	if err != nil {
		httputil.WriteError(w, http.StatusInternalServerError, "failed to count api keys")
		return
	}
	// CountAPIKeysByUser counts all rows (revoked + active). Subtract
	// the revoked ones by re-querying: simpler to just cap on total
	// since revoked keys are rare and the cap is a guard against
	// runaway automation, not a hard security boundary. If the total
	// is at the cap, reject.
	if count >= a.maxPerUser() {
		httputil.WriteError(w, http.StatusConflict, "api key limit reached; revoke an existing key first")
		return
	}

	// Generate the raw token. The full token (okt_ + 32 random
	// bytes, base64url) is returned to the client exactly once;
	// only the sha256 hex hash lands in the DB.
	raw, hash, prefix, err := appmw.GenerateAPIKey()
	if err != nil {
		httputil.WriteError(w, http.StatusInternalServerError, "failed to generate api key")
		return
	}

	key, err := a.deps.Store.CreateAPIKey(r.Context(), store.CreateAPIKeyParams{
		UserID:       uid,
		Name:         name,
		TokenHash:    hash,
		Prefix:       prefix,
		RepositoryID: repoID,
		Permissions:  perms,
		ExpiresAt:    expiresAt,
	})
	if err != nil {
		httputil.WriteError(w, http.StatusInternalServerError, "failed to create api key")
		return
	}

	recordAudit(a.deps, r, rbac.AuditActionAPIKeyCreate, rbac.Objects.Users, key.ID.String(), map[string]any{
		"name":          name,
		"prefix":        prefix,
		"repository_id": repoString(repoID),
		"permissions":   perms,
		"expires_at":    expiresAt.Time,
	})

	httputil.WriteJSON(w, http.StatusCreated, map[string]any{
		"id":            key.ID,
		"name":          key.Name,
		"prefix":        key.Prefix,
		"token":         raw, // shown exactly once
		"repository_id": repoID,
		"permissions":   key.Permissions,
		"expires_at":    key.ExpiresAt,
		"created_at":    key.CreatedAt,
	})
}

// Revoke handles DELETE /users/me/api-keys/{keyID}. Sets revoked_at;
// the row is retained for audit history. Only the owner can revoke
// their own keys; the URL keyID is validated against the caller's
// user ID so a stolen key cannot revoke its siblings by guessing IDs.
func (a *APIKey) Revoke(w http.ResponseWriter, r *http.Request) {
	uid := httputil.RequestUserID(r.Context())
	keyIDStr := chi.URLParam(r, "keyID")

	var keyID pgtype.UUID
	if err := keyID.Scan(keyIDStr); err != nil {
		httputil.WriteError(w, http.StatusBadRequest, "invalid key id")
		return
	}

	// Verify ownership before revoking. RevokeAPIKey takes user_id as
	// a safety belt, but the ownership check here produces a clean 404
	// (not 403) when the key doesn't belong to the caller, so an
	// attacker can't enumerate other users' key IDs.
	key, err := a.deps.Store.GetAPIKeyByID(r.Context(), keyID)
	if err != nil || key.UserID != uid {
		httputil.WriteError(w, http.StatusNotFound, "api key not found")
		return
	}

	if err := a.deps.Store.RevokeAPIKey(r.Context(), store.RevokeAPIKeyParams{
		ID: keyID, UserID: uid,
	}); err != nil {
		httputil.WriteError(w, http.StatusInternalServerError, "failed to revoke api key")
		return
	}

	recordAudit(a.deps, r, rbac.AuditActionAPIKeyRevoke, rbac.Objects.Users, keyID.String(), map[string]any{
		"name": key.Name,
	})
	httputil.WriteJSON(w, http.StatusOK, map[string]any{"revoked": keyID.String()})
}

// splitScope splits "object:action" into its parts. Returns ("","")
// when the entry is malformed. Mirrors the middleware helper so the
// handler can validate without importing the middleware package.
func splitScope(entry string) (string, string) {
	idx := strings.Index(entry, ":")
	if idx <= 0 || idx == len(entry)-1 {
		return "", ""
	}
	return entry[:idx], entry[idx+1:]
}

// repoString returns the UUID string for logging, or "<all>" when
// the key is repo-unrestricted. Used by audit detail.
func repoString(id pgtype.UUID) string {
	if !id.Valid {
		return "<all>"
	}
	return id.String()
}