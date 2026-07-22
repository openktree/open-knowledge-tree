package handler

import (
	"net/http"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/openktree/open-knowledge-tree/backend/internal/api/httputil"
	appmw "github.com/openktree/open-knowledge-tree/backend/internal/api/middleware"
	"github.com/openktree/open-knowledge-tree/backend/internal/config"
	"github.com/openktree/open-knowledge-tree/backend/internal/providers/ai"
	"github.com/openktree/open-knowledge-tree/backend/internal/store"
)

// AIUsage is the HTTP handler bundle for the AI usage dashboard.
// It reads the ai_usage table through the sqlc-generated store
// and computes an estimated dollar cost per row by joining the
// model id against the in-memory model cost config (config.
// AIModelConfig.InputCostPer1M / OutputCostPer1M). Cost is
// computed at query time so it stays in sync with config; it is
// never persisted (a config change reprices historical rows on
// the next dashboard load).
//
// Every endpoint takes an optional date range (from / to as
// RFC3339 timestamps) and an optional repository_id query param
// (UUID string). nil / empty values mean "no filter". The
// endpoints are mounted under /api/v1/ai/usage/* and gated by
// the rbac.Objects.AIUsage / Actions.Read permission (sysadmin
// only for now; the object exists so other roles can be granted
// later via the admin role-assign endpoint).
type AIUsage struct {
	Store *store.Queries
	Cfg   *config.Config
}

func NewAIUsage(queries *store.Queries, cfg *config.Config) *AIUsage {
	return &AIUsage{Store: queries, Cfg: cfg}
}

// usageFilters parses the from / to / repository_id query params
// shared by every dashboard endpoint. from/to are RFC3339
// timestamps; an empty/invalid value yields a zero-valid
// pgtype.Timestamptz (the SQL `IS NULL` branch matches it).
// repository_id is a UUID string; an empty/invalid value yields
// a zero-valid pgtype.UUID. The bool returns are for handler
// validation (a malformed value is a 400).
//
// When forcedRepoID is non-nil, the repository_id query param is
// ignored and the forced UUID is used instead. This is how the
// repo-scoped handlers (SummaryRepo, ByDayRepo, …) enforce that
// the scope comes from the URL (RequireRepoPermission) and not a
// client-supplied query param — a repoadmin of repo A can't pass
// repo B's UUID to see its usage.
type usageFilters struct {
	from         pgtype.Timestamptz
	to           pgtype.Timestamptz
	repositoryID pgtype.UUID
}

func parseUsageFilters(r *http.Request, forcedRepoID *pgtype.UUID) (usageFilters, string, bool) {
	var f usageFilters
	if raw := r.URL.Query().Get("from"); raw != "" {
		t, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			return f, "invalid 'from' timestamp (use RFC3339)", false
		}
		f.from = pgtype.Timestamptz{Time: t, Valid: true}
	}
	if raw := r.URL.Query().Get("to"); raw != "" {
		t, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			return f, "invalid 'to' timestamp (use RFC3339)", false
		}
		f.to = pgtype.Timestamptz{Time: t, Valid: true}
	}
	if forcedRepoID != nil {
		f.repositoryID = *forcedRepoID
	} else if raw := r.URL.Query().Get("repository_id"); raw != "" {
		var u pgtype.UUID
		if err := u.Scan(raw); err != nil {
			return f, "invalid 'repository_id' (must be a UUID)", false
		}
		f.repositoryID = u
	}
	return f, "", true
}

// repoIDFromContext returns the repository UUID from the request
// context (set by appmw.WithRepoQueries on /{repoID} routes) and a
// bool indicating whether it was present. Used by the repo-scoped
// AI Usage handlers to force the repository_id filter.
func repoIDFromContext(r *http.Request) (pgtype.UUID, bool) {
	id, ok := appmw.RepoIDFromContext(r.Context())
	if !ok || !id.Valid {
		return pgtype.UUID{}, false
	}
	return id, true
}

// modelCost looks up the per-model cost config and returns the
// (input, output) cost per 1M tokens. Unknown models return
// (0,0) — the row is still returned, just with zero cost.
func (a *AIUsage) modelCost(modelID string) (float64, float64) {
	m, ok := ai.LookupModel(a.Cfg, modelID)
	if !ok {
		return 0, 0
	}
	return m.InputCostPer1M, m.OutputCostPer1M
}

// rowCost computes the estimated dollar cost for a rollup row
// given its prompt / completion token totals and the model's
// per-1M input/output cost. prompt/completion are int64 sums.
func rowCost(modelID string, prompt, completion int64, a *AIUsage) float64 {
	inCost, outCost := a.modelCost(modelID)
	return float64(prompt)/1_000_000*inCost + float64(completion)/1_000_000*outCost
}

