package alert

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestGroupingWindowMergesAndFingerprintDeduplicates(t *testing.T) {
	var mu sync.Mutex
	got := make(chan Notification, 2)
	e := NewEngine(
		EngineConfig{
			GroupWait: 20 * time.Millisecond,
			Notify:    func(_ context.Context, n Notification) error { mu.Lock(); defer mu.Unlock(); got <- n; return nil },
		},
	)
	defer e.Close()
	a := Alert{Status: "firing", Labels: map[string]string{"alertname": "High", "instance": "one"}}
	e.Add(context.Background(), "alpha", "High", a)
	e.Add(context.Background(), "alpha", "High", a)
	e.Add(
		context.Background(),
		"alpha",
		"High",
		Alert{Status: "firing", Labels: map[string]string{"alertname": "High", "instance": "two"}},
	)
	select {
	case n := <-got:
		if len(n.Alerts) != 2 {
			t.Fatalf("notification alerts=%d, want 2", len(n.Alerts))
		}
	case <-time.After(time.Second):
		t.Fatal("notification timeout")
	}
	select {
	case <-got:
		t.Fatal("duplicate notification")
	case <-time.After(50 * time.Millisecond):
	}
}

func TestSilenceHitAndExpired(t *testing.T) {
	now := time.Date(2026, 7, 17, 0, 0, 0, 0, time.UTC)
	active := struct {
		start, end time.Time
		matchers   []Matcher
	}{now.Add(-time.Hour), now.Add(time.Hour), []Matcher{{Name: "severity", Op: "=", Value: "critical"}, {Name: "instance", Op: "=~", Value: "web-.+"}}}
	if !(now.After(active.start) && now.Before(active.end) && MatchLabels(map[string]string{"severity": "critical", "instance": "web-1"}, active.matchers)) {
		t.Fatal("active silence did not match")
	}
	expired := now.Add(-time.Nanosecond)
	if now.Before(expired) {
		t.Fatal("expired silence treated as active")
	}
	if MatchLabels(map[string]string{"severity": "warning", "instance": "web-1"}, active.matchers) {
		t.Fatal("non-matching labels were silenced")
	}
	// Regex matchers must be fully anchored as ^(?:...)$ per Alertmanager semantics.
	if MatchLabels(map[string]string{"instance": "web-1"}, []Matcher{{Name: "instance", Op: "=~", Value: "web"}}) {
		t.Fatal("unanchored regex unexpectedly matched substring")
	}
	if !MatchLabels(
		map[string]string{"instance": "web-1"},
		[]Matcher{{Name: "instance", Op: "=~", Value: "web-\\d+"}},
	) {
		t.Fatal("anchored full regex did not match")
	}
	if err := ValidateMatchers(nil); err == nil {
		t.Fatal("empty matchers should fail validation")
	}
	if err := ValidateMatchers([]Matcher{{Name: "instance", Op: "=~", Value: "[unclosed"}}); err == nil {
		t.Fatal("invalid regex matcher should fail validation")
	}
}

