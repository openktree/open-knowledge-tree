---
id: multi-database
sidebar_position: 1
title: Multi-Database Layout
---

# Multi-Database Layout

OKT supports multiple Postgres databases by name. The default posture is "one database, two schemas, all repositories in `default`" — splitting into more databases is a config-only change.

## The system/repo schema split

Users, auth, and admin data live with the repositories registry in the same database. The system/repo distinction is logical (a Postgres schema), not physical:

```sql
-- databases.default (Postgres database "okt")
CREATE SCHEMA IF NOT EXISTS okt_system;     -- users, sessions, casbin_rule, repositories
CREATE SCHEMA IF NOT EXISTS okt_repository; -- sources, facts, concepts, syntheses
```

The sqlc queries are unqualified (`SELECT * FROM users`). A `search_path` is set on each pool's `AfterConnect` hook:

| Pool role | `search_path` |
|-----------|---------------|
| System-only | `okt_system, public` |
| System + repository | `okt_system, okt_repository, public` |
| Repository-only | `okt_repository, public` |

This keeps the sqlc-generated queries untouched. The schema is a connection-time concern, not a query-time one.

## The databases map

A `map[string]DatabaseConfig` keyed by logical name. `default` is always required.

```yaml
databases:
  default:                  # always required
    host: localhost
    port: 5432
    user: okt
    password: okt_dev
    name: okt
    ssl_mode: disable
    max_conns: 20
  tasks:                    # optional — River task queue
    host: localhost
    name: okt_tasks
    max_conns: 10
  repo_eu:                  # optional, used as a per-repository DB
    host: db-eu.internal
    name: okt_repo_eu
    max_conns: 50

task:
  database: tasks          # empty falls back to "default"

system:
  database: default        # empty falls back to "default"

isolation:
  default_database: default  # where new repos land when the picker is skipped
  allowed_databases:          # what the picker shows to a permitted user
    - default
    - repo_eu
    - repo_us
```

## Pool registry

The `internal/dbpool` package owns a `Registry` that builds a `*pgxpool.Pool` per declared database at startup, sets the appropriate `search_path` on each pool's `AfterConnect` hook, pings each, and exposes `Get(name) *pgxpool.Pool` plus `Default()`.

Why a registry (not lazy get-or-create):
- Fail fast at boot — if `repo_us` is unreachable, the operator sees it immediately.
- Pool sizing is a boot-time concern.
- River's pool is special (it owns the pool and runs migrations through it), so the registry is the natural seam where River plugs in.

The registry is wired once in `cmd/app/api.go` and threaded through `Deps`.

## Per-repository DB allocation

The `repositories` table has a `database_name TEXT NOT NULL DEFAULT 'default'` column. It's a logical name that must match a key in `isolation.allowed_databases`.

- `CreateRepository` accepts an optional `database_name` field.
- The handler validates the name against the live allow-list; otherwise 400.
- If empty, falls back to `isolation.default_database`.
- The runtime reads the row's `database_name`, fetches the pool from the registry, and routes all repo-scoped queries to that pool.

## RBAC: who can pick a non-default DB?

The picker is gated by either:
- `*/*` (system admin), or
- A system-scope `repositories.*.manage` policy.

```go
canPickDB, _ := r.deps.RBAC.CanPickRepositoryDatabase(uid)
if !canPickDB {
    if body.DatabaseName != "" && body.DatabaseName != defaultDB {
        body.DatabaseName = defaultDB  // silently override
    }
}
```

Non-permitted callers are silently overridden to the default database. Permitted callers get a 400 when they pick a name not in `cfg.Databases`.

## DDL

The same migration set runs against every declared database (golang-migrate, driven by `backend.MigrationsFS`). Every database carries both schemas (`okt_system` + `okt_repository`). Tier-1 (shared) databases store per-repo rows interleaved in `okt_repository` and filter by `repository_id`; tier-2/3 (isolated/sovereign) databases start as empty mirrors of the DDL.

All table names in `db/queries/*.sql` are unqualified — the connection's `search_path` (`okt_system, okt_repository, public`) is set by the dbpool registry's `AfterConnect` hook on every connection. Don't `SET search_path` in DDL files — it clobbers the registry's setting.

## Backward compatibility

- Legacy `database:` block is still parsed (synthesized into `databases.default` with a deprecation log).
- Legacy env vars (`DATABASE_HOST`, `DATABASE_PORT`, ...) keep working as aliases for `databases.default.*`.
- `repositories.database_name` defaults to `'default'`, so old API clients creating repos still land in the default pool.