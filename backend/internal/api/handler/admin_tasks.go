package handler

import (
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/openktree/open-knowledge-tree/backend/internal/api/httputil"
	"github.com/riverqueue/river/rivertype"
)

// AdminTasks bundles the admin-only River job control endpoints.
// It is intentionally separate from the read-only Tasks bundle so
// the RBAC gate (repositories.tasks.cancel) is mounted at the
// route level, not buried inside the handler.
type AdminTasks struct {
	client TaskClient
	// pool is the task DB pool (where river_job + okt_worker_heartbeat
	// live). Used by RescueStuckJobs to run the rescue SQL directly.
	// May be nil when the task manager isn't configured; the handler
	// returns 503 in that case.
	pool *pgxpool.Pool
}

func NewAdminTasks(client TaskClient, pool *pgxpool.Pool) *AdminTasks {
	return &AdminTasks{client: client, pool: pool}
}

// NewAdminTasksFromTasks constructs an AdminTasks bundle that
// shares the same River client + task DB pool as a read-only Tasks
// bundle. The wiring layer uses this so /admin/tasks and /tasks
// agree on a single River client instance.
func NewAdminTasksFromTasks(t *Tasks) *AdminTasks {
	if t == nil {
		return &AdminTasks{}
	}
	return &AdminTasks{client: t.Client(), pool: t.pool}
}

// GetJob handles GET /admin/tasks/{jobID}.
//
// Returns the full River job row (state, errors, output, metadata)
// so an operator investigating a stuck pipeline can see the same
// payload the read-side /tasks/{jobID} endpoint returns, without
// having to know the job's repository ahead of time.
func (a *AdminTasks) GetJob(w http.ResponseWriter, r *http.Request) {
	if a.client == nil {
		httputil.WriteError(w, http.StatusServiceUnavailable, "task manager not configured")
		return
	}
	id, err := parseJobID(r)
	if err != nil {
		httputil.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}
	job, err := a.client.JobGet(r.Context(), id)
	if err != nil {
		httputil.WriteError(w, http.StatusNotFound, "job not found")
		return
	}
	httputil.WriteJSON(w, http.StatusOK, jobRowToMap(job))
}

// CancelJob handles POST /admin/tasks/{jobID}/cancel.
//
// River's JobCancel marks the job as cancelled. If the job is
// currently running, the worker's context is cancelled and the
// job transitions to cancelled once the worker returns; if the
// job is still available/pending, it is cancelled immediately.
// This is the recovery path for stuck jobs (e.g. an
// extract_concepts pass holding an advisory lock for hours
// because the upstream LLM provider hung): an operator with the
// repositories.tasks.cancel permission can cancel the job from
// the admin UI instead of running `docker exec psql` by hand.
func (a *AdminTasks) CancelJob(w http.ResponseWriter, r *http.Request) {
	if a.client == nil {
		httputil.WriteError(w, http.StatusServiceUnavailable, "task manager not configured")
		return
	}
	id, err := parseJobID(r)
	if err != nil {
		httputil.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}
	job, err := a.client.JobCancel(r.Context(), id)
	if err != nil {
		httputil.WriteError(w, http.StatusInternalServerError, "cancel failed: "+err.Error())
		return
	}
	httputil.WriteJSON(w, http.StatusOK, jobRowToMap(job))
}

// RescueStuckJobs handles POST /admin/tasks/rescue.
//
// Resets "running" jobs whose current owner (attempted_by[last])
// is NOT a live worker — i.e. the worker has no row in
// okt_worker_heartbeat or its last_heartbeat is older than the
// staleness threshold — back to "available" so River re-processes
// them. Jobs with a unique_key are excluded. This is the on-demand
// recovery path for jobs orphaned by an API restart; the same
// rescue also runs automatically on startup (see
// taskmanager.Manager.Start). Returns { rescued, threshold }.
func (a *AdminTasks) RescueStuckJobs(w http.ResponseWriter, r *http.Request) {
	if a.client == nil || a.pool == nil {
		httputil.WriteError(w, http.StatusServiceUnavailable, "task manager not configured")
		return
	}

	// Default staleness threshold: 10 minutes. Matches the
	// taskmanager default; the operator can override via the
	// older_than query param (Go duration syntax, e.g. 30m).
	threshold := 10 * time.Minute
	if s := r.URL.Query().Get("older_than"); s != "" {
		d, err := time.ParseDuration(s)
		if err != nil {
			httputil.WriteError(w, http.StatusBadRequest, "invalid older_than (use Go duration like 10m or 1h): "+err.Error())
			return
		}
		if d <= 0 {
			httputil.WriteError(w, http.StatusBadRequest, "older_than must be positive")
			return
		}
		threshold = d
	}

	res, err := a.pool.Exec(r.Context(),
		`UPDATE river_job
		    SET state       = 'available',
		        attempted_at = NULL,
		        finalized_at = NULL,
		        scheduled_at = now()
		  WHERE state = 'running'
		    AND unique_key IS NULL
		    AND attempted_by[array_length(attempted_by, 1)] NOT IN (
		        SELECT worker_id FROM okt_worker_heartbeat
		         WHERE last_heartbeat > now() - $1::interval
		    )`,
		threshold.String())
	if err != nil {
		httputil.WriteError(w, http.StatusInternalServerError, "rescue failed: "+err.Error())
		return
	}

	httputil.WriteJSON(w, http.StatusOK, map[string]interface{}{
		"rescued":   res.RowsAffected(),
		"threshold": threshold.String(),
	})
}

func parseJobID(r *http.Request) (int64, error) {
	idStr := chi.URLParam(r, "jobID")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		return 0, errInvalidJobID
	}
	return id, nil
}

var errInvalidJobID = &httpError{code: http.StatusBadRequest, msg: "invalid job ID"}

type httpError struct {
	code int
	msg  string
}

func (e *httpError) Error() string { return e.msg }

// compile-time guard: rivertype.JobRow must keep the shape
// jobRowToMap relies on. The blank identifier assignment silences
// "imported and not used" if rivertype ever drops a field we read.
var _ = rivertype.JobStateAvailable