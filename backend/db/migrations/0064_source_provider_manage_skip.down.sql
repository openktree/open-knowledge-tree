-- 0064_source_provider_manage_skip.down.sql
-- Removes the backfilled source_provider:manage policies.
DELETE FROM okt_system.casbin_rule
WHERE p_type='p' AND v2='source_provider' AND v3='manage'
  AND v0 IN ('sysadmin','repoadmin');