package meta

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/eimsound/lazy-observe/internal/secret"
)

func TestAlertRulePersistenceListAndCreateAreProjectScoped(t *testing.T) {
	forEachDialect(t, func(t *testing.T, store *Store) {
		ctx := context.Background()
		if _, err := store.CreateProject(ctx, "Alpha", "alpha"); err != nil {
			t.Fatal(err)
		}
		if _, err := store.CreateProject(ctx, "Beta", "beta"); err != nil {
			t.Fatal(err)
		}
		created, err := store.CreateAlertRule(ctx, AlertRuleCreate{
			ProjectSlug: "alpha",
			Name:        "InstanceDown",
			Signal:      "metrics",
			Expression:  "up == 0",
			For:         "1m",
			Severity:    "critical",
		})
		if err != nil {
			t.Fatal(err)
		}
		if created.Project.Slug != "alpha" || created.Name != "InstanceDown" {
			t.Fatalf("created rule = %+v", created)
		}
		alpha, err := store.ListAlertRules(ctx, "alpha")
		if err != nil {
			t.Fatal(err)
		}
		if len(alpha) != 1 || alpha[0].ID != created.ID {
			t.Fatalf("alpha rules = %+v", alpha)
		}
		beta, err := store.ListAlertRules(ctx, "beta")
		if err != nil {
			t.Fatal(err)
		}
		if len(beta) != 0 {
			t.Fatalf("beta rules = %+v", beta)
		}
		name := "CrossProject"
		if _, err := store.UpdateAlertRule(
			ctx,
			"beta",
			created.ID,
			AlertRuleUpdate{Name: &name},
		); !IsNotFound(err) {
			t.Fatalf("cross-project rule update error=%v, want not found", err)
		}
		if err := store.DeleteAlertRule(ctx, "beta", created.ID); !IsNotFound(err) {
			t.Fatalf("cross-project rule delete error=%v, want not found", err)
		}
		preserved, err := store.AlertRule(ctx, "alpha", created.ID)
		if err != nil || preserved.Name != "InstanceDown" {
			t.Fatalf("alpha rule after cross-project writes=%+v err=%v", preserved, err)
		}
		byID, err := store.AlertRuleByID(ctx, created.ID)
		if err != nil || byID.ID != created.ID || byID.Project.Slug != "alpha" || byID.Name != "InstanceDown" {
			t.Fatalf("AlertRuleByID=%+v err=%v", byID, err)
		}
		if _, err := store.AlertRuleByID(ctx, created.ID+999999); !IsNotFound(err) {
			t.Fatalf("missing AlertRuleByID error=%v, want not found", err)
		}
	})
}

