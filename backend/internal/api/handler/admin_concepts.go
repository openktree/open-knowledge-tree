package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/openktree/open-knowledge-tree/backend/internal/api/httputil"
	"github.com/openktree/open-knowledge-tree/backend/internal/store"
)

// SetRepoPoolResolver wires the per-repo pool resolver used by the
// concept-reextract and source-reprocess admin endpoints. Called
// by the wiring layer after the dbpool.Registry is built. Reuses
// the source handler's RepoPoolResolver signature (string repoID
// → pool + UUID + err) so the wiring layer can pass the same
// resolver it hands to the source handler.
func (a *Admin) SetRepoPoolResolver(r RepoPoolResolver) {
	a.repoPoolResolver = r
}

// SetTaskEnqueuer wires the background-task enqueuer used by the
// concept-reextract and source-reprocess admin endpoints to kick
// off extract_concepts / source_decomposition jobs.
func (a *Admin) SetTaskEnqueuer(eq TaskEnqueuer) {
	a.taskEnqueuer = eq
}

// reextractPreviewResponse is the wire shape for GET
// /api/v1/admin/repos/{repoID}/concepts/reextract — the
// "what would be affected" preview the UI shows in the danger
// box before the user confirms.
type reextractPreviewResponse struct {
	RepositoryID            string `json:"repository_id"`
	UnlinkedStableFacts     int64  `json:"unlinked_stable_facts"`
	RetryableSkips          int64  `json:"retryable_skips"`
	UnresolvedCandidates    int64  `json:"unresolved_candidates"`
	MaxConceptAttempts      int32  `json:"max_concept_attempts"`
	SourceCount             int    `json:"source_count"`
}

// PreviewReextractRepoConcepts handles GET
// /api/v1/admin/repos/{repoID}/concepts/reextract. Returns the
// counts of facts and candidates that WOULD be affected by the
// POST endpoint, without actually clearing or enqueuing anything.
// The UI uses this to render the danger box with live counts.
func (a *Admin) PreviewReextractRepoConcepts(w http.ResponseWriter, r *http.Request) {
	if a.repoPoolResolver == nil {
		httputil.WriteError(w, http.StatusServiceUnavailable, "repo pool resolver not configured")
		return
	}
	repoIDStr := chi.URLParam(r, "repoID")
	if repoIDStr == "" {
		httputil.WriteError(w, http.StatusBadRequest, "repoID is required")
		return
	}
	pool, _, err := a.repoPoolResolver(r.Context(), repoIDStr)
	if err != nil {
		httputil.WriteError(w, http.StatusNotFound, "repository not found: "+err.Error())
		return
	}
	maxAttempts := int32(3)
	if v := r.URL.Query().Get("max_attempts"); v != "" {
		if n, perr := strconv.ParseInt(v, 10, 32); perr == nil && n > 0 {
			maxAttempts = int32(n)
		}
	}
	queries := store.New(pool)
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	unlinked, err := queries.CountUnlinkedStableFactsByRepo(ctx, store.CountUnlinkedStableFactsByRepoParams{
		RepositoryID:       pgRepoIDFromStr(repoIDStr),
		MaxConceptAttempts: maxAttempts,
	})
	if err != nil {
		httputil.WriteError(w, http.StatusInternalServerError, "counting unlinked facts: "+err.Error())
		return
	}
	skips, err := queries.CountRetryableSkipsByRepo(ctx, store.CountRetryableSkipsByRepoParams{
		RepositoryID:       pgRepoIDFromStr(repoIDStr),
		MaxConceptAttempts: maxAttempts,
	})
	if err != nil {
		httputil.WriteError(w, http.StatusInternalServerError, "counting retryable skips: "+err.Error())
		return
	}
	cands, err := queries.CountUnresolvedCandidatesByRepo(ctx, pgRepoIDFromStr(repoIDStr))
	if err != nil {
		httputil.WriteError(w, http.StatusInternalServerError, "counting unresolved candidates: "+err.Error())
		return
	}
	sources, err := queries.ListSourcesWithUnlinkedFactsByRepo(ctx, store.ListSourcesWithUnlinkedFactsByRepoParams{
		RepositoryID:       pgRepoIDFromStr(repoIDStr),
		MaxConceptAttempts: maxAttempts,
	})
	if err != nil {
		httputil.WriteError(w, http.StatusInternalServerError, "counting sources: "+err.Error())
		return
	}
	httputil.WriteJSON(w, http.StatusOK, reextractPreviewResponse{
		RepositoryID:         repoIDStr,
		UnlinkedStableFacts:  unlinked,
		RetryableSkips:       skips,
		UnresolvedCandidates: cands,
		MaxConceptAttempts:   maxAttempts,
		SourceCount:          len(sources),
	})
}

