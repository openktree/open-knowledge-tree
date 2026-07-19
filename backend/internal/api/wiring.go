// Package api wires together the HTTP middlewares and handler bundles and
// exposes the resulting chi router. Concrete handlers live in
// internal/api/handler, middlewares in internal/api/middleware, and
// shared helpers in internal/api/httputil.
package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	chimw "github.com/go-chi/chi/v5/middleware"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/openktree/open-knowledge-tree/backend/internal/api/handler"
	appmw "github.com/openktree/open-knowledge-tree/backend/internal/api/middleware"
	"github.com/openktree/open-knowledge-tree/backend/internal/bootstrap"
	"github.com/openktree/open-knowledge-tree/backend/internal/config"
	"github.com/openktree/open-knowledge-tree/backend/internal/dbpool"
	"github.com/openktree/open-knowledge-tree/backend/internal/providers/ontology"
	"github.com/openktree/open-knowledge-tree/backend/internal/providers/registry"
	"github.com/openktree/open-knowledge-tree/backend/internal/promptset"
	"github.com/openktree/open-knowledge-tree/backend/internal/rbac"
	"github.com/openktree/open-knowledge-tree/backend/internal/store"
)

// Handler is the application's HTTP wiring layer. It holds the shared
// dependencies and the handler bundles needed to build a router.
type Handler struct {
	deps    handler.Deps
	auth    *handler.Auth
	user    *handler.User
	admin   *handler.Admin
	adminDB *handler.AdminDB
	repo    *handler.Repository
	source  *handler.Source
	storage *handler.Storage
	tasks   *handler.Tasks
	adminTasks *handler.AdminTasks
	groups  *handler.Groups
	ai      *handler.AI
	aiUsage *handler.AIUsage
	investigations *handler.Investigations
	concepts       *handler.Concepts
	summaries      *handler.Summaries
	syntheses      *handler.Syntheses
	reports        *handler.Reports
	oauth          *handler.OAuth
	mcp           *handler.MCP
	repoSettings  *handler.RepositorySettings
	promptsets    *handler.Promptsets
	remote        *handler.Remote
	repoDBCache    *appmw.RepoDBCache
	slugCache   *appmw.SlugCache
	// providerGateCache memoizes the per-repo enabled-provider set
	// (5-min TTL) so the gate doesn't hit the DB on every search.
	providerGateCache   map[string]providerGateEntry
	providerGateCacheMu sync.Mutex
}

// NewHandler constructs a Handler with the given shared dependencies.
// `queries` is the default-pool store.Queries (used for system
// tables and for the repositories registry); `registry` is the
// connection-pool registry the per-repo middleware uses to route
// repository-scoped queries to the right pool.
//
// The system pool is the *pgxpool.Pool that backs the system
// database (the one named by cfg.System.Database). It is passed
// explicitly so the rbac.GroupManager can be constructed here;
// the manager needs the pool to talk to the `groups` and
// `group_members` tables, and avoiding a setter keeps the
// handler bundles built in NewHandler in a single, atomic step.
//
// The per-repo routing cache (RepoDBCache) is built here so its
// TTL is a single place to change; the default is 5 minutes,
// which is short enough that a tier-upgrade takes effect quickly
// but long enough that a busy repo doesn't hit the system DB on
// every request.
//
// The repository handler reads `deps.LazyEnsureRepository` to
// optionally run the default-repository bootstrap from inside
// GET /repositories. We wire it here to bootstrap.EnsureDefaultRepository
// so the lazy path and the startup path use the exact same
// insertion logic; tests that need a no-op simply leave
// `deps.LazyEnsureRepository` nil (the handler checks for nil
// before calling it).
func NewHandler(queries *store.Queries, cfg *config.Config, rbacSvc *rbac.Service, systemPool *pgxpool.Pool, registry *dbpool.Registry) *Handler {
	deps := handler.Deps{
		Store:    queries,
		Config:   cfg,
		RBAC:     rbacSvc,
		Registry: registry,
		LazyEnsureRepository: func(ctx context.Context, ownerID string) error {
			// Placeholder; SetOntologySource rebinds this once
			// the ProviderRegistry + OntologySource are wired so
			// the seeder can actually seed. Until then, calling
			// this returns an error (which the repository handler
			// logs and swallows), so a lazy call that races ahead
			// of the wiring just retries on the next list.
			_, err := bootstrap.EnsureDefaultRepository(ctx, registry, cfg, ownerID, nil)
			return err
		},
	}
	if systemPool != nil {
		deps.Groups = rbac.NewGroupManager(systemPool, rbacSvc)
		deps.Users = rbac.NewUserManager(systemPool, rbacSvc)
	}
	return &Handler{
		deps:        deps,
		auth:        handler.NewAuth(deps),
		user:        handler.NewUser(deps),
		admin:       handler.NewAdmin(deps),
		adminDB:     handler.NewAdminDB(deps),
		repo:        handler.NewRepository(deps),
		source:      nil, // set via SetSource once providers are wired up
		storage:     nil, // set via SetStorage once the storage backend is wired up
		tasks:       nil, // set via SetTasks once the task client is wired up
		groups:      handler.NewGroups(deps),
		ai:          nil, // set via SetAI once AI providers are wired up
		aiUsage:     handler.NewAIUsage(queries, cfg),
		investigations: handler.NewInvestigations(deps),
		concepts:       handler.NewConcepts(deps),
		summaries:      handler.NewSummaries(deps),
		syntheses:      handler.NewSyntheses(deps),
		reports:        handler.NewReports(deps),
		repoSettings:   handler.NewRepositorySettings(deps),
		promptsets:     handler.NewPromptsets(deps),
		repoDBCache:    appmw.NewRepoDBCache(queries, 5*time.Minute),
		slugCache:   appmw.NewSlugCache(queries, 5*time.Minute),
	}
}

