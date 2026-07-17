-- 0036_repository_auto_contribute.up.sql
--
-- Add an `auto_contribute` column to the repositories table (lives
-- in okt_system via search_path) so each repo can opt into
-- automatically pushing processed sources to the remote knowledge
-- registry. When FALSE (the default), the cleanup_facts worker
-- skips enqueuing contribute_source jobs — sources are only
-- pushed when an admin triggers "Push All to Registry" manually.
-- When TRUE, every source that finishes the ingestion pipeline is
-- contributed to the registry automatically.
--
-- Follows the same pattern as 0035_repository_registry_id: a
-- repo-level registry setting stored as a column on the
-- repositories table (the (provider_kind, provider_id)-keyed
-- table is reserved for provider triples).

ALTER TABLE repositories
    ADD COLUMN IF NOT EXISTS auto_contribute BOOLEAN NOT NULL DEFAULT FALSE;