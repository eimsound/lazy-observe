package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/eimsound/lazy-observe/internal/alert"
	"github.com/eimsound/lazy-observe/internal/errortrack"
	"github.com/eimsound/lazy-observe/internal/meta"
	"github.com/eimsound/lazy-observe/internal/outbound"
)

func TestErrorRateHandleErrorEventsWithoutAlertsIsNoop(t *testing.T) {
	gw := &Gateway{}
	if err := gw.HandleErrorEvents(context.Background(), []errortrack.ErrorOccurrence{{ProjectID: 1}}); err != nil {
		t.Fatalf("HandleErrorEvents without alerts err=%v", err)
	}
}

// Issue #375: one ingest request whose events complete as one error Inbox
// batch drives the batched error-rate accounting through the real gateway,
// alert service and metadata store, and fires error_rate_spike exactly once.
func TestErrorRateSpikeFromSingleInboxBatch(t *testing.T) {
	ctx := context.Background()
	store := testStore(t)
	project, err := store.CreateProject(ctx, "Batch Spike", "batch-spike")
	if err != nil {
		t.Fatal(err)
	}
	token, err := store.CreateProjectAPIToken(ctx, project.ID)
	if err != nil {
		t.Fatal(err)
	}
	notifications := make(chan alert.Notification, 16)
	webhook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var n alert.Notification
		if err := json.NewDecoder(r.Body).Decode(&n); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		notifications <- n
		w.WriteHeader(http.StatusNoContent)
	}))
	defer webhook.Close()
	logs := httptest.NewServer(
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }),
	)
	defer logs.Close()

	alerts := alert.NewService(alert.Config{
		Store:          store,
		OutboundPolicy: outbound.Policy{AllowPrivate: true},
		GroupWait:      10 * time.Millisecond,
		RepeatInterval: time.Hour,
	})
	channel, err := alerts.CreateChannel(
		ctx,
		alert.ChannelInput{Type: "webhook", Name: "batch-spike", Config: map[string]any{"url": webhook.URL}},
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.PutProjectAlertConfig(ctx, project.ID, meta.ProjectAlertConfigUpdate{
		ErrorRateThresholdPerMinute: 1,
		ChannelIDs:                  []int{channel.ID},
	}); err != nil {
		t.Fatal(err)
	}
	gw := newMetadataTestGateway(store, Config{Tokens: store, Store: store, Alerts: alerts, Logs: logs.URL})
	t.Cleanup(func() { _ = gw.Shutdown(); alerts.Close() })

	now := time.Now().UTC()
	events := make([]any, 8)
	for i := range events {
		events[i] = map[string]any{
			"event_id":  fmt.Sprintf("batch-spike-%d", i),
			"timestamp": now.Add(-time.Duration(i) * time.Second),
			"platform":  "go",
			"level":     "error",
			"exception": map[string]any{
				"type":  "BatchError",
				"value": "burst",
				"stacktrace": map[string]any{
					"frames": []map[string]any{{"function": "burst", "filename": "burst.go", "in_app": true}},
				},
			},
		}
	}
	payload, _ := json.Marshal(map[string]any{"events": events})
	req := httptest.NewRequest(http.MethodPost, "/api/events/errors", bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := gw.App().Test(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("post errors: status=%d err=%v", resp.StatusCode, err)
	}
	resp.Body.Close()

	select {
	case n := <-notifications:
		if n.Rule != "error_rate_spike" || n.Status != "firing" || len(n.Alerts) != 1 ||
			n.Alerts[0].Annotations["cycle"] != "1" {
			t.Fatalf("notification=%#v want error_rate_spike firing cycle 1", n)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timeout waiting for error_rate_spike firing")
	}
	select {
	case n := <-notifications:
		t.Fatalf("unexpected extra notification: %#v", n)
	case <-time.After(100 * time.Millisecond):
	}
	firing, err := store.ProjectFiringPlatformAlertStates(ctx, project.ID)
	if err != nil || len(firing) != 1 || firing[0].ActiveCycle != 1 {
		t.Fatalf("firing platform states=%#v err=%v", firing, err)
	}
}
