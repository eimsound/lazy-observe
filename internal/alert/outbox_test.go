package alert

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/eimsound/lazy-observe/internal/meta"
	"github.com/eimsound/lazy-observe/internal/outbound"
)

func TestOutboxWorkerRetriesThenDead(t *testing.T) {
	ctx := context.Background()
	store := openOutboxTestStore(t)
	project, err := store.CreateProject(ctx, "Alpha", "alpha")
	if err != nil {
		t.Fatal(err)
	}
	var requests atomic.Int32
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	}))
	defer endpoint.Close()
	channel, err := store.CreateNotificationChannel(ctx, meta.NotificationChannelCreate{
		Type: "webhook", Name: "dead-letter", Config: map[string]any{"url": endpoint.URL},
	})
	if err != nil {
		t.Fatal(err)
	}
	payload, _ := json.Marshal(Notification{Project: "alpha", Rule: "new_issue", Status: "firing"})
	id, inserted, err := store.EnqueueNotification(ctx, meta.NotificationOutboxEnqueue{
		IdempotencyKey: "dead-letter-key", ProjectID: project.ID, ChannelID: channel.ID, Payload: payload,
	})
	if err != nil || !inserted {
		t.Fatalf("enqueue: inserted=%v err=%v", inserted, err)
	}
	worker := NewOutboxWorker(OutboxWorkerConfig{
		Store: store, Sender: &Sender{Client: endpoint.Client(), Sleep: func(time.Duration) {}},
		WorkerID: "dead-letter-worker", PollInterval: 5 * time.Millisecond,
		RetryBase: time.Millisecond, MaxAttempts: 2, ClaimBatch: 1,
	})
	worker.Start()
	defer worker.Close()
	worker.Wake()
	item := waitOutboxStatus(t, store, id, "dead", 2*time.Second)
	if item.Attempts != 2 || item.LastError == "" {
		t.Fatalf("dead item attempts=%d last_error=%q", item.Attempts, item.LastError)
	}
	if got := requests.Load(); got != 2 {
		t.Fatalf("HTTP attempts=%d want=2 (no nested retries)", got)
	}
}

type failingLimiter struct {
	err error
}

func (f failingLimiter) Allow(_ context.Context, _ string, _ int, _ time.Duration) (bool, error) {
	return false, f.err
}

func TestOutboxWorkerRateLimiterErrorConsumesAttemptsAndSanitizesError(t *testing.T) {
	ctx := context.Background()
	store := openOutboxTestStore(t)
	project, err := store.CreateProject(ctx, "Alpha", "alpha")
	if err != nil {
		t.Fatal(err)
	}
	channel, err := store.CreateNotificationChannel(ctx, meta.NotificationChannelCreate{
		Type: "dingtalk", Name: "limiter-fail",
		Config: map[string]any{"url": "https://oapi.dingtalk.com/robot/send?access_token=ding-secret-token"},
	})
	if err != nil {
		t.Fatal(err)
	}
	payload, _ := json.Marshal(Notification{Project: "alpha", Rule: "new_issue", Status: "firing"})
	id, _, err := store.EnqueueNotification(ctx, meta.NotificationOutboxEnqueue{
		IdempotencyKey: "limiter-fail-key", ProjectID: project.ID, ChannelID: channel.ID, Payload: payload,
	})
	if err != nil {
		t.Fatal(err)
	}
	worker := NewOutboxWorker(OutboxWorkerConfig{
		Store:        store,
		WorkerID:     "limiter-fail-worker",
		PollInterval: 5 * time.Millisecond,
		RetryBase:    time.Millisecond,
		MaxAttempts:  2,
		ClaimBatch:   1,
		Limiter:      failingLimiter{err: errors.New("dial tcp 10.0.0.9:6379: redis password=redis-secret failed")},
	})
	worker.Start()
	defer worker.Close()
	worker.Wake()

	item := waitOutboxStatus(t, store, id, "dead", 2*time.Second)
	if item.Attempts != 2 {
		t.Fatalf("attempts=%d want 2", item.Attempts)
	}
	if strings.Contains(item.LastError, "redis-secret") || strings.Contains(item.LastError, "10.0.0.9") {
		t.Fatalf("last_error leaked limiter internals: %q", item.LastError)
	}
}

