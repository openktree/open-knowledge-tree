-- 0041_report_posture.down.sql
--
-- Reverses 0041_report_posture.up.sql. Drops the posture column,
-- the per-repo report settings table, and restores the original
-- task_kind CHECK on repository_model_settings.

ALTER TABLE okt_system.repository_model_settings
    DROP CONSTRAINT IF EXISTS repository_model_settings_task_kind_check;
ALTER TABLE okt_system.repository_model_settings
    ADD CONSTRAINT repository_model_settings_task_kind_check
    CHECK (task_kind IN
        ('fact_extraction','image_extraction','concept_extraction',
         'alias_generation','summarization','synthesis'));

DROP TABLE IF EXISTS okt_system.repository_report_settings;

ALTER TABLE okt_repository.report_annotations
    DROP COLUMN IF EXISTS posture;