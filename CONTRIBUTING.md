# Contributing to Open Knowledge Tree

Thanks for helping grow the tree. This file is deliberately thin — the full
development conventions live in [`AGENTS.md`](./AGENTS.md). Read it before
your first PR; it covers folder structure, naming, artifact placement, the
frontend page-size policy, and the mandatory testing policy.

## Quick start for contributors

Prerequisites:

- **Docker with Compose v2**
- **[just](https://github.com/casey/just)** — command runner for all dev workflows
- **Go 1.26+** — backend + registry builds and tests
- **Node 18+** — frontend and docs site

```bash
just dev        # hot-reload API + frontend (compose dev profile)
just test-e2e   # Go e2e suite
```

`just test-e2e` boots an **isolated tmpfs test Postgres on :5433** and runs
migrations from scratch — it never touches your dev database on :5432. Do not
run the raw e2e suite against your dev DB; the harness drops all schemas
before re-applying migrations.

## The gates

Every PR is gated by the `ci` workflow (`.github/workflows/ci.yml`). Path
filtering skips jobs whose files weren't touched.

| Job | Triggered by changes in | What it runs |
|-----|-------------------------|--------------|
| `lint` | `backend/**`, `registry/**` | golangci-lint on both Go modules |
| `frontend-lint` | `frontend/**` | Biome check |
| `frontend-build` | `frontend/**` | page-size policy check + production build |
| `backend-unit` | `backend/**` | `go build` + `go vet` + `go test` on `internal/` and `cmd/` |
| `backend-e2e` | `backend/**` | e2e suite against an isolated tmpfs Postgres |
| `registry` | `registry/**` | `go build` + `go vet` + `go test ./...` |
| `sqlc-diff` | `backend/**` | `sqlc generate` then `git diff --exit-code internal/store/` |
| `ai-plugins` | `ai-plugins/**`, workflows | plugin sync drift gate + generator/manifest/runtime tests |

## Local hooks

Run `just lefthook-install` once after cloning. The same gates then run
locally so you never push something CI will reject:

- **pre-commit** (parallel): `just check-pages`, `just check-plugins`,
  `go vet` on staged Go files, `npx biome check` on staged frontend files.
- **pre-push** (parallel): `just check-frontend` (page-size + vite build),
  `go build` in both Go modules.

Bypass with `--no-verify` sparingly — CI still catches what you skipped.

## Commit message scopes

We use [Conventional Commits](https://www.conventionalcommits.org/) with
per-service scopes. Each service (`api`, `registry`, `frontend`, `docs`) has
an independent SemVer; `feat`/`fix` commits trigger release-please release PRs.

```
feat(api): add MCP tool for X
fix(registry): correct context seeding order
feat(frontend): add sources filter
docs(docs): clarify deployment guide
```

Other scopes (`chore`, `ci`, `test`) don't trigger releases. See the
[Releases section](./README.md#releases) in the README for the full flow and
tag format.

## Where to put changes

Follow [`AGENTS.md`](./AGENTS.md):

- **"Where to Put New Artifacts"** — the table mapping each artifact type
  (endpoint, DB query, migration, provider, page, component, e2e test, …) to
  its location.
- **Testing Policy (Mandatory)** — every feature or behavior change must
  update the corresponding e2e tests in `backend/e2e/` before the work is
  done. Run `just test-e2e` and keep it green.

## Issues

- **Bug reports and feature requests**: open a
  [GitHub issue](https://github.com/openktree/open-knowledge-tree/issues/new/choose)
  using the templates.
- **Security vulnerabilities**: do **not** open an issue. See
  [`SECURITY.md`](./SECURITY.md) for the private reporting channel.
