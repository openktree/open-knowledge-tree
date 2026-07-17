-- 0037_repository_registry_enabled.down.sql
ALTER TABLE repositories ALTER COLUMN registry_id DROP DEFAULT;
ALTER TABLE repositories DROP COLUMN IF EXISTS registry_enabled;