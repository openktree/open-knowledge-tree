-- 0001_init.down.sql
-- Reverse of 0001_init.up.sql.
--
-- Order matters: drop the tables that reference okt_system.users
-- (sessions) before dropping users, drop casbin_rule last because
-- it has no FK to anything else. Schemas are kept (an operator
-- may have added objects to them); the down migration only
-- undoes the OKT-managed objects.
DROP TABLE IF EXISTS sessions;
DROP TABLE IF EXISTS users;
DROP TABLE IF EXISTS casbin_rule;
-- Note: we do not drop the schemas or the uuid-ossp extension
-- here. Dropping the schemas would erase any non-OKT objects
-- the operator added (e.g. monitoring tables), and dropping
-- uuid-ossp would break any extension that depends on it.
-- Operators who want a clean reset should DROP SCHEMA manually
-- after stopping the application.
