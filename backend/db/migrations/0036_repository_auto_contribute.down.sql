-- 0036_repository_auto_contribute.down.sql

ALTER TABLE repositories DROP COLUMN IF EXISTS auto_contribute;