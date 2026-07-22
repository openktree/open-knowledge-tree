-- 0058_concepts_search_tsv.up.sql
--
-- Adds GIN expression indexes over `concepts` and `concept_aliases`
-- so the concept list/search endpoints can run weighted full-text
-- search over canonical_name + description (on concepts) and
-- alias_text (on concept_aliases), mirroring the `facts.search_tsv`
-- pattern from migration 0015.
--
-- Unlike 0015 we use expression indexes (`to_tsvector(...)`) rather
-- than a STORED generated column. A STORED generated column on
-- `concepts` cannot reference the `concept_aliases` sibling table,
-- so it could not cover aliases. Expression indexes avoid that
-- limitation and need no write-path maintenance: the index is
-- recomputed on insert/update of the indexed columns.
--
-- Weights (per Postgres tsvector weight labels A > B > C > D):
--   canonical_name  -> A (highest)
--   alias_text      -> A (an alias is as good as the canonical name
--                       for discoverability)
--   description     -> D (lowest; a description hit should rank far
--                       below a name/alias hit)
--
-- Both indexes use the 'english' configuration and
-- `websearch_to_tsquery` on the read side, matching the facts search
-- syntax (quoted phrases, OR, -exclude). The read query combines the
-- per-concept weighted tsv with the per-alias weighted tsv via
-- EXISTS (filter) and a MAX subquery (rank contribution), so an
-- alias hit pulls in the parent concept's whole canonical-name
-- group while still ranking name/alias hits above description hits.
--
-- Idempotent per the migration rules in AGENTS.md: CREATE INDEX IF
-- NOT EXISTS. The same file runs against every database declared in
-- `cfg.Databases`.

-- concepts: weighted search over canonical_name (A) + description (D).
-- coalesce guards against NULL description (the column is nullable
-- per 0023).
CREATE INDEX IF NOT EXISTS idx_concepts_search_tsv
    ON okt_repository.concepts USING GIN (
        tsvector_concat(
            setweight(to_tsvector('english', coalesce(canonical_name, '')), 'A'),
            setweight(to_tsvector('english', coalesce(description, '')), 'D')
        )
    );

-- concept_aliases: weighted search over alias_text (A). Used by the
-- EXISTS / MAX subqueries in ListGroupedConceptsByRepo /
-- CountGroupedConceptsByRepo so an alias hit pulls in the parent
-- concept's whole canonical-name group at the same priority as a
-- canonical-name hit.
CREATE INDEX IF NOT EXISTS idx_concept_aliases_search_tsv
    ON okt_repository.concept_aliases USING GIN (
        setweight(to_tsvector('english', coalesce(alias_text, '')), 'A')
    );