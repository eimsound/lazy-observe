package meta

import (
	"context"
	"errors"
	"strings"
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
