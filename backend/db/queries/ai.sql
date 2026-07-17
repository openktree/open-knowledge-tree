-- name: RecordAIUsage :exec
INSERT INTO okt_system.ai_usage (
    task_id, model, provider,
    prompt_tokens, completion_tokens, total_tokens,
    repository_id, source_id, operation
)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9);

-- name: ListAIUsageByTask :many
SELECT * FROM okt_system.ai_usage
WHERE task_id = $1
ORDER BY created_at DESC;

-- name: ListAIUsageByProvider :many
SELECT * FROM okt_system.ai_usage
WHERE provider = $1
ORDER BY created_at DESC
LIMIT $2;

-- name: ListAIUsageByModel :many
SELECT * FROM okt_system.ai_usage
WHERE model = $1
ORDER BY created_at DESC
LIMIT $2;

-- name: GetAIUsageSummary :many
SELECT
    provider,
    model,
    COUNT(*) AS request_count,
    SUM(prompt_tokens) AS total_prompt_tokens,
    SUM(completion_tokens) AS total_completion_tokens,
    SUM(total_tokens) AS total_tokens
FROM okt_system.ai_usage
GROUP BY provider, model
ORDER BY total_tokens DESC;

-- Dashboard queries. The dashboard fetches several rollups in
-- parallel; each query takes an optional date range (from / to)
-- and an optional repository_id filter so the UI can scope to a
-- single repo. sqlc.narg marks the param as nullable so a NULL
-- value matches "no filter" without a separate query.

-- name: GetAIUsageSummaryInRange :many
SELECT
    provider,
    model,
    operation,
    COUNT(*) AS request_count,
    SUM(prompt_tokens) AS total_prompt_tokens,
    SUM(completion_tokens) AS total_completion_tokens,
    SUM(total_tokens) AS total_tokens
FROM okt_system.ai_usage
WHERE (sqlc.narg('from')::timestamptz IS NULL OR created_at >= sqlc.narg('from'))
  AND (sqlc.narg('to')::timestamptz IS NULL OR created_at < sqlc.narg('to'))
  AND (sqlc.narg('repository_id')::uuid IS NULL OR repository_id = sqlc.narg('repository_id'))
GROUP BY provider, model, operation
ORDER BY total_tokens DESC;

-- name: GetAIUsageByDay :many
-- Time-bucketed consumption for the over-time chart. The bucket
-- width (day/week/month) is passed as a string param to date_trunc;
-- sqlc cannot parameterize identifiers, so the literal is bound at
-- exec time via the Bucket param.
SELECT
    date_trunc(sqlc.arg('bucket'), created_at)::timestamptz AS bucket,
    model,
    SUM(prompt_tokens) AS total_prompt_tokens,
    SUM(completion_tokens) AS total_completion_tokens,
    SUM(total_tokens) AS total_tokens,
    COUNT(*) AS request_count
FROM okt_system.ai_usage
WHERE (sqlc.narg('from')::timestamptz IS NULL OR created_at >= sqlc.narg('from'))
  AND (sqlc.narg('to')::timestamptz IS NULL OR created_at < sqlc.narg('to'))
  AND (sqlc.narg('repository_id')::uuid IS NULL OR repository_id = sqlc.narg('repository_id'))
GROUP BY bucket, model
ORDER BY bucket, model;

-- name: GetAIUsageByOperation :many
SELECT
    operation,
    model,
    COUNT(*) AS request_count,
    SUM(prompt_tokens) AS total_prompt_tokens,
    SUM(completion_tokens) AS total_completion_tokens,
    SUM(total_tokens) AS total_tokens
FROM okt_system.ai_usage
WHERE (sqlc.narg('from')::timestamptz IS NULL OR created_at >= sqlc.narg('from'))
  AND (sqlc.narg('to')::timestamptz IS NULL OR created_at < sqlc.narg('to'))
  AND (sqlc.narg('repository_id')::uuid IS NULL OR repository_id = sqlc.narg('repository_id'))
GROUP BY operation, model
ORDER BY total_tokens DESC;

-- name: GetAIUsageByRepository :many
SELECT
    repository_id,
    model,
    COUNT(*) AS request_count,
    SUM(prompt_tokens) AS total_prompt_tokens,
    SUM(completion_tokens) AS total_completion_tokens,
    SUM(total_tokens) AS total_tokens
FROM okt_system.ai_usage
WHERE (sqlc.narg('from')::timestamptz IS NULL OR created_at >= sqlc.narg('from'))
  AND (sqlc.narg('to')::timestamptz IS NULL OR created_at < sqlc.narg('to'))
  AND (sqlc.narg('repository_id')::uuid IS NULL OR repository_id = sqlc.narg('repository_id'))
GROUP BY repository_id, model
ORDER BY total_tokens DESC;

-- name: GetAIUsageBySource :many
SELECT
    source_id,
    repository_id,
    model,
    COUNT(*) AS request_count,
    SUM(prompt_tokens) AS total_prompt_tokens,
    SUM(completion_tokens) AS total_completion_tokens,
    SUM(total_tokens) AS total_tokens
FROM okt_system.ai_usage
WHERE (sqlc.narg('from')::timestamptz IS NULL OR created_at >= sqlc.narg('from'))
  AND (sqlc.narg('to')::timestamptz IS NULL OR created_at < sqlc.narg('to'))
  AND (sqlc.narg('repository_id')::uuid IS NULL OR repository_id = sqlc.narg('repository_id'))
GROUP BY source_id, repository_id, model
ORDER BY total_tokens DESC;