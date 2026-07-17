-- 0007_groups.down.sql
--
-- Reverse of 0007_groups. The order matters: group_members
-- has FKs to groups and to users, and dropping the parent
-- tables in the wrong order would leave the FKs dangling.

DROP INDEX IF EXISTS idx_group_members_user;
DROP TABLE IF EXISTS group_members;
DROP TABLE IF EXISTS groups;
