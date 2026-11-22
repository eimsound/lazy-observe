package errortrack

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"time"

	"github.com/eimsound/lazy-observe/internal/backoff"
	"go.opentelemetry.io/otel/metric"
)

var (
	ErrInvalid     = errors.New("invalid error work")
	ErrConflict    = errors.New("error event ID conflicts with accepted content")
	ErrCapacity    = errors.New("error Inbox capacity exhausted")
	ErrUnavailable = errors.New("error tracking unavailable")
	ErrClaimLost   = errors.New("error Inbox claim is no longer owned")
)

const (
	// MaxFutureClockSkew is the maximum amount a client-reported occurred_at
	// may lead server accepted_at before being clamped (ADR 0007).
	MaxFutureClockSkew = 5 * time.Minute
	// MaxPastClockSkew is the maximum age a client-reported occurred_at may lag
	// behind server accepted_at before being clamped (ADR 0007).
	MaxPastClockSkew = 30 * 24 * time.Hour

	defaultClaimTTL            = 60 * time.Second
	defaultClaimBatch          = 16
	defaultAttemptTimeout      = 15 * time.Second
	defaultMinSymbolicateLease = 5 * time.Second

	// DefaultClusteredWorkers and MaxClusteredWorkers bound worker concurrency
	// when backed by a clustered metadata store (PostgreSQL).
	DefaultClusteredWorkers = 2
	MaxClusteredWorkers     = 4
)

// ClampOccurredAt enforces ADR 0007 clock-skew bounds on client-reported error
// timestamps so forged or broken clocks cannot pin issue last_seen in the far
// future, bypass retention, or pollute sliding alert windows.
func ClampOccurredAt(occurredAt, acceptedAt time.Time) time.Time {
	accepted := acceptedAt.UTC()
	occurred := occurredAt.UTC()
	if occurred.IsZero() {
		return accepted
	}
	if occurred.After(accepted.Add(MaxFutureClockSkew)) || occurred.Before(accepted.Add(-MaxPastClockSkew)) {
		return accepted
	}
	return occurred
}

type Project struct {
	ID   int
	Slug string
}

type AcceptedEvent struct {
	Raw   json.RawMessage
	Event Event
}

type Accepter interface {
	Accept(context.Context, Project, []AcceptedEvent) error
	Close(context.Context) error
}

type InboxSummary struct {
	ActiveItems           int64 `json:"active_items"`
	ActiveBytes           int64 `json:"active_bytes"`
	RejectionsTotal       int64 `json:"rejections_total"`
	RetriesTotal          int64 `json:"retries_total"`
	TerminalFailuresTotal int64 `json:"terminal_failures_total"`
	DropsTotal            int64 `json:"drops_total"`
}

type InboxMonitor interface {
	Summary(context.Context) (InboxSummary, error)
	Diagnostics(context.Context) (InboxDiagnostics, error)
	Readiness(context.Context) error
}

type InboxEntry struct {
	ProjectID       int
	EventID         string
	Release         string
	CanonicalDigest string
	Payload         []byte
	PayloadBytes    int64
	OccurredAt      time.Time
	AcceptedAt      time.Time
}

type InboxCapacity struct {
	MaxItems       int64
	MaxBytes       int64
	ReadinessItems int64
	ReadinessBytes int64
}

type TerminalDiagnostic struct {
	ProjectID    int        `json:"project_id"`
	EventID      string     `json:"event_id"`
	Attempts     int        `json:"attempts"`
	PayloadBytes int64      `json:"payload_bytes"`
	LastError    string     `json:"last_error"`
	CompletedAt  *time.Time `json:"completed_at,omitempty"`
}

type InboxDiagnostics struct {
	PendingItems          int64                `json:"pending_items"`
	ProcessingItems       int64                `json:"processing_items"`
	ActiveItems           int64                `json:"active_items"`
	ActiveBytes           int64                `json:"active_bytes"`
	OldestAcceptedAt      *time.Time           `json:"oldest_accepted_at,omitempty"`
	RetriesTotal          int64                `json:"retries_total"`
	RejectionsTotal       int64                `json:"rejections_total"`
	TerminalFailuresTotal int64                `json:"terminal_failures_total"`
	DropsTotal            int64                `json:"drops_total"`
	RecentTerminal        []TerminalDiagnostic `json:"recent_terminal"`
}

type InboxItem struct {
	ID                int64
	ProjectID         int
	ProjectSlug       string
	EventID           string
	Payload           []byte
	OccurredAt        time.Time
	AcceptedAt        time.Time
	SourceMapSnapshot json.RawMessage
	ClaimEpoch        int64
	ClaimUntil        time.Time
	Attempts          int
}

type LifecycleEvent struct {
	ID          int64
	ProjectID   int
	IssueID     int
	InboxItemID int64
	Kind        string
	Title       string
	Level       string
	SeenAt      time.Time
	ClaimEpoch  int64
	ClaimUntil  time.Time
	Attempts    int
}

func (e LifecycleEvent) Change() IssueChange {
	return IssueChange{
		LifecycleID: e.ID,
		Kind:        e.Kind,
		ProjectID:   e.ProjectID,
		IssueID:     e.IssueID,
		Title:       e.Title,
		Level:       e.Level,
		SeenAt:      e.SeenAt,
	}
}

