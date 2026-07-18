-- 0046_promptsets.up.sql
--
-- User-defined promptsets. A promptset is the complete set of phase
-- prompts an OKT instance (or a repository) uses to decompose, refine,
-- summarize, synthesize, and classify facts/concepts. The hash of the
-- 8 phase strings is the identity (two promptsets with the same prompts
-- are the same philosophy), so the PK is the hash, not a surrogate id.
--
-- OwnerID is the user who created the promptset; a user can assign any
-- of their own promptsets (plus the built-in) to repositories they
-- manage. The built-in promptset is NOT stored here — it is compiled
-- into the binary (see internal/promptset.Default) and resolved by the
-- BuiltinProvider, so its hash never appears in this table.
--
-- The phase columns are NOT NULL: a promptset missing a phase would
-- silently fall back to the built-in at runtime and produce a hash that
-- doesn't match the actual prompts used, so the create handler rejects
-- incomplete promptsets before they reach this table.
--
-- The table lives in okt_system (it is user-owned metadata, not
-- per-repo data). ALTER TABLE and the table are unqualified to match
-- the pattern in 0033/0038/0039 (sqlc's parser resolves via the runtime
-- search_path).

CREATE TABLE IF NOT EXISTS okt_system.promptsets (
    hash                  TEXT PRIMARY KEY,
    name                  TEXT NOT NULL,
    owner_id              UUID REFERENCES okt_system.users(id) ON DELETE CASCADE,
    fact_extraction       TEXT NOT NULL,
    image_fact_extraction TEXT NOT NULL,
    concept_extraction    TEXT NOT NULL,
    refinement            TEXT NOT NULL,
    synthesis             TEXT NOT NULL,
    image_picker          TEXT NOT NULL,
    summarization         TEXT NOT NULL,
    posture               TEXT NOT NULL,
    created_at            TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at            TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_promptsets_owner ON okt_system.promptsets(owner_id);