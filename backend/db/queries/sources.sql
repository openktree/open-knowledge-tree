-- name: CreateSource :one
INSERT INTO okt_repository.sources (id, repository_id, url, kind, status, doi)
VALUES ($1, $2, $3, $4, $5, $6)
RETURNING *;

-- name: GetSourceByID :one
SELECT * FROM okt_repository.sources WHERE id = $1;

-- name: ListSourcesByRepo :many
-- Paginated, searchable repo-level source list. The optional
-- `$2` (search) is run through websearch_to_tsquery so the UI
-- search box supports quoted phrases, OR, and -exclude exactly
-- the way a web search box would. An empty search string matches
-- every row (the `@@` predicate is short-circuited by the
-- `'' OR ...` guard). Ordered by created_at DESC to match the
-- previous unpaginated behavior; LIMIT/OFFSET apply on top.
SELECT * FROM okt_repository.sources
WHERE repository_id = $1
  AND ($2::text = '' OR search_tsv @@ websearch_to_tsquery('english', $2))
ORDER BY created_at DESC
LIMIT $3 OFFSET $4;

-- name: CountSourcesByRepo :one
-- Companion count for ListSourcesByRepo so the API envelope can
-- return `total` without a window-function COUNT(*) OVER () on
-- the list query (which would force Postgres to materialize the
-- whole result set before applying LIMIT). Same WHERE clause as
-- the list query, minus the ORDER BY.
SELECT COUNT(*) FROM okt_repository.sources
WHERE repository_id = $1
  AND ($2::text = '' OR search_tsv @@ websearch_to_tsquery('english', $2));

-- name: GetSourceByRepoAndURL :one
-- Focused lookup used by the RetrieveSource worker's
-- unique-violation fallback path. The (repository_id, url) UNIQUE
-- constraint guarantees at most one row, so this is cheaper than
-- the previous "ListSourcesByRepo + filter in Go" approach and
-- keeps the worker independent of the HTTP pagination query.
SELECT * FROM okt_repository.sources
WHERE repository_id = $1 AND url = $2;

-- name: GetExistingSourceURLsAndDOIsByRepo :many
-- Batched existence lookup used by the TestSearch HTTP handler to
-- tag search results with `already_exists` / `existing_status`. The
-- caller passes the URL set and the (non-empty) DOI set from the
-- current search page; the query returns the subset of rows that
-- already exist in the repository, matched on either URL or DOI.
-- A row matches when its `url` is in the URL set OR its `doi` is in
-- the DOI set (the OR is the whole point: it catches the case where
-- the same paper was fetched via a different URL — e.g. doi.org vs
-- a publisher landing page — but the stored DOI agrees). Pass empty
-- arrays when the search page carried no DOIs (most Serper pages);
-- the ANY() on an empty array matches nothing, which is correct.
SELECT url, doi, status
FROM okt_repository.sources
WHERE repository_id = $1
  AND (url = ANY($2::text[]) OR doi = ANY($3::text[]));

-- name: UpdateSourceStatus :one
UPDATE okt_repository.sources
SET status = $2, updated_at = now()
WHERE id = $1
RETURNING *;

-- name: UpdateSourceDoi :one
-- Records the bare DOI the classifier or the search result
-- click-through discovered for this source. Called by the
-- RetrieveSource worker so the row carries the DOI even when
-- the user fetched a non-DOI URL (e.g. an openalex.org/W…
-- landing page that the search provider knows resolves to
-- a DOI). DOI is nullable in the table; passing NULL is a
-- no-op for non-scholarly sources.
UPDATE okt_repository.sources
SET doi = $2, updated_at = now()
WHERE id = $1
RETURNING *;

-- name: UpdateSourcePublishedAt :one
-- Records the publication date the search-result
-- click-through or any future early-stage pipeline
-- surfaced, before the parsing step has had a chance
-- to run. The worker's publish-on-create path
-- (CreateSource) does not accept a date because the
-- `sources` insert is a single statement and we want
-- to keep the new column out of the hot create path;
-- this query is the small backfill the worker does
-- after a successful CreateSource or a unique-violation
-- fallback. NULL is a no-op: the worker never wants to
-- clear a date it didn't set itself.
UPDATE okt_repository.sources
SET published_at = $2, updated_at = now()
WHERE id = $1
RETURNING *;