// Summary returns the per (provider, model, operation) rollup
// for the optional date range + repository filter, with an
// estimated cost per row and a grand total. System-scope: the
// repository_id query param is honored when present.
func (a *AIUsage) Summary(w http.ResponseWriter, r *http.Request) {
	a.summaryInner(w, r, nil)
}

// SummaryRepo is the repo-scoped counterpart to Summary. The
// repository UUID is read from the request context (set by
// WithRepoQueries on /{repoID} routes) and forced into the
// filter; any client-supplied repository_id query param is
// ignored, so a repoadmin of repo A cannot see repo B's usage.
func (a *AIUsage) SummaryRepo(w http.ResponseWriter, r *http.Request) {
	id, ok := repoIDFromContext(r)
	if !ok {
		httputil.WriteError(w, http.StatusBadRequest, "repoID is required")
		return
	}
	a.summaryInner(w, r, &id)
}

func (a *AIUsage) summaryInner(w http.ResponseWriter, r *http.Request, forcedRepoID *pgtype.UUID) {
	f, msg, ok := parseUsageFilters(r, forcedRepoID)
	if !ok {
		httputil.WriteError(w, http.StatusBadRequest, msg)
		return
	}
	rows, err := a.Store.GetAIUsageSummaryInRange(r.Context(), store.GetAIUsageSummaryInRangeParams{
		From:         f.from,
		To:           f.to,
		RepositoryID: f.repositoryID,
	})
	if err != nil {
		httputil.WriteError(w, http.StatusInternalServerError, err.Error())
		return
	}

	type summaryRow struct {
		Provider              string  `json:"provider"`
		Model                 string  `json:"model"`
		Operation             string  `json:"operation"`
		RequestCount          int64   `json:"request_count"`
		TotalPromptTokens     int64   `json:"total_prompt_tokens"`
		TotalCompletionTokens int64   `json:"total_completion_tokens"`
		TotalTokens           int64   `json:"total_tokens"`
		EstimatedCost         float64 `json:"estimated_cost"`
	}
	out := make([]summaryRow, 0, len(rows))
	var grandTotalPrompt, grandTotalCompletion, grandTotalTokens int64
	var grandTotalCost float64
	for _, row := range rows {
		cost := rowCost(row.Model, row.TotalPromptTokens, row.TotalCompletionTokens, a)
		out = append(out, summaryRow{
			Provider:              row.Provider,
			Model:                 row.Model,
			Operation:             row.Operation,
			RequestCount:          row.RequestCount,
			TotalPromptTokens:     row.TotalPromptTokens,
			TotalCompletionTokens: row.TotalCompletionTokens,
			TotalTokens:           row.TotalTokens,
			EstimatedCost:          cost,
		})
		grandTotalPrompt += row.TotalPromptTokens
		grandTotalCompletion += row.TotalCompletionTokens
		grandTotalTokens += row.TotalTokens
		grandTotalCost += cost
	}
	httputil.WriteJSON(w, http.StatusOK, map[string]interface{}{
		"rows":                 out,
		"total_prompt_tokens":  grandTotalPrompt,
		"total_completion_tokens": grandTotalCompletion,
		"total_tokens":         grandTotalTokens,
		"total_cost":           grandTotalCost,
	})
}

// ByDay returns time-bucketed consumption for the over-time
// chart. The `bucket` query param selects the date_trunc width
// (day/week/month); default is "day".
func (a *AIUsage) ByDay(w http.ResponseWriter, r *http.Request) {
	a.byDayInner(w, r, nil)
}

// ByDayRepo is the repo-scoped counterpart to ByDay. See SummaryRepo
// for the scope-enforcement contract.
func (a *AIUsage) ByDayRepo(w http.ResponseWriter, r *http.Request) {
	id, ok := repoIDFromContext(r)
	if !ok {
		httputil.WriteError(w, http.StatusBadRequest, "repoID is required")
		return
	}
	a.byDayInner(w, r, &id)
}

