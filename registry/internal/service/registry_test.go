package service

import (
	"context"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/openktree/knowledge-registry/internal/model"
	"github.com/openktree/knowledge-registry/internal/storage"
	"github.com/openktree/knowledge-registry/internal/store"
)

// mockStorage is a test double for the Storage interface. It records
// StoreJSON calls and can optionally delay to simulate S3 latency so
// tests can verify the fire-and-forget path returns before the
// storage write completes.
type mockStorage struct {
	mu           sync.Mutex
	storeCalls   []storeCall
	storeDelay   time.Duration
	storeDone    chan struct{}
	presignError error // when non-nil, PresignedURL/PresignedPUTURL return this
}

type storeCall struct {
	Key  string
	Data []byte
}

func (m *mockStorage) StoreJSON(ctx context.Context, key string, data []byte) error {
	return m.Store(ctx, key, data, "application/json")
}

func (m *mockStorage) Store(ctx context.Context, key string, data []byte, contentType string) error {
	if m.storeDelay > 0 {
		time.Sleep(m.storeDelay)
	}
	m.mu.Lock()
	m.storeCalls = append(m.storeCalls, storeCall{Key: key, Data: data})
	m.mu.Unlock()
	if m.storeDone != nil {
		select {
		case m.storeDone <- struct{}{}:
		default:
		}
	}
	return nil
}

// StoreStream reads body into a []byte and delegates to Store so the
// mock records the call the same way. Real backends stream; the mock
// buffers since tests use small payloads.
func (m *mockStorage) StoreStream(ctx context.Context, key string, body io.Reader, contentType string) (int64, error) {
	data, err := io.ReadAll(body)
	if err != nil {
		return 0, err
	}
	if err := m.Store(ctx, key, data, contentType); err != nil {
		return 0, err
	}
	return int64(len(data)), nil
}

func (m *mockStorage) Delete(ctx context.Context, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := m.storeCalls[:0]
	for _, c := range m.storeCalls {
		if c.Key != key {
			out = append(out, c)
		}
	}
	m.storeCalls = out
	return nil
}

func (m *mockStorage) ReadAll(ctx context.Context, key string) ([]byte, string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, c := range m.storeCalls {
		if c.Key == key {
			return c.Data, "application/json", nil
		}
	}
	return nil, "", nil
}

func (m *mockStorage) PresignedURL(ctx context.Context, key string, ttl time.Duration) (string, error) {
	if m.presignError != nil {
		return "", m.presignError
	}
	return "http://mock/" + key, nil
}

func (m *mockStorage) PresignedPUTURL(ctx context.Context, key string, ttl time.Duration) (string, error) {
	if m.presignError != nil {
		return "", m.presignError
	}
	return "http://mock/" + key, nil
}

func (m *mockStorage) getCalls() []storeCall {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]storeCall, len(m.storeCalls))
	copy(out, m.storeCalls)
	return out
}

// newTestRegistry builds a Registry with an in-memory SQLite store
// and a mock storage. The caller can configure the mock's delay.
// Promptset validation is disabled (the legacy default).
func newTestRegistry(t *testing.T, storeDelay time.Duration) (*Registry, *mockStorage) {
	return newTestRegistryWithValidation(t, storeDelay, false)
}

