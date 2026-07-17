-- 0035_repository_registry_id.down.sql
ALTER TABLE okt_system.repositories DROP COLUMN IF EXISTS registry_id;
