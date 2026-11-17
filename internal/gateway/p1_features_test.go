package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/eimsound/lazy-observe/internal/errortrack"
	"github.com/eimsound/lazy-observe/internal/meta"
)

func mustCreateIssue(t *testing.T, store *meta.Store, projectID int, fingerprint, title string) int {
	t.Helper()
	now := time.Now()
	changes, err := store.ApplyIssueDeltas(context.Background(), []errortrack.IssueDelta{{
		ProjectID: projectID, Fingerprint: fingerprint, Title: title, Level: "error", Count: 1,
		FirstSeen: now, LastSeen: now, LastEventID: "evt-" + fingerprint,
		AcceptedAt: now, AppliedAt: now,
	}})
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 1 {
		t.Fatalf("issue changes=%+v", changes)
	}
	return changes[0].IssueID
}

func TestIssueBatchStatusUpdate(t *testing.T) {
	store := testStore(t)
	project, err := store.CreateProject(t.Context(), "Batch", "batch")
	if err != nil {
		t.Fatal(err)
	}
	first := mustCreateIssue(t, store, project.ID, "fp-1", "First")
	second := mustCreateIssue(t, store, project.ID, "fp-2", "Second")
	otherProject, err := store.CreateProject(t.Context(), "Other", "other")
	if err != nil {
		t.Fatal(err)
	}
	foreign := mustCreateIssue(t, store, otherProject.ID, "fp-3", "Foreign")
	cookie := sessionCookie(t, store, "batcher", "password", "member")
	app := newMetadataTestGateway(store, Config{Store: store, Tokens: store}).App()

	body, _ := json.Marshal(map[string]any{"ids": []int{first, second, foreign}, "status": "resolved"})
	req := httptest.NewRequest(http.MethodPost, "/api/issues/batch", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-LO-Project", "batch")
	req.AddCookie(cookie)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("batch status=%d", resp.StatusCode)
	}
	var result struct {
		Updated int `json:"updated"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}
	if result.Updated != 2 {
		t.Fatalf("updated=%d want 2 (foreign issue must be ignored)", result.Updated)
	}
	row, err := store.GetIssue(t.Context(), otherProject.ID, foreign)
	if err != nil {
		t.Fatal(err)
	}
	if row.Status != "open" {
		t.Fatalf("cross-project issue changed to %q", row.Status)
	}
}

func TestNotificationHistoryAndRequeue(t *testing.T) {
	store := testStore(t)
	project, err := store.CreateProject(t.Context(), "Notify", "notify")
	if err != nil {
		t.Fatal(err)
	}
	channel, err := store.CreateNotificationChannel(t.Context(), meta.NotificationChannelCreate{
		Type: "webhook", Name: "webhook-1", Config: map[string]any{"url": "http://example.invalid/hook"},
	})
	if err != nil {
		t.Fatal(err)
	}
	enqueue := func(status string, ruleID *int) int64 {
		t.Helper()
		payload, _ := json.Marshal(map[string]string{"title": "Test"})
		id, inserted, err := store.EnqueueNotification(t.Context(), meta.NotificationOutboxEnqueue{
			IdempotencyKey: status + "-key-" + time.Now().Format("150405.000000000"),
			ProjectID:      project.ID,
			RuleID:         ruleID,
			ChannelID:      channel.ID,
			Payload:        payload,
		})
		if err != nil || !inserted {
			t.Fatalf("enqueue: inserted=%v err=%v", inserted, err)
		}
		if _, err := store.RequeueNotification(t.Context(), id, project.ID); err != nil {
			t.Fatal(err)
		}
		return id
	}
	enqueue("pending", nil)
	_ = enqueue

	cookie := sessionCookie(t, store, "notifier", "password", "member")
	app := newMetadataTestGateway(store, Config{Store: store, Tokens: store}).App()

	req := httptest.NewRequest(http.MethodGet, "/api/notifications", nil)
	req.Header.Set("X-LO-Project", "notify")
	req.AddCookie(cookie)
	resp, err := app.Test(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("list notifications: status=%d err=%v", resp.StatusCode, err)
	}
	var items []meta.NotificationOutboxView
	if err := json.NewDecoder(resp.Body).Decode(&items); err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 {
		t.Fatalf("notification items = %d, want 1", len(items))
	}
	if items[0].ChannelName != "webhook-1" || items[0].ChannelType != "webhook" {
		t.Fatalf("channel join wrong: %#v", items[0])
	}
	if items[0].Status != "pending" {
		t.Fatalf("requeued item status = %q, want pending", items[0].Status)
	}
}
