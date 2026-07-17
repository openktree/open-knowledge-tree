-- 0021_investigations.up.sql
--
-- Investigations: a lightweight, user-facing bucket that groups a
-- subset of a repository's sources (and, transitively, their facts)
-- around a research topic. An investigation is purely an end-user
-- convenience — sources and facts know nothing about investigations;
-- only the `investigation_sources` junction records the membership.
--
-- This keeps the existing source/fact domain untouched and lets the
-- same source belong to multiple investigations (or none) without
-- schema changes to `sources`. Future phases (concepts, relations)
-- will follow the same pattern: derived artifacts stay repo-owned,
-- and an investigation just references a subset of them.
--
-- Tables live in `okt_repository` (per-repo data), scoped by
-- `repository_id`, matching the sources/facts convention. On a
-- shared (tier-1) database rows for every repo are interleaved and
-- filtered by `repository_id`; on an isolated/sovereign database
-- only one repo's rows are physically present.

CREATE TABLE IF NOT EXISTS okt_repository.investigations (
    id            UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    repository_id UUID NOT NULL REFERENCES okt_system.repositories(id) ON DELETE CASCADE,
    title         TEXT NOT NULL,
    topic         TEXT,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_investigations_repository_id
    ON okt_repository.investigations(repository_id);

-- Junction: which sources belong to which investigation. A source
-- may belong to many investigations; an investigation tracks many
-- sources. CASCADE on both sides so deleting a source or an
-- investigation cleans up the membership automatically. The PK
-- (investigation_id, source_id) makes AddInvestigationSource
-- idempotent via ON CONFLICT DO NOTHING.
CREATE TABLE IF NOT EXISTS okt_repository.investigation_sources (
    investigation_id UUID NOT NULL REFERENCES okt_repository.investigations(id) ON DELETE CASCADE,
    source_id        UUID NOT NULL REFERENCES okt_repository.sources(id) ON DELETE CASCADE,
    added_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (investigation_id, source_id)
);
CREATE INDEX IF NOT EXISTS idx_investigation_sources_source
    ON okt_repository.investigation_sources(source_id);