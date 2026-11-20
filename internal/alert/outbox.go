package alert

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/eimsound/lazy-observe/internal/backoff"
	"github.com/eimsound/lazy-observe/internal/meta"
	"github.com/eimsound/lazy-observe/internal/outbound"
	"github.com/eimsound/lazy-observe/internal/state"
)

const (
	defaultOutboxPollInterval    = time.Second
	defaultOutboxClaimTTL        = 2 * time.Minute
	defaultOutboxRetryBase       = 5 * time.Second
	defaultOutboxMaxAttempts     = 5
	defaultOutboxClaimBatch      = 8
	defaultChannelRateLimit      = 30
	defaultOutboxDeliveryTimeout = 15 * time.Second
)

type OutboxWorkerConfig struct {
	Store           *meta.Store
	Sender          *Sender
	WorkerID        string
	PollInterval    time.Duration
	ClaimTTL        time.Duration
	RetryBase       time.Duration
	MaxAttempts     int
	ClaimBatch      int
	RateLimitWindow time.Duration
	Limiter         state.Limiter
	Now             func() time.Time
}

// OutboxSnapshot reports live queue backlog and cumulative delivery metrics for
// an OutboxWorker instance.
type OutboxSnapshot struct {
	WorkerID          string                       `json:"worker_id"`
	DeliveredTotal    uint64                       `json:"delivered_total"`
	SkippedTotal      uint64                       `json:"skipped_total"`
	RetriedTotal      uint64                       `json:"retried_total"`
	DeadTotal         uint64                       `json:"dead_total"`
	DeferredTotal     uint64                       `json:"deferred_total"`
	ClaimLostTotal    uint64                       `json:"claim_lost_total"`
	LastBatchDuration time.Duration                `json:"last_batch_duration"`
	Depth             meta.NotificationOutboxDepth `json:"depth"`
}

// OutboxWorker competes for durable notification claims. Delivery is
// at-least-once: a process may stop after the receiver accepts a request but
// before the database claim is completed, so HTTP receivers also get the
// stable Idempotency-Key stored with the outbox item.
type OutboxWorker struct {
	store        *meta.Store
	sender       *Sender
	workerID     string
	pollInterval time.Duration
	claimTTL     time.Duration
	retryBase    time.Duration
	maxAttempts  int
	claimBatch   int
	rateWindow   time.Duration
	limiter      state.Limiter
	wake         chan struct{}
	cancel       context.CancelFunc
	wg           sync.WaitGroup

	deliveredTotal    atomic.Uint64
	skippedTotal      atomic.Uint64
	retriedTotal      atomic.Uint64
	deadTotal         atomic.Uint64
	deferredTotal     atomic.Uint64
	claimLostTotal    atomic.Uint64
	lastBatchDuration atomic.Int64
}