// SetProviderRegistry wires the live provider catalog the
// CreateRepository seeding iterates and the settings UI lists.
// Split out from NewHandler because the registry is built in
// cmd/app/api.go from the same provider maps passed to NewSource
// (those maps are env-var-gated and not known at NewHandler time).
// Also wires the registry onto deps so the RepositorySettings
// handler can intersect stored rows with the live set.
func (h *Handler) SetProviderRegistry(r *handler.ProviderRegistry) {
	h.deps.ProviderRegistry = r
	if h.repo != nil {
		h.repo.SetProviderRegistry(r)
	}
	if h.repoSettings != nil {
		h.repoSettings.SetRegistry(r)
	}
	// Wire the gate cache invalidator now that the settings
	// handler is fully constructed so toggles take effect
	// immediately instead of waiting for the 5-min TTL.
	h.SetGateInvalidator()
}

// SetOntologySource wires the embedded curated context vocabulary
// the CreateRepository seeding reads to populate repository_contexts.
// Wired alongside SetProviderRegistry; the server always uses
// EmbeddedL3Source (the local file) — the file is the single
// source of truth, refreshed by editing contexts.json and
// redeploying. Propagates to the Repository handler (which owns
// CreateRepository seeding) and the settings handler (which lists
// presets).
func (h *Handler) SetOntologySource(s ontology.L3Source) {
	h.deps.OntologySource = s
	if h.repo != nil {
		h.repo.SetOntologySource(s)
	}
	// Now that both the ProviderRegistry (wired by SetProviderRegistry)
	// and the OntologySource are in place, build the default-settings
	// seeder and rebind the lazy default-repository bootstrap so the
	// freshly-created default repo gets seeded with providers +
	// contexts instead of being left with no settings (which the
	// search/retrieve/extract gates deny). The seeder reads
	// h.deps at call time, so it sees the fully-wired registry +
	// ontology source. Rebinding the closure here (rather than in
	// NewHandler) avoids capturing a half-wired deps.
	h.deps.DefaultSettingsSeeder = func(ctx context.Context, repoID string) error {
		return handler.SeedDefaultRepositorySettings(ctx, h.deps, repoID)
	}
	h.deps.LazyEnsureRepository = func(ctx context.Context, ownerID string) error {
		_, err := bootstrap.EnsureDefaultRepository(ctx, h.deps.Registry, h.deps.Config, ownerID, h.deps.DefaultSettingsSeeder)
		return err
	}
	// Propagate the re-bound lazy callback to the Repository handler,
	// which captured its own value copy of Deps at NewHandler time.
	// Without this, ListRepositories would keep calling the
	// placeholder closure (which passes nil as the seeder) and the
	// default repo would be created with no settings.
	if h.repo != nil {
		h.repo.SetLazyEnsureRepository(h.deps.LazyEnsureRepository)
	}
}

// SetSource attaches the source-provider handler bundle. It is split out
// from NewHandler because the providers may not be known at construction
// time (e.g. when API keys come from env vars).
//
// It also wires the per-repository pool resolver the TestSearch handler
// uses to look up already-fetched sources in the active repository.
// The resolver reuses the same Registry / RepoDBCache / system Store
// the WithRepoQueries middleware uses, so the search-side resolution
// and the per-repo-route resolution agree on which pool backs a given
// repository.
func (h *Handler) SetSource(s *handler.Source) {
	h.source = s
	if s != nil && h.deps.Registry != nil && h.repoDBCache != nil {
		s.SetRepoPoolResolver(h.resolveRepoPool)
	}
	// Wire the per-repo provider gate. The gate reads
	// repository_provider_settings from the system pool (the same
	// pool Deps.Store uses) and intersects with the live registry.
	// A 5-min cache (matching RepoDBCache) avoids a DB hit per
	// search; the cache is keyed by repoID and invalidated implicitly
	// by the TTL — a settings change takes effect within the TTL.
	if s != nil {
		s.SetProviderRegistry(h.deps.ProviderRegistry)
		s.SetSettingsGate(h.repoProviderGate)
		// Wire the system-pool store so the content-type gate
		// (per-repo allowed_content_types, migration 0049) can
		// read the repo's allow-list and 403-reject disallowed
		// source kinds at CreateSource / UploadSource /
		// EnqueueRetrieveSource.
		s.SetSystemStore(h.deps.Store)
	}
}

// resolveRepoPool is the per-repository pool resolver shared by the
// source handler's TestSearch "already-added" tagging. It mirrors
// what appmw.WithRepoQueries does inside the middleware: parse the
// repo ID, look up its database_name (via the cache, hitting the
// system DB on a miss), and hand back the registered pool for that
// name. The parsed pgtype.UUID is returned alongside so the caller
// can pass it straight to per-repo queries.
func (h *Handler) resolveRepoPool(ctx context.Context, repoID string) (*pgxpool.Pool, pgtype.UUID, error) {
	if repoID == "" {
		return nil, pgtype.UUID{}, errors.New("repository_id is required")
	}
	var id pgtype.UUID
	if err := id.Scan(repoID); err != nil {
		return nil, pgtype.UUID{}, fmt.Errorf("invalid repository_id: %w", err)
	}
	dbName, err := h.repoDBCache.Get(ctx, id)
	if err != nil {
		return nil, pgtype.UUID{}, fmt.Errorf("resolving repository database: %w", err)
	}
	pool := h.deps.Registry.Get(dbName).Pool
	if pool == nil {
		return nil, pgtype.UUID{}, fmt.Errorf("no pool registered for database %q", dbName)
	}
	return pool, id, nil
}

