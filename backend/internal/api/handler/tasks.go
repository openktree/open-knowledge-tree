package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/openktree/open-knowledge-tree/backend/internal/api/httputil"
	appmw "github.com/openktree/open-knowledge-tree/backend/internal/api/middleware"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
)

type TaskClient interface {
	JobList(ctx context.Context, params *river.JobListParams) (*river.JobListResult, error)
	JobGet(ctx context.Context, id int64) (*rivertype.JobRow, error)
	JobCancel(ctx context.Context, id int64) (*rivertype.JobRow, error)
}

type Tasks struct {
	client       TaskClient
	pool         *pgxpool.Pool
	queueConfigs map[string]int
}

func NewTasks(client TaskClient, pool *pgxpool.Pool, queueConfigs map[string]int) *Tasks {
	return &Tasks{client: client, pool: pool, queueConfigs: queueConfigs}
}

// Client returns the underlying River client. Used by the admin
// tasks bundle to share a single client instance with the
// read-only tasks bundle.
func (t *Tasks) Client() TaskClient {
	return t.client
}

func (t *Tasks) ListJobs(w http.ResponseWriter, r *http.Request) {
	if t.client == nil {
		httputil.WriteError(w, http.StatusServiceUnavailable, "task manager not configured")
		return
	}

	params := river.NewJobListParams()
	writeJobListResponse(w, r, t.client, params, nil)
}

// ListRepoJobs handles GET /{repoID}/tasks.
//
// It is the per-repo scoped counterpart to ListJobs: it filters
// River jobs by the `repo_id` metadata tag written at enqueue time
// (see tasks.MarshalMetadata), and optionally by `source_id` when
// the `source_id` query param is present. The filter uses River's
// metadata containment check (`metadata @> fragment::jsonb`), so a
// partial JSON object is enough to match.
//
// The endpoint lives under the /{repoID} route group, so the
// WithRepoQueries middleware has already resolved and attached the
// repository UUID to the request context (appmw.RepoIDFromContext).
// We read it here rather than from a query param so the caller
// can't list another repo's jobs by passing a different id.
//
// `kind` and `state` filters are forwarded to River like ListJobs
// does. `limit` defaults to 100 and caps at 10000.
func (t *Tasks) ListRepoJobs(w http.ResponseWriter, r *http.Request) {
	if t.client == nil {
		httputil.WriteError(w, http.StatusServiceUnavailable, "task manager not configured")
		return
	}

	repoID, ok := appmw.RepoIDFromContext(r.Context())
	if !ok {
		httputil.WriteError(w, http.StatusBadRequest, "repoID is required")
		return
	}

	// Build the metadata containment fragment. We always include
	// repo_id; source_id is added only when the caller passed it
	// (a source-scoped lookup, e.g. the Sources phase showing one
	// source's ingestion + decomposition jobs). Jobs that carry
	// no metadata (e.g. fact_catchup) won't match the fragment and
	// are correctly excluded from the per-repo listing.
	meta := map[string]string{"repo_id": repoIDString(repoID)}
	if v := r.URL.Query().Get("source_id"); v != "" {
		meta["source_id"] = v
	}
	metaJSON, err := json.Marshal(meta)
	if err != nil {
		httputil.WriteError(w, http.StatusInternalServerError, "encoding metadata filter: "+err.Error())
		return
	}

	params := river.NewJobListParams().Metadata(string(metaJSON))
	writeJobListResponse(w, r, t.client, params, map[string]string{"repo_id": repoIDString(repoID)})
}

// repoIDString renders a pgtype.UUID as the canonical lowercase
// hyphenated form River's metadata carries (set by
// tasks.MarshalMetadata from the string-form repository id). We
// avoid pulling in the fmt-based helper in source.go to keep this
// file independent of the source handler.
func repoIDString(id pgtype.UUID) string {
	if !id.Valid {
		return ""
	}
	b := id.Bytes
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func (t *Tasks) GetJob(w http.ResponseWriter, r *http.Request) {
	if t.client == nil {
		httputil.WriteError(w, http.StatusServiceUnavailable, "task manager not configured")
		return
	}

	idStr := chi.URLParam(r, "jobID")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		httputil.WriteError(w, http.StatusBadRequest, "invalid job ID")
		return
	}

	job, err := t.client.JobGet(r.Context(), id)
	if err != nil {
		httputil.WriteError(w, http.StatusNotFound, "job not found")
		return
	}

	httputil.WriteJSON(w, http.StatusOK, jobRowToMap(job))
}

