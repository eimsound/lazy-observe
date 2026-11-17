package alert

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/eimsound/lazy-observe/internal/meta"
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
	if got := requests.Load(); got != 6 {
		t.Fatalf("HTTP attempts=%d want=6", got)
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
				Store:  store,
				Sender: &Sender{Client: endpoint.Client(), Sleep: func(time.Duration) {}},
				Outbox: OutboxWorkerConfig{WorkerID: "replica-a", PollInterval: 5 * time.Millisecond},
			},
		),
		NewService(
			Config{
				Store:  store,
				Sender: &Sender{Client: endpoint.Client(), Sleep: func(time.Duration) {}},
				Outbox: OutboxWorkerConfig{WorkerID: "replica-b", PollInterval: 5 * time.Millisecond},
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
			if err := service.notifyPlatform(ctx, n); err != nil {
				t.Errorf("notifyPlatform: %v", err)
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
