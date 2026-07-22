-- 0055_repoadmin_audit_read.up.sql
--
-- Backfills the repoadmin → audit.read policy into existing
-- deployments. The seed.go change (rbac/seed.go) only runs on a
-- fresh casbin_rule table, so existing deployments need an explicit
-- INSERT to grant repoadmin the audit.read permission that the new
-- Audit feature expects. The policy is repo-scoped at enforcement
-- time (RequireRepoPermission reads the repoID from the URL), so the
-- row uses "*" as the domain — the same form seed.go uses.
--
-- The WHERE NOT EXISTS guard keeps the migration idempotent: a
-- deployment that already has the policy (e.g. one seeded fresh
-- after the seed.go change) is unaffected. There is no unique
-- constraint on the policy columns so ON CONFLICT can't be used.

INSERT INTO okt_system.casbin_rule (p_type, v0, v1, v2, v3)
SELECT 'p', 'repoadmin', '*', 'audit', 'read'
WHERE NOT EXISTS (
    SELECT 1 FROM okt_system.casbin_rule
    WHERE p_type='p' AND v0='repoadmin' AND v1='*' AND v2='audit' AND v3='read'
);