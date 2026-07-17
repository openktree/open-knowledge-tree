-- repository_settings.sql — per-repository provider + context settings.
--
-- These tables live in okt_system (repo metadata). The CreateRepository
-- handler seeds both on creation; the RepositorySettings handler reads
-- and mutates them; the extract_concepts worker reads the context list
-- to build the concept-extraction prompt; the gate helpers in the
-- source handler read the provider list to decide whether a search/
-- retrieve call is allowed for the active repo.

-- name: SeedRepositoryProviderSetting :one
-- Single-row seed used by CreateRepository when iterating the live
-- provider registry. ON CONFLICT DO NOTHING so a re-seed (e.g. a
-- retry after a partial failure) is idempotent.
INSERT INTO okt_system.repository_provider_settings (repository_id, provider_kind, provider_id, enabled)
VALUES ($1, $2, $3, $4)
ON CONFLICT (repository_id, provider_kind, provider_id) DO NOTHING
RETURNING *;

-- name: ListRepositoryProviderSettings :many
SELECT * FROM okt_system.repository_provider_settings
WHERE repository_id = $1
ORDER BY provider_kind, provider_id;

-- name: SetRepositoryProviderEnabled :one
-- Upsert the enabled flag for a (repo, kind, id) triple. Used by the
-- settings PUT endpoint. The unique target is the PK, so ON CONFLICT
-- DO UPDATE bumps enabled + updated_at.
INSERT INTO okt_system.repository_provider_settings (repository_id, provider_kind, provider_id, enabled)
VALUES ($1, $2, $3, $4)
ON CONFLICT (repository_id, provider_kind, provider_id)
DO UPDATE SET enabled = EXCLUDED.enabled, updated_at = now()
RETURNING *;

-- name: SeedRepositoryContext :one
-- Single-row seed for contexts. Used by CreateRepository for each
-- dbpedia label (is_custom=FALSE) and by the settings POST endpoint
-- for custom contexts (is_custom=TRUE). ON CONFLICT DO NOTHING.
INSERT INTO okt_system.repository_contexts (repository_id, context, is_custom, description)
VALUES ($1, $2, $3, $4)
ON CONFLICT (repository_id, lower(context)) DO NOTHING
RETURNING *;

-- name: UpsertRepositoryContext :one
-- Add or update a custom context's description. The settings PUT
-- endpoint edits description only (renaming a context is add-new +
-- migrate + delete-old). ON CONFLICT DO UPDATE sets description +
-- updated_at (is_custom is preserved so editing a dbpedia-derived
-- label's description doesn't flip its custom flag — an admin
-- annotating a dbpedia context keeps is_custom=FALSE).
INSERT INTO okt_system.repository_contexts (repository_id, context, is_custom, description)
VALUES ($1, $2, $3, $4)
ON CONFLICT (repository_id, lower(context))
DO UPDATE SET description = EXCLUDED.description, updated_at = now()
RETURNING *;

-- name: ListRepositoryContexts :many
SELECT * FROM okt_system.repository_contexts
WHERE repository_id = $1
ORDER BY is_custom, context;

-- name: GetRepositoryContext :one
SELECT * FROM okt_system.repository_contexts
WHERE repository_id = @repository_id AND lower(context) = lower(@context);

-- name: DeleteRepositoryContext :exec
DELETE FROM okt_system.repository_contexts
WHERE repository_id = @repository_id AND lower(context) = lower(@context);

-- name: ListEnabledRepositoryProviderIDs :many
-- The gate helper: returns every enabled (kind, id) pair for a repo.
-- The caller intersects with the live registry; orphans are filtered
-- out here implicitly because they're either disabled or absent.
SELECT provider_kind, provider_id
FROM okt_system.repository_provider_settings
WHERE repository_id = $1 AND enabled = TRUE
ORDER BY provider_kind, provider_id;

-- ──────────────────────────────────────────────────────────────
-- Context mappings (local ↔ registry). See migration 0038.
-- ──────────────────────────────────────────────────────────────

-- name: UpsertRepositoryContextMapping :one
-- Add or update a local→registry mapping. The unique target is
-- (repository_id, lower(local_context)) so a local context maps to
-- exactly one registry context; ON CONFLICT DO UPDATE swaps the
-- registry target + bumps updated_at. Used by the
-- SetContextMapping handler (PUT .../settings/context-mappings).
INSERT INTO okt_system.repository_context_mappings (repository_id, local_context, registry_context)
VALUES ($1, $2, $3)
ON CONFLICT (repository_id, lower(local_context))
DO UPDATE SET registry_context = EXCLUDED.registry_context, updated_at = now()
RETURNING *;

