-- 0051_report_max_facts.up.sql
--
-- Per-repository overrides for the annotate_report worker:
--   - max_facts_per_sentence: caps how many candidate facts are
--     fetched per sentence before the posture classifier runs. The
--     global default (config.providers.reports.max_facts_per_sentence,
--     5) can crowd out exact-numeric-match facts for value-heavy
--     repos; an operator may raise this. NULL = inherit the global
--     default.
--   - lexical_similarity_floor: the semantic-distance floor for the
--     hybrid lexical fallback. Facts the tsvector search surfaces
--     (because they share numeric/unit tokens with the sentence) are
--     re-checked against the sentence embedding and dropped if cosine
--     similarity is below this floor, preventing apples-to-oranges
--     matches (e.g. "0.9 kg weight gain" surfacing "0.9 kg CO2
--     emissions"). NULL = inherit the global default (0.6).
--
-- NOTE: this migration ships alongside a chunker change
-- (backend/internal/providers/decomposition/sentences.go) that splits
-- list blocks into per-item sentence units instead of one unit per
-- contiguous block. That change shifts sentence_index values for any
-- report/source containing list blocks, so existing
-- report_annotations.sentence_index and fact_references.sentence_index
-- rows become stale. Operators should re-annotate every report after
-- applying this migration (POST
-- /api/v1/repositories/{slug}/reports/{id}/annotate) so the stored
-- sentence_index values match the new chunker. The `just
-- reannotate-reports [slug]` recipe automates this. Source-page
-- fact_references highlighting will be stale for sources containing
-- list blocks until those sources are re-extracted; that is a
-- visual-only staleness (the facts themselves are intact) and
-- operators can opt into re-extraction via the existing
-- sources/{id}/re-extract endpoint.

ALTER TABLE okt_system.repository_report_settings
    ADD COLUMN IF NOT EXISTS max_facts_per_sentence INT
    CHECK (max_facts_per_sentence IS NULL OR (max_facts_per_sentence BETWEEN 1 AND 50));

ALTER TABLE okt_system.repository_report_settings
    ADD COLUMN IF NOT EXISTS lexical_similarity_floor DOUBLE PRECISION
    CHECK (lexical_similarity_floor IS NULL OR (lexical_similarity_floor BETWEEN 0 AND 1));