type InboxStore interface {
	AcceptErrorEvents(context.Context, []InboxEntry, InboxCapacity) error
	ClaimErrorEvents(context.Context, string, int, time.Time, time.Duration) ([]InboxItem, error)
	CompleteErrorEvent(
		context.Context,
		int64,
		string,
		int64,
		IssueDelta,
		time.Time,
	) ([]IssueChange, error)
	RetryErrorEvent(context.Context, int64, string, int64, time.Time, time.Time, string) error
	ReleaseErrorEvent(context.Context, int64, string, int64, time.Time) error
	TerminalErrorEvent(context.Context, int64, string, int64, time.Time, int, string) error
	ErrorInboxSummary(context.Context) (InboxSummary, error)
	OldestActiveErrorAcceptedAt(context.Context) (*time.Time, error)
	ErrorInboxDiagnostics(context.Context, int) (InboxDiagnostics, error)
}

// LifecycleStore is optionally implemented by InboxStore backends that durably
// persist committed Issue lifecycle events in the same transaction as
// CompleteErrorEvent (ADR 0007).
type LifecycleStore interface {
	ClaimIssueLifecycleEvents(context.Context, string, int, time.Time, time.Duration) ([]LifecycleEvent, error)
	CompleteIssueLifecycleEvent(context.Context, int64, string, int64, time.Time) error
	RetryIssueLifecycleEvent(context.Context, int64, string, int64, time.Time, time.Time, string) error
	ReleaseIssueLifecycleEvent(context.Context, int64, string, int64, time.Time) error
	FailIssueLifecycleEvent(context.Context, int64, string, int64, time.Time, string) error
}

// ErrorCompletion describes one successfully written Inbox item ready for
// batch projection completion.
type ErrorCompletion struct {
	ID          int64
	EventID     string
	ClaimEpoch  int64
	Attempts    int
	Delta       IssueDelta
	ProcessedAt time.Time
}

// BatchCompletionResult reports which items in a batch completed vs lost their
// lease fence, alongside the persisted issue lifecycle changes for the batch.
type BatchCompletionResult struct {
	CompletedIDs []int64
	LostIDs      []int64
	Changes      []IssueChange
}

// BatchInboxStore is optionally implemented by InboxStore backends that complete
// a batch of claimed items within a single metadata transaction.
type BatchInboxStore interface {
	InboxStore
	CompleteErrorEvents(context.Context, string, []ErrorCompletion) (BatchCompletionResult, error)
}

type clusteredStore interface {
	IsClustered() bool
}

type EventWriter interface {
	WriteErrorEvent(context.Context, Project, json.RawMessage, Event) error
}

// BatchEventWriter is optionally implemented by EventWriter backends that write
// multiple pre-parsed events in grouped NDJSON batches.
type BatchEventWriter interface {
	EventWriter
	WriteErrorEvents(context.Context, []PreparedEvent) []error
}

type EventSymbolicator interface {
	Symbolicate(context.Context, string, json.RawMessage, Event) (Event, error)
}

type InboxOptions struct {
	Owner          string
	Logger         *slog.Logger
	Now            func() time.Time
	Capacity       InboxCapacity
	ClaimTTL       time.Duration
	ClaimBatch     int
	Workers        int
	MaxAttempts    int
	RetryInitial   time.Duration
	RetryCap       time.Duration
	PollInterval   time.Duration
	AttemptTimeout time.Duration
	JitterSample   func() float64
	MeterProvider  metric.MeterProvider
	Symbolicator   EventSymbolicator
	OnChange       IssueChangeFunc
	OnEvent        ErrorOccurrenceFunc
}

type Inbox struct {
	store          InboxStore
	batchStore     BatchInboxStore
	lifecycle      LifecycleStore
	writer         EventWriter
	batchWriter    BatchEventWriter
	logger         *slog.Logger
	now            func() time.Time
	capacity       InboxCapacity
	claimTTL       time.Duration
	claimBatch     int
	workers        int
	maxAttempts    int
	retryInitial   time.Duration
	retryCap       time.Duration
	pollInterval   time.Duration
	attemptTimeout time.Duration
	jitterSample   func() float64
	symbolicator   EventSymbolicator
	onChange       IssueChangeFunc
	onEvent        ErrorOccurrenceFunc
	metrics        *inboxMetrics
	owner          string

	ctx       context.Context
	cancel    context.CancelFunc
	wake      chan struct{}
	done      chan struct{}
	wg        sync.WaitGroup
	closeOnce sync.Once
	closed    atomic.Bool
}

var inboxOwnerSequence atomic.Uint64

func generateInboxOwner() string {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return fmt.Sprintf("error-inbox-%d-%d", time.Now().UnixNano(), inboxOwnerSequence.Add(1))
	}
	return fmt.Sprintf("error-inbox-%s-%d", hex.EncodeToString(raw[:]), inboxOwnerSequence.Add(1))
}