-- name: DeleteRepositoryContextMapping :exec
-- Remove a mapping by local context (case-insensitive). Used by the
-- DeleteContextMapping handler.
DELETE FROM okt_system.repository_context_mappings
WHERE repository_id = $1 AND lower(local_context) = lower($2);

-- name: ListRepositoryContextMappings :many
-- Every mapping row for a repo, ordered by local_context. Used by
-- the ListContextMappings handler and by the contribute/pull
-- workers to build their in-memory translation tables.
SELECT * FROM okt_system.repository_context_mappings
WHERE repository_id = $1
ORDER BY local_context;

-- name: GetRepositoryContextMappingByLocal :one
-- Outbound (contribute) lookup: local context → registry context.
-- Case-insensitive on the local label. Used by the contribute_source
-- worker.
SELECT * FROM okt_system.repository_context_mappings
WHERE repository_id = $1 AND lower(local_context) = lower($2);

-- name: GetRepositoryContextMappingByRegistry :one
-- Inbound (pull) lookup: registry context → local context.
-- Case-insensitive on the registry label. Used by the
-- pull_all_from_registry worker.
SELECT * FROM okt_system.repository_context_mappings
WHERE repository_id = $1 AND lower(registry_context) = lower($2);

-- name: SetUnmappedContextPolicy :exec
-- Update the per-repo pull policy for unmapped registry contexts.
-- Called by the SetUnmappedPolicy handler. catch_all_context is
-- NULL unless policy is 'catch_all'; the handler validates the
-- label is a member of repository_contexts before accepting it.
UPDATE repositories
SET unmapped_context_policy = $2,
    catch_all_context = $3,
    updated_at = now()
WHERE id = $1;

-- name: GetUnmappedContextPolicy :one
-- Combined policy + catch_all_context lookup. Read by the
-- pull_all_from_registry worker (once per repo per Work call) and
-- by the ListContextMappings handler to surface the current policy
-- in the settings UI.
SELECT unmapped_context_policy, catch_all_context FROM repositories WHERE id = $1;

-- ──────────────────────────────────────────────────────────────
-- Per-repository model selection (see migration 0039).
-- ──────────────────────────────────────────────────────────────

-- name: GetRepositoryModelSetting :one
-- Single (task_kind, model_id) row for a repo. model_id is NULL
-- when the repo inherits the global config default for this task.
-- Read by the ModelResolver at Work() start to decide which model
-- + provider a worker should use for this repo.
SELECT * FROM okt_system.repository_model_settings
WHERE repository_id = $1 AND task_kind = $2;

-- name: ListRepositoryModelSettings :many
-- Every (task_kind, model_id) override for a repo. Used by
-- GetSettings to surface the per-task model selections in the UI.
SELECT * FROM okt_system.repository_model_settings
WHERE repository_id = $1
ORDER BY task_kind;

-- name: UpsertRepositoryModelSetting :one
-- Upsert a per-repo model override for a task kind. model_id may
-- be NULL (clear → inherit global default). Called by the
-- SetModelSetting handler (PUT .../settings/models).
INSERT INTO okt_system.repository_model_settings (repository_id, task_kind, model_id)
VALUES ($1, $2, $3)
ON CONFLICT (repository_id, task_kind)
DO UPDATE SET model_id = EXCLUDED.model_id, updated_at = now()
RETURNING *;

-- name: DeleteRepositoryModelSetting :exec
-- Clear a per-repo model override (revert to inheriting the global
-- default). Called when the UI sets model_id to empty/null.
DELETE FROM okt_system.repository_model_settings
WHERE repository_id = $1 AND task_kind = $2;

-- name: GetRepositoryReportSettings :one
-- Per-repository report annotation settings. Returns the single row
-- for this repo; absence returns sql.ErrNoRows and the caller falls
-- back to the global config defaults (threshold = 0.84, classifier
-- enabled = true). The annotate_report worker reads this before
-- resolving the per-repo pool.
SELECT * FROM okt_system.repository_report_settings
WHERE repository_id = $1;

-- name: UpsertRepositoryReportSettings :one
-- Insert or update the per-repo report annotation settings. Pass
-- similarity_threshold NULL to inherit the global default; pass
-- posture_classifier_enabled false to turn the LLM step off for
-- this repo without touching the global config.
INSERT INTO okt_system.repository_report_settings
    (repository_id, similarity_threshold, posture_classifier_enabled)
VALUES ($1, $2, $3)
ON CONFLICT (repository_id)
DO UPDATE SET
    similarity_threshold      = EXCLUDED.similarity_threshold,
    posture_classifier_enabled = EXCLUDED.posture_classifier_enabled,
    updated_at = now()
RETURNING *;

-- name: DeleteRepositoryReportSettings :exec
-- Drop the per-repo override row so the repo inherits the global
-- defaults again.
DELETE FROM okt_system.repository_report_settings
WHERE repository_id = $1;