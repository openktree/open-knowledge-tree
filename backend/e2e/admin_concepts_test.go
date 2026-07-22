//go:build e2e

package e2e_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/openktree/open-knowledge-tree/backend/e2e/testutil"
	"github.com/openktree/open-knowledge-tree/backend/internal/store"
)

// TestAdminReextractRepoConcepts verifies the on-demand concept
// re-extraction endpoint: POST
// /api/v1/admin/repos/{repoID}/concepts/reextract. It clears
// retryable fact_concept_skips (attempts < max_concept_attempts)
// and unresolved fact_candidates for the repo, then enqueues a
// repo-wide extract_concepts job. Used to recover from the
// historical permanent-skip bug.
func TestAdminReextractRepoConcepts(t *testing.T) {
	env := testutil.NewTestEnv(t)
	defer env.Server.Close()
	ensureRiverSchema(t, env.DB)

	admin := bootstrapSysAdmin(t, env, "reextract@example.com")
	_, _, repoID := createRepositoryWithDB(t, admin, "Reextract", "reextract", "desc", "")
	queries := store.New(env.DB)

	// Create a source + a stable fact with a soft-skip row
	// (attempts=1, retryable).
	srcID := pgtype.UUID{}
	if err := srcID.Scan(uuid.NewString()); err != nil {
		t.Fatalf("scan src id: %v", err)
	}
	if _, err := queries.CreateSource(context.Background(), store.CreateSourceParams{
		ID: srcID, RepositoryID: pgRepoID(t, repoID), Url: "https://example.com/s", Kind: "homepage", Status: "fetched",
	}); err != nil {
		t.Fatalf("create source: %v", err)
	}
	factIDStr := insertFactWithSource(t, env, pgRepoID(t, repoID), srcID, "A skipped fact.", 0)
	factID := pgtype.UUID{}
	if err := factID.Scan(factIDStr); err != nil {
		t.Fatalf("scan fact id: %v", err)
	}
	if _, err := env.DB.Exec(context.Background(),
		`UPDATE okt_repository.facts SET status = 'stable' WHERE id = $1`, factID,
	); err != nil {
		t.Fatalf("promote fact: %v", err)
	}
	if _, err := queries.RecordFactConceptSkip(context.Background(), store.RecordFactConceptSkipParams{
		FactID:    factID,
		LastError: "concept extraction: failed to parse response as JSON array: invalid character 'x'",
	}); err != nil {
		t.Fatalf("record skip: %v", err)
	}

	// Sanity: the skip row exists.
	var skipCount int
	if err := env.DB.QueryRow(context.Background(),
		`SELECT count(*) FROM okt_repository.fact_concept_skips WHERE fact_id = $1`, factID,
	).Scan(&skipCount); err != nil {
		t.Fatalf("query skip: %v", err)
	}
	if skipCount != 1 {
		t.Fatalf("setup: skip rows = %d, want 1", skipCount)
	}

	// Hit the admin endpoint. It should clear the retryable skip
	// (attempts=1 < max=3) and enqueue an extract_concepts job.
	resp, raw := admin.do("POST", "/api/v1/admin/repos/"+repoID+"/concepts/reextract", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, raw)
	}
	var out struct {
		RepositoryID      string   `json:"repository_id"`
		ClearedSkips      int64    `json:"cleared_skips"`
		ClearedCandidates int64    `json:"cleared_candidates"`
		EnqueuedJobCount  int      `json:"enqueued_job_count"`
		EnqueuedJobIDs    []string `json:"enqueued_job_ids"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if out.ClearedSkips != 1 {
		t.Errorf("cleared_skips = %d, want 1", out.ClearedSkips)
	}
	if out.EnqueuedJobCount < 1 {
		t.Errorf("enqueued_job_count = %d, want >= 1", out.EnqueuedJobCount)
	}

	// The skip row must be gone.
	if err := env.DB.QueryRow(context.Background(),
		`SELECT count(*) FROM okt_repository.fact_concept_skips WHERE fact_id = $1`, factID,
	).Scan(&skipCount); err != nil {
		t.Fatalf("query skip after: %v", err)
	}
	if skipCount != 0 {
		t.Errorf("skip rows after reextract = %d, want 0 (cleared)", skipCount)
	}

	// The recording enqueuer must have captured at least one
	// extract_concepts call (one per source with unlinked facts).
	if got := env.TaskEnqueuer.ExtractConceptsCount(); got < 1 {
		t.Errorf("extract_concepts enqueue count = %d, want >= 1", got)
	}
	calls := env.TaskEnqueuer.ExtractConceptsSnapshot()
	for _, c := range calls {
		if c.RepositoryID != repoID {
			t.Errorf("enqueue call RepositoryID = %s, want %s", c.RepositoryID, repoID)
		}
		if c.SourceID == "" {
			t.Errorf("enqueue call SourceID is empty; expected per-source jobs, not repo-wide")
		}
	}
}

// TestAdminReextractRepoConcepts_PermissionDenied verifies that a
// non-admin user (no repositories.*.manage permission) gets 403.
func TestAdminReextractRepoConcepts_PermissionDenied(t *testing.T) {
	env := testutil.NewTestEnv(t)
	defer env.Server.Close()
	ensureRiverSchema(t, env.DB)

	admin := bootstrapSysAdmin(t, env, "reextract-admin@example.com")
	_, _, repoID := createRepositoryWithDB(t, admin, "ReextractDeny", "reextract-deny", "desc", "")

	// A regular user (registered, no admin role) must be denied.
	regular := newAuthClient(env.BaseURL)
	regular.register("reextract-deny@example.com", "passw0rd!", "Deny")
	regular.token = loginUser(regular, "reextract-deny@example.com", "passw0rd!")

	resp, _ := regular.do("POST", "/api/v1/admin/repos/"+repoID+"/concepts/reextract", nil)
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("expected 403 for non-admin, got %d", resp.StatusCode)
	}
}

// TestAdminReprocessSource verifies the on-demand source reprocess
// endpoint: POST
// /api/v1/admin/repos/{repoID}/sources/{sourceID}/reprocess. It
// reads the failed chunk indices from the source row's
// chunk_errors JSONB and enqueues a source_decomposition job
// with RetryChunkIndices set, so only the failed chunks are re-LLM'd
// (no duplicate fact rows from successful chunks).
func TestAdminReprocessSource(t *testing.T) {
	env := testutil.NewTestEnv(t)
	defer env.Server.Close()
	ensureRiverSchema(t, env.DB)

	admin := bootstrapSysAdmin(t, env, "reprocess@example.com")
	_, _, repoID := createRepositoryWithDB(t, admin, "Reprocess", "reprocess", "desc", "")
	queries := store.New(env.DB)

	// Create a source with chunk_errors JSONB recording two failed
	// chunks (indices 2 and 5).
	srcID := pgtype.UUID{}
	if err := srcID.Scan(uuid.NewString()); err != nil {
		t.Fatalf("scan src id: %v", err)
	}
	if _, err := queries.CreateSource(context.Background(), store.CreateSourceParams{
		ID: srcID, RepositoryID: pgRepoID(t, repoID), Url: "https://example.com/s", Kind: "homepage", Status: "fetched",
	}); err != nil {
		t.Fatalf("create source: %v", err)
	}
	chunkErrors := []byte(`[{"index":2,"type":"text","error":"concept extraction: timeout","attempts":2},{"index":5,"type":"text","error":"concept extraction: 5xx","attempts":2}]`)
	if _, err := queries.UpdateSourceChunkFailures(context.Background(), store.UpdateSourceChunkFailuresParams{
		ID:            srcID,
		ChunkFailures: 2,
		ChunkErrors:   chunkErrors,
	}); err != nil {
		t.Fatalf("update chunk failures: %v", err)
	}

	// Hit the admin endpoint. It should read chunk_errors, enqueue
	// a source_decomposition job with RetryChunkIndices=[2,5].
	resp, raw := admin.do("POST", "/api/v1/admin/repos/"+repoID+"/sources/"+srcID.String()+"/reprocess", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, raw)
	}
	var out struct {
		RepositoryID    string `json:"repository_id"`
		SourceID        string `json:"source_id"`
		EnqueuedJobID  string `json:"enqueued_job_id"`
		RetryChunkCount int   `json:"retry_chunk_count"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if out.RetryChunkCount != 2 {
		t.Errorf("retry_chunk_count = %d, want 2", out.RetryChunkCount)
	}
	if out.EnqueuedJobID == "" {
		t.Errorf("enqueued_job_id is empty; expected a job id")
	}

	// The recording enqueuer must have captured one
	// source_decomposition call with RetryChunkIndices=[2,5].
	calls := env.TaskEnqueuer.Decompositions
	if len(calls) != 1 {
		t.Fatalf("decomposition enqueue count = %d, want 1", len(calls))
	}
	if calls[0].SourceID != srcID.String() {
		t.Errorf("enqueue SourceID = %s, want %s", calls[0].SourceID, srcID.String())
	}
	if len(calls[0].RetryChunkIndices) != 2 {
		t.Fatalf("RetryChunkIndices len = %d, want 2", len(calls[0].RetryChunkIndices))
	}
	if calls[0].RetryChunkIndices[0] != 2 || calls[0].RetryChunkIndices[1] != 5 {
		t.Errorf("RetryChunkIndices = %v, want [2,5]", calls[0].RetryChunkIndices)
	}
}

