package lazyobserve

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestDeliveryQueueIsNonBlockingAndCountsCapacityDrops(t *testing.T) {
	t.Parallel()
	started := make(chan struct{})
	release := make(chan struct{})
	var warnings atomic.Int64
	queue := newDeliveryQueue(deliveryOptions[int]{
		Capacity:      1,
		BatchSize:     1,
		FlushInterval: time.Hour,
		MaxAttempts:   1,
		Send: func(ctx context.Context, batch []int) error {
			close(started)
			select {
			case <-release:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		},
		Warn: func(string, error, int) { warnings.Add(1) },
	})
	if !queue.enqueue(1) {
		t.Fatal("first enqueue was rejected")
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("sender did not start")
	}
	startedAt := time.Now()
	if queue.enqueue(2) {
		t.Fatal("enqueue beyond capacity unexpectedly succeeded")
	}
	if elapsed := time.Since(startedAt); elapsed > 50*time.Millisecond {
		t.Fatalf("full enqueue blocked for %s", elapsed)
	}
	if queue.dropped() != 1 || warnings.Load() != 1 {
		t.Fatalf("drops=%d warnings=%d want=1", queue.dropped(), warnings.Load())
	}
	close(release)
	if err := queue.close(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestDeliveryQueueRetriesTheSameBatchWithinFiniteBudget(t *testing.T) {
	t.Parallel()
	var attempts atomic.Int64
	var mu sync.Mutex
	var batches [][]int
	queue := newDeliveryQueue(deliveryOptions[int]{
		Capacity:      1,
		BatchSize:     1,
		FlushInterval: time.Hour,
		MaxAttempts:   3,
		RetryInitial:  time.Second,
		RetryMax:      2 * time.Second,
		Wait:          func(context.Context, time.Duration) error { return nil },
		Send: func(_ context.Context, batch []int) error {
			attempt := attempts.Add(1)
			mu.Lock()
			batches = append(batches, append([]int(nil), batch...))
			mu.Unlock()
			if attempt < 3 {
				return errors.New("temporary export failure")
			}
			return nil
		},
	})
	if !queue.enqueue(42) {
		t.Fatal("enqueue was rejected")
	}
	if err := queue.close(context.Background()); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if attempts.Load() != 3 || len(batches) != 3 {
		t.Fatalf("attempts=%d batches=%v", attempts.Load(), batches)
	}
	for _, batch := range batches {
		if len(batch) != 1 || batch[0] != 42 {
			t.Fatalf("retried a different batch: %v", batches)
		}
	}
	if queue.dropped() != 0 {
		t.Fatalf("drops=%d want=0", queue.dropped())
	}
}

func TestDeliveryQueueReturnsTerminalFailureAfterRetryExhaustion(t *testing.T) {
	t.Parallel()
	var attempts atomic.Int64
	queue := newDeliveryQueue(deliveryOptions[int]{
		Capacity:      1,
		BatchSize:     1,
		FlushInterval: time.Hour,
		MaxAttempts:   3,
		Wait:          func(context.Context, time.Duration) error { return nil },
		Send: func(context.Context, []int) error {
			attempts.Add(1)
			return errors.New("permanent export failure")
		},
	})
	if !queue.enqueue(7) {
		t.Fatal("enqueue was rejected")
	}
	err := queue.close(context.Background())
	if err == nil || err.Error() != "permanent export failure" {
		t.Fatalf("close error=%v", err)
	}
	if attempts.Load() != 3 || queue.dropped() != 1 {
		t.Fatalf("attempts=%d drops=%d want=3/1", attempts.Load(), queue.dropped())
	}
}

func TestDeliveryQueueShutdownHonorsCallerDeadline(t *testing.T) {
	t.Parallel()
	started := make(chan struct{})
	queue := newDeliveryQueue(deliveryOptions[int]{
		Capacity:      1,
		BatchSize:     1,
		FlushInterval: time.Hour,
		MaxAttempts:   3,
		Send: func(ctx context.Context, _ []int) error {
			close(started)
			<-ctx.Done()
			return ctx.Err()
		},
	})
	if !queue.enqueue(9) {
		t.Fatal("enqueue was rejected")
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("sender did not start")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	startedAt := time.Now()
	err := queue.close(ctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("close error=%v", err)
	}
	if elapsed := time.Since(startedAt); elapsed > 250*time.Millisecond {
		t.Fatalf("shutdown exceeded caller budget: %s", elapsed)
	}
}
