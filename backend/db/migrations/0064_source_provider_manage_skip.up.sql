-- 0064_source_provider_manage_skip.up.sql
--
-- Backfills the source_provider:manage policy the new
-- POST/DELETE /sources/skip endpoints require. seed.go only
-- runs on a fresh casbin_rule table, so existing deployments
-- need explicit INSERTs to grant repoadmin + sysadmin the
-- manage action the skip endpoints gate on.
--
-- Idempotent: WHERE NOT EXISTS guards each INSERT (no unique
-- constraint on the policy columns, so ON CONFLICT can't be
-- used). The same file runs against every database declared in
-- cfg.Databases; casbin_rule lives in okt_system.

INSERT INTO okt_system.casbin_rule (p_type, v0, v1, v2, v3)
SELECT 'p', 'sysadmin', '*', 'source_provider', 'manage'
WHERE NOT EXISTS (
    SELECT 1 FROM okt_system.casbin_rule
    WHERE p_type='p' AND v0='sysadmin' AND v1='*' AND v2='source_provider' AND v3='manage'
);

INSERT INTO okt_system.casbin_rule (p_type, v0, v1, v2, v3)
SELECT 'p', 'repoadmin', '*', 'source_provider', 'manage'
WHERE NOT EXISTS (
    SELECT 1 FROM okt_system.casbin_rule
    WHERE p_type='p' AND v0='repoadmin' AND v1='*' AND v2='source_provider' AND v3='manage'
);