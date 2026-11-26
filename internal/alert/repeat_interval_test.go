package alert

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/eimsound/lazy-observe/internal/meta"
	"github.com/eimsound/lazy-observe/internal/outbound"
)

func TestNormalizeRuleRepeatInterval(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		want    string
		wantErr string
	}{
		{name: "empty inherits global", raw: "", want: ""},
		{name: "whitespace inherits global", raw: " \t ", want: ""},
		{name: "min 1m", raw: "1m", want: "1m"},
		{name: "canonical 60s", raw: "60s", want: "1m"},
		{name: "canonical 60m", raw: " 60m ", want: "1h"},
		{name: "canonical 90m", raw: "90m", want: "1h30m"},
		{name: "days kept", raw: "1d", want: "1d"},
		{name: "canonical 36h", raw: "36h", want: "1d12h"},
		{name: "max 7d", raw: "7d", want: "7d"},
		{name: "max as 168h", raw: "168h", want: "7d"},
		{name: "max as 1w", raw: "1w", want: "7d"},
		{
			name:    "unparsable",
			raw:     "soon",
			wantErr: `invalid alert repeat_interval "soon": must be a duration such as 30m, 1h or 1d`,
		},
		{
			name:    "negative unparsable",
			raw:     "-1h",
			wantErr: `invalid alert repeat_interval "-1h": must be a duration such as 30m, 1h or 1d`,
		},
		{name: "zero", raw: "0s", wantErr: "alert repeat_interval must be between 1m and 7d"},
		{name: "below min", raw: "59s", wantErr: "alert repeat_interval must be between 1m and 7d"},
		{name: "above max", raw: "7d1s", wantErr: "alert repeat_interval must be between 1m and 7d"},
		{name: "above max 8d", raw: "8d", wantErr: "alert repeat_interval must be between 1m and 7d"},
		{
			name:    "fractional seconds",
			raw:     "1m500ms",
			wantErr: `alert repeat_interval "1m500ms" must be a whole number of seconds`,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := NormalizeRuleRepeatInterval(tc.raw)
			if tc.wantErr != "" {
				if !errors.Is(err, ErrInvalidInput) || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("NormalizeRuleRepeatInterval(%q) err=%v want %q", tc.raw, err, tc.wantErr)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("NormalizeRuleRepeatInterval(%q)=%q err=%v want %q", tc.raw, got, err, tc.want)
			}
		})
	}
}

func TestFormatRepeatInterval(t *testing.T) {
	for d, want := range map[time.Duration]string{
		0:                             "0s",
		time.Minute:                   "1m",
		time.Hour:                     "1h",
		4 * time.Hour:                 "4h",
		7 * 24 * time.Hour:            "7d",
		25*time.Hour + 30*time.Second: "1d1h30s",
		1500 * time.Millisecond:       "1s500ms",
	} {
		if got := FormatRepeatInterval(d); got != want {
			t.Fatalf("FormatRepeatInterval(%s)=%q want %q", d, got, want)
		}
	}
}

func TestDefaultRepeatIntervalIsOneHour(t *testing.T) {
	if DefaultRepeatInterval != time.Hour {
		t.Fatalf("DefaultRepeatInterval=%s want 1h", DefaultRepeatInterval)
	}
	svc := NewService(Config{})
	defer svc.Close()
	if svc.repeatInterval != time.Hour {
		t.Fatalf("service default repeatInterval=%s want 1h", svc.repeatInterval)
	}
	evaluator := NewRuleEvaluator(RuleEvaluatorConfig{Store: &meta.Store{}, Query: &countingRuleQuery{}})
	if evaluator == nil || evaluator.repeatInterval != time.Hour {
		t.Fatalf("evaluator default repeatInterval=%v want 1h", evaluator)
	}
	var nilEvaluator *RuleEvaluator
	if got := nilEvaluator.ruleRepeatInterval(&meta.AlertRuleView{RepeatInterval: "5m"}); got != 5*time.Minute {
		t.Fatalf("nil evaluator override=%s want 5m", got)
	}
	if got := nilEvaluator.ruleRepeatInterval(&meta.AlertRuleView{}); got != time.Hour {
		t.Fatalf("nil evaluator default=%s want 1h", got)
	}
	if got := evaluator.ruleRepeatInterval(&meta.AlertRuleView{ID: 9, RepeatInterval: "garbage"}); got != time.Hour {
		t.Fatalf("invalid stored override=%s want global 1h fallback", got)
	}
}

