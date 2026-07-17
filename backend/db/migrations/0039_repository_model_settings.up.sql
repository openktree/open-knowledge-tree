-- 0039_repository_model_settings.up.sql
--
-- Per-repository model selection per task. Each row stores a
-- per-repo override of which AI model runs a given task kind.
-- task_kind is one of the six generation tasks:
--   fact_extraction, image_extraction, concept_extraction,
--   alias_generation, summarization, synthesis.
-- Embedding is deliberately excluded (dimension-specific, mixing
-- breaks vector search).
--
-- model_id is NULL when the repo inherits the global config default
-- for that task; a non-empty model_id must be in the deployment's
-- ai.models catalog (validated at the API layer). One row per
-- (repository_id, task_kind); absence = inherit global default.
--
-- Note: the table lives in okt_system (repo metadata). ALTER TABLE
-- and the table are unqualified to match the pattern in
-- 0033/0038 (sqlc's parser resolves via the runtime search_path).

CREATE TABLE IF NOT EXISTS okt_system.repository_model_settings (
    repository_id UUID NOT NULL REFERENCES okt_system.repositories(id) ON DELETE CASCADE,
    task_kind     TEXT NOT NULL CHECK (task_kind IN
        ('fact_extraction','image_extraction','concept_extraction',
         'alias_generation','summarization','synthesis')),
    model_id      TEXT,
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (repository_id, task_kind)
);