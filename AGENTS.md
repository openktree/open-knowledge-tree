# Agent Guide: Open Knowledge Tree

## Tech Stack

| Layer | Tech |
|-------|------|
| Backend | Go 1.22+, Chi router, pgx/v5, sqlc, Casbin, Viper |
| Frontend | SolidJS, Vite, Tailwind CSS, @solidjs/router |
| Data | PostgreSQL 16 |
| DevOps | Docker Compose, `just` runner |

## Key Patterns

**Backend**
- **sqlc**: Write SQL in `backend/db/queries/*.sql` → generated typesafe Go in `backend/internal/store`. Never hand-write store boilerplate.
- **Provider Strategy**: Search and resolution use strategy/adapter patterns (`internal/providers/`). Register new providers in `cmd/app/api.go`.
- **RBAC**: Casbin with a custom `pgx` adapter (`internal/rbac/adapter.go`). Policies are rows in PostgreSQL.
- **Config**: Layered Viper loading: `configs/config.default.yaml` → `configs/config.local.yaml` → `.env` overrides. The default is bundled into the binary (`backend/configs/embed.go`) and auto-written to `<binary_dir>/configs/config.default.yaml` on first run when no on-disk copy is found. Searched in `./configs`, `.`, the binary's directory, and `<binary-dir>/configs`; override the search with `--config <file|dir>`.
- **Schema**: migrations live in `backend/db/migrations/NNNN_*.up.sql` / `.down.sql`, embedded as `backend.MigrationsFS` and applied by golang-migrate at boot (see `backend/internal/dbpool/registry.go`).

**Frontend**
- **Stores**: Solid signals/stores in `frontend/src/store/` (auth, rbac, theme, repository).
- **API layer**: Thin fetch wrapper in `frontend/src/services/api.js` that injects `Authorization` and `X-Repository-ID` headers.
- **Routing**: `@solidjs/router` with page components in `frontend/src/pages/`.

## Folder Structure

```
open-knowledge-tree/
├── backend/
│   ├── cmd/app/                  # Entry point; wire deps, start server
│   ├── configs/                  # YAML configs (default + local overrides)
│   ├── db/
│   │   ├── migrations/           # golang-migrate files (NNNN_*.up.sql / .down.sql)
│   │   └── queries/              # sqlc query files (*.sql)
│   ├── e2e/                      # Go e2e tests (build tag `e2e`)
│   ├── internal/
│   │   ├── api/                  # HTTP layer (single namespace)
│   │   │   ├── wiring.go         # Composition root: Handler struct, NewHandler, Router, route groups
│   │   │   ├── handler/          # HTTP handlers grouped by domain
│   │   │   │   ├── handler.go    # Shared Deps bundle
│   │   │   │   ├── auth.go       # Register, Login, Logout, Refresh
│   │   │   │   ├── user.go       # GetMe, GetProfile, UpdateProfile, GetOwnPermissions
│   │   │   │   ├── admin.go      # ListUsers, AssignRole, RemoveRole, ListPermissions
│   │   │   │   ├── repository.go # Repository CRUD + GetMyPermissions
│   │   │   │   ├── oauth.go      # OAuth 2.1 authorize/token/register/revoke
│   │   │   │   ├── oauth_consent.go # server-rendered login + consent HTML
│   │   │   │   ├── mcp.go        # MCP server (mark3labs/mcp-go) + 16 tools (getRepositories, searchFacts, getFact, searchConcepts, getConcept, getConceptSummaries, getRelatedConcepts, getInvestigation, createInvestigation, searchSources, listSearchProviders, fetchAndProcessSource, getSourceTasks, createReport, getReport, listReports, getReportTasks)
│   │   │   │   └── source.go     # ListProviders, TestSearch, ClassifyResource
│   │   │   ├── middleware/       # HTTP middlewares (AuthRequired, RequirePermission, OAuthBearer)
│   │   │   └── httputil/         # Response helpers (WriteJSON/WriteError) and context keys
│   │   ├── auth/                 # JWT and crypto helpers (transport-agnostic)
│   │   ├── config/               # Config struct and Load()
│   │   ├── oauth/                # OAuth 2.1 authorization server (transport-agnostic)
│   │   │   ├── token.go          # JWT issue/verify, PKCE, opaque-token helpers
│   │   │   ├── types.go          # Config, TokenPair, ClientRegistrationResponse
│   │   │   └── server.go         # authorize/token/register/revoke logic
│   │   ├── providers/            # External integrations (transport-agnostic)
│   │   │   ├── search/           # Search providers (interface + concrete impls)
│   │   │   │   ├── search.go     # SearchResult, SearchProvider interface
│   │   │   │   ├── serper.go     # Serper (Google Search)
│   │   │   │   └── openalex.go   # OpenAlex (Academic Works)
│   │   │   └── fetch/            # Resolution providers + strategy
│   │   │       ├── resolution.go # ResolutionProvider interface, Resource, ResolvedContent
│   │   │       ├── fetch.go      # HTTP fetch implementation
│   │   │       └── strategy.go   # FetchStrategy (composes resolution providers)
│   │   ├── rbac/                 # Casbin service, adapter, seed
│   │   └── store/                # sqlc-generated code (DO NOT EDIT)
│   ├── docker-compose.yml        # postgres, api, frontend, test services
│   └── sqlc.yaml                 # sqlc codegen config
├── frontend/
│   ├── src/
│   │   ├── components/           # Reusable UI (Button, Card, Layout, etc.)
│   │   ├── pages/                # Route-level views
│   │   ├── services/             # API client module
│   │   └── store/                # Solid global state modules
│   └── package.json
├── .env                          # Secrets injected by docker-compose / Viper
└── justfile                      # Dev commands (`just dev`, `just test-e2e`)
```