// jobStatsRow is the scan target for the per-queue/per-state
// aggregation query in ListJobStats.
type jobStatsRow struct {
	Queue string `db:"queue"`
	State string `db:"state"`
	Count int64  `db:"count"`
}

// ListJobStats handles GET /api/v1/tasks/stats.
//
// It returns a system-wide aggregation of every River job, grouped
// by queue and state. The response has two sections:
//
//   - "queues": one entry per queue that exists in the river_job
//     table, each carrying a "states" map (state → count) and
//     a "total" sum.
//   - "totals": cross-queue rollup of every state plus an
//     overall "total".
//
// This endpoint is designed for the Tasks-page overview card so an
// operator can see the entire backlog at a glance. It uses a raw
// SQL GROUP BY on the river_job table rather than River's JobList
// API because the latter has no built-in aggregate/count feature.
//
// Auth: requires an authenticated session (same as ListJobs).
// Returns 503 when the task database pool is not wired.
func (t *Tasks) ListJobStats(w http.ResponseWriter, r *http.Request) {
	if t.client == nil || t.pool == nil {
		httputil.WriteError(w, http.StatusServiceUnavailable, "task manager not configured")
		return
	}

	rows, err := t.pool.Query(r.Context(),
		`SELECT queue, state, count(*)::bigint AS count
		 FROM river_job
		 GROUP BY queue, state
		 ORDER BY queue, state`)
	if err != nil {
		httputil.WriteError(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer rows.Close()

	// Accumulate: queueName → { stateName → count },
	// and per-state totals.
	type queueAcc struct {
		States map[string]int64 `json:"states"`
		Total  int64            `json:"total"`
	}
	queues := make(map[string]*queueAcc)
	totals := make(map[string]int64)
	var grandTotal int64

	for rows.Next() {
		var row jobStatsRow
		if err := rows.Scan(&row.Queue, &row.State, &row.Count); err != nil {
			httputil.WriteError(w, http.StatusInternalServerError, err.Error())
			return
		}
		qa, ok := queues[row.Queue]
		if !ok {
			qa = &queueAcc{States: make(map[string]int64)}
			queues[row.Queue] = qa
		}
		qa.States[row.State] = row.Count
		qa.Total += row.Count
		totals[row.State] += row.Count
		grandTotal += row.Count
	}
	if err := rows.Err(); err != nil {
		httputil.WriteError(w, http.StatusInternalServerError, err.Error())
		return
	}

	// Sort queue names for deterministic output.
	queueNames := make([]string, 0, len(queues))
	for q := range queues {
		queueNames = append(queueNames, q)
	}
	sort.Strings(queueNames)

	// defaultQueueWorkers is the fallback River uses when the
	// task.queues config block is empty or misconfigured. We
	// mirror it here so the API response is honest about the
	// effective worker cap, even when the operator's config
	// didn't declare per-queue limits.
	const defaultQueueWorkers = 100

	queueList := make([]map[string]interface{}, 0, len(queueNames))
	for _, q := range queueNames {
		qa := queues[q]
		mw, ok := t.queueConfigs[q]
		if !ok {
			mw = defaultQueueWorkers
		}
		queueList = append(queueList, map[string]interface{}{
			"queue":       q,
			"states":      qa.States,
			"total":       qa.Total,
			"max_workers": mw,
		})
	}

	totals["total"] = grandTotal

	httputil.WriteJSON(w, http.StatusOK, map[string]interface{}{
		"queues": queueList,
		"totals": totals,
	})
}

// defaultJobListLimit is the page size the tasks endpoints serve
// when the caller doesn't pass an explicit limit. Kept modest so a
// bare GET /tasks doesn't dump the whole queue; the UI paginates
// via cursor for anything beyond the first page.
const defaultJobListLimit = 50

// writeJobListResponse factors the shared "apply filters + sort +
// cursor + limit, then write the JSON response" tail that both
// ListJobs and ListRepoJobs use. The caller passes the base
// JobListParams (with any metadata containment filter already
// applied) and the response writer/request.
//
// Sort defaults to ID descending (most recently created job first)
// because River auto-increments the job id on insert, so id-desc
// is the cheapest stable proxy for "most recent first" without
// the per-state time-field special-casing that
// JobListOrderByTime would require. The caller can't override the
// sort direction from the query string today; that's intentional —
// the UI only needs "newest first" for now, and exposing a sort
// param is a feature creep we can add later.
//
// Pagination is cursor-based. The opaque cursor is the marshaled
// JobListCursor River returns in JobListResult.LastCursor; the UI
// passes it back as ?cursor=... and the helper feeds it to
// params.After. has_more is reported true when River returned a
// non-nil LastCursor (which it does whenever the result set was
// non-empty — River emits a cursor for the last row regardless of
// whether more rows exist, so we treat "cursor present AND we
// filled the page" as "has more"). next_cursor is the cursor
// string to pass on the next request, or null when there is no
// next page.
func writeJobListResponse(w http.ResponseWriter, r *http.Request, client TaskClient, params *river.JobListParams, _ map[string]string) {
	// Default sort: most recently created first. River auto-
	// increments job id, so id-desc is the stable proxy for
	// created_at-desc.
	params = params.OrderBy(river.JobListOrderByID, river.SortOrderDesc)

	if v := r.URL.Query().Get("state"); v != "" {
		params = params.States(rivertype.JobState(v))
	}
	if v := r.URL.Query().Get("kind"); v != "" {
		params = params.Kinds(v)
	}
	if v := r.URL.Query().Get("queue"); v != "" {
		params = params.Queues(v)
	}
	limit := defaultJobListLimit
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 10000 {
			httputil.WriteError(w, http.StatusBadRequest, "limit must be between 1 and 10000")
			return
		}
		limit = n
	}
	params = params.First(limit)

	if v := r.URL.Query().Get("cursor"); v != "" {
		var cursor river.JobListCursor
		if err := cursor.UnmarshalText([]byte(v)); err != nil {
			httputil.WriteError(w, http.StatusBadRequest, "invalid cursor: "+err.Error())
			return
		}
		params = params.After(&cursor)
	}

	result, err := client.JobList(r.Context(), params)
	if err != nil {
		httputil.WriteError(w, http.StatusInternalServerError, err.Error())
		return
	}

	jobs := make([]map[string]interface{}, 0, len(result.Jobs))
	for _, j := range result.Jobs {
		jobs = append(jobs, jobRowToMap(j))
	}

	// has_more: River returns a LastCursor whenever the result set
	// was non-empty. We treat "we filled the page AND a cursor was
	// emitted" as "more rows may exist" — the only false positive
	// is the exact-page-boundary case (the last row happens to be
	// the final row), which the UI resolves by fetching the next
	// page and rendering an empty list. That's cheaper than a
	// separate count query on every list call.
	hasMore := false
	var nextCursor interface{}
	if result.LastCursor != nil && len(result.Jobs) >= limit {
		hasMore = true
		if raw, err := result.LastCursor.MarshalText(); err == nil {
			nextCursor = string(raw)
		}
	}

	httputil.WriteJSON(w, http.StatusOK, map[string]interface{}{
		"jobs":         jobs,
		"has_more":     hasMore,
		"next_cursor":  nextCursor,
	})
}

func jobRowToMap(j *rivertype.JobRow) map[string]interface{} {
	// json.RawMessage is finicky: the zero value (json.RawMessage{})
	// is NOT valid JSON, and the encoder refuses to marshal it
	// (it complains "unexpected end of JSON input"). For rows
	// the worker never populated (EncodedArgs / Metadata are
	// nil for jobs the worker never started, e.g. a scheduled
	// job the test suite inserts) we have to keep the value
	// nil so it marshals as JSON null, not an empty bytes
	// slice that the encoder rejects. The previous version
	// used json.RawMessage{} for the empty case and only
	// worked because production always populates the fields.
	var encodedArgs, output, metadata json.RawMessage
	if len(j.EncodedArgs) > 0 {
		encodedArgs = json.RawMessage(j.EncodedArgs)
	}
	if raw := j.Output(); len(raw) > 0 {
		output = json.RawMessage(raw)
	}
	if len(j.Metadata) > 0 {
		metadata = json.RawMessage(j.Metadata)
	}

	errors := make([]map[string]interface{}, 0, len(j.Errors))
	for _, e := range j.Errors {
		errors = append(errors, map[string]interface{}{
			"at":      e.At,
			"attempt": e.Attempt,
			"error":   e.Error,
			"trace":   e.Trace,
		})
	}

	m := map[string]interface{}{
		"id":            j.ID,
		"attempt":       j.Attempt,
		"attempted_at":  j.AttemptedAt,
		"attempted_by":  j.AttemptedBy,
		"created_at":    j.CreatedAt,
		"encoded_args":  encodedArgs,
		"errors":        errors,
		"finalized_at":  j.FinalizedAt,
		"kind":          j.Kind,
		"max_attempts":  j.MaxAttempts,
		"metadata":      metadata,
		"output":        output,
		"priority":      j.Priority,
		"queue":         j.Queue,
		"scheduled_at":  j.ScheduledAt,
		"state":         string(j.State),
		"tags":          j.Tags,
		"duration_ms":   jobDurationMs(j, time.Now()),
		"queue_wait_ms": jobQueueWaitMs(j),
	}

	return m
}

// jobDurationMs returns the wall-clock execution time of a job in
// milliseconds, or nil when the job has not been attempted yet.
//
// Semantics by state:
//
//   - running / retryable: a worker has started at attempted_at
//     but the job is not finalised. We report the elapsed time
//     from attempted_at up to the supplied "now" so the UI can
//     show a live counter without a server round-trip per tick.
//   - completed / cancelled / discarded: the job is finalised.
//     When attempted_at is set, we report the worker's execution
//     span (finalized_at - attempted_at). When the job was
//     finalized without ever being attempted (cancelled before
//     pickup), we fall back to finalized_at - created_at so the
//     field is still meaningful.
//   - available / scheduled / pending: never attempted. The
//     caller has not done any work yet, so we return nil.
//
// The "now" argument is injected so callers (and tests) can pass
// a deterministic clock; production callers pass time.Now().
func jobDurationMs(j *rivertype.JobRow, now time.Time) interface{} {
	if j.AttemptedAt == nil {
		return nil
	}
	end := now
	if j.FinalizedAt != nil {
		end = *j.FinalizedAt
	}
	d := end.Sub(*j.AttemptedAt).Milliseconds()
	if d < 0 {
		// Defensive: clock skew between the worker's host and
		// this server can produce a negative span. Clamp at 0
		// so the UI never renders "-3s".
		return int64(0)
	}
	return d
}

// jobQueueWaitMs returns the time the job spent waiting in the
// queue before its first attempt, in milliseconds. Nil when the
// job has not been attempted yet (a "queue wait" is undefined).
//
// For completed jobs this measures how long the worker took to
// pick the job up after insert — useful for diagnosing a slow
// or saturated worker pool. For running/retryable jobs the same
// span is reported at the snapshot time, so the UI can render a
// "waiting in queue" indicator that doesn't tick (it stopped
// ticking the moment the worker started).
func jobQueueWaitMs(j *rivertype.JobRow) interface{} {
	if j.AttemptedAt == nil {
		return nil
	}
	d := j.AttemptedAt.Sub(j.CreatedAt).Milliseconds()
	if d < 0 {
		return int64(0)
	}
	return d
}
