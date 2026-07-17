-- 0037_repository_registry_enabled.up.sql
--
-- Per-repository registry integration toggle + a stable default for
-- the registry_id column introduced by 0035. Together they let an
-- admin (a) pick which configured knowledge registry a repo uses and
-- (b) turn the registry integration off for a repo without touching
-- global config.
--
-- `registry_enabled` defaults TRUE so existing deployments keep
-- using the registry as a cache when one is configured. When FALSE,
-- the retrieve_source worker skips the registry cache-lookup
-- (tryRegistryImport is a no-op) and the /remote browse+pull
-- endpoints return 503 for that repo. The auto_contribute flag
-- (migration 0036) is separately gated: enabling it requires
-- registry_enabled=TRUE.
--
-- `registry_id` is backfilled to 'default' for existing rows so the
-- single-registry deployment (legacy `providers.registry` block,
-- synthesized as id "default") keeps working without an admin
-- round-trip. New rows pick up the column default.

ALTER TABLE repositories
    ADD COLUMN IF NOT EXISTS registry_enabled BOOLEAN NOT NULL DEFAULT TRUE;

UPDATE repositories SET registry_id = 'default' WHERE registry_id IS NULL;

ALTER TABLE repositories ALTER COLUMN registry_id SET DEFAULT 'default';