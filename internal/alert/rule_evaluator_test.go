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

	"github.com/eimsound/lazy-observe/internal/alert/ownership"
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

func TestUpdateAlertRuleExpressionAndDisableDeliverResolvedOnce(t *testing.T) {
	ctx := context.Background()
	store, err := meta.OpenStore(ctx, filepath.Join(t.TempDir(), "meta.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	project, err := store.CreateProject(ctx, "RuleUpdateResolve", "rule-update-resolve")
	if err != nil {
		t.Fatal(err)
	}
	received := make(chan Notification, 8)
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var notification Notification
		if err := json.NewDecoder(r.Body).Decode(&notification); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		received <- notification
		w.WriteHeader(http.StatusNoContent)
	}))
	defer endpoint.Close()

	query := &exprAwareRuleQuery{firingExpr: "up == 0"}
	service := NewService(Config{
		Store:          store,
		OutboundPolicy: outbound.Policy{AllowPrivate: true},
		Sender:         &Sender{Client: endpoint.Client(), Sleep: func(time.Duration) {}},
		GroupWait:      time.Millisecond,
		RepeatInterval: time.Hour,
		Outbox:         OutboxWorkerConfig{WorkerID: "update-resolve-outbox", PollInterval: 5 * time.Millisecond},
		Evaluator: RuleEvaluatorConfig{
			Query:           query,
			MemberID:        "update-resolve-engine",
			TickInterval:    5 * time.Millisecond,
			MetricsInterval: 5 * time.Millisecond,
			LeaseTTL:        time.Second,
		},
	})
	defer service.Close()

	channel, err := service.CreateChannel(ctx, ChannelInput{
		Type: "webhook", Name: "update-resolve-hook", Config: map[string]any{"url": endpoint.URL},
	})
	if err != nil {
		t.Fatal(err)
	}
	rule, err := service.CreateRule(ctx, RuleInput{
		Project: project.Slug, Name: "UpdateResolveRule", Expr: "up == 0", ChannelIDs: []int{channel.ID},
	})
	if err != nil {
		t.Fatal(err)
	}
	firing := waitNotificationStatus(t, received, "firing")
	if firing.Rule != "UpdateResolveRule" {
		t.Fatalf("unexpected firing notification: %#v", firing)
	}

	// Updating expression to a non-firing query must emit exactly one resolved notification.
	if _, err := service.UpdateRule(ctx, rule.ID, RuleInput{
		Project: project.Slug, Expr: "vector(0) > 0",
	}); err != nil {
		t.Fatal(err)
	}
	resolved := waitNotificationStatus(t, received, "resolved")
	if resolved.Rule != "UpdateResolveRule" || len(resolved.Alerts) != 1 || resolved.Alerts[0].EndsAt.IsZero() {
		t.Fatalf("unexpected resolved notification after expr update: %#v", resolved)
	}
	select {
	case extra := <-received:
		t.Fatalf("duplicate notification after expr update: %#v", extra)
	case <-time.After(100 * time.Millisecond):
	}

	// Re-trigger firing, then disable the rule -> must emit exactly one resolved notification.
	if _, err := service.UpdateRule(ctx, rule.ID, RuleInput{
		Project: project.Slug, Expr: "up == 0",
	}); err != nil {
		t.Fatal(err)
	}
	_ = waitNotificationStatus(t, received, "firing")
	disabled := false
	if _, err := service.UpdateRule(ctx, rule.ID, RuleInput{
		Project: project.Slug, Enabled: &disabled,
	}); err != nil {
		t.Fatal(err)
	}
	resolved = waitNotificationStatus(t, received, "resolved")
	if resolved.Rule != "UpdateResolveRule" {
		t.Fatalf("unexpected resolved notification after disable: %#v", resolved)
	}
	select {
	case extra := <-received:
		t.Fatalf("duplicate notification after disable: %#v", extra)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestNativeEvaluatorRefreshesRuleAfterLeaseAcquisition(t *testing.T) {
	t.Run("expression update before lease acquisition does not revive old alert", func(t *testing.T) {
		ctx := context.Background()
		store, err := meta.OpenStore(ctx, filepath.Join(t.TempDir(), "meta.db"))
		if err != nil {
			t.Fatal(err)
		}
		defer store.Close()
		project, err := store.CreateProject(ctx, "RefreshExpr", "refresh-expr")
		if err != nil {
			t.Fatal(err)
		}
		channel, err := store.CreateNotificationChannel(ctx, meta.NotificationChannelCreate{
			Type: "webhook", Name: "refresh-hook", Config: map[string]any{"url": "https://example.invalid/hook"},
		})
		if err != nil {
			t.Fatal(err)
		}
		rule, err := store.CreateAlertRule(ctx, meta.AlertRuleCreate{
			ProjectSlug: project.Slug, Name: "RefreshRule", Signal: "metrics", Expression: "up == 0", For: "0s",
			Severity: "critical", ChannelIDs: []int{channel.ID},
		})
		if err != nil {
			t.Fatal(err)
		}

		now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
		var curTime atomic.Int64
		curTime.Store(now.UnixNano())
		query := &recordingExprRuleQuery{firingExpr: "up == 0"}
		const memberID = "refresh-member"
		evaluator := NewRuleEvaluator(RuleEvaluatorConfig{
			Store:           store,
			Query:           query,
			MemberID:        memberID,
			MetricsInterval: time.Second,
			GroupWait:       time.Second,
			RepeatInterval:  time.Hour,
			MaxConcurrency:  1,
			LeaseTTL:        time.Minute,
			MemberTTL:       time.Minute,
			Now:             func() time.Time { return time.Unix(0, curTime.Load()).UTC() },
		})

		// Cycle 1 & 2: fire and notify under the original expression.
		evaluator.evaluateCycle(ctx)
		curTime.Store(now.Add(2 * time.Second).UnixNano())
		evaluator.evaluateCycle(ctx)
		states, err := store.AlertRuleStates(ctx, rule.ID)
		if err != nil || len(states) != 1 || states[0].Status != "firing" {
			t.Fatalf("expected active firing state before update, got states=%#v err=%v", states, err)
		}
		evaluator.mu.Lock()
		initialEpoch := evaluator.leases[rule.ID].Epoch
		evaluator.mu.Unlock()

		// Capture a stale snapshot as evaluateCycle would before UpdateAlertRule commits.
		staleRules, err := store.AlertEvaluationRules(ctx)
		if err != nil || len(staleRules) != 1 || staleRules[0].Expression != "up == 0" {
			t.Fatalf("unexpected stale rules snapshot=%#v err=%v", staleRules, err)
		}
		query.Reset()

		// Commit expression change: advances & soft-expires lease, enqueues resolved notification, clears state.
		newExpr := "vector(0) > 0"
		resolvedRuleID := rule.ID
		if _, err := store.UpdateAlertRule(ctx, project.Slug, rule.ID, meta.AlertRuleUpdate{
			Expression:    &newExpr,
			ResolveFiring: true,
			BuildResolveNotifications: func(
				resolved []meta.AlertRuleState,
				enabledChannelIDs []int,
				resolvedAt time.Time,
			) ([]meta.NotificationOutboxEnqueue, error) {
				alerts := make([]Alert, 0, len(resolved))
				for _, state := range resolved {
					alerts = append(alerts, alertFromRuleState(state, "resolved", resolvedAt))
				}
				ruleView := rule
				ruleView.EnabledChannelIDs = enabledChannelIDs
				return nativeOutboxEntries(
					&ruleView,
					&rule.Project,
					[]Notification{{Project: project.Slug, Rule: rule.Name, Status: "resolved", Alerts: alerts}},
					resolvedAt,
					time.Hour,
				), nil
			},
		}); err != nil {
			t.Fatal(err)
		}

		// Now run evaluateRules with the stale snapshot after the interval is due:
		// AcquireAlertRuleLease obtains a fresh epoch, then must reload the new rule.
		curTime.Store(now.Add(4 * time.Second).UnixNano())
		evaluator.evaluateRules(ctx, staleRules, []ownership.MemberID{memberID}, evaluator.now().UTC())

		if seen := query.Expressions(); !slices.Equal(seen, []string{newExpr}) {
			t.Fatalf("evaluator queried stale expression after lease acquire: got=%v want=[%q]", seen, newExpr)
		}
		evaluator.mu.Lock()
		refreshedEpoch := evaluator.leases[resolvedRuleID].Epoch
		evaluator.mu.Unlock()
		if refreshedEpoch <= initialEpoch {
			t.Fatalf("expected refreshed lease epoch > %d, got %d", initialEpoch, refreshedEpoch)
		}
		states, err = store.AlertRuleStates(ctx, rule.ID)
		if err != nil || len(states) != 0 {
			t.Fatalf("stale snapshot revived alert states: states=%#v err=%v", states, err)
		}

		// A subsequent normal evaluation cycle must not emit a duplicate resolved notification.
		curTime.Store(now.Add(6 * time.Second).UnixNano())
		evaluator.evaluateCycle(ctx)

		outbox, err := store.ListNotificationOutbox(ctx, meta.NotificationOutboxListOptions{ProjectID: project.ID})
		if err != nil {
			t.Fatal(err)
		}
		var resolvedCount int
		for _, entry := range outbox {
			item, loadErr := store.NotificationOutboxItem(ctx, entry.ID)
			if loadErr != nil {
				t.Fatal(loadErr)
			}
			var n Notification
			if unmarshalErr := json.Unmarshal(item.Payload, &n); unmarshalErr != nil {
				t.Fatal(unmarshalErr)
			}
			if n.Status == "resolved" {
				resolvedCount++
			}
		}
		if resolvedCount != 1 {
			t.Fatalf(
				"expected exactly 1 resolved notification in outbox, got %d (total=%d)",
				resolvedCount,
				len(outbox),
			)
		}
	})

	t.Run("disabled paused or deleted rule in stale snapshot is skipped without evaluation", func(t *testing.T) {
		for _, action := range []string{"disable", "pause", "delete"} {
			t.Run(action, func(t *testing.T) {
				ctx := context.Background()
				store, err := meta.OpenStore(ctx, filepath.Join(t.TempDir(), "meta.db"))
				if err != nil {
					t.Fatal(err)
				}
				defer store.Close()
				project, err := store.CreateProject(ctx, "Ineligible", "ineligible")
				if err != nil {
					t.Fatal(err)
				}
				rule, err := store.CreateAlertRule(ctx, meta.AlertRuleCreate{
					ProjectSlug: project.Slug, Name: "StaleSkip", Signal: "metrics", Expression: "up == 0", For: "0s",
					Severity: "warning",
				})
				if err != nil {
					t.Fatal(err)
				}
				const memberID = "skip-member"
				if _, err := store.HeartbeatAlertEngine(ctx, memberID, time.Minute); err != nil {
					t.Fatal(err)
				}
				staleRules, err := store.AlertEvaluationRules(ctx)
				if err != nil || len(staleRules) != 1 {
					t.Fatalf("staleRules=%#v err=%v", staleRules, err)
				}
				switch action {
				case "disable":
					disabled := false
					if _, err := store.UpdateAlertRule(ctx, project.Slug, rule.ID, meta.AlertRuleUpdate{
						Enabled: &disabled,
					}); err != nil {
						t.Fatal(err)
					}
				case "pause":
					if err := store.PauseAlertRuleEvaluation(ctx, rule.ID); err != nil {
						t.Fatal(err)
					}
				case "delete":
					if err := store.DeleteAlertRule(ctx, project.Slug, rule.ID); err != nil {
						t.Fatal(err)
					}
				}
				query := &recordingExprRuleQuery{firingExpr: "up == 0"}
				evaluator := NewRuleEvaluator(RuleEvaluatorConfig{
					Store: store, Query: query, MemberID: memberID,
					MetricsInterval: time.Second, LeaseTTL: time.Minute, MemberTTL: time.Minute,
				})
				evaluator.evaluateRules(ctx, staleRules, []ownership.MemberID{memberID}, evaluator.now().UTC())
				if seen := query.Expressions(); len(seen) != 0 {
					t.Fatalf("ineligible rule after %s should not be queried, got=%v", action, seen)
				}
				evaluator.mu.Lock()
				_, held := evaluator.leases[rule.ID]
				evaluator.mu.Unlock()
				if held {
					t.Fatalf("evaluator retained local lease after %s", action)
				}
			})
		}
	})

	t.Run("signal change transferring ownership releases newly acquired lease", func(t *testing.T) {
		ctx := context.Background()
		store, err := meta.OpenStore(ctx, filepath.Join(t.TempDir(), "meta.db"))
		if err != nil {
			t.Fatal(err)
		}
		defer store.Close()
		project, err := store.CreateProject(ctx, "SignalMove", "signal-move")
		if err != nil {
			t.Fatal(err)
		}
		rule, err := store.CreateAlertRule(ctx, meta.AlertRuleCreate{
			ProjectSlug: project.Slug, Name: "MoveRule", Signal: "metrics", Expression: "up == 0", For: "0s",
			Severity: "warning",
		})
		if err != nil {
			t.Fatal(err)
		}

		// Pick two members so metrics maps to metricsOwner and logs maps to logsOwner.
		var metricsOwner, logsOwner ownership.MemberID
		for i := range 128 {
			a := ownership.MemberID(fmt.Sprintf("member-a-%d", i))
			b := ownership.MemberID(fmt.Sprintf("member-b-%d", i))
			o1, err1 := ownership.Owner(
				ownership.RuleKey{ProjectID: project.ID, Signal: "metrics", RuleID: rule.ID},
				[]ownership.MemberID{a, b},
			)
			o2, err2 := ownership.Owner(
				ownership.RuleKey{ProjectID: project.ID, Signal: "logs", RuleID: rule.ID},
				[]ownership.MemberID{a, b},
			)
			if err1 == nil && err2 == nil && o1 != o2 {
				metricsOwner, logsOwner = o1, o2
				break
			}
		}
		if metricsOwner == "" || logsOwner == "" {
			t.Fatal("failed to find distinct owners for metrics vs logs")
		}
		for _, member := range []ownership.MemberID{metricsOwner, logsOwner} {
			if _, err := store.HeartbeatAlertEngine(ctx, string(member), time.Minute); err != nil {
				t.Fatal(err)
			}
		}
		staleRules, err := store.AlertEvaluationRules(ctx)
		if err != nil || len(staleRules) != 1 {
			t.Fatalf("staleRules=%#v err=%v", staleRules, err)
		}

		logsSignal := "logs"
		if _, err := store.UpdateAlertRule(ctx, project.Slug, rule.ID, meta.AlertRuleUpdate{
			Signal: &logsSignal,
		}); err != nil {
			t.Fatal(err)
		}

		query := &recordingExprRuleQuery{firingExpr: "up == 0"}
		evaluator := NewRuleEvaluator(RuleEvaluatorConfig{
			Store: store, Query: query, MemberID: string(metricsOwner),
			MetricsInterval: time.Second, LogsInterval: time.Second, LeaseTTL: time.Minute, MemberTTL: time.Minute,
		})
		evaluator.evaluateRules(
			ctx,
			staleRules,
			[]ownership.MemberID{metricsOwner, logsOwner},
			evaluator.now().UTC(),
		)

		if seen := query.Expressions(); len(seen) != 0 {
			t.Fatalf("previous owner queried rule after signal ownership transfer: %v", seen)
		}
		evaluator.mu.Lock()
		_, held := evaluator.leases[rule.ID]
		evaluator.mu.Unlock()
		if held {
			t.Fatal("previous owner retained local lease after signal ownership transfer")
		}
		// New owner must be able to acquire the lease immediately because previous owner released it.
		if _, err := store.AcquireAlertRuleLease(ctx, rule.ID, string(logsOwner), time.Minute); err != nil {
			t.Fatalf("new owner failed to acquire lease released after ownership transfer: %v", err)
		}
	})
}

type recordingExprRuleQuery struct {
	mu          sync.Mutex
	firingExpr  string
	expressions []string
}

func (q *recordingExprRuleQuery) Evaluate(
	_ context.Context,
	rule *meta.AlertRuleView,
	project *meta.ProjectInfo,
	_ time.Time,
) ([]RuleSample, error) {
	q.mu.Lock()
	q.expressions = append(q.expressions, rule.Expression)
	q.mu.Unlock()
	if rule.Expression != q.firingExpr {
		return nil, nil
	}
	return []RuleSample{{
		Fingerprint: "recording-expr-fp",
		Labels:      map[string]string{"project_id": project.Slug, "alertname": rule.Name},
		Annotations: map[string]string{"summary": "active"},
		Value:       1,
	}}, nil
}

func (q *recordingExprRuleQuery) Expressions() []string {
	q.mu.Lock()
	defer q.mu.Unlock()
	return slices.Clone(q.expressions)
}

func (q *recordingExprRuleQuery) Reset() {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.expressions = nil
}

type exprAwareRuleQuery struct {
	firingExpr string
}

func (q *exprAwareRuleQuery) Evaluate(
	_ context.Context,
	rule *meta.AlertRuleView,
	project *meta.ProjectInfo,
	_ time.Time,
) ([]RuleSample, error) {
	if rule.Expression != q.firingExpr {
		return nil, nil
	}
	return []RuleSample{{
		Fingerprint: "expr-aware-fp",
		Labels:      map[string]string{"project_id": project.Slug, "alertname": rule.Name},
		Annotations: map[string]string{"summary": "active"},
		Value:       1,
	}}, nil
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

func TestNativeEvaluatorRuleEvaluationInterval(t *testing.T) {
	t.Run("override respected", func(t *testing.T) {
		ctx := context.Background()
		store, err := meta.OpenStore(ctx, filepath.Join(t.TempDir(), "meta.db"))
		if err != nil {
			t.Fatal(err)
		}
		defer store.Close()
		project, err := store.CreateProject(ctx, "EvalOverride", "eval-override")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := store.CreateAlertRule(ctx, meta.AlertRuleCreate{
			ProjectSlug:        project.Slug,
			Name:               "Override30s",
			Signal:             "metrics",
			Expression:         "up == 0",
			For:                "0s",
			EvaluationInterval: "30s",
			Severity:           "warning",
		}); err != nil {
			t.Fatal(err)
		}
		var cur atomic.Int64
		t0 := time.Date(2026, 11, 25, 10, 0, 0, 0, time.UTC)
		cur.Store(t0.UnixNano())
		query := &countingRuleQuery{}
		eval := NewRuleEvaluator(RuleEvaluatorConfig{
			Store:           store,
			Query:           query,
			MemberID:        "override-member",
			TickInterval:    5 * time.Second,
			MetricsInterval: 5 * time.Second,
			LeaseTTL:        time.Minute,
			MemberTTL:       time.Minute,
			Now:             func() time.Time { return time.Unix(0, cur.Load()).UTC() },
		})

		eval.evaluateCycle(ctx)
		if got := query.calls.Load(); got != 1 {
			t.Fatalf("initial calls=%d want 1", got)
		}
		for _, offset := range []time.Duration{5 * time.Second, 15 * time.Second, 29 * time.Second} {
			cur.Store(t0.Add(offset).UnixNano())
			eval.evaluateCycle(ctx)
			if got := query.calls.Load(); got != 1 {
				t.Fatalf("at +%s calls=%d want 1 (before 30s interval)", offset, got)
			}
		}
		cur.Store(t0.Add(30 * time.Second).UnixNano())
		eval.evaluateCycle(ctx)
		if got := query.calls.Load(); got != 2 {
			t.Fatalf("at +30s calls=%d want 2", got)
		}
	})

	t.Run("cross-replica edit via store shortens and lengthens interval", func(t *testing.T) {
		ctx := context.Background()
		store, err := meta.OpenStore(ctx, filepath.Join(t.TempDir(), "meta.db"))
		if err != nil {
			t.Fatal(err)
		}
		defer store.Close()
		project, err := store.CreateProject(ctx, "ReplicaEdit", "replica-edit")
		if err != nil {
			t.Fatal(err)
		}
		rule, err := store.CreateAlertRule(ctx, meta.AlertRuleCreate{
			ProjectSlug:        project.Slug,
			Name:               "ReplicaEditRule",
			Signal:             "metrics",
			Expression:         "up == 0",
			For:                "0s",
			EvaluationInterval: "1h",
			Severity:           "warning",
		})
		if err != nil {
			t.Fatal(err)
		}
		var cur atomic.Int64
		t0 := time.Date(2026, 11, 25, 11, 0, 0, 0, time.UTC)
		cur.Store(t0.UnixNano())
		query := &countingRuleQuery{}
		eval := NewRuleEvaluator(RuleEvaluatorConfig{
			Store:           store,
			Query:           query,
			MemberID:        "replica-edit-member",
			TickInterval:    5 * time.Second,
			MetricsInterval: 5 * time.Second,
			LeaseTTL:        2 * time.Hour,
			MemberTTL:       2 * time.Hour,
			Now:             func() time.Time { return time.Unix(0, cur.Load()).UTC() },
		})

		eval.evaluateCycle(ctx)
		if got := query.calls.Load(); got != 1 {
			t.Fatalf("initial calls=%d want 1", got)
		}

		// Simulate another replica shortening 1h -> 10s: next tick at +10s evaluates immediately.
		tenSec := "10s"
		if _, err := store.UpdateAlertRule(ctx, project.Slug, rule.ID, meta.AlertRuleUpdate{
			EvaluationInterval: &tenSec,
		}); err != nil {
			t.Fatal(err)
		}
		cur.Store(t0.Add(10 * time.Second).UnixNano())
		eval.evaluateCycle(ctx)
		if got := query.calls.Load(); got != 2 {
			t.Fatalf("after shortening 1h->10s calls=%d want 2", got)
		}

		// Simulate another replica lengthening 10s -> 1h: +20s defers, +10s+1h evaluates.
		oneHour := "1h"
		if _, err := store.UpdateAlertRule(ctx, project.Slug, rule.ID, meta.AlertRuleUpdate{
			EvaluationInterval: &oneHour,
		}); err != nil {
			t.Fatal(err)
		}
		cur.Store(t0.Add(20 * time.Second).UnixNano())
		eval.evaluateCycle(ctx)
		if got := query.calls.Load(); got != 2 {
			t.Fatalf("after lengthening 10s->1h at +20s calls=%d want 2", got)
		}
		cur.Store(t0.Add(10*time.Second + time.Hour).UnixNano())
		eval.evaluateCycle(ctx)
		if got := query.calls.Load(); got != 3 {
			t.Fatalf("after lengthening at +10s+1h calls=%d want 3", got)
		}
	})

	t.Run("due slack with 5s tick", func(t *testing.T) {
		ctx := context.Background()
		store, err := meta.OpenStore(ctx, filepath.Join(t.TempDir(), "meta.db"))
		if err != nil {
			t.Fatal(err)
		}
		defer store.Close()
		project, err := store.CreateProject(ctx, "Slack", "slack-eval")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := store.CreateAlertRule(ctx, meta.AlertRuleCreate{
			ProjectSlug:        project.Slug,
			Name:               "SlackRule",
			Signal:             "metrics",
			Expression:         "up == 0",
			For:                "0s",
			EvaluationInterval: "30s",
			Severity:           "warning",
		}); err != nil {
			t.Fatal(err)
		}
		var cur atomic.Int64
		t0 := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
		cur.Store(t0.UnixNano())
		query := &countingRuleQuery{}
		eval := NewRuleEvaluator(RuleEvaluatorConfig{
			Store:           store,
			Query:           query,
			MemberID:        "slack-member",
			TickInterval:    5 * time.Second,
			MetricsInterval: 5 * time.Second,
			LeaseTTL:        time.Minute,
			MemberTTL:       time.Minute,
			Now:             func() time.Time { return time.Unix(0, cur.Load()).UTC() },
		})

		eval.evaluateCycle(ctx)
		if got := query.calls.Load(); got != 1 {
			t.Fatalf("initial calls=%d want 1", got)
		}
		// 300ms before due exceeds 250ms slack -> not due.
		cur.Store(t0.Add(30*time.Second - 300*time.Millisecond).UnixNano())
		eval.evaluateCycle(ctx)
		if got := query.calls.Load(); got != 1 {
			t.Fatalf("at -300ms calls=%d want 1", got)
		}
		// 100ms before due is within 250ms slack -> due.
		cur.Store(t0.Add(30*time.Second - 100*time.Millisecond).UnixNano())
		eval.evaluateCycle(ctx)
		if got := query.calls.Load(); got != 2 {
			t.Fatalf("at -100ms calls=%d want 2", got)
		}
	})

	t.Run("due slack aligns for transition when for equals interval", func(t *testing.T) {
		ctx := context.Background()
		store, err := meta.OpenStore(ctx, filepath.Join(t.TempDir(), "meta.db"))
		if err != nil {
			t.Fatal(err)
		}
		defer store.Close()
		project, err := store.CreateProject(ctx, "SlackForAlign", "slack-for-align")
		if err != nil {
			t.Fatal(err)
		}
		rule30s, err := store.CreateAlertRule(ctx, meta.AlertRuleCreate{
			ProjectSlug:        project.Slug,
			Name:               "For30sInterval30s",
			Signal:             "metrics",
			Expression:         "up == 0",
			For:                "30s",
			EvaluationInterval: "30s",
			Severity:           "warning",
		})
		if err != nil {
			t.Fatal(err)
		}
		rule1m, err := store.CreateAlertRule(ctx, meta.AlertRuleCreate{
			ProjectSlug:        project.Slug,
			Name:               "For1mInterval30s",
			Signal:             "metrics",
			Expression:         "up == 0",
			For:                "1m",
			EvaluationInterval: "30s",
			Severity:           "warning",
		})
		if err != nil {
			t.Fatal(err)
		}

		var cur atomic.Int64
		t0 := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
		cur.Store(t0.UnixNano())
		query := &perRuleQuery{
			byRule: map[int][]RuleSample{
				rule30s.ID: {{
					Fingerprint: "fp-30s",
					Labels:      map[string]string{"project_id": project.Slug, "alertname": "For30sInterval30s"},
					Annotations: map[string]string{},
					Value:       1,
				}},
				rule1m.ID: {{
					Fingerprint: "fp-1m",
					Labels:      map[string]string{"project_id": project.Slug, "alertname": "For1mInterval30s"},
					Annotations: map[string]string{},
					Value:       1,
				}},
			},
		}
		eval := NewRuleEvaluator(RuleEvaluatorConfig{
			Store:           store,
			Query:           query,
			MemberID:        "slack-for-member",
			TickInterval:    5 * time.Second,
			MetricsInterval: 5 * time.Second,
			LeaseTTL:        2 * time.Minute,
			MemberTTL:       2 * time.Minute,
			Now:             func() time.Time { return time.Unix(0, cur.Load()).UTC() },
		})

		eval.evaluateCycle(ctx)
		states30s, err := store.AlertRuleStates(ctx, rule30s.ID)
		if err != nil || len(states30s) != 1 || states30s[0].Status != "pending" ||
			states30s[0].PendingSince == nil || !states30s[0].PendingSince.Equal(t0) {
			t.Fatalf("t0 rule30s states=%#v err=%v", states30s, err)
		}

		// 300ms before due exceeds 250ms slack -> skipped.
		cur.Store(t0.Add(30*time.Second - 300*time.Millisecond).UnixNano())
		eval.evaluateCycle(ctx)
		states30s, err = store.AlertRuleStates(ctx, rule30s.ID)
		if err != nil || len(states30s) != 1 || !states30s[0].LastEvaluatedAt.Equal(t0) {
			t.Fatalf("at +30s-300ms rule30s should not evaluate, states=%#v err=%v", states30s, err)
		}

		// 100ms before due is within 250ms slack -> rule30s transitions to firing at anchor t0+30s.
		cur.Store(t0.Add(30*time.Second - 100*time.Millisecond).UnixNano())
		eval.evaluateCycle(ctx)
		states30s, err = store.AlertRuleStates(ctx, rule30s.ID)
		if err != nil || len(states30s) != 1 || states30s[0].Status != "firing" ||
			states30s[0].FiringSince == nil || !states30s[0].FiringSince.Equal(t0.Add(30*time.Second)) ||
			!states30s[0].LastEvaluatedAt.Equal(t0.Add(30*time.Second)) {
			t.Fatalf("at +30s-100ms rule30s states=%#v err=%v want firing at +30s", states30s, err)
		}
		// rule1m (for=2*interval) stays pending at +30s anchor.
		states1m, err := store.AlertRuleStates(ctx, rule1m.ID)
		if err != nil || len(states1m) != 1 || states1m[0].Status != "pending" ||
			!states1m[0].LastEvaluatedAt.Equal(t0.Add(30*time.Second)) {
			t.Fatalf("at +30s-100ms rule1m states=%#v err=%v want pending at +30s", states1m, err)
		}

		// Consecutive -100ms early tick at +60s-100ms transitions rule1m to firing without cumulative drift.
		cur.Store(t0.Add(60*time.Second - 100*time.Millisecond).UnixNano())
		eval.evaluateCycle(ctx)
		states1m, err = store.AlertRuleStates(ctx, rule1m.ID)
		if err != nil || len(states1m) != 1 || states1m[0].Status != "firing" ||
			states1m[0].FiringSince == nil || !states1m[0].FiringSince.Equal(t0.Add(60*time.Second)) ||
			!states1m[0].LastEvaluatedAt.Equal(t0.Add(60*time.Second)) {
			t.Fatalf("at +60s-100ms rule1m states=%#v err=%v want firing at +60s", states1m, err)
		}
	})

	t.Run("group_wait follow-up triggers once before long interval", func(t *testing.T) {
		ctx := context.Background()
		store, err := meta.OpenStore(ctx, filepath.Join(t.TempDir(), "meta.db"))
		if err != nil {
			t.Fatal(err)
		}
		defer store.Close()
		project, err := store.CreateProject(ctx, "GroupWake", "group-wake")
		if err != nil {
			t.Fatal(err)
		}
		channel, err := store.CreateNotificationChannel(ctx, meta.NotificationChannelCreate{
			Type: "webhook", Name: "wake-hook", Config: map[string]any{"url": "https://example.invalid/hook"},
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := store.CreateAlertRule(ctx, meta.AlertRuleCreate{
			ProjectSlug:        project.Slug,
			Name:               "LongIntervalRule",
			Signal:             "metrics",
			Expression:         "up == 0",
			For:                "0s",
			EvaluationInterval: "10m",
			Severity:           "critical",
			ChannelIDs:         []int{channel.ID},
		}); err != nil {
			t.Fatal(err)
		}

		var cur atomic.Int64
		t0 := time.Date(2026, 11, 25, 13, 0, 0, 0, time.UTC)
		cur.Store(t0.UnixNano())
		query := &countingRuleQuery{
			samples: []RuleSample{{
				Fingerprint: "wake-fp",
				Labels:      map[string]string{"project_id": project.Slug, "alertname": "LongIntervalRule"},
				Annotations: map[string]string{},
				Value:       1,
			}},
		}
		eval := NewRuleEvaluator(RuleEvaluatorConfig{
			Store:           store,
			Query:           query,
			MemberID:        "wake-member",
			TickInterval:    5 * time.Second,
			MetricsInterval: 5 * time.Second,
			GroupWait:       30 * time.Second,
			RepeatInterval:  4 * time.Hour,
			LeaseTTL:        time.Hour,
			MemberTTL:       time.Hour,
			Now:             func() time.Time { return time.Unix(0, cur.Load()).UTC() },
		})

		// T0: initial evaluation enters firing, within group_wait (30s) -> no outbox entry yet, wakeAt=T0+30s.
		eval.evaluateCycle(ctx)
		if got := query.calls.Load(); got != 1 {
			t.Fatalf("T0 calls=%d want 1", got)
		}
		outbox, err := store.ListNotificationOutbox(ctx, meta.NotificationOutboxListOptions{ProjectID: project.ID})
		if err != nil || len(outbox) != 0 {
			t.Fatalf("T0 outbox=%#v err=%v want empty", outbox, err)
		}

		// T0+5s, T0+25s: not due yet.
		for _, offset := range []time.Duration{5 * time.Second, 25 * time.Second} {
			cur.Store(t0.Add(offset).UnixNano())
			eval.evaluateCycle(ctx)
			if got := query.calls.Load(); got != 1 {
				t.Fatalf("at +%s calls=%d want 1", offset, got)
			}
		}

		// T0+30s: group_wait wake-up triggers single follow-up evaluation and enqueues notification.
		cur.Store(t0.Add(30 * time.Second).UnixNano())
		eval.evaluateCycle(ctx)
		if got := query.calls.Load(); got != 2 {
			t.Fatalf("at +30s calls=%d want 2", got)
		}
		outbox, err = store.ListNotificationOutbox(ctx, meta.NotificationOutboxListOptions{ProjectID: project.ID})
		if err != nil || len(outbox) != 1 {
			t.Fatalf("at +30s outbox len=%d err=%v want 1", len(outbox), err)
		}

		// After wake-up notification, no extra evaluation runs until T0+30s+10m.
		for _, offset := range []time.Duration{35 * time.Second, 5 * time.Minute, 10 * time.Minute} {
			cur.Store(t0.Add(offset).UnixNano())
			eval.evaluateCycle(ctx)
			if got := query.calls.Load(); got != 2 {
				t.Fatalf("at +%s calls=%d want 2 (no extra evals before next regular interval)", offset, got)
			}
		}
		cur.Store(t0.Add(30*time.Second + 10*time.Minute).UnixNano())
		eval.evaluateCycle(ctx)
		if got := query.calls.Load(); got != 3 {
			t.Fatalf("at +10m30s calls=%d want 3", got)
		}
	})

	t.Run("group_wait follow-up within due slack notifies without extra cycle", func(t *testing.T) {
		ctx := context.Background()
		store, err := meta.OpenStore(ctx, filepath.Join(t.TempDir(), "meta.db"))
		if err != nil {
			t.Fatal(err)
		}
		defer store.Close()
		project, err := store.CreateProject(ctx, "GroupWakeSlack", "group-wake-slack")
		if err != nil {
			t.Fatal(err)
		}
		channel, err := store.CreateNotificationChannel(ctx, meta.NotificationChannelCreate{
			Type: "webhook", Name: "wake-slack-hook", Config: map[string]any{"url": "https://example.invalid/hook"},
		})
		if err != nil {
			t.Fatal(err)
		}
		rule, err := store.CreateAlertRule(ctx, meta.AlertRuleCreate{
			ProjectSlug:        project.Slug,
			Name:               "LongIntervalSlackRule",
			Signal:             "metrics",
			Expression:         "up == 0",
			For:                "0s",
			EvaluationInterval: "10m",
			Severity:           "critical",
			ChannelIDs:         []int{channel.ID},
		})
		if err != nil {
			t.Fatal(err)
		}

		var cur atomic.Int64
		t0 := time.Date(2026, 11, 25, 13, 30, 0, 0, time.UTC)
		cur.Store(t0.UnixNano())
		query := &countingRuleQuery{
			samples: []RuleSample{{
				Fingerprint: "wake-slack-fp",
				Labels:      map[string]string{"project_id": project.Slug, "alertname": "LongIntervalSlackRule"},
				Annotations: map[string]string{},
				Value:       1,
			}},
		}
		eval := NewRuleEvaluator(RuleEvaluatorConfig{
			Store:           store,
			Query:           query,
			MemberID:        "wake-slack-member",
			TickInterval:    5 * time.Second,
			MetricsInterval: 5 * time.Second,
			GroupWait:       30 * time.Second,
			RepeatInterval:  4 * time.Hour,
			LeaseTTL:        time.Hour,
			MemberTTL:       time.Hour,
			Now:             func() time.Time { return time.Unix(0, cur.Load()).UTC() },
		})

		eval.evaluateCycle(ctx)
		if got := query.calls.Load(); got != 1 {
			t.Fatalf("T0 calls=%d want 1", got)
		}
		outbox, err := store.ListNotificationOutbox(ctx, meta.NotificationOutboxListOptions{ProjectID: project.ID})
		if err != nil || len(outbox) != 0 {
			t.Fatalf("T0 outbox=%#v err=%v want empty", outbox, err)
		}

		// 300ms before wakeAt exceeds 250ms slack -> skipped.
		cur.Store(t0.Add(30*time.Second - 300*time.Millisecond).UnixNano())
		eval.evaluateCycle(ctx)
		if got := query.calls.Load(); got != 1 {
			t.Fatalf("at +30s-300ms calls=%d want 1", got)
		}

		// 100ms before wakeAt is within 250ms slack -> notifies immediately at anchor t0+30s.
		cur.Store(t0.Add(30*time.Second - 100*time.Millisecond).UnixNano())
		eval.evaluateCycle(ctx)
		if got := query.calls.Load(); got != 2 {
			t.Fatalf("at +30s-100ms calls=%d want 2", got)
		}
		outbox, err = store.ListNotificationOutbox(ctx, meta.NotificationOutboxListOptions{ProjectID: project.ID})
		if err != nil || len(outbox) != 1 {
			t.Fatalf("at +30s-100ms outbox len=%d err=%v want 1", len(outbox), err)
		}
		states, err := store.AlertRuleStates(ctx, rule.ID)
		if err != nil || len(states) != 1 || states[0].LastNotifiedAt == nil ||
			!states[0].LastNotifiedAt.Equal(t0.Add(30*time.Second)) {
			t.Fatalf("at +30s-100ms states=%#v err=%v want LastNotifiedAt=+30s", states, err)
		}

		// No extra evaluation at +35s; next regular evaluation occurs at +30s+10m.
		cur.Store(t0.Add(35 * time.Second).UnixNano())
		eval.evaluateCycle(ctx)
		if got := query.calls.Load(); got != 2 {
			t.Fatalf("at +35s calls=%d want 2", got)
		}
		cur.Store(t0.Add(30*time.Second + 10*time.Minute).UnixNano())
		eval.evaluateCycle(ctx)
		if got := query.calls.Load(); got != 3 {
			t.Fatalf("at +10m30s calls=%d want 3", got)
		}
	})

	t.Run("corrupted stored evaluation_interval falls back to signal default", func(t *testing.T) {
		ctx := context.Background()
		store, err := meta.OpenStore(ctx, filepath.Join(t.TempDir(), "meta.db"))
		if err != nil {
			t.Fatal(err)
		}
		defer store.Close()
		project, err := store.CreateProject(ctx, "CorruptInterval", "corrupt-interval")
		if err != nil {
			t.Fatal(err)
		}
		badRule, err := store.CreateAlertRule(ctx, meta.AlertRuleCreate{
			ProjectSlug:        project.Slug,
			Name:               "CorruptMetrics",
			Signal:             "metrics",
			Expression:         "up == 0",
			For:                "0s",
			EvaluationInterval: "not-a-duration",
			Severity:           "warning",
		})
		if err != nil {
			t.Fatal(err)
		}
		zeroLogsRule, err := store.CreateAlertRule(ctx, meta.AlertRuleCreate{
			ProjectSlug:        project.Slug,
			Name:               "ZeroLogs",
			Signal:             "logs",
			Expression:         "* | stats count() as c",
			For:                "0s",
			EvaluationInterval: "0s",
			Severity:           "warning",
		})
		if err != nil {
			t.Fatal(err)
		}

		var buf bytes.Buffer
		prevLogger := slog.Default()
		slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
		defer slog.SetDefault(prevLogger)

		var cur atomic.Int64
		t0 := time.Date(2026, 11, 25, 14, 0, 0, 0, time.UTC)
		cur.Store(t0.UnixNano())
		perRule := &countingPerRuleQuery{}
		eval := NewRuleEvaluator(RuleEvaluatorConfig{
			Store:           store,
			Query:           perRule,
			MemberID:        "corrupt-member",
			TickInterval:    5 * time.Second,
			MetricsInterval: 15 * time.Second,
			LogsInterval:    45 * time.Second,
			LeaseTTL:        2 * time.Minute,
			MemberTTL:       2 * time.Minute,
			Now:             func() time.Time { return time.Unix(0, cur.Load()).UTC() },
		})

		eval.evaluateCycle(ctx)
		if perRule.count(badRule.ID) != 1 || perRule.count(zeroLogsRule.ID) != 1 {
			t.Fatalf("T0 counts bad=%d zero=%d", perRule.count(badRule.ID), perRule.count(zeroLogsRule.ID))
		}
		logsOut := buf.String()
		if !strings.Contains(logsOut, fmt.Sprintf("rule_id=%d", badRule.ID)) ||
			!strings.Contains(logsOut, "value=not-a-duration") ||
			!strings.Contains(logsOut, fmt.Sprintf("rule_id=%d", zeroLogsRule.ID)) ||
			!strings.Contains(logsOut, "value=0s") {
			t.Fatalf("expected slog.Warn with rule_id and corrupted value, got:\n%s", logsOut)
		}

		// Before 15s: neither re-evaluates. At +15s: metrics falls back to 15s, logs waits until 45s.
		cur.Store(t0.Add(10 * time.Second).UnixNano())
		eval.evaluateCycle(ctx)
		if perRule.count(badRule.ID) != 1 || perRule.count(zeroLogsRule.ID) != 1 {
			t.Fatalf("+10s counts bad=%d zero=%d want 1/1", perRule.count(badRule.ID), perRule.count(zeroLogsRule.ID))
		}
		cur.Store(t0.Add(15 * time.Second).UnixNano())
		eval.evaluateCycle(ctx)
		if perRule.count(badRule.ID) != 2 || perRule.count(zeroLogsRule.ID) != 1 {
			t.Fatalf("+15s counts bad=%d zero=%d want 2/1", perRule.count(badRule.ID), perRule.count(zeroLogsRule.ID))
		}
		cur.Store(t0.Add(45 * time.Second).UnixNano())
		eval.evaluateCycle(ctx)
		if perRule.count(zeroLogsRule.ID) != 2 {
			t.Fatalf("+45s logs count=%d want 2", perRule.count(zeroLogsRule.ID))
		}

		// Repeated cycles and list-API EffectiveEvaluationInterval calls must deduplicate warnings.
		_ = eval.ruleInterval(&badRule)
		logsOut = buf.String()
		if got := strings.Count(logsOut, fmt.Sprintf("rule_id=%d", badRule.ID)); got != 1 {
			t.Fatalf("badRule warn count=%d want 1, logs:\n%s", got, logsOut)
		}
		if got := strings.Count(logsOut, fmt.Sprintf("rule_id=%d", zeroLogsRule.ID)); got != 1 {
			t.Fatalf("zeroLogsRule warn count=%d want 1, logs:\n%s", got, logsOut)
		}

		// Clearing to a valid interval re-arms warning if the rule is later corrupted again,
		// and a distinct corrupted value also logs once.
		cleanRule := badRule
		cleanRule.EvaluationInterval = ""
		_ = eval.ruleInterval(&cleanRule)
		_ = eval.ruleInterval(&badRule)
		if got := strings.Count(buf.String(), fmt.Sprintf("rule_id=%d", badRule.ID)); got != 2 {
			t.Fatalf("after reset+recorrupt warn count=%d want 2, logs:\n%s", got, buf.String())
		}
		otherBad := badRule
		otherBad.EvaluationInterval = "still-bad"
		_ = eval.ruleInterval(&otherBad)
		if got := strings.Count(buf.String(), fmt.Sprintf("rule_id=%d", badRule.ID)); got != 3 {
			t.Fatalf("after distinct corrupt value warn count=%d want 3, logs:\n%s", got, buf.String())
		}
	})
}

type countingPerRuleQuery struct {
	mu     sync.Mutex
	counts map[int]int
}

func (q *countingPerRuleQuery) Evaluate(
	_ context.Context,
	rule *meta.AlertRuleView,
	_ *meta.ProjectInfo,
	_ time.Time,
) ([]RuleSample, error) {
	q.mu.Lock()
	if q.counts == nil {
		q.counts = make(map[int]int)
	}
	q.counts[rule.ID]++
	q.mu.Unlock()
	return nil, nil
}

func (q *countingPerRuleQuery) count(ruleID int) int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.counts[ruleID]
}
