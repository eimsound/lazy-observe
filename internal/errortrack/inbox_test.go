package errortrack_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/eimsound/lazy-observe/internal/errortrack"
	"github.com/eimsound/lazy-observe/internal/meta"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

type inboxTestWriter struct {
	started chan struct{}
	release chan struct{}
	fail    bool
	calls   atomic.Int64
}

func (w *inboxTestWriter) WriteErrorEvent(
	ctx context.Context,
	_ errortrack.Project,
	_ json.RawMessage,
	_ errortrack.Event,
) error {
	w.calls.Add(1)
	if w.started != nil {
		select {
		case w.started <- struct{}{}:
		default:
		}
	}
	if w.release != nil {
		select {
		case <-w.release:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if w.fail {
		return errors.New("controlled VLogs failure")
	}
	return nil
}

func TestInboxCapacityNeverEvictsAcceptedWork(t *testing.T) {
	store, project := newInboxTestStore(t, "capacity")
	writer := &inboxTestWriter{started: make(chan struct{}, 1), release: make(chan struct{})}
	inbox, err := errortrack.NewInbox(store, writer, errortrack.InboxOptions{
		Capacity:     errortrack.InboxCapacity{MaxItems: 1, MaxBytes: 1 << 20},
		PollInterval: time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = inbox.Close(ctx)
	})

	if err := inbox.Accept(
		context.Background(),
		project,
		[]errortrack.AcceptedEvent{inboxEvent("accepted-first")},
	); err != nil {
		t.Fatal(err)
	}
	select {
	case <-writer.started:
	case <-time.After(time.Second):
		t.Fatal("accepted item was not claimed")
	}
	if err := inbox.Accept(
		context.Background(),
		project,
		[]errortrack.AcceptedEvent{inboxEvent("rejected-second")},
	); !errors.Is(err, errortrack.ErrCapacity) {
		t.Fatalf("second acceptance error=%v", err)
	}
	diagnostics, err := inbox.Diagnostics(context.Background())
	if err != nil || diagnostics.ActiveItems != 1 || diagnostics.RejectionsTotal != 1 {
		t.Fatalf("capacity diagnostics=%#v err=%v", diagnostics, err)
	}

	close(writer.release)
	waitInbox(t, inbox, func(d errortrack.InboxDiagnostics) bool { return d.ActiveItems == 0 })
	if err := inbox.Accept(
		context.Background(),
		project,
		[]errortrack.AcceptedEvent{inboxEvent("accepted-after-space")},
	); err != nil {
		t.Fatalf("accept after completion: %v", err)
	}
	waitInbox(t, inbox, func(d errortrack.InboxDiagnostics) bool { return d.ActiveItems == 0 })
	issues, err := store.ListIssues(context.Background(), project.ID, meta.IssueListOptions{})
	if err != nil || len(issues) != 1 || issues[0].Count != 2 {
		t.Fatalf("accepted issues=%#v err=%v", issues, err)
	}
}

func TestInboxClaimExpiresAndResumes(t *testing.T) {
	store, project := newInboxTestStore(t, "claim")
	now := time.Now().UTC().Truncate(time.Millisecond)
	event := inboxEvent("claim-event")
	payload, _ := json.Marshal(event.Event)
	if err := store.AcceptErrorEvents(context.Background(), []errortrack.InboxEntry{{
		ProjectID: project.ID, EventID: event.Event.EventID, CanonicalDigest: "claim-digest",
		Payload: payload, PayloadBytes: int64(len(payload)), OccurredAt: event.Event.Timestamp, AcceptedAt: now,
	}}, errortrack.InboxCapacity{MaxItems: 10, MaxBytes: 1 << 20}); err != nil {
		t.Fatal(err)
	}
	first, err := store.ClaimErrorEvents(context.Background(), "worker-a", 1, now, time.Second)
	if err != nil || len(first) != 1 || first[0].Attempts != 1 {
		t.Fatalf("first claim=%#v err=%v", first, err)
	}
	early, err := store.ClaimErrorEvents(
		context.Background(),
		"worker-b",
		1,
		now.Add(time.Second-time.Millisecond),
		time.Second,
	)
	if err != nil || len(early) != 0 {
		t.Fatalf("early claim=%#v err=%v", early, err)
	}
	recovered, err := store.ClaimErrorEvents(context.Background(), "worker-b", 1, now.Add(time.Second), time.Second)
	if err != nil || len(recovered) != 1 || recovered[0].ID != first[0].ID || recovered[0].Attempts != 2 {
		t.Fatalf("recovered claim=%#v err=%v", recovered, err)
	}
}

func TestInboxExhaustionClearsPayloadAndReportsDrop(t *testing.T) {
	store, project := newInboxTestStore(t, "exhaustion")
	writer := &inboxTestWriter{fail: true}
	inbox, err := errortrack.NewInbox(store, writer, errortrack.InboxOptions{
		Capacity:     errortrack.InboxCapacity{MaxItems: 10, MaxBytes: 1 << 20},
		MaxAttempts:  20,
		RetryInitial: time.Millisecond,
		RetryCap:     time.Millisecond,
		PollInterval: time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = inbox.Close(ctx)
	})
	if err := inbox.Accept(
		context.Background(),
		project,
		[]errortrack.AcceptedEvent{inboxEvent("terminal-event")},
	); err != nil {
		t.Fatal(err)
	}
	diagnostics := waitInbox(t, inbox, func(d errortrack.InboxDiagnostics) bool { return d.TerminalFailuresTotal == 1 })
	if diagnostics.ActiveItems != 0 || diagnostics.ActiveBytes != 0 || diagnostics.RetriesTotal != 19 ||
		diagnostics.DropsTotal != 1 || len(diagnostics.RecentTerminal) != 1 {
		t.Fatalf("terminal diagnostics=%#v", diagnostics)
	}
	terminal := diagnostics.RecentTerminal[0]
	if terminal.EventID != "terminal-event" || terminal.Attempts != 20 || terminal.PayloadBytes != 0 ||
		terminal.LastError == "" || terminal.CompletedAt == nil {
		t.Fatalf("terminal item=%#v", terminal)
	}
	if calls := writer.calls.Load(); calls != 20 {
		t.Fatalf("writer calls=%d", calls)
	}
}

func TestInboxMetricsUseOneLowCardinalitySeries(t *testing.T) {
	store, project := newInboxTestStore(t, "metrics")
	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })
	writer := &inboxTestWriter{started: make(chan struct{}, 1), release: make(chan struct{})}
	inbox, err := errortrack.NewInbox(store, writer, errortrack.InboxOptions{
		Capacity:      errortrack.InboxCapacity{MaxItems: 1, MaxBytes: 1 << 20},
		PollInterval:  time.Millisecond,
		MeterProvider: provider,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		close(writer.release)
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = inbox.Close(ctx)
	})
	if err := inbox.Accept(
		context.Background(),
		project,
		[]errortrack.AcceptedEvent{inboxEvent("metrics-first")},
	); err != nil {
		t.Fatal(err)
	}
	select {
	case <-writer.started:
	case <-time.After(time.Second):
		t.Fatal("metrics item was not claimed")
	}
	if err := inbox.Accept(
		context.Background(),
		project,
		[]errortrack.AcceptedEvent{inboxEvent("metrics-rejected")},
	); !errors.Is(err, errortrack.ErrCapacity) {
		t.Fatalf("metrics capacity error=%v", err)
	}
	var metrics metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &metrics); err != nil {
		t.Fatal(err)
	}
	values := make(map[string]int64)
	for _, scope := range metrics.ScopeMetrics {
		for _, measured := range scope.Metrics {
			switch data := measured.Data.(type) {
			case metricdata.Gauge[int64]:
				if len(data.DataPoints) != 1 || data.DataPoints[0].Attributes.Len() != 0 {
					t.Fatalf("metric %s data points=%#v", measured.Name, data.DataPoints)
				}
				values[measured.Name] = data.DataPoints[0].Value
			case metricdata.Sum[int64]:
				if len(data.DataPoints) != 1 || data.DataPoints[0].Attributes.Len() != 0 {
					t.Fatalf("metric %s data points=%#v", measured.Name, data.DataPoints)
				}
				values[measured.Name] = data.DataPoints[0].Value
			}
		}
	}
	for _, name := range []string{
		"lazy_observe.error_inbox.active_items",
		"lazy_observe.error_inbox.active_bytes",
		"lazy_observe.error_inbox.oldest_age_seconds",
		"lazy_observe.error_inbox.retries",
		"lazy_observe.error_inbox.rejections",
		"lazy_observe.error_inbox.terminal_failures",
		"lazy_observe.error_inbox.drops",
	} {
		if _, ok := values[name]; !ok {
			t.Fatalf("missing metric %q in %#v", name, values)
		}
	}
	if values["lazy_observe.error_inbox.active_items"] != 1 ||
		values["lazy_observe.error_inbox.active_bytes"] <= 0 ||
		values["lazy_observe.error_inbox.rejections"] != 1 {
		t.Fatalf("Inbox metric values=%#v", values)
	}
}

