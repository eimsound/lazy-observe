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
