-- 0052_report_context_window.down.sql
--
-- Reverses 0052_report_context_window.up.sql. Drops the per-repo
-- context-window override columns. Existing
-- repository_report_settings rows are preserved (only the new
-- columns are dropped).

ALTER TABLE okt_system.repository_report_settings
    DROP COLUMN IF EXISTS context_window_before;

ALTER TABLE okt_system.repository_report_settings
    DROP COLUMN IF EXISTS context_window_after;