// repoProviderGate is the RepoProviderGate implementation backed by
// the system Store + a 5-minute in-memory cache (keyed by repoID).
// It reads ListEnabledRepositoryProviderIDs, intersects with the
// live registry (orphans ignored), and returns the enabled set.
// The cache avoids a DB hit per search; the TTL matches
// RepoDBCache so a settings change takes effect within 5 minutes
// (an admin toggling a provider sees the gate update on the next
// search after the cache entry expires). Returns (nil, false, nil)
// when the repo has no stored rows — the caller (Source) treats
// "checked=true, enabled=false" as "deny all", which is the
// settings-as-source-of-truth behavior.
func (h *Handler) repoProviderGate(ctx context.Context, repoID string) (map[[2]string]bool, bool, error) {
	if repoID == "" {
		return nil, false, nil
	}
	var id pgtype.UUID
	if err := id.Scan(repoID); err != nil {
		return nil, false, fmt.Errorf("invalid repository_id: %w", err)
	}
	// Cache lookup.
	h.providerGateCacheMu.Lock()
	if h.providerGateCache == nil {
		h.providerGateCache = make(map[string]providerGateEntry)
	}
	if e, ok := h.providerGateCache[repoID]; ok && time.Since(e.fetchedAt) < 5*time.Minute {
		h.providerGateCacheMu.Unlock()
		return e.set, e.ok, nil
	}
	h.providerGateCacheMu.Unlock()

	rows, err := h.deps.Store.ListEnabledRepositoryProviderIDs(ctx, id)
	if err != nil {
		return nil, false, fmt.Errorf("listing enabled providers: %w", err)
	}
	if len(rows) == 0 {
		// No stored rows → deny all. Cache the "deny" result too so
		// a misconfigured repo doesn't hit the DB on every search.
		h.cacheProviderGate(repoID, nil, false)
		return nil, true, nil
	}
	// Intersect with the live registry (orphans ignored).
	live := map[[2]string]bool{}
	if h.deps.ProviderRegistry != nil {
		live = h.deps.ProviderRegistry.LiveProviderIDs()
	}
	set := make(map[[2]string]bool, len(rows))
	for _, r := range rows {
		key := [2]string{r.ProviderKind, r.ProviderID}
		if len(live) == 0 || live[key] {
			set[key] = true
		}
	}
	h.cacheProviderGate(repoID, set, true)
	return set, true, nil
}

func (h *Handler) cacheProviderGate(repoID string, set map[[2]string]bool, ok bool) {
	h.providerGateCacheMu.Lock()
	defer h.providerGateCacheMu.Unlock()
	if h.providerGateCache == nil {
		h.providerGateCache = make(map[string]providerGateEntry)
	}
	h.providerGateCache[repoID] = providerGateEntry{set: set, ok: ok, fetchedAt: time.Now()}
}

// InvalidateProviderGate drops the cached enabled-provider set for
// the given repo so the next gate call re-reads from the DB. Called
// by SetProviderEnabled after a successful upsert so a toggle takes
// effect immediately (instead of up to the 5-min cache TTL). The
// repoID is the string form the settings handler resolved from the
// URL param; a no-op when the entry isn't cached.
func (h *Handler) InvalidateProviderGate(repoID string) {
	if repoID == "" {
		return
	}
	h.providerGateCacheMu.Lock()
	defer h.providerGateCacheMu.Unlock()
	delete(h.providerGateCache, repoID)
}

type providerGateEntry struct {
	set        map[[2]string]bool
	ok         bool
	fetchedAt  time.Time
}

// SetStorage attaches the storage handler bundle (the endpoints that
// serve stored source assets). Split out for the same reason as
// SetSource: the storage backend is built from config/env in
// cmd/app/api.go after NewHandler runs.
func (h *Handler) SetStorage(s *handler.Storage) {
	h.storage = s
}

// SetTaskEnqueuer attaches the background-task enqueuer the source
// handler uses to fan work out of a request. It's split out the
// same way SetSource is so wiring stays explicit. The source
// handler holds the only consumer of the enqueuer today, so we
// forward it via the source bundle rather than passing it through
// every handler method.
func (h *Handler) SetTaskEnqueuer(eq handler.TaskEnqueuer) {
	if h.source != nil {
		h.source.SetTaskEnqueuer(eq)
	}
	if h.reports != nil {
		h.reports.SetTaskEnqueuer(eq)
	}
}

// SetMigrateEnqueuer wires the migrate-context enqueuer the settings
// handler uses. Split from SetTaskEnqueuer because the contract is
// different (MigrateEnqueuer vs TaskEnqueuer); the wiring layer
// adapts the same River client to both via separate adapters.
func (h *Handler) SetMigrateEnqueuer(eq handler.MigrateEnqueuer) {
	if h.repoSettings != nil {
		h.repoSettings.SetMigrateEnqueuer(eq)
	}
}

// SetRegistrySyncEnqueuer wires the registry sync (contribute-all /
// pull-all) enqueuer the settings handler uses. Split from
// SetMigrateEnqueuer because the contract is different; the wiring
// layer adapts the same River client via its own adapter.
func (h *Handler) SetRegistrySyncEnqueuer(eq handler.RegistrySyncEnqueuer) {
	if h.repoSettings != nil {
		h.repoSettings.SetRegistrySyncEnqueuer(eq)
	}
}

// SetRemote attaches the remote-registry handler bundle and wires
// the per-registry client map onto the settings handler (so the
// SetRegistrySettings endpoint can validate the selected id against
// the configured registries). Split out from NewHandler because the
// registry *Client is built from config after the task manager is
// constructed (the Client is also passed to the task manager's
// workers). When nil, the /{repoID}/remote routes return 503 and the
// frontend hides the nav link.
func (h *Handler) SetRemote(r *handler.Remote) {
	h.remote = r
}

