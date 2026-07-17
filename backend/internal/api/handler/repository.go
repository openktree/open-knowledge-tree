package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/openktree/open-knowledge-tree/backend/internal/api/httputil"
	"github.com/openktree/open-knowledge-tree/backend/internal/providers/ontology"
	"github.com/openktree/open-knowledge-tree/backend/internal/rbac"
	"github.com/openktree/open-knowledge-tree/backend/internal/store"
)

type createRepositoryRequest struct {
	Name         string `json:"name"`
	Slug         string `json:"slug"`
	Description  string `json:"description"`
	DatabaseName string `json:"database_name"`
	// Preset names a repository_preset (e.g. "scientific",
	// "general", "enterprise") whose provider + context lists seed
	// the new repo's settings. Empty falls back to
	// cfg.DefaultRepositoryPreset. Providers/Contexts, when
	// non-empty, override the preset's values for that dimension
	// (so the advanced create form can pick a subset).
	Preset    string              `json:"preset"`
	Providers map[string][]string `json:"providers"`
	Contexts  []string             `json:"contexts"`
}

// Repository bundles the repository-scoped HTTP handlers.
type Repository struct {
	deps Deps
}

// NewRepository constructs a Repository handler bundle.
func NewRepository(d Deps) *Repository {
	return &Repository{deps: d}
}

// SetProviderRegistry wires the live provider catalog the
// CreateRepository seeding iterates. Split out because the
// registry is built in cmd/app/api.go after NewHandler runs (the
// provider maps are env-gated). Propagates to the settings handler
// too when wired.
func (r *Repository) SetProviderRegistry(reg *ProviderRegistry) {
	r.deps.ProviderRegistry = reg
}

// SetOntologySource wires the embedded DBpedia L3 source the
// CreateRepository seeding reads to populate repository_contexts.
func (r *Repository) SetOntologySource(s ontology.L3Source) {
	r.deps.OntologySource = s
}

// SetLazyEnsureRepository wires the lazy default-repository
// bootstrap callback. The wiring layer re-binds this after the
// ProviderRegistry + OntologySource are in place (so the seeder
// the callback closes over is fully wired); the Repository
// handler captured a value copy of Deps at construction time, so
// without this setter it would keep calling the placeholder
// closure from NewHandler.
func (r *Repository) SetLazyEnsureRepository(fn func(ctx context.Context, ownerID string) error) {
	r.deps.LazyEnsureRepository = fn
}

// ListRepositories handles GET /repositories.
func (r *Repository) ListRepositories(w http.ResponseWriter, req *http.Request) {
	uid := httputil.RequestUserID(req.Context())
	isSysAdmin, _ := r.deps.RBAC.EnforceSystemAdmin(uid.String())

	// Lazy bootstrap: when the bootstrap flag is on, the
	// repositories table is empty, and the caller owns no
	// repositories yet, attach the calling user as the owner
	// of a freshly-created starter repository. This is the
	// fix for the "I registered but the Repositories page is
	// empty" bug: previously, EnsureDefaultRepository ran
	// only at startup, before any user existed, so it always
	// no-op'd on a fresh install. The lazy path in this
	// handler runs *after* the user authenticates, so a
	// starter repo is always available on first list.
	//
	// The callback is best-effort: a misconfigured bootstrap
	// is logged at the call site and must never turn a
	// 200-with-empty-list into a 5xx for the user. The
	// startup path is the source of truth for the
	// "should this even run?" flag (cfg.Bootstrap.DefaultRepository),
	// so the callback itself can short-circuit cheaply when
	// the flag is off.
	if r.deps.LazyEnsureRepository != nil {
		if err := r.deps.LazyEnsureRepository(req.Context(), uid.String()); err != nil {
			// Don't fail the request; the caller's
			// repositories list is still valid (just
			// empty). Logging here is intentional: a
			// bootstrap failure usually means a
			// permission/DB problem that ops needs to
			// see, but a single user request
			// shouldn't be the only place that
			// surfaces it. The startup path also
			// logs on failure.
			log.Printf("lazy default repository bootstrap: %v", err)
		}
	}

	var repos []store.Repository
	var err error

	if isSysAdmin {
		repos, err = r.deps.Store.ListAllRepositories(req.Context())
	} else {
		repos, err = r.deps.Store.ListRepositoriesByOwner(req.Context(), uid)
	}
	if err != nil {
		httputil.WriteError(w, http.StatusInternalServerError, "failed to list repositories")
		return
	}

	type repoWithRoles struct {
		ID           string   `json:"id"`
		Name         string   `json:"name"`
		Slug         string   `json:"slug"`
		Description  string   `json:"description"`
		OwnerID      string   `json:"owner_id"`
		DatabaseName string   `json:"database_name"`
		Tier         string   `json:"tier"`
		Roles        []string `json:"roles"`
	}

	result := make([]repoWithRoles, 0, len(repos))
	for _, repo := range repos {
		roles, _ := r.deps.RBAC.GetRolesForUser(uid.String(), repo.ID.String())
		if roles == nil {
			roles = []string{}
		}
		if isSysAdmin {
			// Sys admins can manage every repo; inject the
			// role so the frontend's canManage() check works
			// without a per-repo Casbin lookup (the sysadmin
			// role lives in the "system" domain, not the repo
			// UUID domain).
			found := false
			for _, r := range roles {
				if r == rbac.RoleSysAdmin {
					found = true
					break
				}
			}
			if !found {
				roles = append(roles, rbac.RoleSysAdmin)
			}
		}
		if repo.OwnerID.String() == uid.String() && len(roles) == 0 {
			roles = []string{rbac.RoleRepoAdmin}
		}

		result = append(result, repoWithRoles{
			ID:           repo.ID.String(),
			Name:         repo.Name,
			Slug:         repo.Slug,
			Description:  repo.Description,
			OwnerID:      repo.OwnerID.String(),
			DatabaseName: repo.DatabaseName,
			Tier:         repo.Tier,
			Roles:        roles,
		})
	}

	httputil.WriteJSON(w, http.StatusOK, map[string]interface{}{
		"repositories": result,
	})
}

