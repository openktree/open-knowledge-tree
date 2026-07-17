---
id: repositories
sidebar_position: 2
title: Repositories API
---

# Repositories API

Repositories are the top-level isolation boundary. Each repository has its own set of sources, facts, concepts, and syntheses. Access is controlled via per-repo roles.

## List repositories

`GET /api/v1/repositories/`

Requires auth. Returns repositories the authenticated user can see (owned + shared).

---

## Create repository

`POST /api/v1/repositories/`

Permission: `repository:write`.

**Body:** `{name, slug?, description?, database?, tier?}`

The `database` field picks which database (from `cfg.Databases`) the repo lives in. The picker is gated on `rbac.CanPickRepositoryDatabase(uid)`: a sys admin or any user with `repositories.*.manage` can pick a non-default database; non-permitted callers are silently overridden to `cfg.Isolation.DefaultDatabase`. See [Architecture > Multi-database](/docs/architecture/multi-database).

---

## List presets

`GET /api/v1/repositories/presets`

Returns available repository presets (preconfigured settings bundles).

---

## Get repository

`GET /api/v1/repositories/{repoID}`

Permission: `repository:read`.

---

## Update repository

`PUT /api/v1/repositories/{repoID}`

Permission: `repository:update`.

**Body:** `{name?, description?}`

---

## Delete repository

`DELETE /api/v1/repositories/{repoID}`

Permission: `repository:delete`.

---

## Get my permissions

`GET /api/v1/repositories/{repoID}/permissions`

Returns the authenticated user's permissions on this repository. Requires auth (no specific permission check — any authenticated user can see their own permissions).

---

## Repository settings

### Get settings

`GET /api/v1/repositories/{repoID}/settings`

Permission: `repository:manage`. Returns per-repo settings: enabled providers, custom contexts, auto-contribute, registry sync.

---

### Set provider enabled

`PUT /api/v1/repositories/{repoID}/settings/providers`

Permission: `repository:manage`.

**Body:** `{provider, enabled}`

---

### Add context

`POST /api/v1/repositories/{repoID}/settings/contexts`

Permission: `repository:manage`.

**Body:** `{label, description?}`

Adds a custom context to the per-repo ontology used by concept extraction. See [Concept & Alias Extraction](/docs/reference/knowledge-flow/5-concept-alias-extraction).

---

### Update context

`PUT /api/v1/repositories/{repoID}/settings/contexts/{context}`

Permission: `repository:manage`.

---

### Migrate context

`POST /api/v1/repositories/{repoID}/settings/contexts/{context}/migrate`

Permission: `repository:manage`. Merges one context's semantics into another (relinks facts/concepts).

---

### Delete context

`DELETE /api/v1/repositories/{repoID}/settings/contexts/{context}`

Permission: `repository:manage`.

---

### Contribute all to registry

`POST /api/v1/repositories/{repoID}/settings/contribute-all`

Permission: `repository:manage`. Pushes all source decompositions to the optional knowledge registry.

---

### Pull all from registry

`POST /api/v1/repositories/{repoID}/settings/pull-all`

Permission: `repository:manage`. Imports pre-computed decompositions from the registry.

---

### Set auto-contribute

`PUT /api/v1/repositories/{repoID}/settings/auto-contribute`

Permission: `repository:manage`.

**Body:** `{enabled}`