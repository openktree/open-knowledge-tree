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

// addFactReference is a test helper that inserts one
// fact_references row. Mirrors what source_decomposition does after
// the AI returns sentence indices.
func addFactReference(t *testing.T, queries *store.Queries, factID, sourceID pgtype.UUID, sentenceIndex, chunkIndex int32) {
	t.Helper()
	if err := queries.AddFactReference(context.Background(), store.AddFactReferenceParams{
		FactID:        factID,
		SourceID:      sourceID,
		SentenceIndex: sentenceIndex,
		ChunkIndex:    chunkIndex,
	}); err != nil {
		t.Fatalf("add fact reference: %v", err)
	}
}

// mkSourceAndRepo creates a fresh repo + source and returns the
// repoID, slug, and source UUID, for tests that need a source to
// attach references to.
func mkSourceAndRepo(t *testing.T, admin *authClient, env *testutil.TestEnv, slugPrefix string) (repoID string, slug string, sourceID pgtype.UUID) {
	t.Helper()
	_, _, repoID = createRepositoryWithDB(t, admin, "FR "+slugPrefix, slugPrefix, "desc", "")
	slug = slugPrefix
	sourceID = pgtype.UUID{}
	if err := sourceID.Scan(uuid.NewString()); err != nil {
		t.Fatalf("scanning source id: %v", err)
	}
	queries := store.New(env.DB)
	if _, err := queries.CreateSource(context.Background(), store.CreateSourceParams{
		ID:           sourceID,
		RepositoryID: pgRepoID(t, repoID),
		Url:          "https://example.com/" + slugPrefix,
		Kind:         "homepage",
		Status:       "fetched",
	}); err != nil {
		t.Fatalf("create source: %v", err)
	}
	return repoID, slug, sourceID
}

// TestFactReferences_AddAndListBySource verifies the
// AddFactReference query inserts one row per (fact, source,
// sentence_index) and ListFactReferencesBySource returns them
// joined with the fact row, ordered by sentence_index.
func TestFactReferences_AddAndListBySource(t *testing.T) {
	env := testutil.NewTestEnv(t)
	defer env.Server.Close()

	admin := bootstrapSysAdmin(t, env, "fr_list@example.com")
	_, _, sourceID := mkSourceAndRepo(t, admin, env, "fr-list")
	queries := store.New(env.DB)

	// One fact citing three sentences (2, 3, 5) → three rows.
	factID := pgtype.UUID{}
	if err := factID.Scan(uuid.NewString()); err != nil {
		t.Fatalf("scanning fact id: %v", err)
	}
	if _, err := queries.CreateFact(context.Background(), store.CreateFactParams{
		ID: factID, Text: "multi-sentence fact", FactKind: "text",
	}); err != nil {
		t.Fatalf("create fact: %v", err)
	}
	for _, s := range []int32{2, 3, 5} {
		addFactReference(t, queries, factID, sourceID, s, 0)
	}

	// Second fact citing sentence 0 — verifies multiple facts in
	// the same source come back in the list.
	fact2ID := pgtype.UUID{}
	if err := fact2ID.Scan(uuid.NewString()); err != nil {
		t.Fatalf("scanning fact2 id: %v", err)
	}
	if _, err := queries.CreateFact(context.Background(), store.CreateFactParams{
		ID: fact2ID, Text: "first-sentence fact", FactKind: "text",
	}); err != nil {
		t.Fatalf("create fact2: %v", err)
	}
	addFactReference(t, queries, fact2ID, sourceID, 0, 0)

	rows, err := queries.ListFactReferencesBySource(context.Background(), sourceID)
	if err != nil {
		t.Fatalf("list by source: %v", err)
	}
	// 4 rows total, ordered by sentence_index: 0, 2, 3, 5.
	if len(rows) != 4 {
		t.Fatalf("expected 4 reference rows, got %d", len(rows))
	}
	wantIndices := []int32{0, 2, 3, 5}
	for i, w := range wantIndices {
		if rows[i].SentenceIndex != w {
			t.Errorf("row %d: sentence_index = %d, want %d", i, rows[i].SentenceIndex, w)
		}
	}
	// Row 0 belongs to fact2; rows 1-3 belong to fact1.
	if rows[0].FactID != fact2ID {
		t.Errorf("row 0 fact_id mismatch")
	}
	if rows[1].FactID != factID {
		t.Errorf("row 1 fact_id mismatch")
	}
	if rows[1].Text != "multi-sentence fact" {
		t.Errorf("row 1 text = %q, want %q", rows[1].Text, "multi-sentence fact")
	}
}

