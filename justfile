dev:
	docker compose -f backend/docker-compose.yml --env-file .env --profile dev stop
	docker compose -f backend/docker-compose.yml --env-file .env --profile dev up -d --build --force-recreate

# Build and run the Knowledge Registry with air hot-reload.
# The registry source lives in this repo at registry/.
# Registry listens on :8081, MinIO console on :9001.
# `just dev` must be running first (registry shares the dev network).
dev-registry:
	docker compose -f backend/docker-compose.yml --env-file .env --profile dev up -d --build --force-recreate minio registry-dev
	docker compose -f backend/docker-compose.yml --env-file .env logs -f registry-dev

# Boot just the Knowledge Registry + MinIO standalone.
# Most users don't need this — use the shared registry instance.
# Registry listens on :8081, MinIO console on :9001.
# The registry source lives in this repo at registry/.
knowledge-registry:
	docker compose -f backend/docker-compose.registry.yml up -d --build
	docker compose -f backend/docker-compose.registry.yml logs -f registry

# Wipe the dev Knowledge Registry to a truly empty state and restart
# it so it re-seeds its canonical context vocabulary from the embedded
# contexts.json. Drops both named volumes (registry_data = the SQLite
# metadata DB, minio_data = the S3 bucket contents) so sources,
# decompositions, fact_hashes, and the contexts table are all gone.
# The registry re-creates the contexts table + seeds the 88 DBpedia
# L3 labels on boot. Use this when you want a fresh registry you'll
# repopulate from your local OKT via contribute-all.
#
# The volume names are resolved dynamically via `docker compose config
# --volumes` + the project name so the recipe survives a project-name
# change (the previous hardcoded `okt-knowledge-tree-go_*` prefix was
# wrong — the project is named `backend` — so the rm silently failed
# and sources survived the "reset").
#
# The registry source lives in this repo at registry/.
reset-registry:
	#!/usr/bin/env bash
	set -euo pipefail
	# Resolve the project-prefixed volume names for the two volumes
	# we want to wipe. The compose project is named after the -f
	# directory (`backend`), so the running volumes are
	# `backend_registry_data` and `backend_minio_data`. We resolve
	# the project name dynamically so the recipe survives a rename.
	PROJECT=$(docker compose -f backend/docker-compose.yml --env-file .env --profile dev config | sed -n 's/^name: //p')
	# Stop + remove the containers so their volume references are
	# released (a stopped container still holds its volumes; only
	# removing the container frees them). MinIO is the registry's
	# S3 backend and is recreated by the `up` below, so removing it
	# here is safe.
	docker compose -f backend/docker-compose.yml --env-file .env --profile dev rm -f -s registry-dev minio
	for v in registry_data minio_data; do
		full="${PROJECT}_${v}"
		if docker volume inspect "$full" >/dev/null 2>&1; then
			echo "removing volume $full"
			docker volume rm -f "$full"
		else
			echo "volume $full not found; skipping"
		fi
	done
	docker compose -f backend/docker-compose.yml --env-file .env --profile dev up -d --build --force-recreate minio registry-dev
	echo "Registry wiped + restarted. It will seed the canonical contexts on boot."
	echo "Check the boot log: just registry-logs"

# Production stack from PRE-BUILT public images (the one-liner).
# Uses the root docker-compose.yml, which pulls
# ghcr.io/openktree/api and ghcr.io/openktree/frontend. No source
# build, no -f flag, no --profile. Requires a .env with at least
# SERPER_API_KEY and one chat-model key (OPENROUTER or OLLAMA).
#   cp .env.example .env   # then edit
#   just up
up:
	docker compose up -d

# Stop the pre-built production stack.
down:
	docker compose down --remove-orphans

# Stop ALL profiles / compose files (pre-built stack + dev + test).
down-all:
	docker compose down --remove-orphans
	docker compose -f backend/docker-compose.yml --profile dev --profile prod --profile test down --remove-orphans

# Production stack built FROM SOURCE (the old `up`).
# Uses backend/docker-compose.yml --profile prod, which builds the
# api + frontend images locally. Use this when you're modifying
# backend or frontend code and want to test the prod build path
# without pushing images. For everyday dev, use `just dev`.
up-dev-source:
	docker compose -f backend/docker-compose.yml --env-file .env --profile prod stop
	docker compose -f backend/docker-compose.yml --env-file .env --profile prod up -d --build --force-recreate