// TestAdminReprocessSource_NoChunkErrors verifies that a source
// with no recorded chunk_failures returns 400 (nothing to
// reprocess) — the operator should use the normal /process endpoint
// for a full re-decomposition.
func TestAdminReprocessSource_NoChunkErrors(t *testing.T) {
	env := testutil.NewTestEnv(t)
	defer env.Server.Close()
	ensureRiverSchema(t, env.DB)

	admin := bootstrapSysAdmin(t, env, "reprocess-empty@example.com")
	_, _, repoID := createRepositoryWithDB(t, admin, "ReprocessEmpty", "reprocess-empty", "desc", "")
	queries := store.New(env.DB)

	srcID := pgtype.UUID{}
	if err := srcID.Scan(uuid.NewString()); err != nil {
		t.Fatalf("scan src id: %v", err)
	}
	if _, err := queries.CreateSource(context.Background(), store.CreateSourceParams{
		ID: srcID, RepositoryID: pgRepoID(t, repoID), Url: "https://example.com/s", Kind: "homepage", Status: "fetched",
	}); err != nil {
		t.Fatalf("create source: %v", err)
	}
	// No chunk_errors recorded.

	resp, _ := admin.do("POST", "/api/v1/admin/repos/"+repoID+"/sources/"+srcID.String()+"/reprocess", nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("expected 400 (no chunk failures to reprocess), got %d", resp.StatusCode)
	}
}

