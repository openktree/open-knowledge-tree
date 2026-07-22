-- No safe automatic downgrade: deleting the policy would revoke
-- repoadmin AI Usage access on deployments that rely on it.
-- Operators who want to revoke can manually run:
--   DELETE FROM okt_system.casbin_rule
--   WHERE p_type='p' AND v0='repoadmin' AND v1='*' AND v2='ai_usage' AND v3='read';
SELECT 1;