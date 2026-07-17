package decomposition

import (
	"context"
	"sync"

	"golang.org/x/sync/semaphore"
)

// extractParallel runs fn over items with at most concurrency workers in
// flight. It is the two-phase fan-out helper used by the source_decomposition
// and extract_concepts workers to parallelize the AI-bound extraction calls
// while keeping the DB-bound persistence in a single serial phase afterwards.
//
// Results are returned in input order (indexed by position, not by completion
// order) so the caller's phase-2 persistence loop can iterate them with the
// same ordering guarantees as the original serial loop. errs[i] is the error
// (or nil) for items[i]; a non-nil errs[i] means results[i] is the zero value.
//
// fn is expected to perform only the AI call (plus any pure computation needed
// to build its input) and must NOT touch the database — persistence is the
// caller's phase-2 responsibility. The per-call retry/backoff already lives
// inside each Extract* provider via retryWithBackoff, so fn does not need to
// retry on its own.
//
// concurrency <= 0 is clamped to 1 (serial). ctx cancellation stops dispatching
// new items; in-flight items keep running with their own per-call timeout
// (enforced by the provider's retryWithBackoff) and will report back. The
// helper returns when all dispatched items have finished (or errored); it
// does not abort early on the first item error — per-item tolerance is
// preserved so a single failing chunk/fact/image does not poison the batch.
func ExtractParallel[T any, R any](
	ctx context.Context,
	concurrency int,
	items []T,
	fn func(ctx context.Context, item T) (R, error),
) (results []R, errs []error) {
	n := len(items)
	results = make([]R, n)
	errs = make([]error, n)
	if n == 0 {
		return results, errs
	}
	if concurrency < 1 {
		concurrency = 1
	}

	sem := semaphore.NewWeighted(int64(concurrency))
	var wg sync.WaitGroup
	// One Done per item; we only Add for items we actually dispatch so the
	// pre-fill-on-cancel path doesn't double-count.
	dispatched := 0

	for i, item := range items {
		// Stop dispatching if the job ctx is cancelled. Already-dispatched
		// items finish on their own; their results (or ctx errors) are
		// collected below so phase 2 can still see a complete slice.
		if err := ctx.Err(); err != nil {
			// Fill the remaining slots with the ctx error so phase 2 sees a
			// complete (if failing) slice rather than a partial one.
			for j := i; j < n; j++ {
				errs[j] = err
			}
			break
		}

		// Acquire before spawning so dispatch is bounded even under a flood
		// of items. Acquire respects ctx, so a cancelled job unblocks quickly.
		if err := sem.Acquire(ctx, 1); err != nil {
			// ctx cancelled while waiting — record the error for this and the
			// remaining items and stop dispatching.
			for j := i; j < n; j++ {
				errs[j] = err
			}
			break
		}

		dispatched++
		wg.Add(1)
		go func(idx int, it T) {
			defer wg.Done()
			defer sem.Release(1)
			res, err := fn(ctx, it)
			results[idx] = res
			errs[idx] = err
		}(i, item)
	}

	_ = dispatched // only dispatched items got wg.Add(1); the rest are pre-filled
	wg.Wait()
	return results, errs
}
