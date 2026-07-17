CREATE TABLE IF NOT EXISTS repositories (
    id          TEXT PRIMARY KEY,
    name        TEXT NOT NULL,
    description TEXT NOT NULL DEFAULT '',
    owner       TEXT NOT NULL DEFAULT '',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS sources (
    id          TEXT PRIMARY KEY,
    repo_id     TEXT NOT NULL REFERENCES repositories(id),
    url         TEXT,
    doi         TEXT,
    sha256      TEXT,
    title       TEXT,
    s3_key      TEXT NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE(url),
    UNIQUE(doi)
);

CREATE INDEX IF NOT EXISTS idx_sources_repo ON sources(repo_id);
CREATE INDEX IF NOT EXISTS idx_sources_sha256 ON sources(sha256);

CREATE TABLE IF NOT EXISTS decompositions (
    id              TEXT PRIMARY KEY,
    source_id       TEXT NOT NULL REFERENCES sources(id),
    model_id        TEXT NOT NULL,
    decomposed_by   TEXT NOT NULL DEFAULT '',
    decomposed_at   TIMESTAMPTZ,
    fact_count      INTEGER NOT NULL DEFAULT 0,
    summary_count   INTEGER NOT NULL DEFAULT 0,
    has_embeddings  BOOLEAN NOT NULL DEFAULT false,
    embedding_model TEXT,
    embedding_dims  INTEGER NOT NULL DEFAULT 0,
    s3_key          TEXT NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE(source_id, model_id)
);

CREATE INDEX IF NOT EXISTS idx_decompositions_source ON decompositions(source_id);
CREATE INDEX IF NOT EXISTS idx_decompositions_model ON decompositions(model_id);

CREATE TABLE IF NOT EXISTS fact_hashes (
    content_hash     TEXT NOT NULL,
    source_id        TEXT NOT NULL,
    decomposition_id TEXT NOT NULL,
    fact_id          TEXT NOT NULL,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (content_hash, source_id, decomposition_id)
);

CREATE INDEX IF NOT EXISTS idx_fact_hashes_source ON fact_hashes(source_id, content_hash);

CREATE TABLE IF NOT EXISTS contexts (
    label       TEXT PRIMARY KEY,
    description TEXT NOT NULL DEFAULT '',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_contexts_label ON contexts(label);

CREATE TABLE IF NOT EXISTS users (
    id            TEXT PRIMARY KEY,
    email         TEXT NOT NULL UNIQUE,
    password_hash TEXT NOT NULL,
    display_name  TEXT NOT NULL DEFAULT '',
    role          TEXT NOT NULL DEFAULT 'viewer',
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS api_tokens (
    id          TEXT PRIMARY KEY,
    user_id     TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    name        TEXT NOT NULL,
    token_hash  TEXT NOT NULL UNIQUE,
    scope       TEXT NOT NULL DEFAULT 'read',
    expires_at  TIMESTAMPTZ,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_api_tokens_user ON api_tokens(user_id);