func NewInbox(store InboxStore, writer EventWriter, options InboxOptions) (*Inbox, error) {
	if store == nil || writer == nil {
		return nil, fmt.Errorf("%w: metadata store and event writer are required", ErrInvalid)
	}
	if options.Now == nil {
		options.Now = time.Now
	}
	if options.Logger == nil {
		options.Logger = slog.Default()
	}
	if options.ClaimTTL <= 0 {
		options.ClaimTTL = defaultClaimTTL
	}
	if options.ClaimBatch <= 0 {
		options.ClaimBatch = defaultClaimBatch
	}
	if options.Capacity == (InboxCapacity{}) {
		options.Capacity = InboxCapacity{MaxItems: 100_000, MaxBytes: 1 << 30}
	}
	if options.Capacity.MaxItems <= 0 || options.Capacity.MaxBytes <= 0 {
		return nil, fmt.Errorf("%w: Inbox capacity must be positive", ErrInvalid)
	}
	if options.Capacity.ReadinessItems <= 0 {
		options.Capacity.ReadinessItems = max(1, options.Capacity.MaxItems*4/5)
	}
	if options.Capacity.ReadinessBytes <= 0 {
		options.Capacity.ReadinessBytes = max(1, options.Capacity.MaxBytes*4/5)
	}
	if options.Capacity.ReadinessItems > options.Capacity.MaxItems ||
		options.Capacity.ReadinessBytes > options.Capacity.MaxBytes {
		return nil, fmt.Errorf("%w: readiness thresholds exceed Inbox capacity", ErrInvalid)
	}
	if options.MaxAttempts <= 0 {
		options.MaxAttempts = 20
	}
	if options.RetryInitial <= 0 {
		options.RetryInitial = time.Second
	}
	if options.RetryCap <= 0 {
		options.RetryCap = 5 * time.Minute
	}
	if options.RetryCap < options.RetryInitial {
		return nil, fmt.Errorf("%w: retry cap is below initial delay", ErrInvalid)
	}
	if options.PollInterval <= 0 {
		options.PollInterval = time.Second
	}
	if options.AttemptTimeout <= 0 {
		options.AttemptTimeout = defaultAttemptTimeout
		if half := options.ClaimTTL / 2; half > 0 && options.AttemptTimeout > half {
			options.AttemptTimeout = half
		}
	}
	if options.JitterSample == nil {
		options.JitterSample = func() float64 { return 1 }
	}
	owner := options.Owner
	if owner == "" {
		owner = generateInboxOwner()
	}
	clustered := false
	if cs, ok := store.(clusteredStore); ok {
		clustered = cs.IsClustered()
	}
	workers := 1
	if clustered {
		workers = options.Workers
		if workers <= 0 {
			workers = DefaultClusteredWorkers
		}
		if workers > MaxClusteredWorkers {
			workers = MaxClusteredWorkers
		}
	} else if options.Workers > 1 {
		options.Logger.Warn(
			"forcing error Inbox worker count to 1 on non-clustered metadata store",
			"requested_workers", options.Workers,
		)
	}
	batchStore, _ := store.(BatchInboxStore)
	lifecycle, _ := store.(LifecycleStore)
	batchWriter, _ := writer.(BatchEventWriter)
	ctx, cancel := context.WithCancel(context.Background())
	inbox := &Inbox{
		store: store, batchStore: batchStore, lifecycle: lifecycle,
		writer: writer, batchWriter: batchWriter, logger: options.Logger,
		now: options.Now, capacity: options.Capacity, claimTTL: options.ClaimTTL,
		claimBatch: options.ClaimBatch, workers: workers, maxAttempts: options.MaxAttempts,
		retryInitial: options.RetryInitial, retryCap: options.RetryCap,
		pollInterval: options.PollInterval, attemptTimeout: options.AttemptTimeout,
		jitterSample: options.JitterSample,
		symbolicator: options.Symbolicator, onChange: options.OnChange, onEvent: options.OnEvent,
		owner: owner,
		ctx:   ctx, cancel: cancel, wake: make(chan struct{}, workers), done: make(chan struct{}),
	}
	metrics, err := newInboxMetrics(options.MeterProvider, store, options.Now)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("initialize error Inbox metrics: %w", err)
	}
	inbox.metrics = metrics
	inbox.wg.Add(workers)
	for w := range workers {
		workerOwner := owner
		if workers > 1 {
			workerOwner = fmt.Sprintf("%s-w%d", owner, w)
		}
		go inbox.runWorker(workerOwner, w == 0)
	}
	go func() {
		inbox.wg.Wait()
		close(inbox.done)
	}()
	inbox.notifyN(workers)
	return inbox, nil
}

// Workers returns the active background worker count for this Inbox instance.
func (i *Inbox) Workers() int {
	if i == nil {
		return 0
	}
	return i.workers
}

// Owner returns the globally unique lease owner identity of this Inbox worker.
func (i *Inbox) Owner() string {
	if i == nil {
		return ""
	}
	return i.owner
}

func (i *Inbox) Accept(ctx context.Context, project Project, events []AcceptedEvent) error {
	if i == nil || i.closed.Load() {
		return ErrUnavailable
	}
	if project.ID <= 0 || project.Slug == "" || len(events) == 0 {
		return fmt.Errorf("%w: project and at least one event are required", ErrInvalid)
	}
	acceptedAt := i.now().UTC()
	entries := make([]InboxEntry, 0, len(events))
	for _, input := range events {
		if input.Event.EventID == "" || input.Event.Timestamp.IsZero() {
			return fmt.Errorf("%w: event ID and occurrence time are required", ErrInvalid)
		}
		payload, err := canonicalJSON(input.Raw)
		if err != nil {
			return fmt.Errorf("%w: canonicalize event %q: %w", ErrInvalid, input.Event.EventID, err)
		}
		digest := sha256.Sum256(payload)
		entries = append(entries, InboxEntry{
			ProjectID: project.ID, EventID: input.Event.EventID, Release: input.Event.Release,
			CanonicalDigest: hex.EncodeToString(digest[:]), Payload: payload, PayloadBytes: int64(len(payload)),
			OccurredAt: ClampOccurredAt(input.Event.Timestamp, acceptedAt), AcceptedAt: acceptedAt,
		})
	}
	if err := i.store.AcceptErrorEvents(ctx, entries, i.capacity); err != nil {
		if errors.Is(err, ErrInvalid) || errors.Is(err, ErrConflict) || errors.Is(err, ErrCapacity) ||
			errors.Is(err, ErrUnavailable) {
			return err
		}
		return fmt.Errorf("%w: durably accept error events: %w", ErrUnavailable, err)
	}
	i.notifyN((len(entries) + i.claimBatch - 1) / i.claimBatch)
	return nil
}

