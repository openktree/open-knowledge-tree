//go:build e2e

package e2e_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/openktree/open-knowledge-tree/backend/e2e/testutil"
	"github.com/openktree/open-knowledge-tree/backend/internal/store"
	"github.com/openktree/open-knowledge-tree/backend/internal/taskmanager/tasks"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivertype"
)

// relationRow is the minimal decode of one entry in the relations
// list response (GET /concepts/{conceptID}/relations).
type relationRow struct {
	ConceptID      string `json:"concept_id"`
	CanonicalName  string `json:"canonical_name"`
	SharedFactCount int64 `json:"shared_fact_count"`
}

// relationDetailRow is the minimal decode of one entry in the
// relation-details response (GET /concepts/{conceptID}/relations/
// {otherConceptID}). fact_ids is optional; the test only asserts on
// context + shared_fact_count.
type relationDetailRow struct {
	Context         string   `json:"context"`
	SharedFactCount int64    `json:"shared_fact_count"`
	FactIDs         []string `json:"fact_ids"`
}

// TestConceptRelations_ListAndDetails verifies the relations read
// surface end-to-end:
//   - Two concept groups (A and B) share facts via fact_concepts.
//   - GET /concepts/{conceptIDA}/relations returns B with the
//     correct shared_fact_count, ordered by shared_fact_count DESC.
//   - Self-exclusion: A does not appear in its own relations list.
//   - Symmetry: B's relations list includes A with the same count.
//   - Pagination: limit=1 returns the top relation; offset=1 returns
//     empty when there is only one relation.
//   - total matches the number of distinct related names.
//   - GET /concepts/{conceptIDA}/relations/{conceptIDB} returns one
//     row per context of A with the aggregated shared_fact_count.
//   - 404 for a conceptID with no concept in the repo.
//   - 400 when the two conceptIDs are the same (a concept is not
//     related to itself).
func TestConceptRelations_ListAndDetails(t *testing.T) {
	env := testutil.NewTestEnv(t)
	defer env.Server.Close()

	admin := bootstrapSysAdmin(t, env, "relations@example.com")
	const slug = "relations-repo"
	_, _, repoID := createRepositoryWithDB(t, admin, "Relations Repo", slug, "desc", "")
	pgRepo := pgRepoID(t, repoID)
	queries := store.New(env.DB)

	// One source in the repo.
	srcID := pgtype.UUID{}
	if err := srcID.Scan(uuid.NewString()); err != nil {
		t.Fatalf("scan src id: %v", err)
	}
	if _, err := queries.CreateSource(context.Background(), store.CreateSourceParams{
		ID: srcID, RepositoryID: pgRepo, Url: "https://example.com/rel-src", Kind: "homepage", Status: "fetched",
	}); err != nil {
		t.Fatalf("create source: %v", err)
	}

	// Build three concept groups:
	//   A "Trump" with two contexts (Politician, Person).
	//   B "Musk" with one context (Person).
	//   C "DNA" with one context (Biomolecule) — unrelated to A, for
	//     the self-exclusion / ordering assertions.
	// Facts are linked so that A and B share 3 facts, A and C share 0,
	// and B and C share 0. The 3 shared facts are split across A's
	// contexts (2 Politician, 1 Person) to exercise the per-context
	// aggregation in the details endpoint.
	mkConcept := func(name, ctx string) pgtype.UUID {
		c, err := queries.CreateConcept(context.Background(), store.CreateConceptParams{
			RepositoryID: pgRepo, CanonicalName: name, Context: ctx,
		})
		if err != nil {
			t.Fatalf("create concept %s/%s: %v", name, ctx, err)
		}
		return c.ID
	}
	linkFact := func(conceptID pgtype.UUID, chunk int32) pgtype.UUID {
		fidStr := insertFactWithSource(t, env, pgRepo, srcID, "A fact linking concepts.", chunk)
		fid := pgtype.UUID{}
		if err := fid.Scan(fidStr); err != nil {
			t.Fatalf("scan fact id: %v", err)
		}
		if _, err := queries.AddFactConcept(context.Background(), store.AddFactConceptParams{
			FactID: fid, ConceptID: conceptID,
		}); err != nil {
			t.Fatalf("link fact→concept: %v", err)
		}
		return fid
	}
	linkExistingFact := func(fid, conceptID pgtype.UUID) {
		if _, err := queries.AddFactConcept(context.Background(), store.AddFactConceptParams{
			FactID: fid, ConceptID: conceptID,
		}); err != nil {
			t.Fatalf("link existing fact→concept: %v", err)
		}
	}

	trumpPol := mkConcept("Trump", "Politician")
	muskPer := mkConcept("Musk", "Person")
	dnaBio := mkConcept("DNA", "Biomolecule")

	// Shared fact 1: Trump/Politician + Musk/Person.
	f1 := linkFact(trumpPol, 0)
	linkExistingFact(f1, muskPer)
	// Shared fact 2: Trump/Person + Musk/Person. (A second Trump
	// context row is created so the details endpoint has two
	// contexts to aggregate over.)
	trumpPer := mkConcept("Trump", "Person")
	f2 := linkFact(trumpPer, 1)
	linkExistingFact(f2, muskPer)
	// Shared fact 3: Trump/Politician + Musk/Person (drives the
	// per-context aggregation: Politician context shares 2 with Musk).
	f3 := linkFact(trumpPol, 2)
	linkExistingFact(f3, muskPer)
	// Unrelated fact for DNA only (no shared facts with Trump or Musk).
	linkFact(dnaBio, 3)

	// Refresh the matview synchronously so the list endpoint sees the
	// just-inserted links without waiting for the async refresh task.
	// (The refresh_concept_relations worker is the production path;
	// tests bypass it for determinism.)
	if _, err := env.DB.Exec(context.Background(),
		`REFRESH MATERIALIZED VIEW okt_repository.concept_relations`); err != nil {
		t.Fatalf("refresh concept_relations matview: %v", err)
	}

	trumpIDStr := pgUUIDString(trumpPol)
	muskIDStr := pgUUIDString(muskPer)

	// GET /concepts/{trumpID}/relations returns Musk (3 shared
	// facts), not Trump itself, not DNA (0 shared).
	resp, raw := admin.do("GET", "/api/v1/repositories/"+slug+"/concepts/"+trumpIDStr+"/relations", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET relations: %d %s", resp.StatusCode, raw)
	}
	var list pageEnvelope
	if err := json.Unmarshal(raw, &list); err != nil {
		t.Fatalf("decode relations list: %v", err)
	}
	if list.Total != 1 {
		t.Errorf("relations total = %d, want 1 (only Musk shares facts with Trump)", list.Total)
	}
	rows := decodeRelationRows(t, list.Data)
	if len(rows) != 1 {
		t.Fatalf("relations rows = %d, want 1", len(rows))
	}
	if rows[0].CanonicalName != "Musk" {
		t.Errorf("first relation canonical_name = %q, want %q", rows[0].CanonicalName, "Musk")
	}
	if rows[0].SharedFactCount != 3 {
		t.Errorf("first relation shared_fact_count = %d, want 3", rows[0].SharedFactCount)
	}

	// Musk's relations list must include Trump (symmetric) with the
	// same shared_fact_count.
	mResp, mRaw := admin.do("GET", "/api/v1/repositories/"+slug+"/concepts/"+muskIDStr+"/relations", nil)
	if mResp.StatusCode != http.StatusOK {
		t.Fatalf("GET musk relations: %d %s", mResp.StatusCode, mRaw)
	}
	var mList pageEnvelope
	if err := json.Unmarshal(mRaw, &mList); err != nil {
		t.Fatalf("decode musk relations: %v", err)
	}
	mRows := decodeRelationRows(t, mList.Data)
	if len(mRows) != 1 || mRows[0].CanonicalName != "Trump" || mRows[0].SharedFactCount != 3 {
		t.Errorf("musk relations = %+v, want one row {Trump, 3}", mRows)
	}

	// Pagination: limit=1 returns the top relation (Musk).
	p1Resp, p1Raw := admin.do("GET", "/api/v1/repositories/"+slug+"/concepts/"+trumpIDStr+"/relations?limit=1&offset=0", nil)
	if p1Resp.StatusCode != http.StatusOK {
		t.Fatalf("GET relations limit=1: %d %s", p1Resp.StatusCode, p1Raw)
	}
	var p1List pageEnvelope
	if err := json.Unmarshal(p1Raw, &p1List); err != nil {
		t.Fatalf("decode p1: %v", err)
	}
	if len(decodeRelationRows(t, p1List.Data)) != 1 {
		t.Errorf("limit=1 should return 1 row")
	}
	// offset=1 returns empty (only one relation total).
	p2Resp, p2Raw := admin.do("GET", "/api/v1/repositories/"+slug+"/concepts/"+trumpIDStr+"/relations?limit=1&offset=1", nil)
	if p2Resp.StatusCode != http.StatusOK {
		t.Fatalf("GET relations offset=1: %d %s", p2Resp.StatusCode, p2Raw)
	}
	var p2List pageEnvelope
	if err := json.Unmarshal(p2Raw, &p2List); err != nil {
		t.Fatalf("decode p2: %v", err)
	}
	if len(decodeRelationRows(t, p2List.Data)) != 0 {
		t.Errorf("offset=1 should return 0 rows (only one relation total), got %d", len(decodeRelationRows(t, p2List.Data)))
	}

	// Details: GET /concepts/{trumpID}/relations/{muskID} returns
	// one row per context of Trump. Politician shares 2 (f1, f3),
	// Person shares 1 (f2).
	dResp, dRaw := admin.do("GET", "/api/v1/repositories/"+slug+"/concepts/"+trumpIDStr+"/relations/"+muskIDStr, nil)
	if dResp.StatusCode != http.StatusOK {
		t.Fatalf("GET relation details: %d %s", dResp.StatusCode, dRaw)
	}
	var dList pageEnvelope
	if err := json.Unmarshal(dRaw, &dList); err != nil {
		t.Fatalf("decode details: %v", err)
	}
	details := decodeRelationDetailRows(t, dList.Data)
	if len(details) != 2 {
		t.Fatalf("details rows = %d, want 2 (one per Trump context)", len(details))
	}
	wantByCtx := map[string]int64{"Politician": 2, "Person": 1}
	seen := map[string]int64{}
	for _, d := range details {
		seen[d.Context] = d.SharedFactCount
		if d.SharedFactCount == 0 {
			t.Errorf("context %q shared_fact_count = 0, want > 0", d.Context)
		}
	}
	for ctx, want := range wantByCtx {
		if got, ok := seen[ctx]; !ok {
			t.Errorf("details missing context %q (got %v)", ctx, seen)
		} else if got != want {
			t.Errorf("context %q shared_fact_count = %d, want %d", ctx, got, want)
		}
	}

	// 404 for a conceptID with no concept in this repo.
	nonexistentID := uuid.NewString()
	nfResp, _ := admin.do("GET", "/api/v1/repositories/"+slug+"/concepts/"+nonexistentID+"/relations", nil)
	if nfResp.StatusCode != http.StatusNotFound {
		t.Errorf("GET relations for nonexistent conceptID: status %d, want 404", nfResp.StatusCode)
	}

	// 404 when the otherConceptID doesn't exist.
	nf2Resp, _ := admin.do("GET", "/api/v1/repositories/"+slug+"/concepts/"+trumpIDStr+"/relations/"+nonexistentID, nil)
	if nf2Resp.StatusCode != http.StatusNotFound {
		t.Errorf("GET relation details for nonexistent otherConceptID: status %d, want 404", nf2Resp.StatusCode)
	}

	// 400 when the two conceptIDs are the same (a concept is not
	// related to itself).
	selfResp, _ := admin.do("GET", "/api/v1/repositories/"+slug+"/concepts/"+trumpIDStr+"/relations/"+trumpIDStr, nil)
	if selfResp.StatusCode != http.StatusBadRequest {
		t.Errorf("GET relation details self-pair: status %d, want 400", selfResp.StatusCode)
	}
}

