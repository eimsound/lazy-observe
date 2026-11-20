package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/eimsound/lazy-observe/internal/alert"
	"github.com/eimsound/lazy-observe/internal/errortrack"
	"github.com/eimsound/lazy-observe/internal/meta"
	"github.com/eimsound/lazy-observe/internal/outbound"
)

func TestPlatformAlertPipelineRealStore(t *testing.T) {
	ctx := context.Background()
	store := testStore(t)
	project, err := store.CreateProject(ctx, "Platform", "platform")
	if err != nil {
		t.Fatal(err)
	}
	token, err := store.CreateProjectAPIToken(ctx, project.ID)
	if err != nil {
		t.Fatal(err)
	}
	member := sessionCookie(t, store, "platform-member", "password", "member")
	viewer := sessionCookie(t, store, "platform-viewer", "password", "viewer")

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
		alert.ChannelInput{Type: "webhook", Name: "platform-test", Config: map[string]any{"url": webhook.URL}},
	)
	if err != nil {
		t.Fatal(err)
	}
	gw := newMetadataTestGateway(
		store,
		Config{Tokens: store, Store: store, Alerts: alerts, Logs: logs.URL},
	)
	t.Cleanup(func() { _ = gw.Shutdown(); alerts.Close() })
	app := gw.App()

	putConfig := func(cookie *http.Cookie, body map[string]any, want int) {
		t.Helper()
		payload, _ := json.Marshal(body)
		req := httptest.NewRequest(
			http.MethodPut,
			fmt.Sprintf("/api/projects/%d/alert-config", project.ID),
			bytes.NewReader(payload),
		)
		req.Header.Set("Content-Type", "application/json")
		req.AddCookie(cookie)
		resp, requestErr := app.Test(req)
		if requestErr != nil || resp.StatusCode != want {
			t.Fatalf("put alert config: status=%d want=%d err=%v", resp.StatusCode, want, requestErr)
		}
		resp.Body.Close()
	}
	config := func(newIssue, regression bool, threshold int, channels []int) map[string]any {
		return map[string]any{
			"new_issue":                    newIssue,
			"regression":                   regression,
			"error_rate_threshold_per_min": threshold,
			"channel_ids":                  channels,
		}
	}
	putConfig(member, config(true, true, 0, []int{channel.ID}), http.StatusOK)
	putConfig(viewer, config(true, true, 0, []int{channel.ID}), http.StatusForbidden)

	postEventAt := func(id, typ, value string, occurredAt time.Time) {
		t.Helper()
		event := map[string]any{
			"event_id":  id,
			"timestamp": occurredAt,
			"platform":  "go",
			"level":     "error",
			"exception": map[string]any{
				"type":  typ,
				"value": value,
				"stacktrace": map[string]any{
					"frames": []map[string]any{{"function": "checkout", "filename": "checkout.go", "in_app": true}},
				},
			},
		}
		payload, _ := json.Marshal(map[string]any{"events": []any{event}})
		req := httptest.NewRequest(http.MethodPost, "/api/events/errors", bytes.NewReader(payload))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+token)
		resp, requestErr := app.Test(req)
		if requestErr != nil || resp.StatusCode != http.StatusOK {
			t.Fatalf("post error: status=%d err=%v", resp.StatusCode, requestErr)
		}
		resp.Body.Close()
	}
	postEvent := func(id, typ, value string) {
		postEventAt(id, typ, value, time.Now().UTC())
	}
	waitKind := func(kind string) alert.Notification {
		t.Helper()
		select {
		case n := <-notifications:
			if n.Rule != kind || len(n.Alerts) == 0 || n.Alerts[0].Labels["source"] != "platform" ||
				n.Alerts[0].Labels["kind"] != kind {
				t.Fatalf("notification=%#v want kind=%s", n, kind)
			}
			return n
		case <-time.After(2 * time.Second):
			t.Fatalf("timeout waiting for %s", kind)
			return alert.Notification{}
		}
	}
	assertQuiet := func() {
		t.Helper()
		select {
		case n := <-notifications:
			t.Fatalf("unexpected notification: %#v", n)
		case <-time.After(80 * time.Millisecond):
		}
	}

	postEvent("new-1", "CheckoutError", "card declined")
	newIssue := waitKind("new_issue")
	if newIssue.Alerts[0].Annotations["title"] != "CheckoutError: card declined" ||
		newIssue.Alerts[0].Annotations["link"] == "" {
		t.Fatalf("new issue annotations=%#v", newIssue.Alerts[0].Annotations)
	}
	issueID, err := strconv.Atoi(newIssue.Alerts[0].Labels["issue_id"])
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.SetIssueStatus(ctx, project.ID, issueID, "resolved", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	postEvent("regression-1", "CheckoutError", "declined again")
	waitKind("regression")

	putConfig(member, config(false, false, 1, []int{channel.ID}), http.StatusOK)
	for i := 0; i < 6; i++ {
		postEventAt(fmt.Sprintf("old-spike-%d", i), "CheckoutError", "old backlog", time.Now().Add(-10*time.Minute))
	}
	assertQuiet()
	for i := 0; i < 6; i++ {
		postEvent(fmt.Sprintf("spike-%d", i), "CheckoutError", "burst")
	}
	waitKind("error_rate_spike")
	postEvent("spike-6", "CheckoutError", "burst")
	postEvent("spike-7", "CheckoutError", "burst")
	assertQuiet()

	putConfig(member, config(false, true, 0, []int{channel.ID}), http.StatusOK)
	postEvent("disabled", "DisabledIssue", "must not notify")
	assertQuiet()
	putConfig(member, config(true, true, 0, []int{}), http.StatusOK)
	postEvent("empty-channels", "EmptyChannelsIssue", "must not notify")
	assertQuiet()

	putConfig(member, config(true, true, 0, []int{channel.ID}), http.StatusOK)
	silence, err := alerts.CreateSilence(
		ctx,
		alert.SilenceInput{
			Project: project.Slug,
			Matchers: []alert.Matcher{
				{Name: "source", Op: "=", Value: "platform"},
				{Name: "kind", Op: "=", Value: "new_issue"},
			},
			StartsAt: time.Now().Add(-time.Minute),
			EndsAt:   time.Now().Add(time.Minute),
			Creator:  "test",
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	postEvent("silenced", "SilencedIssue", "must not notify")
	assertQuiet()
	if err := alerts.DeleteSilence(ctx, project.Slug, silence.ID); err != nil {
		t.Fatal(err)
	}

	var got projectAlertConfigResponse
	req := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/api/projects/%d/alert-config", project.ID), nil)
	req.AddCookie(viewer)
	resp, err := app.Test(req)
	if err != nil || resp.StatusCode != http.StatusOK || json.NewDecoder(resp.Body).Decode(&got) != nil {
		t.Fatalf("viewer get config: status=%d err=%v", resp.StatusCode, err)
	}
	resp.Body.Close()
	if len(got.ChannelIDs) != 1 || got.ChannelIDs[0] != strconv.Itoa(channel.ID) {
		t.Fatalf("alert config=%#v", got)
	}

	// Verify the migration and lifecycle changes were persisted in real SQLite.
	issue, err := store.GetIssue(ctx, project.ID, issueID)
	if err != nil || issue.Status != "open" || issue.RegressedAt == nil {
		t.Fatalf("persisted regression=%#v err=%v", issue, err)
	}
}

func TestDurableIssueLifecycleSurvivesCrashAndDeduplicatesRedelivery(t *testing.T) {
	ctx := context.Background()
	store := testStore(t)
	project, err := store.CreateProject(ctx, "DurableAlert", "durable-alert")
	if err != nil {
		t.Fatal(err)
	}
	notifications := make(chan alert.Notification, 8)
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
		// Use a long GroupWait to prove issue lifecycle events are durably enqueued
		// to notification_outbox synchronously without waiting on an in-memory timer.
		GroupWait:      time.Hour,
		RepeatInterval: time.Hour,
	})
	defer alerts.Close()
	channel, err := alerts.CreateChannel(
		ctx,
		alert.ChannelInput{Type: "webhook", Name: "durable-hook", Config: map[string]any{"url": webhook.URL}},
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := alerts.PutProjectAlertConfig(ctx, project.ID, alert.ProjectAlertConfigInput{
		NewIssue:   true,
		Regression: true,
		ChannelIDs: []int{channel.ID},
	}); err != nil {
		t.Fatal(err)
	}

	// Simulate worker 1 committing CompleteErrorEvent and crashing before dispatching
	// any lifecycle alert (OnChange == nil).
	writer := &blockedErrorWriter{started: make(chan struct{}, 1), release: make(chan struct{})}
	close(writer.release)
	first, err := errortrack.NewInbox(store, writer, errortrack.InboxOptions{
		PollInterval: time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Millisecond)
	event := errortrack.Event{
		EventID:   "crash-new-1",
		Timestamp: now,
		Platform:  "go",
		Level:     "error",
		Exception: errortrack.Exception{
			Type:  "CrashSafeError",
			Value: "must not drop alert",
			Stacktrace: errortrack.Stacktrace{
				Frames: []errortrack.Frame{{Function: "svc.Run", Filename: "svc.go", InApp: true}},
			},
		},
	}
	raw, _ := json.Marshal(event)
	if err := first.Accept(
		ctx,
		errortrack.Project{ID: project.ID, Slug: project.Slug},
		[]errortrack.AcceptedEvent{{Raw: raw, Event: event}},
	); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		diag, diagErr := first.Diagnostics(ctx)
		if diagErr != nil {
			t.Fatal(diagErr)
		}
		if diag.ActiveItems == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("first inbox did not complete event: %#v", diag)
		}
		time.Sleep(time.Millisecond)
	}
	if err := first.Close(ctx); err != nil {
		t.Fatal(err)
	}

	// Worker 2 restarts with alerts.HandleIssueChange and delivers the persisted
	// lifecycle event immediately into notification_outbox despite GroupWait=1h.
	var deliveredChange errortrack.IssueChange
	second, err := errortrack.NewInbox(store, writer, errortrack.InboxOptions{
		PollInterval: time.Millisecond,
		OnChange: func(cbCtx context.Context, change errortrack.IssueChange) error {
			deliveredChange = change
			return alerts.HandleIssueChange(cbCtx, change)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = second.Close(context.Background()) }()

	select {
	case n := <-notifications:
		if n.Rule != "new_issue" || len(n.Alerts) != 1 ||
			n.Alerts[0].Annotations["title"] != "CrashSafeError: must not drop alert" {
			t.Fatalf("recovered notification=%#v", n)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for recovered durable lifecycle alert")
	}
	if deliveredChange.LifecycleID <= 0 {
		t.Fatalf("expected non-zero LifecycleID on recovered change, got %#v", deliveredChange)
	}

	// Redelivering the exact same LifecycleID (simulating crash between outbox enqueue
	// and CompleteIssueLifecycleEvent) must deduplicate in notification_outbox.
	if err := alerts.HandleIssueChange(ctx, deliveredChange); err != nil {
		t.Fatalf("idempotent redelivery failed: %v", err)
	}
	outboxRows, err := store.ListNotificationOutbox(ctx, meta.NotificationOutboxListOptions{
		ProjectID: project.ID,
		Limit:     10,
	})
	if err != nil || len(outboxRows) != 1 {
		t.Fatalf("expected exactly 1 deduplicated outbox row, got %#v err=%v", outboxRows, err)
	}
}
