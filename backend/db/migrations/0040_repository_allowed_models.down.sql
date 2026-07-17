-- 0040_repository_allowed_models.down.sql
ALTER TABLE okt_system.repositories DROP COLUMN IF EXISTS allowed_models;