// CreateRepository handles POST /repositories.
func (r *Repository) CreateRepository(w http.ResponseWriter, req *http.Request) {
	var body createRepositoryRequest
	if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
		httputil.WriteError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	if body.Name == "" || body.Slug == "" {
		httputil.WriteError(w, http.StatusBadRequest, "name and slug are required")
		return
	}

	// Resolve the database the new repository will live in. The
	// rules:
	//   - Empty body field: always use the default.
	//   - Permitted caller (sys admin or system-scope
	//     repositories.manage) picks a name in the allow-list:
	//     accepted.
	//   - Permitted caller picks a name NOT in the allow-list:
	//     400 (real client error).
	//   - Non-permitted caller picks anything: silently
	//     overridden to the default. The picker is opt-in for
	//     the server to enforce; the client can render it for
	//     UX, but the server is the source of truth.
	uid := httputil.RequestUserID(req.Context())
	dbName, dbErr := r.resolveDatabaseName(uid.String(), body.DatabaseName)
	if dbErr != nil {
		httputil.WriteError(w, http.StatusBadRequest, dbErr.Error())
		return
	}

	repo, err := r.deps.Store.CreateRepository(req.Context(), store.CreateRepositoryParams{
		Name:         body.Name,
		Slug:         body.Slug,
		Description:  body.Description,
		OwnerID:      uid,
		DatabaseName: dbName,
		Tier:         r.deps.Config.Isolation.TierForDatabaseName(dbName),
	})
	if err != nil {
		if strings.Contains(err.Error(), "unique") || strings.Contains(err.Error(), "duplicate") {
			httputil.WriteError(w, http.StatusConflict, "repository slug already exists")
			return
		}
		httputil.WriteError(w, http.StatusInternalServerError, "failed to create repository")
		return
	}

	_ = r.deps.RBAC.AddRoleForUser(uid.String(), rbac.RoleRepoAdmin, repo.ID.String())

	// Seed per-repository settings (providers + contexts) from the
	// resolved preset/overrides. Settings are the source of truth
	// for every gate (search/retrieve/extract), so a repo with no
	// seed is non-functional; a seeding failure fails the whole
	// create so the caller sees a clear error instead of a repo
	// that can't ingest.
	seed, seedErr := resolveSeed(req.Context(), r.deps, createSeedBody{
		Preset:    body.Preset,
		Providers: body.Providers,
		Contexts:  body.Contexts,
	})
	if seedErr != nil {
		// Best-effort rollback: delete the repo row so a failed
		// seed doesn't leave an unconfigured repo. The RBAC role
		// grant is orphaned but harmless (the repo is gone).
		_, _ = r.deps.Store.DeleteRepository(req.Context(), repo.ID)
		httputil.WriteError(w, http.StatusBadRequest, "failed to seed repository settings: "+seedErr.Error())
		return
	}
	if err := seedRepositorySettings(req.Context(), r.deps, repo.ID, seed); err != nil {
		log.Printf("create repository: seeding settings for repo %s: %v", repo.ID, err)
		_, _ = r.deps.Store.DeleteRepository(req.Context(), repo.ID)
		httputil.WriteError(w, http.StatusInternalServerError, "failed to seed repository settings")
		return
	}

	httputil.WriteJSON(w, http.StatusCreated, repo)
}