func (a *AIUsage) byDayInner(w http.ResponseWriter, r *http.Request, forcedRepoID *pgtype.UUID) {
	f, msg, ok := parseUsageFilters(r, forcedRepoID)
	if !ok {
		httputil.WriteError(w, http.StatusBadRequest, msg)
		return
	}
	bucket := r.URL.Query().Get("bucket")
	if bucket == "" {
		bucket = "day"
	}
	switch bucket {
	case "day", "week", "month":
	default:
		httputil.WriteError(w, http.StatusBadRequest, "invalid 'bucket' (use day, week, or month)")
		return
	}
	rows, err := a.Store.GetAIUsageByDay(r.Context(), store.GetAIUsageByDayParams{
		Bucket:       bucket,
		From:         f.from,
		To:           f.to,
		RepositoryID: f.repositoryID,
	})
	if err != nil {
		httputil.WriteError(w, http.StatusInternalServerError, err.Error())
		return
	}

	type dayRow struct {
		Bucket                time.Time `json:"bucket"`
		Model                 string    `json:"model"`
		TotalPromptTokens     int64     `json:"total_prompt_tokens"`
		TotalCompletionTokens int64     `json:"total_completion_tokens"`
		TotalTokens           int64     `json:"total_tokens"`
		RequestCount          int64     `json:"request_count"`
	}
	out := make([]dayRow, 0, len(rows))
	for _, row := range rows {
		var t time.Time
		if row.Bucket.Valid {
			t = row.Bucket.Time
		}
		out = append(out, dayRow{
			Bucket:                t,
			Model:                 row.Model,
			TotalPromptTokens:     row.TotalPromptTokens,
			TotalCompletionTokens: row.TotalCompletionTokens,
			TotalTokens:           row.TotalTokens,
			RequestCount:          row.RequestCount,
		})
	}
	httputil.WriteJSON(w, http.StatusOK, map[string]interface{}{
		"bucket": bucket,
		"rows":   out,
	})
}

// ByOperation returns the per (operation, model) rollup.
func (a *AIUsage) ByOperation(w http.ResponseWriter, r *http.Request) {
	a.byOperationInner(w, r, nil)
}

// ByOperationRepo is the repo-scoped counterpart to ByOperation.
// See SummaryRepo for the scope-enforcement contract.
func (a *AIUsage) ByOperationRepo(w http.ResponseWriter, r *http.Request) {
	id, ok := repoIDFromContext(r)
	if !ok {
		httputil.WriteError(w, http.StatusBadRequest, "repoID is required")
		return
	}
	a.byOperationInner(w, r, &id)
}

func (a *AIUsage) byOperationInner(w http.ResponseWriter, r *http.Request, forcedRepoID *pgtype.UUID) {
	f, msg, ok := parseUsageFilters(r, forcedRepoID)
	if !ok {
		httputil.WriteError(w, http.StatusBadRequest, msg)
		return
	}
	rows, err := a.Store.GetAIUsageByOperation(r.Context(), store.GetAIUsageByOperationParams{
		From:         f.from,
		To:           f.to,
		RepositoryID: f.repositoryID,
	})
	if err != nil {
		httputil.WriteError(w, http.StatusInternalServerError, err.Error())
		return
	}

	type opRow struct {
		Operation             string  `json:"operation"`
		Model                 string  `json:"model"`
		RequestCount          int64   `json:"request_count"`
		TotalPromptTokens     int64   `json:"total_prompt_tokens"`
		TotalCompletionTokens int64   `json:"total_completion_tokens"`
		TotalTokens           int64   `json:"total_tokens"`
		EstimatedCost         float64 `json:"estimated_cost"`
	}
	out := make([]opRow, 0, len(rows))
	for _, row := range rows {
		out = append(out, opRow{
			Operation:             row.Operation,
			Model:                 row.Model,
			RequestCount:          row.RequestCount,
			TotalPromptTokens:     row.TotalPromptTokens,
			TotalCompletionTokens: row.TotalCompletionTokens,
			TotalTokens:           row.TotalTokens,
			EstimatedCost:          rowCost(row.Model, row.TotalPromptTokens, row.TotalCompletionTokens, a),
		})
	}
	httputil.WriteJSON(w, http.StatusOK, map[string]interface{}{"rows": out})
}

// ByRepository returns the per (repository_id, model) rollup.
// Rows with a NULL repository_id (interactive chat without a
// repository_id body field, or pre-attribution historical rows)
// are returned with a null repository_id in the JSON.
func (a *AIUsage) ByRepository(w http.ResponseWriter, r *http.Request) {
	a.byRepositoryInner(w, r, nil)
}

// ByRepositoryRepo is the repo-scoped counterpart to ByRepository.
// See SummaryRepo for the scope-enforcement contract. (The
// breakdown is by source within this repo; the per-repo rollup
// is degenerate — one row group — but kept for UI parity with
// the system page's tabs.)
func (a *AIUsage) ByRepositoryRepo(w http.ResponseWriter, r *http.Request) {
	id, ok := repoIDFromContext(r)
	if !ok {
		httputil.WriteError(w, http.StatusBadRequest, "repoID is required")
		return
	}
	a.byRepositoryInner(w, r, &id)
}

