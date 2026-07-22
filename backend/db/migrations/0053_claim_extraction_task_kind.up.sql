-- 0053_claim_extraction_task_kind.up.sql
--
-- The annotate_report worker now optionally runs a claim-extraction
-- pass before retrieval: a chat model reads each candidate sentence
-- and emits its verifiable assertions (numeric values, causal
-- claims, comparisons, quotations, definitions). The worker uses
-- the extracted claims to drive an additional retrieval pass per
-- claim (numeric → tsvector, prose → embedding on the claim term)
-- so the posture classifier sees facts that match the sentence's
-- SPECIFIC assertion, not just its broad topic.
--
-- This migration adds 'claim_extraction' to the
-- repository_model_settings task_kind CHECK so an operator can pin a
-- per-repo model for the claim extractor the same way the seven
-- existing generation tasks are pinned. The claim rows themselves
-- are ephemeral (computed inside the worker, never persisted) so no
-- new table is needed.

ALTER TABLE okt_system.repository_model_settings
    DROP CONSTRAINT IF EXISTS repository_model_settings_task_kind_check;
ALTER TABLE okt_system.repository_model_settings
    ADD CONSTRAINT repository_model_settings_task_kind_check
    CHECK (task_kind IN
        ('fact_extraction','image_extraction','concept_extraction',
         'alias_generation','summarization','synthesis',
         'report_annotation','claim_extraction'));