func NewOutboxWorker(cfg OutboxWorkerConfig) *OutboxWorker {
	if cfg.Store == nil {
		return nil
	}
	if cfg.Sender == nil {
		cfg.Sender = &Sender{}
	}
	if cfg.WorkerID == "" {
		cfg.WorkerID = randomOutboxWorkerID()
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = defaultOutboxPollInterval
	}
	if cfg.ClaimTTL <= 0 {
		cfg.ClaimTTL = defaultOutboxClaimTTL
	}
	if cfg.RetryBase <= 0 {
		cfg.RetryBase = defaultOutboxRetryBase
	}
	if cfg.MaxAttempts <= 0 {
		cfg.MaxAttempts = defaultOutboxMaxAttempts
	}
	if cfg.ClaimBatch <= 0 {
		cfg.ClaimBatch = defaultOutboxClaimBatch
	}
	if cfg.ClaimBatch > 100 {
		cfg.ClaimBatch = 100
	}
	if cfg.RateLimitWindow <= 0 {
		cfg.RateLimitWindow = time.Minute
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Limiter == nil {
		cfg.Limiter = state.NewMemoryLimiterWithClock(cfg.Now)
	}
	return &OutboxWorker{
		store: cfg.Store, sender: cfg.Sender, workerID: cfg.WorkerID,
		pollInterval: cfg.PollInterval, claimTTL: cfg.ClaimTTL,
		retryBase: cfg.RetryBase, maxAttempts: cfg.MaxAttempts,
		claimBatch: cfg.ClaimBatch, rateWindow: cfg.RateLimitWindow,
		limiter: cfg.Limiter, wake: make(chan struct{}, 1),
	}
}

// Snapshot returns cumulative worker delivery counters along with the current
// database outbox depth.
func (w *OutboxWorker) Snapshot(ctx context.Context) (OutboxSnapshot, error) {
	if w == nil {
		return OutboxSnapshot{}, nil
	}
	snap := OutboxSnapshot{
		WorkerID:          w.workerID,
		DeliveredTotal:    w.deliveredTotal.Load(),
		SkippedTotal:      w.skippedTotal.Load(),
		RetriedTotal:      w.retriedTotal.Load(),
		DeadTotal:         w.deadTotal.Load(),
		DeferredTotal:     w.deferredTotal.Load(),
		ClaimLostTotal:    w.claimLostTotal.Load(),
		LastBatchDuration: time.Duration(w.lastBatchDuration.Load()),
	}
	if w.store != nil {
		depth, err := w.store.NotificationOutboxDepth(ctx)
		if err != nil {
			return snap, err
		}
		snap.Depth = depth
	}
	return snap, nil
}

func (w *OutboxWorker) Start() {
	if w == nil || w.cancel != nil {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	w.cancel = cancel
	w.wg.Add(1)
	go w.run(ctx)
}

func (w *OutboxWorker) Wake() {
	if w == nil {
		return
	}
	notifyOutboxWake(w.wake)
}

func notifyOutboxWake(wake chan struct{}) {
	select {
	case wake <- struct{}{}:
	default:
	}
}

func (w *OutboxWorker) Close() {
	if w == nil || w.cancel == nil {
		return
	}
	w.cancel()
	w.wg.Wait()
}

func (w *OutboxWorker) run(ctx context.Context) {
	defer w.wg.Done()
	ticker := time.NewTicker(w.pollInterval)
	defer ticker.Stop()
	for {
		w.drainSafe(ctx)
		select {
		case <-ctx.Done():
			return
		case <-w.wake:
		case <-ticker.C:
		}
	}
}

func (w *OutboxWorker) drainSafe(ctx context.Context) {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("panic recovered in notification outbox worker", "worker_id", w.workerID, "panic", r)
		}
	}()
	w.drain(ctx)
}

func (w *OutboxWorker) drain(ctx context.Context) {
	for ctx.Err() == nil {
		start := time.Now()
		items, err := w.store.ClaimNotifications(ctx, w.workerID, w.claimBatch, w.claimTTL)
		if err != nil {
			if !errors.Is(err, context.Canceled) {
				slog.Error("claim notification outbox", "worker_id", w.workerID, "error", err)
			}
			return
		}
		if len(items) == 0 {
			return
		}
		var batch sync.WaitGroup
		batch.Add(len(items))
		for i := range items {
			item := items[i]
			go func() {
				defer batch.Done()
				defer func() {
					if r := recover(); r != nil {
						slog.Error("panic recovered delivering notification", "worker_id", w.workerID,
							"outbox_id", item.ID, "panic", r)
						w.retry(ctx, item, errors.New("notification delivery panicked"))
					}
				}()
				w.deliver(ctx, item)
			}()
		}
		batch.Wait()
		w.lastBatchDuration.Store(time.Since(start).Nanoseconds())
	}
}

