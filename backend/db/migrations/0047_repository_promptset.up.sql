-- 0047_repository_promptset.up.sql
--
-- Per-repository promptset selection. A repository has one ACTIVE
-- promptset (the philosophy its future decompositions use) and an
-- optional ACCEPTED set (the hashes the registry cache may pull in
-- without contaminating the graph). Both are pointers to either the
-- built-in promptset (compiled into the binary) or a row in
-- okt_system.promptsets.
--
-- active_promptset_hash is NULL when the repo inherits the global
-- config default (providers.promptset_default in config.default.yaml,
-- which itself defaults to the built-in hash). A non-NULL value must
-- be either the built-in hash or a row in okt_system.promptsets; the
-- handler validates against the resolver before accepting it.
--
-- accepted_promptset_hashes defaults to '{}' meaning "only the active
-- hash is accepted". An admin can add more hashes to allow pulls of
-- foreign decompositions that share those philosophies — e.g. a repo
-- running a v2 promptset can still import v1 facts the admin has
-- vetted. The pull worker filters remote decompositions to those whose
-- promptset_hash is in this set (active always included).
--
-- The columns live on repositories (not a companion table) because the
-- pointer is a single value plus a small array, mirroring the
-- registry_id / allowed_models pattern. See migration 0035 / 0040.

ALTER TABLE repositories
    ADD COLUMN IF NOT EXISTS active_promptset_hash TEXT;

ALTER TABLE repositories
    ADD COLUMN IF NOT EXISTS accepted_promptset_hashes TEXT[] NOT NULL DEFAULT '{}';