func (a *AIUsage) byRepositoryInner(w http.ResponseWriter, r *http.Request, forcedRepoID *pgtype.UUID) {
	f, msg, ok := parseUsageFilters(r, forcedRepoID)
	if !ok {
		httputil.WriteError(w, http.StatusBadRequest, msg)
		return
	}
	rows, err := a.Store.GetAIUsageByRepository(r.Context(), store.GetAIUsageByRepositoryParams{
		From:         f.from,
		To:           f.to,
		RepositoryID: f.repositoryID,
	})
	if err != nil {
		httputil.WriteError(w, http.StatusInternalServerError, err.Error())
		return
	}

	type repoRow struct {
		RepositoryID          *string  `json:"repository_id"`
		Model                 string   `json:"model"`
		RequestCount          int64    `json:"request_count"`
		TotalPromptTokens     int64    `json:"total_prompt_tokens"`
		TotalCompletionTokens int64    `json:"total_completion_tokens"`
		TotalTokens           int64    `json:"total_tokens"`
		EstimatedCost         float64  `json:"estimated_cost"`
	}
	out := make([]repoRow, 0, len(rows))
	for _, row := range rows {
		var rid *string
		if row.RepositoryID.Valid {
			s := row.RepositoryID.String()
			rid = &s
		}
		out = append(out, repoRow{
			RepositoryID:          rid,
			Model:                 row.Model,
			RequestCount:          row.RequestCount,
			TotalPromptTokens:     row.TotalPromptTokens,
			TotalCompletionTokens: row.TotalCompletionTokens,
			TotalTokens:           row.TotalTokens,
			EstimatedCost:          rowCost(row.Model, row.TotalPromptTokens, row.TotalCompletionTokens, a),
		})
	}
	httputil.WriteJSON(w, http.StatusOK, map[string]interface{}{"rows": out})
}

// BySource returns the per (source_id, repository_id, model)
// BySource returns the per (source_id, repository_id, model)
// rollup. Source attribution requires the migration 0014
// columns; rows from before that migration (or interactive
// calls without a source_id) have a NULL source_id.
func (a *AIUsage) BySource(w http.ResponseWriter, r *http.Request) {
	a.bySourceInner(w, r, nil)
}

// BySourceRepo is the repo-scoped counterpart to BySource.
// See SummaryRepo for the scope-enforcement contract.
func (a *AIUsage) BySourceRepo(w http.ResponseWriter, r *http.Request) {
	id, ok := repoIDFromContext(r)
	if !ok {
		httputil.WriteError(w, http.StatusBadRequest, "repoID is required")
		return
	}
	a.bySourceInner(w, r, &id)
}

func (a *AIUsage) bySourceInner(w http.ResponseWriter, r *http.Request, forcedRepoID *pgtype.UUID) {
	f, msg, ok := parseUsageFilters(r, forcedRepoID)
	if !ok {
		httputil.WriteError(w, http.StatusBadRequest, msg)
		return
	}
	rows, err := a.Store.GetAIUsageBySource(r.Context(), store.GetAIUsageBySourceParams{
		From:         f.from,
		To:           f.to,
		RepositoryID: f.repositoryID,
	})
	if err != nil {
		httputil.WriteError(w, http.StatusInternalServerError, err.Error())
		return
	}

	type sourceRow struct {
		SourceID              *string  `json:"source_id"`
		RepositoryID          *string  `json:"repository_id"`
		Model                 string   `json:"model"`
		RequestCount          int64    `json:"request_count"`
		TotalPromptTokens     int64    `json:"total_prompt_tokens"`
		TotalCompletionTokens int64    `json:"total_completion_tokens"`
		TotalTokens           int64    `json:"total_tokens"`
		EstimatedCost         float64  `json:"estimated_cost"`
	}
	out := make([]sourceRow, 0, len(rows))
	for _, row := range rows {
		var sid, rid *string
		if row.SourceID.Valid {
			s := row.SourceID.String()
			sid = &s
		}
		if row.RepositoryID.Valid {
			s := row.RepositoryID.String()
			rid = &s
		}
		out = append(out, sourceRow{
			SourceID:              sid,
			RepositoryID:          rid,
			Model:                 row.Model,
			RequestCount:          row.RequestCount,
			TotalPromptTokens:     row.TotalPromptTokens,
			TotalCompletionTokens: row.TotalCompletionTokens,
			TotalTokens:           row.TotalTokens,
			EstimatedCost:          rowCost(row.Model, row.TotalPromptTokens, row.TotalCompletionTokens, a),
		})
	}
	httputil.WriteJSON(w, http.StatusOK, map[string]interface{}{"rows": out})
}

// limitParam returns a clamped LIMIT for the raw-list endpoints.
// Default 100, max 1000, non-numeric falls back to the default.
func limitParam(r *http.Request) int32 {
	raw := r.URL.Query().Get("limit")
	if raw == "" {
		return 100
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return 100
	}
	if n > 1000 {
		return 1000
	}
	return int32(n)
}