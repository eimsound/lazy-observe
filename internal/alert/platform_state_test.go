package alert

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/eimsound/lazy-observe/internal/meta"
	"github.com/eimsound/lazy-observe/internal/outbound"
)

type spikeOutboxRow struct {
	ID           int64
	Key          string
	Priority     int
	Notification Notification
}

type spikeHarness struct {
	t        *testing.T
	store    *meta.Store
	project  *meta.Project
	channel  int
	received chan Notification
	clock    *atomic.Int64
	base     time.Time
	client   *http.Client
}

func newSpikeHarness(t *testing.T, slug string) *spikeHarness {
	t.Helper()
	ctx := context.Background()
	store := openOutboxTestStore(t)
	project, err := store.CreateProject(ctx, slug, slug)
	if err != nil {
		t.Fatal(err)
	}
	received := make(chan Notification, 32)
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var n Notification
		if decodeErr := json.NewDecoder(r.Body).Decode(&n); decodeErr != nil {
			http.Error(w, decodeErr.Error(), http.StatusBadRequest)
			return
		}
		received <- n
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(endpoint.Close)
	channel, err := store.CreateNotificationChannel(ctx, meta.NotificationChannelCreate{
		Type: "webhook", Name: slug + "-hook", Config: map[string]any{"url": endpoint.URL},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.PutProjectAlertConfig(ctx, project.ID, meta.ProjectAlertConfigUpdate{
		ErrorRateThresholdPerMinute: 1,
		ChannelIDs:                  []int{channel.ID},
	}); err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, 8, 1, 14, 0, 0, 0, time.UTC)
	clock := &atomic.Int64{}
	clock.Store(base.UnixNano())
	return &spikeHarness{
		t: t, store: store, project: project,
		channel: channel.ID, received: received, clock: clock, base: base, client: endpoint.Client(),
	}
}

func (h *spikeHarness) now() time.Time { return time.Unix(0, h.clock.Load()).UTC() }

func (h *spikeHarness) set(ts time.Time) { h.clock.Store(ts.UnixNano()) }

// service starts one replica. GroupWait is deliberately an hour: the removed
// in-process Engine would have held the firing notification that long.
func (h *spikeHarness) service(workerID string) *Service {
	h.t.Helper()
	svc := NewService(Config{
		Store:          h.store,
		OutboundPolicy: outbound.Policy{AllowPrivate: true},
		Sender:         &Sender{Client: h.client, Sleep: func(time.Duration) {}},
		GroupWait:      time.Hour,
		RepeatInterval: time.Hour,
		Now:            h.now,
		Outbox:         OutboxWorkerConfig{WorkerID: workerID, PollInterval: 5 * time.Millisecond},
	})
	h.t.Cleanup(svc.Close)
	return svc
}

// spike records six errors one second apart starting at start (> 1/min * 5m).
func (h *spikeHarness) spike(svc *Service, start time.Time) {
	h.t.Helper()
	for i := range 6 {
		ts := start.Add(time.Duration(i) * time.Second)
		h.set(ts)
		if err := svc.RecordErrorEvent(context.Background(), h.project.ID, ts); err != nil {
			h.t.Fatal(err)
		}
	}
}

func (h *spikeHarness) rows() []spikeOutboxRow {
	h.t.Helper()
	ctx := context.Background()
	views, err := h.store.ListNotificationOutbox(ctx, meta.NotificationOutboxListOptions{ProjectID: h.project.ID})
	if err != nil {
		h.t.Fatal(err)
	}
	rows := make([]spikeOutboxRow, 0, len(views))
	for _, view := range views {
		item, itemErr := h.store.NotificationOutboxItem(ctx, view.ID)
		if itemErr != nil {
			h.t.Fatal(itemErr)
		}
		var n Notification
		if err := json.Unmarshal(item.Payload, &n); err != nil {
			h.t.Fatal(err)
		}
		rows = append(
			rows,
			spikeOutboxRow{ID: item.ID, Key: item.IdempotencyKey, Priority: item.Priority, Notification: n},
		)
	}
	slices.SortFunc(rows, func(a, b spikeOutboxRow) int { return int(a.ID - b.ID) })
	return rows
}

func (h *spikeHarness) expectRows(want ...PlatformStateRef) []spikeOutboxRow {
	h.t.Helper()
	rows := h.rows()
	if len(rows) != len(want) {
		h.t.Fatalf("outbox rows=%d want=%d: %#v", len(rows), len(want), rows)
	}
	for i, ref := range want {
		ref.ProjectID = h.project.ID
		ref.Kind = "error_rate_spike"
		entries := BuildPlatformStateOutboxEntries(ref, []int{h.channel}, []Notification{{Status: ref.Status}})
		row := rows[i]
		if row.Key != entries[0].IdempotencyKey {
			h.t.Fatalf("row %d key=%q want key for %+v", i, row.Key, ref)
		}
		n := row.Notification
		if n.Rule != "error_rate_spike" || n.Status != ref.Status || len(n.Alerts) != 1 ||
			n.Alerts[0].Labels["project_id"] != h.project.Slug ||
			n.Alerts[0].Annotations["cycle"] != strconv.FormatInt(ref.Cycle, 10) {
			h.t.Fatalf("row %d notification=%#v want %+v", i, n, ref)
		}
	}
	return rows
}

func (h *spikeHarness) waitDelivered(status, cycle string) {
	h.t.Helper()
	n := waitNotificationStatus(h.t, h.received, status)
	if n.Rule != "error_rate_spike" || n.Alerts[0].Annotations["cycle"] != cycle {
		h.t.Fatalf("delivered %s notification=%#v want cycle %s", status, n, cycle)
	}
}

func TestBuildPlatformStateOutboxEntriesKeyIgnoresPayloadAndPinsTransition(t *testing.T) {
	ref := PlatformStateRef{ProjectID: 7, Kind: "error_rate_spike", Cycle: 3, Status: "firing"}
	a := BuildPlatformStateOutboxEntries(ref, []int{1, 2}, []Notification{{Rule: "error_rate_spike", Status: "firing",
		Alerts: []Alert{{Annotations: map[string]string{"summary": "Error rate spike: 1.2 events/min"}}}}})
	b := BuildPlatformStateOutboxEntries(ref, []int{1, 2}, []Notification{{Rule: "error_rate_spike", Status: "firing",
		Alerts: []Alert{{Annotations: map[string]string{"summary": "Error rate spike above threshold"}}}}})
	if len(a) != 2 || a[0].IdempotencyKey != b[0].IdempotencyKey || a[1].IdempotencyKey != b[1].IdempotencyKey {
		t.Fatalf("rendered payload changed the key: %#v vs %#v", a, b)
	}
	if a[0].IdempotencyKey == a[1].IdempotencyKey {
		t.Fatal("channels share one idempotency key")
	}
	if a[0].RuleID != nil || a[0].ProjectID != 7 || a[0].Priority != 0 {
		t.Fatalf("firing entry=%#v", a[0])
	}
	seen := map[string]PlatformStateRef{a[0].IdempotencyKey: ref}
	for _, variant := range []PlatformStateRef{
		{ProjectID: 8, Kind: "error_rate_spike", Cycle: 3, Status: "firing"},
		{ProjectID: 7, Kind: "error_rate_spike", Cycle: 4, Status: "firing"},
		{ProjectID: 7, Kind: "error_rate_spike", Cycle: 3, Status: "resolved"},
	} {
		entry := BuildPlatformStateOutboxEntries(variant, []int{1}, []Notification{{Status: variant.Status}})[0]
		if prev, dup := seen[entry.IdempotencyKey]; dup {
			t.Fatalf("%+v and %+v share key", variant, prev)
		}
		seen[entry.IdempotencyKey] = variant
		if variant.Status == "resolved" && entry.Priority != 100 {
			t.Fatalf("resolved priority=%d", entry.Priority)
		}
	}
}

// A committed transition must be in notification_outbox when RecordErrorEvent
// and CheckErrorRateRecoveries return, with no in-memory group_wait delay.
func TestErrorRateSpikeTransitionsEnqueueDurablyBeforeReturning(t *testing.T) {
	h := newSpikeHarness(t, "spike-durable")
	svc := h.service("durable")
	h.spike(svc, h.base)
	rows := h.expectRows(PlatformStateRef{Cycle: 1, Status: "firing"})
	if rows[0].Notification.Alerts[0].Annotations["summary"] != "Error rate spike: 1.2 events/min" {
		t.Fatalf("firing annotations=%#v", rows[0].Notification.Alerts[0].Annotations)
	}
	h.waitDelivered("firing", "1")

	h.set(h.base.Add(6 * time.Minute))
	if err := svc.CheckErrorRateRecoveries(context.Background()); err != nil {
		t.Fatal(err)
	}
	h.expectRows(PlatformStateRef{Cycle: 1, Status: "firing"}, PlatformStateRef{Cycle: 1, Status: "resolved"})
	h.waitDelivered("resolved", "1")

	h.spike(svc, h.base.Add(10*time.Minute))
	h.expectRows(
		PlatformStateRef{Cycle: 1, Status: "firing"},
		PlatformStateRef{Cycle: 1, Status: "resolved"},
		PlatformStateRef{Cycle: 2, Status: "firing"},
	)
	h.waitDelivered("firing", "2")
}

// Replica replays of an already enqueued transition must deduplicate on the
// deterministic key even though they render different annotations.
func TestErrorRateSpikeReplayDeduplicatesAcrossReplicas(t *testing.T) {
	h := newSpikeHarness(t, "spike-dedup")
	a := h.service("replica-a")
	b := h.service("replica-b")
	h.spike(a, h.base)
	h.set(h.base.Add(10 * time.Second))
	for range 3 {
		for _, svc := range []*Service{a, b} {
			if err := svc.CheckErrorRateRecoveries(context.Background()); err != nil {
				t.Fatal(err)
			}
		}
	}
	h.expectRows(PlatformStateRef{Cycle: 1, Status: "firing"})
	h.waitDelivered("firing", "1")
	select {
	case dup := <-h.received:
		t.Fatalf("duplicate delivery: %#v", dup)
	case <-time.After(80 * time.Millisecond):
	}

	// A cycle notifies firing once, like the removed Engine: replays after the
	// repeat interval elapses must not re-enqueue the still-firing spike.
	h.set(h.base.Add(3 * time.Hour))
	for _, svc := range []*Service{a, b} {
		if err := svc.replayPlatformAlertStates(context.Background(), 0); err != nil {
			t.Fatal(err)
		}
	}
	h.expectRows(PlatformStateRef{Cycle: 1, Status: "firing"})
}

// A replica that commits a transition and crashes before enqueueing must not
// lose the notification: any replica's recovery loop replays it.
func TestErrorRateSpikeCrashBeforeEnqueueIsReplayed(t *testing.T) {
	h := newSpikeHarness(t, "spike-crash")
	ctx := context.Background()
	// Simulate the crashed process: the store transaction committed firing, but
	// the service never planned or enqueued it.
	for i := range 6 {
		ts := h.base.Add(time.Duration(i) * time.Second)
		if _, err := h.store.RecordPlatformErrorRate(ctx, h.project.ID, ts, ts, errorRateWindow, 1); err != nil {
			t.Fatal(err)
		}
	}
	if rows := h.rows(); len(rows) != 0 {
		t.Fatalf("precondition: outbox rows=%#v", rows)
	}
	h.set(h.base.Add(10 * time.Second))
	restarted := h.service("restarted")
	if err := restarted.CheckErrorRateRecoveries(ctx); err != nil {
		t.Fatal(err)
	}
	rows := h.expectRows(PlatformStateRef{Cycle: 1, Status: "firing"})
	if rows[0].Notification.Alerts[0].StartsAt.IsZero() {
		t.Fatalf("replayed firing lost starts_at: %#v", rows[0].Notification)
	}
	h.waitDelivered("firing", "1")

	// Same crash window on the resolved side.
	resolvedAt := h.base.Add(6 * time.Minute)
	recoveries, err := h.store.CheckPlatformErrorRateRecoveries(ctx, resolvedAt, errorRateWindow)
	if err != nil || len(recoveries) != 1 {
		t.Fatalf("recoveries=%#v err=%v", recoveries, err)
	}
	h.set(resolvedAt.Add(time.Minute))
	if err := restarted.CheckErrorRateRecoveries(ctx); err != nil {
		t.Fatal(err)
	}
	h.expectRows(PlatformStateRef{Cycle: 1, Status: "firing"}, PlatformStateRef{Cycle: 1, Status: "resolved"})
	h.waitDelivered("resolved", "1")

	// A second cycle whose resolution crashed longer ago than the replay
	// horizon is not resurrected, so a long outage cannot flood stale resolutions.
	second := h.base.Add(time.Hour)
	for i := range 6 {
		ts := second.Add(time.Duration(i) * time.Second)
		if _, err := h.store.RecordPlatformErrorRate(ctx, h.project.ID, ts, ts, errorRateWindow, 1); err != nil {
			t.Fatal(err)
		}
	}
	staleResolvedAt := second.Add(6 * time.Minute)
	if recoveries, err := h.store.CheckPlatformErrorRateRecoveries(ctx, staleResolvedAt, errorRateWindow); err != nil ||
		len(recoveries) != 1 || recoveries[0].Cycle != 2 {
		t.Fatalf("second recoveries=%#v err=%v", recoveries, err)
	}
	h.set(staleResolvedAt.Add(platformResolvedReplayHorizon + time.Minute))
	if err := restarted.CheckErrorRateRecoveries(ctx); err != nil {
		t.Fatal(err)
	}
	h.expectRows(PlatformStateRef{Cycle: 1, Status: "firing"}, PlatformStateRef{Cycle: 1, Status: "resolved"})
}

// HA ordering: replica A fires, replica B resolves. Firing must be durable
// and delivered first instead of waiting in A's memory past B's resolution.
func TestErrorRateSpikeHAFiringPrecedesResolved(t *testing.T) {
	h := newSpikeHarness(t, "spike-ha")
	a := h.service("replica-a")
	b := h.service("replica-b")
	h.spike(a, h.base)
	h.waitDelivered("firing", "1")

	h.set(h.base.Add(6 * time.Minute))
	if err := b.CheckErrorRateRecoveries(context.Background()); err != nil {
		t.Fatal(err)
	}
	h.waitDelivered("resolved", "1")
	rows := h.expectRows(PlatformStateRef{Cycle: 1, Status: "firing"}, PlatformStateRef{Cycle: 1, Status: "resolved"})
	if rows[0].ID >= rows[1].ID {
		t.Fatalf("firing outbox id %d not before resolved id %d", rows[0].ID, rows[1].ID)
	}
	// Replica A replaying afterwards must not re-emit firing for the resolved cycle.
	if err := a.CheckErrorRateRecoveries(context.Background()); err != nil {
		t.Fatal(err)
	}
	h.expectRows(PlatformStateRef{Cycle: 1, Status: "firing"}, PlatformStateRef{Cycle: 1, Status: "resolved"})
}

// A spike that fires under a silence is not enqueued, and is delivered by the
// recovery loop once the silence expires while it is still firing.
func TestErrorRateSpikeSilencedAtFiringDeliversAfterSilenceExpires(t *testing.T) {
	h := newSpikeHarness(t, "spike-silence")
	svc := h.service("silence")
	if _, err := svc.CreateSilence(context.Background(), SilenceInput{
		Project:  h.project.Slug,
		Matchers: []Matcher{{Name: "alertname", Op: "=", Value: "error_rate_spike"}},
		StartsAt: h.base.Add(-time.Minute),
		EndsAt:   h.base.Add(time.Minute),
		Creator:  "test",
	}); err != nil {
		t.Fatal(err)
	}
	h.spike(svc, h.base)
	if rows := h.rows(); len(rows) != 0 {
		t.Fatalf("silenced spike enqueued: %#v", rows)
	}
	h.set(h.base.Add(90 * time.Second))
	if err := svc.replayPlatformAlertStates(context.Background(), 0); err != nil {
		t.Fatal(err)
	}
	h.expectRows(PlatformStateRef{Cycle: 1, Status: "firing"})
	h.waitDelivered("firing", "1")
}

// Deleting the silence that held back a still-firing spike enqueues it at once
// rather than on the next recovery tick.
func TestErrorRateSpikeDeleteSilenceReplaysImmediately(t *testing.T) {
	h := newSpikeHarness(t, "spike-unsilence")
	svc := h.service("unsilence")
	ctx := context.Background()
	silence, err := svc.CreateSilence(ctx, SilenceInput{
		Project:  h.project.Slug,
		Matchers: []Matcher{{Name: "kind", Op: "=", Value: "error_rate_spike"}},
		StartsAt: h.base.Add(-time.Minute),
		EndsAt:   h.base.Add(time.Hour),
		Creator:  "test",
	})
	if err != nil {
		t.Fatal(err)
	}
	h.spike(svc, h.base)
	if rows := h.rows(); len(rows) != 0 {
		t.Fatalf("silenced spike enqueued: %#v", rows)
	}
	if err := svc.DeleteSilence(ctx, h.project.Slug, silence.ID); err != nil {
		t.Fatal(err)
	}
	h.expectRows(PlatformStateRef{Cycle: 1, Status: "firing"})
	h.waitDelivered("firing", "1")
}
