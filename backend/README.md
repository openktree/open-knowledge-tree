# Open Knowledge Tree — Backend

Go API server for the Open Knowledge Tree platform.

## Prerequisites

- Go 1.24+
- PostgreSQL (default: `localhost:5432`, database `okt`)
- [Serper API key](https://serper.dev) (for search provider)

## Quick Start

```bash
# edit configs/config.local.yaml as needed (already exists)
export PROVIDERS_SEARCH_SERPER_API_KEY=your-serper-key
go run ./cmd/app --mode=api
```

## Configuration

Configuration is loaded from `configs/config.default.yaml`, merged with `configs/config.local.yaml`, and overridable via environment variables. Dots in keys are replaced with underscores.

| Config path | Env var | Default |
|---|---|---|
| `server.port` | `SERVER_PORT` | `8080` |
| `database.host` | `DATABASE_HOST` | `localhost` |
| `providers.search.provider` | `PROVIDERS_SEARCH_PROVIDER` | `serper` |
| `providers.search.serper.api_key` | `PROVIDERS_SEARCH_SERPER_API_KEY` | — |

## Running Tests

```bash
# Unit tests (no external dependencies)
go test ./...

# E2E tests (requires a live Serper API key)
export SERPER_API_KEY=your-serper-key
go test -tags e2e -v ./e2e/
```

E2E tests are gated behind the `e2e` build tag and live in the `e2e/` package. They are excluded from `go test ./...` so CI and local unit-test runs stay fast.
