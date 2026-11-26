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

func TestValidateRuleAnnotations(t *testing.T) {
	valid := map[string]string{
		"summary":     `{{$labels.instance}} 磁盘使用率 {{printf "%.0f" $value}}%`,
		"description": "plain text without templates",
		"runbook":     `{{if eq $labels.env "prod"}}page{{else}}ticket{{end}}`,
		"literal":     "uses } braces { safely",
	}
	if err := ValidateRuleAnnotations(valid); err != nil {
		t.Fatalf("valid annotations rejected: %v", err)
	}
	for _, bad := range []string{
		`{{$labels.instance`,
		`{{ .Labels.instance }`,
		`{{$undefined}}`,
		`{{if $value}}unterminated`,
		`{{nosuchfunc $value}}`,
	} {
		err := ValidateRuleAnnotations(map[string]string{"summary": bad})
		if !errors.Is(err, ErrInvalidInput) || !strings.Contains(err.Error(), `"summary"`) {
			t.Fatalf("ValidateRuleAnnotations(%q) err=%v, want ErrInvalidInput naming the key", bad, err)
		}
	}
}

func TestAnnotationRendererRendersLabelsAndValue(t *testing.T) {
	oversized := strings.Repeat("x", maxRenderedAnnotationBytes) + `{{$value}}`
	r := newAnnotationRenderer(7)
	got := r.Render(RuleSample{
		Fingerprint: "fp",
		Labels:      map[string]string{"instance": "node-1"},
		Value:       95.4,
		Annotations: map[string]string{
			"summary":     `{{$labels.instance}} 磁盘使用率 {{printf "%.0f" $value}}%`,
			"missing":     `[{{$labels.nope}}]`,
			"dot":         `{{.Labels.instance}}={{.Value}}`,
			"plain":       "no template {here}",
			"runtimeFail": `{{index $value 1}}`,
			"tooLarge":    oversized,
		},
	})
	want := map[string]string{
		"summary":     "node-1 磁盘使用率 95%",
		"missing":     "[]",
		"dot":         "node-1=95.4",
		"plain":       "no template {here}",
		"runtimeFail": `{{index $value 1}}`,
		"tooLarge":    oversized,
	}
	for key, value := range want {
		if got[key] != value {
			t.Fatalf("annotation %q = %q, want %q", key, got[key], value)
		}
	}
	// A template that was never validated (stored before validation existed)
	// falls back to its raw text instead of failing the evaluation.
	legacy := r.Render(RuleSample{Annotations: map[string]string{"summary": "{{ broken"}})
	if legacy["summary"] != "{{ broken" {
		t.Fatalf("legacy invalid template = %q, want raw text", legacy["summary"])
	}
}

func TestAdvanceRuleStatesRendersAnnotationsAndResolvedReusesFiringTitle(t *testing.T) {
	rule := &meta.AlertRuleView{ID: 9, For: "0s"}
	t0 := time.Date(2026, 7, 18, 0, 0, 0, 0, time.UTC)
	sample := func(value float64) RuleSample {
		return RuleSample{
			Fingerprint: "node-1",
			Labels:      map[string]string{"instance": "node-1"},
			Annotations: map[string]string{"summary": `{{$labels.instance}} usage {{printf "%.0f" $value}}%`},
			Value:       value,
		}
	}
	states, err := advanceRuleStates(rule, nil, []RuleSample{sample(91)}, t0)
	if err != nil || len(states) != 1 || states[0].Status != "firing" {
		t.Fatalf("firing states=%#v err=%v", states, err)
	}
	if got := states[0].Annotations["summary"]; got != "node-1 usage 91%" {
		t.Fatalf("firing summary = %q", got)
	}
	states, err = advanceRuleStates(rule, states, []RuleSample{sample(95)}, t0.Add(time.Minute))
	if err != nil || states[0].Annotations["summary"] != "node-1 usage 95%" {
		t.Fatalf("re-evaluated firing states=%#v err=%v", states, err)
	}
	states, err = advanceRuleStates(rule, states, nil, t0.Add(2*time.Minute))
	if err != nil || len(states) != 1 || states[0].Status != "recovered" {
		t.Fatalf("recovered states=%#v err=%v", states, err)
	}
	resolved := alertFromRuleState(states[0], "resolved", t0.Add(2*time.Minute))
	if resolved.Annotations["summary"] != "node-1 usage 95%" {
		t.Fatalf("resolved summary = %q, want last firing-time rendered title", resolved.Annotations["summary"])
	}
	title := buildNotificationView(Notification{
		Project: "prod", Rule: "DiskFull", Status: "resolved", Alerts: []Alert{resolved},
	}).Title
	if title != "[RESOLVED] prod / node-1 usage 95%" {
		t.Fatalf("resolved title = %q", title)
	}
}

func TestServiceRuleSeverityAndAnnotationValidation(t *testing.T) {
	ctx := context.Background()
	store, err := meta.OpenStore(ctx, filepath.Join(t.TempDir(), "meta.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	project, err := store.CreateProject(ctx, "Sev", "sev")
	if err != nil {
		t.Fatal(err)
	}
	svc := NewService(Config{Store: store})
	defer svc.Close()

	for _, bad := range []RuleInput{
		{Project: project.Slug, Name: "BadSeverity", Expr: "up == 0", Severity: "page"},
		{Project: project.Slug, Name: "UpperSeverity", Expr: "up == 0", Severity: "CRITICAL"},
		{
			Project: project.Slug, Name: "BadTemplate", Expr: "up == 0",
			Annotations: map[string]string{"summary": "{{$labels.instance"},
		},
	} {
		if _, err := svc.CreateRule(ctx, bad); !errors.Is(err, ErrInvalidInput) {
			t.Fatalf("CreateRule(%q) err=%v want ErrInvalidInput", bad.Name, err)
		}
	}

	defaulted, err := svc.CreateRule(ctx, RuleInput{
		Project: project.Slug, Name: "Defaulted", Expr: "up == 0", Severity: "  ",
		Annotations: map[string]string{"summary": "{{$labels.instance}} down"},
	})
	if err != nil || defaulted.Severity != "warning" {
		t.Fatalf("defaulted rule=%+v err=%v, want severity warning", defaulted, err)
	}
	critical, err := svc.CreateRule(ctx, RuleInput{
		Project: project.Slug, Name: "Critical", Expr: "up == 0", Severity: " critical ",
	})
	if err != nil || critical.Severity != "critical" {
		t.Fatalf("critical rule=%+v err=%v", critical, err)
	}

	if _, err := svc.UpdateRule(ctx, critical.ID, RuleInput{Project: project.Slug, Severity: "error"}); !errors.Is(
		err,
		ErrInvalidInput,
	) {
		t.Fatalf("UpdateRule invalid severity err=%v want ErrInvalidInput", err)
	}
	if _, err := svc.UpdateRule(ctx, critical.ID, RuleInput{
		Project: project.Slug, Annotations: map[string]string{"summary": "{{end}}"},
	}); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("UpdateRule invalid template err=%v want ErrInvalidInput", err)
	}
	kept, err := svc.UpdateRule(ctx, critical.ID, RuleInput{Project: project.Slug, Severity: "  ", For: "1m"})
	if err != nil || kept.Severity != "critical" {
		t.Fatalf("blank severity update=%+v err=%v, want stored critical kept", kept, err)
	}
	info, err := svc.UpdateRule(ctx, critical.ID, RuleInput{Project: project.Slug, Severity: "info"})
	if err != nil || info.Severity != "info" {
		t.Fatalf("info update=%+v err=%v", info, err)
	}
}