-- name: MarkSourceFetching :one
-- Flips status to 'fetching' and refreshes updated_at. The
-- worker calls this as the first step so the UI can show
-- the row is in flight before the network round trip
-- completes.
UPDATE okt_repository.sources
SET status = 'fetching', updated_at = now()
WHERE id = $1
RETURNING *;

-- name: MarkSourceFetched :one
-- Persists the body the fetch strategy returned plus the
-- final status / fetch timestamp. The worker calls this
-- on success. content is nullable so a future caller that
-- only wants to record the fetch metadata can pass NULL.
UPDATE okt_repository.sources
SET status     = 'fetched',
    content    = $2,
    fetched_at = now(),
    error      = NULL,
    updated_at = now()
WHERE id = $1
RETURNING *;

-- name: MarkSourceFailed :one
-- Records the worker's error string and bumps the row to
-- 'failed'. fetched_at is also bumped so the UI can show
-- when the attempt completed.
UPDATE okt_repository.sources
SET status     = 'failed',
    error      = $2,
    fetched_at = now(),
    updated_at = now()
WHERE id = $1
RETURNING *;

-- name: DeleteSource :exec
DELETE FROM okt_repository.sources WHERE id = $1;

-- name: MarkSourceParsed :one
-- Persists the content_parsing.ParsedDoc fields on the
-- source row. Called by the worker after MarkSourceFetched
-- (or MarkSourceFailed). The parsed_* columns are nullable
-- individually so a partial parse — for example a parser
-- that returned a title but no body — is representable.
-- parse_status is the only required field: it tells the
-- UI whether to surface a parsed view, hide it, or show
-- an error placeholder. Pass NULL for parsed_* when
-- parse_status = 'failed' or 'unsupported'.
--
-- published_at is the only field on this query that is
-- NOT overwritten on every re-parse. The worker
-- intentionally leaves NULL alone: a re-parse that didn't
-- surface a date must not erase a date the caller (or an
-- earlier parse) had already set. Callers that want to
-- clear the date pass NULL deliberately.
UPDATE okt_repository.sources
SET parsed_title    = $2,
    parsed_text     = $3,
    parsed_html     = $4,
    parsed_markdown = $5,
    parsed_author   = $6,
    parsed_sitename = $7,
    parsed_language = $8,
    published_at    = COALESCE($10, published_at),
    parsed_at       = now(),
    parse_status    = $9,
    updated_at      = now()
WHERE id = $1
RETURNING *;

-- name: AddSourceImage :one
-- Inserts one image row. The CHECK constraints on
-- okt_repository.source_images enforce the (kind, page_number,
-- url) shape so the worker doesn't have to.
INSERT INTO okt_repository.source_images (
    source_id, kind, page_number, position, url, width, height, bytes, local_path, alt_text
) VALUES (
    $1, $2, $3, $4, $5, $6, $7, $8, $9, $10
)
RETURNING *;

-- name: ListSourceImages :many
-- Returns every image for a source in display order
-- (page renders first by page number, then inline images
-- by DOM position). The composite index on
-- (source_id, kind, page_number NULLS LAST, position)
-- covers this query without a sort.
SELECT * FROM okt_repository.source_images
WHERE source_id = $1
ORDER BY
    CASE kind WHEN 'page' THEN 0 ELSE 1 END,
    page_number NULLS LAST,
    position;

-- name: ClearSourceImages :exec
-- Wipes every image row for a source. Called by the
-- worker on re-parse: a fresh parse replaces the image
-- set wholesale rather than merging, so deleted inline
-- images disappear from the row.
DELETE FROM okt_repository.source_images
WHERE source_id = $1;