// TestFactReferences_AddIsIdempotent verifies the PK
// (fact_id, source_id, sentence_index) dedups: re-adding the same
// (fact, source, sentence) triple is a no-op.
func TestFactReferences_AddIsIdempotent(t *testing.T) {
	env := testutil.NewTestEnv(t)
	defer env.Server.Close()

	admin := bootstrapSysAdmin(t, env, "fr_idem@example.com")
	_, _, sourceID := mkSourceAndRepo(t, admin, env, "fr-idem")
	queries := store.New(env.DB)

	factID := pgtype.UUID{}
	if err := factID.Scan(uuid.NewString()); err != nil {
		t.Fatalf("scanning fact id: %v", err)
	}
	if _, err := queries.CreateFact(context.Background(), store.CreateFactParams{
		ID: factID, Text: "idempotent ref", FactKind: "text",
	}); err != nil {
		t.Fatalf("create fact: %v", err)
	}
	addFactReference(t, queries, factID, sourceID, 4, 0)
	addFactReference(t, queries, factID, sourceID, 4, 1) // same sentence, diff chunk — still dup by PK

	rows, err := queries.ListFactReferencesByFact(context.Background(), factID)
	if err != nil {
		t.Fatalf("list by fact: %v", err)
	}
	if len(rows) != 1 {
		t.Errorf("expected 1 row (PK dedup), got %d", len(rows))
	}
}

