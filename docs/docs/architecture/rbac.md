---
id: rbac
sidebar_position: 2
title: RBAC (Casbin)
---

# RBAC (Casbin)

OKT uses [Casbin](https://casbin.org/) for role-based access control with a custom `pgx` adapter (`backend/internal/rbac/adapter.go`). Policies are rows in the `casbin_rule` table in the `okt_system` schema.

## Model

The Casbin model (`backend/internal/rbac/model.conf`) defines:
- Roles (`g`): user -> role mappings, scoped by domain.
- Permissions (`p`): role -> (resource, object, action) tuples.

A "domain" is either `system` (for system-wide policies) or a repository UUID (for per-repo policies).

## Seed policies

Default policies are seeded in `backend/internal/rbac/seed.go`:
- `sysadmin` role: `*/*` (all resources, all actions) on the `system` domain.
- Per-repo roles: `admin`, `editor`, `viewer` with scoped permissions.

## Permission enforcement

Two middleware functions enforce permissions:

- `RequirePermission(rbac, resource, action, next)` — checks the user's permissions against the system domain (for system-level routes).
- `RequireRepoPermission(rbac, resource, action, next)` — checks against the repository domain from the URL's `{repoID}`.

Both are composed with `AuthRequired` (which sets the user ID on the context) in the wiring layer:

```go
func (h *Handler) perm(resource, action string, next http.HandlerFunc) http.HandlerFunc {
    return appmw.AuthRequired(h.deps.Store, appmw.RequirePermission(h.deps.RBAC, resource, action, next))
}
```

## Common permissions

| Resource | Action | Who has it |
|----------|--------|------------|
| `repository` | `write` | Users with `repository:write` on the system domain (can create repos) |
| `repository` | `read` | Per-repo `viewer`+ |
| `repository` | `manage` | Per-repo `admin`; system-scope `repositories.*.manage` for the DB picker |
| `source` | `read` / `write` / `delete` | Per-repo `viewer` / `editor` / `admin` |
| `fact` | `read` | Per-repo `viewer`+ |
| `concept` | `read` | Per-repo `viewer`+ |
| `investigation` | `read` / `write` / `delete` | Per-repo `viewer` / `editor` / `admin` |
| `report` | `read` / `write` / `update` / `delete` | Per-repo `viewer` / `editor` / `editor` / `admin` |
| `task` | `read` / `cancel` / `manage` | System domain |
| `user` | `read` | System domain (`user:read`) |
| `role` | `read` / `manage` | System domain |

## Promoting a user to sysadmin

```bash
just bootstrap-admin user@example.com
```

This inserts the grouping row (sysadmin role on the `system` domain) and restarts the dev API service so the in-memory enforcer reloads. Idempotent.