---
id: auth
sidebar_position: 1
title: Auth API
---

# Auth API

## Session (JWT)

### Register

`POST /api/v1/auth/register`

Create a new user account.

**Body:** `{email, password, name?}`

**Returns:** `{user: {id, email, name}, token, refresh_token}`

---

### Login

`POST /api/v1/auth/login`

**Body:** `{email, password}`

**Returns:** `{user: {id, email, name}, token, refresh_token}`

---

### Logout

`POST /api/v1/auth/logout`

Invalidates the current session. Requires `Authorization: Bearer <token>`.

---

### Refresh

`POST /api/v1/auth/refresh`

Exchange a refresh token for a new access token.

**Body:** `{refresh_token}`

**Returns:** `{token, refresh_token}`

---

## User profile

### Get current user

`GET /api/v1/users/me`

Requires auth. Returns the authenticated user's profile.

---

### Get user profile

`GET /api/v1/users/{userID}`

Requires auth.

---

### Update profile

`PUT /api/v1/users/{userID}`

**Body:** `{name?, email?}`

---

### Get own permissions

`GET /api/v1/permissions`

Returns the authenticated user's permissions across all repositories and the system domain. Used by the frontend RBAC store.

---

## Admin (role management)

### List users

`GET /api/v1/admin/users`

Permission: `user:read`. Returns all users.

---

### Assign role

`PUT /api/v1/admin/users/roles`

Permission: `role:manage`.

**Body:** `{user_id, role, domain}`

---

### Remove role

`DELETE /api/v1/admin/users/roles`

Permission: `role:manage`.

**Body:** `{user_id, role, domain}`

---

### List permissions

`GET /api/v1/admin/permissions`

Permission: `role:read`. Returns all known permissions and their roles.

---

## OAuth 2.1 Endpoints

These are the endpoints an MCP client or programmatic agent uses to obtain access tokens. The server implements OAuth 2.1 with PKCE.

### Register client

`POST /api/v1/oauth/register`

Register an OAuth 2.1 client (typically a "public" client with PKCE). Returns the client credentials.

---

### Authorize

`GET /api/v1/oauth/authorize`
`POST /api/v1/oauth/authorize`

The authorization endpoint. The GET returns a server-rendered login + consent HTML page; the POST submits the authorization decision.

---

### Authorize login

`POST /api/v1/oauth/authorize/login`

Server-rendered login form submission for the OAuth flow.

---

### Token

`POST /api/v1/oauth/token`

Exchange an authorization code (with PKCE verification) for an access + refresh token. Access tokens are HS256 JWTs.

---

### Revoke

`POST /api/v1/oauth/revoke`

Revoke a refresh token. Refresh tokens are hashed at rest in `okt_system.oauth_refresh_tokens`.

---

## Well-known

### Authorization server metadata

`GET /.well-known/oauth-authorization-server`

Returns the OAuth 2.1 authorization server metadata (issuer, endpoints, supported scopes, etc.).

---

### Protected resource metadata

`GET /.well-known/oauth-protected-resource`

Returns the protected resource metadata.

---

## Bootstrap admin

There are two ways to get a sysadmin on a fresh install — pick one based on
whether the deployment is reachable from the public internet.

### First-user autopromotion (default, localhost dev)

`bootstrap.auto_promote_first_user` (default `true`) makes the **first**
successful `POST /api/v1/auth/register` on an empty users table grant the
sysadmin role on the `system` domain to that user. So:

1. `docker compose up`
2. Open [http://localhost:3000](http://localhost:3000) and register.
3. You're sysadmin. Done — no env vars, no scripts.

Bindable from `.env` via `OKT_BOOTSTRAP_AUTO_PROMOTE` (accepts
`true`/`false`/`1`/`0`). A log line is emitted when it fires so an
operator notices if it fires unexpectedly.

### Explicit admin from env vars (public deployments)

For a publicly-exposed deployment, set `OKT_BOOTSTRAP_AUTO_PROMOTE=false`
in `.env` and use the `default_admin` flag + the
`OKT_BOOTSTRAP_DEFAULT_ADMIN_*` env vars so an attacker cannot become
sysadmin by registering first:

```yaml
bootstrap:
  auto_promote_first_user: false
  default_admin: true
```

```bash
OKT_BOOTSTRAP_DEFAULT_ADMIN_EMAIL=admin@example.com
OKT_BOOTSTRAP_DEFAULT_ADMIN_PASSWORD=         # generate a strong one
OKT_BOOTSTRAP_DEFAULT_ADMIN_DISPLAY_NAME=Default Admin
```

The admin is seeded at boot (`bootstrap.EnsureDefaultAdmin`) when the
users table is empty. Once any user exists, the step is a no-op (the env
vars may be removed or rotated safely).

When both `auto_promote_first_user` and `default_admin` are enabled,
`default_admin` wins: it runs at boot before any `Register` call, so the
users table is non-empty by the time autopromote's `CountUsers() == 1`
guard would fire.

### Promoting an existing user (dev only)

To promote an already-registered user to system admin (sysadmin role on
the `system` domain, giving `*/*`):

```bash
just bootstrap-admin user@example.com
```

This is idempotent and restarts the **dev** API service so the in-memory
enforcer reloads. It targets the dev compose profile
(`backend/docker-compose.yml --profile dev`); there is no
production-stack equivalent short of `psql` surgery or having an existing
admin call `PUT /api/v1/admin/users/roles`. See [Operator guide](/docs/architecture/overview).