// TestAdminRecomputeRepoConceptGroups verifies the on-demand
// concept_groups summary recompute endpoint: GET (preview) + POST
// /api/v1/admin/repos/{repoID}/concepts/recompute. The preview
// returns the current group/concept counts; the POST enqueues a
// recompute_concept_groups River job. A regular user without
// repositories.*.manage gets 403.
func TestAdminRecomputeRepoConceptGroups(t *testing.T) {
	env := testutil.NewTestEnv(t)
	defer env.Server.Close()

	admin := bootstrapSysAdmin(t, env, "recompute@example.com")
	const slug = "recompute-repo"
	_, _, repoID := createRepositoryWithDB(t, admin, "Recompute Repo", slug, "desc", "")
	pgRepo := pgRepoID(t, repoID)
	queries := store.New(env.DB)

	// Insert two concepts directly so the summary has something to
	// count. Direct store writes don't trigger the ingest workers'
	// incremental recompute, so the summary starts empty — this is
	// exactly the scenario the recompute endpoint repairs.
	ctx := context.Background()
	for _, name := range []string{"Alpha", "Beta"} {
		if _, err := queries.CreateConcept(ctx, store.CreateConceptParams{
			RepositoryID: pgRepo, CanonicalName: name, Context: "TestCtx",
		}); err != nil {
			t.Fatalf("create concept %s: %v", name, err)
		}
	}

	// Preview before recompute: 0 groups (direct inserts, no
	// incremental recompute) + 2 concepts.
	pResp, pRaw := admin.do("GET", "/api/v1/admin/repos/"+repoID+"/concepts/recompute", nil)
	if pResp.StatusCode != http.StatusOK {
		t.Fatalf("GET recompute preview: %d %s", pResp.StatusCode, pRaw)
	}
	var preview recomputePreviewResponse
	if err := json.Unmarshal(pRaw, &preview); err != nil {
		t.Fatalf("decode preview: %v", err)
	}
	if preview.ConceptsTotal != 2 {
		t.Errorf("preview concepts_total = %d, want 2", preview.ConceptsTotal)
	}
	if preview.CurrentGroups != 0 {
		t.Errorf("preview current_groups = %d, want 0 (direct inserts don't recompute)", preview.CurrentGroups)
	}

	// POST enqueues the recompute job.
	rResp, rRaw := admin.do("POST", "/api/v1/admin/repos/"+repoID+"/concepts/recompute", nil)
	if rResp.StatusCode != http.StatusOK {
		t.Fatalf("POST recompute: %d %s", rResp.StatusCode, rRaw)
	}
	var resp recomputeResponse
	if err := json.Unmarshal(rRaw, &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if !resp.Enqueued || resp.EnqueuedJobID == "" {
		t.Errorf("response enqueued=%v job_id=%q, want enqueued=true with job id", resp.Enqueued, resp.EnqueuedJobID)
	}

	// A regular user (no repositories.*.manage) gets 403 on both.
	regular := newAuthClient(env.BaseURL)
	regular.register("recompute-user@example.com", "passw0rd!", "Recompute")
	regular.token = loginUser(regular, "recompute-user@example.com", "passw0rd!")
	if r, _ := regular.do("GET", "/api/v1/admin/repos/"+repoID+"/concepts/recompute", nil); r.StatusCode != http.StatusForbidden {
		t.Errorf("regular GET recompute: status %d, want 403", r.StatusCode)
	}
	if r, _ := regular.do("POST", "/api/v1/admin/repos/"+repoID+"/concepts/recompute", nil); r.StatusCode != http.StatusForbidden {
		t.Errorf("regular POST recompute: status %d, want 403", r.StatusCode)
	}
}

type recomputePreviewResponse struct {
	RepositoryID  string `json:"repository_id"`
	CurrentGroups int64  `json:"current_groups"`
	ConceptsTotal int64  `json:"concepts_total"`
}

type recomputeResponse struct {
	RepositoryID  string `json:"repository_id"`
	EnqueuedJobID string `json:"enqueued_job_id"`
	Enqueued      bool   `json:"enqueued"`
}
