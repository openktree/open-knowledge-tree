-- 0040_repository_allowed_models.up.sql
--
-- Per-repository model whitelist for the registry cache import.
-- The registry keys decompositions by (source_id, model_id) and
-- the import loop iterates every decomposition in the package;
-- this column selects which generation models a repo imports.
--
-- NULL (the default) = inherit the global registry.allowed_models
-- config value. A non-NULL array replaces the global list for this
-- repo: ["*"] = allow all, [] = allow none, ["model-a","model-b"]
-- = explicit set. The per-repo list is the enforcement list; the
-- global config becomes the fallback for repos that haven't set
-- one (per-repo replaces global).
--
-- Note: unqualified ALTER TABLE to match 0035/0036/0037/0038.

ALTER TABLE repositories
    ADD COLUMN IF NOT EXISTS allowed_models TEXT[];