func (w *OutboxWorker) deliver(ctx context.Context, item meta.NotificationOutboxItem) {
	var n Notification
	if err := json.Unmarshal(item.Payload, &n); err != nil {
		w.retry(ctx, item, fmt.Errorf("decode notification payload: %w", err))
		return
	}
	n.IdempotencyKey = item.IdempotencyKey
	channelRow, err := w.store.NotificationChannel(ctx, item.ChannelID)
	if err != nil {
		w.retry(ctx, item, fmt.Errorf("load notification channel: %w", err))
		return
	}
	if !channelRow.Enabled {
		if err := w.store.CompleteNotification(ctx, item.ID, w.workerID); err != nil {
			if errors.Is(err, meta.ErrNotificationOutboxClaimLost) {
				w.claimLostTotal.Add(1)
				slog.Warn("notification outbox claim lost while skipping disabled channel", "outbox_id", item.ID,
					"worker_id", w.workerID, "attempts", item.Attempts)
			} else {
				slog.Error("complete disabled notification", "outbox_id", item.ID, "error", err)
			}
			return
		}
		w.skippedTotal.Add(1)
		w.recordDelivery(ctx, item, "skipped", "notification channel is disabled")
		return
	}
	channel := channelFromView(channelRow)
	if n.Status != "resolved" {
		limit := intValue(channel.Config["rate_limit_per_minute"], defaultChannelRateLimit)
		if limit <= 0 {
			limit = defaultChannelRateLimit
		}
		allowed, limitErr := w.limiter.Allow(ctx, "notification-channel:"+strconv.Itoa(channel.ID), limit, w.rateWindow)
		if limitErr != nil {
			slog.Error("check notification rate limit", "channel_id", channel.ID, "error", limitErr)
			w.retry(ctx, item, errors.New("notification rate limiter unavailable"))
			return
		}
		if !allowed {
			if err := w.store.DeferNotification(
				ctx,
				item.ID,
				w.workerID,
				w.rateWindow,
			); err != nil {
				if errors.Is(err, meta.ErrNotificationOutboxClaimLost) {
					w.claimLostTotal.Add(1)
					slog.Warn("notification outbox claim lost while deferring", "outbox_id", item.ID,
						"worker_id", w.workerID, "attempts", item.Attempts)
				} else {
					slog.Error("defer rate-limited notification", "outbox_id", item.ID, "error", err)
				}
				return
			}
			w.deferredTotal.Add(1)
			return
		}
	}
	sendCtx, cancel := context.WithTimeout(ctx, w.deliverTimeout())
	defer cancel()
	_, err = w.sender.Send(sendCtx, channel, n)
	if err != nil {
		w.retry(ctx, item, err)
		return
	}
	if err := w.store.CompleteNotification(ctx, item.ID, w.workerID); err != nil {
		if errors.Is(err, meta.ErrNotificationOutboxClaimLost) {
			w.claimLostTotal.Add(1)
			slog.Warn("notification outbox claim lost before completion", "outbox_id", item.ID,
				"worker_id", w.workerID, "attempts", item.Attempts)
		} else {
			slog.Error("complete notification outbox", "outbox_id", item.ID, "error", err)
		}
		return
	}
	w.deliveredTotal.Add(1)
	w.recordDelivery(ctx, item, "sent", "")
}

func (w *OutboxWorker) deliverTimeout() time.Duration {
	timeout := defaultOutboxDeliveryTimeout
	if halfTTL := w.claimTTL / 2; halfTTL > 0 && halfTTL < timeout {
		timeout = halfTTL
	}
	return timeout
}

func (w *OutboxWorker) retry(ctx context.Context, item meta.NotificationOutboxItem, deliveryErr error) {
	delay, delayErr := backoff.Delay(item.Attempts-1, w.retryBase, time.Hour, 1, 0, 0)
	if delayErr != nil {
		slog.Error("calculate notification retry", "outbox_id", item.ID, "error", delayErr)
		return
	}
	safeMessage := outbound.SanitizeError(deliveryErr)
	status, err := w.store.RetryNotification(ctx, item.ID, w.workerID, safeMessage, delay, w.maxAttempts)
	if err != nil {
		if errors.Is(err, meta.ErrNotificationOutboxClaimLost) {
			w.claimLostTotal.Add(1)
			slog.Warn("notification outbox claim lost before retry update", "outbox_id", item.ID,
				"worker_id", w.workerID, "attempts", item.Attempts)
		} else if !errors.Is(err, context.Canceled) {
			slog.Error("retry notification outbox", "outbox_id", item.ID, "error", err)
		}
		return
	}
	w.retriedTotal.Add(1)
	if status == "dead" {
		w.deadTotal.Add(1)
		w.recordDelivery(ctx, item, "failed", safeMessage)
	}
}

func (w *OutboxWorker) recordDelivery(ctx context.Context, item meta.NotificationOutboxItem, status, message string) {
	var err error
	if item.RuleID == nil {
		err = w.store.RecordPlatformNotificationDelivery(ctx, item.ChannelID, status, message, item.Attempts)
	} else {
		err = w.store.RecordNotificationDelivery(ctx, item.ChannelID, *item.RuleID, status, message, item.Attempts)
	}
	if err != nil {
		slog.Error("record notification delivery", "outbox_id", item.ID, "error", err)
	}
}

func randomOutboxWorkerID() string {
	var value [16]byte
	if _, err := rand.Read(value[:]); err == nil {
		return "outbox-" + hex.EncodeToString(value[:])
	}
	return fmt.Sprintf("outbox-%d", time.Now().UnixNano())
}