// SetRegistryClients wires the per-registry client map onto the
// settings handler so the registry selector + on/off toggle can
// validate against the configured registries. Called once during
// wiring, after the client map is built from cfg.Providers.
func (h *Handler) SetRegistryClients(m *registry.ClientMap) {
	if h.repoSettings != nil {
		h.repoSettings.SetRegistryClients(m)
	}
	if h.remote != nil {
		h.remote.SetClientMap(m)
		h.remote.SetStore(h.deps.Store)
	}
}

// SetModelCatalog wires the AI model catalog onto the settings handler
// so the per-repo model selection UI can list and validate models.
func (h *Handler) SetModelCatalog(c *handler.ModelCatalog) {
	if h.repoSettings != nil {
		h.repoSettings.SetModelCatalog(c)
	}
}

// SetPromptsetResolver wires the promptset resolver (built-in + DB)
// onto the promptsets CRUD handler and the repository-settings
// handler (so SetPromptset can validate hashes). Called once during
// wiring, after the resolver is built in cmd/app/api.go.
func (h *Handler) SetPromptsetResolver(r *promptset.Resolver) {
	if h.promptsets != nil {
		h.promptsets.SetResolver(r)
	}
	if h.repoSettings != nil {
		h.repoSettings.SetPromptsetResolver(r)
	}
}

// SetRemoteDedupEnqueuer wires the task enqueuer the remote handler
// uses to kick off the embed→dedup pipeline after pulling a source.
func (h *Handler) SetRemoteDedupEnqueuer(eq handler.RemoteDedupEnqueuer) {
	if h.remote != nil {
		h.remote.SetDedupEnqueuer(eq)
	}
}

// SetRemotePullBatchEnqueuer wires the task enqueuer the remote
// handler uses to kick off a pull_remote_batch job (the "Pull page"
// / "Pull all results" buttons on the Remote page).
func (h *Handler) SetRemotePullBatchEnqueuer(eq handler.RemotePullBatchEnqueuer) {
	if h.remote != nil {
		h.remote.SetPullBatchEnqueuer(eq)
	}
}

// SetGateInvalidator wires the per-repo provider gate cache
// invalidator onto the settings handler so a provider toggle takes
// effect immediately. The callback closes over the Handler's
// InvalidateProviderGate method. Called once during wiring (after
// the settings handler is constructed); safe to call multiple times.
func (h *Handler) SetGateInvalidator() {
	if h.repoSettings != nil {
		h.repoSettings.SetGateInvalidator(h.InvalidateProviderGate)
	}
}

// SetTasks attaches the tasks handler bundle. It is split out from
// NewHandler because the River client may not be known at
// construction time. The same client backs the admin-only
// AdminTasks bundle (cancel/get), which is constructed here so
// the two bundles share a single River client instance.
func (h *Handler) SetTasks(t *handler.Tasks) {
	h.tasks = t
	if t != nil {
		h.adminTasks = handler.NewAdminTasksFromTasks(t)
	}
}

// SetAI attaches the AI provider handler bundle. It is split out
// from NewHandler because the AI providers may not be known at
// construction time (e.g. when API keys come from env vars).
func (h *Handler) SetAI(a *handler.AI) {
	h.ai = a
}

// SetOAuth attaches the OAuth 2.1 authorization-server handler
// bundle. Split out because the internal/oauth.Server needs the
// system-pool *store.Queries and the UserLookup callback, which the
// wiring layer builds after NewHandler (the queries instance is the
// same one NewHandler receives, but the callback closes over it).
// When nil, the /api/v1/oauth/* routes are not registered and the
// /.well-known/oauth-* endpoints return 404.
func (h *Handler) SetOAuth(o *handler.OAuth) {
	h.oauth = o
}

// SetMCP attaches the MCP server handler bundle. Split out for the
// same reason as SetOAuth: the MCP handler needs the per-call
// repository resolver, which closes over the RepoDBCache + SlugCache
// the Handler owns. When nil, the /api/v1/mcp route is not
// registered.
func (h *Handler) SetMCP(m *handler.MCP) {
	h.mcp = m
}

// Deps returns the shared dependency bundle. The wiring layer uses it
// to build the per-call repository resolver for the MCP handler; the
// MCP handler needs the same Store / RBAC / Config the REST handlers
// use, so we expose the bundle rather than re-constructing it.
func (h *Handler) Deps() handler.Deps { return h.deps }

// RepoDBCache returns the repository→database-name cache. Exported
// so the MCP handler's per-call resolver can reuse the same cache
// the per-repo chi middleware uses, keeping resolution consistent.
func (h *Handler) RepoDBCache() *appmw.RepoDBCache { return h.repoDBCache }

// SlugCache returns the slug→repository-UUID cache. Same purpose as
// RepoDBCache; the MCP resolver tries UUID first and falls back to
// slug, mirroring appmw.WithRepoQueries.
func (h *Handler) SlugCache() *appmw.SlugCache { return h.slugCache }

