-- 0003_repositories_indexes.down.sql
DROP INDEX IF EXISTS okt_system.idx_repositories_database_name;
DROP INDEX IF EXISTS okt_system.idx_repositories_owner_id;
DROP INDEX IF EXISTS okt_system.idx_repositories_slug;
DROP INDEX IF EXISTS okt_system.idx_casbin_rule_v0;
DROP INDEX IF EXISTS okt_system.idx_casbin_rule_p_type;