// newTestRegistryWithValidation builds a Registry with the given
// promptset-validation flag. Used by the promptset_hash tests to
// exercise both the legacy (off) and enforcing (on) behaviors.
func newTestRegistryWithValidation(t *testing.T, storeDelay time.Duration, validation bool) (*Registry, *mockStorage) {
	t.Helper()
	s, err := store.NewSQLiteStore("file::memory:?cache=shared&_pragma=busy_timeout=5000")
	if err != nil {
		t.Fatalf("creating sqlite: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	if err := s.CreateRepository(context.Background(), &model.Repository{
		ID:        "default",
		Name:      "Test",
		CreatedAt: time.Now().UTC(),
		UpdatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("creating default repo: %v", err)
	}
	ms := &mockStorage{storeDelay: storeDelay}
	r := New(s, ms, 3600, 0, 0, validation)
	return r, ms
}

func makeFacts(n int) []model.FactData {
	facts := make([]model.FactData, n)
	for i := range facts {
		facts[i] = model.FactData{
			ID:      "fact-" + string(rune('a'+i)),
			Content: "fact content " + string(rune('a'+i)),
		}
	}
	return facts
}

// TestPushDecomposition_BatchFactHashes verifies the batch fact-hash
// upsert inserts all facts on the first push and re-links them on
// the second push (no duplicates).
func TestPushDecomposition_BatchFactHashes(t *testing.T) {
	r, _ := newTestRegistry(t, 0)
	ctx := context.Background()

	// Push source first (required for decomposition FK).
	_, err := r.PushSource(ctx, &model.SourceData{
		ID:    "src-1",
		URL:   "http://example.com/test",
		Title: "Test Source",
	})
	if err != nil {
		t.Fatalf("PushSource: %v", err)
	}

	decomp := &model.DecompositionPackage{
		ModelID: "test-model",
		Facts:   makeFacts(100),
	}

	// First push: all 100 should be new.
	res, err := r.PushDecomposition(ctx, "src-1", decomp)
	if err != nil {
		t.Fatalf("PushDecomposition (first): %v", err)
	}
	if res.FactsNew != 100 {
		t.Errorf("first push: expected FactsNew=100, got %d", res.FactsNew)
	}
	if res.FactsLinked != 0 {
		t.Errorf("first push: expected FactsLinked=0, got %d", res.FactsLinked)
	}

	// Give the fire-and-forget goroutine time to finish.
	time.Sleep(50 * time.Millisecond)

	// Second push of the same facts: all should be linked, none new.
	res, err = r.PushDecomposition(ctx, "src-1", decomp)
	if err != nil {
		t.Fatalf("PushDecomposition (second): %v", err)
	}
	if res.FactsNew != 0 {
		t.Errorf("second push: expected FactsNew=0, got %d", res.FactsNew)
	}
	if res.FactsLinked != 100 {
		t.Errorf("second push: expected FactsLinked=100, got %d", res.FactsLinked)
	}
}

// TestPushDecomposition_DuplicateFactHashes verifies that a
// decomposition containing duplicate fact texts (same content_hash
// appearing multiple times) does not trigger a PK constraint failure.
// The batch upsert must track hashes inserted earlier in the same
// loop so the second occurrence takes the link path.
func TestPushDecomposition_DuplicateFactHashes(t *testing.T) {
	r, _ := newTestRegistry(t, 0)
	ctx := context.Background()

	_, err := r.PushSource(ctx, &model.SourceData{
		ID:    "src-dup",
		URL:   "http://example.com/dup",
		Title: "Dup Source",
	})
	if err != nil {
		t.Fatalf("PushSource: %v", err)
	}

	// 10 facts, but 3 are duplicated (same content → same hash).
	decomp := &model.DecompositionPackage{
		ModelID: "test-model",
		Facts: []model.FactData{
			{ID: "f1", Content: "duplicate text"},
			{ID: "f2", Content: "unique text 2"},
			{ID: "f3", Content: "duplicate text"}, // dup of f1
			{ID: "f4", Content: "unique text 4"},
			{ID: "f5", Content: "unique text 5"},
			{ID: "f6", Content: "duplicate text"}, // dup of f1
			{ID: "f7", Content: "unique text 7"},
			{ID: "f8", Content: "unique text 2"}, // dup of f2
			{ID: "f9", Content: "unique text 9"},
			{ID: "f10", Content: "unique text 10"},
		},
	}

	res, err := r.PushDecomposition(ctx, "src-dup", decomp)
	if err != nil {
		t.Fatalf("PushDecomposition with duplicates: %v", err)
	}
	// 7 unique hashes → new. 3 duplicates → linked.
	if res.FactsNew != 7 {
		t.Errorf("expected FactsNew=7 (unique hashes), got %d", res.FactsNew)
	}
	if res.FactsLinked != 3 {
		t.Errorf("expected FactsLinked=3 (duplicates), got %d", res.FactsLinked)
	}
}

// TestPushDecomposition_EmptyFacts verifies a decomposition with no
// facts doesn't error and returns zero counts.
func TestPushDecomposition_EmptyFacts(t *testing.T) {
	r, _ := newTestRegistry(t, 0)
	ctx := context.Background()

	_, err := r.PushSource(ctx, &model.SourceData{
		ID:    "src-empty",
		URL:   "http://example.com/empty",
		Title: "Empty Source",
	})
	if err != nil {
		t.Fatalf("PushSource: %v", err)
	}

	res, err := r.PushDecomposition(ctx, "src-empty", &model.DecompositionPackage{
		ModelID: "test-model",
		Facts:   []model.FactData{},
	})
	if err != nil {
		t.Fatalf("PushDecomposition (empty): %v", err)
	}
	if res.FactsNew != 0 || res.FactsLinked != 0 {
		t.Errorf("empty push: expected 0/0, got new=%d linked=%d", res.FactsNew, res.FactsLinked)
	}
}

// TestPushDecomposition_AsyncS3 verifies the HTTP-equivalent call
// returns before the S3 write completes (fire-and-forget). Uses a
// mock storage with a 200ms artificial delay and asserts the
// PushDecomposition call returns in well under that.
func TestPushDecomposition_AsyncS3(t *testing.T) {
	r, ms := newTestRegistry(t, 200*time.Millisecond)
	ctx := context.Background()

	_, err := r.PushSource(ctx, &model.SourceData{
		ID:    "src-async",
		URL:   "http://example.com/async",
		Title: "Async Source",
	})
	if err != nil {
		t.Fatalf("PushSource: %v", err)
	}

	decomp := &model.DecompositionPackage{
		ModelID: "test-model",
		Facts:   makeFacts(10),
	}

	start := time.Now()
	_, err = r.PushDecomposition(ctx, "src-async", decomp)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("PushDecomposition: %v", err)
	}
	// Should return well before the 200ms storage delay.
	if elapsed >= 200*time.Millisecond {
		t.Errorf("PushDecomposition took %v (expected <200ms with async S3)", elapsed)
	}

	// Wait for the goroutine to finish and verify the storage was called.
	time.Sleep(300 * time.Millisecond)
	calls := ms.getCalls()
	// Should have at least 2 calls: one for PushSource, one for
	// PushDecomposition.
	if len(calls) < 2 {
		t.Errorf("expected >=2 StoreJSON calls (source + decomp), got %d", len(calls))
	}
}