func TestServiceRuleRepeatIntervalCreateUpdateAndEffective(t *testing.T) {
	ctx := context.Background()
	store, err := meta.OpenStore(ctx, filepath.Join(t.TempDir(), "meta.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	project, err := store.CreateProject(ctx, "RepeatSvc", "repeat-svc")
	if err != nil {
		t.Fatal(err)
	}
	svc := NewService(Config{Store: store})
	defer svc.Close()

	inherited, err := svc.CreateRule(ctx, RuleInput{Project: project.Slug, Name: "Inherited", Expr: "up == 0"})
	if err != nil {
		t.Fatal(err)
	}
	if inherited.RepeatInterval != "" || svc.EffectiveRepeatInterval(inherited) != "1h" {
		t.Fatalf("inherited repeat=%q effective=%q", inherited.RepeatInterval, svc.EffectiveRepeatInterval(inherited))
	}

	sixtyMinutes := "60m"
	custom, err := svc.CreateRule(ctx, RuleInput{
		Project:        project.Slug,
		Name:           "Custom",
		Expr:           "up == 0",
		RepeatInterval: &sixtyMinutes,
	})
	if err != nil {
		t.Fatal(err)
	}
	if custom.RepeatInterval != "1h" || svc.EffectiveRepeatInterval(custom) != "1h" {
		t.Fatalf("custom repeat=%q effective=%q", custom.RepeatInterval, svc.EffectiveRepeatInterval(custom))
	}

	for _, bad := range []string{"30s", "8d", "often"} {
		raw := bad
		if _, err := svc.CreateRule(ctx, RuleInput{
			Project:        project.Slug,
			Name:           "Bad" + bad,
			Expr:           "up == 0",
			RepeatInterval: &raw,
		}); !errors.Is(err, ErrInvalidInput) {
			t.Fatalf("create repeat_interval=%q err=%v want ErrInvalidInput", bad, err)
		}
		if _, err := svc.UpdateRule(ctx, custom.ID, RuleInput{
			Project:        project.Slug,
			RepeatInterval: &raw,
		}); !errors.Is(err, ErrInvalidInput) {
			t.Fatalf("update repeat_interval=%q err=%v want ErrInvalidInput", bad, err)
		}
	}

	// Omitting repeat_interval keeps the stored override.
	kept, err := svc.UpdateRule(ctx, custom.ID, RuleInput{Project: project.Slug, Severity: "critical"})
	if err != nil {
		t.Fatal(err)
	}
	if kept.RepeatInterval != "1h" || kept.Severity != "critical" {
		t.Fatalf("omitted repeat_interval rule=%+v", kept)
	}

	twoDays := "48h"
	changed, err := svc.UpdateRule(ctx, custom.ID, RuleInput{Project: project.Slug, RepeatInterval: &twoDays})
	if err != nil {
		t.Fatal(err)
	}
	if changed.RepeatInterval != "2d" || svc.EffectiveRepeatInterval(changed) != "2d" {
		t.Fatalf("changed repeat=%q effective=%q", changed.RepeatInterval, svc.EffectiveRepeatInterval(changed))
	}

	empty := ""
	reset, err := svc.UpdateRule(ctx, custom.ID, RuleInput{Project: project.Slug, RepeatInterval: &empty})
	if err != nil {
		t.Fatal(err)
	}
	if reset.RepeatInterval != "" || svc.EffectiveRepeatInterval(reset) != "1h" {
		t.Fatalf("reset repeat=%q effective=%q", reset.RepeatInterval, svc.EffectiveRepeatInterval(reset))
	}

	// A configured global default is what an empty override inherits.
	globalSvc := NewService(Config{Store: store, RepeatInterval: 4 * time.Hour})
	defer globalSvc.Close()
	if got := globalSvc.EffectiveRepeatInterval(reset); got != "4h" {
		t.Fatalf("global 4h effective=%q want 4h", got)
	}
	if got := globalSvc.EffectiveRepeatInterval(changed); got != "2d" {
		t.Fatalf("override effective=%q want 2d", got)
	}
}

func TestRuleOutboxKeysSeparateRepeatIntervals(t *testing.T) {
	startsAt := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	firing := []Notification{{
		Project: "p",
		Rule:    "r",
		Status:  "firing",
		Alerts:  []Alert{{Status: "firing", Fingerprint: "fp", StartsAt: startsAt}},
	}}
	// Generation 1 under a 1h interval and generation 1 under a 4h interval
	// must not share a key, or a mid-incident interval change would dedupe a
	// legitimate repeat against an earlier delivery.
	hourly := BuildRuleOutboxEntries(1, 7, []int{3}, firing, startsAt.Add(90*time.Minute), time.Hour, time.Hour)
	fourHourly := BuildRuleOutboxEntries(1, 7, []int{3}, firing, startsAt.Add(5*time.Hour), time.Hour, 4*time.Hour)
	if len(hourly) != 1 || len(fourHourly) != 1 || hourly[0].IdempotencyKey == fourHourly[0].IdempotencyKey {
		t.Fatalf("repeat generations collided: %#v %#v", hourly, fourHourly)
	}
	// Same interval and generation stays deterministic across replicas.
	again := BuildRuleOutboxEntries(1, 7, []int{3}, firing, startsAt.Add(100*time.Minute), time.Hour, time.Hour)
	if again[0].IdempotencyKey != hourly[0].IdempotencyKey {
		t.Fatalf("same generation keys differ: %q vs %q", again[0].IdempotencyKey, hourly[0].IdempotencyKey)
	}
	// BuildOutboxEntries (single interval) matches the rule builder when both
	// intervals are equal.
	legacy := BuildOutboxEntries(1, intPtr(7), []int{3}, firing, startsAt.Add(90*time.Minute), time.Hour)
	if legacy[0].IdempotencyKey != hourly[0].IdempotencyKey {
		t.Fatalf("BuildOutboxEntries key=%q want %q", legacy[0].IdempotencyKey, hourly[0].IdempotencyKey)
	}
	// Storm summaries bucket on the global interval regardless of the rule's.
	storm := []Notification{{Project: "p", Rule: "alert_storm", Status: "firing", Storm: &StormSummary{}}}
	stormShort := BuildRuleOutboxEntries(1, 7, []int{3}, storm, startsAt.Add(10*time.Minute), time.Hour, time.Minute)
	stormGlobal := BuildOutboxEntries(1, nil, []int{3}, storm, startsAt.Add(10*time.Minute), time.Hour)
	if stormShort[0].IdempotencyKey != stormGlobal[0].IdempotencyKey {
		t.Fatalf("storm key depends on rule repeat_interval: %q vs %q",
			stormShort[0].IdempotencyKey, stormGlobal[0].IdempotencyKey)
	}
}

func intPtr(v int) *int { return &v }

// TestNativeEvaluatorRepeatIntervalUpdateAppliesToFiringRule verifies that
// editing repeat_interval on an already-firing rule takes effect on the next
// evaluation without resetting its firing state or alert event.
func TestNativeEvaluatorRepeatIntervalUpdateAppliesToFiringRule(t *testing.T) {
	ctx := context.Background()
	store, err := meta.OpenStore(ctx, filepath.Join(t.TempDir(), "meta.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	project, err := store.CreateProject(ctx, "RepeatLive", "repeat-live")
	if err != nil {
		t.Fatal(err)
	}
	received := make(chan Notification, 8)
	keys := make(chan string, 8)
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
	var offset atomic.Int64
	clock := func() time.Time { return time.Now().Add(time.Duration(offset.Load())) }
	query := &mutableRuleQuery{}
	query.Set([]RuleSample{{
		Fingerprint: "repeat-alert",
		Labels:      map[string]string{"project_id": "repeat-live", "alertname": "Repeat"},
		Value:       1,
	}})
	service := NewService(Config{
		Store:          store,
		OutboundPolicy: outbound.Policy{AllowPrivate: true},
		Sender:         &Sender{Client: endpoint.Client(), Sleep: func(time.Duration) {}},
		GroupWait:      time.Millisecond,
		RepeatInterval: time.Hour,
		Now:            clock,
		Outbox:         OutboxWorkerConfig{WorkerID: "repeat-outbox", PollInterval: 5 * time.Millisecond},
		Evaluator: RuleEvaluatorConfig{
			Query:           query,
			MemberID:        "repeat-engine",
			TickInterval:    5 * time.Millisecond,
			MetricsInterval: 5 * time.Millisecond,
			LeaseTTL:        time.Second,
		},
	})
	defer service.Close()
	channel, err := service.CreateChannel(
		ctx,
		ChannelInput{Type: "webhook", Name: "repeat", Config: map[string]any{"url": endpoint.URL}},
	)
	if err != nil {
		t.Fatal(err)
	}
	rule, err := service.CreateRule(ctx, RuleInput{
		Project:          project.Slug,
		Name:             "Repeat",
		Expr:             "vector(1)",
		EvaluationEngine: "native",
		ChannelIDs:       []int{channel.ID},
	})
	if err != nil {
		t.Fatal(err)
	}
	waitNotificationStatus(t, received, "firing")
	firstKey := <-keys
	before, err := store.AlertRuleStates(ctx, rule.ID)
	if err != nil || len(before) != 1 || before[0].Status != "firing" || before[0].FiringSince == nil {
		t.Fatalf("states after first firing=%#v err=%v", before, err)
	}

	// Ten minutes later the global 1h interval is not yet due.
	offset.Store(int64(10 * time.Minute))
	select {
	case notification := <-received:
		t.Fatalf("repeat sent before global repeat_interval elapsed: %#v", notification)
	case <-time.After(150 * time.Millisecond):
	}

	// Shortening the rule's repeat_interval applies immediately.
	fiveMinutes := "5m"
	updated, err := service.UpdateRule(ctx, rule.ID, RuleInput{Project: project.Slug, RepeatInterval: &fiveMinutes})
	if err != nil {
		t.Fatal(err)
	}
	if updated.RepeatInterval != "5m" {
		t.Fatalf("updated repeat_interval=%q want 5m", updated.RepeatInterval)
	}
	repeat := waitNotificationStatus(t, received, "firing")
	if len(repeat.Alerts) != 1 || repeat.Alerts[0].Fingerprint != "repeat-alert" {
		t.Fatalf("repeat notification=%#v", repeat)
	}
	if repeatKey := <-keys; repeatKey == "" || repeatKey == firstKey {
		t.Fatalf("repeat key=%q first=%q want distinct non-empty", repeatKey, firstKey)
	}
	after, err := store.AlertRuleStates(ctx, rule.ID)
	if err != nil || len(after) != 1 || after[0].Status != "firing" || after[0].FiringSince == nil ||
		!after[0].FiringSince.Equal(*before[0].FiringSince) {
		t.Fatalf("firing state reset by repeat_interval update: before=%#v after=%#v err=%v", before, after, err)
	}
	events, err := store.ListAlertEvents(ctx, project.Slug, "", nil)
	if err != nil || len(events) != 1 || events[0].Status != "firing" || events[0].EndsAt != nil {
		t.Fatalf("events after repeat_interval update=%#v err=%v", events, err)
	}
	// No further repeat until the new 5m interval elapses.
	select {
	case notification := <-received:
		t.Fatalf("unexpected extra repeat: %#v", notification)
	case <-time.After(150 * time.Millisecond):
	}

	// Resetting to "" (inherit global 1h) lengthens the interval immediately:
	// six minutes after the last repeat the old 5m override would be due, but
	// the inherited 1h is not.
	empty := ""
	reset, err := service.UpdateRule(ctx, rule.ID, RuleInput{Project: project.Slug, RepeatInterval: &empty})
	if err != nil || reset.RepeatInterval != "" {
		t.Fatalf("reset repeat_interval=%q err=%v", reset.RepeatInterval, err)
	}
	offset.Store(int64(16 * time.Minute))
	select {
	case notification := <-received:
		t.Fatalf("repeat sent under the old 5m interval after reset to global 1h: %#v", notification)
	case <-time.After(200 * time.Millisecond):
	}
	final, err := store.AlertRuleStates(ctx, rule.ID)
	if err != nil || len(final) != 1 || final[0].Status != "firing" ||
		!final[0].FiringSince.Equal(*before[0].FiringSince) {
		t.Fatalf("firing state reset by repeat_interval reset: %#v err=%v", final, err)
	}
}

func TestRuleEvaluatorRepeatWarningClearedWithRuleStatus(t *testing.T) {
	evaluator := NewRuleEvaluator(RuleEvaluatorConfig{Store: &meta.Store{}, Query: &countingRuleQuery{}})
	broken := &meta.AlertRuleView{ID: 42, RepeatInterval: "garbage"}
	evaluator.ruleRepeatInterval(broken)
	if evaluator.shouldWarnInvalidRepeat(42, "garbage") {
		t.Fatal("same invalid repeat_interval must warn only once")
	}
	evaluator.ClearRuleStatus(42)
	if !evaluator.shouldWarnInvalidRepeat(42, "garbage") {
		t.Fatal("ClearRuleStatus must forget the repeat_interval warning")
	}
	// A valid value clears the warning so a later regression warns again.
	evaluator.ruleRepeatInterval(&meta.AlertRuleView{ID: 42, RepeatInterval: "5m"})
	if !evaluator.shouldWarnInvalidRepeat(42, "garbage") {
		t.Fatal("valid repeat_interval must clear the warning")
	}
}