**Rule of thumb for `internal/api/`**: every file in this tree is HTTP-specific. Domain packages (`auth`, `rbac`, `store`, `providers`, `config`) know nothing about HTTP and stay reusable for any future transport (CLI, worker, gRPC).

### Page folder convention

When a page exceeds the [Page Size Policy](#frontend-page-size-policy-mandatory) limits, convert it to a folder. The folder shape and the rules below are how every refactor must look:

```
frontend/src/pages/<name>/
├── index.jsx              # route entry (default export) — owns page-level state
├── <Name>Content.jsx      # main view, picks tabs / composes sub-pieces (optional)
├── <Name>Form.jsx         # subcomponents specific to this page
├── <Name>Table.jsx
├── <Name>Card.jsx
└── constants.js           # tab definitions, role/badge maps, etc.
```

Rules:
- **`index.jsx` is the only file other modules import** (the router resolves `import Sources from "./pages/Sources"` to `pages/Sources/index.jsx` automatically). Keep `*Content`, `*Form`, `*Table`, etc. private to the folder.
- **Naming**: `<PageName>Content` for the top-level view; then noun-based names for pieces (`UsersTable`, `AssignRoleForm`, `TestSearchPanel`, `AvailablePermissions`). The filename should describe what it renders.
- **Promote to `components/` only when something is reused across pages.** `ProviderList` was promoted to `components/ItemList.jsx` because the titled-card-with-list + render-prop slot pattern is generic. Pieces used by a single page stay inside the page folder.
- **State ownership**: the page-level state (fetched data, top-level alerts, refs) lives in `index.jsx`; subcomponents are controlled via props and call back via `onXxx` props. This keeps `index.jsx` small and subcomponents easy to test/reuse.

## Frontend Page Size Policy (Mandatory)

**Every page in `frontend/src/pages/` MUST stay within the size budget defined below, or be split into a folder per the *Page folder convention*.** This rule is enforced automatically — see *Enforcement* — and a change is not complete while a violation exists.

### Hard limits (any one of these is a violation)

| # | Trigger | Limit |
|---|---------|-------|
| 1 | Flat page file is longer than… | **150 lines** |
| 2 | Flat page file with internal subcomponents (functions returning JSX) is longer than… | **100 lines** |
| 3 | Flat page file imports from `../components/`… | **6 or more distinct components** |

Notes on what is and is not counted:
- **Internal subcomponents** (trigger 2) = any function or arrow-const in the same file that returns JSX, excluding the default export. The presence of internal subcomponents is the strongest signal that a page mixes concerns and should be split.
- **Reactive primitives** (`createSignal` / `createResource` / `createMemo`) are intentionally **not** counted. A single form legitimately has 4-5 of them; that is fine. Pain comes from breadth of UI composition, not local state count.
- **Page folders** (`pages/<name>/index.jsx` plus siblings) are not subject to these limits at the folder level; each file inside should still be reasonable (aim for <150 lines, no internal subcomponents). The split itself is the win.

### Escape hatch

In exceptional cases, a flat page may exceed the limits with a top-of-file justification:

```jsx
// @okt-page-allow-large: <one-line reason this page is allowed to stay flat>
import { createSignal } from "solid-js";
// ...
```

Rules for the escape hatch:
- The directive must be on the **first 5 lines** of the file.
- The directive must include a justification in the form `@okt-page-allow-large: <reason>` (also accepts `- ` or `— ` as separators).
- The page is still expected to be split eventually. Reference a tracking issue or milestone in the justification. Reviewers are expected to push back on unjustified uses.

### Enforcement

A zero-dependency Node script `frontend/scripts/check-page-size.mjs` walks `frontend/src/pages/`, applies the limits above, and exits non-zero on violations. Run it from the repo root:

```bash
just check-pages        # via justfile
# or
cd frontend && npm run check:pages
```

The `just check-frontend` recipe chains it with a production build and is the recommended gate before pushing:

```bash
just check-frontend     # check-pages + vite build
```

CI (or a pre-push hook) should run `just check-frontend`. A pull request that introduces a violating flat page will fail the gate.

### How to comply

1. Convert the page to a folder per the *Page folder convention* above (`pages/<name>/index.jsx` + subcomponents + `constants.js`).
2. Move page-level state into `index.jsx`; pass it to subcomponents as props / accessors.
3. Promote to `components/` only what is genuinely reused across pages.
4. Re-run `just check-pages` to confirm the gate is green.

## Testing Policy (Mandatory)

**Every new feature or change to an existing feature MUST update the corresponding e2e tests in `backend/e2e/`.** A change is not complete until tests reflect it.

Rules:

- **New endpoint or new behavior on an existing endpoint** → add or update a test in the matching e2e file (e.g. a new `POST /api/v1/auth/refresh` case goes in `e2e/auth_test.go`). Cover the happy path **and** at least one error case (invalid input, missing auth, wrong role, etc.) when applicable.
- **New domain or new handler bundle** → create a new `backend/e2e/<domain>_test.go` file with the `//go:build e2e` tag, and add it to the test suite. Follow the patterns in the existing e2e files.
- **New permission / role / RBAC behavior** → add a test that exercises both the allow and deny paths against a real session (see how `e2e/users_test.go` and the admin flows assert on status codes).
- **New provider** → add a test in `e2e/<provider>_test.go` that exercises the provider interface (the existing `serper_test.go` and `openalex_test.go` are the template). Tests should skip gracefully when the relevant API key env var is unset, not fail.
- **Changed response shape** (new field, renamed field, status code change) → update every test that asserts on the old shape. Do not leave stale assertions.
- **Refactors with no behavior change** (e.g. moving handlers between packages) → re-run the full e2e suite and confirm everything still passes; no new tests required, but no tests may be deleted.

Before marking work done, the author must be able to run:

```bash
just test-e2e
```

or, equivalently, point the e2e suite at the dedicated test Postgres (port 5433, `okt_test` password — never the dev DB on port 5432, the e2e reset drops all schemas):

```bash
cd backend && OKT_TEST_DATABASE_URL="postgres://okt:okt_test@localhost:5433/okt?sslmode=disable" \
  go test -count=1 -tags=e2e -timeout 180s -skip TestSerperSearchProvider_Search ./e2e/...
```

and see green. (The `-skip` is for the SERPER_API_KEY env-gated test only; remove it once the key is configured locally.)

**Never run the e2e suite against your dev database.** The test harness in `backend/e2e/testutil/setup.go` runs `DROP SCHEMA IF EXISTS okt_repository CASCADE; DROP SCHEMA IF EXISTS okt_system CASCADE; DROP SCHEMA public CASCADE; CREATE SCHEMA public;` before re-applying migrations, which deletes all users, sessions, facts, sources, and other application data. Always use the test Postgres (port 5433) or `just test-e2e`, which boots an isolated tmpfs test container.

## Where to Put New Artifacts

| Artifact | Location |
|----------|----------|
| New HTTP endpoint | `backend/internal/api/handler/<domain>.go` (method on a domain struct), then wire it in `backend/internal/api/wiring.go` (add a route inside the matching `<domain>Routes` method, optionally wrapped with `h.authed(...)` or `h.perm("resource", "action", ...)`) |
| New shared response / context helper | `backend/internal/api/httputil/` (response.go or context.go) |
| New HTTP middleware | `backend/internal/api/middleware/<name>.go` as a plain function, not a method on `*api.Handler` |
| New DB query | `backend/db/queries/<domain>.sql` → run `sqlc generate` → use `internal/store` |
| New DB table | A new `backend/db/migrations/NNNN_<name>.up.sql` (with matching `.down.sql`). DDL must be idempotent (`CREATE TABLE IF NOT EXISTS`, `CREATE INDEX IF NOT EXISTS`). The same file runs against every database declared in `databases`. |
| New external integration | `backend/internal/providers/search/<name>.go` (search) or `backend/internal/providers/fetch/<name>.go` (resolution) + register in `cmd/app/api.go` |
| New auth rule / policy | `backend/internal/rbac/` (model.conf or seed logic) |
| New config field | `backend/internal/config/config.go` + `configs/config.default.yaml`. For a new *database*, add it to the `databases:` map and reference it by name from `system.database`, `task.database`, or `isolation.allowed_databases`. |
| New background task | `backend/internal/taskmanager/tasks/<name>.go` (JobArgs + Worker), register the worker in `taskmanager.New`, then expose an `Enqueue*` helper on the `Manager` if the HTTP layer needs to insert jobs. Tasks share a River schema; the DB connection is read from the `*dbpool.Pool` passed to `taskmanager.New` (resolved by the wiring layer from `cfg.Task.Database`). |
| New frontend page | `frontend/src/pages/<Name>.jsx` while it fits the **Page Size Policy** (≤150 lines, ≤100 if it has internal subcomponents, ≤6 component imports); once it exceeds any limit, convert to `frontend/src/pages/<name>/` folder per the *Page folder convention* below + add route in `App.jsx` |
| New UI component | `frontend/src/components/<Name>.jsx` (generic, reusable across pages). Page-specific pieces (e.g. `UsersTable`, `AssignRoleForm`) live inside the page folder, not in `components/`. |
| New global state | `frontend/src/store/<domain>.js` or `.jsx` |
| New e2e test | `backend/e2e/<name>_test.go` with `//go:build e2e` |
| New OAuth 2.1 endpoint / MCP tool | Authorization logic: `backend/internal/oauth/` (transport-agnostic). HTTP layer: `backend/internal/api/handler/oauth.go` (authorize/token/register/revoke) + `oauth_consent.go` (server-rendered login + consent HTML). MCP server: `backend/internal/api/handler/mcp.go` (uses `github.com/mark3labs/mcp-go`). Bearer validation: `backend/internal/api/middleware/oauthbearer.go`. Wire in `cmd/app/api.go` (`oauth.NewServer` + `handler.NewOAuth` + `handler.NewMCP`) and `wiring.go` (`SetOAuth` / `SetMCP`, the `/api/v1/oauth/*` routes, the `POST /api/v1/mcp` route wrapped with `OAuthBearer`, and the two `/.well-known/oauth-*` documents at the router root). Access tokens are HS256 JWTs signed with `cfg.Auth.JWTSecret`; refresh tokens are opaque + hashed at rest in `okt_system.oauth_refresh_tokens`. |

### Multi-database layout

The backend supports multiple Postgres databases by name. A few rules to keep in mind when adding anything that touches a database:

- `databases.default` is always required. The legacy `database:` block is still parsed (synthesized into `databases.default` with a deprecation log) for one release.
- The same migration set runs against every database declared in `databases` (golang-migrate, driven by `backend.MigrationsFS`). Every database carries both schemas (`okt_system` for users/sessions/casbin_rule/repositories and `okt_repository` for per-repo data). Tier-1 (shared) databases store per-repo rows interleaved in `okt_repository` and filter by `repository_id`; tier-2/3 (isolated/sovereign) databases start as empty mirrors of the DDL and are populated by the tier-upgrade flow.
- All table names in `db/queries/*.sql` are unqualified; the connection's `search_path` (`okt_system, okt_repository, public`) is set by the dbpool registry's `AfterConnect` hook on every connection. **Don't `SET search_path` in DDL files**, it clobbers the registry's setting.
- A repository-scoped query that needs the per-repo pool reads `appmw.PoolFromContext(ctx)` (set by `appmw.WithRepoQueries`, registered in the `/{repoID}` route group in `wiring.go`) and builds a per-request `*store.Queries` with `store.New(pool)`. Handlers in the system-side routes use `Deps.Store` directly (the default pool).
- The picker for a new repository's database is gated on `rbac.CanPickRepositoryDatabase(uid)`: a sys admin (`*/*`) or any user with a system-scope `repositories.*.manage` policy. The server returns 400 when a permitted caller picks a name not in `cfg.Databases`; non-permitted callers are silently overridden to `cfg.Isolation.DefaultDatabase` (default `default`).

## Expanding the Project

1. **Add a domain** (e.g., `collections`):
   - `backend/db/migrations/NNNN_collections.up.sql`: add tables.
   - `backend/db/queries/collections.sql`: write queries.
   - Run `sqlc generate`.
   - `backend/internal/api/handler/collections.go`: define a `Collections` struct with a `NewCollections(deps Deps)` constructor and the HTTP methods on `*Collections`.
   - `backend/internal/api/wiring.go`: instantiate it in `NewHandler` (add a field, build it, store the pointer) and add a `collectionsRoutes` method that registers endpoints under `/api/v1/collections`.
   - Optional: `backend/e2e/collections_test.go`.
   - Frontend: add page + service method + store slice.

2. **Add a provider** (e.g., Scopus):
   - Add a new file in the matching folder:
     - For a **search** provider: `backend/internal/providers/search/<name>.go` in package `search` that implements `search.SearchProvider`.
     - For a **resolution** provider: `backend/internal/providers/fetch/<name>.go` in package `fetch` that implements `fetch.ResolutionProvider`.
   - Register it in `cmd/app/api.go` and pass it to `handler.NewSource(...)` (which is then attached to the api handler via `h.SetSource(...)`).

3. **Add a role/permission**:
   - Update `backend/internal/rbac/seed.go` to insert default policies.
   - Use `rbac.Enforce(...)` in a middleware (preferred) or directly in a handler.

4. **Add a middleware** (e.g., rate limiting):
   - Add `backend/internal/api/middleware/ratelimit.go` as a plain function `func RateLimit(...deps..., next http.HandlerFunc) http.HandlerFunc` (not a method on `*api.Handler` — that avoids import cycles with `handler/`).
   - Compose it in `wiring.go` where needed, alongside `appmw.AuthRequired` and `appmw.RequirePermission`.

## Common Commands

```bash
# Dev (hot-reload API + frontend)
just dev

# Run e2e tests
just test-e2e

# Regenerate store after query changes
cd backend && sqlc generate

# Build prod images
just up

# Pre-commit gate for frontend changes
just check-pages        # page-size policy only
just check-frontend     # page-size policy + production build
```