func (i *Inbox) Summary(ctx context.Context) (InboxSummary, error) {
	if i == nil {
		return InboxSummary{}, ErrUnavailable
	}
	return i.store.ErrorInboxSummary(ctx)
}

func (i *Inbox) Diagnostics(ctx context.Context) (InboxDiagnostics, error) {
	if i == nil {
		return InboxDiagnostics{}, ErrUnavailable
	}
	return i.store.ErrorInboxDiagnostics(ctx, 20)
}

func (i *Inbox) Readiness(ctx context.Context) error {
	summary, err := i.Summary(ctx)
	if err != nil {
		return err
	}
	if summary.ActiveItems >= i.capacity.ReadinessItems || summary.ActiveBytes >= i.capacity.ReadinessBytes {
		return fmt.Errorf(
			"error Inbox backlog exceeds readiness threshold: items=%d bytes=%d",
			summary.ActiveItems,
			summary.ActiveBytes,
		)
	}
	return nil
}

func (i *Inbox) Close(ctx context.Context) error {
	if i == nil {
		return nil
	}
	i.closeOnce.Do(func() {
		i.closed.Store(true)
		i.cancel()
		_ = i.metrics.Close()
	})
	select {
	case <-i.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (i *Inbox) notifyN(n int) {
	if n <= 0 {
		n = 1
	}
	if n > i.workers {
		n = i.workers
	}
	for range n {
		select {
		case i.wake <- struct{}{}:
		default:
			return
		}
	}
}

func (i *Inbox) runWorker(workerOwner string, pollLifecycle bool) {
	defer i.wg.Done()
	ticker := time.NewTicker(i.pollInterval)
	defer ticker.Stop()
	startupOrphaned := pollLifecycle
	for {
		select {
		case <-i.ctx.Done():
			return
		case <-i.wake:
			i.safeProcessCycle(workerOwner, startupOrphaned)
			startupOrphaned = false
		case <-ticker.C:
			i.safeProcessCycle(workerOwner, pollLifecycle)
			startupOrphaned = false
		}
	}
}

func (i *Inbox) safeProcessCycle(workerOwner string, dispatchOrphaned bool) {
	defer func() {
		if recovered := recover(); recovered != nil {
			i.logger.Error(
				"recovered panic in error Inbox worker loop",
				"owner", workerOwner,
				"panic", recovered,
				"stack", string(debug.Stack()),
			)
		}
	}()
	i.processAvailable(workerOwner)
	if dispatchOrphaned {
		i.dispatchLifecycleEvents(workerOwner)
	}
}

type batchProcessStats struct {
	attempted    int
	leaseSkipped int
}

type preparedInboxItem struct {
	item     InboxItem
	prepared PreparedEvent
	delta    IssueDelta
}

func (i *Inbox) processAvailable(workerOwner string) {
	for i.ctx.Err() == nil {
		items, err := i.store.ClaimErrorEvents(i.ctx, workerOwner, i.claimBatch, i.now().UTC(), i.claimTTL)
		if err != nil || len(items) == 0 {
			return
		}
		stats := i.processBatch(workerOwner, items)
		if len(items) < i.claimBatch || (stats.attempted == 0 && stats.leaseSkipped > 0) {
			return
		}
	}
}

func (i *Inbox) processBatch(workerOwner string, items []InboxItem) batchProcessStats {
	var stats batchProcessStats
	ready := make([]preparedInboxItem, 0, len(items))
	for idx, item := range items {
		if i.ctx.Err() != nil {
			i.releasePrepared(workerOwner, ready)
			i.releaseRemaining(workerOwner, items[idx:])
			return stats
		}
		if !item.ClaimUntil.IsZero() && !i.now().UTC().Before(item.ClaimUntil) {
			i.logger.Warn(
				"skip error Inbox item after claim lease expired",
				"item_id", item.ID,
				"event_id", item.EventID,
				"project_id", item.ProjectID,
				"owner", workerOwner,
				"claim_epoch", item.ClaimEpoch,
				"attempts", item.Attempts,
			)
			stats.leaseSkipped++
			continue
		}
		if item.Attempts > i.maxAttempts {
			stats.attempted++
			err := i.store.TerminalErrorEvent(
				i.ctx,
				item.ID,
				workerOwner,
				item.ClaimEpoch,
				i.now().UTC(),
				i.maxAttempts,
				"retry_budget: exhausted",
			)
			i.logFenceOrError(workerOwner, item, "terminal_budget", err)
			continue
		}
		prepared, ok, leaseExpired := i.prepareItem(workerOwner, item)
		if leaseExpired {
			stats.leaseSkipped++
			continue
		}
		stats.attempted++
		if ok {
			ready = append(ready, prepared)
		}
	}
	if len(ready) == 0 {
		return stats
	}
	if i.ctx.Err() != nil {
		i.releasePrepared(workerOwner, ready)
		return stats
	}

	active := make([]preparedInboxItem, 0, len(ready))
	for _, entry := range ready {
		if !entry.item.ClaimUntil.IsZero() && !i.now().UTC().Before(entry.item.ClaimUntil) {
			i.logger.Warn(
				"skip error Inbox item after claim lease expired",
				"item_id", entry.item.ID,
				"event_id", entry.item.EventID,
				"project_id", entry.item.ProjectID,
				"owner", workerOwner,
				"claim_epoch", entry.item.ClaimEpoch,
				"attempts", entry.item.Attempts,
			)
			stats.leaseSkipped++
			continue
		}
		active = append(active, entry)
	}
	if len(active) == 0 {
		return stats
	}

	written := make([]preparedInboxItem, 0, len(active))
	if i.batchWriter != nil {
		writeErrs := i.writePreparedBatch(workerOwner, active)
		for idx, entry := range active {
			if writeErrs[idx] != nil {
				i.retry(workerOwner, entry.item, "write_vlogs", writeErrs[idx])
				continue
			}
			written = append(written, entry)
		}
	} else {
		for idx, entry := range active {
			if i.ctx.Err() != nil {
				i.releasePrepared(workerOwner, active[idx:])
				break
			}
			if !entry.item.ClaimUntil.IsZero() && !i.now().UTC().Before(entry.item.ClaimUntil) {
				i.logger.Warn(
					"skip error Inbox item after claim lease expired",
					"item_id", entry.item.ID,
					"event_id", entry.item.EventID,
					"project_id", entry.item.ProjectID,
					"owner", workerOwner,
					"claim_epoch", entry.item.ClaimEpoch,
					"attempts", entry.item.Attempts,
				)
				stats.leaseSkipped++
				continue
			}
			if err := i.writeSingleFallback(workerOwner, entry); err != nil {
				i.retry(workerOwner, entry.item, "write_vlogs", err)
				continue
			}
			written = append(written, entry)
		}
	}
	if len(written) == 0 {
		return stats
	}
	if i.ctx.Err() != nil {
		i.releasePrepared(workerOwner, written)
		return stats
	}

	i.completeBatch(workerOwner, written)
	return stats
}

func (i *Inbox) minSymbolicateLease() time.Duration {
	minLease := defaultMinSymbolicateLease
	if quarter := i.claimTTL / 4; quarter > 0 && quarter < minLease {
		minLease = quarter
	}
	return minLease
}

func (i *Inbox) prepareItem(workerOwner string, item InboxItem) (res preparedInboxItem, ok bool, leaseExpired bool) {
	defer func() {
		if recovered := recover(); recovered != nil {
			i.logger.Error(
				"recovered panic while processing error Inbox item",
				"item_id", item.ID,
				"event_id", item.EventID,
				"project_id", item.ProjectID,
				"owner", workerOwner,
				"claim_epoch", item.ClaimEpoch,
				"attempts", item.Attempts,
				"panic", recovered,
				"stack", string(debug.Stack()),
			)
			i.retry(workerOwner, item, "panic", fmt.Errorf("panic: %v", recovered))
			res, ok, leaseExpired = preparedInboxItem{}, false, false
		}
	}()
	var event Event
	if err := json.Unmarshal(item.Payload, &event); err != nil {
		i.retry(workerOwner, item, "decode_event", err)
		return preparedInboxItem{}, false, false
	}
	event.Timestamp = item.OccurredAt.UTC()
	project := Project{ID: item.ProjectID, Slug: item.ProjectSlug}
	var rawStack *Stacktrace
	if i.symbolicator != nil {
		symTimeout := i.attemptTimeout
		if !item.ClaimUntil.IsZero() {
			remaining := item.ClaimUntil.Sub(i.now().UTC())
			if remaining <= 0 {
				i.logger.Warn(
					"skip error Inbox symbolication after claim lease expired",
					"item_id", item.ID,
					"event_id", item.EventID,
					"project_id", item.ProjectID,
					"owner", workerOwner,
					"claim_epoch", item.ClaimEpoch,
					"attempts", item.Attempts,
				)
				return preparedInboxItem{}, false, true
			}
			minLease := i.minSymbolicateLease()
			if remaining < minLease {
				i.logger.Warn(
					"release error Inbox item before symbolication due to insufficient remaining lease",
					"item_id", item.ID,
					"event_id", item.EventID,
					"project_id", item.ProjectID,
					"owner", workerOwner,
					"claim_epoch", item.ClaimEpoch,
					"attempts", item.Attempts,
					"remaining", remaining,
					"min_remaining", minLease,
				)
				releaseCtx, releaseCancel := context.WithTimeout(context.Background(), 5*time.Second)
				releaseErr := i.store.ReleaseErrorEvent(
					releaseCtx,
					item.ID,
					workerOwner,
					item.ClaimEpoch,
					i.now().UTC(),
				)
				releaseCancel()
				i.logFenceOrError(workerOwner, item, "release_low_lease", releaseErr)
				return preparedInboxItem{}, false, false
			}
			if remaining < symTimeout {
				symTimeout = remaining
			}
		}
		origStack := cloneStacktrace(event.Exception.Stacktrace)
		rawStack = &origStack
		symInput := event
		symInput.Exception.Stacktrace = cloneStacktrace(event.Exception.Stacktrace)
		symCtx, symCancel := context.WithTimeout(i.ctx, symTimeout)
		symbolicated, err := i.symbolicator.Symbolicate(symCtx, project.Slug, item.SourceMapSnapshot, symInput)
		symCancel()
		if err != nil {
			i.retry(workerOwner, item, "symbolicate", err)
			return preparedInboxItem{}, false, false
		}
		event = symbolicated
	}
	fp := Fingerprint(event)
	return preparedInboxItem{
		item: item,
		prepared: PreparedEvent{
			Project:       project,
			Raw:           item.Payload,
			Event:         event,
			RawStacktrace: rawStack,
			Fingerprint:   fp,
		},
		delta: IssueDelta{
			ProjectID:   item.ProjectID,
			Fingerprint: fp,
			Title:       event.Title(),
			Level:       event.Level,
			Count:       1,
			SessionBits: sessionBit(event.SessionID),
			FirstSeen:   item.OccurredAt,
			LastSeen:    item.OccurredAt,
			LastEventID: item.EventID,
			AcceptedAt:  item.AcceptedAt,
		},
	}, true, false
}

func cloneStacktrace(s Stacktrace) Stacktrace {
	if len(s.Frames) == 0 {
		return s
	}
	frames := make([]Frame, len(s.Frames))
	for idx, f := range s.Frames {
		if f.Lineno != nil {
			v := *f.Lineno
			f.Lineno = &v
		}
		if f.Colno != nil {
			v := *f.Colno
			f.Colno = &v
		}
		frames[idx] = f
	}
	s.Frames = frames
	return s
}

func (i *Inbox) writePreparedBatch(workerOwner string, active []preparedInboxItem) (errs []error) {
	errs = make([]error, len(active))
	defer func() {
		if recovered := recover(); recovered != nil {
			i.logger.Error(
				"recovered panic while batch-writing error Inbox items",
				"owner", workerOwner,
				"batch_size", len(active),
				"panic", recovered,
				"stack", string(debug.Stack()),
			)
			panicErr := fmt.Errorf("panic: %v", recovered)
			for idx := range errs {
				if errs[idx] == nil {
					errs[idx] = panicErr
				}
			}
		}
	}()
	batch := make([]PreparedEvent, len(active))
	for idx, entry := range active {
		batch[idx] = entry.prepared
	}
	attemptCtx, cancel := context.WithTimeout(i.ctx, i.attemptTimeout)
	defer cancel()
	writeErrs := i.batchWriter.WriteErrorEvents(attemptCtx, batch)
	for idx := range active {
		if idx < len(writeErrs) {
			errs[idx] = writeErrs[idx]
		} else {
			errs[idx] = fmt.Errorf("%w: incomplete batch writer result", ErrUnavailable)
		}
	}
	return errs
}

func (i *Inbox) writeSingleFallback(workerOwner string, entry preparedInboxItem) (err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			i.logger.Error(
				"recovered panic while writing error Inbox item",
				"item_id", entry.item.ID,
				"event_id", entry.item.EventID,
				"project_id", entry.item.ProjectID,
				"owner", workerOwner,
				"claim_epoch", entry.item.ClaimEpoch,
				"attempts", entry.item.Attempts,
				"panic", recovered,
				"stack", string(debug.Stack()),
			)
			err = fmt.Errorf("panic: %v", recovered)
		}
	}()
	attemptCtx, cancel := context.WithTimeout(i.ctx, i.attemptTimeout)
	defer cancel()
	return i.writer.WriteErrorEvent(attemptCtx, entry.prepared.Project, entry.prepared.Raw, entry.prepared.Event)
}

