package alert

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/eimsound/lazy-observe/internal/alert/matcher"
	"github.com/eimsound/lazy-observe/internal/errortrack"
	"github.com/eimsound/lazy-observe/internal/meta"
	"github.com/eimsound/lazy-observe/internal/outbound"
)

func TestPlanNotificationsUnifiesSilenceInhibitionStormAndRepeat(t *testing.T) {
	base := time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)

	t.Run("group wait and repeat interval due semantics", func(t *testing.T) {
		firingAt := base
		if IsFiringDue(&firingAt, nil, false, 30*time.Second, time.Hour, base.Add(29*time.Second)) {
			t.Fatal("should not be due before groupWait elapses")
		}
		if !IsFiringDue(&firingAt, nil, false, 30*time.Second, time.Hour, base.Add(30*time.Second)) {
			t.Fatal("should be due once groupWait elapses on initial fire")
		}
		lastNotified := base.Add(30 * time.Second)
		if IsFiringDue(&firingAt, &lastNotified, true, 30*time.Second, time.Hour, base.Add(30*time.Minute)) {
			t.Fatal("should not repeat before repeatInterval elapses")
		}
		if !IsFiringDue(&firingAt, &lastNotified, true, 30*time.Second, time.Hour, lastNotified.Add(time.Hour)) {
			t.Fatal("should repeat once repeatInterval elapses")
		}
	})

	t.Run("silenced and inhibited alerts do not advance NotifiedFingerprints", func(t *testing.T) {
		policy := CompilePolicy(
			[][]Matcher{{{Name: "instance", Op: "=~", Value: `silenced-\d+`}}},
			[]InhibitionRule{{
				SourceMatchers: []Matcher{{Name: "alertname", Op: "=", Value: "HostDown"}},
				TargetMatchers: []Matcher{{Name: "alertname", Op: "=", Value: "ServiceDown"}},
				EqualLabels:    []string{"instance"},
			}},
			[]Alert{{
				Status:      "firing",
				Fingerprint: "src-host",
				Labels:      map[string]string{"alertname": "HostDown", "instance": "inhibited-1"},
			}},
			nil,
		)
		firingAt := base
		candidates := []AlertCandidate{
			{
				Alert: Alert{
					Status:      "firing",
					Fingerprint: "fp-silenced",
					Labels:      map[string]string{"alertname": "ServiceDown", "instance": "silenced-1"},
					StartsAt:    firingAt,
				},
				FiringSince: &firingAt,
			},
			{
				Alert: Alert{
					Status:      "firing",
					Fingerprint: "fp-inhibited",
					Labels:      map[string]string{"alertname": "ServiceDown", "instance": "inhibited-1"},
					StartsAt:    firingAt,
				},
				FiringSince: &firingAt,
			},
			{
				Alert: Alert{
					Status:      "firing",
					Fingerprint: "fp-deliver",
					Labels:      map[string]string{"alertname": "ServiceDown", "instance": "healthy-1"},
					StartsAt:    firingAt,
				},
				FiringSince: &firingAt,
			},
		}
		plan := PlanNotifications(PlanConfig{
			ProjectSlug:    "alpha",
			RuleName:       "ServiceDown",
			GroupKey:       "rule:1",
			Severity:       "critical",
			GroupWait:      10 * time.Second,
			RepeatInterval: time.Hour,
			StormThreshold: 10,
			Now:            base.Add(15 * time.Second),
		}, policy, candidates)

		if plan.FiringDisposition != DispositionNotify || len(plan.Notifications) != 1 {
			t.Fatalf("unexpected plan=%+v", plan)
		}
		if len(plan.Notifications[0].Alerts) != 1 || plan.Notifications[0].Alerts[0].Fingerprint != "fp-deliver" {
			t.Fatalf("unexpected delivered alerts=%#v", plan.Notifications[0].Alerts)
		}
		if _, ok := plan.NotifiedFingerprints["fp-silenced"]; ok {
			t.Fatal("silenced alert must not enter NotifiedFingerprints")
		}
		if _, ok := plan.NotifiedFingerprints["fp-inhibited"]; ok {
			t.Fatal("inhibited alert must not enter NotifiedFingerprints")
		}
		if _, ok := plan.NotifiedFingerprints["fp-deliver"]; !ok {
			t.Fatal("delivered alert must enter NotifiedFingerprints")
		}
		if !plan.InhibitedFingerprints["fp-inhibited"] || plan.InhibitedFingerprints["fp-deliver"] {
			t.Fatalf("unexpected InhibitedFingerprints=%#v", plan.InhibitedFingerprints)
		}
	})

	t.Run("storm summary and unified outbox key across native and platform", func(t *testing.T) {
		policy := CompiledPolicy{
			ActiveGroups: []GroupSummary{
				{Key: "rule:1", Name: "CPUHigh", Severity: "critical"},
				{Key: "platform:error_rate_spike", Name: "error_rate_spike", Severity: "warning"},
			},
		}
		firingAt := base
		plan := PlanNotifications(PlanConfig{
			ProjectSlug:    "alpha",
			ProjectID:      7,
			RuleName:       "MemHigh",
			GroupKey:       "rule:2",
			Severity:       "critical",
			GroupWait:      time.Second,
			RepeatInterval: time.Hour,
			StormThreshold: 2,
			Now:            base.Add(2 * time.Second),
		}, policy, []AlertCandidate{{
			Alert: Alert{
				Status:      "firing",
				Fingerprint: "fp-mem",
				Labels:      map[string]string{"alertname": "MemHigh", "severity": "critical"},
				StartsAt:    firingAt,
			},
			FiringSince: &firingAt,
		}})
		if plan.FiringDisposition != DispositionStorm || len(plan.Notifications) != 1 ||
			plan.Notifications[0].Storm == nil {
			t.Fatalf("expected storm plan, got %+v", plan)
		}
		if plan.Notifications[0].Storm.GroupCount != 3 {
			t.Fatalf("storm group_count=%d want 3", plan.Notifications[0].Storm.GroupCount)
		}

		ruleID := 2
		nativeStorm := BuildOutboxEntries(7, &ruleID, []int{9}, plan.Notifications, base.Add(2*time.Second), time.Hour)
		platformStorm := BuildOutboxEntries(7, nil, []int{9}, plan.Notifications, base.Add(2*time.Second), time.Hour)
		if len(nativeStorm) != 1 || len(platformStorm) != 1 {
			t.Fatalf("expected 1 entry each: native=%d platform=%d", len(nativeStorm), len(platformStorm))
		}
		if nativeStorm[0].IdempotencyKey != platformStorm[0].IdempotencyKey {
			t.Fatalf("native and platform storm idempotency keys drifted: %q vs %q",
				nativeStorm[0].IdempotencyKey, platformStorm[0].IdempotencyKey)
		}
	})
}