func TestUpdateAlertRulePersistenceIsAtomic(t *testing.T) {
	forEachDialect(t, func(t *testing.T, store *Store) {
		ctx := context.Background()
		if _, err := store.CreateProject(ctx, "Alpha", "alpha"); err != nil {
			t.Fatal(err)
		}
		created, err := store.CreateAlertRule(ctx, AlertRuleCreate{
			ProjectSlug: "alpha",
			Name:        "Original",
			Signal:      "metrics",
			Expression:  "up == 0",
			For:         "0s",
			Severity:    "warning",
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := store.PauseAlertRuleEvaluation(ctx, created.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := store.CreateAlertEvent(ctx, AlertEventCreate{
			RuleID:      created.ID,
			Fingerprint: "atomic-update",
			Status:      "firing",
			Labels:      map[string]string{"alertname": "Original"},
			Annotations: map[string]string{},
			StartsAt:    time.Now().UTC(),
		}); err != nil {
			t.Fatal(err)
		}
		name := "Changed"
		missingChannels := []int{999999}
		_, err = store.UpdateAlertRule(ctx, "alpha", created.ID, AlertRuleUpdate{
			Name:               &name,
			ChannelIDs:         &missingChannels,
			ActivateEvaluation: true,
			ResolveFiring:      true,
		})
		if err == nil {
			t.Fatal("update unexpectedly succeeded")
		}
		if errors.Is(err, context.Canceled) {
			t.Fatalf("unexpected error: %v", err)
		}
		unchanged, err := store.AlertRule(ctx, "alpha", created.ID)
		if err != nil {
			t.Fatal(err)
		}
		if unchanged.Name != "Original" || unchanged.EvaluationEngine != "paused" {
			t.Fatalf("partial update persisted: %+v", unchanged)
		}
		events, err := store.ListAlertEvents(ctx, "alpha", "", nil)
		if err != nil {
			t.Fatal(err)
		}
		if len(events) != 1 || events[0].Status != "firing" || events[0].EndsAt != nil {
			t.Fatalf("firing event was partially resolved: %+v", events)
		}
	})
}

func TestAlertEvaluationSnapshotIsProjectScoped(t *testing.T) {
	forEachDialect(t, func(t *testing.T, store *Store) {
		ctx := context.Background()
		for _, project := range []struct{ name, slug string }{{"Alpha", "alpha"}, {"Beta", "beta"}} {
			if _, err := store.CreateProject(ctx, project.name, project.slug); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := store.CreateAlertRule(ctx, AlertRuleCreate{
			ProjectSlug: "alpha",
			Name:        "AlphaRule",
			Signal:      "metrics",
			Expression:  "up == 0",
			For:         "0s",
			Severity:    "warning",
		}); err != nil {
			t.Fatal(err)
		}
		disabled := false
		if _, err := store.CreateAlertRule(ctx, AlertRuleCreate{
			ProjectSlug: "beta",
			Name:        "BetaRule",
			Signal:      "logs",
			Expression:  "error",
			For:         "0s",
			Severity:    "warning",
			Enabled:     &disabled,
		}); err != nil {
			t.Fatal(err)
		}
		rules, err := store.AlertEvaluationRules(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if len(rules) != 1 || rules[0].Project.Slug != "alpha" || rules[0].Name != "AlphaRule" {
			t.Fatalf("evaluation rules = %+v", rules)
		}
	})
}

func TestNotificationChannelPatchPreservesUnspecifiedConfig(t *testing.T) {
	forEachDialect(t, func(t *testing.T, store *Store) {
		ctx := context.Background()
		channel, err := store.CreateNotificationChannel(ctx, NotificationChannelCreate{
			Type: "webhook", Name: "primary", Config: map[string]any{"url": "https://example.test/hook"},
		})
		if err != nil {
			t.Fatal(err)
		}
		name := "renamed"
		updated, err := store.UpdateNotificationChannel(ctx, channel.ID, NotificationChannelUpdate{Name: &name})
		if err != nil {
			t.Fatal(err)
		}
		if updated.Name != "renamed" || updated.Config["url"] != "https://example.test/hook" {
			t.Fatalf("updated channel = %+v", updated)
		}
		loaded, err := store.NotificationChannel(ctx, channel.ID)
		if err != nil {
			t.Fatal(err)
		}
		if loaded.Config["url"] != "https://example.test/hook" {
			t.Fatalf("loaded channel = %+v", loaded)
		}
	})
}

func TestNotificationChannelConfigEncryptedAtRest(t *testing.T) {
	box, err := secret.LoadOrCreate(t.TempDir() + "/secret.key")
	if err != nil {
		t.Fatal(err)
	}
	forEachDialect(t, func(t *testing.T, store *Store) {
		store.SetSecretBox(box)
		ctx := context.Background()
		channel, err := store.CreateNotificationChannel(ctx, NotificationChannelCreate{
			Type: "dingtalk",
			Name: "encrypted-dingtalk",
			Config: map[string]any{
				"url":      "https://oapi.dingtalk.com/robot/send?access_token=ding-plain-secret",
				"secret":   "SEC-signing-secret",
				"password": "smtp-plain-password",
				"headers":  map[string]any{"Authorization": "Bearer header-plain-secret"},
				"host":     "smtp.example.com",
			},
		})
		if err != nil {
			t.Fatal(err)
		}

		// Raw database column must store ciphertext, never plaintext secrets.
		var rawConfig string
		if err := store.db.QueryRowContext(
			ctx,
			bind(store.dialect, `SELECT config FROM notification_channels WHERE id = ?`),
			channel.ID,
		).Scan(&rawConfig); err != nil {
			t.Fatal(err)
		}
		for _, secretVal := range []string{"ding-plain-secret", "SEC-signing-secret", "smtp-plain-password", "header-plain-secret"} {
			if strings.Contains(rawConfig, secretVal) {
				t.Fatalf("raw DB config leaked %q: %s", secretVal, rawConfig)
			}
		}
		if !strings.Contains(rawConfig, channelConfigCipherPrefix) {
			t.Fatalf("raw DB config missing %q prefix: %s", channelConfigCipherPrefix, rawConfig)
		}

		// Decrypted view returns exact plaintext for internal delivery.
		loaded, err := store.NotificationChannel(ctx, channel.ID)
		if err != nil {
			t.Fatal(err)
		}
		if loaded.Config["url"] != "https://oapi.dingtalk.com/robot/send?access_token=ding-plain-secret" ||
			loaded.Config["secret"] != "SEC-signing-secret" ||
			loaded.Config["password"] != "smtp-plain-password" ||
			loaded.Config["host"] != "smtp.example.com" {
			t.Fatalf("decrypted channel config = %#v", loaded.Config)
		}
		headers, ok := loaded.Config["headers"].(map[string]any)
		if !ok || headers["Authorization"] != "Bearer header-plain-secret" {
			t.Fatalf("decrypted headers = %#v", loaded.Config["headers"])
		}
	})
}

func TestProjectAlertConfigIsProjectScoped(t *testing.T) {
	forEachDialect(t, func(t *testing.T, store *Store) {
		ctx := context.Background()
		alpha, err := store.CreateProject(ctx, "Alpha", "alpha")
		if err != nil {
			t.Fatal(err)
		}
		beta, err := store.CreateProject(ctx, "Beta", "beta")
		if err != nil {
			t.Fatal(err)
		}
		channel, err := store.CreateNotificationChannel(ctx, NotificationChannelCreate{
			Type: "webhook", Name: "primary", Config: map[string]any{"url": "https://example.test/hook"},
		})
		if err != nil {
			t.Fatal(err)
		}
		alphaConfig, err := store.PutProjectAlertConfig(ctx, alpha.ID, ProjectAlertConfigUpdate{
			NewIssue: true, Regression: false, ErrorRateThresholdPerMinute: 7, ChannelIDs: []int{channel.ID},
		})
		if err != nil {
			t.Fatal(err)
		}
		if len(alphaConfig.ChannelIDs) != 1 || alphaConfig.ChannelIDs[0] != channel.ID {
			t.Fatalf("alpha config = %+v", alphaConfig)
		}
		betaConfig, err := store.ProjectAlertConfig(ctx, beta.ID)
		if err != nil {
			t.Fatal(err)
		}
		if !betaConfig.NewIssue || !betaConfig.Regression || len(betaConfig.ChannelIDs) != 0 {
			t.Fatalf("beta config = %+v", betaConfig)
		}
	})
}

func TestSilenceAndInhibitionPersistenceIsProjectScoped(t *testing.T) {
	forEachDialect(t, func(t *testing.T, store *Store) {
		ctx := context.Background()
		for _, project := range []struct{ name, slug string }{{"Alpha", "alpha"}, {"Beta", "beta"}} {
			if _, err := store.CreateProject(ctx, project.name, project.slug); err != nil {
				t.Fatal(err)
			}
		}
		now := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
		var alphaSilence SilenceView
		var alphaInhibition InhibitionRuleView
		for _, slug := range []string{"alpha", "beta"} {
			createdSilence, err := store.CreateSilence(ctx, SilenceCreate{
				ProjectSlug: slug,
				Matchers:    []map[string]string{{"name": "alertname", "op": "=", "value": "Down"}},
				StartsAt:    now.Add(-time.Minute),
				EndsAt:      now.Add(time.Minute),
				Creator:     "test",
			})
			if err != nil {
				t.Fatal(err)
			}
			createdInhibition, err := store.CreateInhibitionRule(ctx, InhibitionRuleCreate{
				ProjectSlug:    slug,
				SourceMatchers: []map[string]string{{"name": "role", "op": "=", "value": "host"}},
				TargetMatchers: []map[string]string{{"name": "role", "op": "=", "value": "service"}},
				EqualLabels:    []string{"instance"},
			})
			if err != nil {
				t.Fatal(err)
			}
			if slug == "alpha" {
				alphaSilence = createdSilence
				alphaInhibition = createdInhibition
			}
		}
		if _, err := store.CreateSilence(ctx, SilenceCreate{
			ProjectSlug: "alpha",
			Matchers:    []map[string]string{{"name": "alertname", "op": "=", "value": "Expired"}},
			StartsAt:    now.Add(-2 * time.Minute),
			EndsAt:      now.Add(-time.Minute),
			Creator:     "test",
		}); err != nil {
			t.Fatal(err)
		}
		comment := "cross-project"
		if _, err := store.UpdateSilence(
			ctx,
			"beta",
			alphaSilence.ID,
			SilenceUpdate{Comment: &comment},
		); !IsNotFound(
			err,
		) {
			t.Fatalf("cross-project silence update error=%v, want not found", err)
		}
		if err := store.DeleteSilence(ctx, "beta", alphaSilence.ID); !IsNotFound(err) {
			t.Fatalf("cross-project silence delete error=%v, want not found", err)
		}
		if _, err := store.UpdateInhibitionRule(
			ctx,
			"beta",
			alphaInhibition.ID,
			InhibitionRuleUpdate{Comment: &comment},
		); !IsNotFound(err) {
			t.Fatalf("cross-project inhibition update error=%v, want not found", err)
		}
		if _, err := store.DeleteInhibitionRule(ctx, "beta", alphaInhibition.ID); !IsNotFound(err) {
			t.Fatalf("cross-project inhibition delete error=%v, want not found", err)
		}
		silences, err := store.ListSilences(ctx, "alpha")
		if err != nil || len(silences) != 2 || silences[0].ProjectSlug != "alpha" ||
			silences[1].ProjectSlug != "alpha" {
			t.Fatalf("alpha silences=%+v err=%v", silences, err)
		}
		inhibitions, err := store.ListInhibitionRules(ctx, "beta")
		if err != nil || len(inhibitions) != 1 || inhibitions[0].ProjectSlug != "beta" {
			t.Fatalf("beta inhibitions=%+v err=%v", inhibitions, err)
		}
		policy, err := store.AlertPolicySnapshot(ctx, silences[0].ProjectID, now)
		if err != nil || len(policy.Silences) != 1 || len(policy.InhibitionRules) != 1 {
			t.Fatalf("alpha policy=%+v err=%v", policy, err)
		}
	})
}

func TestUpdateAlertRuleResetsStateAndFencesInFlightEvaluation(t *testing.T) {
	forEachDialect(t, func(t *testing.T, store *Store) {
		ctx := context.Background()
		project, err := store.CreateProject(ctx, "Fence", "fence")
		if err != nil {
			t.Fatal(err)
		}
		channel, err := store.CreateNotificationChannel(ctx, NotificationChannelCreate{
			Type: "webhook", Name: "fence-hook", Config: map[string]any{"url": "https://example.invalid/hook"},
		})
		if err != nil {
			t.Fatal(err)
		}
		rule, err := store.CreateAlertRule(ctx, AlertRuleCreate{
			ProjectSlug: "fence", Name: "HighErr", Signal: "metrics", Expression: "up == 0", For: "0s",
			Severity: "critical", ChannelIDs: []int{channel.ID},
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := store.HeartbeatAlertEngine(ctx, "engine-a", time.Minute); err != nil {
			t.Fatal(err)
		}
		lease, err := store.AcquireAlertRuleLease(ctx, rule.ID, "engine-a", time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		now := time.Now().UTC()
		state := AlertRuleState{
			RuleID: rule.ID, Fingerprint: "fp-a", Status: "firing", FiringSince: &now, LastEvaluatedAt: now,
			Labels: map[string]string{"alertname": "HighErr"}, Annotations: map[string]string{},
		}
		if err := store.CommitNativeAlertEvaluation(ctx, NativeAlertEvaluation{
			RuleID: rule.ID, MemberID: "engine-a", LeaseEpoch: lease.Epoch, States: []AlertRuleState{state},
		}); err != nil {
			t.Fatal(err)
		}

		// Updating expression must clear alert_rule_states, resolve firing
		// alert_events, atomically enqueue resolved notifications via the
		// callback, and advance the lease epoch to fence in-flight commits.
		newExpr := "up == 2"
		ruleID := rule.ID
		if _, err := store.UpdateAlertRule(ctx, "fence", rule.ID, AlertRuleUpdate{
			Expression: &newExpr,
			BuildResolveNotifications: func(
				resolved []AlertRuleState,
				enabledChannelIDs []int,
				_ time.Time,
			) ([]NotificationOutboxEnqueue, error) {
				if len(resolved) != 1 || resolved[0].Fingerprint != "fp-a" ||
					len(enabledChannelIDs) != 1 || enabledChannelIDs[0] != channel.ID {
					return nil, errors.New("unexpected firing states or channels passed to resolve builder")
				}
				return []NotificationOutboxEnqueue{{
					IdempotencyKey: "update-rule-resolved-1",
					ProjectID:      project.ID,
					RuleID:         &ruleID,
					ChannelID:      channel.ID,
					Payload:        json.RawMessage(`{"status":"resolved","rule":"HighErr"}`),
					Priority:       100,
				}}, nil
			},
		}); err != nil {
			t.Fatal(err)
		}
		states, err := store.AlertRuleStates(ctx, rule.ID)
		if err != nil || len(states) != 0 {
			t.Fatalf("states after expr change=%#v err=%v", states, err)
		}
		events, err := store.ListAlertEvents(ctx, "fence", "", nil)
		if err != nil || len(events) != 1 || events[0].Status != "resolved" {
			t.Fatalf("events after expr change=%#v err=%v", events, err)
		}
		outbox, err := store.ListNotificationOutbox(ctx, NotificationOutboxListOptions{ProjectID: project.ID})
		if err != nil || len(outbox) != 1 || outbox[0].Priority != 100 {
			t.Fatalf("resolved outbox after expr change=%#v err=%v", outbox, err)
		}
		if err := store.CommitNativeAlertEvaluation(ctx, NativeAlertEvaluation{
			RuleID: rule.ID, MemberID: "engine-a", LeaseEpoch: lease.Epoch, States: []AlertRuleState{state},
		}); !errors.Is(err, ErrStaleRuleFence) {
			t.Fatalf("in-flight commit after expr update err=%v want ErrStaleRuleFence", err)
		}
		nextLease, err := store.AcquireAlertRuleLease(ctx, rule.ID, "engine-a", time.Minute)
		if err != nil || nextLease.Epoch <= lease.Epoch {
			t.Fatalf("lease after update=%#v want epoch>%d err=%v", nextLease, lease.Epoch, err)
		}
	})
}

func TestDeleteAlertRuleWithFiringEventsSucceeds(t *testing.T) {
	forEachDialect(t, func(t *testing.T, store *Store) {
		ctx := context.Background()
		project, err := store.CreateProject(ctx, "DeleteRule", "delete-rule")
		if err != nil {
			t.Fatal(err)
		}
		channel, err := store.CreateNotificationChannel(ctx, NotificationChannelCreate{
			Type: "webhook", Name: "del-rule-hook", Config: map[string]any{"url": "https://example.invalid/hook"},
		})
		if err != nil {
			t.Fatal(err)
		}
		rule, err := store.CreateAlertRule(ctx, AlertRuleCreate{
			ProjectSlug: project.Slug, Name: "Deletable", Signal: "metrics", Expression: "up == 0", For: "0s",
			Severity: "warning", ChannelIDs: []int{channel.ID},
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := store.HeartbeatAlertEngine(ctx, "engine-del", time.Minute); err != nil {
			t.Fatal(err)
		}
		lease, err := store.AcquireAlertRuleLease(ctx, rule.ID, "engine-del", time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		now := time.Now().UTC()
		ruleID := rule.ID
		if err := store.CommitNativeAlertEvaluation(ctx, NativeAlertEvaluation{
			RuleID: rule.ID, MemberID: "engine-del", LeaseEpoch: lease.Epoch,
			States: []AlertRuleState{{
				RuleID: rule.ID, Fingerprint: "fp-del", Status: "firing", FiringSince: &now, LastEvaluatedAt: now,
				Labels: map[string]string{"alertname": "Deletable"}, Annotations: map[string]string{},
			}},
			Notifications: []NotificationOutboxEnqueue{{
				IdempotencyKey: "del-rule-outbox", ProjectID: project.ID, RuleID: &ruleID, ChannelID: channel.ID,
				Payload: json.RawMessage(`{"status":"firing"}`),
			}},
		}); err != nil {
			t.Fatal(err)
		}
		if err := store.RecordNotificationDelivery(ctx, channel.ID, rule.ID, "sent", "", 1); err != nil {
			t.Fatal(err)
		}
		if err := store.DeleteAlertRule(ctx, project.Slug, rule.ID); err != nil {
			t.Fatalf("DeleteAlertRule with existing alert_events failed: %v", err)
		}
		events, err := store.ListAlertEvents(ctx, project.Slug, "", nil)
		if err != nil || len(events) != 0 {
			t.Fatalf("events after rule delete=%#v err=%v", events, err)
		}
		outbox, err := store.ListNotificationOutbox(ctx, NotificationOutboxListOptions{ProjectID: project.ID})
		if err != nil || len(outbox) != 1 || outbox[0].RuleID != nil {
			t.Fatalf("outbox after rule delete=%#v err=%v", outbox, err)
		}
	})
}

func TestPutProjectAlertConfigConcurrentFirstWrite(t *testing.T) {
	forEachDialect(t, func(t *testing.T, store *Store) {
		ctx := context.Background()
		project, err := store.CreateProject(ctx, "ConcurrentCfg", "concurrent-cfg")
		if err != nil {
			t.Fatal(err)
		}
		channel, err := store.CreateNotificationChannel(ctx, NotificationChannelCreate{
			Type: "webhook", Name: "concurrent-cfg-hook", Config: map[string]any{"url": "https://example.invalid/hook"},
		})
		if err != nil {
			t.Fatal(err)
		}
		const workers = 6
		start := make(chan struct{})
		errs := make(chan error, workers)
		var wg sync.WaitGroup
		for i := range workers {
			wg.Add(1)
			go func(threshold int) {
				defer wg.Done()
				<-start
				_, putErr := store.PutProjectAlertConfig(ctx, project.ID, ProjectAlertConfigUpdate{
					NewIssue: true, Regression: true, ErrorRateThresholdPerMinute: threshold + 1,
					ChannelIDs: []int{channel.ID, channel.ID},
				})
				errs <- putErr
			}(i)
		}
		close(start)
		wg.Wait()
		close(errs)
		for putErr := range errs {
			if putErr != nil {
				t.Fatalf("concurrent PutProjectAlertConfig failed: %v", putErr)
			}
		}
		cfg, err := store.ProjectAlertConfig(ctx, project.ID)
		if err != nil || len(cfg.ChannelIDs) != 1 || cfg.ChannelIDs[0] != channel.ID {
			t.Fatalf("final project alert config=%#v err=%v", cfg, err)
		}
	})
}

func TestPlatformErrorRateDurableSpikeRecoveryAndNextCycle(t *testing.T) {
	forEachDialect(t, func(t *testing.T, store *Store) {
		ctx := context.Background()
		project, err := store.CreateProject(ctx, "Spike", "spike")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := store.PutProjectAlertConfig(ctx, project.ID, ProjectAlertConfigUpdate{
			NewIssue: false, Regression: false, ErrorRateThresholdPerMinute: 1,
		}); err != nil {
			t.Fatal(err)
		}
		t0 := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
		window := 5 * time.Minute
		var fired PlatformErrorSpikeTransition
		for i := range 6 {
			tr, err := store.RecordPlatformErrorRate(
				ctx,
				project.ID,
				t0.Add(time.Duration(i)*time.Second),
				t0.Add(time.Duration(i)*time.Second),
				window,
				1,
			)
			if err != nil {
				t.Fatal(err)
			}
			if tr.Transition == "firing" {
				fired = tr
			}
		}
		if fired.Transition != "firing" || fired.Cycle != 1 {
			t.Fatalf("first spike transition=%#v want firing cycle=1", fired)
		}
		// While still within window, no recovery occurs.
		recoveries, err := store.CheckPlatformErrorRateRecoveries(ctx, t0.Add(2*time.Minute), window)
		if err != nil || len(recoveries) != 0 {
			t.Fatalf("premature recoveries=%#v err=%v", recoveries, err)
		}
		// Once window elapses with no new errors, durable state resolves.
		recoveries, err = store.CheckPlatformErrorRateRecoveries(ctx, t0.Add(6*time.Minute), window)
		if err != nil || len(recoveries) != 1 || recoveries[0].Cycle != 1 || recoveries[0].ProjectID != project.ID {
			t.Fatalf("recoveries=%#v err=%v", recoveries, err)
		}
		// A second spike advances active_cycle to 2.
		t1 := t0.Add(10 * time.Minute)
		var second PlatformErrorSpikeTransition
		for i := range 6 {
			tr, err := store.RecordPlatformErrorRate(
				ctx,
				project.ID,
				t1.Add(time.Duration(i)*time.Second),
				t1.Add(time.Duration(i)*time.Second),
				window,
				1,
			)
			if err != nil {
				t.Fatal(err)
			}
			if tr.Transition == "firing" {
				second = tr
			}
		}
		if second.Transition != "firing" || second.Cycle != 2 {
			t.Fatalf("second spike transition=%#v want firing cycle=2", second)
		}
	})
}

func TestAlertRuleEvaluationIntervalPersistenceAndNoReset(t *testing.T) {
	forEachDialect(t, func(t *testing.T, store *Store) {
		ctx := context.Background()
		project, err := store.CreateProject(ctx, "EvalInterval", "eval-interval")
		if err != nil {
			t.Fatal(err)
		}
		defaultRule, err := store.CreateAlertRule(ctx, AlertRuleCreate{
			ProjectSlug: project.Slug,
			Name:        "DefaultInterval",
			Signal:      "metrics",
			Expression:  "up == 0",
			For:         "0s",
			Severity:    "warning",
		})
		if err != nil {
			t.Fatal(err)
		}
		if defaultRule.EvaluationInterval != "" {
			t.Fatalf("default EvaluationInterval=%q want empty", defaultRule.EvaluationInterval)
		}

		customRule, err := store.CreateAlertRule(ctx, AlertRuleCreate{
			ProjectSlug:        project.Slug,
			Name:               "CustomInterval",
			Signal:             "metrics",
			Expression:         "up == 0",
			For:                "0s",
			EvaluationInterval: "30s",
			Severity:           "warning",
		})
		if err != nil {
			t.Fatal(err)
		}
		if customRule.EvaluationInterval != "30s" {
			t.Fatalf("created EvaluationInterval=%q want 30s", customRule.EvaluationInterval)
		}

		if _, err := store.HeartbeatAlertEngine(ctx, "engine-eval", time.Minute); err != nil {
			t.Fatal(err)
		}
		lease, err := store.AcquireAlertRuleLease(ctx, customRule.ID, "engine-eval", time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		now := time.Now().UTC().Truncate(time.Millisecond)
		firingState := AlertRuleState{
			RuleID:          customRule.ID,
			Fingerprint:     "fp-eval-1",
			Status:          "firing",
			FiringSince:     &now,
			LastEvaluatedAt: now,
			Labels:          map[string]string{"alertname": "CustomInterval"},
			Annotations:     map[string]string{},
		}
		if err := store.CommitNativeAlertEvaluation(ctx, NativeAlertEvaluation{
			RuleID:     customRule.ID,
			MemberID:   "engine-eval",
			LeaseEpoch: lease.Epoch,
			States:     []AlertRuleState{firingState},
		}); err != nil {
			t.Fatal(err)
		}

		// Update with EvaluationInterval=nil keeps existing value.
		sev := "critical"
		kept, err := store.UpdateAlertRule(ctx, project.Slug, customRule.ID, AlertRuleUpdate{
			Severity: &sev,
		})
		if err != nil {
			t.Fatal(err)
		}
		if kept.EvaluationInterval != "30s" || kept.Severity != "critical" {
			t.Fatalf("update nil EvaluationInterval=%+v", kept)
		}

		// Interval-only change must update value without resetting rule state, resolving events, or bumping lease epoch.
		oneMin := "1m"
		updated, err := store.UpdateAlertRule(ctx, project.Slug, customRule.ID, AlertRuleUpdate{
			EvaluationInterval: &oneMin,
		})
		if err != nil {
			t.Fatal(err)
		}
		if updated.EvaluationInterval != "1m" {
			t.Fatalf("updated EvaluationInterval=%q want 1m", updated.EvaluationInterval)
		}
		states, err := store.AlertRuleStates(ctx, customRule.ID)
		if err != nil || len(states) != 1 || states[0].Status != "firing" {
			t.Fatalf("states after interval-only update=%#v err=%v", states, err)
		}
		events, err := store.ListAlertEvents(ctx, project.Slug, "", nil)
		if err != nil || len(events) != 1 || events[0].Status != "firing" || events[0].EndsAt != nil {
			t.Fatalf("events after interval-only update=%#v err=%v", events, err)
		}
		if err := store.CommitNativeAlertEvaluation(ctx, NativeAlertEvaluation{
			RuleID:     customRule.ID,
			MemberID:   "engine-eval",
			LeaseEpoch: lease.Epoch,
			States:     []AlertRuleState{firingState},
		}); err != nil {
			t.Fatalf("commit with original lease epoch after interval-only update failed: %v", err)
		}

		// Explicit empty string resets back to default "" without resetting rule state.
		reset := ""
		cleared, err := store.UpdateAlertRule(ctx, project.Slug, customRule.ID, AlertRuleUpdate{
			EvaluationInterval: &reset,
		})
		if err != nil {
			t.Fatal(err)
		}
		if cleared.EvaluationInterval != "" {
			t.Fatalf("reset EvaluationInterval=%q want empty", cleared.EvaluationInterval)
		}
		states, err = store.AlertRuleStates(ctx, customRule.ID)
		if err != nil || len(states) != 1 || states[0].Status != "firing" {
			t.Fatalf("states after interval reset=%#v err=%v", states, err)
		}
	})
}
