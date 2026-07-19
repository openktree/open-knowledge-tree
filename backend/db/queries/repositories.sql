-- name: CreateRepository :one
INSERT INTO repositories (name, slug, description, owner_id, database_name, tier)
VALUES ($1, $2, $3, $4, $5, $6)
RETURNING *;

-- name: GetRepositoryByID :one
SELECT * FROM repositories WHERE id = $1;

-- name: GetRepositoryDatabaseName :one
SELECT database_name FROM repositories WHERE id = $1;

-- name: GetRepositoryRegistryID :one
SELECT registry_id FROM repositories WHERE id = $1;

-- name: GetRepositoryRegistryEnabled :one
-- Single-column lookup of the registry_enabled flag. Read by the
-- retrieve_source worker (to skip the cache lookup) and the remote
-- handler (to 503 the /remote endpoints) for a repo that has turned
-- the integration off.
SELECT registry_enabled FROM repositories WHERE id = $1;

-- name: GetRepositoryRegistryConfig :one
-- Combined registry_id + registry_enabled lookup in one round-trip.
-- Used by the workers and handlers that need both values to decide
-- which client to use and whether the integration is on for this repo.
SELECT registry_id, registry_enabled FROM repositories WHERE id = $1;

-- name: SetRepositoryRegistryID :exec
-- Update the per-repo registry selector. Called by the
-- SetRegistry handler (PUT .../settings/registry). The id is
-- validated against the configured registries list in the handler,
-- not via a foreign key (registries are config, not a DB table).
UPDATE repositories
SET registry_id = $2, updated_at = now()
WHERE id = $1;

-- name: SetRepositoryRegistryEnabled :exec
-- Toggle the per-repo registry integration. Called by the
-- SetRegistry handler. When FALSE, the retrieve_source worker
-- skips the cache lookup and the /remote endpoints return 503.
UPDATE repositories
SET registry_enabled = $2, updated_at = now()
WHERE id = $1;

-- name: GetRepositorySlug :one
-- Single-column lookup for the slug. Used by the
-- source_decomposition worker to synthesize a service-routable
-- image_url for image facts whose source image has no remote URL
-- (PDF page renders stored under storage_key). Cheaper than
-- fetching the whole row when only the slug is needed.
SELECT slug FROM repositories WHERE id = $1;

-- name: GetRepositoryBySlug :one
SELECT * FROM repositories WHERE slug = $1;

-- name: ListRepositoriesByOwner :many
SELECT * FROM repositories WHERE owner_id = $1 ORDER BY created_at DESC;

-- name: ListAllRepositories :many
SELECT * FROM repositories ORDER BY created_at DESC;

-- name: UpdateRepository :one
UPDATE repositories
SET name = $2, description = $3, updated_at = now()
WHERE id = $1
RETURNING *;

-- name: GetRepositoryAutoContribute :one
-- Single-column lookup of the auto_contribute flag. Read by the
-- cleanup_facts worker to decide whether to chain contribute_source
-- after a source finishes the ingestion pipeline, and by the
-- settings handler to surface the flag in GetSettings.
SELECT auto_contribute FROM repositories WHERE id = $1;

-- name: SetRepositoryAutoContribute :exec
-- Upsert the auto_contribute flag for a repo. Called by the
-- SetAutoContribute handler (PUT .../settings/auto-contribute).
UPDATE repositories
SET auto_contribute = $2, updated_at = now()
WHERE id = $1;

-- name: GetRepositoryAllowedModels :one
-- Per-repo model whitelist for the registry cache import. NULL
-- means "inherit the global registry.allowed_models config"; a
-- non-NULL array replaces the global list for this repo (per-repo
-- replaces global). Read by the retrieve_source / pull_all workers
-- to decide which decomposition models to import.
SELECT allowed_models FROM repositories WHERE id = $1;

-- name: SetRepositoryAllowedModels :exec
-- Upsert the per-repo allowed_models list. Called by the
-- SetRegistrySettings handler (PUT .../settings/registry) when the
-- body carries an allowed_models field. Pass NULL to clear the
-- per-repo override (revert to inheriting the global config); pass
-- an array (incl. ["*"] or []) to set the per-repo whitelist.
UPDATE repositories
SET allowed_models = $2, updated_at = now()
WHERE id = $1;

