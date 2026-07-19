-- 0050_repository_contributor.up.sql
--
-- Per-repository contributor identity for registry attribution. A
-- repo's decompositions pushed to the registry (contribute_source)
-- carry a contributor object so pulling repos can see who
-- contributed a source. By default every repo contributes
-- ANONYMOUSLY (contributor_anonymous = TRUE, display_name NULL);
-- an admin can opt out of anonymity and set a display name from the
-- repository settings page.
--
-- The columns are nullable / defaulted so the migration is
-- idempotent and back-fills existing rows: every existing repo
-- stays anonymous, matching the pre-migration behavior (the
-- contribute_source worker sent no contributor field at all).
--
-- The table lives in okt_system (the repositories table is there);
-- ALTER TABLE and the table are unqualified to match the pattern in
-- 0046/0047/0049 (sqlc's parser resolves via the runtime
-- search_path).

ALTER TABLE repositories
    ADD COLUMN IF NOT EXISTS contributor_display_name TEXT NULL,
    ADD COLUMN IF NOT EXISTS contributor_anonymous BOOLEAN NOT NULL DEFAULT TRUE;