// Router returns the fully configured chi router for the API.
func (h *Handler) Router() chi.Router {
	r := chi.NewRouter()

	r.Use(chimw.Logger)
	r.Use(chimw.Recoverer)
	r.Use(chimw.RequestID)
	r.Use(chimw.RealIP)
	r.Use(appmw.NoRobots)

	r.Route("/api/v1", func(r chi.Router) {
		r.Route("/auth", h.authRoutes)
		r.Route("/users", h.userRoutes)
		r.Route("/sources", h.sourceRoutes)
		r.Route("/admin", h.adminRoutes)
		r.Route("/repositories", h.repoRoutes)
		r.Route("/tasks", h.tasksRoutes)
		r.Route("/groups", h.groupsRoutes)
		r.Route("/ai", h.aiRoutes)
		r.Route("/promptsets", h.promptsetsRoutes)
		r.Get("/permissions", h.authed(h.user.GetOwnPermissions))
		// OAuth 2.1 authorization server. Mounts the authorize,
		// token, register, and revoke endpoints; the
		// /.well-known/oauth-* discovery documents are mounted at
		// the router root (outside /api/v1) because the RFCs put
		// them at /.well-known/. The MCP endpoint is mounted here
		// too so it shares the chi middleware stack.
		if h.oauth != nil {
			r.Route("/oauth", h.oauthRoutes)
		}
		// MCP server endpoint. Wrapped with OAuthBearer so every
		// tools/call requires a valid OAuth access JWT; the
		// bearer's user id lands on the context the same way
		 // AuthRequired puts it for the REST routes.
		if h.mcp != nil {
			r.Post("/mcp", appmw.OAuthBearer(
				h.deps.Config.Auth.JWTSecret,
				h.deps.Config.OAuth.Issuer+"/.well-known/oauth-protected-resource",
				h.mcp.ServeHTTP,
			))
		}
	})

	// Well-known discovery documents live at the router root (not
	// under /api/v1) per RFC 8414 / RFC 9728. The OAuth bundle serves
	// both; when OAuth is not wired, the routes are absent and
	// clients get a 404.
	if h.oauth != nil {
		r.Get("/.well-known/oauth-authorization-server", h.oauth.Metadata)
		r.Get("/.well-known/oauth-protected-resource", h.oauth.ProtectedResource)
	}

	return r
}

// oauthRoutes registers the OAuth 2.1 authorization-server
// endpoints under /api/v1/oauth. The authorize endpoint accepts
// both GET (show login/consent) and POST (consent decision); the
// login form posts to a sub-route so the form-action target is
// distinct from the authorize GET.
func (h *Handler) oauthRoutes(r chi.Router) {
	r.Post("/register", h.oauth.Register)
	r.Get("/authorize", h.oauth.Authorize)
	r.Post("/authorize", h.oauth.Authorize)
	r.Post("/authorize/login", h.oauth.AuthorizeLoginPOST)
	r.Post("/token", h.oauth.Token)
	r.Post("/revoke", h.oauth.Revoke)
}

func (h *Handler) authRoutes(r chi.Router) {
	r.Post("/register", h.auth.Register)
	r.Post("/login", h.auth.Login)
	r.Post("/logout", h.auth.Logout)
	r.Post("/refresh", h.auth.RefreshToken)
}

// userRoutes is mounted on /api/v1/users. The
// {userID} pattern must come last so the more specific
// /me route matches first; chi prefers literal
// segments over params. /me is defined before
// /{userID} for that reason.
func (h *Handler) userRoutes(r chi.Router) {
	r.Get("/me", h.authed(h.user.GetMe))
	r.Get("/{userID}", h.authed(h.user.GetProfile))
	r.Put("/{userID}", h.authed(h.user.UpdateProfile))
	// /users/{userID}/groups is the per-user "what
	// groups am I in?" view. The Groups bundle
	// enforces the self-or-sysadmin rule; the route
	// only requires authentication. Mounted here
	// (not in groupsRoutes) because chi's
	// {userID} pattern lives under /users/.
	if h.groups != nil {
		r.Get("/{userID}/groups", h.authed(h.groups.ListUserGroups))
	}
}

func (h *Handler) adminRoutes(r chi.Router) {
	r.Group(func(r chi.Router) {
		r.Get("/users", h.perm("user", "read", h.admin.ListUsers))
		r.Put("/users/roles", h.perm("role", "manage", h.admin.AssignRole))
		r.Delete("/users/roles", h.perm("role", "manage", h.admin.RemoveRole))
		r.Get("/permissions", h.perm("role", "read", h.admin.ListPermissions))
		r.Get("/databases", h.perm("database", "read", h.adminDB.ListDatabases))

		// Admin task control. The cancel endpoint lets an
		// operator with the task:cancel permission recover a
		// stuck River job (e.g. an extract_concepts pass holding
		// a transaction for hours because the upstream LLM
		// provider hung) without `docker exec psql`. The get
		// endpoint is the same shape as the read-side
		// /tasks/{jobID} but lives under /admin so the RBAC gate
		// is obvious. When the task manager is not configured,
		// both routes return 503 via the notConfigured handler.
		if h.adminTasks != nil {
			r.Get("/tasks/{jobID}", h.perm("task", "read", h.adminTasks.GetJob))
			r.Post("/tasks/{jobID}/cancel", h.perm("task", "cancel", h.adminTasks.CancelJob))
			r.Post("/tasks/rescue", h.perm("task", "manage", h.adminTasks.RescueStuckJobs))
		} else {
			r.Get("/tasks/{jobID}", notConfigured)
			r.Post("/tasks/{jobID}/cancel", notConfigured)
			r.Post("/tasks/rescue", notConfigured)
		}
	})
}

