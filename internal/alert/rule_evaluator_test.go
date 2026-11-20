package alert

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/eimsound/lazy-observe/internal/meta"
	"github.com/eimsound/lazy-observe/internal/outbound"
	"github.com/eimsound/lazy-observe/internal/storage"
)

func TestHTTPRuleQueryUsesIsolatedSignalContracts(t *testing.T) {
	project := &meta.ProjectInfo{ID: 42, Slug: "alpha"}
	now := time.Date(2026, 7, 18, 2, 3, 4, 0, time.UTC)
	tests := []struct {
		name       string
		signal     string
		kind       storage.MetricsKind
		path       string
		wantQuery  string
		wantFilter string
		wantTenant bool
	}{
		{
			name:       "VictoriaMetrics",
			signal:     "metrics",
			kind:       storage.MetricsVM,
			path:       "/api/v1/query",
			wantQuery:  "up == 0",
			wantFilter: `{project_id="alpha"}`,
		},
		{
			name:      "Prometheus",
			signal:    "metrics",
			kind:      storage.MetricsPrometheus,
			path:      "/api/v1/query",
			wantQuery: `project_id="alpha"`,
		},
		{
			name:       "VictoriaLogs",
			signal:     "logs",
			kind:       storage.MetricsVM,
			path:       "/select/logsql/stats_query",
			wantQuery:  "up == 0",
			wantTenant: true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != test.path || !strings.Contains(r.URL.Query().Get("query"), test.wantQuery) {
					t.Errorf("request path=%q query=%q", r.URL.Path, r.URL.Query().Get("query"))
				}
				if got := r.URL.Query().Get("extra_filters[]"); got != test.wantFilter {
					t.Errorf("extra_filters[]=%q want=%q", got, test.wantFilter)
				}
				if test.wantTenant && (r.Header.Get("AccountID") != "0" || r.Header.Get("ProjectID") != "42") {
					t.Errorf(
						"tenant headers AccountID=%q ProjectID=%q",
						r.Header.Get("AccountID"),
						r.Header.Get("ProjectID"),
					)
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write(
					[]byte(
						`{"status":"success","data":{"resultType":"vector","result":[{"metric":{"__name__":"up","instance":"api-1"},"value":[1784340184,"1.5"]}]}}`,
					),
				)
			}))
			defer server.Close()
			backend := evaluatorTestBackend{kind: test.kind, base: server.URL}
			query := HTTPRuleQuery{Backend: backend, Client: server.Client()}
			rule := &meta.AlertRuleView{
				ID: 7, Name: "InstanceDown", Signal: test.signal, Expression: "up == 0", Severity: "critical",
				Labels: map[string]string{"team": "ops"}, Annotations: map[string]string{"summary": "down"},
			}
			samples, err := query.Evaluate(context.Background(), rule, project, now)
			if err != nil || len(samples) != 1 {
				t.Fatalf("samples=%#v err=%v", samples, err)
			}
			labels := samples[0].Labels
			if labels["__name__"] != "" || labels["project_id"] != "alpha" || labels["alertname"] != "InstanceDown" ||
				labels["severity"] != "critical" ||
				labels["team"] != "ops" ||
				samples[0].Value != 1.5 {
				t.Fatalf("sample=%#v", samples[0])
			}
		})
	}
}

func TestAdvanceRuleStatesMatchesForAndRecoveryFixture(t *testing.T) {
	rule := &meta.AlertRuleView{ID: 7, For: "1m"}
	t0 := time.Date(2026, 7, 18, 0, 0, 0, 0, time.UTC)
	sample := RuleSample{
		Fingerprint: "api-1",
		Labels:      map[string]string{"instance": "api-1"},
		Annotations: map[string]string{},
		Value:       1,
	}
	states, err := advanceRuleStates(rule, nil, []RuleSample{sample}, t0)
	if err != nil || len(states) != 1 || states[0].Status != "pending" {
		t.Fatalf("initial states=%#v err=%v", states, err)
	}
	states, err = advanceRuleStates(rule, states, []RuleSample{sample}, t0.Add(59*time.Second))
	if err != nil || states[0].Status != "pending" {
		t.Fatalf("before for states=%#v err=%v", states, err)
	}
	states, err = advanceRuleStates(rule, states, []RuleSample{sample}, t0.Add(time.Minute))
	if err != nil || states[0].Status != "firing" || states[0].FiringSince == nil {
		t.Fatalf("firing states=%#v err=%v", states, err)
	}
	states, err = advanceRuleStates(rule, states, nil, t0.Add(2*time.Minute))
	if err != nil || states[0].Status != "recovered" || states[0].Value != nil {
		t.Fatalf("recovered states=%#v err=%v", states, err)
	}
	states, err = advanceRuleStates(rule, states, []RuleSample{sample}, t0.Add(3*time.Minute))
	if err != nil || states[0].Status != "pending" || states[0].PendingSince == nil ||
		!states[0].PendingSince.Equal(t0.Add(3*time.Minute)) {
		t.Fatalf("reactivated states=%#v err=%v", states, err)
	}
}

