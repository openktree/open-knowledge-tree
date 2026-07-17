CREATE TABLE IF NOT EXISTS okt_system.ai_usage (
    id          UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    task_id     TEXT,
    model       TEXT NOT NULL,
    provider    TEXT NOT NULL,
    prompt_tokens     INTEGER NOT NULL DEFAULT 0,
    completion_tokens INTEGER NOT NULL DEFAULT 0,
    total_tokens      INTEGER NOT NULL DEFAULT 0,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_ai_usage_task_id ON okt_system.ai_usage(task_id);
CREATE INDEX IF NOT EXISTS idx_ai_usage_provider ON okt_system.ai_usage(provider);
CREATE INDEX IF NOT EXISTS idx_ai_usage_model ON okt_system.ai_usage(model);
CREATE INDEX IF NOT EXISTS idx_ai_usage_created_at ON okt_system.ai_usage(created_at);
