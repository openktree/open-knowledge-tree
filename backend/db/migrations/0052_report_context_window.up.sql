-- 0052_report_context_window.up.sql
--
-- Per-repository override for the annotate_report posture classifier
-- context window. The classifier now receives the candidate sentence
-- plus up to N sentences immediately before it and M sentences after
-- it, so the LLM can disambiguate pronouns / referenced entities
-- ("it", "this approach", "the study") against the surrounding
-- prose. The window is bounded by the report's sentence array; the
-- first/last sentences naturally yield fewer than N/M context lines.
--
-- NULL = inherit the global defaults
-- (config.providers.reports.posture_classifier.context_window_before
--  / context_window_after, both defaulting to 2). A value of 0
--  disables context on that side (sentences past the report boundary
--  are never synthesized — the worker clamps to the available range).

ALTER TABLE okt_system.repository_report_settings
    ADD COLUMN IF NOT EXISTS context_window_before INT
    CHECK (context_window_before IS NULL OR (context_window_before BETWEEN 0 AND 10));

ALTER TABLE okt_system.repository_report_settings
    ADD COLUMN IF NOT EXISTS context_window_after INT
    CHECK (context_window_after IS NULL OR (context_window_after BETWEEN 0 AND 10));