package errortrack_test

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
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
