package lazyobserve

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"
)

const (
	deliveryDefaultFlushInterval = 100 * time.Millisecond
	deliveryDefaultRetryInitial  = 100 * time.Millisecond
	deliveryDefaultRetryMax      = time.Second
	deliveryDefaultMaxAttempts   = 3
)

type deliveryOptions[T any] struct {
	Capacity      int
	BatchSize     int
	FlushInterval time.Duration
	MaxAttempts   int
	RetryInitial  time.Duration
	RetryMax      time.Duration
	Send          func(context.Context, []T) error
	Wait          func(context.Context, time.Duration) error
	Warn          func(reason string, err error, dropped int)
}

type deliveryQueue[T any] struct {
	options deliveryOptions[T]
	items   chan T
	done    chan struct{}
	runCtx  context.Context
	cancel  context.CancelFunc

	stateMu   sync.RWMutex
	closed    bool
	closeOnce sync.Once

	resident atomic.Int64
	drops    atomic.Uint64
	errMu    sync.Mutex
	lastErr  error
}

func newDeliveryQueue[T any](options deliveryOptions[T]) *deliveryQueue[T] {
	if options.Capacity <= 0 {
		panic("delivery queue capacity must be positive")
	}
	if options.BatchSize <= 0 || options.BatchSize > options.Capacity {
		options.BatchSize = min(options.Capacity, 1)
	}
	if options.FlushInterval <= 0 {
		options.FlushInterval = deliveryDefaultFlushInterval
	}
	if options.MaxAttempts <= 0 {
		options.MaxAttempts = deliveryDefaultMaxAttempts
	}
	if options.RetryInitial <= 0 {
		options.RetryInitial = deliveryDefaultRetryInitial
	}
	if options.RetryMax <= 0 {
		options.RetryMax = deliveryDefaultRetryMax
	}
	if options.RetryMax < options.RetryInitial {
		options.RetryMax = options.RetryInitial
	}
	if options.Send == nil {
		panic("delivery queue sender is required")
	}
	if options.Wait == nil {
		options.Wait = waitForDeliveryRetry
	}
	runCtx, cancel := context.WithCancel(context.Background())
	queue := &deliveryQueue[T]{
		options: options,
		items:   make(chan T, options.Capacity),
		done:    make(chan struct{}),
		runCtx:  runCtx,
		cancel:  cancel,
	}
	go queue.run()
	return queue
}

func (q *deliveryQueue[T]) enqueue(item T) bool {
	if !q.reserve() {
		q.recordDrop("queue_full", nil, 1)
		return false
	}
	q.stateMu.RLock()
	defer q.stateMu.RUnlock()
	if q.closed {
		q.resident.Add(-1)
		q.recordDrop("shutdown_budget", nil, 1)
		return false
	}
	select {
	case q.items <- item:
		return true
	default:
		q.resident.Add(-1)
		q.recordDrop("queue_full", nil, 1)
		return false
	}
}

func (q *deliveryQueue[T]) reserve() bool {
	capacity := int64(q.options.Capacity)
	for {
		current := q.resident.Load()
		if current >= capacity {
			return false
		}
		if q.resident.CompareAndSwap(current, current+1) {
			return true
		}
	}
}

func (q *deliveryQueue[T]) dropped() uint64 {
	return q.drops.Load()
}

func (q *deliveryQueue[T]) close(ctx context.Context) error {
	q.closeOnce.Do(func() {
		q.stateMu.Lock()
		q.closed = true
		close(q.items)
		q.stateMu.Unlock()
	})
	select {
	case <-q.done:
		q.cancel()
		return q.deliveryError()
	case <-ctx.Done():
		q.cancel()
		return errors.Join(ctx.Err(), q.deliveryError())
	}
}

func (q *deliveryQueue[T]) run() {
	defer close(q.done)
	for {
		item, ok := <-q.items
		if !ok {
			return
		}
		batch := []T{item}
		closed := q.collectBatch(&batch)
		if q.runCtx.Err() != nil {
			q.dropBatchAndQueued(batch, q.runCtx.Err())
			return
		}
		q.deliver(batch)
		q.resident.Add(-int64(len(batch)))
		if closed {
			return
		}
	}
}

func (q *deliveryQueue[T]) collectBatch(batch *[]T) bool {
	if len(*batch) >= q.options.BatchSize {
		return false
	}
	timer := time.NewTimer(q.options.FlushInterval)
	defer func() {
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
	}()
	for len(*batch) < q.options.BatchSize {
		select {
		case item, ok := <-q.items:
			if !ok {
				return true
			}
			*batch = append(*batch, item)
		case <-timer.C:
			return false
		case <-q.runCtx.Done():
			return false
		}
	}
	return false
}

func (q *deliveryQueue[T]) deliver(batch []T) {
	var lastErr error
	for attempt := 0; attempt < q.options.MaxAttempts; attempt++ {
		lastErr = q.options.Send(q.runCtx, batch)
		if lastErr == nil {
			return
		}
		if q.runCtx.Err() != nil {
			q.setDeliveryError(lastErr)
			q.recordDrop("shutdown_budget", lastErr, len(batch))
			return
		}
		if attempt+1 == q.options.MaxAttempts {
			q.setDeliveryError(lastErr)
			q.recordDrop("retry_exhausted", lastErr, len(batch))
			return
		}
		if err := q.options.Wait(q.runCtx, q.retryDelay(attempt)); err != nil {
			q.setDeliveryError(errors.Join(lastErr, err))
			q.recordDrop("shutdown_budget", lastErr, len(batch))
			return
		}
	}
}

func (q *deliveryQueue[T]) retryDelay(retryIndex int) time.Duration {
	delay := q.options.RetryInitial
	for range retryIndex {
		if delay >= q.options.RetryMax/2 {
			return q.options.RetryMax
		}
		delay *= 2
	}
	return min(delay, q.options.RetryMax)
}

func (q *deliveryQueue[T]) dropBatchAndQueued(batch []T, err error) {
	dropped := len(batch)
	for range q.items {
		dropped++
	}
	q.resident.Add(-int64(dropped))
	q.setDeliveryError(err)
	q.recordDrop("shutdown_budget", err, dropped)
}

func (q *deliveryQueue[T]) recordDrop(reason string, err error, dropped int) {
	q.drops.Add(uint64(dropped))
	if q.options.Warn != nil {
		q.options.Warn(reason, err, dropped)
	}
}

func (q *deliveryQueue[T]) setDeliveryError(err error) {
	q.errMu.Lock()
	q.lastErr = err
	q.errMu.Unlock()
}

func (q *deliveryQueue[T]) deliveryError() error {
	q.errMu.Lock()
	defer q.errMu.Unlock()
	return q.lastErr
}

func waitForDeliveryRetry(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer func() {
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
	}()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