func TestBuildOutboxEntriesUnchangedWithEmptyURLFields(t *testing.T) {
	now := time.Date(2026, 7, 17, 1, 2, 3, 0, time.UTC)
	ruleID := 7
	repeatInterval := time.Hour

	native := Notification{
		Project: "alpha",
		Rule:    "DiskFull",
		Status:  "firing",
		Alerts: []Alert{{
			Status:      "firing",
			Labels:      map[string]string{"alertname": "DiskFull", "instance": "node-1", "project_id": "alpha"},
			Annotations: map[string]string{"summary": "disk full"},
			StartsAt:    now,
			Fingerprint: "abc",
		}},
	}
	wantNativePayload := `{"project":"alpha","rule":"DiskFull","status":"firing","alerts":[{"status":"firing","labels":{"alertname":"DiskFull","instance":"node-1","project_id":"alpha"},"annotations":{"summary":"disk full"},"startsAt":"2026-07-17T01:02:03Z","endsAt":"0001-01-01T00:00:00Z","fingerprint":"abc"}]}`
	entries := BuildOutboxEntries(1, &ruleID, []int{4}, []Notification{native}, now, repeatInterval)
	if len(entries) != 1 {
		t.Fatalf("native entries = %d, want 1", len(entries))
	}
	if string(entries[0].Payload) != wantNativePayload {
		t.Fatalf("native payload changed:\ngot:  %s\nwant: %s", entries[0].Payload, wantNativePayload)
	}
	if strings.Contains(string(entries[0].Payload), `"url"`) {
		t.Fatalf("native payload unexpectedly contains url key: %s", entries[0].Payload)
	}
	wantNativeKey := notificationOutboxKey(4, append([]byte(wantNativePayload), []byte("\x007\x000")...))
	if entries[0].IdempotencyKey != wantNativeKey {
		t.Fatalf("native idempotency key = %q, want %q", entries[0].IdempotencyKey, wantNativeKey)
	}

	issue := Notification{
		Project: "alpha",
		Rule:    "new_issue",
		Status:  "firing",
		Alerts: []Alert{
			{
				Status: "firing",
				Labels: map[string]string{
					"issue_id":   "42",
					"kind":       "new_issue",
					"project_id": "alpha",
					"source":     "platform",
				},
				Annotations: map[string]string{"issue": "/issues/42", "link": "/issues/42", "summary": "boom"},
				StartsAt:    now,
				Fingerprint: "issue-fp",
			},
		},
	}
	wantIssuePayload := `{"project":"alpha","rule":"new_issue","status":"firing","alerts":[{"status":"firing","labels":{"issue_id":"42","kind":"new_issue","project_id":"alpha","source":"platform"},"annotations":{"issue":"/issues/42","link":"/issues/42","summary":"boom"},"startsAt":"2026-07-17T01:02:03Z","endsAt":"0001-01-01T00:00:00Z","fingerprint":"issue-fp"}]}`
	issueEntries := BuildOutboxEntriesWithLifecycle(1, nil, 99, []int{4}, []Notification{issue}, now, repeatInterval)
	if len(issueEntries) != 1 {
		t.Fatalf("issue entries = %d, want 1", len(issueEntries))
	}
	if string(issueEntries[0].Payload) != wantIssuePayload {
		t.Fatalf("issue payload changed:\ngot:  %s\nwant: %s", issueEntries[0].Payload, wantIssuePayload)
	}
	wantIssueKey := notificationOutboxKey(4, []byte("issue-lifecycle\x0099\x00new_issue\x00firing"))
	if issueEntries[0].IdempotencyKey != wantIssueKey {
		t.Fatalf("issue idempotency key = %q, want %q", issueEntries[0].IdempotencyKey, wantIssueKey)
	}

	storm := Notification{
		Project: "alpha",
		Rule:    "alert_storm",
		Status:  "firing",
		Storm: &StormSummary{
			GroupCount: 12,
			Severities: []Count{{Name: "critical", Count: 8}},
			Rules:      []Count{{Name: "HostDown", Count: 5}},
		},
	}
	wantStormPayload := `{"project":"alpha","rule":"alert_storm","status":"firing","alerts":null,"storm":{"group_count":12,"severities":[{"name":"critical","count":8}],"rules":[{"name":"HostDown","count":5}]}}`
	stormEntries := BuildOutboxEntries(1, nil, []int{4}, []Notification{storm}, now, repeatInterval)
	if len(stormEntries) != 1 {
		t.Fatalf("storm entries = %d, want 1", len(stormEntries))
	}
	if string(stormEntries[0].Payload) != wantStormPayload {
		t.Fatalf("storm payload changed:\ngot:  %s\nwant: %s", stormEntries[0].Payload, wantStormPayload)
	}
}

