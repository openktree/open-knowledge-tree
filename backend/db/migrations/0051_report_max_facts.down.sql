-- 0051_report_max_facts.down.sql
--
-- Reverses 0051_report_max_facts.up.sql. Drops the
-- max_facts_per_sentence and lexical_similarity_floor override
-- columns from repository_report_settings; the annotate_report
-- worker falls back to the global config defaults.

ALTER TABLE okt_system.repository_report_settings
    DROP COLUMN IF EXISTS lexical_similarity_floor;

ALTER TABLE okt_system.repository_report_settings
    DROP COLUMN IF EXISTS max_facts_per_sentence;