-- promptsets.sql — user-defined promptset catalog (see migration 0046).
--
-- The table lives in okt_system (user-owned metadata). The PK is the
-- hash (sha256 of the 8 phase strings), so two promptsets with the
-- same prompts collapse to one row — the identity IS the philosophy.
-- The create handler computes the hash server-side and ignores any
-- client-supplied hash; ON CONFLICT DO UPDATE lets a re-create with
-- the same prompts rename + re-own the row.

-- name: GetPromptsetByHash :one
SELECT * FROM okt_system.promptsets WHERE hash = $1;

-- name: ListPromptsetsByOwner :many
-- Every promptset owned by a user, newest first. The handler
-- intersects this with the built-in promptset to build the full
-- catalog the UI offers.
SELECT * FROM okt_system.promptsets
WHERE owner_id = $1
ORDER BY created_at DESC;

-- name: ListAllPromptsets :many
-- Every promptset in the catalog. Used by the sysadmin view; the
-- per-user handler uses ListPromptsetsByOwner instead.
SELECT * FROM okt_system.promptsets
ORDER BY created_at DESC;

-- name: UpsertPromptset :one
-- Insert a new promptset, or — when the hash already exists (same 8
-- phase strings as an existing row) — update the name + owner + the
-- phase strings (which are necessarily equal, but bumping updated_at
-- is harmless) and return the row. The handler computes the hash
-- from the request body and passes it as $1; the ON CONFLICT target
-- is the PK (hash).
INSERT INTO okt_system.promptsets (
    hash, name, owner_id,
    fact_extraction, image_fact_extraction, concept_extraction,
    refinement, synthesis, image_picker, summarization, posture
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
ON CONFLICT (hash) DO UPDATE SET
    name                  = EXCLUDED.name,
    owner_id              = EXCLUDED.owner_id,
    fact_extraction       = EXCLUDED.fact_extraction,
    image_fact_extraction = EXCLUDED.image_fact_extraction,
    concept_extraction    = EXCLUDED.concept_extraction,
    refinement            = EXCLUDED.refinement,
    synthesis             = EXCLUDED.synthesis,
    image_picker          = EXCLUDED.image_picker,
    summarization         = EXCLUDED.summarization,
    posture               = EXCLUDED.posture,
    updated_at            = now()
RETURNING *;

-- name: DeletePromptset :exec
-- Remove a promptset by hash. The handler enforces ownership (only
-- the owner or a sysadmin may delete) before calling this. Deleting a
-- promptset does NOT cascade to repositories that reference it: the
-- per-repo active_promptset_hash / accepted_promptset_hashes
-- columns are plain TEXT / TEXT[] with no FK (the built-in promptset
-- is not a row, so an FK would force a sentinel row). A repo pointing
-- at a deleted promptset falls back to the global default at resolve
-- time, which is the safe behavior.
DELETE FROM okt_system.promptsets WHERE hash = $1;