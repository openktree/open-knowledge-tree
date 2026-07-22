-- 0062_graph_rbac.down.sql
-- Removes the graph export/import RBAC policies added by the up
-- migration. The seed.go change is not reversed (a fresh seed after
-- this down migration re-adds them on the next boot), so this only
-- removes the backfilled rows from existing deployments.
DELETE FROM okt_system.casbin_rule
WHERE p_type='p' AND v2='graph' AND v3 IN ('export', 'write')
  AND v0 IN ('sysadmin', 'repoadmin', 'editor');