// resolveDatabaseName returns the database name to use for a new
// repository, applying the picker policy described on
// CreateRepository. The returned name is always a member of
// `cfg.Databases` and is guaranteed to have a registered pool
// in the registry. The default database is always allowed; the
// picker allow-list is configured under
// `cfg.Isolation.AllowedDatabases`.
//
// Returns an error when the caller is permitted to pick and the
// requested name is not in the registry. The error is the 400
// body the handler should write.
func (r *Repository) resolveDatabaseName(uid, requested string) (string, error) {
	defaultDB := r.deps.Config.Isolation.DefaultDatabase

	if requested == "" {
		return defaultDB, nil
	}

	// Is the requested name even a database the registry has
	// open? We use cfg.Databases (the operator's declaration)
	// rather than the picker allow-list, because the picker
	// allow-list is what controls *who* may pick, not what is
	// pickable. The system DB is always allowed regardless of
	// the picker config.
	if _, ok := r.deps.Config.Databases[requested]; !ok {
		if r.deps.RBAC.CanPickRepositoryDatabase(uid) {
			return "", fmt.Errorf("database_name %q is not a registered database", requested)
		}
		return defaultDB, nil
	}

	if !r.deps.RBAC.CanPickRepositoryDatabase(uid) {
		return defaultDB, nil
	}
	return requested, nil
}

// GetRepository handles GET /repositories/{repoID}.
func (r *Repository) GetRepository(w http.ResponseWriter, req *http.Request) {
	repoID := chi.URLParam(req, "repoID")

	var uid pgtype.UUID
	if err := uid.Scan(repoID); err != nil {
		httputil.WriteError(w, http.StatusBadRequest, "invalid repository id")
		return
	}

	repo, err := r.deps.Store.GetRepositoryByID(req.Context(), uid)
	if err != nil {
		httputil.WriteError(w, http.StatusNotFound, "repository not found")
		return
	}

	httputil.WriteJSON(w, http.StatusOK, repo)
}

// UpdateRepository handles PUT /repositories/{repoID}.
func (r *Repository) UpdateRepository(w http.ResponseWriter, req *http.Request) {
	repoID := chi.URLParam(req, "repoID")

	var uid pgtype.UUID
	if err := uid.Scan(repoID); err != nil {
		httputil.WriteError(w, http.StatusBadRequest, "invalid repository id")
		return
	}

	var body struct {
		Name        string `json:"name"`
		Description string `json:"description"`
	}
	_ = httputil.DecodeBody(req, &body)

	repo, err := r.deps.Store.UpdateRepository(req.Context(), store.UpdateRepositoryParams{
		ID:          uid,
		Name:        body.Name,
		Description: body.Description,
	})
	if err != nil {
		httputil.WriteError(w, http.StatusInternalServerError, "failed to update repository")
		return
	}

	httputil.WriteJSON(w, http.StatusOK, repo)
}

// DeleteRepository handles DELETE /repositories/{repoID}.
func (r *Repository) DeleteRepository(w http.ResponseWriter, req *http.Request) {
	repoID := chi.URLParam(req, "repoID")

	var uid pgtype.UUID
	if err := uid.Scan(repoID); err != nil {
		httputil.WriteError(w, http.StatusBadRequest, "invalid repository id")
		return
	}

	_, err := r.deps.Store.DeleteRepository(req.Context(), uid)
	if err != nil {
		httputil.WriteError(w, http.StatusInternalServerError, "failed to delete repository")
		return
	}

	httputil.WriteJSON(w, http.StatusOK, map[string]string{"message": "repository deleted"})
}

// GetMyPermissions handles GET /repositories/{repoID}/permissions.
func (r *Repository) GetMyPermissions(w http.ResponseWriter, req *http.Request) {
	uid := httputil.RequestUserID(req.Context())
	repoID := chi.URLParam(req, "repoID")

	perms, err := r.deps.RBAC.GetPermissionsForUser(uid.String(), repoID)
	if err != nil {
		httputil.WriteError(w, http.StatusInternalServerError, "failed to get permissions")
		return
	}

	isSysAdmin, _ := r.deps.RBAC.EnforceSystemAdmin(uid.String())

	var permList []rbac.Permission
	if isSysAdmin {
		permList = append(permList, rbac.Permission{Resource: "*", Action: "*"})
	} else {
		for _, p := range perms {
			if len(p) >= 4 {
				permList = append(permList, rbac.Permission{
					Resource: p[2],
					Action:   p[3],
				})
			}
		}
	}

	httputil.WriteJSON(w, http.StatusOK, map[string]interface{}{
		"user_id":       uid.String(),
		"repository_id": repoID,
		"permissions":   permList,
		"system_admin":  isSysAdmin,
	})
}
