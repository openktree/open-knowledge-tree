-- 0043_reports_nesting.down.sql
-- Reverse of 0043_reports_nesting.up.sql.

DROP INDEX IF EXISTS okt_repository.idx_reports_parent_id;

ALTER TABLE okt_repository.reports
    DROP COLUMN IF EXISTS parent_id;