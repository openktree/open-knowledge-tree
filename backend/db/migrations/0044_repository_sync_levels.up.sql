-- 0044_repository_sync_levels.up.sql
--
-- Per-repository push/pull sync levels. Controls how much of a
-- source's decomposition the backend contributes to the registry
-- (push) and imports from the registry (pull). The levels are
-- cumulative: "facts" includes sources + facts + fact embeddings;
-- "concepts" adds concepts + fact_concept links + concept embeddings.
--
-- The defaults are "concepts" so existing deployments keep the
-- current full push/pull behavior until an admin opts a repo into
-- facts-only mode (used to regenerate concepts from a clean slate:
-- push facts-only to the registry, wipe local concepts, pull back,
-- and let the concept/alias pipeline rebuild from the stable facts).
--
-- Read by:
--   - contribute_source worker (push_level): gates whether to load
--     and push concepts/links/concept-embeddings alongside facts.
--   - pull_all_from_registry / retrieve_source / remote_pull
--     (pull_level): gates whether to import concepts/links/concept-
--     embeddings from the pulled decomposition.
-- The registry.SyncLevelFilter centralizes the field-stripping.

ALTER TABLE repositories
    ADD COLUMN IF NOT EXISTS registry_push_level TEXT NOT NULL DEFAULT 'concepts'
        CHECK (registry_push_level IN ('facts', 'concepts'));

ALTER TABLE repositories
    ADD COLUMN IF NOT EXISTS registry_pull_level TEXT NOT NULL DEFAULT 'concepts'
        CHECK (registry_pull_level IN ('facts', 'concepts'));