// reextractRepoConceptsResponse is the wire shape for POST
// /api/v1/admin/repos/{repoID}/concepts/reextract.
type reextractRepoConceptsResponse struct {
	RepositoryID      string   `json:"repository_id"`
	ClearedSkips      int64    `json:"cleared_skips"`
	ClearedCandidates int64    `json:"cleared_candidates"`
	EnqueuedJobCount  int      `json:"enqueued_job_count"`
	EnqueuedJobIDs    []string `json:"enqueued_job_ids"`
}

// ReextractRepoConcepts handles POST
// /api/v1/admin/repos/{repoID}/concepts/reextract. Clears
// retryable fact_concept_skips (attempts < max_concept_attempts)
// and unresolved fact_candidates for the repo, then enqueues one
// extract_concepts job PER SOURCE that has unlinked stable facts
// (matching the normal deduplicate_facts → extract_concepts chain,
// so each source's facts are processed independently and the queue
// can parallelize across sources). Used to recover from the
// historical permanent-skip bug (121,312 facts severed from their
// concepts by transient OpenRouter failures) and any future
// recurrence.
//
// Gated by repositories.*.manage (sysadmin or repo-admin). The
// endpoint is on-demand (operator-driven), not a periodic re-
// driver — the KISS approach leaves recovery in operator hands so
// the system never silently re-spends LLM quota.
func (a *Admin) ReextractRepoConcepts(w http.ResponseWriter, r *http.Request) {
	if a.repoPoolResolver == nil {
		httputil.WriteError(w, http.StatusServiceUnavailable, "repo pool resolver not configured")
		return
	}
	if a.taskEnqueuer == nil {
		httputil.WriteError(w, http.StatusServiceUnavailable, "task enqueuer not configured")
		return
	}
	repoIDStr := chi.URLParam(r, "repoID")
	if repoIDStr == "" {
		httputil.WriteError(w, http.StatusBadRequest, "repoID is required")
		return
	}
	pool, repoID, err := a.repoPoolResolver(r.Context(), repoIDStr)
	if err != nil {
		httputil.WriteError(w, http.StatusNotFound, "repository not found: "+err.Error())
		return
	}

	maxAttempts := int32(3)
	if v := r.URL.Query().Get("max_attempts"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 32); err == nil && n > 0 {
			maxAttempts = int32(n)
		}
	}

	queries := store.New(pool)
	clearCtx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	// Snapshot the retryable skip fact_ids BEFORE the DELETE so we
	// can decrement the per-source denormalized concept_skip_count
	// accurately (the DELETE removes the rows we'd otherwise join
	// on). The snapshot is bounded by the repo's skip count.
	skipRows, err := queries.ListFactConceptSkipsByRepo(clearCtx, repoID)
	if err != nil {
		httputil.WriteError(w, http.StatusInternalServerError, "listing retryable skips: "+err.Error())
		return
	}
	var retryableFactIDs []pgtype.UUID
	for _, sk := range skipRows {
		if sk.Attempts < maxAttempts {
			retryableFactIDs = append(retryableFactIDs, sk.FactID)
		}
	}

	// Clear retryable skips.
	clearedSkips, err := queries.ClearRetryableFactConceptSkipsByRepo(clearCtx, store.ClearRetryableFactConceptSkipsByRepoParams{
		RepositoryID:       repoID,
		MaxConceptAttempts: maxAttempts,
	})
	if err != nil {
		httputil.WriteError(w, http.StatusInternalServerError, "clearing retryable skips: "+err.Error())
		return
	}

	// Decrement the per-source denormalized concept_skip_count.
	if len(retryableFactIDs) > 0 {
		if _, err := queries.DecrementSourceConceptSkipCountByFactID(clearCtx, retryableFactIDs); err != nil {
			_ = err // non-fatal; skips already cleared
		}
	}

	// Clear unresolved fact_candidates (Mode 5).
	clearedCandidates, err := queries.ClearUnresolvedFactCandidatesByRepo(clearCtx, repoID)
	if err != nil {
		httputil.WriteError(w, http.StatusInternalServerError, "clearing unresolved candidates: "+err.Error())
		return
	}

	// List distinct sources that have unlinked stable facts, then
	// enqueue one extract_concepts job per source (matching the
	// normal pipeline's per-source fan-out). This lets the queue
	// parallelize across sources instead of one giant repo-wide
	// job that processes everything serially.
	sources, err := queries.ListSourcesWithUnlinkedFactsByRepo(clearCtx, store.ListSourcesWithUnlinkedFactsByRepoParams{
		RepositoryID:       repoID,
		MaxConceptAttempts: maxAttempts,
	})
	if err != nil {
		httputil.WriteError(w, http.StatusInternalServerError, "listing sources with unlinked facts: "+err.Error())
		return
	}

	var jobIDs []string
	for _, srcID := range sources {
		jobID, err := a.taskEnqueuer.EnqueueExtractConceptsFromHTTP(r.Context(), ExtractConceptsArgs{
			RepositoryID: repoIDStr,
			SourceID:     srcID.String(),
		})
		if err != nil {
			httputil.WriteError(w, http.StatusInternalServerError, "enqueueing extract_concepts for source "+srcID.String()+": "+err.Error())
			return
		}
		jobIDs = append(jobIDs, jobID)
	}

	httputil.WriteJSON(w, http.StatusOK, reextractRepoConceptsResponse{
		RepositoryID:      repoIDStr,
		ClearedSkips:      clearedSkips,
		ClearedCandidates: clearedCandidates,
		EnqueuedJobCount:  len(jobIDs),
		EnqueuedJobIDs:    jobIDs,
	})
}

