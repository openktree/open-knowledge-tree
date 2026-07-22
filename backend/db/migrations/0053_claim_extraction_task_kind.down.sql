-- 0053_claim_extraction_task_kind.down.sql
--
-- Reverses 0053_claim_extraction_task_kind.up.sql. Removes
-- 'claim_extraction' from the repository_model_settings task_kind
-- CHECK. Existing rows with task_kind='claim_extraction' should be
-- deleted by the operator before running this down migration
-- (otherwise the constraint re-addition will fail).

DELETE FROM okt_system.repository_model_settings
WHERE task_kind = 'claim_extraction';

ALTER TABLE okt_system.repository_model_settings
    DROP CONSTRAINT IF EXISTS repository_model_settings_task_kind_check;
ALTER TABLE okt_system.repository_model_settings
    ADD CONSTRAINT repository_model_settings_task_kind_check
    CHECK (task_kind IN
        ('fact_extraction','image_extraction','concept_extraction',
         'alias_generation','summarization','synthesis',
         'report_annotation'));