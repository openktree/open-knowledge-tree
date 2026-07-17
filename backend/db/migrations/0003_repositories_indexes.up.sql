-- 0003_repositories_indexes.up.sql
--
-- Indexes that the queries in db/queries/repositories.sql rely
-- on. The previous design put these in schema_system.sql
-- alongside the table definition; the migration runner
-- (golang-migrate) applies one file at a time, so DDL and
-- index DDL are split for cleanliness.
--
-- id-style: we don't index on UUIDs we generated. Slug is
-- uniquely indexed already (it's the unique constraint).
-- owner_id is a foreign-key lookup target; database_name is
-- the routing key for the per-tenant middleware cache.

CREATE INDEX IF NOT EXISTS idx_casbin_rule_p_type        ON okt_system.casbin_rule(p_type);
CREATE INDEX IF NOT EXISTS idx_casbin_rule_v0            ON okt_system.casbin_rule(v0);
CREATE INDEX IF NOT EXISTS idx_repositories_slug         ON okt_system.repositories(slug);
CREATE INDEX IF NOT EXISTS idx_repositories_owner_id     ON okt_system.repositories(owner_id);
CREATE INDEX IF NOT EXISTS idx_repositories_database_name ON okt_system.repositories(database_name);
