-- 0045_backfill_silent_failures.down.sql
--
-- Reverses 0045_backfill_silent_failures.up.sql by restoring the
-- rows it flipped back to their pre-migration status. The reversal
-- is best-effort: the original 'fetched' vs 'processed' distinction
-- is reconstructed from processed_at (a non-NULL processed_at means
-- the row had been decomposed, so it was 'processed'; otherwise
-- 'fetched'), and the original error/parse_status are not
-- recoverable from the row itself — we restore parse_status='ok'
-- and clear error, which is the state the rows were in before the
-- backfill (the boilerplate guard wasn't catching them, so they
-- were stamped 'ok').
--
-- The down migration exists for schema-reversal completeness (the
-- golang-migrate contract wants every up to have a down). In
-- practice an operator who wants to undo the backfill is better
-- served by re-fetching the affected rows: the boilerplate content
-- is still boilerplate, and restoring the row to 'fetched' would
-- re-introduce the silent failure the backfill fixed.

UPDATE okt_repository.sources
SET status       = CASE WHEN processed_at IS NOT NULL THEN 'processed' ELSE 'fetched' END,
    parse_status = 'ok',
    error        = NULL,
    updated_at   = now()
WHERE error = 'silent boilerplate detected on backfill';