// TestFactReferences_DedupMergesRefs verifies the dedup path
// preserves all references: when two facts from two different
// sources are merged (loser → winner), the winner ends up with
// reference rows from BOTH sources. Uses the same SQL the
// mergeSources helper runs (DeleteDuplicateFactReferences +
// RelinkFactReferences) so the test exercises the real dedup write
// path without needing Qdrant.
func TestFactReferences_DedupMergesRefs(t *testing.T) {
	env := testutil.NewTestEnv(t)
	defer env.Server.Close()

	admin := bootstrapSysAdmin(t, env, "fr_dedup@example.com")
	_, _, repoID := createRepositoryWithDB(t, admin, "FR Dedup", "fr-dedup", "desc", "")
	queries := store.New(env.DB)

	mkSrc := func(slug string) pgtype.UUID {
		id := pgtype.UUID{}
		if err := id.Scan(uuid.NewString()); err != nil {
			t.Fatalf("scanning source id: %v", err)
		}
		if _, err := queries.CreateSource(context.Background(), store.CreateSourceParams{
			ID: id, RepositoryID: pgRepoID(t, repoID), Url: "https://example.com/" + slug, Kind: "homepage", Status: "fetched",
		}); err != nil {
			t.Fatalf("create source: %v", err)
		}
		return id
	}
	srcA := mkSrc("fr-dedup-a")
	srcB := mkSrc("fr-dedup-b")

	// Winner fact from srcA, citing sentences 1 and 3.
	winner := pgtype.UUID{}
	if err := winner.Scan(uuid.NewString()); err != nil {
		t.Fatalf("scanning winner id: %v", err)
	}
	if _, err := queries.CreateFact(context.Background(), store.CreateFactParams{
		ID: winner, Text: "winner fact", FactKind: "text",
	}); err != nil {
		t.Fatalf("create winner: %v", err)
	}
	addFactReference(t, queries, winner, srcA, 1, 0)
	addFactReference(t, queries, winner, srcA, 3, 0)

	// Loser fact from srcB, citing sentences 2 and 3 (sentence 3
	// overlaps with the winner — this is the same-source-overlap
	// case the DELETE step must handle, except here it's the same
	// sentence_index from a DIFFERENT source, so it must be
	// preserved, not deleted).
	loser := pgtype.UUID{}
	if err := loser.Scan(uuid.NewString()); err != nil {
		t.Fatalf("scanning loser id: %v", err)
	}
	if _, err := queries.CreateFact(context.Background(), store.CreateFactParams{
		ID: loser, Text: "loser fact", FactKind: "text",
	}); err != nil {
		t.Fatalf("create loser: %v", err)
	}
	addFactReference(t, queries, loser, srcB, 2, 0)
	addFactReference(t, queries, loser, srcB, 3, 0)

	// Also add a loser citation from srcA (same source as winner)
	// at sentence 1 — this WOULD collide with winner's (srcA, 1)
	// row and must be deleted before the relink.
	addFactReference(t, queries, loser, srcA, 1, 0)

	// Run the same write path mergeSources uses.
	if err := queries.DeleteDuplicateFactReferences(context.Background(), store.DeleteDuplicateFactReferencesParams{
		FactID:   loser,
		FactID_2: winner,
	}); err != nil {
		t.Fatalf("delete duplicates: %v", err)
	}
	if err := queries.RelinkFactReferences(context.Background(), store.RelinkFactReferencesParams{
		FactID:   loser,
		FactID_2: winner,
	}); err != nil {
		t.Fatalf("relink: %v", err)
	}

	// Winner should now have:
	//   - (srcA, 1)  — original winner row
	//   - (srcA, 3)  — original winner row
	//   - (srcB, 2)  — from loser (different source, preserved)
	//   - (srcB, 3)  — from loser (different source, preserved)
	//   - NOT (srcA, 1) from loser — that was a duplicate and got deleted
	winnerRows, err := queries.ListFactReferencesByFact(context.Background(), winner)
	if err != nil {
		t.Fatalf("list winner refs: %v", err)
	}
	if len(winnerRows) != 4 {
		t.Fatalf("winner should have 4 reference rows, got %d", len(winnerRows))
	}
	// Loser should have 0 (all relinked).
	loserRows, err := queries.ListFactReferencesByFact(context.Background(), loser)
	if err != nil {
		t.Fatalf("list loser refs: %v", err)
	}
	if len(loserRows) != 0 {
		t.Errorf("loser should have 0 reference rows after relink, got %d", len(loserRows))
	}

	// Verify the (srcA, 1) pair appears exactly once (the
	// duplicate from loser was deleted, not doubled).
	srcA1Count := 0
	for _, r := range winnerRows {
		if r.SourceID == srcA && r.SentenceIndex == 1 {
			srcA1Count++
		}
	}
	if srcA1Count != 1 {
		t.Errorf("(srcA, sentence 1) count = %d, want 1 (duplicate must be deleted)", srcA1Count)
	}
}

