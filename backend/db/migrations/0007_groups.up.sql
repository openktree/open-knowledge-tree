-- 0007_groups.up.sql
--
-- Groups: a many-to-many between users and roles that lets an
-- operator grant a role to many users at once. The schema
-- follows the plan discussed in chat ("Phase 1.5 — Groups"):
-- a `groups` table for the named bucket, a `group_members`
-- join table for user membership, and an `updated_at` column
-- on `groups` so the PATCH endpoint can bump it.
--
-- Membership ↔ Casbin: when a user is added to a group, the
-- rbac package writes a `g, userID, groupID, domain` row to
-- casbin_rule. When a role is granted to a group, it writes
-- a `g, groupID, role, domain` row. Casbin's grouping
-- function walks the chain (user → group → role) at enforce
-- time, so no model change is needed.
--
-- Lives in okt_system on the system database. The Casbin
-- side of the relationship is also stored in the same
-- casbin_rule table on the system database; per-tenant
-- databases do not mirror group data (groups are a
-- system-wide concept, not a per-repository one).
--
-- Table names are unqualified on purpose. The connection's
-- search_path (set by the dbpool registry on every new
-- connection) is `okt_system, okt_repository, public`, so
-- bare names resolve to okt_system. We do NOT `SET
-- search_path` from inside this file; that would mutate
-- connection state and clobber the registry's setting.

CREATE TABLE IF NOT EXISTS groups (
    id          UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    name        TEXT NOT NULL UNIQUE,
    description TEXT NOT NULL DEFAULT '',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS group_members (
    group_id  UUID NOT NULL REFERENCES groups(id) ON DELETE CASCADE,
    user_id   UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    joined_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (group_id, user_id)
);

-- Reverse lookup: "what groups is this user a member of?"
-- Used by /api/v1/users/{id}/groups and by the audit hooks
-- to expand a user→group→role chain when reporting changes.
CREATE INDEX IF NOT EXISTS idx_group_members_user ON group_members(user_id);