func (h *Handler) repoRoutes(r chi.Router) {
	r.Group(func(r chi.Router) {
		r.Get("/", h.authed(h.repo.ListRepositories))
		r.Post("/", h.perm("repository", "write", h.repo.CreateRepository))

		// Repository presets (the "type" dropdown on the create
		// form). Authed-only — any logged-in user can create a repo
		// and pick a preset. Mounted before the /{repoID} group so
		// the literal "presets" segment matches before the param.
		r.Get("/presets", h.authed(h.repoSettings.ListPresets))

		// Per-repo route. {repoID} may be a UUID or a slug;
		// the middleware resolves either to the database pool
		// and attaches both the pool and the repo UUID to the
		// request context.
		r.Route("/{repoID}", func(r chi.Router) {
			r.Use(appmw.WithRepoQueries(h.deps.Registry, h.repoDBCache, h.slugCache))

			// Repository CRUD (system-side, ignores the pool).
			r.Get("/", h.repoPerm("repository", "read", h.repo.GetRepository))
			r.Put("/", h.repoPerm("repository", "update", h.repo.UpdateRepository))
			r.Delete("/", h.repoPerm("repository", "delete", h.repo.DeleteRepository))
			r.Get("/permissions", h.authed(h.repo.GetMyPermissions))

		// Per-repository settings (providers + contexts).
		r.Get("/settings", h.repoPerm("repository", "manage", h.repoSettings.GetSettings))
		r.Put("/settings/providers", h.repoPerm("repository", "manage", h.repoSettings.SetProviderEnabled))
		r.Put("/settings/models", h.repoPerm("repository", "manage", h.repoSettings.SetModelSetting))
		r.Get("/settings/reports", h.repoPerm("repository", "manage", h.repoSettings.GetReportSettings))
		r.Put("/settings/reports", h.repoPerm("repository", "manage", h.repoSettings.SetReportSettings))
		r.Post("/settings/contexts", h.repoPerm("repository", "manage", h.repoSettings.AddContext))
		r.Put("/settings/contexts/{context}", h.repoPerm("repository", "manage", h.repoSettings.UpdateContext))
		r.Post("/settings/contexts/{context}/migrate", h.repoPerm("repository", "manage", h.repoSettings.MigrateContext))
		r.Delete("/settings/contexts/{context}", h.repoPerm("repository", "manage", h.repoSettings.DeleteContext))
		// Context mappings (local ↔ registry) — see migration 0038.
		r.Get("/settings/context-mappings", h.repoPerm("repository", "manage", h.repoSettings.ListContextMappings))
		r.Put("/settings/context-mappings", h.repoPerm("repository", "manage", h.repoSettings.UpsertContextMapping))
		r.Delete("/settings/context-mappings/{localContext}", h.repoPerm("repository", "manage", h.repoSettings.DeleteContextMappingHandler))
		r.Put("/settings/unmapped-policy", h.repoPerm("repository", "manage", h.repoSettings.SetUnmappedPolicy))
		r.Post("/settings/contribute-all", h.repoPerm("repository", "manage", h.repoSettings.ContributeAll))
			r.Post("/settings/pull-all", h.repoPerm("repository", "manage", h.repoSettings.PullAllFromRegistry))
			r.Put("/settings/auto-contribute", h.repoPerm("repository", "manage", h.repoSettings.SetAutoContribute))
			r.Put("/settings/registry", h.repoPerm("repository", "manage", h.repoSettings.SetRegistrySettings))
			r.Put("/settings/sync-levels", h.repoPerm("repository", "manage", h.repoSettings.SetSyncLevels))
			r.Put("/settings/content-types", h.repoPerm("repository", "manage", h.repoSettings.SetContentTypes))
		r.Get("/settings/promptset", h.repoPerm("repository", "manage", h.repoSettings.GetPromptset))
		r.Put("/settings/promptset", h.repoPerm("repository", "manage", h.repoSettings.SetPromptset))
		r.Get("/settings/contributor", h.repoPerm("repository", "manage", h.repoSettings.GetContributor))
		r.Put("/settings/contributor", h.repoPerm("repository", "manage", h.repoSettings.SetContributor))

			// Remote registry browse / pull.
			if h.remote != nil {
				r.Get("/remote", h.repoPerm("remote", "read", h.remote.ListSources))
				r.Get("/remote/{sourceID}", h.repoPerm("remote", "read", h.remote.GetSource))
				r.Get("/remote/{sourceID}/decompositions/{modelID}", h.repoPerm("remote", "read", h.remote.GetDecomposition))
				r.Post("/remote/{sourceID}/pull", h.repoPerm("remote", "write", h.remote.PullSource))
				r.Post("/remote/pull-batch", h.repoPerm("remote", "write", h.remote.PullBatch))
			} else {
				r.Get("/remote", notConfigured)
				r.Get("/remote/{sourceID}", notConfigured)
				r.Get("/remote/{sourceID}/decompositions/{modelID}", notConfigured)
				r.Post("/remote/{sourceID}/pull", notConfigured)
				r.Post("/remote/pull-batch", notConfigured)
			}

			// Per-repo data plane.
			r.Get("/sources", h.repoPerm("source", "read", h.source.ListSources))
			r.Post("/sources", h.repoPerm("source", "write", h.source.CreateSource))
			r.Post("/sources/upload", h.repoPerm("source", "write", h.source.UploadSource))
			r.Get("/sources/{sourceID}", h.repoPerm("source", "read", h.source.GetSource))
			r.Delete("/sources/{sourceID}", h.repoPerm("source", "delete", h.source.DeleteSource))
			r.Post("/sources/{sourceID}/process", h.repoPerm("source", "update", h.source.ProcessSource))
			r.Post("/sources/{sourceID}/retry", h.repoPerm("source", "update", h.source.RetrySource))
			r.Get("/sources/{sourceID}/facts", h.repoPerm("source", "read", h.source.ListFacts))
			r.Get("/sources/{sourceID}/references", h.repoPerm("source", "read", h.source.ListSourceReferences))
			r.Get("/facts", h.repoPerm("fact", "read", h.source.ListRepoFacts))
			r.Get("/facts/{factID}", h.repoPerm("fact", "read", h.source.GetFact))

	// Concepts: read surface for the concept-extraction pipeline.
	r.Get("/concepts", h.repoPerm("concept", "read", h.concepts.ListConcepts))
	r.Get("/concepts/{conceptID}/relations", h.repoPerm("concept", "read", h.concepts.ListConceptRelations))
	r.Get("/concepts/{conceptID}/relations/{otherConceptID}", h.repoPerm("concept", "read", h.concepts.GetConceptRelationDetails))
	r.Get("/concepts/{conceptID}", h.repoPerm("concept", "read", h.concepts.GetConcept))
	r.Get("/concepts/{conceptID}/facts", h.repoPerm("concept", "read", h.concepts.ListConceptFacts))
		r.Get("/concepts/{conceptID}/summaries", h.repoPerm("concept", "read", h.summaries.ListByConcept))
		r.Get("/concepts/{conceptID}/definition", h.repoPerm("concept", "read", h.syntheses.GetDefinition))
		r.Get("/facts/{factID}/concepts", h.repoPerm("fact", "read", h.concepts.ListFactConcepts))

			// Per-repo scoped task list.
			if h.tasks != nil {
				r.Get("/tasks", h.repoPerm("task", "read", h.tasks.ListRepoJobs))
			}

			// Investigations.
			r.Route("/investigations", func(r chi.Router) {
				r.Get("/", h.repoPerm("investigation", "read", h.investigations.ListInvestigations))
				r.Post("/", h.repoPerm("investigation", "write", h.investigations.CreateInvestigation))
				r.Route("/{invID}", func(r chi.Router) {
					r.Get("/", h.repoPerm("investigation", "read", h.investigations.GetInvestigation))
					r.Put("/", h.repoPerm("investigation", "update", h.investigations.UpdateInvestigation))
					r.Delete("/", h.repoPerm("investigation", "delete", h.investigations.DeleteInvestigation))
				r.Get("/sources", h.repoPerm("investigation", "read", h.investigations.ListSources))
				r.Post("/sources", h.repoPerm("investigation", "write", h.investigations.AddSource))
				r.Delete("/sources/{sourceID}", h.repoPerm("investigation", "delete", h.investigations.RemoveSource))
				r.Get("/facts", h.repoPerm("investigation", "read", h.investigations.ListFacts))
				r.Get("/concepts", h.repoPerm("investigation", "read", h.investigations.ListConcepts))
				})
			})

			// Reports.
			r.Route("/reports", func(r chi.Router) {
				r.Get("/", h.repoPerm("report", "read", h.reports.ListReports))
				r.Post("/", h.repoPerm("report", "write", h.reports.CreateReport))
				r.Post("/upload", h.repoPerm("report", "write", h.reports.UploadReport))
				r.Route("/{reportID}", func(r chi.Router) {
					r.Get("/", h.repoPerm("report", "read", h.reports.GetReport))
					r.Put("/", h.repoPerm("report", "update", h.reports.UpdateReport))
					r.Delete("/", h.repoPerm("report", "delete", h.reports.DeleteReport))
					r.Post("/annotate", h.repoPerm("report", "update", h.reports.AnnotateReport))
					r.Get("/annotations", h.repoPerm("report", "read", h.reports.ListAnnotations))
				})
			})

			// Stored source assets.
			if h.storage != nil {
				r.Get("/sources/{sourceID}/images/{imageID}", h.repoPerm("source", "read", h.storage.ServeSourceImage))
				r.Get("/sources/{sourceID}/body", h.repoPerm("source", "read", h.storage.ServeSourceBody))
			}
		})
	})
}

