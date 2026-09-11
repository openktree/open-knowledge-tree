# Security Policy

## Supported versions

We support the **latest release** of each service. Each service in the
monorepo is versioned and released independently (see the
[Releases section](./README.md#releases)); older versions do not receive
security patches. Before reporting, please confirm you can reproduce the
issue on the latest tag of the affected service.

| Service | Package | Supported |
|---------|---------------------|---------------------------------------|
| API | `ghcr.io/openktree/api` | latest release only |
| Registry | `ghcr.io/openktree/registry` | latest release only |
| Frontend | `ghcr.io/openktree/frontend` | latest release only |

## Reporting a vulnerability

Please report vulnerabilities through **GitHub private vulnerability
reporting**:

**<https://github.com/openktree/open-knowledge-tree/security/advisories/new>**

Do not open a public issue for security reports.

Reports are triaged promptly, and we are happy to credit you in the advisory
if you'd like — say so in your report. Please keep details private until a fix
is released.

## Security-relevant design notes

Context that may help when assessing a report:

- **Sessions & OAuth 2.1** — user sessions are JWTs; the OAuth 2.1
  authorization server uses PKCE for public clients, and refresh tokens are
  opaque and **hashed at rest**. Access tokens are HS256 JWTs signed with the
  configured JWT secret.
- **RBAC** — authorization is enforced by Casbin backed by PostgreSQL
  policies (system-scope and repository-scope permissions).
- **SSRF protection** — the fetch strategy (external resource resolution)
  includes protections against fetching internal/private network targets.
- **API keys** — provider API keys are **hashed at rest** in the database.
- **First-user bootstrap** — the first account to register on a fresh stack
  is automatically promoted to system admin, controlled by
  `bootstrap.auto_promote_first_user` (**enabled by default** for localhost
  bootstrap). If you expose OKT publicly, set it to `false` and use a strong,
  unique `JWTSecret`.