// TestPushSource_AsyncS3 verifies PushSource also returns before the
// S3 write completes.
func TestPushSource_AsyncS3(t *testing.T) {
	r, ms := newTestRegistry(t, 200*time.Millisecond)
	ctx := context.Background()

	start := time.Now()
	_, err := r.PushSource(ctx, &model.SourceData{
		ID:    "src-push-async",
		URL:   "http://example.com/push-async",
		Title: "Push Async Source",
	})
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("PushSource: %v", err)
	}
	if elapsed >= 200*time.Millisecond {
		t.Errorf("PushSource took %v (expected <200ms with async S3)", elapsed)
	}

	time.Sleep(300 * time.Millisecond)
	calls := ms.getCalls()
	if len(calls) < 1 {
		t.Errorf("expected >=1 StoreJSON calls for source, got %d", len(calls))
	}
}

// TestPullSource_EmbedsDecompositions verifies that PullSource
// returns the decompositions list from the metadata DB (not just
// the stale S3 object written at source-push time) and that the
// source ID is populated even when PushSource was called without
// an explicit ID (the dedup/generate path).
func TestPullSource_EmbedsDecompositions(t *testing.T) {
	r, _ := newTestRegistry(t, 0)
	ctx := context.Background()

	// Push source WITHOUT an explicit ID — simulates the backend
	// client path, which sends url+doi+title and lets the registry
	// generate the ID.
	res, err := r.PushSource(ctx, &model.SourceData{
		URL:   "http://example.com/pull-test",
		Title: "Pull Test Source",
	})
	if err != nil {
		t.Fatalf("PushSource: %v", err)
	}
	sourceID := res.SourceID
	if sourceID == "" {
		t.Fatal("PushSource returned empty source ID")
	}

	// Wait for async S3 write.
	time.Sleep(100 * time.Millisecond)

	// PullSource before any decomposition: should have empty list
	// but populated source ID.
	pkg, err := r.PullSource(ctx, sourceID)
	if err != nil {
		t.Fatalf("PullSource (no decomps): %v", err)
	}
	if pkg.Source.ID != sourceID {
		t.Errorf("source.id = %q, want %q", pkg.Source.ID, sourceID)
	}
	if len(pkg.Decompositions) != 0 {
		t.Errorf("expected 0 decompositions, got %d", len(pkg.Decompositions))
	}

	// Push a decomposition.
	_, err = r.PushDecomposition(ctx, sourceID, &model.DecompositionPackage{
		ModelID: "test-model",
		Facts:   makeFacts(5),
	})
	if err != nil {
		t.Fatalf("PushDecomposition: %v", err)
	}
	time.Sleep(100 * time.Millisecond)

	// PullSource again: should now embed the decomposition with
	// fact_count and a presigned_url.
	pkg, err = r.PullSource(ctx, sourceID)
	if err != nil {
		t.Fatalf("PullSource (with decomps): %v", err)
	}
	if len(pkg.Decompositions) != 1 {
		t.Fatalf("expected 1 decomposition, got %d", len(pkg.Decompositions))
	}
	d := pkg.Decompositions[0]
	if d.ModelID != "test-model" {
		t.Errorf("decomp model_id = %q, want test-model", d.ModelID)
	}
	if d.FactCount != 5 {
		t.Errorf("decomp fact_count = %d, want 5", d.FactCount)
	}
	if d.PresignedURL == "" {
		t.Error("decomp presigned_url is empty; expected a presigned S3 URL")
	}
}