-- name: GetRepositorySyncLevels :one
-- Combined push_level + pull_level lookup in one round-trip. Read by
-- the contribute_source worker (push_level) and the pull workers
-- (pull_level) to decide whether to include concepts/links/concept
-- embeddings in the contribution or import. Defaults to 'concepts'
-- (migration 0044) so existing repos keep full sync behavior.
SELECT registry_push_level, registry_pull_level FROM repositories WHERE id = $1;

-- name: GetRepositoryPromptset :one
-- Combined active_promptset_hash + accepted_promptset_hashes lookup
-- in one round-trip. Read by the promptset resolver at Work() start
-- to decide which promptset a repo's decompositions run under, and
-- by the registry pull worker to filter remote decompositions to
-- those whose promptset_hash is in the accepted set. active_hash is
-- NULL when the repo inherits the global config default; the resolver
-- falls back to cfg.Providers.PromptsetDefault then to the built-in
-- hash. accepted_hashes defaults to '{}' meaning "only the active
-- hash is accepted" (see migration 0047).
SELECT active_promptset_hash, accepted_promptset_hashes FROM repositories WHERE id = $1;

-- name: SetRepositoryPromptset :exec
-- Update the per-repo promptset selection. Called by the
-- SetPromptset handler (PUT .../settings/promptset). The handler
-- validates active_hash against the resolver (built-in or a promptset
-- the user owns / is public) and validates every entry in
-- accepted_hashes the same way before this call. active_hash may be
-- NULL (clear → inherit global default); accepted_hashes may be NULL
-- (clear → only the active hash is accepted) or an array.
UPDATE repositories
SET active_promptset_hash = $2,
    accepted_promptset_hashes = $3,
    updated_at = now()
WHERE id = $1;

-- name: SetRepositorySyncLevels :exec
-- Update the per-repo push/pull sync levels. Called by the
-- SetSyncLevels handler (PUT .../settings/sync-levels). Each level is
-- validated against the CHECK constraint ('facts' | 'concepts') in
-- the handler before this call; passing an invalid value here would
-- fail the constraint and surface as a 500.
UPDATE repositories
SET registry_push_level = $2, registry_pull_level = $3, updated_at = now()
WHERE id = $1;

-- name: GetRepositoryAllowedContentTypes :one
-- Per-repo allowed content types gate (migration 0049). NULL means
-- "allow all" (the default, backward compatible for existing repos);
-- a non-NULL array restricts to the listed kinds ("document", "url",
-- "doi"). Read by the CreateSource / UploadSource / EnqueueRetrieveSource
-- handlers to 403-reject disallowed content types.
SELECT allowed_content_types FROM repositories WHERE id = $1;

-- name: SetRepositoryAllowedContentTypes :exec
-- Upsert the per-repo allowed content types list. Called by the
-- SetContentTypes handler (PUT .../settings/content-types). Pass NULL
-- to clear the per-repo override (revert to allow-all); pass an array
-- (e.g. ["doi"] or ["document","url","doi"]) to restrict. The handler
-- validates each value against {"document","url","doi"} before this
-- call; the CHECK constraint on the column is the defense-in-depth.
UPDATE repositories
SET allowed_content_types = $2, updated_at = now()
WHERE id = $1;

-- name: DeleteRepository :one
DELETE FROM repositories WHERE id = $1 RETURNING *;

-- name: GetRepositoryContributor :one
-- Single-row lookup of the per-repo contributor identity
-- (display_name + anonymous flag). Read by contribute_source to
-- decide what to send on PushSource: when anonymous=true the
-- worker sends display_name="" + anonymous=true (the registry's
-- canonical "anonymous" marker); when anonymous=false it sends
-- the stored display_name so pulling repos can see who
-- contributed the source. Defaults to (NULL, TRUE) for repos
-- that haven't configured attribution (the migration's back-fill).
SELECT contributor_display_name, contributor_anonymous FROM repositories WHERE id = $1;

-- name: SetRepositoryContributor :exec
-- Upsert the per-repo contributor identity. Called by the
-- SetContributor handler (PUT .../settings/contributor). The
-- handler validates the combination before this call: when
-- anonymous=TRUE the display_name MUST be NULL/empty (the handler
-- passes NULL so the column stays clean); when anonymous=FALSE
-- the display_name MUST be a non-empty trimmed string of <=120
-- chars. Omitted fields are not supported here — the handler
-- resolves "keep current" before calling this, so the SQL always
-- writes both columns.
UPDATE repositories
SET contributor_display_name = $2,
    contributor_anonymous = $3,
    updated_at = now()
WHERE id = $1;