// TestConceptRelations_Unauthenticated verifies the relations
// endpoints return 401 without a session, mirroring the other concept
// endpoint auth checks. AuthRequired runs after WithRepoQueries, so
// the pool resolves first and then the request 401s before the
// handler's existence check runs.
func TestConceptRelations_Unauthenticated(t *testing.T) {
	env := testutil.NewTestEnv(t)
	defer env.Server.Close()

	admin := bootstrapSysAdmin(t, env, "relunauth@example.com")
	_, _, _ = createRepositoryWithDB(t, admin, "RelUnauth", "rel-unauth", "desc", "")

	anon := newAuthClient(env.BaseURL)
	resp, _ := anon.do("GET", "/api/v1/repositories/rel-unauth/concepts/"+uuid.NewString()+"/relations", nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("GET relations without auth: status %d, want 401", resp.StatusCode)
	}
}

func decodeRelationRows(t *testing.T, data []json.RawMessage) []relationRow {
	t.Helper()
	out := make([]relationRow, 0, len(data))
	for _, raw := range data {
		var r relationRow
		if err := json.Unmarshal(raw, &r); err != nil {
			t.Fatalf("decode relation row: %v", err)
		}
		out = append(out, r)
	}
	return out
}

func decodeRelationDetailRows(t *testing.T, data []json.RawMessage) []relationDetailRow {
	t.Helper()
	out := make([]relationDetailRow, 0, len(data))
	for _, raw := range data {
		var r relationDetailRow
		if err := json.Unmarshal(raw, &r); err != nil {
			t.Fatalf("decode relation detail row: %v", err)
		}
		out = append(out, r)
	}
	return out
}

