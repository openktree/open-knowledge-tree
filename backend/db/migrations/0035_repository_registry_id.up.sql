-- 0035_repository_registry_id.up.sql
--
-- Add a `registry_id` column to the repositories table (lives in
-- okt_system via search_path) so each repo can select which knowledge
-- registry endpoint to use for cache-lookup (import path) and
-- contribution (upload path). When NULL, the configured default
-- registry ID is used. The column is a free TEXT so the admin can
-- type any configured `registries[].id` value; consistency is
-- enforced at the API layer (a dropdown of valid IDs), not via a
-- foreign key.

ALTER TABLE repositories
    ADD COLUMN IF NOT EXISTS registry_id TEXT;
