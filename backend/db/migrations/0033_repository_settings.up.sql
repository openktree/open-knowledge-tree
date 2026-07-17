-- 0033_repository_settings.up.sql
--
-- Per-repository settings: which search/resolution providers a repo
-- allows, and which concept contexts (ontology class labels) the
-- concept-extraction prompt may assign. Both tables live in
-- okt_system next to `repositories` because they are repo metadata
-- (the system DB is always reachable from the HTTP layer and the
-- extract_concepts worker, which needs the allowed-context list
-- before it can resolve the per-repo pool).
--
-- `repository_provider_settings` is keyed by (provider_kind,
-- provider_id) so future providers only need a new row, not a schema
-- change. `provider_kind` is 'search' | 'resolution'. Stored rows
-- whose provider_id is no longer in the live registry are silently
-- ignored at enforcement time (the runtime intersects with the live
-- provider set), so a deployment that drops a provider doesn't leave
-- dangling state that breaks the gate.
--
-- `repository_contexts` is the per-repo allowed-context list. On
-- repo creation the backend bulk-inserts every label from the
-- embedded dbpedia_l3.json snapshot with is_custom=FALSE; admins add
-- custom rows (is_custom=TRUE) for enterprise/domain-specific
-- contexts (Product, Application, Role, ...). The concepts.context
-- column stays a free TEXT (no FK) so the existing worker/queries
-- keep working; consistency is enforced at the admin layer (a
-- migration target must be a row in this table).

CREATE TABLE IF NOT EXISTS okt_system.repository_provider_settings (
    repository_id UUID NOT NULL REFERENCES okt_system.repositories(id) ON DELETE CASCADE,
    provider_kind  TEXT NOT NULL CHECK (provider_kind IN ('search', 'resolution')),
    provider_id    TEXT NOT NULL,
    enabled        BOOLEAN NOT NULL DEFAULT TRUE,
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (repository_id, provider_kind, provider_id)
);

CREATE TABLE IF NOT EXISTS okt_system.repository_contexts (
    repository_id UUID    NOT NULL REFERENCES okt_system.repositories(id) ON DELETE CASCADE,
    context       TEXT    NOT NULL,
    is_custom     BOOLEAN NOT NULL DEFAULT FALSE,
    description   TEXT    NOT NULL DEFAULT '',
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Case-insensitive uniqueness on (repository_id, context) so an
-- admin adding "Product" can't also add "product" as a second row.
CREATE UNIQUE INDEX IF NOT EXISTS uq_repository_contexts_repo_context
    ON okt_system.repository_contexts (repository_id, lower(context));