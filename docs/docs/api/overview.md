---
id: overview
sidebar_position: 0
title: REST API Overview
---

# REST API Overview

The OKT REST API is mounted at `/api/v1` and uses the Chi router. All routes are registered in `backend/internal/api/wiring.go`.

## Base URL

```
http://localhost:8080/api/v1
```

## Authentication

Two auth mechanisms are supported:

### JWT (session)

For browser/frontend sessions. Obtain a token via `POST /auth/login`, then pass it as `Authorization: Bearer <token>` on every request. Refresh via `POST /auth/refresh`.

See [Auth](/docs/api/auth) for the session endpoints.

### OAuth 2.1 (MCP / programmatic)

For agents and programmatic clients. The server implements OAuth 2.1 with PKCE. Access tokens are HS256 JWTs signed with `cfg.Auth.JWTSecret`; refresh tokens are opaque + hashed at rest in `okt_system.oauth_refresh_tokens`.

Register a client via `POST /oauth/register`, then follow the authorize/token flow. See [Auth > OAuth](/docs/api/auth#oauth-21-endpoints).

## Repository scoping

Repository-scoped routes live under `/{repoID}` and require the `X-Repository-ID` header (or the URL's `repoID` path param). A repo-scoped query reads the per-repo pool from `appmw.PoolFromContext(ctx)` (set by `appmw.WithRepoQueries`, registered in the `/{repoID}` route group). System-side routes use `Deps.Store` directly (the default pool).

## Authorization

RBAC is enforced via Casbin. Most routes are wrapped with `h.perm(resource, action, handler)` or `h.repoPerm(resource, action, handler)`, which run `AuthRequired` then `RequirePermission`. A user's permissions come from their roles (group assignments) in the `system` and `repository` domains. See [Architecture > RBAC](/docs/architecture/rbac).

## Response format

All responses are JSON. Errors use `{"error": "message"}`. Success responses return the resource object or an array directly (no envelope).

## Rate limiting

Not currently implemented.

## Common headers

| Header | Required | Description |
|--------|----------|-------------|
| `Authorization` | yes (most routes) | `Bearer <jwt>` or OAuth access token |
| `X-Repository-ID` | system routes | Repository UUID or slug (for routes not under `/{repoID}`) |
| `Content-Type` | POST/PUT | `application/json` (or `multipart/form-data` for uploads) |