-- 0013_facts.up.sql
--
-- Adds a `processed` status to the sources table, a `facts` table for
-- extracted atomic claims, and a `fact_sources` junction that links
-- facts to the sources that extracted or confirmed them.
--
-- The fact-source relationship is a junction only: a fact shared by
-- N sources has one row in `facts` and N rows in `fact_sources`. There
-- is no "origin" concept and no `facts.source_id` column — all source
-- links live in the junction. `source_count` is computed at read time
-- via an aggregate JOIN, not denormalized.
--
-- `status` tracks the dedup lifecycle:
--   'new'       — freshly extracted, not yet embedded/deduplicated
--   'stable'    — confirmed unique after deduplication
--   'to_delete' — flagged as duplicate by deduplication
-- `embedded_at` + `embedded_model` record when and how the fact was
-- vectorized into Qdrant; `embedded_at IS NULL` is the signal that a
-- fact still needs embedding.

-- Extend the sources status CHECK to include 'processed'.
-- We drop and re-add because Postgres does not support
-- ALTER CHECK constraint; the table is small enough that
-- a brief exclusive lock is acceptable.
ALTER TABLE okt_repository.sources
    DROP CONSTRAINT IF EXISTS sources_status_check;

ALTER TABLE okt_repository.sources
    ADD CONSTRAINT sources_status_check
        CHECK (status IN ('pending', 'fetching', 'fetched', 'failed', 'processed'));

-- Timestamp for when decomposition completed.
ALTER TABLE okt_repository.sources
    ADD COLUMN IF NOT EXISTS processed_at TIMESTAMPTZ;

-- Facts: one row per unique atomic claim. No source_id — all source
-- links live in fact_sources (N:M). A fact shared by 10 sources has
-- 10 rows in fact_sources and 1 row here. source_count is computed
-- at read time (aggregate JOIN), not denormalized.
CREATE TABLE IF NOT EXISTS okt_repository.facts (
    id             UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    text           TEXT NOT NULL,
    status         TEXT NOT NULL DEFAULT 'new'
        CHECK (status IN ('new', 'stable', 'to_delete')),
    embedded_at    TIMESTAMPTZ,
    embedded_model TEXT,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_facts_status     ON okt_repository.facts(status);
CREATE INDEX IF NOT EXISTS idx_facts_created_at ON okt_repository.facts(created_at);

-- Junction: links facts to the sources that extracted or confirmed them.
-- chunk_index is per-extraction (the same fact from source A's chunk 3
-- and source B's chunk 7 records both). first_seen_at tracks when each
-- source link was established (dedup merges add rows here).
CREATE TABLE IF NOT EXISTS okt_repository.fact_sources (
    fact_id       UUID NOT NULL REFERENCES okt_repository.facts(id) ON DELETE CASCADE,
    source_id     UUID NOT NULL REFERENCES okt_repository.sources(id) ON DELETE CASCADE,
    chunk_index   INT NOT NULL DEFAULT 0,
    first_seen_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (fact_id, source_id)
);
CREATE INDEX IF NOT EXISTS idx_fact_sources_source ON okt_repository.fact_sources(source_id);
CREATE INDEX IF NOT EXISTS idx_fact_sources_fact   ON okt_repository.fact_sources(fact_id);