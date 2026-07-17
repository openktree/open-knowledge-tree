-- 0001_init.up.sql
--
-- Foundation migration: install the uuid generator, create the two
-- application schemas (okt_system and okt_repository), and create
-- the system tables that every database in the cluster needs.
--
-- This migration runs against every database declared in
-- `cfg.Databases`. Every database carries both schemas so the
-- per-tenant tier-2/3 databases can hold a mirrored copy of
-- their own repositories registry row.
--
-- Tables are unqualified; the connection's `search_path` (set by
-- the dbpool registry on every new connection) resolves them
-- into okt_system. We don't `SET search_path` from inside this
-- file because that would mutate the connection's session state
-- in a way that affects queries after the DDL batch returns.

CREATE EXTENSION IF NOT EXISTS "uuid-ossp";

CREATE SCHEMA IF NOT EXISTS okt_system;
CREATE SCHEMA IF NOT EXISTS okt_repository;

CREATE TABLE IF NOT EXISTS users (
    id            UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    email         TEXT NOT NULL UNIQUE,
    password_hash TEXT NOT NULL,
    display_name  TEXT NOT NULL DEFAULT '',
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS sessions (
    id         UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    user_id    UUID NOT NULL REFERENCES okt_system.users(id) ON DELETE CASCADE,
    token_hash TEXT NOT NULL UNIQUE,
    expires_at TIMESTAMPTZ NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS casbin_rule (
    id     SERIAL PRIMARY KEY,
    p_type TEXT NOT NULL,
    v0     TEXT NOT NULL DEFAULT '',
    v1     TEXT NOT NULL DEFAULT '',
    v2     TEXT NOT NULL DEFAULT '',
    v3     TEXT NOT NULL DEFAULT '',
    v4     TEXT NOT NULL DEFAULT '',
    v5     TEXT NOT NULL DEFAULT ''
);
