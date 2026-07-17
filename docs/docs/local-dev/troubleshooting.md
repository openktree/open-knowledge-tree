---
id: troubleshooting
sidebar_position: 3
title: Troubleshooting
---

# Troubleshooting

## Dirty migration state

**Symptom:** The API fails to boot with `error: Dirty database version <N>. Reset and re-run.`

golang-migrate tracks applied migrations in a `schema_migrations` row. If a migration partially applied then crashed, the row is marked dirty.

**Fix:**

```bash
just reset-db
```

This runs `down -v` through the compose project (removing all named volumes including `pgdata`), then boots the dev profile. A bare `docker compose down -v` may leave stale volumes if the project name differs — `just reset-db` avoids that.

## Port conflicts

**Symptom:** `Bind for 0.0.0.0:5432 failed: port already allocated`

Check what's using the port:

```bash
sudo lsof -i :5432
```

Common conflicts: a local Postgres, another OKT stack, or a stale container. Either stop the conflicting service or change the port mapping in `docker-compose.yml`.

## Qdrant unreachable

**Symptom:** The API logs `Qdrant health check failed` and the embedding+dedup pipeline is disabled.

The API health-checks Qdrant at boot (`backend/cmd/app/api.go:430-470`). If unreachable, it boots without the pipeline — facts endpoints still serve but no dedup or embedding happens.

**Fix:** Ensure Qdrant is running:

```bash
docker compose -f backend/docker-compose.yml --profile dev ps qdrant
```

Restart it if needed:

```bash
docker compose -f backend/docker-compose.yml --profile dev restart qdrant
```

## PDF parsing failures

**Symptom:** Sources with PDFs show `parse_status='unsupported'`.

The `FZ_VERSION` env var must match the installed MuPDF library version. The docker-compose sets `FZ_VERSION: "1.26.11"` for Alpine 3.23's mupdf. If you're running locally without Docker, ensure your system MuPDF matches.

## LLM calls failing

**Symptom:** Fact extraction or synthesis jobs fail with HTTP errors.

Check that at least one LLM provider key is set in `.env` (`OPENROUTER_API_KEY` or `OLLAMA_API_KEY`). The provider is configured in `configs/config.default.yaml` and can be overridden in `configs/config.local.yaml`.

Check the AI usage logs:

```bash
just api-logs | grep -i "ai_usage\|llm\|error"
```

## FlareSolverr not bypassing

**Symptom:** Sources from Cloudflare-protected sites still fail.

Check FlareSolverr health:

```bash
curl http://localhost:8191/health
```

If it's unhealthy, restart it. Byparr's `/health` handler does synchronous work (~10s per call), so the first request may be slow.

## E2e tests fail with connection refused

**Symptom:** `just test-e2e` fails to connect to the test Postgres.

The test harness uses port 5433 (never 5432). Ensure no other service is using port 5433. The test Postgres boots on a tmpfs volume and is recreated every run.

## Frontend build fails

**Symptom:** `just check-frontend` fails on the page-size policy.

A page in `frontend/src/pages/` exceeded the size budget. See the [Page Size Policy](https://github.com/open-knowledge-tree/open-knowledge-tree-go/blob/main/AGENTS.md#frontend-page-size-policy-mandatory) in AGENTS.md. Fix by converting the page to a folder per the Page folder convention, or add an escape hatch with a justification.