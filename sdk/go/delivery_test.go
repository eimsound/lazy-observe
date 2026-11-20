package lazyobserve

import (
	"context"
	"errors"
	"net/http"
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

func TestDeliveryQueueClampsBatchSizeToCapacity(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	var batchSizes []int
	release := make(chan struct{})
	firstStarted := make(chan struct{})
	queue := newDeliveryQueue(deliveryOptions[int]{
		Capacity:      5,
		BatchSize:     20,
		FlushInterval: 50 * time.Millisecond,
		MaxAttempts:   1,
		Send: func(_ context.Context, batch []int) error {
			mu.Lock()
			first := len(batchSizes) == 0
			batchSizes = append(batchSizes, len(batch))
			mu.Unlock()
			if first {
				close(firstStarted)
				<-release
			}
			return nil
		},
	})
	if queue.options.BatchSize != 5 {
		t.Fatalf("clamped BatchSize=%d want=5", queue.options.BatchSize)
	}
	if !queue.enqueue(0) {
		t.Fatal("initial enqueue failed")
	}
	<-firstStarted
	for i := 1; i <= 4; i++ {
		if !queue.enqueue(i) {
			t.Fatalf("enqueue %d failed", i)
		}
	}
	close(release)
	if err := queue.close(context.Background()); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(batchSizes) != 2 || batchSizes[1] != 4 {
		t.Fatalf("batchSizes=%v want [1 4] (not collapsed to batch size 1)", batchSizes)
	}
}

func TestDeliveryQueueDropsPermanent4xxWithoutRetry(t *testing.T) {
	t.Parallel()
	for _, status := range []int{
		http.StatusBadRequest,
		http.StatusUnauthorized,
		http.StatusForbidden,
		http.StatusNotFound,
		http.StatusConflict,
		http.StatusRequestEntityTooLarge,
		http.StatusUnprocessableEntity,
	} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			t.Parallel()
			var attempts atomic.Int64
			var mu sync.Mutex
			var reasons []string
			queue := newDeliveryQueue(deliveryOptions[int]{
				Capacity:      4,
				BatchSize:     1,
				FlushInterval: time.Hour,
				MaxAttempts:   3,
				Wait:          func(context.Context, time.Duration) error { return nil },
				Send: func(context.Context, []int) error {
					attempts.Add(1)
					return &deliveryError{
						statusCode: status,
						permanent:  isPermanentHTTPStatus(status),
						err:        errors.New("rejected"),
					}
				},
				Warn: func(reason string, _ error, _ int) {
					mu.Lock()
					reasons = append(reasons, reason)
					mu.Unlock()
				},
			})
			queue.enqueue(1)
			err := queue.close(context.Background())
			if err == nil {
				t.Fatal("expected terminal error from close")
			}
			if attempts.Load() != 1 {
				t.Fatalf("status %d attempts=%d want=1", status, attempts.Load())
			}
			mu.Lock()
			defer mu.Unlock()
			if len(reasons) != 1 || reasons[0] != "server_rejected" || queue.dropped() != 1 {
				t.Fatalf("status %d reasons=%v drops=%d", status, reasons, queue.dropped())
			}
		})
	}

	for _, status := range []int{
		http.StatusRequestTimeout,
		http.StatusTooManyRequests,
		http.StatusServiceUnavailable,
	} {
		t.Run("retryable_"+http.StatusText(status), func(t *testing.T) {
			t.Parallel()
			var attempts atomic.Int64
			queue := newDeliveryQueue(deliveryOptions[int]{
				Capacity:      4,
				BatchSize:     1,
				FlushInterval: time.Hour,
				MaxAttempts:   3,
				Wait:          func(context.Context, time.Duration) error { return nil },
				Send: func(context.Context, []int) error {
					attempts.Add(1)
					return &deliveryError{
						statusCode: status,
						permanent:  isPermanentHTTPStatus(status),
						err:        errors.New("retryable"),
					}
				},
			})
			queue.enqueue(1)
			_ = queue.close(context.Background())
			if attempts.Load() != 3 {
				t.Fatalf("retryable status %d attempts=%d want=3", status, attempts.Load())
			}
		})
	}
}

