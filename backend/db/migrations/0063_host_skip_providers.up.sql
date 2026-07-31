-- 0063_host_skip_providers.up.sql
--
-- Adds okt_repository.host_skip_providers: a learned
-- (host, provider_id) skip list the fetch strategy enforces
-- in addition to the static host_overrides. When a tier has
-- at least min_sample total attempts and a failure rate >=
-- failure_threshold for a host, the strategy upserts a skip
-- row here and jumps that tier for that host until the row
-- expires (cooldown). This avoids repeatedly burning the
-- per-provider timeout on hosts a tier can never retrieve
-- (e.g. a host that always 403s the fetch tier).
--
-- Rows are per-repository (repository_id) so skips are
-- scoped to the repo whose fetch_attempts audit trail fed
-- the decision. The strategy reads active rows
-- (expires_at > now()) at fetch time; the Providers page
-- "Fetch Domains" tab surfaces them and lets an operator
-- manually add/remove skips via the /sources/skip endpoints.
--
-- Idempotent per AGENTS.md: CREATE TABLE IF NOT EXISTS /
-- CREATE INDEX IF NOT EXISTS. The same file runs against
-- every database declared in cfg.Databases.

CREATE TABLE IF NOT EXISTS okt_repository.host_skip_providers (
    repository_id  UUID        NOT NULL REFERENCES okt_system.repositories(id) ON DELETE CASCADE,
    host           TEXT        NOT NULL,
    provider_id    TEXT        NOT NULL,
    failure_rate   DOUBLE PRECISION NOT NULL DEFAULT 0.0,
    sample_size    INTEGER     NOT NULL DEFAULT 0,
    skipped_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at     TIMESTAMPTZ NOT NULL,
    manual         BOOLEAN     NOT NULL DEFAULT false,
    PRIMARY KEY (repository_id, host, provider_id)
);

CREATE INDEX IF NOT EXISTS idx_host_skip_providers_expires
    ON okt_repository.host_skip_providers (repository_id, expires_at);