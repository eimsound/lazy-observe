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
	for _, want := range []string{"database latency likely caused checkout failures", "database latency", "inspect database latency"} {
		joined := annotations["ai_summary"] + annotations["ai_probable_causes"] + annotations["ai_suggested_actions"]
		if !bytes.Contains([]byte(joined), []byte(want)) {
			t.Fatalf("AI notification annotations missing %q: %#v", want, annotations)
		}
	}
	if n.Alerts[0].Annotations["ai_summary"] != "" {
		t.Fatal("attachment mutated the engine-owned annotation map")
	}
}

func TestChannelPayloadIncludesIssueAIAnalysis(t *testing.T) {
	n := Notification{Project: "alpha", Rule: "new_issue", Status: "firing", Alerts: []Alert{{
		Labels: map[string]string{"source": "platform", "issue_id": "1"},
		Annotations: map[string]string{
			"ai_summary":           "checkout database timeout",
			"ai_probable_causes":   `[{"cause":"database latency"}]`,
			"ai_suggested_actions": `["inspect database"]`,
		},
	}}}
	for _, kind := range []string{"webhook", "feishu", "dingtalk", "wecom"} {
		payload, err := ChannelPayload(kind, n)
		if err != nil {
			t.Fatal(err)
		}
		for _, want := range [][]byte{[]byte("checkout database timeout"), []byte("database latency"), []byte("inspect database")} {
			if !bytes.Contains(payload, want) {
				t.Errorf("%s payload missing %q: %s", kind, want, payload)
			}
		}
	}
}
