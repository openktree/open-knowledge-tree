-- 0062_graph_rbac.up.sql
--
-- Backfills the graph export/import RBAC policies into existing
-- deployments. The seed.go change (rbac/seed.go) only runs on a fresh
-- casbin_rule table, so existing deployments need explicit INSERTs to
-- grant the graph.export + graph.write permissions the new Shared
-- Graphs feature expects.
--
-- Policies:
--   sysadmin  → graph:export, graph:write  (system scope)
--   repoadmin → graph:export, graph:write   (repo scope, enforced via RequireRepoPermission)
--   editor    → graph:export                (repo scope; export is read-ish)
--
-- The WHERE NOT EXISTS guard keeps each INSERT idempotent: a
-- deployment that already has the policy (e.g. one seeded fresh after
-- the seed.go change) is unaffected. There is no unique constraint on
-- the policy columns so ON CONFLICT can't be used.

INSERT INTO okt_system.casbin_rule (p_type, v0, v1, v2, v3)
SELECT 'p', 'sysadmin', '*', 'graph', 'export'
WHERE NOT EXISTS (
    SELECT 1 FROM okt_system.casbin_rule
    WHERE p_type='p' AND v0='sysadmin' AND v1='*' AND v2='graph' AND v3='export'
);

INSERT INTO okt_system.casbin_rule (p_type, v0, v1, v2, v3)
SELECT 'p', 'sysadmin', '*', 'graph', 'write'
WHERE NOT EXISTS (
    SELECT 1 FROM okt_system.casbin_rule
    WHERE p_type='p' AND v0='sysadmin' AND v1='*' AND v2='graph' AND v3='write'
);

INSERT INTO okt_system.casbin_rule (p_type, v0, v1, v2, v3)
SELECT 'p', 'repoadmin', '*', 'graph', 'export'
WHERE NOT EXISTS (
    SELECT 1 FROM okt_system.casbin_rule
    WHERE p_type='p' AND v0='repoadmin' AND v1='*' AND v2='graph' AND v3='export'
);

INSERT INTO okt_system.casbin_rule (p_type, v0, v1, v2, v3)
SELECT 'p', 'repoadmin', '*', 'graph', 'write'
WHERE NOT EXISTS (
    SELECT 1 FROM okt_system.casbin_rule
    WHERE p_type='p' AND v0='repoadmin' AND v1='*' AND v2='graph' AND v3='write'
);

INSERT INTO okt_system.casbin_rule (p_type, v0, v1, v2, v3)
SELECT 'p', 'editor', '*', 'graph', 'export'
WHERE NOT EXISTS (
    SELECT 1 FROM okt_system.casbin_rule
    WHERE p_type='p' AND v0='editor' AND v1='*' AND v2='graph' AND v3='export'
);