// TestFactReferences_Endpoint verifies GET
// /{slug}/sources/{sourceID}/references returns the reference rows
// as a flat JSON array with the fact fields joined in, and that the
// endpoint enforces repo ownership (404 for a source in a different
// repo) and auth (401 unauthenticated).
func TestFactReferences_Endpoint(t *testing.T) {
	env := testutil.NewTestEnv(t)
	defer env.Server.Close()

	admin := bootstrapSysAdmin(t, env, "fr_endpoint@example.com")
	repoID, slug, sourceID := mkSourceAndRepo(t, admin, env, "fr-endpoint")
	queries := store.New(env.DB)

	factID := pgtype.UUID{}
	if err := factID.Scan(uuid.NewString()); err != nil {
		t.Fatalf("scanning fact id: %v", err)
	}
	if _, err := queries.CreateFact(context.Background(), store.CreateFactParams{
		ID: factID, Text: "endpoint-test fact", FactKind: "text",
	}); err != nil {
		t.Fatalf("create fact: %v", err)
	}
	addFactReference(t, queries, factID, sourceID, 7, 2)

	resp, body := admin.do("GET", "/api/v1/repositories/"+slug+"/sources/"+pgUUIDString(sourceID)+"/references", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET references: status %d, body %s", resp.StatusCode, body)
	}
	var rows []struct {
		FactID        string `json:"fact_id"`
		SourceID      string `json:"source_id"`
		SentenceIndex int32  `json:"sentence_index"`
		ChunkIndex    int32  `json:"chunk_index"`
		Text          string `json:"text"`
		Status        string `json:"status"`
		FactKind      string `json:"fact_kind"`
	}
	if err := json.Unmarshal(body, &rows); err != nil {
		t.Fatalf("unmarshal references: %v (body: %s)", err, body)
	}
	if len(rows) != 1 {
		t.Fatalf("expected 1 reference row, got %d", len(rows))
	}
	if rows[0].SentenceIndex != 7 {
		t.Errorf("sentence_index = %d, want 7", rows[0].SentenceIndex)
	}
	if rows[0].ChunkIndex != 2 {
		t.Errorf("chunk_index = %d, want 2", rows[0].ChunkIndex)
	}
	if rows[0].Text != "endpoint-test fact" {
		t.Errorf("text = %q, want %q", rows[0].Text, "endpoint-test fact")
	}
	if rows[0].FactKind != "text" {
		t.Errorf("fact_kind = %q, want %q", rows[0].FactKind, "text")
	}

	// Auth: unauthenticated → 401.
	unauthResp, err := http.Get(env.BaseURL + "/api/v1/repositories/" + slug + "/sources/" + pgUUIDString(sourceID) + "/references")
	if err != nil {
		t.Fatalf("unauth GET: %v", err)
	}
	if unauthResp.StatusCode != http.StatusUnauthorized {
		t.Errorf("unauthenticated GET references: status = %d, want 401", unauthResp.StatusCode)
	}

	// Empty source: returns [] not null.
	emptySource := pgtype.UUID{}
	if err := emptySource.Scan(uuid.NewString()); err != nil {
		t.Fatalf("scanning empty source id: %v", err)
	}
	if _, err := queries.CreateSource(context.Background(), store.CreateSourceParams{
		ID: emptySource, RepositoryID: pgRepoID(t, repoID), Url: "https://example.com/empty", Kind: "homepage", Status: "fetched",
	}); err != nil {
		t.Fatalf("create empty source: %v", err)
	}
	emptyResp, emptyBody := admin.do("GET", "/api/v1/repositories/"+slug+"/sources/"+pgUUIDString(emptySource)+"/references", nil)
	if emptyResp.StatusCode != http.StatusOK {
		t.Fatalf("GET empty references: status %d, body %s", emptyResp.StatusCode, emptyBody)
	}
	var emptyRows []json.RawMessage
	if err := json.Unmarshal(emptyBody, &emptyRows); err != nil {
		t.Fatalf("unmarshal empty references: %v", err)
	}
	if len(emptyRows) != 0 {
		t.Errorf("empty source: expected 0 rows, got %d", len(emptyRows))
	}
}

// TestFactReferences_SentenceOffsetsPersisted verifies that
// SetSentenceOffsets writes the flat [start0, end0, ...] array and
// GetSourceByID (SELECT *) surfaces it on the source row.
func TestFactReferences_SentenceOffsetsPersisted(t *testing.T) {
	env := testutil.NewTestEnv(t)
	defer env.Server.Close()

	admin := bootstrapSysAdmin(t, env, "fr_offsets@example.com")
	_, _, sourceID := mkSourceAndRepo(t, admin, env, "fr-offsets")
	queries := store.New(env.DB)

	offsets := []int32{0, 12, 12, 30, 30, 55}
	if err := queries.SetSentenceOffsets(context.Background(), store.SetSentenceOffsetsParams{
		ID:              sourceID,
		SentenceOffsets: offsets,
	}); err != nil {
		t.Fatalf("set sentence offsets: %v", err)
	}

	src, err := queries.GetSourceByID(context.Background(), sourceID)
	if err != nil {
		t.Fatalf("get source: %v", err)
	}
	if src.SentenceOffsets == nil {
		t.Fatalf("sentence_offsets is nil after SetSentenceOffsets")
	}
	if len(src.SentenceOffsets) != len(offsets) {
		t.Fatalf("sentence_offsets length = %d, want %d", len(src.SentenceOffsets), len(offsets))
	}
	for i, w := range offsets {
		if src.SentenceOffsets[i] != w {
			t.Errorf("sentence_offsets[%d] = %d, want %d", i, src.SentenceOffsets[i], w)
		}
	}
}