func (i *Inbox) completeBatch(workerOwner string, written []preparedInboxItem) {
	completeCtx, cancel := context.WithTimeout(i.ctx, i.attemptTimeout)
	defer cancel()
	if i.batchStore != nil {
		appliedAt := i.now().UTC()
		completions := make([]ErrorCompletion, len(written))
		for idx, entry := range written {
			delta := entry.delta
			delta.AppliedAt = appliedAt
			completions[idx] = ErrorCompletion{
				ID:          entry.item.ID,
				EventID:     entry.item.EventID,
				ClaimEpoch:  entry.item.ClaimEpoch,
				Attempts:    entry.item.Attempts,
				Delta:       delta,
				ProcessedAt: appliedAt,
			}
		}
		res, err := i.batchStore.CompleteErrorEvents(completeCtx, workerOwner, completions)
		if err != nil {
			for _, entry := range written {
				i.retry(workerOwner, entry.item, "complete_projection", err)
			}
			return
		}
		lostSet := make(map[int64]struct{}, len(res.LostIDs))
		for _, id := range res.LostIDs {
			lostSet[id] = struct{}{}
		}
		completedSet := make(map[int64]struct{}, len(res.CompletedIDs))
		for _, id := range res.CompletedIDs {
			completedSet[id] = struct{}{}
		}
		for _, entry := range written {
			if _, lost := lostSet[entry.item.ID]; lost {
				i.logFenceOrError(workerOwner, entry.item, "complete_projection", ErrClaimLost)
				continue
			}
			if _, ok := completedSet[entry.item.ID]; ok && i.onEvent != nil {
				_ = i.onEvent(completeCtx, ErrorOccurrence{
					ProjectID:  entry.item.ProjectID,
					OccurredAt: entry.item.OccurredAt,
					AcceptedAt: entry.item.AcceptedAt,
				})
			}
		}
		i.dispatchChangesAtBatchEnd(completeCtx, workerOwner, res.Changes)
		return
	}

	allChanges := make([]IssueChange, 0)
	for _, entry := range written {
		delta := entry.delta
		delta.AppliedAt = i.now().UTC()
		changes, err := i.store.CompleteErrorEvent(
			completeCtx,
			entry.item.ID,
			workerOwner,
			entry.item.ClaimEpoch,
			delta,
			delta.AppliedAt,
		)
		if err != nil {
			if errors.Is(err, ErrClaimLost) {
				i.logFenceOrError(workerOwner, entry.item, "complete_projection", err)
				continue
			}
			i.retry(workerOwner, entry.item, "complete_projection", err)
			continue
		}
		if i.onEvent != nil {
			_ = i.onEvent(completeCtx, ErrorOccurrence{
				ProjectID:  entry.item.ProjectID,
				OccurredAt: entry.item.OccurredAt,
				AcceptedAt: entry.item.AcceptedAt,
			})
		}
		allChanges = append(allChanges, changes...)
	}
	i.dispatchChangesAtBatchEnd(completeCtx, workerOwner, allChanges)
}