// TestConceptRelations_RefreshUniqueStateExcludesCompleted is a
// regression test for the bug where the concept_relations matview
// refreshed exactly once at boot and never again. Root cause:
// RefreshConceptRelationsArgs used UniqueOpts{ByArgs:true,
// ByQueue:true} without an explicit ByState, so River's default
// unique state set — which INCLUDES `completed` — kept the finished
// refresh's row blocking every subsequent enqueue for the same
// database until the job cleaner eventually purged it (often many
// hours). With a 10-minute periodic interval, the matview stayed
// stale indefinitely.
//
// The fix sets ByState to {available,pending,running,scheduled,
// retryable}, excluding `completed` and `discarded`, so a finished
// refresh frees the unique slot immediately and the next periodic
// tick can enqueue a fresh one.
//
// This test drives a REAL River client (not rivertest.NewWorker,
// which sets DisableUniqueEnforcement=true and would defeat the
// assertion) to verify:
//
//  1. After a refresh job for DB "default" completes, a second
//     enqueue for the same DB is NOT deduped (UniqueSkippedAs-
//     Duplicate=false) — the completed row no longer blocks.
//  3. While a refresh job is still `available` (not yet worked), a
//     concurrent enqueue for the same DB IS deduped
//     (UniqueSkippedAsDuplicate=true) — coalescing of concurrent
//     bursts still works.
func TestConceptRelations_RefreshUniqueStateExcludesCompleted(t *testing.T) {
	env := testutil.NewTestEnv(t)
	defer env.Server.Close()
	ensureRiverSchema(t, env.DB)

	registry := testutil.NewForTestPool(env.DB)

	worker := tasks.NewRefreshConceptRelationsWorker(registry)
	driver := riverpgxv5.New(env.DB)
	workers := river.NewWorkers()
	river.AddWorker(workers, worker)

	client, err := river.NewClient(driver, &river.Config{
		Queues: map[string]river.QueueConfig{
			tasks.QueueRefreshConceptRelations: {MaxWorkers: 1},
		},
		Workers:           workers,
		FetchPollInterval: 100 * time.Millisecond,
		// JobTimeout short: the refresh against the test DB is a
		// no-op (the matview exists but is empty), so a second is
		// plenty and keeps a stuck worker from hanging the test.
		JobTimeout: 5 * time.Second,
	})
	if err != nil {
		t.Fatalf("creating river client: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := client.Start(ctx); err != nil {
		t.Fatalf("starting river client: %v", err)
	}
	defer client.Stop(ctx) //nolint:errcheck // best-effort on shutdown

	// --- Step 1: enqueue + complete a refresh for "default". ---
	res1, err := client.Insert(ctx, tasks.RefreshConceptRelationsArgs{
		DatabaseName: "default",
	}, nil)
	if err != nil {
		t.Fatalf("insert first refresh: %v", err)
	}
	if res1.UniqueSkippedAsDuplicate {
		t.Fatalf("first insert was deduped; expected a real enqueue (fresh queue)")
	}
	jobID1 := res1.Job.ID

	// Wait for the worker to complete it. The matview refresh against
	// the empty test DB is near-instant; poll until completed.
	completed := false
	for deadline := time.Now().Add(15 * time.Second); time.Now().Before(deadline); {
		var state string
		if err := env.DB.QueryRow(ctx,
			`SELECT state FROM okt_system.river_job WHERE id = $1`, jobID1,
		).Scan(&state); err != nil {
			t.Fatalf("querying job state: %v", err)
		}
		if rivertype.JobState(state) == rivertype.JobStateCompleted {
			completed = true
			break
		}
		time.Sleep(150 * time.Millisecond)
	}
	if !completed {
		t.Fatalf("first refresh job %d never reached completed within timeout", jobID1)
	}

	// --- Step 2: enqueue a second refresh for the same DB. ---
	// The fix: a `completed` job must NOT block this enqueue. Under
	// the old code (default ByState including completed), this insert
	// would return UniqueSkippedAsDuplicate=true and the matview would
	// never refresh again. It must now be a real enqueue.
	res2, err := client.Insert(ctx, tasks.RefreshConceptRelationsArgs{
		DatabaseName: "default",
	}, nil)
	if err != nil {
		t.Fatalf("insert second refresh after completion: %v", err)
	}
	if res2.UniqueSkippedAsDuplicate {
		t.Errorf("second refresh was deduped by the completed first job; "+
			"the unique ByState set must exclude `completed` so a finished "+
			"refresh frees the slot. Got UniqueSkippedAsDuplicate=true (job id %d).", jobID1)
	}

	// Wait for res2 to complete so the unique slot is free again
	// before step 3. Otherwise res2 (still active) would coalesce
	// step 3's insert and we'd conflate "completed doesn't block"
	// with "active coalesces."
	jobID2 := res2.Job.ID
	completed2 := false
	for deadline := time.Now().Add(15 * time.Second); time.Now().Before(deadline); {
		var state string
		if err := env.DB.QueryRow(ctx,
			`SELECT state FROM okt_system.river_job WHERE id = $1`, jobID2,
		).Scan(&state); err != nil {
			t.Fatalf("querying job state: %v", err)
		}
		if rivertype.JobState(state) == rivertype.JobStateCompleted {
			completed2 = true
			break
		}
		time.Sleep(150 * time.Millisecond)
	}
	if !completed2 {
		t.Fatalf("second refresh job %d never reached completed within timeout", jobID2)
	}

	// --- Step 3: coalescing of concurrent active jobs still works. ---
	// Insert a refresh scheduled far in the future so the worker
	// won't pick it up; it stays in the `scheduled` state, which is
	// in our ByState set. An immediate insert for the same DB must
	// then be deduped (UniqueSkippedAsDuplicate=true) — proving the
	// active-state coalescing that bursts of extract_concepts batches
	// rely on is still intact. This guards against accidentally
	// removing dedup entirely when excluding `completed`.
	future := time.Now().Add(1 * time.Hour)
	res4, err := client.Insert(ctx, tasks.RefreshConceptRelationsArgs{
		DatabaseName: "default",
	}, &river.InsertOpts{
		Queue:       tasks.QueueRefreshConceptRelations,
		ScheduledAt: future,
	})
	if err != nil {
		t.Fatalf("insert scheduled refresh: %v", err)
	}
	if res4.UniqueSkippedAsDuplicate {
		t.Fatalf("scheduled insert was deduped; expected a real enqueue (completed no longer blocks)")
	}

	// A fifth enqueue for the same DB while the fourth is `scheduled`
	// (an active state in our ByState set) must be deduped.
	res5, err := client.Insert(ctx, tasks.RefreshConceptRelationsArgs{
		DatabaseName: "default",
	}, nil)
	if err != nil {
		t.Fatalf("insert fifth refresh: %v", err)
	}
	if !res5.UniqueSkippedAsDuplicate {
		t.Errorf("expected the fifth insert to be deduped by the scheduled "+
			"fourth job (active-state coalescing), but it was enqueued. "+
			"Concurrent bursts must still coalesce. scheduled job id=%d", res4.Job.ID)
	}
}

// TestConceptRelations_RefreshSweepStaleCompleted validates the
// self-healing sweep that clears stale unique-key slots. It
// reproduces the production failure mode: a completed
// refresh_concept_relations row carries a legacy unique_states
// bitmask that includes `completed` (bit 2 set), so the partial
// unique index still considers it "active" and every subsequent
// enqueue for the same database is dropped as a duplicate.
//
// The sweep (Manager.SweepStaleUniqueKeyJobs) deletes finalized
// rows whose state is still covered by their stored bitmask. This
// test inserts a completed row with the legacy 0xFF bitmask (all
// 8 states), verifies the slot is blocked, runs the sweep, then
// verifies the slot is freed.
func TestConceptRelations_RefreshSweepStaleCompleted(t *testing.T) {
	env := testutil.NewTestEnv(t)
	defer env.Server.Close()
	ensureRiverSchema(t, env.DB)

	registry := testutil.NewForTestPool(env.DB)
	worker := tasks.NewRefreshConceptRelationsWorker(registry)
	driver := riverpgxv5.New(env.DB)
	workers := river.NewWorkers()
	river.AddWorker(workers, worker)

	client, err := river.NewClient(driver, &river.Config{
		Queues: map[string]river.QueueConfig{
			tasks.QueueRefreshConceptRelations: {MaxWorkers: 1},
		},
		Workers:           workers,
		FetchPollInterval: 100 * time.Millisecond,
		JobTimeout:        5 * time.Second,
	})
	if err != nil {
		t.Fatalf("creating river client: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	if err := client.Start(ctx); err != nil {
		t.Fatalf("starting river client: %v", err)
	}
	defer client.Stop(ctx) //nolint:errcheck

	// --- Step 1: insert a refresh and wait for it to complete. ---
	res1, err := client.Insert(ctx, tasks.RefreshConceptRelationsArgs{
		DatabaseName: "default",
	}, nil)
	if err != nil {
		t.Fatalf("insert first refresh: %v", err)
	}
	if res1.UniqueSkippedAsDuplicate {
		t.Fatalf("first insert was deduped; expected a real enqueue")
	}
	jobID1 := res1.Job.ID

	completed := false
	for deadline := time.Now().Add(15 * time.Second); time.Now().Before(deadline); {
		var state string
		if err := env.DB.QueryRow(ctx,
			`SELECT state FROM river_job WHERE id = $1`, jobID1,
		).Scan(&state); err != nil {
			t.Fatalf("querying job state: %v", err)
		}
		if rivertype.JobState(state) == rivertype.JobStateCompleted {
			completed = true
			break
		}
		time.Sleep(150 * time.Millisecond)
	}
	if !completed {
		t.Fatalf("first refresh job %d never reached completed within timeout", jobID1)
	}

	// --- Step 2: corrupt the completed row's unique_states to the ---
	// --- legacy bitmask that includes `completed` (bit 2).      ---
	// Legacy default 0xFF = all 8 states; bit 2 (value 4) is `completed`.
	// After this, river_job_state_in_bitmask(0xFF, 'completed') = true,
	// so the partial unique index holds the slot.
	_, err = env.DB.Exec(ctx,
		`UPDATE river_job SET unique_states = 255::bit(8) WHERE id = $1`, jobID1)
	if err != nil {
		t.Fatalf("corrupting unique_states: %v", err)
	}

	// --- Step 3: a second enqueue for the same DB must be deduped ---
	// --- (blocked by the stale bitmask).                         ---
	res2, err := client.Insert(ctx, tasks.RefreshConceptRelationsArgs{
		DatabaseName: "default",
	}, nil)
	if err != nil {
		t.Fatalf("insert second refresh: %v", err)
	}
	if !res2.UniqueSkippedAsDuplicate {
		t.Fatal("second insert was NOT deduped; expected the stale completed " +
			"row's bitmask (0xFF including completed) to block re-enqueue")
	}

	// --- Step 4: run the sweep (same SQL as                   ---
	// --- Manager.SweepStaleUniqueKeyJobs).                      ---
	swept, err := env.DB.Exec(ctx, `
		DELETE FROM river_job
		 WHERE unique_key IS NOT NULL
		   AND unique_states IS NOT NULL
		   AND state IN ('completed', 'cancelled', 'discarded')
		   AND kind IN ('refresh_concept_relations', 'refresh_all_concept_relations')
		   AND river_job_state_in_bitmask(unique_states, state)`)
	if err != nil {
		t.Fatalf("sweep stale unique key jobs: %v", err)
	}
	if swept.RowsAffected() != 1 {
		t.Fatalf("sweep deleted %d rows, want 1 (the corrupted completed row)", swept.RowsAffected())
	}

	// --- Step 5: a third enqueue must now succeed (not deduped). ---
	res3, err := client.Insert(ctx, tasks.RefreshConceptRelationsArgs{
		DatabaseName: "default",
	}, nil)
	if err != nil {
		t.Fatalf("insert third refresh: %v", err)
	}
	if res3.UniqueSkippedAsDuplicate {
		t.Fatal("third insert was deduped; expected a fresh enqueue after " +
			"sweep freed the unique-key slot")
	}
}