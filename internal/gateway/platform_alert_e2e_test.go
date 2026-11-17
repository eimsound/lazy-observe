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

	alerts := alert.NewService(alert.Config{Store: store, GroupWait: 10 * time.Millisecond, RepeatInterval: time.Hour})
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