func (i *Inbox) dispatchChangesAtBatchEnd(ctx context.Context, workerOwner string, changes []IssueChange) {
	if i.onChange == nil || len(changes) == 0 {
		return
	}
	if i.lifecycle != nil {
		i.dispatchLifecycleEvents(workerOwner)
		return
	}
	for _, change := range changes {
		_ = i.onChange(ctx, change)
	}
}

func (i *Inbox) releasePrepared(workerOwner string, items []preparedInboxItem) {
	if len(items) == 0 {
		return
	}
	raw := make([]InboxItem, len(items))
	for idx, entry := range items {
		raw[idx] = entry.item
	}
	i.releaseRemaining(workerOwner, raw)
}

func (i *Inbox) releaseRemaining(workerOwner string, items []InboxItem) {
	if len(items) == 0 {
		return
	}
	cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	now := i.now().UTC()
	for _, item := range items {
		err := i.store.ReleaseErrorEvent(cleanupCtx, item.ID, workerOwner, item.ClaimEpoch, now)
		i.logFenceOrError(workerOwner, item, "release_shutdown", err)
	}
}

func (i *Inbox) dispatchLifecycleEvents(workerOwner string) {
	if i.lifecycle == nil || i.onChange == nil {
		return
	}
	for i.ctx.Err() == nil {
		events, err := i.lifecycle.ClaimIssueLifecycleEvents(
			i.ctx,
			workerOwner,
			i.claimBatch,
			i.now().UTC(),
			i.claimTTL,
		)
		if err != nil || len(events) == 0 {
			return
		}
		for idx, evt := range events {
			if i.ctx.Err() != nil {
				i.releaseRemainingLifecycle(workerOwner, events[idx:])
				return
			}
			if !evt.ClaimUntil.IsZero() && !i.now().UTC().Before(evt.ClaimUntil) {
				i.logger.Warn(
					"skip issue lifecycle event after claim lease expired",
					"lifecycle_id", evt.ID,
					"issue_id", evt.IssueID,
					"project_id", evt.ProjectID,
					"kind", evt.Kind,
					"owner", workerOwner,
					"claim_epoch", evt.ClaimEpoch,
					"attempts", evt.Attempts,
				)
				continue
			}
			i.processLifecycleEvent(workerOwner, evt)
		}
		if len(events) < i.claimBatch {
			return
		}
	}
}

