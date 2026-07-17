-- 0043_reports_nesting.up.sql
-- Add optional self-referential parent_id to reports for sub-report nesting.
-- Children are ordered by created_at within their parent (no manual reorder column).
-- ON DELETE CASCADE: deleting a parent removes its entire subtree.

ALTER TABLE okt_repository.reports
    ADD COLUMN IF NOT EXISTS parent_id UUID
        REFERENCES okt_repository.reports(id) ON DELETE CASCADE;

CREATE INDEX IF NOT EXISTS idx_reports_parent_id
    ON okt_repository.reports(parent_id);