// reprocessSourceResponse is the wire shape for POST
// /api/v1/admin/repos/{repoID}/sources/{sourceID}/reprocess.
// reprocessPreviewResponse is the wire shape for GET
// /api/v1/admin/repos/{repoID}/sources/{sourceID}/reprocess —
// the "what would be re-run" preview the UI shows in the danger
// box before the user confirms.
type reprocessPreviewResponse struct {
	RepositoryID     string `json:"repository_id"`
	SourceID         string `json:"source_id"`
	ChunkFailures    int32  `json:"chunk_failures"`
	FailedChunkCount int   `json:"failed_chunk_count"`
	SourceTitle      string `json:"source_title"`
}

// PreviewReprocessSource handles GET
// /api/v1/admin/repos/{repoID}/sources/{sourceID}/reprocess.
// Returns the chunk failure counts from the source row's
// chunk_errors JSONB without enqueuing anything.
func (a *Admin) PreviewReprocessSource(w http.ResponseWriter, r *http.Request) {
	if a.repoPoolResolver == nil {
		httputil.WriteError(w, http.StatusServiceUnavailable, "repo pool resolver not configured")
		return
	}
	repoIDStr := chi.URLParam(r, "repoID")
	sourceIDStr := chi.URLParam(r, "sourceID")
	if repoIDStr == "" || sourceIDStr == "" {
		httputil.WriteError(w, http.StatusBadRequest, "repoID and sourceID are required")
		return
	}
	pool, _, err := a.repoPoolResolver(r.Context(), repoIDStr)
	if err != nil {
		httputil.WriteError(w, http.StatusNotFound, "repository not found: "+err.Error())
		return
	}
	var sourceID pgtype.UUID
	if err := sourceID.Scan(sourceIDStr); err != nil {
		httputil.WriteError(w, http.StatusBadRequest, "invalid sourceID: "+err.Error())
		return
	}
	src, err := store.New(pool).GetSourceByID(r.Context(), sourceID)
	if err != nil {
		httputil.WriteError(w, http.StatusNotFound, "source not found: "+err.Error())
		return
	}
	failedCount := 0
	if src.ChunkErrors != nil {
		var entries []struct {
			Index int `json:"index"`
		}
		if jerr := json.Unmarshal(src.ChunkErrors, &entries); jerr == nil {
			failedCount = len(entries)
		}
	}
	title := src.Url
	if src.ParsedTitle != nil && *src.ParsedTitle != "" {
		title = *src.ParsedTitle
	}
	httputil.WriteJSON(w, http.StatusOK, reprocessPreviewResponse{
		RepositoryID:     repoIDStr,
		SourceID:         sourceIDStr,
		ChunkFailures:    src.ChunkFailures,
		FailedChunkCount: failedCount,
		SourceTitle:      title,
	})
}

type reprocessSourceResponse struct {
	RepositoryID     string `json:"repository_id"`
	SourceID         string `json:"source_id"`
	EnqueuedJobID    string `json:"enqueued_job_id"`
	RetryChunkCount  int    `json:"retry_chunk_count"`
}

