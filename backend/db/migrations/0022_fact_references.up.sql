-- 0022_fact_references.up.sql
--
-- Sentence-level provenance for facts.
--
-- Two additions:
--
-- 1. sources.sentence_offsets INT[]
--    A flat array of rune offsets [start0, end0, start1, end1, ...]
--    describing the deterministic global sentence array produced by
--    decomposition.SegmentSentences over the source's parsed_markdown
--    (or parsed_text when markdown is absent). End offsets are
--    exclusive. The array is written once by the retrieve_source
--    worker after parsing and is the stable contract that
--    fact_references.sentence_index keys into.
--
--    The sentence splitter rules are deterministic and
--    chunker-config-independent. If the splitter rules ever change,
--    every source must be re-segmented and every fact_references row
--    re-derived — there is no implicit migration.
--
-- 2. okt_repository.fact_references
--    One row per (fact, source, sentence_index). A fact derived from
--    sentences 2, 3, and 5 of a source produces three rows sharing
--    the same fact_id. Fully normalized: dedup relinking is a single
--    integer-keyed UPDATE; the PK dedups overlapping citations from
--    merged facts. chunk_index is retained for provenance/debug only
--    (which chunk the AI saw) and is NOT a positioning key.
--
-- The existing okt_repository.fact_sources junction (coarse,
-- chunk-level) is unchanged and remains the source of source_count
-- and the ListFactSources UI. fact_references is the fine-grained
-- layer alongside it.

ALTER TABLE okt_repository.sources
    ADD COLUMN IF NOT EXISTS sentence_offsets INT[];

CREATE TABLE IF NOT EXISTS okt_repository.fact_references (
    fact_id        UUID        NOT NULL REFERENCES okt_repository.facts(id) ON DELETE CASCADE,
    source_id      UUID        NOT NULL REFERENCES okt_repository.sources(id) ON DELETE CASCADE,
    sentence_index INT         NOT NULL,
    chunk_index    INT         NOT NULL DEFAULT 0,
    first_seen_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (fact_id, source_id, sentence_index)
);
CREATE INDEX IF NOT EXISTS idx_fact_references_source
    ON okt_repository.fact_references(source_id, sentence_index);
CREATE INDEX IF NOT EXISTS idx_fact_references_fact
    ON okt_repository.fact_references(fact_id);