func (h *Handler) sourceRoutes(r chi.Router) {
	if h.source == nil {
		// Defensive: the source bundle is optional. Register a 503 so
		// the route still resolves if it was never wired.
		r.Get("/providers", notConfigured)
		r.Post("/{provider}/search", notConfigured)
		r.Post("/classify", notConfigured)
		r.Post("/retrieve", notConfigured)
		return
	}
	r.Get("/providers", h.perm("source_provider", "read", h.source.ListProviders))
	r.Post("/{provider}/search", h.perm("source_provider", "execute", h.source.TestSearch))
	r.Post("/classify", h.perm("source_provider", "execute", h.source.ClassifyResource))
	r.Post("/retrieve", h.perm("source_provider", "execute", h.source.EnqueueRetrieveSource))
	// /decomposition/providers is the source/AI-tab's
	// third sibling: a separate namespace for the chunking +
	// fact-extraction providers that feed the
	// source_decomposition worker. The permission is
	// decomposition:read (the same one a viewer carries),
	// which keeps read-only tabs usable for any role that
	// can already see sources.
	r.Get("/decomposition/providers", h.perm("decomposition", "read", h.source.ListDecompositionProviders))
}

// authed wraps next with the AuthRequired middleware.
func (h *Handler) authed(next http.HandlerFunc) http.HandlerFunc {
	return appmw.AuthRequired(h.deps.Store, next)
}

// perm wraps next with both AuthRequired and the (resource, action)
// permission check. Order matches the pre-refactor behavior: AuthRequired
// runs first (setting the user ID on the context) and RequirePermission
// runs second, reading that user ID to perform the RBAC check.
func (h *Handler) perm(resource, action string, next http.HandlerFunc) http.HandlerFunc {
	return appmw.AuthRequired(h.deps.Store, appmw.RequirePermission(h.deps.RBAC, resource, action, next))
}

// repoPerm wraps next with both AuthRequired and the (resource, action)
// permission check using the repository ID from the URL context (set by
// WithRepoQueries). Used for repo-scope routes under /{repoID} where the
// domain must come from the URL, not the X-Repository-ID header.
func (h *Handler) repoPerm(resource, action string, next http.HandlerFunc) http.HandlerFunc {
	return appmw.AuthRequired(h.deps.Store, appmw.RequireRepoPermission(h.deps.RBAC, resource, action, next))
}

// notConfigured is a fallback for routes whose dependencies were not
// wired up at construction time.
func notConfigured(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusServiceUnavailable)
	_, _ = w.Write([]byte(`{"error":"service not configured"}`))
}

