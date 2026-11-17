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
	"time"

	"github.com/eimsound/lazy-observe/internal/backoff"
	"github.com/eimsound/lazy-observe/internal/meta"
	"github.com/eimsound/lazy-observe/internal/state"
)

const (
	defaultOutboxPollInterval = time.Second
	defaultOutboxClaimTTL     = 2 * time.Minute
	defaultOutboxRetryBase    = 5 * time.Second
	defaultOutboxMaxAttempts  = 5
	defaultOutboxClaimBatch   = 8
	defaultChannelRateLimit   = 30
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
}

func NewOutboxWorker(cfg OutboxWorkerConfig) *OutboxWorker {
	if cfg.Store == nil {
		return nil
	}
	if cfg.Sender == nil {
		cfg.Sender = &Sender{}
	}
	if cfg.Sender.Client == nil {
		cfg.Sender.Client = defaultHTTPClient()
	}
	if cfg.Sender.Sleep == nil {
		cfg.Sender.Sleep = time.Sleep
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
		w.drain(ctx)
		select {
		case <-ctx.Done():
			return
		case <-w.wake:
		case <-ticker.C:
		}
	}
}

func (w *OutboxWorker) drain(ctx context.Context) {
	for ctx.Err() == nil {
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
				w.deliver(ctx, item)
			}()
		}
		batch.Wait()
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
			slog.Error("complete disabled notification", "outbox_id", item.ID, "error", err)
			return
		}
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
		if limitErr != nil || !allowed {
			if limitErr != nil {
				slog.Error("check notification rate limit", "channel_id", channel.ID, "error", limitErr)
			}
			if err := w.store.DeferNotification(
				ctx,
				item.ID,
				w.workerID,
				w.rateWindow,
			); err != nil &&
				!errors.Is(err, meta.ErrNotificationOutboxClaimLost) {
				slog.Error("defer rate-limited notification", "outbox_id", item.ID, "error", err)
			}
			return
		}
	}
	_, err = w.sender.Send(ctx, channel, n)
	if err != nil {
		w.retry(ctx, item, err)
		return
	}
	if err := w.store.CompleteNotification(ctx, item.ID, w.workerID); err != nil {
		if !errors.Is(err, meta.ErrNotificationOutboxClaimLost) {
			slog.Error("complete notification outbox", "outbox_id", item.ID, "error", err)
		}
		return
	}
	w.recordDelivery(ctx, item, "sent", "")
}

func (w *OutboxWorker) retry(ctx context.Context, item meta.NotificationOutboxItem, deliveryErr error) {
	delay, delayErr := backoff.Delay(item.Attempts-1, w.retryBase, time.Hour, 1, 0, 0)
	if delayErr != nil {
		slog.Error("calculate notification retry", "outbox_id", item.ID, "error", delayErr)
		return
	}
	status, err := w.store.RetryNotification(ctx, item.ID, w.workerID, deliveryErr.Error(), delay, w.maxAttempts)
	if err != nil {
		if !errors.Is(err, meta.ErrNotificationOutboxClaimLost) && !errors.Is(err, context.Canceled) {
			slog.Error("retry notification outbox", "outbox_id", item.ID, "error", err)
		}
		return
	}
	if status == "dead" {
		w.recordDelivery(ctx, item, "failed", deliveryErr.Error())
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
