package alert

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"sync"
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

// waitDelivered waits for the next error_rate_spike delivery and fails unless
// it is (status, cycle). The outbox claims one alert's transitions per channel
// strictly in causal order (issue #386), so consecutive calls also assert the
// delivery order even though OutboxWorker.drain delivers batches concurrently.
func (h *spikeHarness) waitDelivered(status, cycle string) {
	h.t.Helper()
	select {
	case n := <-h.received:
		if n.Rule != "error_rate_spike" || n.Status != status || len(n.Alerts) != 1 ||
			n.Alerts[0].Annotations["cycle"] != cycle {
			h.t.Fatalf("next delivery=%#v want %s notification cycle %s", n, status, cycle)
		}
	case <-time.After(2 * time.Second):
		h.t.Fatalf("timeout waiting for %s notification cycle %s", status, cycle)
	}
}

// expectNoDelivery fails if any delivery arrives within d.
func (h *spikeHarness) expectNoDelivery(d time.Duration) {
	h.t.Helper()
	select {
	case n := <-h.received:
		h.t.Fatalf("unexpected delivery: %#v", n)
	case <-time.After(d):
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
	h.expectNoDelivery(80 * time.Millisecond)

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

// A full cycle that fired and resolved while silenced never notified firing, so
// deleting the silence within the resolved replay horizon must not emit an
// orphan resolved notification.
func TestErrorRateSpikeSilencedFullCycleNoOrphanResolvedOnDeleteSilence(t *testing.T) {
	h := newSpikeHarness(t, "spike-silenced-del")
	svc := h.service("silenced-del")
	ctx := context.Background()
	silence, err := svc.CreateSilence(ctx, SilenceInput{
		Project:  h.project.Slug,
		Matchers: []Matcher{{Name: "alertname", Op: "=", Value: "error_rate_spike"}},
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
	h.set(h.base.Add(6 * time.Minute))
	if err := svc.CheckErrorRateRecoveries(ctx); err != nil {
		t.Fatal(err)
	}
	if rows := h.rows(); len(rows) != 0 {
		t.Fatalf("silenced resolved enqueued: %#v", rows)
	}
	h.set(h.base.Add(8 * time.Minute))
	if err := svc.DeleteSilence(ctx, h.project.Slug, silence.ID); err != nil {
		t.Fatal(err)
	}
	if rows := h.rows(); len(rows) != 0 {
		t.Fatalf("orphan resolved enqueued after DeleteSilence: %#v", rows)
	}
	h.set(h.base.Add(8*time.Minute + 5*time.Second))
	if err := svc.CheckErrorRateRecoveries(ctx); err != nil {
		t.Fatal(err)
	}
	if rows := h.rows(); len(rows) != 0 {
		t.Fatalf("orphan resolved enqueued on recovery tick after DeleteSilence: %#v", rows)
	}
}

// A full cycle that fired and resolved while silenced must not emit an orphan
// resolved notification once the silence expires within the replay horizon.
func TestErrorRateSpikeSilencedFullCycleNoOrphanResolvedOnSilenceExpiry(t *testing.T) {
	h := newSpikeHarness(t, "spike-silenced-exp")
	svc := h.service("silenced-exp")
	ctx := context.Background()
	if _, err := svc.CreateSilence(ctx, SilenceInput{
		Project:  h.project.Slug,
		Matchers: []Matcher{{Name: "alertname", Op: "=", Value: "error_rate_spike"}},
		StartsAt: h.base.Add(-time.Minute),
		EndsAt:   h.base.Add(8 * time.Minute),
		Creator:  "test",
	}); err != nil {
		t.Fatal(err)
	}
	h.spike(svc, h.base)
	if rows := h.rows(); len(rows) != 0 {
		t.Fatalf("silenced spike enqueued: %#v", rows)
	}
	h.set(h.base.Add(6 * time.Minute))
	if err := svc.CheckErrorRateRecoveries(ctx); err != nil {
		t.Fatal(err)
	}
	if rows := h.rows(); len(rows) != 0 {
		t.Fatalf("silenced resolved enqueued: %#v", rows)
	}
	h.set(h.base.Add(9 * time.Minute))
	if err := svc.CheckErrorRateRecoveries(ctx); err != nil {
		t.Fatal(err)
	}
	if rows := h.rows(); len(rows) != 0 {
		t.Fatalf("orphan resolved enqueued after silence expired: %#v", rows)
	}
}

// When firing was already enqueued for a cycle and a subsequent silence held
// back its resolved notification, lifting the silence within the horizon must
// still replay the resolved notification.
func TestErrorRateSpikeResolvedReplayedWhenPriorFiringWasEnqueued(t *testing.T) {
	h := newSpikeHarness(t, "spike-resolved-replay")
	svc := h.service("resolved-replay")
	ctx := context.Background()
	h.spike(svc, h.base)
	h.expectRows(PlatformStateRef{Cycle: 1, Status: "firing"})
	h.waitDelivered("firing", "1")

	silence, err := svc.CreateSilence(ctx, SilenceInput{
		Project:  h.project.Slug,
		Matchers: []Matcher{{Name: "alertname", Op: "=", Value: "error_rate_spike"}},
		StartsAt: h.base.Add(5 * time.Minute),
		EndsAt:   h.base.Add(time.Hour),
		Creator:  "test",
	})
	if err != nil {
		t.Fatal(err)
	}
	h.set(h.base.Add(6 * time.Minute))
	if err := svc.CheckErrorRateRecoveries(ctx); err != nil {
		t.Fatal(err)
	}
	h.expectRows(PlatformStateRef{Cycle: 1, Status: "firing"})

	h.set(h.base.Add(8 * time.Minute))
	if err := svc.DeleteSilence(ctx, h.project.Slug, silence.ID); err != nil {
		t.Fatal(err)
	}
	h.expectRows(PlatformStateRef{Cycle: 1, Status: "firing"}, PlatformStateRef{Cycle: 1, Status: "resolved"})
	h.waitDelivered("resolved", "1")
}

// When firing was committed to the store before a crash (no outbox row yet),
// a single CheckErrorRateRecoveries call after the window expires must replay
// firing before enqueueing resolved so firing outbox id < resolved id.
func TestErrorRateSpikeCrashBeforeRecoveryTickReplaysFiringBeforeResolved(t *testing.T) {
	h := newSpikeHarness(t, "spike-crash-order")
	ctx := context.Background()
	for i := range 6 {
		ts := h.base.Add(time.Duration(i) * time.Second)
		if _, err := h.store.RecordPlatformErrorRate(ctx, h.project.ID, ts, ts, errorRateWindow, 1); err != nil {
			t.Fatal(err)
		}
	}
	if rows := h.rows(); len(rows) != 0 {
		t.Fatalf("precondition: outbox rows=%#v", rows)
	}

	h.set(h.base.Add(6 * time.Minute))
	restarted := h.service("restarted-order")
	if err := restarted.CheckErrorRateRecoveries(ctx); err != nil {
		t.Fatal(err)
	}
	rows := h.expectRows(PlatformStateRef{Cycle: 1, Status: "firing"}, PlatformStateRef{Cycle: 1, Status: "resolved"})
	if rows[0].ID >= rows[1].ID {
		t.Fatalf("replayed firing outbox id %d not before resolved id %d", rows[0].ID, rows[1].ID)
	}
}

// Updating a silence so it ends before now must immediately replay a held-back
// firing spike into notification_outbox.
func TestErrorRateSpikeUpdateSilenceReplaysImmediately(t *testing.T) {
	h := newSpikeHarness(t, "spike-update-silence")
	svc := h.service("update-silence")
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

	h.set(h.base.Add(2 * time.Minute))
	if _, err := svc.UpdateSilence(ctx, silence.ID, SilenceInput{
		Project: h.project.Slug,
		EndsAt:  h.base.Add(time.Minute),
	}); err != nil {
		t.Fatal(err)
	}
	h.expectRows(PlatformStateRef{Cycle: 1, Status: "firing"})
	h.waitDelivered("firing", "1")
}

// CreateInhibitionRule, UpdateInhibitionRule, and DeleteInhibitionRule all
// trigger immediate project-scoped replay of durable error_rate_spike states.
func TestErrorRateSpikeInhibitionRuleHooksReplayImmediately(t *testing.T) {
	hCreate := newSpikeHarness(t, "spike-inhib-create")
	ctx := context.Background()
	for i := range 6 {
		ts := hCreate.base.Add(time.Duration(i) * time.Second)
		if _, err := hCreate.store.RecordPlatformErrorRate(
			ctx,
			hCreate.project.ID,
			ts,
			ts,
			errorRateWindow,
			1,
		); err != nil {
			t.Fatal(err)
		}
	}
	if rows := hCreate.rows(); len(rows) != 0 {
		t.Fatalf("precondition: outbox rows=%#v", rows)
	}
	svcCreate := hCreate.service("inhib-create")
	if _, err := svcCreate.CreateInhibitionRule(ctx, InhibitionRuleInput{
		Project:        hCreate.project.Slug,
		SourceMatchers: []Matcher{{Name: "alertname", Op: "=", Value: "OtherSource"}},
		TargetMatchers: []Matcher{{Name: "alertname", Op: "=", Value: "OtherRule"}},
		EqualLabels:    []string{"project_id"},
	}); err != nil {
		t.Fatal(err)
	}
	hCreate.expectRows(PlatformStateRef{Cycle: 1, Status: "firing"})
	hCreate.waitDelivered("firing", "1")

	hInhib := newSpikeHarness(t, "spike-inhib-hooks")
	svc := hInhib.service("inhib-hooks")
	nativeRule, err := svc.CreateRule(ctx, RuleInput{
		Project:  hInhib.project.Slug,
		Name:     "OutageDown",
		Expr:     "up == 0",
		For:      "0s",
		Severity: "critical",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := hInhib.store.HeartbeatAlertEngine(ctx, "seed-member", time.Minute); err != nil {
		t.Fatal(err)
	}
	lease, err := hInhib.store.AcquireAlertRuleLease(ctx, nativeRule.ID, "seed-member", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	now := hInhib.now()
	if err := hInhib.store.PutAlertRuleState(ctx, meta.AlertRuleState{
		RuleID:          nativeRule.ID,
		Fingerprint:     "native-outage-fp",
		Status:          "firing",
		FiringSince:     &now,
		LastEvaluatedAt: now,
		Labels:          map[string]string{"alertname": "OutageDown", "project_id": hInhib.project.Slug},
		Annotations:     map[string]string{},
	}, "seed-member", lease.Epoch); err != nil {
		t.Fatal(err)
	}

	inhib, err := svc.CreateInhibitionRule(ctx, InhibitionRuleInput{
		Project:        hInhib.project.Slug,
		SourceMatchers: []Matcher{{Name: "alertname", Op: "=", Value: "OutageDown"}},
		TargetMatchers: []Matcher{{Name: "kind", Op: "=", Value: "error_rate_spike"}},
		EqualLabels:    []string{"project_id"},
	})
	if err != nil {
		t.Fatal(err)
	}

	hInhib.spike(svc, hInhib.base)
	if rows := hInhib.rows(); len(rows) != 0 {
		t.Fatalf("inhibited cycle 1 spike enqueued: %#v", rows)
	}
	disabled := false
	if _, err := svc.UpdateInhibitionRule(ctx, inhib.ID, InhibitionRuleInput{
		Project: hInhib.project.Slug,
		Enabled: &disabled,
	}); err != nil {
		t.Fatal(err)
	}
	hInhib.expectRows(PlatformStateRef{Cycle: 1, Status: "firing"})
	hInhib.waitDelivered("firing", "1")

	hInhib.set(hInhib.base.Add(6 * time.Minute))
	if err := svc.CheckErrorRateRecoveries(ctx); err != nil {
		t.Fatal(err)
	}
	hInhib.expectRows(PlatformStateRef{Cycle: 1, Status: "firing"}, PlatformStateRef{Cycle: 1, Status: "resolved"})
	hInhib.waitDelivered("resolved", "1")

	enabled := true
	if _, err := svc.UpdateInhibitionRule(ctx, inhib.ID, InhibitionRuleInput{
		Project: hInhib.project.Slug,
		Enabled: &enabled,
	}); err != nil {
		t.Fatal(err)
	}
	hInhib.spike(svc, hInhib.base.Add(10*time.Minute))
	hInhib.expectRows(PlatformStateRef{Cycle: 1, Status: "firing"}, PlatformStateRef{Cycle: 1, Status: "resolved"})

	if err := svc.DeleteInhibitionRule(ctx, hInhib.project.Slug, inhib.ID); err != nil {
		t.Fatal(err)
	}
	hInhib.expectRows(
		PlatformStateRef{Cycle: 1, Status: "firing"},
		PlatformStateRef{Cycle: 1, Status: "resolved"},
		PlatformStateRef{Cycle: 2, Status: "firing"},
	)
	hInhib.waitDelivered("firing", "2")
}

// Issue #386 (intra-batch crash window): a batch whose accounting commits
// resolved(C) and then firing(C+1) leaves only the firing(C+1) state row. If
// the process dies before enqueueing, the logged transitions still deliver
// resolved(C) and firing(C+1), in that order, from any replica.
func TestErrorRateSpikeIntraBatchCrashDeliversResolvedBeforeRefire(t *testing.T) {
	h := newSpikeHarness(t, "spike-batch-crash")
	ctx := context.Background()
	svc := h.service("batch-crash")
	h.spike(svc, h.base)
	h.waitDelivered("firing", "1")

	late := h.base.Add(6 * time.Minute)
	events := make([]time.Time, 7)
	for i := range events {
		events[i] = late.Add(-20*time.Second + time.Duration(i)*time.Second)
	}
	// The crashed replica: one batch committed resolved(1) then firing(2), but
	// nothing reached notification_outbox.
	results, err := h.store.RecordPlatformErrorRates(ctx, late, errorRateWindow, []meta.PlatformErrorRateBatch{
		{ProjectID: h.project.ID, ThresholdPerMin: 1, OccurredAt: events},
	})
	if err != nil || len(results) != 1 || len(results[0].Transitions) != 2 {
		t.Fatalf("crashed batch results=%#v err=%v", results, err)
	}
	h.expectRows(PlatformStateRef{Cycle: 1, Status: "firing"})

	h.set(late.Add(5 * time.Second))
	restarted := h.service("batch-restarted")
	if err := restarted.CheckErrorRateRecoveries(ctx); err != nil {
		t.Fatal(err)
	}
	rows := h.expectRows(
		PlatformStateRef{Cycle: 1, Status: "firing"},
		PlatformStateRef{Cycle: 1, Status: "resolved"},
		PlatformStateRef{Cycle: 2, Status: "firing"},
	)
	if got := rows[1].Notification.Alerts[0].Annotations["summary"]; got != "Error rate spike recovered: 0.2 events/min" {
		t.Fatalf("resolved summary=%q", got)
	}
	h.waitDelivered("resolved", "1")
	h.waitDelivered("firing", "2")
	if pending, err := h.store.PendingPlatformAlertTransitions(ctx, 0, 0); err != nil || len(pending) != 0 {
		t.Fatalf("transitions left after drain=%#v err=%v", pending, err)
	}
}

// Issue #386 (fast-resolve crash window): replica A commits firing(1) and
// dies before enqueueing; before any recovery tick, replica B's ingest path
// resolves the cycle. B's drain must enqueue the logged firing(1) ahead of
// resolved(1) instead of dropping both (the resolved as an orphan).
func TestErrorRateSpikeCrashedFiringSurvivesIngestResolve(t *testing.T) {
	h := newSpikeHarness(t, "spike-fast-resolve")
	ctx := context.Background()
	for i := range 6 {
		ts := h.base.Add(time.Duration(i) * time.Second)
		if _, err := h.store.RecordPlatformErrorRate(ctx, h.project.ID, ts, ts, errorRateWindow, 1); err != nil {
			t.Fatal(err)
		}
	}
	if rows := h.rows(); len(rows) != 0 {
		t.Fatalf("precondition: outbox rows=%#v", rows)
	}
	b := h.service("fast-resolve-b")
	resolveAt := h.base.Add(6 * time.Minute)
	h.set(resolveAt)
	if err := b.RecordErrorEvent(ctx, h.project.ID, resolveAt); err != nil {
		t.Fatal(err)
	}
	rows := h.expectRows(PlatformStateRef{Cycle: 1, Status: "firing"}, PlatformStateRef{Cycle: 1, Status: "resolved"})
	if rows[0].ID >= rows[1].ID {
		t.Fatalf("firing outbox id %d not before resolved id %d", rows[0].ID, rows[1].ID)
	}
	h.waitDelivered("firing", "1")
	h.waitDelivered("resolved", "1")
}

// A configuration reset that resolves an open cycle is logged with the
// resolution, so its notification carries the reset summary and the new
// threshold.
func TestErrorRateSpikeConfigResetResolvesThroughTransitionLog(t *testing.T) {
	h := newSpikeHarness(t, "spike-reset")
	ctx := context.Background()
	svc := h.service("reset")
	h.spike(svc, h.base)
	h.waitDelivered("firing", "1")
	if _, err := svc.PutProjectAlertConfig(ctx, h.project.ID, ProjectAlertConfigInput{
		ErrorRateThresholdPerMinute: 4,
		ChannelIDs:                  []int{h.channel},
	}); err != nil {
		t.Fatal(err)
	}
	rows := h.expectRows(PlatformStateRef{Cycle: 1, Status: "firing"}, PlatformStateRef{Cycle: 1, Status: "resolved"})
	annotations := rows[1].Notification.Alerts[0].Annotations
	if annotations["summary"] != "Error rate spike state reset after alert configuration change" ||
		annotations["threshold_per_min"] != "4" {
		t.Fatalf("reset annotations=%#v", annotations)
	}
	h.waitDelivered("resolved", "1")
}

// Regression test for exclusive transition-stream claiming and fencing across a
// silence expiry boundary: replica B claims t1=firing(1) while silenced and
// pauses before commit; after its lease expires, replica A steals t1 (still
// silenced -> 0 rows), drains t2=resolved(1) (no prior firing -> 0 rows), then
// crosses silence expiry before draining t3=firing(2). When replica B resumes
// after silence expiry and tries to commit its stale t1 claim, it is fenced out
// with zero outbox rows written, leaving strictly [firing(2)] delivered.
func TestErrorRateSpikeSilenceBoundaryStaleClaimFenced(t *testing.T) {
	h := newSpikeHarness(t, "spike-silence-fence")
	ctx := context.Background()
	svcA := h.service("silence-fence-a")
	svcB := h.service("silence-fence-b")
	svcB.transitionClaimTTL = 10 * time.Second

	if _, err := svcA.CreateSilence(ctx, SilenceInput{
		Project:  h.project.Slug,
		Matchers: []Matcher{{Name: "alertname", Op: "=", Value: "error_rate_spike"}},
		StartsAt: h.base.Add(-time.Minute),
		EndsAt:   h.base.Add(7 * time.Minute),
		Creator:  "test",
	}); err != nil {
		t.Fatal(err)
	}

	firstSpike := make([]time.Time, 6)
	for i := range firstSpike {
		firstSpike[i] = h.base.Add(time.Duration(i) * time.Second)
	}
	if _, err := h.store.RecordPlatformErrorRates(
		ctx,
		h.base.Add(10*time.Second),
		errorRateWindow,
		[]meta.PlatformErrorRateBatch{
			{ProjectID: h.project.ID, ThresholdPerMin: 1, OccurredAt: firstSpike},
		},
	); err != nil {
		t.Fatal(err)
	}
	late := h.base.Add(6 * time.Minute)
	secondBatch := make([]time.Time, 7)
	for i := range secondBatch {
		secondBatch[i] = late.Add(-20*time.Second + time.Duration(i)*time.Second)
	}
	if _, err := h.store.RecordPlatformErrorRates(ctx, late, errorRateWindow, []meta.PlatformErrorRateBatch{
		{ProjectID: h.project.ID, ThresholdPerMin: 1, OccurredAt: secondBatch},
	}); err != nil {
		t.Fatal(err)
	}
	if pending, err := h.store.PendingPlatformAlertTransitions(ctx, h.project.ID, 0); err != nil || len(pending) != 3 {
		t.Fatalf("seeded pending transitions=%#v err=%v", pending, err)
	}
	if rows := h.rows(); len(rows) != 0 {
		t.Fatalf("precondition outbox rows=%#v", rows)
	}

	h.set(h.base.Add(6*time.Minute + 10*time.Second))
	bClaimed := make(chan meta.PlatformAlertTransition, 1)
	releaseB := make(chan struct{})
	released := false
	t.Cleanup(func() {
		if !released {
			close(releaseB)
		}
	})
	var bOnce sync.Once
	svcB.onTransitionClaimed = func(tr meta.PlatformAlertTransition) {
		bOnce.Do(func() {
			bClaimed <- tr
			<-releaseB
		})
	}
	bDone := make(chan error, 1)
	go func() {
		bDone <- svcB.drainPlatformAlertTransitions(ctx, h.project.ID)
	}()

	var trB meta.PlatformAlertTransition
	select {
	case trB = <-bClaimed:
	case err := <-bDone:
		t.Fatalf("replica B exited before claiming: err=%v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for replica B to claim t1")
	}
	if trB.Status != "firing" || trB.Cycle != 1 {
		t.Fatalf("replica B claimed=%#v want firing cycle 1", trB)
	}

	// While B's lease is active, A cannot steal or bypass t1.
	h.set(h.base.Add(6*time.Minute + 15*time.Second))
	if err := svcA.drainPlatformAlertTransitions(ctx, h.project.ID); err != nil {
		t.Fatal(err)
	}
	if pending, err := h.store.PendingPlatformAlertTransitions(ctx, h.project.ID, 0); err != nil || len(pending) != 3 {
		t.Fatalf("pending while leased=%#v err=%v", pending, err)
	}

	// Advance past B's 10s lease while the silence is still active, then advance
	// past silence expiry as soon as A claims t3=firing(2).
	h.set(h.base.Add(6*time.Minute + 30*time.Second))
	svcA.onTransitionClaimed = func(tr meta.PlatformAlertTransition) {
		if tr.Status == "firing" && tr.Cycle == 2 {
			h.set(h.base.Add(7*time.Minute + 5*time.Second))
		}
	}
	if err := svcA.drainPlatformAlertTransitions(ctx, h.project.ID); err != nil {
		t.Fatal(err)
	}
	h.expectRows(PlatformStateRef{Cycle: 2, Status: "firing"})
	h.waitDelivered("firing", "2")

	// Resume B now that the silence has expired: B plans firing(1) but must be
	// fenced out cleanly with zero outbox rows written.
	close(releaseB)
	released = true
	select {
	case err := <-bDone:
		if err != nil {
			t.Fatalf("replica B drain err=%v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for replica B drain to finish")
	}
	h.expectRows(PlatformStateRef{Cycle: 2, Status: "firing"})
	h.expectNoDelivery(100 * time.Millisecond)
	if pending, err := h.store.PendingPlatformAlertTransitions(ctx, h.project.ID, 0); err != nil || len(pending) != 0 {
		t.Fatalf("pending after fenced drain=%#v err=%v", pending, err)
	}
}

// Concurrent drains from two Service replicas over [firing(1), resolved(1),
// firing(2)] must enqueue and deliver strictly in causal order without races.
func TestErrorRateSpikeConcurrentServicesDrainInCausalOrder(t *testing.T) {
	h := newSpikeHarness(t, "spike-concurrent-drain")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	firstSpike := make([]time.Time, 6)
	for i := range firstSpike {
		firstSpike[i] = h.base.Add(time.Duration(i) * time.Second)
	}
	if _, err := h.store.RecordPlatformErrorRates(
		ctx,
		h.base.Add(10*time.Second),
		errorRateWindow,
		[]meta.PlatformErrorRateBatch{
			{ProjectID: h.project.ID, ThresholdPerMin: 1, OccurredAt: firstSpike},
		},
	); err != nil {
		t.Fatal(err)
	}
	late := h.base.Add(6 * time.Minute)
	secondBatch := make([]time.Time, 7)
	for i := range secondBatch {
		secondBatch[i] = late.Add(-20*time.Second + time.Duration(i)*time.Second)
	}
	if _, err := h.store.RecordPlatformErrorRates(ctx, late, errorRateWindow, []meta.PlatformErrorRateBatch{
		{ProjectID: h.project.ID, ThresholdPerMin: 1, OccurredAt: secondBatch},
	}); err != nil {
		t.Fatal(err)
	}
	h.set(h.base.Add(6*time.Minute + 5*time.Second))

	svcA := h.service("concurrent-drain-a")
	svcB := h.service("concurrent-drain-b")
	start := make(chan struct{})
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	for _, svc := range []*Service{svcA, svcB} {
		wg.Add(1)
		go func(s *Service) {
			defer wg.Done()
			<-start
			for {
				if err := s.drainPlatformAlertTransitions(ctx, h.project.ID); err != nil {
					errs <- err
					return
				}
				pending, err := h.store.PendingPlatformAlertTransitions(ctx, h.project.ID, 0)
				if err != nil {
					errs <- err
					return
				}
				if len(pending) == 0 {
					return
				}
			}
		}(svc)
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent drain err=%v", err)
		}
	}

	if pending, err := h.store.PendingPlatformAlertTransitions(ctx, h.project.ID, 0); err != nil || len(pending) != 0 {
		t.Fatalf("pending after concurrent drain=%#v err=%v", pending, err)
	}
	rows := h.expectRows(
		PlatformStateRef{Cycle: 1, Status: "firing"},
		PlatformStateRef{Cycle: 1, Status: "resolved"},
		PlatformStateRef{Cycle: 2, Status: "firing"},
	)
	if !(rows[0].ID < rows[1].ID && rows[1].ID < rows[2].ID) {
		t.Fatalf("outbox rows out of causal ID order: %#v", rows)
	}
	h.waitDelivered("firing", "1")
	h.waitDelivered("resolved", "1")
	h.waitDelivered("firing", "2")
	h.expectNoDelivery(100 * time.Millisecond)
}