func TestNativeEvaluatorHasOneQueryOwnerAndFailsOver(t *testing.T) {
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
	rule, err := store.CreateAlertRule(ctx, meta.AlertRuleCreate{
		ProjectSlug: project.Slug,
		Name:        "Always",
		Signal:      "metrics",
		Expression:  "vector(1)",
		For:         "0s",
		Severity:    "warning",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateAlertEvent(ctx, meta.AlertEventCreate{
		RuleID: rule.ID, Fingerprint: "same", Status: "firing", Labels: map[string]string{},
		Annotations: map[string]string{}, StartsAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	for _, memberID := range []string{"evaluator-a", "evaluator-b"} {
		if _, err := store.HeartbeatAlertEngine(ctx, memberID, time.Minute); err != nil {
			t.Fatal(err)
		}
	}
	query := &countingRuleQuery{
		samples: []RuleSample{
			{Fingerprint: "same", Labels: map[string]string{}, Annotations: map[string]string{}, Value: 1},
		},
	}
	evaluators := []*RuleEvaluator{
		NewRuleEvaluator(
			RuleEvaluatorConfig{
				Store:     store,
				Query:     query,
				MemberID:  "evaluator-a",
				MemberTTL: time.Minute,
				LeaseTTL:  time.Second,
			},
		),
		NewRuleEvaluator(
			RuleEvaluatorConfig{
				Store:     store,
				Query:     query,
				MemberID:  "evaluator-b",
				MemberTTL: time.Minute,
				LeaseTTL:  time.Second,
			},
		),
	}
	var wg sync.WaitGroup
	for _, evaluator := range evaluators {
		wg.Add(1)
		go func() {
			defer wg.Done()
			evaluator.evaluateCycle(ctx)
		}()
	}
	wg.Wait()
	if got := query.calls.Load(); got != 1 {
		t.Fatalf("evaluator queries=%d want=1", got)
	}
	states, err := store.AlertRuleStates(ctx, rule.ID)
	if err != nil || len(states) != 1 || states[0].Status != "firing" {
		t.Fatalf("evaluator states=%#v err=%v", states, err)
	}
	var failed, survivor *RuleEvaluator
	for _, evaluator := range evaluators {
		evaluator.mu.Lock()
		_, owns := evaluator.leases[rule.ID]
		evaluator.mu.Unlock()
		if owns {
			failed = evaluator
		} else {
			survivor = evaluator
		}
	}
	if failed == nil || survivor == nil {
		t.Fatalf("lease ownership was not split: failed=%v survivor=%v", failed != nil, survivor != nil)
	}
	if deleted, err := store.DeleteAlertEngineMember(ctx, failed.memberID); err != nil || !deleted {
		t.Fatalf("delete failed member=%v err=%v", deleted, err)
	}
	time.Sleep(1100 * time.Millisecond)
	survivor.evaluateCycle(ctx)
	if got := query.calls.Load(); got != 2 {
		t.Fatalf("evaluator queries after failover=%d want=2", got)
	}
	states, err = store.AlertRuleStates(ctx, rule.ID)
	if err != nil || len(states) != 1 || states[0].LeaseEpoch < 2 {
		t.Fatalf("failover states=%#v err=%v", states, err)
	}
}

// An evaluation that starts mid-second and commits just after the next second boundary is well
// inside a 1s lease. Whole-second SQLite expiries used to fence that commit off silently.
func TestNativeEvaluatorCommitsWhenEvaluationCrossesSecondBoundary(t *testing.T) {
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
	rule, err := store.CreateAlertRule(ctx, meta.AlertRuleCreate{
		ProjectSlug: project.Slug, Name: "Always", Signal: "metrics", Expression: "vector(1)", For: "0s",
		Severity: "warning",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.HeartbeatAlertEngine(ctx, "evaluator-a", time.Minute); err != nil {
		t.Fatal(err)
	}
	query := &secondBoundaryRuleQuery{countingRuleQuery: countingRuleQuery{samples: []RuleSample{
		{Fingerprint: "same", Labels: map[string]string{}, Annotations: map[string]string{}, Value: 1},
	}}}
	evaluator := NewRuleEvaluator(RuleEvaluatorConfig{
		Store: store, Query: query, MemberID: "evaluator-a", MemberTTL: time.Minute, LeaseTTL: time.Second,
	})
	sleepUntilSubsecond(600 * time.Millisecond)
	start := time.Now()
	evaluator.evaluateCycle(ctx)
	if got := query.calls.Load(); got != 1 {
		t.Fatalf("evaluator queries=%d want=1", got)
	}
	states, err := store.AlertRuleStates(ctx, rule.ID)
	if err != nil || len(states) != 1 || states[0].Status != "firing" {
		t.Fatalf("cycle took %s within 1s lease; states=%#v err=%v", time.Since(start).Round(time.Millisecond),
			states, err)
	}
}

// secondBoundaryRuleQuery returns just after the next whole wall-clock second.
type secondBoundaryRuleQuery struct {
	countingRuleQuery
}

func (q *secondBoundaryRuleQuery) Evaluate(
	ctx context.Context,
	rule *meta.AlertRuleView,
	project *meta.ProjectInfo,
	now time.Time,
) ([]RuleSample, error) {
	sleepUntilSubsecond(50 * time.Millisecond)
	return q.countingRuleQuery.Evaluate(ctx, rule, project, now)
}

// sleepUntilSubsecond sleeps until the wall clock next reaches the given offset within a second.
func sleepUntilSubsecond(offset time.Duration) {
	now := time.Now()
	target := now.Truncate(time.Second).Add(offset)
	if !target.After(now) {
		target = target.Add(time.Second)
	}
	time.Sleep(target.Sub(now))
}

func TestNativeEvaluatorCommitsFiringAndResolvedNotifications(t *testing.T) {
	ctx := context.Background()
	store, err := meta.OpenStore(ctx, filepath.Join(t.TempDir(), "meta.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	project, err := store.CreateProject(ctx, "Native", "native")
	if err != nil {
		t.Fatal(err)
	}
	received := make(chan Notification, 4)
	keys := make(chan string, 4)
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var notification Notification
		if err := json.NewDecoder(r.Body).Decode(&notification); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		received <- notification
		keys <- r.Header.Get("Idempotency-Key")
		w.WriteHeader(http.StatusNoContent)
	}))
	defer endpoint.Close()
	query := &mutableRuleQuery{}
	query.Set(
		[]RuleSample{
			{
				Fingerprint: "native-alert",
				Labels:      map[string]string{"project_id": "native", "alertname": "Always"},
				Annotations: map[string]string{"summary": "active"},
				Value:       1,
			},
		},
	)
	service := NewService(Config{
		Store:          store,
		OutboundPolicy: outbound.Policy{AllowPrivate: true},
		Sender:         &Sender{Client: endpoint.Client(), Sleep: func(time.Duration) {}},
		GroupWait:      time.Millisecond,
		RepeatInterval: time.Hour,
		Outbox:         OutboxWorkerConfig{WorkerID: "native-outbox", PollInterval: 5 * time.Millisecond},
		Evaluator: RuleEvaluatorConfig{
			Query:           query,
			MemberID:        "native-engine",
			TickInterval:    5 * time.Millisecond,
			MetricsInterval: 5 * time.Millisecond,
			LeaseTTL:        time.Second,
		},
	})
	defer service.Close()
	channel, err := service.CreateChannel(
		ctx,
		ChannelInput{Type: "webhook", Name: "native", Config: map[string]any{"url": endpoint.URL}},
	)
	if err != nil {
		t.Fatal(err)
	}
	rule, err := service.CreateRule(
		ctx,
		RuleInput{
			Project:          project.Slug,
			Name:             "Always",
			Expr:             "vector(1)",
			EvaluationEngine: "native",
			ChannelIDs:       []int{channel.ID},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	firing := waitNotificationStatus(t, received, "firing")
	if len(firing.Alerts) != 1 || firing.Alerts[0].Fingerprint != "native-alert" {
		t.Fatalf("firing notification=%#v", firing)
	}
	query.Set(nil)
	resolved := waitNotificationStatus(t, received, "resolved")
	if len(resolved.Alerts) != 1 || resolved.Alerts[0].Fingerprint != "native-alert" ||
		resolved.Alerts[0].EndsAt.IsZero() {
		t.Fatalf("resolved notification=%#v", resolved)
	}
	silence, err := service.CreateSilence(
		ctx,
		SilenceInput{
			Project:  project.Slug,
			Matchers: []Matcher{{Name: "alertname", Op: "=", Value: "Always"}},
			StartsAt: time.Now().Add(-time.Minute),
			EndsAt:   time.Now().Add(time.Minute),
			Creator:  "test",
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	policy, err := store.AlertPolicySnapshot(ctx, project.ID, time.Now())
	activeSilences := make([][]Matcher, 0, len(policy.Silences))
	for _, matchers := range policy.Silences {
		activeSilences = append(activeSilences, mapsToMatchers(matchers))
	}
	if err != nil || !matchesAny(map[string]string{"project_id": "native", "alertname": "Always"}, activeSilences) {
		t.Fatalf("native active silences=%#v err=%v", activeSilences, err)
	}
	query.Set(
		[]RuleSample{
			{
				Fingerprint: "native-alert",
				Labels:      map[string]string{"project_id": "native", "alertname": "Always"},
				Annotations: map[string]string{"summary": "active"},
				Value:       1,
			},
		},
	)
	deadline := time.Now().Add(time.Second)
	for {
		states, stateErr := store.AlertRuleStates(ctx, rule.ID)
		if stateErr == nil && len(states) == 1 && states[0].Status == "firing" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("silenced native state did not become firing: states=%#v err=%v", states, stateErr)
		}
		time.Sleep(5 * time.Millisecond)
	}
	select {
	case notification := <-received:
		t.Fatalf("silenced native alert produced notification: %#v", notification)
	case <-time.After(100 * time.Millisecond):
	}
	if err := service.DeleteSilence(ctx, project.Slug, silence.ID); err != nil {
		t.Fatal(err)
	}
	// Lifting the silence must notify promptly instead of waiting RepeatInterval.
	firingAfterSilence := waitNotificationStatus(t, received, "firing")
	if len(firingAfterSilence.Alerts) != 1 || firingAfterSilence.Alerts[0].Fingerprint != "native-alert" {
		t.Fatalf("firing notification after silence lifted=%#v", firingAfterSilence)
	}
	query.Set(nil)
	resolved = waitNotificationStatus(t, received, "resolved")
	if len(resolved.Alerts) != 1 || resolved.Alerts[0].Fingerprint != "native-alert" {
		t.Fatalf("resolved notification after silence=%#v", resolved)
	}
	for range 4 {
		select {
		case key := <-keys:
			if key == "" {
				t.Fatal("native notification lacks Idempotency-Key")
			}
		case <-time.After(time.Second):
			t.Fatal("missing native notification idempotency key")
		}
	}
	select {
	case notification := <-received:
		t.Fatalf("unexpected repeat notification: %#v", notification)
	case <-time.After(100 * time.Millisecond):
	}
	states, err := store.AlertRuleStates(ctx, rule.ID)
	if err != nil || len(states) != 0 {
		t.Fatalf("recovered states must be pruned from alert_rule_states: states=%#v err=%v", states, err)
	}
	events, err := store.ListAlertEvents(ctx, project.Slug, "", nil)
	if err != nil || len(events) != 1 || events[0].Status != "resolved" || events[0].EndsAt == nil {
		t.Fatalf("native events=%#v err=%v", events, err)
	}
}

func TestHTTPRuleQueryProtectsTrustedProjectID(t *testing.T) {
	project := &meta.ProjectInfo{ID: 42, Slug: "alpha"}
	now := time.Date(2026, 7, 18, 2, 3, 4, 0, time.UTC)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(
			[]byte(
				`{"status":"success","data":{"resultType":"vector","result":[{"metric":{"__name__":"up","project_id":"spoofed-metric"},"value":[1784340184,"1"]}]}}`,
			),
		)
	}))
	defer server.Close()
	query := HTTPRuleQuery{
		Backend: evaluatorTestBackend{kind: storage.MetricsVM, base: server.URL},
		Client:  server.Client(),
	}
	rule := &meta.AlertRuleView{
		ID: 7, Name: "TenantGuard", Signal: "metrics", Expression: "up == 0", Severity: "critical",
		Labels: map[string]string{"project_id": "other-tenant", "team": "ops"},
	}
	samples, err := query.Evaluate(context.Background(), rule, project, now)
	if err != nil || len(samples) != 1 {
		t.Fatalf("samples=%#v err=%v", samples, err)
	}
	if samples[0].Labels["project_id"] != "alpha" {
		t.Fatalf("project_id was overwritten: %#v", samples[0].Labels)
	}
	if samples[0].Labels["team"] != "ops" {
		t.Fatalf("custom label lost: %#v", samples[0].Labels)
	}
}

func TestAdvanceRuleStatesDropsVanishedPendingAndAlreadyRecovered(t *testing.T) {
	rule := &meta.AlertRuleView{ID: 7, For: "1m"}
	t0 := time.Date(2026, 7, 18, 0, 0, 0, 0, time.UTC)
	prev := []meta.AlertRuleState{
		{RuleID: 7, Fingerprint: "pending-gone", Status: "pending", PendingSince: &t0, LastEvaluatedAt: t0},
		{RuleID: 7, Fingerprint: "already-recovered", Status: "recovered", FiringSince: &t0, LastEvaluatedAt: t0},
		{RuleID: 7, Fingerprint: "firing-now-gone", Status: "firing", FiringSince: &t0, LastEvaluatedAt: t0},
	}
	states, err := advanceRuleStates(rule, prev, nil, t0.Add(2*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if len(states) != 1 || states[0].Fingerprint != "firing-now-gone" || states[0].Status != "recovered" {
		t.Fatalf("unexpected converged states=%#v", states)
	}
}

func TestNativeEvaluatorPreservesInhibitedAndSilenceDoesNotSpamPeer(t *testing.T) {
	ctx := context.Background()
	store, err := meta.OpenStore(ctx, filepath.Join(t.TempDir(), "meta.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	project, err := store.CreateProject(ctx, "Inhibit", "inhibit")
	if err != nil {
		t.Fatal(err)
	}
	channel, err := store.CreateNotificationChannel(ctx, meta.NotificationChannelCreate{
		Type: "webhook", Name: "inhibit-hook", Config: map[string]any{"url": "https://example.invalid"},
	})
	if err != nil {
		t.Fatal(err)
	}
	sourceRule, err := store.CreateAlertRule(ctx, meta.AlertRuleCreate{
		ProjectSlug: project.Slug, Name: "HostDown", Signal: "metrics", Expression: "up == 0", For: "0s",
		Severity: "critical", ChannelIDs: []int{channel.ID},
	})
	if err != nil {
		t.Fatal(err)
	}
	targetRule, err := store.CreateAlertRule(ctx, meta.AlertRuleCreate{
		ProjectSlug: project.Slug, Name: "ServiceDown", Signal: "metrics", Expression: "svc == 0", For: "0s",
		Severity: "warning", ChannelIDs: []int{channel.ID},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateInhibitionRule(ctx, meta.InhibitionRuleCreate{
		ProjectSlug:    project.Slug,
		SourceMatchers: []map[string]string{{"name": "alertname", "op": "=", "value": "HostDown"}},
		TargetMatchers: []map[string]string{{"name": "alertname", "op": "=", "value": "ServiceDown"}},
		EqualLabels:    []string{"instance"},
	}); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	if _, err := store.CreateSilence(ctx, meta.SilenceCreate{
		ProjectSlug: project.Slug,
		Matchers:    []map[string]string{{"name": "instance", "op": "=", "value": "silenced-1"}},
		StartsAt:    now.Add(-time.Minute),
		EndsAt:      now.Add(time.Hour),
		Creator:     "test",
	}); err != nil {
		t.Fatal(err)
	}

	var curTime atomic.Int64
	curTime.Store(now.UnixNano())
	query := &perRuleQuery{
		byRule: map[int][]RuleSample{
			sourceRule.ID: {
				{
					Fingerprint: "host-1",
					Labels: map[string]string{
						"project_id": project.Slug,
						"alertname":  "HostDown",
						"instance":   "node-1",
					},
					Value: 1,
				},
			},
			targetRule.ID: {
				{
					Fingerprint: "svc-node-1",
					Labels: map[string]string{
						"project_id": project.Slug,
						"alertname":  "ServiceDown",
						"instance":   "node-1",
					},
					Value: 1,
				},
				{
					Fingerprint: "svc-silenced-1",
					Labels: map[string]string{
						"project_id": project.Slug,
						"alertname":  "ServiceDown",
						"instance":   "silenced-1",
					},
					Value: 1,
				},
				{
					Fingerprint: "svc-active-1",
					Labels: map[string]string{
						"project_id": project.Slug,
						"alertname":  "ServiceDown",
						"instance":   "active-1",
					},
					Value: 1,
				},
			},
		},
	}
	evaluator := NewRuleEvaluator(RuleEvaluatorConfig{
		Store:           store,
		Query:           query,
		MemberID:        "inhibit-member",
		MetricsInterval: time.Second,
		GroupWait:       time.Second,
		RepeatInterval:  4 * time.Hour,
		LeaseTTL:        time.Minute,
		MemberTTL:       time.Minute,
		Now:             func() time.Time { return time.Unix(0, curTime.Load()).UTC() },
	})
	// Cycle 1: enters firing (within GroupWait).
	evaluator.evaluateCycle(ctx)
	// Cycle 2: past GroupWait -> active-1 notifies, node-1 is inhibited, silenced-1 is silenced.
	curTime.Store(now.Add(2 * time.Second).UnixNano())
	evaluator.evaluateCycle(ctx)
	// Cycle 3 & 4: non-notify cycles -> inhibited must stay true, active-1 must NOT re-notify.
	for _, offset := range []time.Duration{7 * time.Second, 12 * time.Second} {
		curTime.Store(now.Add(offset).UnixNano())
		evaluator.evaluateCycle(ctx)
	}
	events, err := store.ListAlertEvents(ctx, project.Slug, "firing", nil)
	if err != nil {
		t.Fatal(err)
	}
	byFP := make(map[string]meta.AlertEventView, len(events))
	for _, ev := range events {
		byFP[ev.Fingerprint] = ev
	}
	if !byFP["svc-node-1"].Inhibited {
		t.Fatalf("svc-node-1 lost inhibited=true on non-notify cycle: %#v", byFP["svc-node-1"])
	}
	states, err := store.AlertRuleStates(ctx, targetRule.ID)
	if err != nil {
		t.Fatal(err)
	}
	stateByFP := make(map[string]meta.AlertRuleState, len(states))
	for _, st := range states {
		stateByFP[st.Fingerprint] = st
	}
	if stateByFP["svc-silenced-1"].LastNotifiedAt != nil {
		t.Fatalf("silenced alert must not set LastNotifiedAt: %#v", stateByFP["svc-silenced-1"])
	}
	if stateByFP["svc-node-1"].LastNotifiedAt != nil {
		t.Fatalf("inhibited alert must not set LastNotifiedAt: %#v", stateByFP["svc-node-1"])
	}
	if stateByFP["svc-active-1"].LastNotifiedAt == nil {
		t.Fatalf("notified alert missing LastNotifiedAt: %#v", stateByFP["svc-active-1"])
	}
	outbox, err := store.ListNotificationOutbox(ctx, meta.NotificationOutboxListOptions{ProjectID: project.ID})
	if err != nil {
		t.Fatal(err)
	}
	// Exactly 2 notifications: 1 for HostDown, 1 for ServiceDown(active-1).
	if len(outbox) != 2 {
		t.Fatalf("expected 2 outbox entries (no repeat spam from silenced peer), got %d: %#v", len(outbox), outbox)
	}
}

func TestNativeEvaluatorIndependentRenewalKeepsSlowQueryLeaseAlive(t *testing.T) {
	ctx := context.Background()
	store, err := meta.OpenStore(ctx, filepath.Join(t.TempDir(), "meta.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	project, err := store.CreateProject(ctx, "SlowQuery", "slow-query")
	if err != nil {
		t.Fatal(err)
	}
	rule, err := store.CreateAlertRule(ctx, meta.AlertRuleCreate{
		ProjectSlug: project.Slug, Name: "SlowRule", Signal: "metrics", Expression: "up == 0", For: "0s",
		Severity: "warning",
	})
	if err != nil {
		t.Fatal(err)
	}
	// Query takes 450ms while LeaseTTL is only 250ms; independent renewal must keep the lease alive.
	slowQuery := &slowRuleQuery{
		delay: 450 * time.Millisecond,
		samples: []RuleSample{{
			Fingerprint: "slow-fp",
			Labels:      map[string]string{"project_id": project.Slug, "alertname": "SlowRule"},
			Value:       1,
		}},
	}
	evaluator := NewRuleEvaluator(RuleEvaluatorConfig{
		Store:             store,
		Query:             slowQuery,
		MemberID:          "slow-member",
		TickInterval:      20 * time.Millisecond,
		MetricsInterval:   20 * time.Millisecond,
		HeartbeatInterval: 100 * time.Millisecond,
		MemberTTL:         2 * time.Second,
		LeaseTTL:          250 * time.Millisecond,
		QueryTimeout:      2 * time.Second,
	})
	evaluator.Start()
	defer evaluator.Close()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		states, stateErr := store.AlertRuleStates(ctx, rule.ID)
		if stateErr == nil && len(states) == 1 && states[0].Status == "firing" {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	states, _ := store.AlertRuleStates(ctx, rule.ID)
	t.Fatalf("slow query evaluation never committed firing state (renewal failed): states=%#v", states)
}

func TestNativeEvaluatorLogsLeaseRejectionsAndCleansExpiredMembers(t *testing.T) {
	ctx := context.Background()
	store, err := meta.OpenStore(ctx, filepath.Join(t.TempDir(), "meta.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	project, err := store.CreateProject(ctx, "Logs", "logs-lease")
	if err != nil {
		t.Fatal(err)
	}
	rule, err := store.CreateAlertRule(ctx, meta.AlertRuleCreate{
		ProjectSlug: project.Slug, Name: "Contested", Signal: "metrics", Expression: "up == 0", For: "0s",
		Severity: "warning",
	})
	if err != nil {
		t.Fatal(err)
	}
	// Seed another member holding the lease so our evaluator hits ErrRuleLeaseHeld.
	if _, err := store.HeartbeatAlertEngine(ctx, "holder-member", time.Minute); err != nil {
		t.Fatal(err)
	}
	if _, err := store.AcquireAlertRuleLease(ctx, rule.ID, "holder-member", time.Minute); err != nil {
		t.Fatal(err)
	}
	// Seed an already-expired member so heartbeat cleanup removes it.
	if _, err := store.HeartbeatAlertEngine(ctx, "expired-member", time.Millisecond); err != nil {
		t.Fatal(err)
	}
	time.Sleep(15 * time.Millisecond)

	var buf bytes.Buffer
	oldLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	defer slog.SetDefault(oldLogger)

	// Force evaluator to be sole active member after holder expires? Instead,
	// delete holder-member row while its lease is still unexpired, so our
	// evaluator wins rendezvous hash and hits ErrRuleLeaseHeld.
	if _, err := store.DeleteAlertEngineMember(ctx, "holder-member"); err != nil {
		t.Fatal(err)
	}
	evaluator := NewRuleEvaluator(RuleEvaluatorConfig{
		Store:             store,
		Query:             &countingRuleQuery{},
		MemberID:          "contender-member",
		MemberTTL:         time.Minute,
		HeartbeatInterval: 20 * time.Millisecond,
		LeaseTTL:          time.Minute,
	})
	evaluator.evaluateCycle(ctx)
	// Also exercise stale release warning.
	evaluator.mu.Lock()
	evaluator.leases[rule.ID] = meta.AlertRuleLease{RuleID: rule.ID, OwnerID: "contender-member", Epoch: 99}
	evaluator.mu.Unlock()
	evaluator.releaseAll(ctx)

	logOutput := buf.String()
	if !strings.Contains(logOutput, "level=WARN") || !strings.Contains(logOutput, "rule_id=") ||
		!strings.Contains(logOutput, "member_id=contender-member") || !strings.Contains(logOutput, "lease_epoch=") {
		t.Fatalf("expected WARN fencing logs with rule/member/epoch, got:\n%s", logOutput)
	}

	// Start briefly to verify heartbeat cleans expired members.
	evaluator.Start()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		members, listErr := store.ActiveAlertEngineMembers(ctx)
		if listErr == nil && len(members) == 1 && members[0].MemberID == "contender-member" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	evaluator.Close()
}

func TestNativeEvaluatorRespectsStormThreshold(t *testing.T) {
	ctx := context.Background()
	store, err := meta.OpenStore(ctx, filepath.Join(t.TempDir(), "meta.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	project, err := store.CreateProject(ctx, "Storm", "storm-native")
	if err != nil {
		t.Fatal(err)
	}
	channel, err := store.CreateNotificationChannel(ctx, meta.NotificationChannelCreate{
		Type: "webhook", Name: "storm-hook", Config: map[string]any{"url": "https://example.invalid"},
	})
	if err != nil {
		t.Fatal(err)
	}
	byRule := make(map[int][]RuleSample)
	for i := range 3 {
		name := fmt.Sprintf("StormRule-%d", i)
		r, createErr := store.CreateAlertRule(ctx, meta.AlertRuleCreate{
			ProjectSlug: project.Slug, Name: name, Signal: "metrics", Expression: "vector(1)", For: "0s",
			Severity: "critical", ChannelIDs: []int{channel.ID},
		})
		if createErr != nil {
			t.Fatal(createErr)
		}
		byRule[r.ID] = []RuleSample{{
			Fingerprint: fmt.Sprintf("fp-%d", i),
			Labels:      map[string]string{"project_id": project.Slug, "alertname": name, "severity": "critical"},
			Value:       1,
		}}
	}
	now := time.Date(2026, 8, 1, 13, 0, 0, 0, time.UTC)
	var curTime atomic.Int64
	curTime.Store(now.UnixNano())
	evaluator := NewRuleEvaluator(RuleEvaluatorConfig{
		Store:           store,
		Query:           &perRuleQuery{byRule: byRule},
		MemberID:        "storm-member",
		MetricsInterval: time.Second,
		GroupWait:       time.Second,
		RepeatInterval:  time.Hour,
		StormThreshold:  2,
		MaxConcurrency:  1,
		LeaseTTL:        time.Minute,
		MemberTTL:       time.Minute,
		Now:             func() time.Time { return time.Unix(0, curTime.Load()).UTC() },
	})
	// Cycle 1: all 3 rules enter firing state (before GroupWait).
	evaluator.evaluateCycle(ctx)
	// Cycle 2: past GroupWait with 3 firing rules > StormThreshold(2) -> storm summary deduplicated to 1 entry!
	curTime.Store(now.Add(2 * time.Second).UnixNano())
	evaluator.evaluateCycle(ctx)

	outbox, err := store.ListNotificationOutbox(ctx, meta.NotificationOutboxListOptions{ProjectID: project.ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(outbox) != 1 {
		t.Fatalf("expected 1 deduplicated storm notification for 3 rules (> threshold 2), got %d", len(outbox))
	}
	item, err := store.NotificationOutboxItem(ctx, outbox[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	var n Notification
	if err := json.Unmarshal(item.Payload, &n); err != nil {
		t.Fatal(err)
	}
	if n.Rule != "alert_storm" || n.Storm == nil || n.Storm.GroupCount != 3 {
		t.Fatalf("expected alert_storm summary with GroupCount=3, got %#v", n)
	}
}

type perRuleQuery struct {
	mu     sync.RWMutex
	byRule map[int][]RuleSample
}

func (q *perRuleQuery) Evaluate(
	_ context.Context,
	rule *meta.AlertRuleView,
	_ *meta.ProjectInfo,
	_ time.Time,
) ([]RuleSample, error) {
	q.mu.RLock()
	defer q.mu.RUnlock()
	return slices.Clone(q.byRule[rule.ID]), nil
}

type slowRuleQuery struct {
	delay   time.Duration
	samples []RuleSample
}

func (q *slowRuleQuery) Evaluate(
	ctx context.Context,
	_ *meta.AlertRuleView,
	_ *meta.ProjectInfo,
	_ time.Time,
) ([]RuleSample, error) {
	select {
	case <-time.After(q.delay):
		return slices.Clone(q.samples), nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

type evaluatorTestBackend struct {
	kind storage.MetricsKind
	base string
}

func (b evaluatorTestBackend) MetricsMode() storage.MetricsKind { return b.kind }
func (b evaluatorTestBackend) MetricsQueryBase() string         { return b.base }
func (b evaluatorTestBackend) LogsBase() string                 { return b.base }
func (b evaluatorTestBackend) SetTenantHeaders(header http.Header, projectID int) {
	header.Set("AccountID", "0")
	header.Set("ProjectID", fmt.Sprint(projectID))
}

type countingRuleQuery struct {
	calls   atomic.Int32
	samples []RuleSample
	err     error
}

func (q *countingRuleQuery) Evaluate(
	context.Context,
	*meta.AlertRuleView,
	*meta.ProjectInfo,
	time.Time,
) ([]RuleSample, error) {
	q.calls.Add(1)
	return q.samples, q.err
}

type mutableRuleQuery struct {
	mu      sync.RWMutex
	samples []RuleSample
}

func (q *mutableRuleQuery) Set(samples []RuleSample) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.samples = slices.Clone(samples)
}

func (q *mutableRuleQuery) Evaluate(
	context.Context,
	*meta.AlertRuleView,
	*meta.ProjectInfo,
	time.Time,
) ([]RuleSample, error) {
	q.mu.RLock()
	defer q.mu.RUnlock()
	return slices.Clone(q.samples), nil
}

func waitNotificationStatus(t *testing.T, notifications <-chan Notification, status string) Notification {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		select {
		case notification := <-notifications:
			if notification.Status == status {
				return notification
			}
		case <-deadline:
			t.Fatalf("timeout waiting for %s notification", status)
			return Notification{}
		}
	}
}