// TestPullSource_PresignDisabled verifies that when presigning is
// disabled (ErrPresignDisabled), PullSource still embeds the
// decompositions list — just with an empty presigned_url. Clients
// fall back to the backend proxy in that case.
func TestPullSource_PresignDisabled(t *testing.T) {
	r, ms := newTestRegistry(t, 0)
	ms.presignError = storage.ErrPresignDisabled
	ctx := context.Background()

	res, err := r.PushSource(ctx, &model.SourceData{
		URL:   "http://example.com/no-presign",
		Title: "No Presign Source",
	})
	if err != nil {
		t.Fatalf("PushSource: %v", err)
	}
	sourceID := res.SourceID
	time.Sleep(100 * time.Millisecond)

	_, err = r.PushDecomposition(ctx, sourceID, &model.DecompositionPackage{
		ModelID: "test-model",
		Facts:   makeFacts(3),
	})
	if err != nil {
		t.Fatalf("PushDecomposition: %v", err)
	}
	time.Sleep(100 * time.Millisecond)

	pkg, err := r.PullSource(ctx, sourceID)
	if err != nil {
		t.Fatalf("PullSource: %v", err)
	}
	if len(pkg.Decompositions) != 1 {
		t.Fatalf("expected 1 decomposition, got %d", len(pkg.Decompositions))
	}
	d := pkg.Decompositions[0]
	if d.ModelID != "test-model" {
		t.Errorf("model_id = %q, want test-model", d.ModelID)
	}
	if d.FactCount != 3 {
		t.Errorf("fact_count = %d, want 3", d.FactCount)
	}
	if d.PresignedURL != "" {
		t.Errorf("presigned_url = %q, want empty (presign disabled)", d.PresignedURL)
	}
}

// TestPushDecomposition_PromptsetHash_Persisted verifies the
// promptset_hash on a pushed DecompositionPackage round-trips
// through the registry: it lands on the decompositions row
// (visible via ListDecompositions), echoes on the DecompRef
// PullSource returns, and survives in the S3 JSON PullDecomposition
// returns. This is the contract the OKT backend's pull-side
// RelevanceFilter.AllowsPromptset relies on to filter decompositions
// by philosophy — before the fix the registry silently dropped the
// field on receive, so the filter was a no-op.
func TestPushDecomposition_PromptsetHash_Persisted(t *testing.T) {
	r, _ := newTestRegistry(t, 0)
	ctx := context.Background()

	if _, err := r.PushSource(ctx, &model.SourceData{
		ID:    "src-ps",
		URL:   "http://example.com/ps",
		Title: "PS Source",
	}); err != nil {
		t.Fatalf("PushSource: %v", err)
	}

	const hash = "abc123registryhash"
	if _, err := r.PushDecomposition(ctx, "src-ps", &model.DecompositionPackage{
		ModelID:       "test-model",
		PromptsetHash: hash,
		Facts:         makeFacts(2),
	}); err != nil {
		t.Fatalf("PushDecomposition: %v", err)
	}
	time.Sleep(50 * time.Millisecond) // let the fire-and-forget S3 write land

	// PullDecomposition returns the hash from the S3 JSON.
	decomp, err := r.PullDecomposition(ctx, "src-ps", "test-model")
	if err != nil {
		t.Fatalf("PullDecomposition: %v", err)
	}
	if decomp.PromptsetHash != hash {
		t.Errorf("PullDecomposition: promptset_hash = %q, want %q", decomp.PromptsetHash, hash)
	}

	// PullSource echoes the hash on the embedded DecompRef (read
	// from the metadata DB, not the S3 JSON). This is the field the
	// backend's ListRelevantDecompositions filter reads.
	pkg, err := r.PullSource(ctx, "src-ps")
	if err != nil {
		t.Fatalf("PullSource: %v", err)
	}
	if len(pkg.Decompositions) != 1 {
		t.Fatalf("expected 1 decomposition, got %d", len(pkg.Decompositions))
	}
	if got := pkg.Decompositions[0].PromptsetHash; got != hash {
		t.Errorf("PullSource DecompRef promptset_hash = %q, want %q", got, hash)
	}
}

