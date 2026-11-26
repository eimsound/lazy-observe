package meta

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/eimsound/lazy-observe/internal/db/ent/alertrule"
	"github.com/eimsound/lazy-observe/internal/db/ent/notificationchannel"
)

func TestCommitNativeAlertEvaluationIsAtomicAndFenced(t *testing.T) {
	forEachDialect(t, func(t *testing.T, store *Store) {
		ctx := context.Background()
		project, err := store.client.Project.Create().SetName("Native").SetSlug("native-evaluation").Save(ctx)
		if err != nil {
			t.Fatal(err)
		}
		rule, err := store.client.AlertRule.Create().
			SetProjectID(project.ID).
			SetName("Native rule").
			SetExpr("vector(1)").
			SetEvaluationEngine(alertrule.EvaluationEngineNative).
			Save(ctx)
		if err != nil {
			t.Fatal(err)
		}
		channel, err := store.client.NotificationChannel.Create().
			SetType(notificationchannel.TypeWebhook).
			SetName("native-evaluation").
			SetConfig(map[string]any{"url": "https://example.invalid"}).
			Save(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := store.HeartbeatAlertEngine(ctx, "native-owner", time.Minute); err != nil {
			t.Fatal(err)
		}
		lease, err := store.AcquireAlertRuleLease(ctx, rule.ID, "native-owner", time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		now := time.Now().UTC()
		state := AlertRuleState{
			RuleID:          rule.ID,
			Fingerprint:     "native-fingerprint",
			Status:          "firing",
			FiringSince:     &now,
			LastEvaluatedAt: now,
			Labels:          map[string]string{"alertname": "Native rule"},
			Annotations:     map[string]string{},
		}
		payload := json.RawMessage(`{"project":"native-evaluation","rule":"Native rule","status":"firing","alerts":[]}`)
		ruleID := rule.ID
		input := NotificationOutboxEnqueue{
			IdempotencyKey: "native-atomic",
			ProjectID:      project.ID,
			RuleID:         &ruleID,
			ChannelID:      channel.ID,
			Payload:        payload,
			OrderingKey:    "rule:native",
		}
		if err := store.CommitNativeAlertEvaluation(
			ctx,
			NativeAlertEvaluation{
				RuleID:        rule.ID,
				MemberID:      "native-owner",
				LeaseEpoch:    lease.Epoch,
				States:        []AlertRuleState{state},
				Notifications: []NotificationOutboxEnqueue{input},
			},
		); err != nil {
			t.Fatal(err)
		}
		states, err := store.AlertRuleStates(ctx, rule.ID)
		if err != nil || len(states) != 1 || states[0].Status != "firing" {
			t.Fatalf("states=%#v err=%v", states, err)
		}
		events, err := store.client.AlertEvent.Query().All(ctx)
		if err != nil || len(events) != 1 || string(events[0].Status) != "firing" {
			t.Fatalf("events=%#v err=%v", events, err)
		}
		id, inserted, err := store.EnqueueNotification(ctx, input)
		if err != nil || inserted || id == 0 {
			t.Fatalf("committed outbox id=%d inserted=%v err=%v", id, inserted, err)
		}
		if item, err := store.NotificationOutboxItem(ctx, id); err != nil || item.OrderingKey != "rule:native" {
			t.Fatalf("committed ordering key=%q err=%v", item.OrderingKey, err)
		}
		if err := store.PauseAlertRuleEvaluation(ctx, rule.ID); err != nil {
			t.Fatal(err)
		}
		input.IdempotencyKey = "native-after-pause"
		if err := store.CommitNativeAlertEvaluation(
			ctx,
			NativeAlertEvaluation{
				RuleID:        rule.ID,
				MemberID:      "native-owner",
				LeaseEpoch:    lease.Epoch,
				States:        []AlertRuleState{state},
				Notifications: []NotificationOutboxEnqueue{input},
			},
		); !errors.Is(
			err,
			ErrStaleRuleFence,
		) {
			t.Fatalf("paused native commit error=%v", err)
		}
		if _, inserted, err := store.EnqueueNotification(ctx, input); err != nil || !inserted {
			t.Fatalf("paused commit leaked outbox inserted=%v err=%v", inserted, err)
		}
	})
}

func TestCommitNativeAlertEvaluationPrunesRecoveredAndPreservesInhibited(t *testing.T) {
	forEachDialect(t, func(t *testing.T, store *Store) {
		ctx := context.Background()
		project, err := store.CreateProject(ctx, "Converge", "converge")
		if err != nil {
			t.Fatal(err)
		}
		rule, err := store.CreateAlertRule(ctx, AlertRuleCreate{
			ProjectSlug: project.Slug, Name: "ConvergeRule", Signal: "metrics", Expression: "up == 0", For: "0s",
			Severity: "warning",
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := store.HeartbeatAlertEngine(ctx, "owner-1", time.Minute); err != nil {
			t.Fatal(err)
		}
		lease, err := store.AcquireAlertRuleLease(ctx, rule.ID, "owner-1", time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		t0 := time.Date(2026, 8, 1, 10, 0, 0, 0, time.UTC)
		firingState := AlertRuleState{
			RuleID: rule.ID, Fingerprint: "fp-1", Status: "firing", FiringSince: &t0, LastEvaluatedAt: t0,
			Labels: map[string]string{"alertname": "ConvergeRule"}, Annotations: map[string]string{},
		}
		pendingState := AlertRuleState{
			RuleID: rule.ID, Fingerprint: "fp-pending", Status: "pending", PendingSince: &t0, LastEvaluatedAt: t0,
			Labels: map[string]string{"alertname": "ConvergeRule"}, Annotations: map[string]string{},
		}
		if err := store.CommitNativeAlertEvaluation(ctx, NativeAlertEvaluation{
			RuleID: rule.ID, MemberID: "owner-1", LeaseEpoch: lease.Epoch,
			States:                []AlertRuleState{firingState, pendingState},
			InhibitedFingerprints: map[string]bool{"fp-1": true},
		}); err != nil {
			t.Fatal(err)
		}
		events, err := store.ListAlertEvents(ctx, project.Slug, "", nil)
		if err != nil || len(events) != 1 || !events[0].Inhibited {
			t.Fatalf("initial inhibited event=%#v err=%v", events, err)
		}
		// Subsequent commit with nil InhibitedFingerprints must not clear inhibited=true,
		// and PruneFingerprints must delete vanished pending states.
		firingState.LastEvaluatedAt = t0.Add(5 * time.Second)
		if err := store.CommitNativeAlertEvaluation(ctx, NativeAlertEvaluation{
			RuleID: rule.ID, MemberID: "owner-1", LeaseEpoch: lease.Epoch,
			States:            []AlertRuleState{firingState},
			PruneFingerprints: []string{"fp-pending"},
		}); err != nil {
			t.Fatal(err)
		}
		events, err = store.ListAlertEvents(ctx, project.Slug, "", nil)
		if err != nil || len(events) != 1 || !events[0].Inhibited {
			t.Fatalf("inhibited flag clobbered=%#v err=%v", events, err)
		}
		states, err := store.AlertRuleStates(ctx, rule.ID)
		if err != nil || len(states) != 1 || states[0].Fingerprint != "fp-1" {
			t.Fatalf("pending state not pruned: %#v err=%v", states, err)
		}
		// Transition to recovered: alert_rule_states row is pruned and alert_events records EndsAt.
		resolvedAt := t0.Add(time.Minute)
		recoveredState := firingState
		recoveredState.Status = "recovered"
		recoveredState.LastEvaluatedAt = resolvedAt
		if err := store.CommitNativeAlertEvaluation(ctx, NativeAlertEvaluation{
			RuleID: rule.ID, MemberID: "owner-1", LeaseEpoch: lease.Epoch,
			States: []AlertRuleState{recoveredState},
		}); err != nil {
			t.Fatal(err)
		}
		states, err = store.AlertRuleStates(ctx, rule.ID)
		if err != nil || len(states) != 0 {
			t.Fatalf("recovered state not pruned from alert_rule_states: %#v err=%v", states, err)
		}
		events, err = store.ListAlertEvents(ctx, project.Slug, "", nil)
		if err != nil || len(events) != 1 || events[0].Status != "resolved" || events[0].EndsAt == nil ||
			!events[0].EndsAt.Equal(resolvedAt) {
			t.Fatalf("resolved event=%#v err=%v", events, err)
		}
		origUpdatedAt := events[0].UpdatedAt
		// A duplicate recovered commit must not overwrite EndsAt or UpdatedAt.
		time.Sleep(1100 * time.Millisecond)
		recoveredState.LastEvaluatedAt = resolvedAt.Add(time.Minute)
		if err := store.CommitNativeAlertEvaluation(ctx, NativeAlertEvaluation{
			RuleID: rule.ID, MemberID: "owner-1", LeaseEpoch: lease.Epoch,
			States: []AlertRuleState{recoveredState},
		}); err != nil {
			t.Fatal(err)
		}
		events, err = store.ListAlertEvents(ctx, project.Slug, "", nil)
		if err != nil || len(events) != 1 || events[0].EndsAt == nil || !events[0].EndsAt.Equal(resolvedAt) ||
			!events[0].UpdatedAt.Equal(origUpdatedAt) {
			t.Fatalf(
				"duplicate recovered overwrote timestamps: ends_at=%v want=%v updated_at=%v want=%v",
				events[0].EndsAt,
				resolvedAt,
				events[0].UpdatedAt,
				origUpdatedAt,
			)
		}
	})
}