func (h *Handler) tasksRoutes(r chi.Router) {
	if h.tasks == nil {
		r.Get("/", notConfigured)
		r.Get("/stats", notConfigured)
		r.Get("/{jobID}", notConfigured)
		return
	}
	r.Get("/", h.perm("task", "read", h.tasks.ListJobs))
	r.Get("/stats", h.perm("task", "read", h.tasks.ListJobStats))
	r.Get("/{jobID}", h.perm("task", "read", h.tasks.GetJob))
}

// groupsRoutes registers the /api/v1/groups routes. The
// group manager is optional at construction time (the
// rbac.GroupManager needs the system pool, which the
// wiring layer provides via SetGroups). When it has not
// been wired, the bundle returns 503 for every route so
// misconfigured deployments fail loudly instead of 404.
//
// Authorization: the handlers themselves enforce
// sysadmin-only mutations (see handler/groups.go); the
// router mounts everything behind AuthRequired so even
// reads need a session.
func (h *Handler) groupsRoutes(r chi.Router) {
	if h.groups == nil {
		// Mirror the notConfigured pattern used by
		// sourceRoutes and tasksRoutes so a missing
		// wiring is loud and obvious.
		r.Get("/", notConfigured)
		r.Get("/{groupID}", notConfigured)
		r.Post("/", notConfigured)
		r.Patch("/{groupID}", notConfigured)
		r.Delete("/{groupID}", notConfigured)
		r.Get("/{groupID}/members", notConfigured)
		r.Post("/{groupID}/members", notConfigured)
		r.Delete("/{groupID}/members/{userID}", notConfigured)
		r.Get("/{groupID}/roles", notConfigured)
		r.Put("/{groupID}/roles", notConfigured)
		r.Delete("/{groupID}/roles", notConfigured)
		return
	}
	r.Group(func(r chi.Router) {
		r.Get("/", h.authed(h.groups.ListGroups))
		r.Post("/", h.perm("group", "manage", h.groups.CreateGroup))
		r.Get("/{groupID}", h.authed(h.groups.GetGroup))
		r.Patch("/{groupID}", h.perm("group", "manage", h.groups.UpdateGroup))
		r.Delete("/{groupID}", h.perm("group", "manage", h.groups.DeleteGroup))
		r.Get("/{groupID}/members", h.authed(h.groups.ListMembers))
		r.Post("/{groupID}/members", h.perm("group", "manage", h.groups.AddMember))
		r.Delete("/{groupID}/members/{userID}", h.perm("group", "manage", h.groups.RemoveMember))
		r.Get("/{groupID}/roles", h.authed(h.groups.ListGroupRoles))
		r.Put("/{groupID}/roles", h.perm("group", "manage", h.groups.GrantGroupRole))
		r.Delete("/{groupID}/roles", h.perm("group", "manage", h.groups.RevokeGroupRole))
	})
	// /api/v1/users/{userID}/groups is mounted under
	// userRoutes (where the existing {userID} pattern
	// lives) to avoid breaking chi's route matching.
	// The handler enforces the self-or-sysadmin rule
	// itself.
}

func (h *Handler) aiRoutes(r chi.Router) {
	if h.ai == nil {
		r.Get("/providers", notConfigured)
		r.Get("/embedding/providers", notConfigured)
		r.Post("/{provider}/chat", notConfigured)
	} else {
		r.Get("/providers", h.perm("ai_provider", "read", h.ai.ListProviders))
		r.Get("/embedding/providers", h.perm("ai_provider", "read", h.ai.ListEmbeddingProviders))
		r.Post("/{provider}/chat", h.perm("ai_provider", "execute", h.ai.Chat))
	}

	// Usage dashboard. Gated by the ai_usage.read permission,
	// granted to sysadmin only for now (the object exists so
	// other roles can be granted via the admin role-assign
	// endpoint later). aiUsage is always wired (it only reads
	// the ai_usage table, no provider dependency), so the
	// dashboard works even when AI providers are not configured.
	if h.aiUsage != nil {
		r.Route("/usage", func(r chi.Router) {
			r.Get("/summary", h.perm("ai_usage", "read", h.aiUsage.Summary))
			r.Get("/by-day", h.perm("ai_usage", "read", h.aiUsage.ByDay))
			r.Get("/by-operation", h.perm("ai_usage", "read", h.aiUsage.ByOperation))
			r.Get("/by-repository", h.perm("ai_usage", "read", h.aiUsage.ByRepository))
			r.Get("/by-source", h.perm("ai_usage", "read", h.aiUsage.BySource))
		})
	}
}

// promptsetsRoutes registers the /api/v1/promptsets CRUD surface.
// Reads (List, Get) are authed-only — any logged-in user can see the
// built-in + their own custom promptsets (and sysadmins see all).
// Writes (Create, Update, Delete) are gated by the promptset.manage
// permission, granted to every authenticated user by default so
// creating a custom promptset is a user-scoped action, not an admin
// privilege. The handler enforces ownership on update/delete.
func (h *Handler) promptsetsRoutes(r chi.Router) {
	if h.promptsets == nil {
		r.Get("/", notConfigured)
		r.Get("/{hash}", notConfigured)
		r.Post("/", notConfigured)
		r.Put("/{hash}", notConfigured)
		r.Delete("/{hash}", notConfigured)
		return
	}
	r.Get("/", h.authed(h.promptsets.List))
	r.Get("/{hash}", h.authed(h.promptsets.Get))
	r.Post("/", h.perm("promptset", "manage", h.promptsets.Create))
	r.Put("/{hash}", h.perm("promptset", "manage", h.promptsets.Update))
	r.Delete("/{hash}", h.perm("promptset", "manage", h.promptsets.Delete))
}
