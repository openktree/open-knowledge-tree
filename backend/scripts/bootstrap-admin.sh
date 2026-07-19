#!/usr/bin/env bash
# Promote a user to system admin by inserting a `g, <uid>, sysadmin, system`
# casbin grouping policy and restarting the API so the in-memory enforcer
# reloads. Idempotent.
#
# Targets the dev stack (backend/docker-compose.yml --profile dev). All
# docker interactions go through `docker compose … exec|restart` by
# SERVICE name, which resolves to whatever container Compose spun up
# under the current project name (default `backend`, override with
# COMPOSE_PROJECT_NAME if you run a parallel stack). This keeps the
# script working after the container_name: lines were removed from the
# compose files.
set -euo pipefail

if [ $# -ne 1 ]; then
  echo "usage: bootstrap-admin <email>" >&2
  exit 2
fi

email="$1"

COMPOSE="docker compose -f backend/docker-compose.yml --env-file .env --profile dev"

USER_ID=$($COMPOSE exec -T postgres psql -U okt -d okt -At -c "SELECT id FROM okt_system.users WHERE email='${email}';")
if [ -z "${USER_ID}" ]; then
  echo "error: no user with email '${email}'" >&2
  exit 1
fi

echo "Promoting ${USER_ID} (${email}) to sysadmin"
$COMPOSE exec -T postgres psql -U okt -d okt -c \
  "INSERT INTO okt_system.casbin_rule (p_type, v0, v1, v2, v3, v4, v5) VALUES ('g', '${USER_ID}', 'sysadmin', 'system', '', '', '') ON CONFLICT DO NOTHING;"

$COMPOSE restart api-dev >/dev/null
echo "ok. api restarted; '${email}' is now sysadmin."