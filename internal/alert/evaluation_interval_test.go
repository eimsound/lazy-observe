package alert

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/eimsound/lazy-observe/internal/meta"
)

func TestNormalizeRuleEvaluationInterval(t *testing.T) {
	tests := []struct {
		name    string
		signal  string
		raw     string
		want    string
		wantErr string
	}{
		{name: "empty metrics", signal: "metrics", raw: "", want: ""},
		{name: "whitespace logs", signal: "logs", raw: "  \t ", want: ""},
		{name: "default signal empty", signal: "", raw: "60s", want: "1m"},
		{name: "metrics min 5s", signal: "metrics", raw: "5s", want: "5s"},
		{name: "metrics canonical 60s", signal: "metrics", raw: " 60s ", want: "1m"},
		{name: "metrics canonical 90s", signal: "metrics", raw: "90s", want: "1m30s"},
		{name: "metrics max 1h", signal: "metrics", raw: "1h", want: "1h"},
		{name: "logs min 30s", signal: "logs", raw: "30s", want: "30s"},
		{name: "logs canonical 120s", signal: "logs", raw: "120s", want: "2m"},
		{name: "logs max 1h", signal: "logs", raw: "60m", want: "1h"},
		{
			name:    "unparsable",
			signal:  "metrics",
			raw:     "abc",
			wantErr: `invalid alert evaluation_interval "abc": must be a duration such as 30s or 1m`,
		},
		{
			name:    "negative rejected as unparsable",
			signal:  "metrics",
			raw:     "-5s",
			wantErr: `invalid alert evaluation_interval "-5s": must be a duration such as 30s or 1m`,
		},
		{
			name:    "zero duration",
			signal:  "metrics",
			raw:     "0s",
			wantErr: "alert evaluation_interval for metrics rules must be between 5s and 1h",
		},
		{
			name:    "not multiple of 5s",
			signal:  "metrics",
			raw:     "7s",
			wantErr: `alert evaluation_interval "7s" must be a multiple of 5s`,
		},
		{
			name:    "logs not multiple of 5s",
			signal:  "logs",
			raw:     "32s",
			wantErr: `alert evaluation_interval "32s" must be a multiple of 5s`,
		},
		{
			name:    "metrics below min",
			signal:  "metrics",
			raw:     "4s",
			wantErr: "alert evaluation_interval for metrics rules must be between 5s and 1h",
		},
		{
			name:    "metrics above max",
			signal:  "metrics",
			raw:     "1h5s",
			wantErr: "alert evaluation_interval for metrics rules must be between 5s and 1h",
		},
		{
			name:    "logs below min",
			signal:  "logs",
			raw:     "25s",
			wantErr: "alert evaluation_interval for logs rules must be between 30s and 1h",
		},
		{
			name:    "logs above max",
			signal:  "logs",
			raw:     "1h5s",
			wantErr: "alert evaluation_interval for logs rules must be between 30s and 1h",
		},
		{
			name:    "unsupported signal",
			signal:  "traces",
			raw:     "30s",
			wantErr: `unsupported alert signal "traces"`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := NormalizeRuleEvaluationInterval(tc.signal, tc.raw)
			if tc.wantErr != "" {
				if !errors.Is(err, ErrInvalidInput) {
					t.Fatalf("err=%v want ErrInvalidInput", err)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err=%q want substring %q", err.Error(), tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Fatalf("got=%q want=%q", got, tc.want)
			}
		})
	}
}

func TestServiceRuleEvaluationIntervalCreateUpdateAndEffective(t *testing.T) {
	ctx := context.Background()
	store, err := meta.OpenStore(ctx, filepath.Join(t.TempDir(), "meta.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	project, err := store.CreateProject(ctx, "EvalSvc", "eval-svc")
	if err != nil {
		t.Fatal(err)
	}

	// Nil-evaluator service falls back to package defaults (5s / 1m).
	svc := NewService(Config{Store: store})
	defer svc.Close()

	metricsDefault, err := svc.CreateRule(ctx, RuleInput{
		Project: project.Slug,
		Name:    "MetricsDefault",
		Signal:  "metrics",
		Expr:    "up == 0",
	})
	if err != nil {
		t.Fatal(err)
	}
	if metricsDefault.EvaluationInterval != "" || svc.EffectiveEvaluationInterval(metricsDefault) != "5s" {
		t.Fatalf(
			"metricsDefault interval=%q effective=%q",
			metricsDefault.EvaluationInterval,
			svc.EffectiveEvaluationInterval(metricsDefault),
		)
	}

	logsDefault, err := svc.CreateRule(ctx, RuleInput{
		Project: project.Slug,
		Name:    "LogsDefault",
		Signal:  "logs",
		Expr:    "* | stats count() as c",
	})
	if err != nil {
		t.Fatal(err)
	}
	if logsDefault.EvaluationInterval != "" || svc.EffectiveEvaluationInterval(logsDefault) != "1m" {
		t.Fatalf(
			"logsDefault interval=%q effective=%q",
			logsDefault.EvaluationInterval,
			svc.EffectiveEvaluationInterval(logsDefault),
		)
	}

	tenSec := "10s"
	metricsCustom, err := svc.CreateRule(ctx, RuleInput{
		Project:            project.Slug,
		Name:               "MetricsCustom",
		Signal:             "metrics",
		Expr:               "up == 0",
		EvaluationInterval: &tenSec,
	})
	if err != nil {
		t.Fatal(err)
	}
	if metricsCustom.EvaluationInterval != "10s" || svc.EffectiveEvaluationInterval(metricsCustom) != "10s" {
		t.Fatalf(
			"metricsCustom interval=%q effective=%q",
			metricsCustom.EvaluationInterval,
			svc.EffectiveEvaluationInterval(metricsCustom),
		)
	}

	// Switching metrics -> logs while keeping 10s (omitted EvaluationInterval) must fail.
	if _, err := svc.UpdateRule(ctx, metricsCustom.ID, RuleInput{
		Project: project.Slug,
		Signal:  "logs",
		Expr:    "* | stats count() as c",
	}); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("signal switch with out-of-bounds override err=%v want ErrInvalidInput", err)
	}

	// Switching metrics -> logs while clearing EvaluationInterval to "" must succeed.
	empty := ""
	switched, err := svc.UpdateRule(ctx, metricsCustom.ID, RuleInput{
		Project:            project.Slug,
		Signal:             "logs",
		Expr:               "* | stats count() as c",
		EvaluationInterval: &empty,
	})
	if err != nil {
		t.Fatalf("signal switch with reset failed: %v", err)
	}
	if switched.Signal != "logs" || switched.EvaluationInterval != "" ||
		svc.EffectiveEvaluationInterval(switched) != "1m" {
		t.Fatalf("switched rule=%+v effective=%q", switched, svc.EffectiveEvaluationInterval(switched))
	}

	// Custom evaluator defaults are reflected in EffectiveEvaluationInterval.
	customSvc := NewService(Config{
		Store: store,
		Evaluator: RuleEvaluatorConfig{
			Query:           &countingRuleQuery{},
			MetricsInterval: 15 * time.Second,
			LogsInterval:    2 * time.Minute,
		},
	})
	defer customSvc.Close()
	if got := customSvc.EffectiveEvaluationInterval(metricsDefault); got != "15s" {
		t.Fatalf("custom evaluator metrics effective=%q want 15s", got)
	}
	if got := customSvc.EffectiveEvaluationInterval(logsDefault); got != "2m" {
		t.Fatalf("custom evaluator logs effective=%q want 2m", got)
	}
}
