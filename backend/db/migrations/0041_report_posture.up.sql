-- 0041_report_posture.up.sql
--
-- Autocite posture classifier: the annotate_report worker now passes
-- each (sentence, candidate fact) pair through an LLM that labels
-- the relationship as one of:
--   'related'     — the fact is topically relevant but neither
--                   supports nor contradicts the sentence;
--   'supports'    — the fact provides evidence for the sentence's
--                   claim;
--   'contradicts'— the fact provides evidence against the claim.
-- Pairs the LLM judges 'irrelevant' are dropped before persistence,
-- so they never appear as annotations. posture is NULL on rows
-- produced before the classifier existed (or when the chat/AI
-- provider is not configured and the worker falls back to the
-- keep-all behavior). The CHECK constraint mirrors the enum: NULL
-- (legacy/fallback) plus the three persisted postures.

ALTER TABLE okt_repository.report_annotations
    ADD COLUMN IF NOT EXISTS posture TEXT
    CHECK (posture IN ('related','supports','contradicts'));

-- Per-repository report annotation settings. Lives in okt_system
-- next to repository_model_settings / repository_provider_settings
-- because it is repo metadata the worker reads before resolving
-- the per-repo pool. One row per repository (absence = inherit
-- global defaults). similarity_threshold NULL = inherit the global
-- reports.similarity_threshold (0.84). posture_classifier_enabled
-- lets an operator disable the LLM step for a single repo (e.g.
-- cost control) without touching the global config.

CREATE TABLE IF NOT EXISTS okt_system.repository_report_settings (
    repository_id             UUID NOT NULL REFERENCES okt_system.repositories(id) ON DELETE CASCADE,
    similarity_threshold      DOUBLE PRECISION,
    posture_classifier_enabled BOOLEAN NOT NULL DEFAULT TRUE,
    updated_at               TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (repository_id)
);

-- Extend the repository_model_settings task_kind CHECK to admit the
-- new 'report_annotation' task kind so an operator can pin a per-repo
-- model for the posture classifier the same way the six existing
-- generation tasks are pinned. ALTER the constraint in place: drop
-- the old one and recreate with the extended list (the table is
-- small and unqualified so the runtime search_path resolves it).
ALTER TABLE okt_system.repository_model_settings
    DROP CONSTRAINT IF EXISTS repository_model_settings_task_kind_check;
ALTER TABLE okt_system.repository_model_settings
    ADD CONSTRAINT repository_model_settings_task_kind_check
    CHECK (task_kind IN
        ('fact_extraction','image_extraction','concept_extraction',
         'alias_generation','summarization','synthesis',
         'report_annotation'));