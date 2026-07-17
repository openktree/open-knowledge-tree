-- 0018_sources_fetch_attempts.up.sql
--
-- Adds a `fetch_attempts` JSONB column to okt_repository.sources
-- so the retrieve_source worker can persist the audit trail the
-- fetch strategy records on every Resolve call. The column
-- stores an array of {provider, success, error, elapsed_ms}
-- objects — one per provider the strategy tried, in chain
-- order — so the UI can show "which tier fetched this content
-- (or which tiers failed and why)" without grepping logs.
--
-- The column is nullable: existing rows keep NULL until a
-- fetch with the new strategy runs, so the migration is
-- backward-compatible. The worker writes the JSONB via a
-- new MarkSourceFetchAttempts query; reads are best-effort
-- (the UI treats NULL as "no audit trail available").
--
-- Idempotent per AGENTS.md: ADD COLUMN IF NOT EXISTS. The
-- same file runs against every database declared in
-- cfg.Databases.

ALTER TABLE okt_repository.sources
    ADD COLUMN IF NOT EXISTS fetch_attempts JSONB;