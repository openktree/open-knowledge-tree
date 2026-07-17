-- 0019_sources_oa_status.up.sql
--
-- Adds an `oa_status` column to okt_repository.sources so the
-- retrieve_source worker can record the open-access status
-- Unpaywall reported for the DOI (e.g. "green", "gold",
-- "bronze", "hybrid", "closed"). The UI uses this to show
-- users why an article might be incomplete — a "closed"
-- status explains that the full text is paywalled and only
-- the abstract/landing page was retrieved.
--
-- The column is nullable: existing rows keep NULL until a
-- fetch with Unpaywall in the chain runs. Sources fetched
-- without a DOI (plain URLs) stay NULL — the OA concept
-- only applies to DOI-classified works.
--
-- Idempotent per AGENTS.md: ADD COLUMN IF NOT EXISTS.

ALTER TABLE okt_repository.sources
    ADD COLUMN IF NOT EXISTS oa_status TEXT;