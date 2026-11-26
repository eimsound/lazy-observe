package alert

import (
	"bytes"
	"context"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/eimsound/lazy-observe/internal/errortrack"
	"github.com/eimsound/lazy-observe/internal/meta"
)

func TestIssueAIAnalysisIsAttachedToPlatformNotification(t *testing.T) {
	ctx := context.Background()
	store, err := meta.OpenStore(ctx, filepath.Join(t.TempDir(), "meta.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	project, err := store.CreateProject(ctx, "Alpha", "alpha")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	changes, err := store.ApplyIssueDeltas(ctx, []errortrack.IssueDelta{{
		ProjectID: project.ID, Fingerprint: "ai-notification", Title: "CheckoutError", Level: "error",
		Count: 1, FirstSeen: now, LastSeen: now, LastEventID: "event-1",
		AcceptedAt: now, AppliedAt: now,
	}})
	if err != nil || len(changes) != 1 {
		t.Fatalf("changes=%#v err=%v", changes, err)
	}
	if _, err := store.SaveIssueAnalysis(ctx, project.ID, changes[0].IssueID, map[string]any{
		"summary": "database latency likely caused checkout failures",
		"probable_causes": []any{
			map[string]any{"cause": "database latency", "confidence": "high", "evidence": "trace timeout"},
		},
		"suggested_actions": []any{"inspect database latency"},
	}, now); err != nil {
		t.Fatal(err)
	}
	service := NewService(Config{Store: store})
	defer service.Close()
	n := Notification{Project: "alpha", Rule: "new_issue", Status: "firing", Alerts: []Alert{{
		Labels:      map[string]string{"source": "platform", "issue_id": strconv.Itoa(changes[0].IssueID)},
		Annotations: map[string]string{"summary": "CheckoutError"},
	}}}
	attached := service.attachIssueAIAnalysis(ctx, n)
	annotations := attached.Alerts[0].Annotations
	if annotations["ai_summary"] != "database latency likely caused checkout failures" {
		t.Fatalf("ai_summary = %q", annotations["ai_summary"])
	}
	for _, key := range []string{"ai_probable_causes", "ai_suggested_actions"} {
		if _, leaked := annotations[key]; leaked {
			t.Fatalf("notification must carry only ai_summary, got %q: %#v", key, annotations)
		}
	}
	if n.Alerts[0].Annotations["ai_summary"] != "" {
		t.Fatal("attachment mutated the engine-owned annotation map")
	}

	// A regression must not reuse the analysis captured for the original
	// issue: it may no longer describe the new failure.
	regression := n
	regression.Rule = "regression"
	if got := service.attachIssueAIAnalysis(ctx, regression).Alerts[0].Annotations; got["ai_summary"] != "" {
		t.Fatalf("regression notification carried stale AI analysis: %#v", got)
	}
}

func TestChannelPayloadIncludesOnlyIssueAISummary(t *testing.T) {
	n := Notification{Project: "alpha", Rule: "new_issue", Status: "firing", Alerts: []Alert{{
		Labels: map[string]string{"source": "platform", "kind": "new_issue", "issue_id": "1", "level": "error"},
		Annotations: map[string]string{
			"summary":    "CheckoutError",
			"ai_summary": "checkout database timeout",
			// Legacy outbox rows may still carry these; IM channels ignore them.
			"ai_probable_causes":   `[{"cause":"database latency"}]`,
			"ai_suggested_actions": `["inspect database"]`,
		},
	}}}
	for _, kind := range []string{"feishu", "dingtalk", "wecom", "slack"} {
		payload, err := ChannelPayload(kind, n)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Contains(payload, []byte("AI analysis: checkout database timeout")) {
			t.Errorf("%s payload missing AI summary: %s", kind, payload)
		}
		for _, unwanted := range [][]byte{[]byte("database latency"), []byte("inspect database"), []byte("issue_id")} {
			if bytes.Contains(payload, unwanted) {
				t.Errorf("%s payload leaked %q: %s", kind, unwanted, payload)
			}
		}
	}
}
