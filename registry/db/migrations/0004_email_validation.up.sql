-- 0004_email_validation.up.sql
--
-- Account email validation. Adds two things:
--
--   1. users.email_verified (BOOLEAN NOT NULL DEFAULT false) so
--      Login can refuse unverified accounts when
--      email_validation.enable_validation is on. The default
--      false keeps existing rows unverified; an operator who
--      flips enable_validation on after the fact is expected to
--      manually mark existing trusted users verified (or just
--      re-register them — there's no admin-only verify endpoint
--      in the KISS cut).
--
--   2. email_verifications, the per-user pending-verification
--      row. PK is user_id (one outstanding token per user; a
--      resend overwrites the row). token_hash is the sha256 of
--      the random token the user receives in the email link, so
--      the DB never stores the raw token (same pattern as
--      api_tokens.token_hash). expires_at drives the 410 on
--      VerifyEmail.
--
-- Idempotent: CREATE TABLE IF NOT EXISTS + the ensureColumn
-- pattern in the SQLite store mirrors this for the embedded
-- migration. ALTER TABLE ADD COLUMN lacks IF NOT EXISTS on older
-- SQLite, so the Go-side ensureColumn helper is the source of
-- truth for the column add; the Postgres path uses ADD COLUMN IF
-- NOT EXISTS here.

ALTER TABLE users ADD COLUMN IF NOT EXISTS email_verified BOOLEAN NOT NULL DEFAULT false;

CREATE TABLE IF NOT EXISTS email_verifications (
    user_id     TEXT PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
    token_hash  TEXT NOT NULL,
    expires_at  TIMESTAMPTZ NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_email_verifications_expires ON email_verifications(expires_at);