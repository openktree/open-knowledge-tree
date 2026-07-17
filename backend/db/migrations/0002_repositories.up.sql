-- 0002_repositories.up.sql
--
-- The repositories registry: one row per repository in the
-- system. The `database_name` column points at the cfg.Databases
-- key the repository's data lives in (default for the shared
-- tier, a per-tenant name for the isolated/sovereign tier). The
-- `tier` column records the same fact in human-readable form
-- ('shared', 'isolated', 'sovereign') and is the column the
-- picker UI uses to render a default-warning callout.
--
-- Lives in okt_system on the system database (the database
-- named by `system.database` in config). On per-tenant
-- databases, the row is mirrored — a single repo's row is
-- duplicated there so the per-tenant DB can resolve its own
-- metadata without crossing the network.

CREATE TABLE IF NOT EXISTS repositories (
    id            UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    name          TEXT NOT NULL,
    slug          TEXT NOT NULL UNIQUE,
    description   TEXT NOT NULL DEFAULT '',
    owner_id      UUID NOT NULL REFERENCES okt_system.users(id) ON DELETE CASCADE,
    database_name TEXT NOT NULL DEFAULT 'default',
    -- tier: 'shared' (rows live alongside the system tables in
    -- the default database), 'isolated' (rows live in a
    -- dedicated database on a shared cluster, typically for
    -- paying customers who need IO isolation), 'sovereign'
    -- (dedicated cluster, optional region pinning, custom
    -- encryption). The column is informational; routing
    -- decisions read `database_name`, not `tier`.
    tier          TEXT NOT NULL DEFAULT 'shared'
        CHECK (tier IN ('shared', 'isolated', 'sovereign')),
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);
