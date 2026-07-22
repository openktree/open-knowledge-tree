-- 0003_graphs.up.sql
--
-- Shared knowledge graphs: a registry-side namespace for whole-repository
-- graph bundles (sources + facts + concepts + summaries + syntheses +
-- investigations + reports + embeddings). A graph is the unit of
-- zero-cost sharing: team A exports its derived graph, pushes it here,
-- and any other OKT instance imports it into a fresh repository in a
-- single task — no re-decomposition, no re-summarization, no LLM cost.
--
-- The bundle itself (the GraphPackage JSON, gzipped) lives in S3 at
-- `graphs/{id}.json.gz`; this table is the searchable metadata index,
-- mirroring the sources/decompositions pattern (S3 for blobs, PG/SQLite
-- for metadata + search).

CREATE TABLE IF NOT EXISTS graphs (
    id             TEXT PRIMARY KEY,
    name           TEXT NOT NULL,
    description    TEXT NOT NULL DEFAULT '',
    owner          TEXT NOT NULL DEFAULT '',
    tags           TEXT[] NOT NULL DEFAULT '{}',
    source_count   INTEGER NOT NULL DEFAULT 0,
    fact_count     INTEGER NOT NULL DEFAULT 0,
    concept_count  INTEGER NOT NULL DEFAULT 0,
    s3_key         TEXT NOT NULL,
    sha256         TEXT NOT NULL DEFAULT '',
    schema_version INTEGER NOT NULL DEFAULT 1,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_graphs_tags ON graphs USING GIN (tags);
CREATE INDEX IF NOT EXISTS idx_graphs_name ON graphs(name);
CREATE INDEX IF NOT EXISTS idx_graphs_created_at ON graphs(created_at DESC);