func TestDeliveryQueueHonorsRetryAfterAndJitter(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	var waits []time.Duration
	var attempts atomic.Int64
	queue := newDeliveryQueue(deliveryOptions[int]{
		Capacity:      2,
		BatchSize:     1,
		FlushInterval: time.Hour,
		MaxAttempts:   3,
		RetryInitial:  100 * time.Millisecond,
		RetryMax:      time.Second,
		Wait: func(_ context.Context, delay time.Duration) error {
			mu.Lock()
			waits = append(waits, delay)
			mu.Unlock()
			return nil
		},
		Send: func(context.Context, []int) error {
			switch attempts.Add(1) {
			case 1:
				return &deliveryError{
					statusCode: http.StatusServiceUnavailable,
					retryAfter: 2 * time.Second,
					err:        errors.New("busy"),
				}
			case 2:
				return &deliveryError{
					statusCode: http.StatusTooManyRequests,
					retryAfter: 5 * time.Minute,
					err:        errors.New("rate limited"),
				}
			default:
				return nil
			}
		},
	})
	queue.enqueue(1)
	if err := queue.close(context.Background()); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(waits) != 2 {
		t.Fatalf("waits=%v want 2 waits", waits)
	}
	if waits[0] < 2*time.Second || waits[0] > 2500*time.Millisecond {
		t.Fatalf("first wait=%s want [2s, 2.5s]", waits[0])
	}
	if waits[1] != deliveryMaxRetryAfter {
		t.Fatalf("capped wait=%s want %s", waits[1], deliveryMaxRetryAfter)
	}

	// Verify parseRetryAfter for delay-seconds, HTTP-date, and invalid inputs.
	if got := parseRetryAfter("3"); got != 3*time.Second {
		t.Fatalf("parseRetryAfter(3)=%s want=3s", got)
	}
	futureDate := time.Now().UTC().Add(5 * time.Second).Format(http.TimeFormat)
	if got := parseRetryAfter(futureDate); got <= 0 || got > 6*time.Second {
		t.Fatalf("parseRetryAfter(HTTP-date)=%s want (0, 6s]", got)
	}
	if got := parseRetryAfter("-1"); got != 0 {
		t.Fatalf("parseRetryAfter(-1)=%s want=0", got)
	}
	if got := parseRetryAfter("not-a-date"); got != 0 {
		t.Fatalf("parseRetryAfter(garbage)=%s want=0", got)
	}

	// Verify Full Jitter stays within [0, backoff], varies across samples, and
	// can sample below backoff/2 (unlike Equal Jitter).
	seen := make(map[time.Duration]struct{})
	sawLowerHalf := false
	for range 128 {
		d := queue.retryDelay(0, 0)
		if d < 0 || d > 100*time.Millisecond {
			t.Fatalf("full-jittered retryDelay=%s out of [0, 100ms]", d)
		}
		if d < 50*time.Millisecond {
			sawLowerHalf = true
		}
		seen[d] = struct{}{}
	}
	if len(seen) < 2 || !sawLowerHalf {
		t.Fatalf(
			"retryDelay did not exhibit Full Jitter in [0, 100ms]: samples=%d sawLowerHalf=%v",
			len(seen),
			sawLowerHalf,
		)
	}
}

func TestQueueFullLogThrottle(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	throttle := &queueFullLogThrottle{
		interval: 10 * time.Second,
		now:      func() time.Time { return now },
	}

	// First queue_full logs immediately with 0 suppressed.
	if ok, suppressed := throttle.allow("queue_full", 1); !ok || suppressed != 0 {
		t.Fatalf("first allow=(%v, %d) want (true, 0)", ok, suppressed)
	}
	// Burst within throttle window is suppressed while accumulating dropped count.
	for range 25 {
		if ok, suppressed := throttle.allow("queue_full", 1); ok || suppressed != 0 {
			t.Fatalf("burst allow=(%v, %d) want (false, 0)", ok, suppressed)
		}
	}
	// Other drop reasons are never throttled.
	if ok, suppressed := throttle.allow("server_rejected", 3); !ok || suppressed != 0 {
		t.Fatalf("server_rejected allow=(%v, %d) want (true, 0)", ok, suppressed)
	}
	// Advancing clock past interval emits again with suppressed count.
	now = now.Add(10 * time.Second)
	if ok, suppressed := throttle.allow("queue_full", 1); !ok || suppressed != 25 {
		t.Fatalf("post-window allow=(%v, %d) want (true, 25)", ok, suppressed)
	}
	// Subsequent drop in new window is suppressed again.
	if ok, _ := throttle.allow("queue_full", 1); ok {
		t.Fatal("expected second window drop to be suppressed")
	}
}

func TestDeliveryQueueClearsLastErrorAfterSubsequentSuccess(t *testing.T) {
	t.Parallel()
	var calls atomic.Int64
	queue := newDeliveryQueue(deliveryOptions[int]{
		Capacity:      4,
		BatchSize:     1,
		FlushInterval: time.Hour,
		MaxAttempts:   1,
		Send: func(_ context.Context, batch []int) error {
			if calls.Add(1) == 1 {
				return &deliveryError{statusCode: http.StatusBadRequest, permanent: true, err: errors.New("bad first")}
			}
			return nil
		},
	})
	queue.enqueue(1)
	queue.enqueue(2)
	if err := queue.close(context.Background()); err != nil {
		t.Fatalf("close error=%v want=nil after subsequent success", err)
	}
	if queue.dropped() != 1 {
		t.Fatalf("dropped=%d want=1", queue.dropped())
	}
}

func TestDeliveryQueueCloseWithoutDeadlineUsesDefaultTimeout(t *testing.T) {
	prev := defaultDeliveryShutdownTimeout
	defaultDeliveryShutdownTimeout = 40 * time.Millisecond
	t.Cleanup(func() { defaultDeliveryShutdownTimeout = prev })

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
	queue.enqueue(1)
	<-started

	startedAt := time.Now()
	err := queue.close(context.Background())
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("close(context.Background()) err=%v want DeadlineExceeded", err)
	}
	if elapsed := time.Since(startedAt); elapsed > 300*time.Millisecond {
		t.Fatalf("close without deadline blocked for %s", elapsed)
	}
}