func TestOutboxWorkerSanitizesWebhookSecretsInLastError(t *testing.T) {
	ctx := context.Background()
	store := openOutboxTestStore(t)
	project, err := store.CreateProject(ctx, "Alpha", "alpha")
	if err != nil {
		t.Fatal(err)
	}
	// Closed local listener triggers a transport *url.Error carrying the full URL.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	deadAddr := ln.Addr().String()
	_ = ln.Close()

	channel, err := store.CreateNotificationChannel(ctx, meta.NotificationChannelCreate{
		Type: "dingtalk", Name: "secret-err",
		Config: map[string]any{"url": "http://" + deadAddr + "/robot/send?access_token=ding-top-secret-xyz"},
	})
	if err != nil {
		t.Fatal(err)
	}
	payload, _ := json.Marshal(Notification{Project: "alpha", Rule: "new_issue", Status: "firing"})
	id, _, err := store.EnqueueNotification(ctx, meta.NotificationOutboxEnqueue{
		IdempotencyKey: "secret-err-key", ProjectID: project.ID, ChannelID: channel.ID, Payload: payload,
	})
	if err != nil {
		t.Fatal(err)
	}
	worker := NewOutboxWorker(OutboxWorkerConfig{
		Store:        store,
		Sender:       &Sender{Outbound: outbound.Policy{AllowPrivate: true}, Timeout: 200 * time.Millisecond},
		WorkerID:     "secret-err-worker",
		PollInterval: 5 * time.Millisecond,
		RetryBase:    time.Millisecond,
		MaxAttempts:  1,
		ClaimBatch:   1,
	})
	worker.Start()
	defer worker.Close()
	worker.Wake()

	item := waitOutboxStatus(t, store, id, "dead", 2*time.Second)
	if item.LastError == "" {
		t.Fatal("expected non-empty last_error")
	}
	for _, forbidden := range []string{"ding-top-secret-xyz", deadAddr, "access_token"} {
		if strings.Contains(item.LastError, forbidden) {
			t.Fatalf("last_error leaked %q: %q", forbidden, item.LastError)
		}
	}
}

func TestOutboxWorkerRecoversExpiredClaim(t *testing.T) {
	ctx := context.Background()
	store := openOutboxTestStore(t)
	project, err := store.CreateProject(ctx, "Alpha", "alpha")
	if err != nil {
		t.Fatal(err)
	}
	received := make(chan string, 1)
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received <- r.Header.Get("Idempotency-Key")
		w.WriteHeader(http.StatusNoContent)
	}))
	defer endpoint.Close()
	channel, err := store.CreateNotificationChannel(ctx, meta.NotificationChannelCreate{
		Type: "webhook", Name: "recovery", Config: map[string]any{"url": endpoint.URL},
	})
	if err != nil {
		t.Fatal(err)
	}
	payload, _ := json.Marshal(Notification{Project: "alpha", Rule: "new_issue", Status: "firing"})
	id, _, err := store.EnqueueNotification(ctx, meta.NotificationOutboxEnqueue{
		IdempotencyKey: "recover-key", ProjectID: project.ID, ChannelID: channel.ID, Payload: payload,
	})
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := store.ClaimNotifications(ctx, "stopped-worker", 1, time.Second)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("initial claim=%d err=%v", len(claimed), err)
	}
	time.Sleep(1100 * time.Millisecond)
	worker := NewOutboxWorker(OutboxWorkerConfig{
		Store: store, Sender: &Sender{Client: endpoint.Client(), Sleep: func(time.Duration) {}},
		WorkerID: "recovery-worker", PollInterval: 5 * time.Millisecond, ClaimBatch: 1,
	})
	worker.Start()
	defer worker.Close()
	worker.Wake()
	waitOutboxStatus(t, store, id, "delivered", 2*time.Second)
	select {
	case key := <-received:
		if key != "recover-key" {
			t.Fatalf("Idempotency-Key=%q", key)
		}
	case <-time.After(time.Second):
		t.Fatal("recovered notification was not sent")
	}
}

