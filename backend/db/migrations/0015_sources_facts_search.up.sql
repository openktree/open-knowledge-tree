-- 0014_sources_facts_search.up.sql
--
-- Adds generated tsvector columns + GIN indexes to `sources` and
-- `facts` so the list endpoints can run full-text search over the
-- fields the UI already shows. The columns are STORED generated
-- columns (recomputed on insert/update of the source columns) so
-- there is no write-path code to maintain: the tsvector is always
-- in sync with the row.
--
-- Both columns use the 'english' configuration and
-- `websearch_to_tsquery` on the read side, which gives the UI a
-- familiar web-style query syntax (quoted phrases, OR, -exclude).
--
-- Idempotent per the migration rules in AGENTS.md: ADD COLUMN IF
-- NOT EXISTS and CREATE INDEX IF NOT EXISTS. The same file runs
-- against every database declared in `cfg.Databases`.

-- sources: search over url + parsed_title + doi. coalesce guards
-- against NULL parsed_title / doi on rows that were created before
-- parsing ran or that have no DOI.
ALTER TABLE okt_repository.sources
    ADD COLUMN IF NOT EXISTS search_tsv tsvector
    GENERATED ALWAYS AS (
        to_tsvector('english',
            coalesce(url, '') || ' ' ||
            coalesce(parsed_title, '') || ' ' ||
            coalesce(doi, ''))
    ) STORED;

CREATE INDEX IF NOT EXISTS idx_sources_search_tsv
    ON okt_repository.sources USING GIN (search_tsv);

-- facts: text is NOT NULL per the 0013 schema, but coalesce keeps
-- the generated column robust if the constraint is ever relaxed.
ALTER TABLE okt_repository.facts
    ADD COLUMN IF NOT EXISTS search_tsv tsvector
    GENERATED ALWAYS AS (
        to_tsvector('english', coalesce(text, ''))
    ) STORED;

CREATE INDEX IF NOT EXISTS idx_facts_search_tsv
    ON okt_repository.facts USING GIN (search_tsv);