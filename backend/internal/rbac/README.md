# RBAC

Casbin-based role-based access control. The package owns the
default policy set, the in-memory enforcement service, and
the pgx adapter that persists casbin_rule rows to Postgres.

## Model at a glance

* **System scope**: one role, `sysadmin`. Grants `*:*`, so
  any handler that consults the RBAC service lets the call
  through. Granted to one or more users via the
  admin endpoint (`PUT /api/v1/admin/users/roles` with
  `role=sysadmin`), at bootstrap from
  `OKT_BOOTSTRAP_DEFAULT_ADMIN_*` env vars
  (`bootstrap.EnsureDefaultAdmin`), or — by default, for a
  smooth first-boot experience — to the first user to
  register on an empty users table
  (`bootstrap.auto_promote_first_user`, see
  `internal/api/handler/auth.go`'s `Register` handler).

* **Repository scope**: four object-typed roles —
  `repoadmin`, `editor`, `viewer`, `curator`. None of
  them get `*:*`. The seed (`seed.go`) only establishes
  the templates; the actual per-repo policies are
  expanded by the `CreateRepository` handler when a
  repository is created and the creator becomes its
  first member.

* **Legacy roles**: `user`, `admin`, `viewer`. The
  existing test suite, the `auth.Register` flow, and the
  `ListRepositories` response all reference them. The
  legacy `user` role gets a permissive but bounded policy
  set (read/create/update on most resources; no
  destructive admin actions) so the previous behavior
  survives. The legacy `admin` role maps to sysadmin
  effectively (it gets `*:*`); we keep it so a deployment
  that has been granting `admin` to operators keeps
  working.

  `RoleUserLegacy = "user"` and `RoleAdminLegacy = "admin"`
  are aliases; the legacy `viewer` and the new `viewer`
  share the same string on purpose so a deployment that
  has been granting `viewer` keeps working.

## Ownership vs RBAC

Casbin is **coarse-grained**: "can this user touch this
repository?" The fine-grained check — "is this user the
*owner* of this repository?" — lives in
`internal/api/middleware/ownership.go` (added in phase 1
of the hardening plan, see *Hardening roadmap* below).
Both must pass: a `repoadmin` cannot delete a repository
they don't own, and only the owner (or `sysadmin`) can
delete a repository. Phase 1 of the roadmap wires the
middleware; before it lands, repo delete is gated by RBAC
alone and any `repoadmin` can delete.

## Two scopes, one engine

Casbin's matcher is a single string. The boundary
between system and repository scope is enforced by
**the route the request hits**, not by the matcher:

* System-scope routes (`/api/v1/repositories/...`,
  `/api/v1/users/...`, `/api/v1/admin/...`,
  `/api/v1/sources/...` outside the `/{repoID}` group)
  are checked with the bare object name (`repositories`,
  `user`, ...).

* Repository-scope routes (everything inside
  `/api/v1/repositories/{repoID}/...`) are checked with
  `<object>:<repoID>`. The `WithRepoQueries` middleware
  injects the per-repo pool; the `RequirePermission`
  middleware uses the route's `{repoID}` to build the
  scoped object.

The Casbin matcher itself is unchanged. See
`internal/api/wiring.go` and
`internal/api/middleware/{auth,rbac}.go` for the
boundary.

## Hardening roadmap

This is phase 1 of the role-model refactor (see the
plan discussed in chat: "Phase 1 — System + Repo
Permissions Hardening"). What's landed:

* Typed permission constants in `permissions.go`
  (Objects, Actions, Role*, Domain*).
* The new role model in `seed.go`.
* Bootstrap updated to grant `RoleSysAdmin` to the
  default admin.

What's NOT in phase 1 (intentional):

* **Owner-check middleware** (`RequireOwner`). When
  it lands, the repo-delete flow switches from RBAC to
  "owner OR sysadmin". Tracked in the plan.
* **Audit log** (`permission_audit` table,
  `GET /api/v1/admin/audit`). Required for compliance
  and for "who gave Bob editor on repo X last
  Tuesday?" queries. Not blocking; not landed.
* **Groups**. Implemented in `groups.go` /
  `group_manager.go`. The schema is live; the
  casbin `g` chain walks user → group → role
  transparently. The HTTP surface is mounted
  under `/api/v1/groups` (mutations require
  `sysadmin`) and `/api/v1/users/{id}/groups`
  (self-or-sysadmin for reads). See the
  `groups_test.go` e2e suite for the contract.
* **Source-level perms**. Out of scope. When the time
  comes, do NOT store per-source rows in Casbin; use
  source `visibility` + `owner_user_id` columns and a
  query filter, with Casbin for the coarse "can you
  touch this repo" check.

## Adding a new role

1. Add the role constant to `permissions.go` (`Role*`).
2. Add the role to `IsValidRole` in `permissions.go`.
3. Add the policy rows to `defaultPolicies()` in
   `seed.go` (if the role has repo-scope policies, mark
   them object-typed; do not use `*:*`).
4. Bump a migration or document that the change only
   takes effect on a fresh database (the seeder only
   runs when `casbin_rule` is empty).
5. Add tests in `backend/e2e/rbac_test.go` (created as
   part of this phase) covering allow + deny.

## Adding a new permission on an existing role

1. Add the action constant to `permissions.go` (`Actions.*`)
   if it's a new action kind.
2. Add the policy row to `defaultPolicies()` in `seed.go`.
3. Update the role-table docs in this file.
4. Add tests.
