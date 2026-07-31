package handler

import (
	"context"
	"log"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/openktree/open-knowledge-tree/backend/internal/providers/fetch"
	"github.com/openktree/open-knowledge-tree/backend/internal/store"
)

// hostSkipStore adapts the per-repo store.Queries to the fetch
// strategy's HostSkipStore interface. It resolves the per-repo
// pool via the same RepoPoolResolver the Source handler uses, so
// skip reads/writes land in the repository's own database. The
// adapter is best-effort: a pool resolve failure is logged and
// skipped (the strategy then runs without auto-skip for that
// repo) so a transient pool error never blocks a fetch.
type hostSkipStore struct {
	resolve RepoPoolResolver
}

// NewHostSkipStore builds a fetch.HostSkipStore backed by the
// per-repo store.Queries. Pass the same RepoPoolResolver the
// Source handler uses (wired in wiring.go). Returns nil when
// resolver is nil so the wiring layer can fall back to no
// auto-skip.
func NewHostSkipStore(resolver RepoPoolResolver) fetch.HostSkipStore {
	if resolver == nil {
		return nil
	}
	return &hostSkipStore{resolve: resolver}
}

func (h *hostSkipStore) queries(ctx context.Context, repoID string) (*store.Queries, pgtype.UUID, bool) {
	pool, repoUUID, err := h.resolve(ctx, repoID)
	if err != nil || pool == nil {
		if err != nil {
			log.Printf("host_skip_store: resolve repo %s: %v", repoID, err)
		}
		return nil, pgtype.UUID{}, false
	}
	return store.New(pool), repoUUID, true
}

func (h *hostSkipStore) ActiveForRepo(ctx context.Context, repoID string) ([]fetch.HostSkipEntry, error) {
	q, _, ok := h.queries(ctx, repoID)
	if !ok {
		return nil, nil
	}
	rows, err := q.ListActiveHostSkipProviders(ctx, parseUUID(repoID))
	if err != nil {
		return nil, err
	}
	out := make([]fetch.HostSkipEntry, 0, len(rows))
	for _, r := range rows {
		out = append(out, fetch.HostSkipEntry{Host: r.Host, ProviderID: r.ProviderID})
	}
	return out, nil
}

func (h *hostSkipStore) RecordAndMaybeSkip(
	ctx context.Context,
	repoID, host, providerID string,
	success bool,
	cfg fetch.AutoSkipConfig,
) error {
	q, _, ok := h.queries(ctx, repoID)
	if !ok {
		return nil
	}
	repoUUID := parseUUID(repoID)
	counts, err := q.GetHostProviderAttemptCounts(ctx, store.GetHostProviderAttemptCountsParams{
		Host:       host,
		ProviderID: providerID,
	})
	if err != nil {
		return err
	}
	total := int(counts.TotalAttempts)
	failures := int(counts.Failures)
	if total == 0 {
		return nil
	}
	rate := float64(failures) / float64(total)
	threshold := cfg.FailureThreshold
	if threshold <= 0 {
		threshold = 0.85
	}
	minSample := cfg.MinSample
	if minSample <= 0 {
		minSample = 100
	}
	cooldown := cfg.Cooldown
	if cooldown <= 0 {
		cooldown = 24 * time.Hour
	}
	// Crossed threshold with enough sample → upsert a learned skip.
	if total >= minSample && rate >= threshold {
		_, err := q.UpsertHostSkipProvider(ctx, store.UpsertHostSkipProviderParams{
			RepositoryID: repoUUID,
			Host:         host,
			ProviderID:   providerID,
			FailureRate:  rate,
			SampleSize:   int32(total),
			ExpiresAt:    pgtype.Timestamptz{Time: time.Now().Add(cooldown), Valid: true},
			Manual:       false,
		})
		return err
	}
	// Below threshold after a success → clear a learned (non-manual) skip.
	if success && rate < threshold {
		return q.DeleteLearnedHostSkipProvider(ctx, store.DeleteLearnedHostSkipProviderParams{
			RepositoryID: repoUUID,
			Host:         host,
			ProviderID:   providerID,
		})
	}
	return nil
}

// parseUUID parses a repository id string into a pgtype.UUID.
// Returns a zero (invalid) UUID on parse failure; callers should
// guard with a validity check when the value must be non-zero.
func parseUUID(s string) pgtype.UUID {
	u := pgtype.UUID{}
	_ = u.Scan(s)
	return u
}