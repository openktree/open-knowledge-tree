package decomposition

import (
	"context"
	"errors"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestExtractParallel_OrderPreserved(t *testing.T) {
	items := []int{0, 1, 2, 3, 4, 5, 6, 7, 8, 9}
	results, errs := ExtractParallel(context.Background(), 3, items,
		func(_ context.Context, x int) (string, error) {
			// Sleep proportional to reversed index so completion order != input order.
			time.Sleep(time.Duration(9-x) * time.Millisecond)
			return "r" + strconv.Itoa(x), nil
		},
	)
	for i, r := range results {
		want := "r" + strconv.Itoa(items[i])
		if r != want {
			t.Errorf("results[%d] = %q, want %q", i, r, want)
		}
		if errs[i] != nil {
			t.Errorf("errs[%d] = %v, want nil", i, errs[i])
		}
	}
}

func TestExtractParallel_ConcurrencyBounded(t *testing.T) {
	const concurrency = 3
	var inFlight, maxInFlight int32
	var mu sync.Mutex
	var calls int32

	items := make([]int, 20)
	for i := range items {
		items[i] = i
	}
	_, _ = ExtractParallel(context.Background(), concurrency, items,
		func(_ context.Context, _ int) (struct{}, error) {
			cur := atomic.AddInt32(&inFlight, 1)
			mu.Lock()
			if cur > maxInFlight {
				maxInFlight = cur
			}
			mu.Unlock()
			atomic.AddInt32(&calls, 1)
			time.Sleep(5 * time.Millisecond)
			atomic.AddInt32(&inFlight, -1)
			return struct{}{}, nil
		},
	)
	if maxInFlight > concurrency {
		t.Errorf("max in-flight = %d, want <= %d", maxInFlight, concurrency)
	}
	if calls != int32(len(items)) {
		t.Errorf("calls = %d, want %d", calls, len(items))
	}
}

func TestExtractParallel_PerItemErrorsNotFatal(t *testing.T) {
	items := []int{0, 1, 2, 3, 4}
	wantErr := errors.New("boom")
	results, errs := ExtractParallel(context.Background(), 2, items,
		func(_ context.Context, x int) (string, error) {
			if x%2 == 0 {
				return "", wantErr
			}
			return "ok" + strconv.Itoa(x), nil
		},
	)
	for i, err := range errs {
		if i%2 == 0 {
			if !errors.Is(err, wantErr) {
				t.Errorf("errs[%d] = %v, want %v", i, err, wantErr)
			}
			if results[i] != "" {
				t.Errorf("results[%d] = %q, want empty on error", i, results[i])
			}
		} else {
			if err != nil {
				t.Errorf("errs[%d] = %v, want nil", i, err)
			}
			if results[i] != "ok"+strconv.Itoa(i) {
				t.Errorf("results[%d] = %q, want ok%d", i, results[i], i)
			}
		}
	}
}

func TestExtractParallel_Empty(t *testing.T) {
	results, errs := ExtractParallel[int, string](context.Background(), 4, nil,
		func(context.Context, int) (string, error) { return "x", nil },
	)
	if len(results) != 0 || len(errs) != 0 {
		t.Errorf("expected empty slices, got results=%v errs=%v", results, errs)
	}
}

func TestExtractParallel_ConcurrencyClampedToOne(t *testing.T) {
	var calls int32
	items := []int{0, 1, 2}
	_, _ = ExtractParallel(context.Background(), 0, items,
		func(_ context.Context, _ int) (struct{}, error) {
			atomic.AddInt32(&calls, 1)
			return struct{}{}, nil
		},
	)
	if calls != 3 {
		t.Errorf("calls = %d, want 3 (concurrency<1 clamps to 1, still runs all)", calls)
	}
}

func TestExtractParallel_ContextCancelledStopsDispatch(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // pre-cancelled

	var calls int32
	items := []int{0, 1, 2, 3, 4}
	_, errs := ExtractParallel(ctx, 2, items,
		func(_ context.Context, _ int) (struct{}, error) {
			atomic.AddInt32(&calls, 1)
			return struct{}{}, nil
		},
	)
	if calls != 0 {
		t.Errorf("calls = %d, want 0 (pre-cancelled ctx must not dispatch)", calls)
	}
	for i, err := range errs {
		if !errors.Is(err, context.Canceled) {
			t.Errorf("errs[%d] = %v, want context.Canceled", i, err)
		}
	}
}