func TestPlatformNotificationsUseOneOutboxItemAcrossReplicas(t *testing.T) {
	ctx := context.Background()
	store := openOutboxTestStore(t)
	project, err := store.CreateProject(ctx, "Alpha", "alpha")
	if err != nil {
		t.Fatal(err)
	}
	var requests atomic.Int32
	header := make(chan string, 2)
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		header <- r.Header.Get("Idempotency-Key")
		w.WriteHeader(http.StatusNoContent)
	}))
	defer endpoint.Close()
	services := []*Service{
		NewService(
			Config{
				Store:          store,
				OutboundPolicy: outbound.Policy{AllowPrivate: true},
				Sender:         &Sender{Client: endpoint.Client(), Sleep: func(time.Duration) {}},
				Outbox:         OutboxWorkerConfig{WorkerID: "replica-a", PollInterval: 5 * time.Millisecond},
			},
		),
		NewService(
			Config{
				Store:          store,
				OutboundPolicy: outbound.Policy{AllowPrivate: true},
				Sender:         &Sender{Client: endpoint.Client(), Sleep: func(time.Duration) {}},
				Outbox:         OutboxWorkerConfig{WorkerID: "replica-b", PollInterval: 5 * time.Millisecond},
			},
		),
	}
	defer services[0].Close()
	defer services[1].Close()
	channel, err := services[0].CreateChannel(
		ctx,
		ChannelInput{
			Type: "webhook",
			Name: "platform-ha",
			Config: map[string]any{
				"url":     endpoint.URL,
				"headers": map[string]any{"Idempotency-Key": "must-not-override"},
			},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := services[0].PutProjectAlertConfig(
		ctx,
		project.ID,
		ProjectAlertConfigInput{NewIssue: true, Regression: true, ChannelIDs: []int{channel.ID}},
	); err != nil {
		t.Fatal(err)
	}
	n := Notification{Project: project.Slug, Rule: "new_issue", Status: "firing", Alerts: []Alert{
		{
			Status:      "firing",
			Labels:      map[string]string{"source": "platform", "kind": "new_issue", "project_id": project.Slug},
			StartsAt:    time.Date(2026, 7, 18, 1, 2, 3, 0, time.UTC),
			Fingerprint: "same-event",
		},
	}}
	var wg sync.WaitGroup
	for _, service := range services {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := service.notifyPlatformLifecycle(ctx, n, 0); err != nil {
				t.Errorf("notifyPlatformLifecycle: %v", err)
			}
		}()
	}
	wg.Wait()
	select {
	case key := <-header:
		if key == "" || key == "must-not-override" {
			t.Fatalf("Idempotency-Key=%q", key)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("platform notification was not sent")
	}
	time.Sleep(100 * time.Millisecond)
	if got := requests.Load(); got != 1 {
		t.Fatalf("platform HTTP requests=%d want=1", got)
	}
}

func TestPlatformErrorRateSpikeRecoversAndSecondSpikeNotSwallowed(t *testing.T) {
	ctx := context.Background()
	store := openOutboxTestStore(t)
	project, err := store.CreateProject(ctx, "SpikeCycle", "spike-cycle")
	if err != nil {
		t.Fatal(err)
	}
	received := make(chan Notification, 8)
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var n Notification
		if err := json.NewDecoder(r.Body).Decode(&n); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		received <- n
		w.WriteHeader(http.StatusNoContent)
	}))
	defer endpoint.Close()

	base := time.Date(2026, 8, 1, 14, 0, 0, 0, time.UTC)
	var curTime atomic.Int64
	curTime.Store(base.UnixNano())
	svc := NewService(Config{
		Store:          store,
		OutboundPolicy: outbound.Policy{AllowPrivate: true},
		Sender:         &Sender{Client: endpoint.Client(), Sleep: func(time.Duration) {}},
		GroupWait:      10 * time.Millisecond,
		RepeatInterval: time.Hour,
		Now:            func() time.Time { return time.Unix(0, curTime.Load()).UTC() },
		Outbox:         OutboxWorkerConfig{WorkerID: "spike-worker", PollInterval: 5 * time.Millisecond},
	})
	defer svc.Close()

	ch, err := svc.CreateChannel(ctx, ChannelInput{
		Type: "webhook", Name: "spike-hook", Config: map[string]any{"url": endpoint.URL},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.PutProjectAlertConfig(ctx, project.ID, ProjectAlertConfigInput{
		NewIssue: false, Regression: false, ErrorRateThresholdPerMinute: 1, ChannelIDs: []int{ch.ID},
	}); err != nil {
		t.Fatal(err)
	}

	// Trigger first spike (6 events > 1/min * 5min).
	for i := range 6 {
		ts := base.Add(time.Duration(i) * time.Second)
		curTime.Store(ts.UnixNano())
		if err := svc.RecordErrorEvent(ctx, project.ID, ts); err != nil {
			t.Fatal(err)
		}
	}
	first := waitNotificationStatus(t, received, "firing")
	if first.Rule != "error_rate_spike" || first.Alerts[0].Annotations["cycle"] != "1" {
		t.Fatalf("first spike notification=%#v", first)
	}

	// Advance past 5m window and check recoveries -> resolved notification emitted.
	curTime.Store(base.Add(6 * time.Minute).UnixNano())
	if err := svc.CheckErrorRateRecoveries(ctx); err != nil {
		t.Fatal(err)
	}
	resolved := waitNotificationStatus(t, received, "resolved")
	if resolved.Rule != "error_rate_spike" || resolved.Alerts[0].Annotations["cycle"] != "1" {
		t.Fatalf("resolved spike notification=%#v", resolved)
	}

	// Trigger second spike with identical rate -> must NOT be swallowed by idempotency key.
	secondBase := base.Add(10 * time.Minute)
	for i := range 6 {
		ts := secondBase.Add(time.Duration(i) * time.Second)
		curTime.Store(ts.UnixNano())
		if err := svc.RecordErrorEvent(ctx, project.ID, ts); err != nil {
			t.Fatal(err)
		}
	}
	second := waitNotificationStatus(t, received, "firing")
	if second.Rule != "error_rate_spike" || second.Alerts[0].Annotations["cycle"] != "2" {
		t.Fatalf("second spike notification=%#v", second)
	}
}

func openOutboxTestStore(t *testing.T) *meta.Store {
	t.Helper()
	store, err := meta.OpenStore(context.Background(), filepath.Join(t.TempDir(), "meta.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func waitOutboxStatus(
	t *testing.T,
	store *meta.Store,
	id int64,
	want string,
	timeout time.Duration,
) meta.NotificationOutboxItem {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		item, err := store.NotificationOutboxItem(context.Background(), id)
		if err == nil && item.Status == want {
			return item
		}
		time.Sleep(5 * time.Millisecond)
	}
	item, err := store.NotificationOutboxItem(context.Background(), id)
	t.Fatalf("outbox item status=%q want=%q err=%v", item.Status, want, err)
	return meta.NotificationOutboxItem{}
}

func TestServiceExternalURLDerivesLinksAtSendTimeAndTestChannel(t *testing.T) {
	ctx := context.Background()
	store := openOutboxTestStore(t)
	project, err := store.CreateProject(ctx, "Links", "links")
	if err != nil {
		t.Fatal(err)
	}
	received := make(chan Notification, 4)
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var n Notification
		if err := json.NewDecoder(r.Body).Decode(&n); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		received <- n
		w.WriteHeader(http.StatusNoContent)
	}))
	defer endpoint.Close()

	// Verify caller-provided Sender with empty ExternalURL inherits cfg.ExternalURL,
	// and default Sender receives it too.
	svc := NewService(Config{
		Store:          store,
		OutboundPolicy: outbound.Policy{AllowPrivate: true},
		ExternalURL:    "https://observe.example.com",
		Sender:         &Sender{Client: endpoint.Client(), Sleep: func(time.Duration) {}},
		Outbox:         OutboxWorkerConfig{WorkerID: "links-worker", PollInterval: 5 * time.Millisecond},
	})
	defer svc.Close()

	if svc.sender.ExternalURL != "https://observe.example.com" {
		t.Fatalf("sender.ExternalURL = %q, want %q", svc.sender.ExternalURL, "https://observe.example.com")
	}

	defaultSvc := NewService(Config{Store: store, ExternalURL: "https://default.example.com"})
	defer defaultSvc.Close()
	if defaultSvc.sender.ExternalURL != "https://default.example.com" {
		t.Fatalf(
			"default sender.ExternalURL = %q, want %q",
			defaultSvc.sender.ExternalURL,
			"https://default.example.com",
		)
	}

	ch, err := svc.CreateChannel(ctx, ChannelInput{
		Type: "webhook", Name: "links-hook", Config: map[string]any{"url": endpoint.URL},
	})
	if err != nil {
		t.Fatal(err)
	}

	// 1. TestChannel delivers /alerts/channels with no alert URLs.
	if err := svc.TestChannel(ctx, ch.ID); err != nil {
		t.Fatalf("TestChannel failed: %v", err)
	}
	select {
	case got := <-received:
		if got.URL != "https://observe.example.com/alerts/channels" {
			t.Fatalf("TestChannel URL = %q, want %q", got.URL, "https://observe.example.com/alerts/channels")
		}
		if len(got.Alerts) != 1 || got.Alerts[0].URL != "" {
			t.Fatalf("TestChannel alert URLs should be empty: %+v", got.Alerts)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("TestChannel notification was not received")
	}

	// 2. Outbox stores payload without URLs and fills them only at send time.
	entries := BuildOutboxEntries(project.ID, nil, []int{ch.ID}, []Notification{{
		Project: project.Slug,
		Rule:    "DiskFull",
		Status:  "firing",
		Alerts: []Alert{{
			Status:      "firing",
			Fingerprint: "fp-send-time",
			Labels:      map[string]string{"alertname": "DiskFull"},
			StartsAt:    time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC),
		}},
	}}, time.Now().UTC(), time.Hour)
	if len(entries) != 1 {
		t.Fatalf("entries len = %d, want 1", len(entries))
	}
	if strings.Contains(string(entries[0].Payload), `"url"`) {
		t.Fatalf("outbox payload must not persist url: %s", entries[0].Payload)
	}
	id, inserted, err := store.EnqueueNotification(ctx, entries[0])
	if err != nil || !inserted {
		t.Fatalf("enqueue failed: inserted=%v err=%v", inserted, err)
	}
	svc.WakeOutbox()
	waitOutboxStatus(t, store, id, "delivered", 2*time.Second)

	select {
	case got := <-received:
		wantURL := "https://observe.example.com/alerts?fingerprint=fp-send-time&project=links"
		if got.URL != wantURL || len(got.Alerts) != 1 || got.Alerts[0].URL != wantURL {
			t.Fatalf("delivered notification URL mismatch: got=%+v want=%q", got, wantURL)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("outbox notification was not delivered")
	}
}