-- name: MarkSourceImageStored :one
-- Records that the storage backend has persisted this image's
-- bytes. Called by the retrieve_source worker after a successful
-- `storage.Store(...)`. `local_path` is the human-readable mirror
-- of `storage_key` (kept for parity with the original 0008 column
-- and for direct filesystem inspection). `mirrored_at` is the
-- storage write completion time; NULL stays NULL until the
-- hosting job runs, which the frontend uses to decide whether to
-- render the served URL or fall back to the external URL.
UPDATE okt_repository.source_images
SET storage_key  = $2,
    content_type = $3,
    local_path   = $4,
    mirrored_at  = now()
WHERE id = $1
RETURNING *;

-- name: GetSourceImageByID :one
-- Single-row lookup used by the serving endpoint. The handler
-- additionally verifies the row's source_id matches the route
-- param to prevent cross-source image access via ID guessing.
SELECT * FROM okt_repository.source_images
WHERE id = $1;

-- name: MarkSourceBodyStored :one
-- Records that the storage backend has persisted the full source
-- body (today: PDF sources only). `storage_key` is the opaque key
-- passed to the backend (e.g.
-- "repositories/{repoID}/sources/{sourceID}/body.pdf");
-- `content_type` is the MIME to serve (typically
-- "application/pdf"); `local_path` is the human-readable mirror.
-- `stored_at` is the storage write completion time. HTML/text
-- sources are NOT stored this way — the 32KB content preview in
-- `sources.content` remains the source of truth for text.
UPDATE okt_repository.sources
SET storage_key  = $2,
    content_type = $3,
    local_path   = $4,
    stored_at    = now(),
    updated_at   = now()
WHERE id = $1
RETURNING *;