func (i *Inbox) releaseRemainingLifecycle(workerOwner string, events []LifecycleEvent) {
	if len(events) == 0 {
		return
	}
	cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	now := i.now().UTC()
	for _, evt := range events {
		err := i.lifecycle.ReleaseIssueLifecycleEvent(cleanupCtx, evt.ID, workerOwner, evt.ClaimEpoch, now)
		i.logLifecycleFenceOrError(workerOwner, evt, "release_shutdown", err)
	}
}

func (i *Inbox) processLifecycleEvent(workerOwner string, evt LifecycleEvent) {
	defer func() {
		if recovered := recover(); recovered != nil {
			i.logger.Error(
				"recovered panic while dispatching issue lifecycle event",
				"lifecycle_id", evt.ID,
				"issue_id", evt.IssueID,
				"project_id", evt.ProjectID,
				"kind", evt.Kind,
				"owner", workerOwner,
				"claim_epoch", evt.ClaimEpoch,
				"panic", recovered,
				"stack", string(debug.Stack()),
			)
			i.retryLifecycleEvent(workerOwner, evt, fmt.Errorf("panic: %v", recovered))
		}
	}()
	if evt.Attempts > i.maxAttempts {
		err := i.lifecycle.FailIssueLifecycleEvent(
			i.ctx,
			evt.ID,
			workerOwner,
			evt.ClaimEpoch,
			i.now().UTC(),
			"retry_budget: exhausted",
		)
		i.logLifecycleFenceOrError(workerOwner, evt, "fail_budget", err)
		return
	}
	attemptCtx, cancel := context.WithTimeout(i.ctx, i.attemptTimeout)
	defer cancel()
	if err := i.onChange(attemptCtx, evt.Change()); err != nil {
		i.retryLifecycleEvent(workerOwner, evt, err)
		return
	}
	err := i.lifecycle.CompleteIssueLifecycleEvent(attemptCtx, evt.ID, workerOwner, evt.ClaimEpoch, i.now().UTC())
	i.logLifecycleFenceOrError(workerOwner, evt, "complete", err)
}

