-- 0038_repository_context_mappings.up.sql
--
-- Per-repository context mapping: translates a repo's local context
-- vocabulary (okt_system.repository_contexts) to and from the
-- registry's published canonical context vocabulary
-- (GET /api/v1/contexts on the knowledge registry). The mapping is
-- consumed at the contribute/pull boundary so concept contexts are
-- rewritten on the way in/out, preventing vocabulary drift between
-- the local and registry context sets.
--
-- Contribute (local → registry): 1 local context maps to exactly 1
-- registry context. A local context with multiple registry targets
-- is not expressible (unique index on (repo, lower(local_context))
-- enforces it); the admin stores one. Unmapped local contexts whose
-- label exists in the registry vocab are pushed verbatim; unmapped
-- and absent-from-vocab contexts are skipped.
--
-- Pull (registry → local): 1 registry context maps to exactly 1
-- local context. Unmapped registry contexts are handled per the
-- repo's unmapped_context_policy (skip | auto_add | catch_all).
--
-- Both columns are free TEXT (no FK) so the mapping survives a
-- registry vocab refresh; consistency is enforced at the handler
-- layer (the admin picks from the live registry vocab dropdown).
--
-- Note: ALTER TABLE repositories is unqualified to match the
-- pattern in 0035/0036/0037. sqlc's parser resolves it via the
-- same search_path the dbpool registry sets at runtime; qualifying
-- it with okt_system. breaks sqlc generate.

CREATE TABLE IF NOT EXISTS okt_system.repository_context_mappings (
    repository_id      UUID    NOT NULL REFERENCES okt_system.repositories(id) ON DELETE CASCADE,
    local_context      TEXT    NOT NULL,
    registry_context   TEXT    NOT NULL,
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at         TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- One local context maps to exactly one registry context.
CREATE UNIQUE INDEX IF NOT EXISTS uq_repo_ctx_map_local
    ON okt_system.repository_context_mappings (repository_id, lower(local_context));

-- The reverse map (registry → local) is also unique per repo so a
-- registry context resolves to a single local target on pull.
CREATE UNIQUE INDEX IF NOT EXISTS uq_repo_ctx_map_registry
    ON okt_system.repository_context_mappings (repository_id, lower(registry_context));

-- Per-repo pull policy for unmapped registry contexts. 'skip' (the
-- default) drops the concept; 'auto_add' seeds a new
-- repository_contexts row (is_custom=TRUE) named after the registry
-- label and imports under it; 'catch_all' routes unmapped registry
-- contexts to the repo's configured catch_all_context.
ALTER TABLE repositories
    ADD COLUMN IF NOT EXISTS unmapped_context_policy TEXT NOT NULL DEFAULT 'skip'
    CHECK (unmapped_context_policy IN ('skip','auto_add','catch_all'));

-- The local context label used when unmapped_context_policy is
-- 'catch_all'. NULL when the policy is 'skip' or 'auto_add'. The
-- handler validates this is a member of repository_contexts before
-- accepting a catch_all policy.
ALTER TABLE repositories
    ADD COLUMN IF NOT EXISTS catch_all_context TEXT;