-- name: MarkSourceFetchAttempts :one
-- Persists the fetch strategy's audit trail (one
-- {provider, success, error, elapsed_ms} object per
-- provider tried, in chain order) on the source row. The
-- worker calls this after the strategy returns, regardless
-- of success or failure, so the UI can show which tier
-- fetched the content or which tiers failed and why. The
-- argument is a JSONB array; pass NULL to clear (e.g. on a
-- re-fetch that didn't run the strategy).
UPDATE okt_repository.sources
SET fetch_attempts = $2,
    updated_at     = now()
WHERE id = $1
RETURNING *;

-- name: ResetSourceForRetry :one
-- Flips a 'failed' source row back to 'pending' and clears
-- the recorded error / parse_status so the UI shows a clean
-- re-queue state before the retrieve_source worker runs.
-- Called by the HTTP RetrySource handler just before it
-- enqueues a fresh retrieve_source job. The worker's
-- MarkSourceFetching call will then move the row through
-- fetching → fetched/failed as usual.
UPDATE okt_repository.sources
SET status       = 'pending',
    error        = NULL,
    parse_status = NULL,
    updated_at   = now()
WHERE id = $1
RETURNING *;

-- name: MarkSourceOAStatus :one
-- Persists the open-access status Unpaywall reported for
-- the DOI (e.g. "green", "gold", "bronze", "hybrid",
-- "closed"). The worker calls this after the fetch strategy
-- returns, carrying the OAStatus from the Unpaywall attempt
-- (even when Unpaywall fell through). Pass NULL or empty to
-- clear (e.g. a non-DOI source where OA doesn't apply).
UPDATE okt_repository.sources
SET oa_status   = $2,
    updated_at  = now()
WHERE id = $1
RETURNING *;

-- name: ListFlareSolverrHostCandidates :many
-- Returns one row per host where FlareSolverr was attempted at
-- least once, with the failure and success counts. Used by
-- GET /sources/providers to surface "candidate hosts to pin
-- out of FlareSolverr" — operators review the list and, when a
-- host has many failures and zero successes, add it to a future
-- host_skip_providers config key. Today the strategy does NOT
-- enforce a skip list; this query is the data-side preparation
-- so the blacklist is ready to wire.
--
-- The query reads fetch_attempts (JSONB array of
-- {provider, success, error, elapsed_ms} objects, one per
-- provider tried in chain order) and counts, per host, how
-- many FlareSolverr attempts failed vs succeeded. A host with
-- failures > 0 and successes = 0 is a strong skip candidate.
-- The host is extracted from the url column with a regex
-- substring; rows whose url doesn't parse are grouped under
-- NULL (filtered out by the HAVING clause).
--
-- Scoped to the active repository's per-repo pool: the sources
-- table lives in okt_repository, and on the shared tier-1 DB
-- the query naturally covers every repo's rows. The handler
-- passes the per-repo pool when X-Repository-ID is set; when
-- no repo is in context the field is omitted from the
-- /sources/providers response.
SELECT
    substring(url FROM 'https?://([^/]+)')::text AS host,
    COUNT(*) AS total_attempts,
    COUNT(*) FILTER (
        WHERE EXISTS (
            SELECT 1 FROM jsonb_array_elements(fetch_attempts) AS a
            WHERE a->>'provider' = 'flaresolverr'
              AND (a->>'success')::boolean = false
        )
    ) AS flare_failures,
    COUNT(*) FILTER (
        WHERE EXISTS (
            SELECT 1 FROM jsonb_array_elements(fetch_attempts) AS a
            WHERE a->>'provider' = 'flaresolverr'
              AND (a->>'success')::boolean = true
        )
    ) AS flare_successes
FROM okt_repository.sources
WHERE fetch_attempts IS NOT NULL
GROUP BY 1
HAVING COUNT(*) FILTER (
    WHERE EXISTS (
        SELECT 1 FROM jsonb_array_elements(fetch_attempts) AS a
        WHERE a->>'provider' = 'flaresolverr'
          AND (a->>'success')::boolean = false
    )
) > 0
ORDER BY flare_failures DESC;

-- name: ListProviderHostCandidates :many
-- Generalized per-provider host-candidate query. Returns per-host
-- failure/success counts for ONE resolution provider, filtered by
-- the provider id parameter. The handler calls this once per
-- provider in the chain so the Providers UI can show a "hosts
-- that don't reply" card for each resolver (fetch, tls,
-- unpaywall, flaresolverr), not just FlareSolverr.
--
-- A host with failures > 0 and successes = 0 is a strong
-- candidate for pinning out of that provider tier. Hosts with
-- mixed results tell the operator the provider sometimes works,
-- so the issue is rate-limiting or transient rather than a hard
-- block.
--
-- Implementation note: sqlc's analyzer cannot resolve a bind
-- parameter ($1 or sqlc.arg) inside a jsonb_array_elements
-- subquery — it reports "column 'a' does not exist". The four
-- provider ids are known at compile time, so this query uses a
-- CASE expression that hardcodes the four ids and picks the
-- matching branch based on the $1 argument. Each branch is a
-- full literal query sqlc can analyze. The runtime cost is one
-- CASE evaluation per row; the planner short-circuits the
-- non-matching branches. This is the same shape the
-- FlareSolverr-specific query uses, just generalized over the
-- four providers without duplicating the query four times.
SELECT
    substring(url FROM 'https?://([^/]+)')::text AS host,
    COUNT(*) AS total_attempts,
    CASE WHEN $1 IN ('fetch','tls','unpaywall','flaresolverr','url_safety') THEN
        COUNT(*) FILTER (
            WHERE EXISTS (
                SELECT 1 FROM jsonb_array_elements(fetch_attempts) AS a
                WHERE a->>'provider' = $1
                  AND (a->>'success')::boolean = false
            )
        )
    ELSE 0 END AS failures,
    CASE WHEN $1 IN ('fetch','tls','unpaywall','flaresolverr','url_safety') THEN
        COUNT(*) FILTER (
            WHERE EXISTS (
                SELECT 1 FROM jsonb_array_elements(fetch_attempts) AS a
                WHERE a->>'provider' = $1
                  AND (a->>'success')::boolean = true
            )
        )
    ELSE 0 END AS successes
FROM okt_repository.sources
WHERE fetch_attempts IS NOT NULL
GROUP BY 1
HAVING COUNT(*) FILTER (
    WHERE EXISTS (
        SELECT 1 FROM jsonb_array_elements(fetch_attempts) AS a
        WHERE a->>'provider' = $1
          AND (a->>'success')::boolean = false
    )
) > 0
ORDER BY failures DESC;