func (i *Inbox) retryLifecycleEvent(workerOwner string, evt LifecycleEvent, cause error) {
	now := i.now().UTC()
	ctx := i.ctx
	if ctx.Err() != nil {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		ctx = cleanupCtx
	}
	if i.ctx.Err() != nil && errors.Is(cause, context.Canceled) {
		err := i.lifecycle.ReleaseIssueLifecycleEvent(ctx, evt.ID, workerOwner, evt.ClaimEpoch, now)
		i.logLifecycleFenceOrError(workerOwner, evt, "release_canceled", err)
		return
	}
	reason := "dispatch: failed"
	if errors.Is(cause, context.DeadlineExceeded) {
		reason = "dispatch: deadline_exceeded"
	} else if errors.Is(cause, context.Canceled) {
		reason = "dispatch: canceled"
	}
	if evt.Attempts >= i.maxAttempts {
		err := i.lifecycle.FailIssueLifecycleEvent(ctx, evt.ID, workerOwner, evt.ClaimEpoch, now, reason)
		i.logLifecycleFenceOrError(workerOwner, evt, "fail_exhausted", err)
		return
	}
	delay, err := backoff.Delay(
		evt.Attempts-1,
		i.retryInitial,
		i.retryCap,
		i.jitterSample(),
		0,
		0,
	)
	if err != nil {
		failErr := i.lifecycle.FailIssueLifecycleEvent(
			ctx,
			evt.ID,
			workerOwner,
			evt.ClaimEpoch,
			now,
			"retry_policy: invalid",
		)
		i.logLifecycleFenceOrError(workerOwner, evt, "fail_policy", failErr)
		return
	}
	retryErr := i.lifecycle.RetryIssueLifecycleEvent(
		ctx,
		evt.ID,
		workerOwner,
		evt.ClaimEpoch,
		now,
		now.Add(delay),
		reason,
	)
	i.logLifecycleFenceOrError(workerOwner, evt, "retry", retryErr)
}

func (i *Inbox) retry(workerOwner string, item InboxItem, stage string, cause error) {
	now := i.now().UTC()
	reason := stage + ": failed"
	if errors.Is(cause, context.DeadlineExceeded) {
		reason = stage + ": deadline_exceeded"
	} else if errors.Is(cause, context.Canceled) {
		reason = stage + ": canceled"
	}
	ctx := i.ctx
	if ctx.Err() != nil {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		ctx = cleanupCtx
	}
	if i.ctx.Err() != nil && errors.Is(cause, context.Canceled) {
		err := i.store.ReleaseErrorEvent(ctx, item.ID, workerOwner, item.ClaimEpoch, now)
		i.logFenceOrError(workerOwner, item, "release_canceled", err)
		return
	}
	if item.Attempts >= i.maxAttempts {
		err := i.store.TerminalErrorEvent(ctx, item.ID, workerOwner, item.ClaimEpoch, now, i.maxAttempts, reason)
		i.logFenceOrError(workerOwner, item, "terminal_exhausted", err)
		return
	}
	delay, err := backoff.Delay(
		item.Attempts-1,
		i.retryInitial,
		i.retryCap,
		i.jitterSample(),
		0,
		0,
	)
	if err != nil {
		termErr := i.store.TerminalErrorEvent(
			ctx,
			item.ID,
			workerOwner,
			item.ClaimEpoch,
			now,
			i.maxAttempts,
			"retry_policy: invalid",
		)
		i.logFenceOrError(workerOwner, item, "terminal_policy", termErr)
		return
	}
	retryErr := i.store.RetryErrorEvent(ctx, item.ID, workerOwner, item.ClaimEpoch, now, now.Add(delay), reason)
	i.logFenceOrError(workerOwner, item, "retry_"+stage, retryErr)
}

func (i *Inbox) logFenceOrError(workerOwner string, item InboxItem, stage string, err error) {
	if err == nil {
		return
	}
	if errors.Is(err, ErrClaimLost) {
		i.logger.Warn(
			"discard error Inbox mutation after losing claim lease",
			"item_id", item.ID,
			"event_id", item.EventID,
			"project_id", item.ProjectID,
			"owner", workerOwner,
			"claim_epoch", item.ClaimEpoch,
			"attempts", item.Attempts,
			"stage", stage,
			"error", err,
		)
		return
	}
	i.logger.Warn(
		"error Inbox state transition failed",
		"item_id", item.ID,
		"event_id", item.EventID,
		"project_id", item.ProjectID,
		"owner", workerOwner,
		"claim_epoch", item.ClaimEpoch,
		"attempts", item.Attempts,
		"stage", stage,
		"error", err,
	)
}

func (i *Inbox) logLifecycleFenceOrError(workerOwner string, evt LifecycleEvent, stage string, err error) {
	if err == nil {
		return
	}
	if errors.Is(err, ErrClaimLost) {
		i.logger.Warn(
			"discard issue lifecycle transition after losing claim lease",
			"lifecycle_id", evt.ID,
			"issue_id", evt.IssueID,
			"project_id", evt.ProjectID,
			"kind", evt.Kind,
			"owner", workerOwner,
			"claim_epoch", evt.ClaimEpoch,
			"attempts", evt.Attempts,
			"stage", stage,
			"error", err,
		)
		return
	}
	i.logger.Warn(
		"issue lifecycle state transition failed",
		"lifecycle_id", evt.ID,
		"issue_id", evt.IssueID,
		"project_id", evt.ProjectID,
		"kind", evt.Kind,
		"owner", workerOwner,
		"claim_epoch", evt.ClaimEpoch,
		"attempts", evt.Attempts,
		"stage", stage,
		"error", err,
	)
}

func canonicalJSON(raw json.RawMessage) ([]byte, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return nil, errors.New("multiple JSON values")
		}
		return nil, err
	}
	return json.Marshal(value)
}