// ReprocessSource handles POST
// /api/v1/admin/repos/{repoID}/sources/{sourceID}/reprocess. Re-runs
// source_decomposition for the FAILED chunks of a source only
// (passed as RetryChunkIndices on the enqueued job). Successful
// chunks from the prior run are not re-LLM'd, so no duplicate fact
// rows are created (CreateFact has no ON CONFLICT; only
// AddFactSource is idempotent — deduplicate_facts would eventually
// clean up but doubles LLM cost).
//
// The failed chunk indices are read from the source row's
// chunk_errors JSONB column (written by source_decomposition when
// in-job retries are exhausted). If chunk_errors is empty the
// endpoint returns 400 (nothing to reprocess) — the operator should
// use the normal /sources/{sourceID}/process endpoint for a full
// re-decomposition.
//
// Gated by repositories.*.manage.
func (a *Admin) ReprocessSource(w http.ResponseWriter, r *http.Request) {
	if a.repoPoolResolver == nil {
		httputil.WriteError(w, http.StatusServiceUnavailable, "repo pool resolver not configured")
		return
	}
	if a.taskEnqueuer == nil {
		httputil.WriteError(w, http.StatusServiceUnavailable, "task enqueuer not configured")
		return
	}
	repoIDStr := chi.URLParam(r, "repoID")
	sourceIDStr := chi.URLParam(r, "sourceID")
	if repoIDStr == "" || sourceIDStr == "" {
		httputil.WriteError(w, http.StatusBadRequest, "repoID and sourceID are required")
		return
	}
	pool, repoID, err := a.repoPoolResolver(r.Context(), repoIDStr)
	if err != nil {
		httputil.WriteError(w, http.StatusNotFound, "repository not found: "+err.Error())
		return
	}
	_ = repoID // repoIDStr is used for the enqueue; repoID is validated by the resolver.
	var sourceID pgtype.UUID
	if err := sourceID.Scan(sourceIDStr); err != nil {
		httputil.WriteError(w, http.StatusBadRequest, "invalid sourceID: "+err.Error())
		return
	}

	queries := store.New(pool)
	src, err := queries.GetSourceByID(r.Context(), sourceID)
	if err != nil {
		httputil.WriteError(w, http.StatusNotFound, "source not found: "+err.Error())
		return
	}

	// Read the failed chunk indices from chunk_errors JSONB.
	if src.ChunkErrors == nil {
		httputil.WriteError(w, http.StatusBadRequest, "source has no recorded chunk failures; use the normal process endpoint for a full re-decomposition")
		return
	}
	var entries []struct {
		Index    int    `json:"index"`
		Type     string `json:"type"`
		Error    string `json:"error"`
		Attempts int    `json:"attempts"`
	}
	if err := json.Unmarshal(src.ChunkErrors, &entries); err != nil {
		httputil.WriteError(w, http.StatusInternalServerError, "parsing chunk_errors: "+err.Error())
		return
	}
	if len(entries) == 0 {
		httputil.WriteError(w, http.StatusBadRequest, "chunk_errors is empty; nothing to reprocess")
		return
	}
	retryIndices := make([]int32, 0, len(entries))
	for _, e := range entries {
		retryIndices = append(retryIndices, int32(e.Index))
	}

	jobID, err := a.taskEnqueuer.EnqueueSourceDecompositionFromHTTP(r.Context(), SourceDecompositionArgs{
		SourceID:          sourceIDStr,
		RepositoryID:      repoIDStr,
		RetryChunkIndices:  retryIndices,
	})
	if err != nil {
		httputil.WriteError(w, http.StatusInternalServerError, "enqueueing source_decomposition: "+err.Error())
		return
	}

	httputil.WriteJSON(w, http.StatusOK, reprocessSourceResponse{
		RepositoryID:    repoIDStr,
		SourceID:        sourceIDStr,
		EnqueuedJobID:   jobID,
		RetryChunkCount: len(retryIndices),
	})
}

// errAdminDisabled is a sentinel for endpoints that require the
// task enqueuer / pool resolver and find them unset.
var errAdminDisabled = errors.New("admin endpoint not configured")

// pgRepoIDFromStr scans a repo UUID string into a pgtype.UUID.
// Panics on invalid input — callers validate via repoPoolResolver
// first, so by the time we call this the UUID is known-good.
func pgRepoIDFromStr(s string) pgtype.UUID {
	var id pgtype.UUID
	if err := id.Scan(s); err != nil {
		panic("pgRepoIDFromStr: invalid UUID " + s)
	}
	return id
}