# Wipe the dev databases and restart the dev stack on a truly empty
# state. Unlike a bare `docker compose down -v` (whose `-v` only
# removes volumes belonging to the *current* compose project name,
# so a stale volume from a different project name survives and the
# new container reattaches to a half-migrated DB), this recipe runs
# `down -v` through the compose file/project the stack actually
# uses, so every named volume (pgdata, pgdata_tasks, qdrant_data,
# source_assets) is removed and recreated empty, then boots the dev
# profile. Use this instead of `down -v` when golang-migrate
# complains about a dirty schema_migrations row.
reset-db:
	docker compose -f backend/docker-compose.yml --env-file .env --profile dev --profile prod --profile test down -v --remove-orphans
	docker compose -f backend/docker-compose.yml --env-file .env --profile dev up -d --build --force-recreate

# Docusaurus dev server with hot reload (localhost:3001).
# Installs deps on first run, then starts the dev server.
docs:
	cd docs && npm install && npm run start -- --port 3001 --host 0.0.0.0

# Production build of the docs site.
docs-build:
	cd docs && npm install && npm run build

# Serve the built docs site via the docs container (localhost:3002).
docs-serve:
	docker compose -f backend/docker-compose.yml --profile docs up -d --build docs

# Stop the docs container.
docs-down:
	docker compose -f backend/docker-compose.yml --profile docs down

test-e2e:
	docker compose -f backend/docker-compose.yml --profile test up -d test-postgres
	until docker compose -f backend/docker-compose.yml --profile test exec -T test-postgres pg_isready -U okt -d okt; do sleep 0.5; done
	cd backend && set -a && . ../.env && set +a && go test -tags=e2e -count=1 -timeout=300s ./e2e/...
	docker compose -f backend/docker-compose.yml --profile test down

# Wipe all per-repo data (sources, facts, concepts, candidates,
# summaries, syntheses, skips) from the dev Postgres + Qdrant for a
# single repository, leaving the repository row + its settings intact.
# Used in the concept-regeneration runbook: after wiping, set
# pull_level=facts and POST .../settings/pull-all to repopulate
# sources + facts, then the concept pipeline rebuilds from scratch.
#
# Accepts a repo UUID or slug. Connects to the dev DB (port 5432)
# and dev Qdrant (port 6334) via the same env vars the API uses.
# Refuses to run against the e2e test DB (port 5433).
#
#   just reset-repo my-repo-slug
#   just reset-repo 00000000-0000-0000-0000-000000000000
reset-repo ident:
	cd backend && set -a && . ../.env && set +a && go run ./scripts/reset-repo {{ident}}

# Enforces the Frontend Page Size Policy in AGENTS.md.
# Fails (non-zero) when any flat page in frontend/src/pages/ exceeds the size budget.
# Run from the repo root.
check-pages:
	cd frontend && npm run check:pages

# Pre-commit gate: page size policy + frontend production build.
check-frontend: check-pages
	cd frontend && npm run build

api-logs:
	docker logs okt-api-dev

frontend-logs:
	docker logs okt-frontend-dev

registry-logs:
	docker logs okt-registry-dev

# Lazy Shortcuts
al: api-logs
fl: frontend-logs

# Promote a user to system admin (sysadmin role on the `system`
# domain, which gives `*/*` via the seed policy). Idempotent:
# re-running is a no-op. Looks the user up by email, inserts the
# grouping row, and restarts okt-api-dev so the in-memory
# enforcer reloads.
#
#   just bootstrap-admin carlosgomezsoza@gmail.com
# Promote a user to system admin (sysadmin role on the `system`
# domain, which gives `*/*` via the seed policy). Idempotent:
# re-running is a no-op. Looks the user up by email, inserts the
# grouping row, and restarts okt-api-dev so the in-memory
# enforcer reloads.
#
#   just bootstrap-admin carlosgomezsoza@gmail.com
bootstrap-admin email:
	backend/scripts/bootstrap-admin.sh {{email}}