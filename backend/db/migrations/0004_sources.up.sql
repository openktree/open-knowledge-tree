-- 0004_sources.up.sql
--
-- Per-repository data example: a "source" is a URL a user has
-- flagged as worth fetching content from. The table lives in
-- okt_repository and is the proof that the per-tenant routing
-- works:
--
--   * On the default database (Tier 1, shared), the table
--     holds rows for every repository and queries filter by
--     `repository_id`.
--   * On a per-tenant database (Tier 2, isolated), the table
--     holds rows for a single repository. The
--     `repository_id` filter is still in the query (belt and
--     suspenders), but physically only one repo's rows are
--     present.
--
-- The sqlc queries in db/queries/sources.sql reference this
-- table by its fully-qualified name (okt_repository.sources)
-- so they work unchanged against any tier's database.
--
-- The schema is intentionally minimal: a UUID id (so we can
-- generate it on the application side without round-tripping
-- the database), a repository_id (the routing key the
-- middleware resolves the per-request pool for), the URL, a
-- free-text kind (e.g. "homepage", "paper", "dataset"),
-- a status, and timestamps. Future per-repo tables follow
-- the same shape.

CREATE TABLE IF NOT EXISTS okt_repository.sources (
    id            UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    repository_id UUID NOT NULL REFERENCES okt_system.repositories(id) ON DELETE CASCADE,
    url           TEXT NOT NULL,
    kind          TEXT NOT NULL DEFAULT 'homepage',
    status        TEXT NOT NULL DEFAULT 'pending'
        CHECK (status IN ('pending', 'fetching', 'fetched', 'failed')),
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- A source is unique per (repository_id, url) so a duplicate
    -- insert returns a clean unique-violation error rather than
    -- silently creating a second row.
    UNIQUE (repository_id, url)
);

CREATE INDEX IF NOT EXISTS idx_sources_repository_id ON okt_repository.sources(repository_id);
CREATE INDEX IF NOT EXISTS idx_sources_status        ON okt_repository.sources(status);
