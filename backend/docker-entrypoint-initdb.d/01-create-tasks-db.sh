#!/bin/bash
# docker-entrypoint-initdb.d/01-create-tasks-db.sh
#
# Create a second database on the same Postgres instance
# for the River task manager. The application reads
# `TASK_NAME` (or `databases.tasks.name`) to decide which
# database to use; when the env var is unset it falls back
# to `default` (the database `POSTGRES_DB` named), so local
# dev that doesn't bring up a second database still works.
# Production docker-compose sets TASK_NAME=okt_tasks
# explicitly and the config layer opens a second pool
# against this database.
#
# Postgres's entrypoint runs every `*.sh` and `*.sql` file
# in this directory once, on first boot of an empty data
# directory. Subsequent boots are no-ops because the
# database already exists. The `psql -tAc` query makes the
# script idempotent: it only issues CREATE DATABASE when
# the row is missing, so mounting this file against the
# test-postgres container (which uses tmpfs and is
# recreated on every `just test-e2e` run) is safe.
#
# The script is a shell wrapper rather than a raw .sql
# file because CREATE DATABASE cannot run inside a
# transaction block; the entrypoint would otherwise wrap
# the .sql file in BEGIN/COMMIT and the CREATE would be
# rejected with "CREATE DATABASE cannot run inside a
# transaction block".
#
# We connect with -U "$POSTGRES_USER" because the default
# peer auth in the entrypoint connects as the OS user
# `postgres`, but the alpine image's POSTGRES_USER (which
# we set in docker-compose to `okt`) is what owns the
# databases. Falling through to `psql` with no flags
# triggers the FATAL "role postgres does not exist"
# error.

set -e

# Connect to the instance's default database ($POSTGRES_DB,
# set per-service in docker-compose) so the script works
# whether it runs on the primary instance (POSTGRES_DB=okt)
# or on the dedicated tasks instance (POSTGRES_DB=okt_tasks,
# where the `okt` database does not exist). Without `-d`,
# psql defaults to a database named after the user, which
# fails on the tasks instance.
PSQL_DB="${POSTGRES_DB:-okt}"

DB_EXISTS=$(psql -U "$POSTGRES_USER" -d "$PSQL_DB" -tAc "SELECT 1 FROM pg_database WHERE datname = 'okt_tasks'")
if [ "$DB_EXISTS" != "1" ]; then
    echo "initdb: creating okt_tasks database"
    psql -U "$POSTGRES_USER" -d "$PSQL_DB" -c "CREATE DATABASE okt_tasks"
fi
