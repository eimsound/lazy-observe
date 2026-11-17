package errortrack

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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

type InboxMonitor interface {
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
	Attempts          int
}

type InboxStore interface {
	AcceptErrorEvents(context.Context, []InboxEntry, InboxCapacity) error
	ClaimErrorEvents(context.Context, string, int, time.Time, time.Duration) ([]InboxItem, error)
	CompleteErrorEvent(
		context.Context,
		int64,
		string,
		IssueDelta,
		time.Time,
	) ([]IssueChange, error)
	RetryErrorEvent(context.Context, int64, string, time.Time, time.Time, string) error
	ReleaseErrorEvent(context.Context, int64, string, time.Time) error
	TerminalErrorEvent(context.Context, int64, string, time.Time, int, string) error
	ErrorInboxDiagnostics(context.Context, int) (InboxDiagnostics, error)
}

type EventWriter interface {
	WriteErrorEvent(context.Context, Project, json.RawMessage, Event) error
}

type EventSymbolicator interface {
	Symbolicate(context.Context, string, json.RawMessage, Event) Event
}

type InboxOptions struct {
	Now            func() time.Time
	Capacity       InboxCapacity
	ClaimTTL       time.Duration
	ClaimBatch     int
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
	writer         EventWriter
	now            func() time.Time
	capacity       InboxCapacity
	claimTTL       time.Duration
	claimBatch     int
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
	closeOnce sync.Once
	closed    atomic.Bool
}

var inboxOwnerSequence atomic.Uint64

func NewInbox(store InboxStore, writer EventWriter, options InboxOptions) (*Inbox, error) {
	if store == nil || writer == nil {
		return nil, fmt.Errorf("%w: metadata store and event writer are required", ErrInvalid)
	}
	if options.Now == nil {
		options.Now = time.Now
	}
	if options.ClaimTTL <= 0 {
		options.ClaimTTL = 30 * time.Second
	}
	if options.ClaimBatch <= 0 {
		options.ClaimBatch = 16
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
		options.AttemptTimeout = 30 * time.Second
	}
	if options.JitterSample == nil {
		options.JitterSample = func() float64 { return 1 }
	}
	ctx, cancel := context.WithCancel(context.Background())
	inbox := &Inbox{
		store: store, writer: writer, now: options.Now, capacity: options.Capacity, claimTTL: options.ClaimTTL,
		claimBatch: options.ClaimBatch, maxAttempts: options.MaxAttempts,
		retryInitial: options.RetryInitial, retryCap: options.RetryCap,
		pollInterval: options.PollInterval, attemptTimeout: options.AttemptTimeout,
		jitterSample: options.JitterSample,
		symbolicator: options.Symbolicator, onChange: options.OnChange, onEvent: options.OnEvent,
		owner: fmt.Sprintf("error-inbox-%d", inboxOwnerSequence.Add(1)),
		ctx:   ctx, cancel: cancel, wake: make(chan struct{}, 1), done: make(chan struct{}),
	}
	metrics, err := newInboxMetrics(options.MeterProvider, store, options.Now)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("initialize error Inbox metrics: %w", err)
	}
	inbox.metrics = metrics
	go inbox.run()
	inbox.notify()
	return inbox, nil
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
			return fmt.Errorf("%w: canonicalize event %q: %v", ErrInvalid, input.Event.EventID, err)
		}
		digest := sha256.Sum256(payload)
		entries = append(entries, InboxEntry{
			ProjectID: project.ID, EventID: input.Event.EventID, Release: input.Event.Release,
			CanonicalDigest: hex.EncodeToString(digest[:]), Payload: payload, PayloadBytes: int64(len(payload)),
			OccurredAt: input.Event.Timestamp.UTC(), AcceptedAt: acceptedAt,
		})
	}
	if err := i.store.AcceptErrorEvents(ctx, entries, i.capacity); err != nil {
		if errors.Is(err, ErrInvalid) || errors.Is(err, ErrConflict) || errors.Is(err, ErrCapacity) ||
			errors.Is(err, ErrUnavailable) {
			return err
		}
		return fmt.Errorf("%w: durably accept error events: %v", ErrUnavailable, err)
	}
	i.notify()
	return nil
}

