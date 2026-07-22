-- 0054_permission_audit.up.sql
--
-- System-wide audit log. Records RBAC mutations (grant/revoke, role
-- assign/remove, group create/assign/remove), admin actions
-- (user/repo create/update/delete, OAuth client register/revoke,
-- provider-settings changes) and per-repo source-ingestion starts
-- (the actions that cost money or grant access). The table follows
-- the okt_system.ai_usage precedent: a single physical table on the
-- default/system database with an optional `repository_id` column
-- (NULL = system event, non-NULL = repo-scoped event). The daily
-- audit_cleanup periodic job deletes rows older than
-- `audit.retention_days` (default 30).
--
-- The same migration runs against every declared database (per the
-- multi-database rule) but only the system database's table is
-- actually used — same as ai_usage. The table is idempotent
-- (CREATE TABLE IF NOT EXISTS) so re-applying is safe.

CREATE TABLE IF NOT EXISTS okt_system.permission_audit (
    id              BIGSERIAL    PRIMARY KEY,
    occurred_at     TIMESTAMPTZ  NOT NULL DEFAULT now(),
    actor_user_id   UUID         NOT NULL REFERENCES okt_system.users(id) ON DELETE SET NULL,
    actor_username  TEXT         NOT NULL,
    action          TEXT         NOT NULL,
    object          TEXT         NOT NULL,
    repository_id   UUID         REFERENCES okt_system.repositories(id) ON DELETE CASCADE,
    target          TEXT,
    detail          JSONB        NOT NULL DEFAULT '{}'::jsonb,
    source_url      TEXT
);

CREATE INDEX IF NOT EXISTS permission_audit_occurred_at_idx
    ON okt_system.permission_audit (occurred_at DESC);
CREATE INDEX IF NOT EXISTS permission_audit_repository_idx
    ON okt_system.permission_audit (repository_id, occurred_at DESC)
    WHERE repository_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS permission_audit_actor_idx
    ON okt_system.permission_audit (actor_user_id, occurred_at DESC);
CREATE INDEX IF NOT EXISTS permission_audit_action_idx
    ON okt_system.permission_audit (action);