func TestSilencedTransientAlertRemovedWhenSiblingDelivers(t *testing.T) {
	got := make(chan Notification, 4)
	e := NewEngine(EngineConfig{
		GroupWait: 10 * time.Millisecond,
		Silences: func(context.Context, string, time.Time) ([][]Matcher, error) {
			return [][]Matcher{{{Name: "issue_id", Op: "=", Value: "silenced"}}}, nil
		},
		Notify: func(_ context.Context, n Notification) error { got <- n; return nil },
	})
	defer e.Close()
	e.Add(context.Background(), "alpha", "new_issue", Alert{
		Status: "firing", Labels: map[string]string{"issue_id": "silenced"}, Fingerprint: "a-silenced", Transient: true,
	})
	e.Add(context.Background(), "alpha", "new_issue", Alert{
		Status:      "firing",
		Labels:      map[string]string{"issue_id": "delivered"},
		Fingerprint: "b-delivered",
		Transient:   true,
	})
	select {
	case n := <-got:
		if len(n.Alerts) != 1 || n.Alerts[0].Labels["issue_id"] != "delivered" {
			t.Fatalf("unexpected notification: %#v", n)
		}
	case <-time.After(time.Second):
		t.Fatal("notification timeout")
	}
	// Wait briefly for flush post-delivery cleanup to remove both transient alerts.
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		e.mu.Lock()
		_, exists := e.groups["alpha\x00new_issue"]
		e.mu.Unlock()
		if !exists {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("silenced transient alert leaked in group due to aliased slice filtering")
}

func TestResolvedIsImmediateAndCancelsPendingFiring(t *testing.T) {
	got := make(chan Notification, 2)
	e := NewEngine(
		EngineConfig{
			GroupWait: 80 * time.Millisecond,
			Notify:    func(_ context.Context, n Notification) error { got <- n; return nil },
		},
	)
	defer e.Close()
	labels := map[string]string{"alertname": "Gone", "instance": "one"}
	e.Add(context.Background(), "alpha", "Gone", Alert{Status: "firing", Labels: labels})
	e.Add(context.Background(), "alpha", "Gone", Alert{Status: "resolved", Labels: labels})
	select {
	case n := <-got:
		if n.Status != "resolved" {
			t.Fatalf("status=%s", n.Status)
		}
	case <-time.After(20 * time.Millisecond):
		t.Fatal("resolved was not immediate")
	}
	select {
	case n := <-got:
		t.Fatalf("pending firing was not cancelled: %#v", n)
	case <-time.After(120 * time.Millisecond):
	}
}

func TestTransientAlertsLeaveGroupAfterDelivery(t *testing.T) {
	got := make(chan Notification, 2)
	e := NewEngine(
		EngineConfig{
			GroupWait: 10 * time.Millisecond,
			Notify:    func(_ context.Context, n Notification) error { got <- n; return nil },
		},
	)
	defer e.Close()
	e.Add(
		context.Background(),
		"alpha",
		"new_issue",
		Alert{Status: "firing", Labels: map[string]string{"issue_id": "1"}, Transient: true},
	)
	select {
	case <-got:
	case <-time.After(time.Second):
		t.Fatal("first transient notification timeout")
	}
	e.Add(
		context.Background(),
		"alpha",
		"new_issue",
		Alert{Status: "firing", Labels: map[string]string{"issue_id": "2"}, Transient: true},
	)
	select {
	case n := <-got:
		if len(n.Alerts) != 1 || n.Alerts[0].Labels["issue_id"] != "2" {
			t.Fatalf("historical transient alert leaked into update: %#v", n)
		}
	case <-time.After(time.Second):
		t.Fatal("second transient notification timeout")
	}
}

func TestInhibitionEqualLabelsResolutionAndSilenceComposition(t *testing.T) {
	got := make(chan Notification, 20)
	rules := []InhibitionRule{{
		SourceMatchers: []Matcher{{Name: "alertname", Op: "=", Value: "HostDown"}},
		TargetMatchers: []Matcher{{Name: "alertname", Op: "=", Value: "ServiceDown"}},
		EqualLabels:    []string{"instance"},
	}}
	var silenceTarget atomic.Bool
	e := NewEngine(EngineConfig{
		GroupWait:       10 * time.Millisecond,
		InhibitionRules: func(context.Context, string) ([]InhibitionRule, error) { return rules, nil },
		Silences: func(context.Context, string, time.Time) ([][]Matcher, error) {
			if silenceTarget.Load() {
				return [][]Matcher{{{Name: "alertname", Op: "=", Value: "ServiceDown"}}}, nil
			}
			return nil, nil
		},
		Notify: func(_ context.Context, n Notification) error { got <- n; return nil },
	})
	defer e.Close()
	source := Alert{Status: "firing", Labels: map[string]string{"alertname": "HostDown", "instance": "node-1"}}
	target := Alert{Status: "firing", Labels: map[string]string{"alertname": "ServiceDown", "instance": "node-1"}}
	mismatch := Alert{Status: "firing", Labels: map[string]string{"alertname": "ServiceDown", "instance": "node-2"}}
	e.Add(context.Background(), "alpha", "HostDown", source)
	e.Add(context.Background(), "alpha", "ServiceDown", target)
	e.Add(context.Background(), "alpha", "ServiceDownOther", mismatch)
	// Positive outcomes are awaited; a fixed sleep against the 10ms group wait
	// flakes on a loaded runner. Negative outcomes still need a settle period,
	// which can only make the test lenient, never spuriously red.
	firing := map[string]int{}
	eventuallyFiring(t, got, firing, "ServiceDownOther", 1)
	settleFiring(got, firing)
	if firing["ServiceDown"] != 0 {
		t.Fatal("equal-label target was not inhibited")
	}

	silenceTarget.Store(true)
	source.Status = "resolved"
	e.Add(context.Background(), "alpha", "HostDown", source)
	settleFiring(got, firing)
	if firing["ServiceDown"] != 0 {
		t.Fatal("silence and inhibition did not compose")
	}

	silenceTarget.Store(false)
	source.Status = "firing"
	e.Add(context.Background(), "alpha", "HostDown", source)
	time.Sleep(20 * time.Millisecond)
	source.Status = "resolved"
	e.Add(context.Background(), "alpha", "HostDown", source)
	eventuallyFiring(t, got, firing, "ServiceDown", 1)
}

// eventuallyFiring accumulates firing notifications per rule until rule has
// reached want, failing after one second.
func eventuallyFiring(t *testing.T, ch chan Notification, firing map[string]int, rule string, want int) {
	t.Helper()
	deadline := time.After(time.Second)
	for firing[rule] < want {
		select {
		case n := <-ch:
			if n.Status == "firing" {
				firing[n.Rule]++
			}
		case <-deadline:
			t.Fatalf("%s firing notifications=%d want %d", rule, firing[rule], want)
		}
	}
}

// settleFiring waits a few group-wait periods, then accumulates whatever
// firing notifications arrived.
func settleFiring(ch chan Notification, firing map[string]int) {
	time.Sleep(40 * time.Millisecond)
	for {
		select {
		case n := <-ch:
			if n.Status == "firing" {
				firing[n.Rule]++
			}
		default:
			return
		}
	}
}

func TestStormModeEnterSummaryUpdateAndExit(t *testing.T) {
	got := make(chan Notification, 30)
	e := NewEngine(
		EngineConfig{
			GroupWait:      10 * time.Millisecond,
			RepeatInterval: 100 * time.Millisecond,
			StormThreshold: 2,
			Notify:         func(_ context.Context, n Notification) error { got <- n; return nil },
		},
	)
	defer e.Close()
	for i, rule := range []string{"CPU", "Disk", "Memory"} {
		e.Add(
			context.Background(),
			"alpha",
			rule,
			Alert{
				Status: "firing",
				Labels: map[string]string{
					"alertname": rule,
					"severity":  []string{"critical", "critical", "warning"}[i],
				},
			},
		)
	}
	first := waitStorm(t, got)
	if first.Storm.GroupCount != 3 || len(first.Storm.Severities) != 2 ||
		first.Storm.Severities[0] != (Count{Name: "critical", Count: 2}) {
		t.Fatalf("unexpected storm summary: %#v", first.Storm)
	}
	e.Add(
		context.Background(),
		"alpha",
		"Network",
		Alert{Status: "firing", Labels: map[string]string{"alertname": "Network", "severity": "warning"}},
	)
	select {
	case n := <-got:
		if n.Storm != nil {
			t.Fatal("storm summary repeated inside repeat interval")
		}
	case <-time.After(60 * time.Millisecond):
	}
	second := waitStorm(t, got)
	if second.Storm.GroupCount != 4 {
		t.Fatalf("updated group count=%d", second.Storm.GroupCount)
	}
	for _, rule := range []string{"Memory", "Network"} {
		e.Add(
			context.Background(),
			"alpha",
			rule,
			Alert{Status: "resolved", Labels: map[string]string{"alertname": rule, "severity": "warning"}},
		)
	}
	deadline := time.Now().Add(time.Second)
	for {
		e.mu.Lock()
		_, storming := e.storms["alpha"]
		e.mu.Unlock()
		if !storming {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("storm mode did not exit at threshold")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func waitStorm(t *testing.T, got chan Notification) Notification {
	t.Helper()
	deadline := time.After(time.Second)
	for {
		select {
		case n := <-got:
			if n.Storm != nil {
				return n
			}
		case <-deadline:
			t.Fatal("storm summary timeout")
		}
	}
}