func (i *Inbox) Diagnostics(ctx context.Context) (InboxDiagnostics, error) {
	if i == nil {
		return InboxDiagnostics{}, ErrUnavailable
	}
	return i.store.ErrorInboxDiagnostics(ctx, 20)
}

func (i *Inbox) Readiness(ctx context.Context) error {
	diagnostics, err := i.Diagnostics(ctx)
	if err != nil {
		return err
	}
	if diagnostics.ActiveItems >= i.capacity.ReadinessItems || diagnostics.ActiveBytes >= i.capacity.ReadinessBytes {
		return fmt.Errorf(
			"error Inbox backlog exceeds readiness threshold: items=%d bytes=%d",
			diagnostics.ActiveItems,
			diagnostics.ActiveBytes,
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

func (i *Inbox) notify() {
	select {
	case i.wake <- struct{}{}:
	default:
	}
}

func (i *Inbox) run() {
	defer close(i.done)
	ticker := time.NewTicker(i.pollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-i.ctx.Done():
			return
		case <-i.wake:
			i.processAvailable()
		case <-ticker.C:
			i.processAvailable()
		}
	}
}

func (i *Inbox) processAvailable() {
	for i.ctx.Err() == nil {
		items, err := i.store.ClaimErrorEvents(i.ctx, i.owner, i.claimBatch, i.now().UTC(), i.claimTTL)
		if err != nil || len(items) == 0 {
			return
		}
		for _, item := range items {
			i.process(item)
		}
		if len(items) < i.claimBatch {
			return
		}
	}
}

func (i *Inbox) process(item InboxItem) {
	if item.Attempts > i.maxAttempts {
		_ = i.store.TerminalErrorEvent(
			i.ctx,
			item.ID,
			i.owner,
			i.now().UTC(),
			i.maxAttempts,
			"retry_budget: exhausted",
		)
		return
	}
	attemptCtx, cancel := context.WithTimeout(i.ctx, i.attemptTimeout)
	defer cancel()
	var event Event
	if err := json.Unmarshal(item.Payload, &event); err != nil {
		i.retry(item, "decode_event", err)
		return
	}
	event.Timestamp = item.OccurredAt.UTC()
	project := Project{ID: item.ProjectID, Slug: item.ProjectSlug}
	if i.symbolicator != nil {
		event = i.symbolicator.Symbolicate(attemptCtx, project.Slug, item.SourceMapSnapshot, event)
	}
	if err := i.writer.WriteErrorEvent(attemptCtx, project, item.Payload, event); err != nil {
		i.retry(item, "write_vlogs", err)
		return
	}
	delta := IssueDelta{
		ProjectID: item.ProjectID, Fingerprint: Fingerprint(event), Title: event.Title(), Level: event.Level,
		Count: 1, SessionBits: sessionBit(event.SessionID), FirstSeen: item.OccurredAt,
		LastSeen: item.OccurredAt, LastEventID: item.EventID, AcceptedAt: item.AcceptedAt, AppliedAt: i.now().UTC(),
	}
	changes, err := i.store.CompleteErrorEvent(attemptCtx, item.ID, i.owner, delta, delta.AppliedAt)
	if err != nil {
		i.retry(item, "complete_projection", err)
		return
	}
	if i.onEvent != nil {
		_ = i.onEvent(attemptCtx, ErrorOccurrence{
			ProjectID: item.ProjectID, OccurredAt: item.OccurredAt, AcceptedAt: item.AcceptedAt,
		})
	}
	if i.onChange != nil {
		for _, change := range changes {
			_ = i.onChange(attemptCtx, change)
		}
	}
}

func (i *Inbox) retry(item InboxItem, stage string, cause error) {
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
		_ = i.store.ReleaseErrorEvent(ctx, item.ID, i.owner, now)
		return
	}
	if item.Attempts >= i.maxAttempts {
		_ = i.store.TerminalErrorEvent(ctx, item.ID, i.owner, now, i.maxAttempts, reason)
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
		_ = i.store.TerminalErrorEvent(ctx, item.ID, i.owner, now, i.maxAttempts, "retry_policy: invalid")
		return
	}
	_ = i.store.RetryErrorEvent(ctx, item.ID, i.owner, now, now.Add(delay), reason)
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