// TestPushDecomposition_PromptsetHash_RejectedWhenValidationEnabled
// verifies that when promptset.enable_validation is true, a push
// without a promptset_hash is rejected with ErrPromptsetHashRequired
// (the handler maps this to 400 Bad Request). This is the forcing
// function that guarantees every decomposition on a validating
// registry carries a real philosophy hash so pullers can filter.
func TestPushDecomposition_PromptsetHash_RejectedWhenValidationEnabled(t *testing.T) {
	r, _ := newTestRegistryWithValidation(t, 0, true)
	ctx := context.Background()

	if _, err := r.PushSource(ctx, &model.SourceData{
		ID:    "src-vs",
		URL:   "http://example.com/vs",
		Title: "VS Source",
	}); err != nil {
		t.Fatalf("PushSource: %v", err)
	}

	// Empty hash → rejected.
	_, err := r.PushDecomposition(ctx, "src-vs", &model.DecompositionPackage{
		ModelID: "test-model",
		Facts:   makeFacts(1),
	})
	if err != ErrPromptsetHashRequired {
		t.Errorf("PushDecomposition with empty hash: err = %v, want ErrPromptsetHashRequired", err)
	}

	// Non-empty hash → accepted.
	const hash = "validregistryhash"
	if _, err := r.PushDecomposition(ctx, "src-vs", &model.DecompositionPackage{
		ModelID:       "test-model",
		PromptsetHash: hash,
		Facts:         makeFacts(1),
	}); err != nil {
		t.Errorf("PushDecomposition with hash: unexpected err %v", err)
	}
}

// TestPushDecomposition_PromptsetHash_AcceptedWhenValidationDisabled
// verifies the legacy default: when validation is off, an empty
// promptset_hash is accepted (stored as NULL/empty). This preserves
// backward compatibility for existing registries and deployments
// that haven't configured promptsets on their contributing backends.
func TestPushDecomposition_PromptsetHash_AcceptedWhenValidationDisabled(t *testing.T) {
	r, _ := newTestRegistry(t, 0) // validation off (default)
	ctx := context.Background()

	if _, err := r.PushSource(ctx, &model.SourceData{
		ID:    "src-legacy",
		URL:   "http://example.com/legacy",
		Title: "Legacy Source",
	}); err != nil {
		t.Fatalf("PushSource: %v", err)
	}

	if _, err := r.PushDecomposition(ctx, "src-legacy", &model.DecompositionPackage{
		ModelID: "test-model",
		Facts:   makeFacts(1),
	}); err != nil {
		t.Errorf("PushDecomposition with empty hash (validation off): unexpected err %v", err)
	}
}

// TestPushDecomposition_PromptsetHash_UpdatedOnRepush verifies a
// re-push of the same (source, model) updates the stored
// promptset_hash (the ON CONFLICT DO UPDATE clause covers it). A
// contributing backend that switches promptsets and re-pushes must
// overwrite the old hash so pullers see the current philosophy.
func TestPushDecomposition_PromptsetHash_UpdatedOnRepush(t *testing.T) {
	r, _ := newTestRegistry(t, 0)
	ctx := context.Background()

	if _, err := r.PushSource(ctx, &model.SourceData{
		ID:    "src-re",
		URL:   "http://example.com/re",
		Title: "Re Source",
	}); err != nil {
		t.Fatalf("PushSource: %v", err)
	}

	const oldHash = "oldhash"
	if _, err := r.PushDecomposition(ctx, "src-re", &model.DecompositionPackage{
		ModelID:       "test-model",
		PromptsetHash: oldHash,
		Facts:         makeFacts(1),
	}); err != nil {
		t.Fatalf("first PushDecomposition: %v", err)
	}
	time.Sleep(50 * time.Millisecond)

	const newHash = "newhash"
	if _, err := r.PushDecomposition(ctx, "src-re", &model.DecompositionPackage{
		ModelID:       "test-model",
		PromptsetHash: newHash,
		Facts:         makeFacts(1),
	}); err != nil {
		t.Fatalf("second PushDecomposition: %v", err)
	}
	time.Sleep(50 * time.Millisecond)

	pkg, err := r.PullSource(ctx, "src-re")
	if err != nil {
		t.Fatalf("PullSource: %v", err)
	}
	if len(pkg.Decompositions) != 1 {
		t.Fatalf("expected 1 decomposition, got %d", len(pkg.Decompositions))
	}
	if got := pkg.Decompositions[0].PromptsetHash; got != newHash {
		t.Errorf("promptset_hash after re-push = %q, want %q (should be updated)", got, newHash)
	}
}