func newInboxTestStore(t *testing.T, slug string) (*meta.Store, errortrack.Project) {
	t.Helper()
	store, err := meta.OpenStore(context.Background(), filepath.Join(t.TempDir(), "meta.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	project, err := store.CreateProject(context.Background(), slug, slug)
	if err != nil {
		t.Fatal(err)
	}
	return store, errortrack.Project{ID: project.ID, Slug: project.Slug}
}

func inboxEvent(id string) errortrack.AcceptedEvent {
	event := errortrack.Event{
		EventID: id, Timestamp: time.Now().UTC(), Platform: "go", Level: "error",
		Exception: errortrack.Exception{
			Type: "InboxError", Value: "boom",
			Stacktrace: errortrack.Stacktrace{Frames: []errortrack.Frame{{
				Function: "main.handle", Filename: "main.go", InApp: true,
			}}},
		},
	}
	raw, _ := json.Marshal(event)
	return errortrack.AcceptedEvent{Raw: raw, Event: event}
}

func waitInbox(
	t *testing.T,
	inbox *errortrack.Inbox,
	ready func(errortrack.InboxDiagnostics) bool,
) errortrack.InboxDiagnostics {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		diagnostics, err := inbox.Diagnostics(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if ready(diagnostics) {
			return diagnostics
		}
		if time.Now().After(deadline) {
			t.Fatalf("Inbox condition timed out: %#v", diagnostics)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestInboxGeneratesUniqueOwnersAcrossInstances(t *testing.T) {
	store, _ := newInboxTestStore(t, "unique-owner")
	writer := &inboxTestWriter{}
	a, err := errortrack.NewInbox(store, writer, errortrack.InboxOptions{PollInterval: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = a.Close(context.Background()) }()
	b, err := errortrack.NewInbox(store, writer, errortrack.InboxOptions{PollInterval: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = b.Close(context.Background()) }()
	if a.Owner() == "" || b.Owner() == "" || a.Owner() == b.Owner() {
		t.Fatalf("expected distinct non-empty owners, got a=%q b=%q", a.Owner(), b.Owner())
	}
}

func TestInboxClampsOccurredAtClockSkew(t *testing.T) {
	store, project := newInboxTestStore(t, "clock-skew")
	fixedNow := time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC)
	writer := &inboxTestWriter{}
	inbox, err := errortrack.NewInbox(store, writer, errortrack.InboxOptions{
		Now:          func() time.Time { return fixedNow },
		PollInterval: time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = inbox.Close(context.Background()) }()

	mkEvent := func(id, typ string, ts time.Time) errortrack.AcceptedEvent {
		e := errortrack.Event{
			EventID: id, Timestamp: ts, Platform: "go", Level: "error",
			Exception: errortrack.Exception{
				Type:  typ,
				Value: "skew",
				Stacktrace: errortrack.Stacktrace{
					Frames: []errortrack.Frame{{Function: typ, Filename: "a.go", InApp: true}},
				},
			},
		}
		raw, _ := json.Marshal(e)
		return errortrack.AcceptedEvent{Raw: raw, Event: e}
	}
	farFuture := fixedNow.Add(10 * 365 * 24 * time.Hour)
	farPast := time.Date(1970, 1, 1, 0, 0, 0, 0, time.UTC)
	validPast := fixedNow.Add(-2 * time.Minute)
	if err := inbox.Accept(context.Background(), project, []errortrack.AcceptedEvent{
		mkEvent("future", "FutureErr", farFuture),
		mkEvent("past", "PastErr", farPast),
		mkEvent("normal", "NormalErr", validPast),
	}); err != nil {
		t.Fatal(err)
	}
	waitInbox(t, inbox, func(d errortrack.InboxDiagnostics) bool { return d.ActiveItems == 0 })
	issues, err := store.ListIssues(context.Background(), project.ID, meta.IssueListOptions{})
	if err != nil || len(issues) != 3 {
		t.Fatalf("issues=%#v err=%v", issues, err)
	}
	byEvent := make(map[string]meta.IssueView)
	for _, iss := range issues {
		byEvent[iss.LastEventID] = iss
	}
	if !byEvent["future"].LastSeen.Equal(fixedNow) {
		t.Fatalf("far future occurred_at was not clamped: got %s want %s", byEvent["future"].LastSeen, fixedNow)
	}
	if !byEvent["past"].LastSeen.Equal(fixedNow) {
		t.Fatalf("far past occurred_at was not clamped: got %s want %s", byEvent["past"].LastSeen, fixedNow)
	}
	if !byEvent["normal"].LastSeen.Equal(validPast) {
		t.Fatalf("valid occurred_at was unexpectedly modified: got %s want %s", byEvent["normal"].LastSeen, validPast)
	}
}

type panicSymbolicator struct {
	calls atomic.Int64
}

func (p *panicSymbolicator) Symbolicate(
	_ context.Context,
	_ string,
	_ json.RawMessage,
	event errortrack.Event,
) (errortrack.Event, error) {
	p.calls.Add(1)
	panic("simulated go-sourcemap panic")
}

func TestInboxWorkerRecoversFromPanicAndMarksTerminal(t *testing.T) {
	store, project := newInboxTestStore(t, "panic-recover")
	var logBuf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logBuf, nil))
	sym := &panicSymbolicator{}
	inbox, err := errortrack.NewInbox(store, &inboxTestWriter{}, errortrack.InboxOptions{
		Logger:       logger,
		Symbolicator: sym,
		MaxAttempts:  2,
		RetryInitial: time.Millisecond,
		RetryCap:     time.Millisecond,
		PollInterval: time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = inbox.Close(context.Background()) }()

	if err := inbox.Accept(
		context.Background(),
		project,
		[]errortrack.AcceptedEvent{inboxEvent("poison")},
	); err != nil {
		t.Fatal(err)
	}
	diagnostics := waitInbox(t, inbox, func(d errortrack.InboxDiagnostics) bool { return d.TerminalFailuresTotal == 1 })
	if diagnostics.ActiveItems != 0 || diagnostics.DropsTotal != 1 || sym.calls.Load() != 2 {
		t.Fatalf("diagnostics=%#v calls=%d", diagnostics, sym.calls.Load())
	}
	if !strings.Contains(logBuf.String(), "recovered panic") {
		t.Fatalf("expected structured panic recovery log, got: %s", logBuf.String())
	}
}

type flakySymbolicator struct {
	failOnce atomic.Bool
	calls    atomic.Int64
}

func (f *flakySymbolicator) Symbolicate(
	_ context.Context,
	_ string,
	_ json.RawMessage,
	event errortrack.Event,
) (errortrack.Event, error) {
	f.calls.Add(1)
	if f.failOnce.CompareAndSwap(true, false) {
		return event, errors.New("transient s3 timeout")
	}
	event.Exception.Stacktrace.Frames[0].Symbolicated = true
	return event, nil
}

func TestInboxRetriesTransientSymbolicationError(t *testing.T) {
	store, project := newInboxTestStore(t, "sym-retry")
	sym := &flakySymbolicator{}
	sym.failOnce.Store(true)
	writer := &inboxTestWriter{}
	inbox, err := errortrack.NewInbox(store, writer, errortrack.InboxOptions{
		Symbolicator: sym,
		RetryInitial: time.Millisecond,
		RetryCap:     time.Millisecond,
		PollInterval: time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = inbox.Close(context.Background()) }()

	if err := inbox.Accept(
		context.Background(),
		project,
		[]errortrack.AcceptedEvent{inboxEvent("sym-ev")},
	); err != nil {
		t.Fatal(err)
	}
	diagnostics := waitInbox(t, inbox, func(d errortrack.InboxDiagnostics) bool { return d.ActiveItems == 0 })
	if diagnostics.RetriesTotal != 1 || sym.calls.Load() != 2 || writer.calls.Load() != 1 {
		t.Fatalf("diagnostics=%#v symCalls=%d writerCalls=%d", diagnostics, sym.calls.Load(), writer.calls.Load())
	}
}

type blockingSymbolicator struct {
	firstStarted chan struct{}
	releaseFirst chan struct{}
	calls        atomic.Int64
}

func (b *blockingSymbolicator) Symbolicate(
	ctx context.Context,
	_ string,
	_ json.RawMessage,
	event errortrack.Event,
) (errortrack.Event, error) {
	n := b.calls.Add(1)
	if n == 1 {
		close(b.firstStarted)
		select {
		case <-b.releaseFirst:
		case <-ctx.Done():
			return event, ctx.Err()
		}
	}
	return event, nil
}

func TestInboxCloseStopsRemainingClaimedBatchAndReleasesItems(t *testing.T) {
	store, project := newInboxTestStore(t, "close-release")
	sym := &blockingSymbolicator{firstStarted: make(chan struct{}), releaseFirst: make(chan struct{})}
	inbox, err := errortrack.NewInbox(store, &inboxTestWriter{}, errortrack.InboxOptions{
		Symbolicator: sym,
		ClaimBatch:   4,
		ClaimTTL:     time.Minute,
		PollInterval: time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := inbox.Accept(context.Background(), project, []errortrack.AcceptedEvent{
		inboxEvent("shut-1"), inboxEvent("shut-2"), inboxEvent("shut-3"),
	}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-sym.firstStarted:
	case <-time.After(time.Second):
		t.Fatal("first item did not start")
	}
	closeCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := inbox.Close(closeCtx); err != nil {
		t.Fatalf("Close error: %v", err)
	}
	if calls := sym.calls.Load(); calls != 1 {
		t.Fatalf("symbolicator ran %d times after Close, want 1", calls)
	}
	// All 3 items must be released back to pending so a new worker can claim
	// them immediately without waiting for ClaimTTL.
	claimed, err := store.ClaimErrorEvents(context.Background(), "next-worker", 10, time.Now().UTC(), time.Minute)
	if err != nil || len(claimed) != 3 {
		t.Fatalf("expected 3 immediately claimable items after shutdown release, got %d err=%v", len(claimed), err)
	}
}

func TestInboxLogsWarningOnClaimLost(t *testing.T) {
	store, project := newInboxTestStore(t, "claim-lost-log")
	var logBuf bytes.Buffer
	var logMu sync.Mutex
	logger := slog.New(slog.NewTextHandler(&lockedWriter{mu: &logMu, buf: &logBuf}, nil))
	writer := &inboxTestWriter{started: make(chan struct{}, 1), release: make(chan struct{})}
	inbox, err := errortrack.NewInbox(store, writer, errortrack.InboxOptions{
		Owner:        "replica-a",
		Logger:       logger,
		ClaimTTL:     50 * time.Millisecond,
		PollInterval: 5 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = inbox.Close(context.Background()) }()

	if err := inbox.Accept(
		context.Background(),
		project,
		[]errortrack.AcceptedEvent{inboxEvent("fenced-ev")},
	); err != nil {
		t.Fatal(err)
	}
	select {
	case <-writer.started:
	case <-time.After(time.Second):
		t.Fatal("replica-a did not claim event")
	}
	// Replica B steals the expired lease while replica-a is blocked in writer.
	recovered, err := store.ClaimErrorEvents(
		context.Background(),
		"replica-b",
		1,
		time.Now().UTC().Add(time.Second),
		time.Minute,
	)
	if err != nil || len(recovered) != 1 || recovered[0].ClaimEpoch != 2 {
		t.Fatalf("replica-b steal=%#v err=%v", recovered, err)
	}
	close(writer.release)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		logMu.Lock()
		logged := logBuf.String()
		logMu.Unlock()
		if strings.Contains(logged, "losing claim lease") &&
			strings.Contains(logged, "owner=replica-a") &&
			strings.Contains(logged, "claim_epoch=1") {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	logMu.Lock()
	defer logMu.Unlock()
	t.Fatalf("expected slog.Warn with owner and claim_epoch on ErrClaimLost, got: %s", logBuf.String())
}

type lockedWriter struct {
	mu  *sync.Mutex
	buf *bytes.Buffer
}

func (w *lockedWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.Write(p)
}

type summaryOnlySpyStore struct {
	errortrack.InboxStore
	summaryCalls     atomic.Int64
	diagnosticsCalls atomic.Int64
}

func (s *summaryOnlySpyStore) ErrorInboxSummary(ctx context.Context) (errortrack.InboxSummary, error) {
	s.summaryCalls.Add(1)
	return s.InboxStore.ErrorInboxSummary(ctx)
}

func (s *summaryOnlySpyStore) ErrorInboxDiagnostics(
	ctx context.Context,
	limit int,
) (errortrack.InboxDiagnostics, error) {
	s.diagnosticsCalls.Add(1)
	return s.InboxStore.ErrorInboxDiagnostics(ctx, limit)
}

func TestInboxReadinessUsesO1SummaryOnly(t *testing.T) {
	store, _ := newInboxTestStore(t, "o1-ready")
	spy := &summaryOnlySpyStore{InboxStore: store}
	inbox, err := errortrack.NewInbox(spy, &inboxTestWriter{}, errortrack.InboxOptions{PollInterval: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = inbox.Close(context.Background()) }()
	if err := inbox.Readiness(context.Background()); err != nil {
		t.Fatalf("Readiness error: %v", err)
	}
	if spy.summaryCalls.Load() != 1 || spy.diagnosticsCalls.Load() != 0 {
		t.Fatalf("Readiness summaryCalls=%d diagnosticsCalls=%d, want 1 and 0",
			spy.summaryCalls.Load(), spy.diagnosticsCalls.Load())
	}
}

func TestInboxPersistsAndRedeliversIssueLifecycleAcrossRestarts(t *testing.T) {
	store, project := newInboxTestStore(t, "durable-lifecycle")
	// First worker completes the Inbox item without an OnChange callback
	// (simulating a crash right after CompleteErrorEvent commits).
	first, err := errortrack.NewInbox(store, &inboxTestWriter{}, errortrack.InboxOptions{
		PollInterval: time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Accept(
		context.Background(),
		project,
		[]errortrack.AcceptedEvent{inboxEvent("life-1")},
	); err != nil {
		t.Fatal(err)
	}
	waitInbox(t, first, func(d errortrack.InboxDiagnostics) bool { return d.ActiveItems == 0 })
	if err := first.Close(context.Background()); err != nil {
		t.Fatal(err)
	}

	// Second worker starts with a flaky OnChange that fails once, then succeeds.
	var delivered []errortrack.IssueChange
	var mu sync.Mutex
	var attempts atomic.Int64
	second, err := errortrack.NewInbox(store, &inboxTestWriter{}, errortrack.InboxOptions{
		RetryInitial: time.Millisecond,
		RetryCap:     time.Millisecond,
		PollInterval: time.Millisecond,
		OnChange: func(_ context.Context, change errortrack.IssueChange) error {
			if attempts.Add(1) == 1 {
				return errors.New("temporary downstream failure")
			}
			mu.Lock()
			delivered = append(delivered, change)
			mu.Unlock()
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = second.Close(context.Background()) }()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := len(delivered)
		mu.Unlock()
		if n == 1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(delivered) != 1 || delivered[0].Kind != "new_issue" || delivered[0].ProjectID != project.ID {
		t.Fatalf("delivered lifecycle changes=%#v attempts=%d", delivered, attempts.Load())
	}
}

type batchLifecycleSpyStore struct {
	*meta.Store
	clustered           bool
	completeBatchCalls  atomic.Int64
	completeSingleCalls atomic.Int64
	claimLifecycleCalls atomic.Int64
	claimLifecycleCh    chan struct{}
}

func (s *batchLifecycleSpyStore) IsClustered() bool {
	return s.clustered
}

func (s *batchLifecycleSpyStore) CompleteErrorEvent(
	ctx context.Context,
	id int64,
	owner string,
	claimEpoch int64,
	delta errortrack.IssueDelta,
	processedAt time.Time,
) ([]errortrack.IssueChange, error) {
	s.completeSingleCalls.Add(1)
	return s.Store.CompleteErrorEvent(ctx, id, owner, claimEpoch, delta, processedAt)
}

func (s *batchLifecycleSpyStore) CompleteErrorEvents(
	ctx context.Context,
	owner string,
	completions []errortrack.ErrorCompletion,
) (errortrack.BatchCompletionResult, error) {
	s.completeBatchCalls.Add(1)
	return s.Store.CompleteErrorEvents(ctx, owner, completions)
}

func (s *batchLifecycleSpyStore) ClaimIssueLifecycleEvents(
	ctx context.Context,
	owner string,
	limit int,
	now time.Time,
	ttl time.Duration,
) ([]errortrack.LifecycleEvent, error) {
	s.claimLifecycleCalls.Add(1)
	events, err := s.Store.ClaimIssueLifecycleEvents(ctx, owner, limit, now, ttl)
	if s.claimLifecycleCh != nil {
		select {
		case s.claimLifecycleCh <- struct{}{}:
		default:
		}
	}
	return events, err
}

func TestInboxBatchWritesOnceAndDispatchesLifecycleOncePerBatch(t *testing.T) {
	baseStore, project := newInboxTestStore(t, "batch-lifecycle")
	spy := &batchLifecycleSpyStore{Store: baseStore, claimLifecycleCh: make(chan struct{}, 4)}

	var vlogsPosts atomic.Int64
	var vlogsLines atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		vlogsPosts.Add(1)
		vlogsLines.Add(int64(bytes.Count(bytes.TrimSpace(body), []byte("\n")) + 1))
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	var changeMu sync.Mutex
	var changes []errortrack.IssueChange
	writer := errortrack.NewVictoriaLogsWriter(&testLogsBackend{baseURL: srv.URL}, srv.Client())
	inbox, err := errortrack.NewInbox(spy, writer, errortrack.InboxOptions{
		PollInterval: time.Hour,
		OnChange: func(_ context.Context, change errortrack.IssueChange) error {
			changeMu.Lock()
			changes = append(changes, change)
			changeMu.Unlock()
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = inbox.Close(context.Background()) }()

	select {
	case <-spy.claimLifecycleCh:
	case <-time.After(2 * time.Second):
		t.Fatal("initial startup lifecycle poll did not complete")
	}

	firstBatch := make([]errortrack.AcceptedEvent, 10)
	for i := range firstBatch {
		firstBatch[i] = inboxEvent(fmt.Sprintf("batch1-%d", i))
	}
	if err := inbox.Accept(context.Background(), project, firstBatch); err != nil {
		t.Fatal(err)
	}
	waitInbox(t, inbox, func(d errortrack.InboxDiagnostics) bool { return d.ActiveItems == 0 })
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		changeMu.Lock()
		n := len(changes)
		changeMu.Unlock()
		if n == 1 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	changeMu.Lock()
	gotChanges := len(changes)
	changeMu.Unlock()
	if gotChanges != 1 {
		t.Fatalf("delivered changes=%d want 1", gotChanges)
	}
	if vlogsPosts.Load() != 1 || vlogsLines.Load() != 10 {
		t.Fatalf("after batch 1: vlogsPosts=%d vlogsLines=%d want 1 and 10", vlogsPosts.Load(), vlogsLines.Load())
	}
	if spy.completeBatchCalls.Load() != 1 || spy.completeSingleCalls.Load() != 0 {
		t.Fatalf("completeBatch=%d completeSingle=%d", spy.completeBatchCalls.Load(), spy.completeSingleCalls.Load())
	}
	lifecycleCallsAfterFirst := spy.claimLifecycleCalls.Load()

	// Second batch on the same open issue produces 0 lifecycle changes -> must
	// not call ClaimIssueLifecycleEvents at batch end.
	secondBatch := make([]errortrack.AcceptedEvent, 10)
	for i := range secondBatch {
		secondBatch[i] = inboxEvent(fmt.Sprintf("batch2-%d", i))
	}
	if err := inbox.Accept(context.Background(), project, secondBatch); err != nil {
		t.Fatal(err)
	}
	waitInbox(t, inbox, func(d errortrack.InboxDiagnostics) bool { return d.ActiveItems == 0 })
	if vlogsPosts.Load() != 2 || vlogsLines.Load() != 20 {
		t.Fatalf("after batch 2: vlogsPosts=%d vlogsLines=%d want 2 and 20", vlogsPosts.Load(), vlogsLines.Load())
	}
	if spy.claimLifecycleCalls.Load() != lifecycleCallsAfterFirst {
		t.Fatalf(
			"expected no extra ClaimIssueLifecycleEvents when batch has no changes: before=%d after=%d",
			lifecycleCallsAfterFirst,
			spy.claimLifecycleCalls.Load(),
		)
	}
	issues, err := baseStore.ListIssues(context.Background(), project.ID, meta.IssueListOptions{})
	if err != nil || len(issues) != 1 || issues[0].Count != 20 {
		t.Fatalf("issues=%#v err=%v", issues, err)
	}
}

func TestInboxBatch4xxFallbackCompletesGoodAndRetriesBad(t *testing.T) {
	store, project := newInboxTestStore(t, "inbox-4xx-fallback")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if bytes.Contains(body, []byte(`"bad-4xx"`)) {
			http.Error(w, "bad payload", http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	writer := errortrack.NewVictoriaLogsWriter(&testLogsBackend{baseURL: srv.URL}, srv.Client())
	inbox, err := errortrack.NewInbox(store, writer, errortrack.InboxOptions{
		MaxAttempts:  2,
		RetryInitial: time.Millisecond,
		RetryCap:     time.Millisecond,
		PollInterval: time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = inbox.Close(context.Background()) }()

	if err := inbox.Accept(context.Background(), project, []errortrack.AcceptedEvent{
		inboxEvent("good-1"),
		inboxEvent("bad-4xx"),
		inboxEvent("good-2"),
		inboxEvent("good-3"),
	}); err != nil {
		t.Fatal(err)
	}
	diag := waitInbox(t, inbox, func(d errortrack.InboxDiagnostics) bool {
		return d.ActiveItems == 0 && d.TerminalFailuresTotal == 1
	})
	if len(diag.RecentTerminal) != 1 || diag.RecentTerminal[0].EventID != "bad-4xx" {
		t.Fatalf("terminal diagnostics=%#v", diag.RecentTerminal)
	}
	issues, err := store.ListIssues(context.Background(), project.ID, meta.IssueListOptions{})
	if err != nil || len(issues) != 1 || issues[0].Count != 3 {
		t.Fatalf("expected 3 good events in issue, got issues=%#v err=%v", issues, err)
	}
}

func TestInboxBatch5xxRetriesPerEvent(t *testing.T) {
	store, project := newInboxTestStore(t, "inbox-5xx-retry")
	var attempts atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if attempts.Add(1) == 1 {
			http.Error(w, "vlogs down", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	writer := errortrack.NewVictoriaLogsWriter(&testLogsBackend{baseURL: srv.URL}, srv.Client())
	inbox, err := errortrack.NewInbox(store, writer, errortrack.InboxOptions{
		MaxAttempts:  5,
		RetryInitial: time.Millisecond,
		RetryCap:     time.Millisecond,
		PollInterval: time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = inbox.Close(context.Background()) }()

	if err := inbox.Accept(context.Background(), project, []errortrack.AcceptedEvent{
		inboxEvent("retry-1"), inboxEvent("retry-2"), inboxEvent("retry-3"), inboxEvent("retry-4"),
	}); err != nil {
		t.Fatal(err)
	}
	diag := waitInbox(t, inbox, func(d errortrack.InboxDiagnostics) bool { return d.ActiveItems == 0 })
	if diag.RetriesTotal != 4 || attempts.Load() < 2 {
		t.Fatalf("diag=%#v attempts=%d", diag, attempts.Load())
	}
	issues, err := store.ListIssues(context.Background(), project.ID, meta.IssueListOptions{})
	if err != nil || len(issues) != 1 || issues[0].Count != 4 {
		t.Fatalf("issues=%#v err=%v", issues, err)
	}
}

type blockingBatchWriter struct {
	started chan struct{}
	release chan struct{}
	calls   atomic.Int64
}

func (w *blockingBatchWriter) WriteErrorEvent(
	_ context.Context,
	_ errortrack.Project,
	_ json.RawMessage,
	_ errortrack.Event,
) error {
	return nil
}

func (w *blockingBatchWriter) WriteErrorEvents(ctx context.Context, events []errortrack.PreparedEvent) []error {
	w.calls.Add(1)
	if w.started != nil {
		select {
		case w.started <- struct{}{}:
		default:
		}
	}
	if w.release != nil {
		select {
		case <-w.release:
		case <-ctx.Done():
			errs := make([]error, len(events))
			for i := range errs {
				errs[i] = ctx.Err()
			}
			return errs
		}
	}
	return make([]error, len(events))
}

func TestInboxBatchPartialClaimLostLogsWarnAndCompletesValid(t *testing.T) {
	store, project := newInboxTestStore(t, "batch-partial-claim-lost")
	var logBuf bytes.Buffer
	var logMu sync.Mutex
	logger := slog.New(slog.NewTextHandler(&lockedWriter{mu: &logMu, buf: &logBuf}, nil))
	fixedNow := time.Date(2026, 11, 23, 2, 0, 0, 0, time.UTC)
	writer := &blockingBatchWriter{started: make(chan struct{}, 1), release: make(chan struct{})}
	var onEventCalls atomic.Int64

	inbox, err := errortrack.NewInbox(store, writer, errortrack.InboxOptions{
		Owner:        "replica-a",
		Logger:       logger,
		Now:          func() time.Time { return fixedNow },
		ClaimTTL:     time.Minute,
		PollInterval: time.Hour,
		OnEvent: func(context.Context, errortrack.ErrorOccurrence) error {
			onEventCalls.Add(1)
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = inbox.Close(context.Background()) }()

	if err := inbox.Accept(context.Background(), project, []errortrack.AcceptedEvent{
		inboxEvent("partial-1"),
		inboxEvent("partial-2"),
		inboxEvent("partial-3"),
	}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-writer.started:
	case <-time.After(time.Second):
		t.Fatal("batch writer was not invoked")
	}

	// Replica B steals ONLY the first item while replica-a is blocked in writer.
	stolen, err := store.ClaimErrorEvents(
		context.Background(),
		"replica-b",
		1,
		fixedNow.Add(2*time.Minute),
		time.Minute,
	)
	if err != nil || len(stolen) != 1 || stolen[0].EventID != "partial-1" {
		t.Fatalf("stolen=%#v err=%v", stolen, err)
	}
	close(writer.release)

	// Items 2 and 3 must commit while item 1 remains owned by replica-b.
	waitInbox(t, inbox, func(d errortrack.InboxDiagnostics) bool {
		return d.ActiveItems == 1 && onEventCalls.Load() >= 2
	})
	if err := inbox.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := onEventCalls.Load(); got != 2 {
		t.Fatalf("onEventCalls=%d want 2 (must skip lost item)", got)
	}
	issues, err := store.ListIssues(context.Background(), project.ID, meta.IssueListOptions{})
	if err != nil || len(issues) != 1 || issues[0].Count != 2 {
		t.Fatalf("issues=%#v err=%v", issues, err)
	}
	logMu.Lock()
	logged := logBuf.String()
	logMu.Unlock()
	if !strings.Contains(logged, "losing claim lease") ||
		!strings.Contains(logged, "event_id=partial-1") ||
		!strings.Contains(logged, "owner=replica-a") ||
		!strings.Contains(logged, "claim_epoch=1") {
		t.Fatalf("expected slog.Warn with event_id, owner, and claim_epoch, got: %s", logged)
	}
}

type clockAdvancingSymbolicator struct {
	now   *atomic.Int64
	step  time.Duration
	calls atomic.Int64
}

func (s *clockAdvancingSymbolicator) Symbolicate(
	_ context.Context,
	_ string,
	_ json.RawMessage,
	event errortrack.Event,
) (errortrack.Event, error) {
	if s.calls.Add(1) == 1 {
		s.now.Add(int64(s.step))
	}
	return event, nil
}

func TestInboxSkipsSymbolicationWhenRemainingLeaseIsLow(t *testing.T) {
	store, project := newInboxTestStore(t, "low-lease-symbolicate")
	var logBuf bytes.Buffer
	var logMu sync.Mutex
	logger := slog.New(slog.NewTextHandler(&lockedWriter{mu: &logMu, buf: &logBuf}, nil))

	start := time.Date(2026, 11, 23, 3, 0, 0, 0, time.UTC)
	seedEntries := make([]errortrack.InboxEntry, 0, 2)
	for _, id := range []string{"lease-1", "lease-2"} {
		ev := inboxEvent(id)
		ev.Event.Timestamp = start
		payload, err := json.Marshal(ev.Event)
		if err != nil {
			t.Fatal(err)
		}
		seedEntries = append(seedEntries, errortrack.InboxEntry{
			ProjectID:       project.ID,
			EventID:         ev.Event.EventID,
			CanonicalDigest: "digest-" + id,
			Payload:         payload,
			PayloadBytes:    int64(len(payload)),
			OccurredAt:      start,
			AcceptedAt:      start,
		})
	}
	if err := store.AcceptErrorEvents(
		context.Background(),
		seedEntries,
		errortrack.InboxCapacity{MaxItems: 10, MaxBytes: 1 << 20},
	); err != nil {
		t.Fatal(err)
	}

	var nowNanos atomic.Int64
	nowNanos.Store(start.UnixNano())
	sym := &clockAdvancingSymbolicator{now: &nowNanos, step: 58 * time.Second}

	inbox, err := errortrack.NewInbox(store, &inboxTestWriter{}, errortrack.InboxOptions{
		Logger:       logger,
		Now:          func() time.Time { return time.Unix(0, nowNanos.Load()).UTC() },
		Symbolicator: sym,
		ClaimTTL:     time.Minute,
		PollInterval: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = inbox.Close(context.Background()) }()

	waitInbox(t, inbox, func(d errortrack.InboxDiagnostics) bool {
		return d.ActiveItems == 1 && d.PendingItems == 1
	})
	if err := inbox.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if sym.calls.Load() != 1 {
		t.Fatalf("symbolicator calls=%d want 1 (item 2 skipped due to low remaining lease)", sym.calls.Load())
	}
	logMu.Lock()
	logged := logBuf.String()
	logMu.Unlock()
	if !strings.Contains(logged, "insufficient remaining lease") {
		t.Fatalf("expected low-lease warning log, got: %s", logged)
	}
	// Item 2 was released back to pending and can be claimed cleanly now.
	claimed, err := store.ClaimErrorEvents(
		context.Background(),
		"next-worker",
		10,
		start.Add(58*time.Second),
		time.Minute,
	)
	if err != nil || len(claimed) != 1 || claimed[0].EventID != "lease-2" || claimed[0].Attempts != 1 {
		t.Fatalf("claimed=%#v err=%v", claimed, err)
	}
}

func TestInboxWorkersClampedOnSQLiteAndScaledWhenClustered(t *testing.T) {
	store, _ := newInboxTestStore(t, "workers-mode")
	var logBuf bytes.Buffer
	var logMu sync.Mutex
	logger := slog.New(slog.NewTextHandler(&lockedWriter{mu: &logMu, buf: &logBuf}, nil))

	sqliteInbox, err := errortrack.NewInbox(store, &inboxTestWriter{}, errortrack.InboxOptions{
		Logger:       logger,
		Workers:      4,
		PollInterval: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := sqliteInbox.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if sqliteInbox.Workers() != 1 {
		t.Fatalf("SQLite Inbox.Workers()=%d want 1", sqliteInbox.Workers())
	}
	logMu.Lock()
	logged := logBuf.String()
	logMu.Unlock()
	if !strings.Contains(logged, "forcing error Inbox worker count to 1") {
		t.Fatalf("expected SQLite worker clamp warning, got: %s", logged)
	}

	clustered := &batchLifecycleSpyStore{Store: store, clustered: true}
	for _, tc := range []struct {
		requested int
		want      int
	}{
		{requested: 0, want: errortrack.DefaultClusteredWorkers},
		{requested: 3, want: 3},
		{requested: 10, want: errortrack.MaxClusteredWorkers},
	} {
		inbox, err := errortrack.NewInbox(clustered, &inboxTestWriter{}, errortrack.InboxOptions{
			Workers:      tc.requested,
			PollInterval: time.Hour,
		})
		if err != nil {
			t.Fatal(err)
		}
		got := inbox.Workers()
		if err := inbox.Close(context.Background()); err != nil {
			t.Fatal(err)
		}
		if got != tc.want {
			t.Fatalf("clustered Workers(requested=%d)=%d want %d", tc.requested, got, tc.want)
		}
	}
}

func BenchmarkInbox(b *testing.B) {
	store, err := meta.OpenStore(context.Background(), filepath.Join(b.TempDir(), "bench.db"))
	if err != nil {
		b.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	projectRow, err := store.CreateProject(context.Background(), "Bench", "bench")
	if err != nil {
		b.Fatal(err)
	}
	project := errortrack.Project{ID: projectRow.ID, Slug: projectRow.Slug}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	const batchSize = 16
	doneCh := make(chan struct{}, batchSize*4)
	writer := errortrack.NewVictoriaLogsWriter(&testLogsBackend{baseURL: srv.URL}, srv.Client())
	inbox, err := errortrack.NewInbox(store, writer, errortrack.InboxOptions{
		PollInterval: 10 * time.Millisecond,
		OnEvent: func(context.Context, errortrack.ErrorOccurrence) error {
			doneCh <- struct{}{}
			return nil
		},
	})
	if err != nil {
		b.Fatal(err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = inbox.Close(ctx)
	}()

	ts := time.Date(2026, 11, 23, 0, 0, 0, 0, time.UTC)
	baseEvents := make([]errortrack.AcceptedEvent, batchSize)
	for j := range baseEvents {
		ev := errortrack.Event{
			EventID:   fmt.Sprintf("seed-%d", j),
			Timestamp: ts,
			Platform:  "go",
			Level:     "error",
			Exception: errortrack.Exception{
				Type:  "BenchError",
				Value: "boom",
				Stacktrace: errortrack.Stacktrace{Frames: []errortrack.Frame{
					{Function: "main.bench", Filename: "bench.go", InApp: true},
				}},
			},
		}
		raw, _ := json.Marshal(ev)
		baseEvents[j] = errortrack.AcceptedEvent{Raw: raw, Event: ev}
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := range b.N {
		batch := make([]errortrack.AcceptedEvent, batchSize)
		for j := range batchSize {
			batch[j] = baseEvents[j]
			batch[j].Event.EventID = fmt.Sprintf("bench-%d-%d", i, j)
		}
		if err := inbox.Accept(context.Background(), project, batch); err != nil {
			b.Fatal(err)
		}
		for range batchSize {
			<-doneCh
		}
	}
	b.StopTimer()
}
