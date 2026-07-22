-- 0061_concept_groups.up.sql
--
-- Per-repo, per-canonical-name concept summary table maintained by
-- the application (the ingest workers + migrate_context + registry
-- imports) so the concept LIST endpoint can paginate groups in SQL
-- instead of loading every per-context concept row and grouping in
-- Go. Always live (no matview, no refresh worker); a manual
-- "recompute concept groups" River job (recompute_concept_groups) is
-- the safety net for repair.
--
-- One row per (repository_id, lower(canonical_name)) — the group key
-- the API presents as "one concept, many contexts". total_fact_count
-- is the group's summed fact_concepts count across every context;
-- context_count is the number of per-context rows; canonical_name is
-- the display name (min(canonical_name)); any_embedded is true when
-- any context has been embedded.
--
-- Reads: ListConceptGroupsByRepoPage uses idx_concept_groups_repo_count_name
-- for ORDER BY total_fact_count DESC, canonical_name ASC + LIMIT/OFFSET,
-- an O(page) index range scan regardless of repo size. CountConceptGroupsByRepo
-- uses the PK prefix (index-only COUNT).
--
-- Writes: UpsertConceptGroups recomputes the touched name_keys from
-- live concepts+fact_concepts and upserts; DeleteStaleConceptGroups
-- removes groups with no remaining concepts (e.g. after migrate_context
-- deletes the last context of a name). RecomputeAllConceptGroupsForRepo
-- does the same for every group in a repo (the repair path).

CREATE TABLE IF NOT EXISTS okt_repository.concept_groups (
    repository_id    UUID NOT NULL REFERENCES okt_system.repositories(id) ON DELETE CASCADE,
    name_key         TEXT NOT NULL,                -- lower(canonical_name), the group key
    canonical_name   TEXT NOT NULL,                -- min(canonical_name), the display name
    context_count    INTEGER NOT NULL DEFAULT 0,
    total_fact_count BIGINT NOT NULL DEFAULT 0,
    any_embedded     BOOLEAN NOT NULL DEFAULT false,
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (repository_id, name_key)
);

-- Serves ORDER BY total_fact_count DESC, canonical_name ASC + LIMIT/OFFSET
-- for the q="" list page (the slow default path this table exists to fix).
CREATE INDEX IF NOT EXISTS idx_concept_groups_repo_count_name
    ON okt_repository.concept_groups (repository_id, total_fact_count DESC, canonical_name ASC);

-- One-time backfill from existing concepts + fact_concepts. Idempotent
-- (ON CONFLICT DO NOTHING) so it's safe to re-run; subsequent maintenance
-- is incremental via UpsertConceptGroups. The backfill is a single grouped
-- aggregate over the repo's concepts, bounded by concept count (200k now,
-- millions in production — a few seconds at 200k, proportionally longer
-- at scale). Runs in the migration so the list page is paginated from the
-- first boot after upgrade, with no "empty summary until a job runs" window.
INSERT INTO okt_repository.concept_groups
    (repository_id, name_key, canonical_name, context_count, total_fact_count, any_embedded, updated_at)
SELECT c.repository_id,
       lower(c.canonical_name)                               AS name_key,
       min(c.canonical_name)                                 AS canonical_name,
       count(*)::int                                         AS context_count,
       COALESCE(COUNT(fc.fact_id), 0)::bigint                AS total_fact_count,
       bool_or(c.embedded_at IS NOT NULL)                    AS any_embedded,
       now()
FROM okt_repository.concepts c
LEFT JOIN okt_repository.fact_concepts fc ON fc.concept_id = c.id
GROUP BY c.repository_id, lower(c.canonical_name)
ON CONFLICT (repository_id, name_key) DO NOTHING;