func TestCrossTrackNativeRuleInhibitsPlatformAlertAndUnblocksOnDelete(t *testing.T) {
	ctx := context.Background()
	store, err := meta.OpenStore(ctx, filepath.Join(t.TempDir(), "meta.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	project, err := store.CreateProject(ctx, "Unified", "unified")
	if err != nil {
		t.Fatal(err)
	}
	received := make(chan Notification, 8)
	webhook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var n Notification
		if decodeErr := json.NewDecoder(r.Body).Decode(&n); decodeErr != nil {
			http.Error(w, decodeErr.Error(), http.StatusBadRequest)
			return
		}
		received <- n
		w.WriteHeader(http.StatusNoContent)
	}))
	defer webhook.Close()

	svc := NewService(Config{
		Store:          store,
		OutboundPolicy: outbound.Policy{AllowPrivate: true},
		Sender:         &Sender{Client: webhook.Client(), Sleep: func(time.Duration) {}},
		GroupWait:      15 * time.Millisecond,
		RepeatInterval: time.Hour,
		Outbox:         OutboxWorkerConfig{WorkerID: "cross-track-outbox", PollInterval: 5 * time.Millisecond},
	})
	defer svc.Close()

	ch, err := svc.CreateChannel(ctx, ChannelInput{
		Type: "webhook", Name: "unified-hook", Config: map[string]any{"url": webhook.URL},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.PutProjectAlertConfig(ctx, project.ID, ProjectAlertConfigInput{
		NewIssue: true, Regression: true, ErrorRateThresholdPerMinute: 1, ChannelIDs: []int{ch.ID},
	}); err != nil {
		t.Fatal(err)
	}
	nativeRule, err := svc.CreateRule(ctx, RuleInput{
		Project: project.Slug, Name: "OutageDown", Expr: "up == 0", For: "0s", Severity: "critical",
	})
	if err != nil {
		t.Fatal(err)
	}
	// Seed a durable firing native rule state in the DB.
	if _, err := store.HeartbeatAlertEngine(ctx, "seed-member", time.Minute); err != nil {
		t.Fatal(err)
	}
	lease, err := store.AcquireAlertRuleLease(ctx, nativeRule.ID, "seed-member", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if err := store.PutAlertRuleState(ctx, meta.AlertRuleState{
		RuleID:          nativeRule.ID,
		Fingerprint:     "native-outage-fp",
		Status:          "firing",
		FiringSince:     &now,
		LastEvaluatedAt: now,
		Labels:          map[string]string{"alertname": "OutageDown", "project_id": project.Slug},
		Annotations:     map[string]string{},
	}, "seed-member", lease.Epoch); err != nil {
		t.Fatal(err)
	}

	// Inhibition rule: native OutageDown inhibits platform alerts in the same project.
	inhib, err := svc.CreateInhibitionRule(ctx, InhibitionRuleInput{
		Project:        project.Slug,
		SourceMatchers: []Matcher{{Name: "alertname", Op: "=", Value: "OutageDown"}},
		TargetMatchers: []Matcher{{Name: "source", Op: "=", Value: "platform"}},
		EqualLabels:    []string{"project_id"},
	})
	if err != nil {
		t.Fatal(err)
	}

	// 1. Durable synchronous HandleIssueChange is inhibited by the native rule.
	if err := svc.HandleIssueChange(ctx, errortrack.IssueChange{
		LifecycleID: 501,
		ProjectID:   project.ID,
		IssueID:     101,
		Kind:        "new_issue",
		Level:       "error",
		Title:       "InhibitedPlatformIssue",
		SeenAt:      now,
	}); err != nil {
		t.Fatal(err)
	}
	// 2. Durable error_rate_spike (direct outbox planning) is also inhibited by the native rule.
	for i := range 6 {
		if err := svc.RecordErrorEvent(ctx, project.ID, now.Add(-time.Duration(6-i)*time.Second)); err != nil {
			t.Fatal(err)
		}
	}
	select {
	case n := <-received:
		t.Fatalf("platform alert was not inhibited by native firing rule: %#v", n)
	case <-time.After(80 * time.Millisecond):
	}

	// Deleting the inhibition rule replays the still-firing error_rate_spike state into the outbox.
	if err := svc.DeleteInhibitionRule(ctx, project.Slug, inhib.ID); err != nil {
		t.Fatal(err)
	}
	got := waitNotificationStatus(t, received, "firing")
	if got.Rule != "error_rate_spike" {
		t.Fatalf("unexpected unblocked platform notification: %#v", got)
	}

	// Subsequent HandleIssueChange with a LifecycleID enqueues synchronously and deduplicates.
	unblockedChange := errortrack.IssueChange{
		LifecycleID: 502,
		ProjectID:   project.ID,
		IssueID:     102,
		Kind:        "new_issue",
		Level:       "error",
		Title:       "UnblockedPlatformIssue",
		SeenAt:      now,
	}
	if err := svc.HandleIssueChange(ctx, unblockedChange); err != nil {
		t.Fatal(err)
	}
	if err := svc.HandleIssueChange(ctx, unblockedChange); err != nil {
		t.Fatal(err)
	}
	issueNotif := waitNotificationStatus(t, received, "firing")
	if issueNotif.Rule != "new_issue" || len(issueNotif.Alerts) != 1 ||
		issueNotif.Alerts[0].Labels["issue_id"] != "102" {
		t.Fatalf("unexpected durable lifecycle notification: %#v", issueNotif)
	}
	select {
	case dup := <-received:
		t.Fatalf("duplicate LifecycleID notification delivered: %#v", dup)
	case <-time.After(80 * time.Millisecond):
	}
}

func TestEvaluatorObservabilityLastErrorLeaseLossAndOutboxDepth(t *testing.T) {
	ctx := context.Background()
	store, err := meta.OpenStore(ctx, filepath.Join(t.TempDir(), "meta.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	project, err := store.CreateProject(ctx, "Obs", "obs")
	if err != nil {
		t.Fatal(err)
	}
	rule, err := store.CreateAlertRule(ctx, meta.AlertRuleCreate{
		ProjectSlug: project.Slug, Name: "ObsRule", Signal: "metrics", Expression: "up == 0", For: "0s",
		Severity: "warning",
	})
	if err != nil {
		t.Fatal(err)
	}

	var logBuf bytes.Buffer
	prevLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	defer slog.SetDefault(prevLogger)

	query := &switchableRuleQuery{
		err: &url.Error{
			Op:  "Get",
			URL: "http://internal-vm:8428/api/v1/query?token=super-secret-query-token",
			Err: &net.OpError{
				Op:   "dial",
				Net:  "tcp",
				Addr: &net.TCPAddr{IP: net.ParseIP("10.0.0.5"), Port: 8428},
				Err:  errors.New("connection refused"),
			},
		},
	}
	evaluator := NewRuleEvaluator(RuleEvaluatorConfig{
		Store:           store,
		Query:           query,
		MemberID:        "obs-member-a",
		MetricsInterval: time.Millisecond,
		LeaseTTL:        time.Minute,
		MemberTTL:       time.Minute,
		MaxConcurrency:  2,
	})

	// 1. Query error must record sanitized last_error and increment error counters.
	evaluator.evaluateCycle(ctx)
	lastErr := evaluator.RuleLastError(rule.ID)
	if lastErr == "" {
		t.Fatal("expected non-empty rule last_error after failed evaluation")
	}
	if strings.Contains(lastErr, "super-secret-query-token") || strings.Contains(lastErr, "10.0.0.5") {
		t.Fatalf("rule last_error leaked sensitive transport details: %q", lastErr)
	}
	snap := evaluator.Snapshot()
	if snap.EvaluationsTotal != 1 || snap.EvaluationErrorsTotal != 1 || snap.OwnedLeases != 1 {
		t.Fatalf("unexpected snapshot after error: %+v", snap)
	}
	if snap.LastCycleDuration <= 0 || snap.LastEvalDuration <= 0 || snap.MaxEvalDuration <= 0 {
		t.Fatalf("expected positive evaluation durations: %+v", snap)
	}

	// 2. Successful evaluation clears rule last_error.
	query.set(nil, []RuleSample{{
		Fingerprint: "obs-fp",
		Labels:      map[string]string{"alertname": "ObsRule", "project_id": project.Slug},
		Annotations: map[string]string{},
		Value:       1,
	}})
	time.Sleep(2 * time.Millisecond)
	evaluator.evaluateCycle(ctx)
	if got := evaluator.RuleLastError(rule.ID); got != "" {
		t.Fatalf("expected last_error to clear after healthy eval, got %q", got)
	}
	st, ok := evaluator.RuleStatus(rule.ID)
	if !ok || st.EvalCount != 2 || st.ErrorCount != 1 {
		t.Fatalf("unexpected rule status: %+v ok=%v", st, ok)
	}

	// 3. Lease loss on renewal increments LeaseLossesTotal and logs slog.Warn.
	if err := store.PauseAlertRuleEvaluation(ctx, rule.ID); err != nil {
		t.Fatal(err)
	}
	evaluator.renewLeases(ctx)
	snap = evaluator.Snapshot()
	if snap.LeaseLossesTotal == 0 {
		t.Fatalf("expected LeaseLossesTotal > 0 after lease revocation, got %+v", snap)
	}
	if !strings.Contains(logBuf.String(), "discard alert rule lease after renewal fence rejection") ||
		!strings.Contains(logBuf.String(), fmt.Sprintf("rule_id=%d", rule.ID)) {
		t.Fatalf("expected slog.Warn with rule_id on renewal fence rejection, got %s", logBuf.String())
	}

	// 4. Service.Diagnostics aggregates evaluator, outbox depth, and matcher cache stats.
	ch, err := store.CreateNotificationChannel(ctx, meta.NotificationChannelCreate{
		Type: "webhook", Name: "diag-hook", Config: map[string]any{"url": "https://example.invalid/hook"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.EnqueueNotification(ctx, meta.NotificationOutboxEnqueue{
		IdempotencyKey: "diag-pending-1",
		ProjectID:      project.ID,
		ChannelID:      ch.ID,
		Payload:        json.RawMessage(`{"status":"firing"}`),
	}); err != nil {
		t.Fatal(err)
	}
	svc := &Service{store: store, evaluator: evaluator}
	diag, err := svc.Diagnostics(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if diag.Outbox.Depth.Pending != 1 || diag.Outbox.Depth.Due != 1 {
		t.Fatalf("unexpected outbox depth in diagnostics: %+v", diag.Outbox.Depth)
	}
	if diag.Evaluator.EvaluationsTotal != 2 || diag.Evaluator.LeaseLossesTotal < 1 {
		t.Fatalf("unexpected evaluator diagnostics: %+v", diag.Evaluator)
	}
}

func TestServiceCentralizedDomainValidation(t *testing.T) {
	ctx := context.Background()
	store, err := meta.OpenStore(ctx, filepath.Join(t.TempDir(), "meta.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	project, err := store.CreateProject(ctx, "Val", "val")
	if err != nil {
		t.Fatal(err)
	}
	svc := NewService(Config{Store: store})
	defer svc.Close()

	// Rule validation: name, PromQL syntax, LogsQL bounds, `for`, signal, labels.
	badRules := []RuleInput{
		{Project: project.Slug, Name: "   ", Expr: "up == 0"},
		{Project: project.Slug, Name: "EmptyExpr", Expr: "   "},
		{Project: project.Slug, Name: "BadPromQL", Signal: "metrics", Expr: "sum(rate(http_requests[))"},
		{Project: project.Slug, Name: "BadFor", Expr: "up == 0", For: "abc"},
		{Project: project.Slug, Name: "BadSignal", Signal: "traces", Expr: "up == 0"},
		{Project: project.Slug, Name: "BadEngine", EvaluationEngine: "vmalert", Expr: "up == 0"},
		{Project: project.Slug, Name: "BadLabel", Expr: "up == 0", Labels: map[string]string{"": "v"}},
		{Project: project.Slug, Name: "NullLogsQL", Signal: "logs", Expr: "* \x00 | stats count()"},
	}
	for _, in := range badRules {
		if _, err := svc.CreateRule(ctx, in); !errors.Is(err, ErrInvalidInput) {
			t.Fatalf("CreateRule(%q) err=%v want ErrInvalidInput", in.Name, err)
		}
	}

	// Valid LogsQL rule succeeds; switching signal to metrics with LogsQL expr fails.
	logsRule, err := svc.CreateRule(ctx, RuleInput{
		Project: project.Slug,
		Name:    "LogsRule",
		Signal:  "logs",
		Expr:    "* | stats count() as total | filter total:>0",
		For:     "1m",
	})
	if err != nil {
		t.Fatalf("CreateRule logs failed: %v", err)
	}
	if _, err := svc.UpdateRule(ctx, logsRule.ID, RuleInput{
		Project: project.Slug,
		Signal:  "metrics",
	}); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("UpdateRule switching LogsQL to metrics err=%v want ErrInvalidInput", err)
	}

	// Channel validation: unsupported type and blank name rejected in service layer.
	if _, err := svc.CreateChannel(ctx, ChannelInput{Type: "pagerduty", Name: "x"}); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("CreateChannel unsupported type err=%v want ErrInvalidInput", err)
	}
	if _, err := svc.CreateChannel(ctx, ChannelInput{Type: "webhook", Name: "  "}); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("CreateChannel blank name err=%v want ErrInvalidInput", err)
	}

	// Silence & inhibition validation: overlong regex and blank equal_labels rejected.
	now := time.Now().UTC()
	if _, err := svc.CreateSilence(ctx, SilenceInput{
		Project:  project.Slug,
		Matchers: []Matcher{{Name: "alertname", Op: "=~", Value: strings.Repeat("x", matcher.MaxRegexLength+1)}},
		StartsAt: now,
		EndsAt:   now.Add(time.Hour),
	}); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("CreateSilence overlong regex err=%v want ErrInvalidInput", err)
	}
	if _, err := svc.CreateInhibitionRule(ctx, InhibitionRuleInput{
		Project:        project.Slug,
		SourceMatchers: []Matcher{{Name: "alertname", Op: "=", Value: "HostDown"}},
		TargetMatchers: []Matcher{{Name: "alertname", Op: "=", Value: "ServiceDown"}},
		EqualLabels:    []string{"  "},
	}); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("CreateInhibitionRule blank equal_labels err=%v want ErrInvalidInput", err)
	}
	if _, err := svc.PutProjectAlertConfig(ctx, project.ID, ProjectAlertConfigInput{
		ErrorRateThresholdPerMinute: -1,
	}); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("PutProjectAlertConfig negative threshold err=%v want ErrInvalidInput", err)
	}
}

type switchableRuleQuery struct {
	mu      sync.Mutex
	err     error
	samples []RuleSample
}

func (q *switchableRuleQuery) set(err error, samples []RuleSample) {
	q.mu.Lock()
	q.err = err
	q.samples = samples
	q.mu.Unlock()
}

func (q *switchableRuleQuery) Evaluate(
	_ context.Context,
	_ *meta.AlertRuleView,
	_ *meta.ProjectInfo,
	_ time.Time,
) ([]RuleSample, error) {
	q.mu.Lock()
	err := q.err
	samples := q.samples
	q.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return samples, nil
}
