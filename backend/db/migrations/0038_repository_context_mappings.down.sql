-- 0038_repository_context_mappings.down.sql
--
-- Reverses 0038. Drops the mapping table and the two policy columns.
-- Safe to run against any database (IF EXISTS guards).

DROP TABLE IF EXISTS okt_system.repository_context_mappings;

ALTER TABLE okt_system.repositories DROP COLUMN IF EXISTS catch_all_context;
ALTER TABLE okt_system.repositories DROP COLUMN IF EXISTS unmapped_context_policy;