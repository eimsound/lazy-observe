package alert

import (
	"testing"
	"time"
)

func TestNotificationLinks(t *testing.T) {
	const base = "https://observe.example.com"

	cases := []struct {
		name      string
		base      string
		input     Notification
		wantTop   string
		wantAlert []string
	}{
		{
			name: "empty base returns notification unchanged",
			base: "",
			input: Notification{
				Project: "alpha",
				Rule:    "DiskFull",
				Status:  "firing",
				URL:     "https://existing.example.com/top",
				Alerts: []Alert{
					{Fingerprint: "fp-1", URL: "https://existing.example.com/a1"},
				},
			},
			wantTop:   "https://existing.example.com/top",
			wantAlert: []string{"https://existing.example.com/a1"},
		},
		{
			name: "whitespace and trailing slashes trimmed from base",
			base: "  https://observe.example.com///  ",
			input: Notification{
				Project: "alpha",
				Rule:    "DiskFull",
				Status:  "firing",
				Alerts:  []Alert{{Fingerprint: "fp-1"}},
			},
			wantTop:   base + "/alerts?fingerprint=fp-1&project=alpha",
			wantAlert: []string{base + "/alerts?fingerprint=fp-1&project=alpha"},
		},
		{
			name: "test channel notification ignores project and omits alert links",
			base: base,
			input: Notification{
				Project: "test",
				Rule:    "test",
				Status:  "firing",
				Test:    true,
				Alerts:  []Alert{{Status: "firing", Labels: map[string]string{"test": "true"}}},
			},
			wantTop:   base + "/alerts/channels",
			wantAlert: []string{""},
		},
		{
			name: "storm notification",
			base: base,
			input: Notification{
				Project: "alpha",
				Rule:    "alert_storm",
				Status:  "firing",
				Storm:   &StormSummary{GroupCount: 12},
			},
			wantTop: base + "/alerts?project=alpha&status=firing",
		},
		{
			name: "storm notification without project omits project param",
			base: base,
			input: Notification{
				Rule:   "alert_storm",
				Status: "firing",
				Storm:  &StormSummary{GroupCount: 12},
			},
			wantTop: base + "/alerts?status=firing",
		},
		{
			name: "single native rule alert with fingerprint",
			base: base,
			input: Notification{
				Project: "alpha",
				Rule:    "DiskFull",
				Status:  "firing",
				Alerts:  []Alert{{Fingerprint: "abc123"}},
			},
			wantTop:   base + "/alerts?fingerprint=abc123&project=alpha",
			wantAlert: []string{base + "/alerts?fingerprint=abc123&project=alpha"},
		},
		{
			name: "single native rule alert with empty fingerprint falls back to rule",
			base: base,
			input: Notification{
				Project: "alpha",
				Rule:    "DiskFull",
				Status:  "firing",
				Alerts:  []Alert{{}},
			},
			wantTop:   base + "/alerts?project=alpha&rule=DiskFull",
			wantAlert: []string{base + "/alerts?project=alpha&rule=DiskFull"},
		},
		{
			name: "multi native rule alerts with distinct fingerprints",
			base: base,
			input: Notification{
				Project: "alpha",
				Rule:    "DiskFull",
				Status:  "firing",
				Alerts:  []Alert{{Fingerprint: "fp-1"}, {Fingerprint: "fp-2"}},
			},
			wantTop: base + "/alerts?project=alpha&rule=DiskFull",
			wantAlert: []string{
				base + "/alerts?fingerprint=fp-1&project=alpha",
				base + "/alerts?fingerprint=fp-2&project=alpha",
			},
		},
		{
			name: "multi native rule alerts all missing fingerprint share rule URL",
			base: base,
			input: Notification{
				Project: "alpha",
				Rule:    "DiskFull",
				Status:  "firing",
				Alerts:  []Alert{{}, {}},
			},
			wantTop: base + "/alerts?project=alpha&rule=DiskFull",
			wantAlert: []string{
				base + "/alerts?project=alpha&rule=DiskFull",
				base + "/alerts?project=alpha&rule=DiskFull",
			},
		},
		{
			name: "platform new_issue with positive issue_id",
			base: base,
			input: Notification{
				Project: "alpha",
				Rule:    "new_issue",
				Status:  "firing",
				Alerts: []Alert{{
					Labels: map[string]string{"source": "platform", "kind": "new_issue", "issue_id": "42"},
				}},
			},
			wantTop:   base + "/issues/42?project=alpha",
			wantAlert: []string{base + "/issues/42?project=alpha"},
		},
		{
			name: "platform regression with positive issue_id",
			base: base,
			input: Notification{
				Project: "alpha",
				Rule:    "regression",
				Status:  "firing",
				Alerts: []Alert{{
					Labels: map[string]string{"source": "platform", "kind": "regression", "issue_id": "99"},
				}},
			},
			wantTop:   base + "/issues/99?project=alpha",
			wantAlert: []string{base + "/issues/99?project=alpha"},
		},
		{
			name: "multi-issue platform notification falls back to issues list",
			base: base,
			input: Notification{
				Project: "alpha",
				Rule:    "new_issue",
				Status:  "firing",
				Alerts: []Alert{
					{Labels: map[string]string{"source": "platform", "kind": "new_issue", "issue_id": "10"}},
					{Labels: map[string]string{"source": "platform", "kind": "regression", "issue_id": "20"}},
				},
			},
			wantTop: base + "/issues?project=alpha",
			wantAlert: []string{
				base + "/issues/10?project=alpha",
				base + "/issues/20?project=alpha",
			},
		},
		{
			name: "platform error_rate_spike",
			base: base,
			input: Notification{
				Project: "alpha",
				Rule:    "error_rate_spike",
				Status:  "firing",
				Alerts: []Alert{{
					Labels: map[string]string{"source": "platform", "kind": "error_rate_spike"},
				}},
			},
			wantTop:   base + "/issues?project=alpha",
			wantAlert: []string{base + "/issues?project=alpha"},
		},
		{
			name: "platform new_issue with missing or non-positive issue_id",
			base: base,
			input: Notification{
				Project: "alpha",
				Rule:    "new_issue",
				Status:  "firing",
				Alerts: []Alert{
					{Labels: map[string]string{"source": "platform", "kind": "new_issue"}},
					{Labels: map[string]string{"source": "platform", "kind": "new_issue", "issue_id": "0"}},
					{Labels: map[string]string{"source": "platform", "kind": "new_issue", "issue_id": "-5"}},
					{Labels: map[string]string{"source": "platform", "kind": "regression", "issue_id": "abc"}},
				},
			},
			wantTop: base + "/issues?project=alpha",
			wantAlert: []string{
				base + "/issues?project=alpha",
				base + "/issues?project=alpha",
				base + "/issues?project=alpha",
				base + "/issues?project=alpha",
			},
		},
		{
			name: "platform unknown kind with issue_id falls back to issues list",
			base: base,
			input: Notification{
				Project: "alpha",
				Rule:    "other_kind",
				Status:  "firing",
				Alerts: []Alert{{
					Labels: map[string]string{"source": "platform", "kind": "other_kind", "issue_id": "42"},
				}},
			},
			wantTop:   base + "/issues?project=alpha",
			wantAlert: []string{base + "/issues?project=alpha"},
		},
		{
			name: "mixed platform and native alerts fall back to issues list",
			base: base,
			input: Notification{
				Project: "alpha",
				Rule:    "mixed",
				Status:  "firing",
				Alerts: []Alert{
					{Labels: map[string]string{"source": "platform", "kind": "new_issue", "issue_id": "7"}},
					{Fingerprint: "native-fp"},
				},
			},
			wantTop: base + "/issues?project=alpha",
			wantAlert: []string{
				base + "/issues/7?project=alpha",
				base + "/alerts?fingerprint=native-fp&project=alpha",
			},
		},
		{
			name: "empty alerts",
			base: base,
			input: Notification{
				Project: "alpha",
				Rule:    "DiskFull",
				Status:  "firing",
			},
			wantTop: base + "/alerts?project=alpha",
		},
		{
			name: "empty alerts and empty project has no dangling question mark",
			base: base,
			input: Notification{
				Rule:   "DiskFull",
				Status: "firing",
			},
			wantTop: base + "/alerts",
		},
		{
			name: "empty project with fingerprint and rule",
			base: base,
			input: Notification{
				Rule:   "DiskFull",
				Status: "firing",
				Alerts: []Alert{
					{Fingerprint: "fp-1"},
					{},
				},
			},
			wantTop: base + "/alerts?rule=DiskFull",
			wantAlert: []string{
				base + "/alerts?fingerprint=fp-1",
				base + "/alerts?rule=DiskFull",
			},
		},
		{
			name: "special characters in project slug and rule are encoded and sorted",
			base: base,
			input: Notification{
				Project: "a b(c)&d",
				Rule:    "a b(c)&d",
				Status:  "firing",
				Alerts: []Alert{
					{Fingerprint: "fp-1"},
					{},
				},
			},
			wantTop: base + "/alerts?project=a+b%28c%29%26d&rule=a+b%28c%29%26d",
			wantAlert: []string{
				base + "/alerts?fingerprint=fp-1&project=a+b%28c%29%26d",
				base + "/alerts?project=a+b%28c%29%26d&rule=a+b%28c%29%26d",
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := NotificationLinks(tc.base, tc.input)
			if got.URL != tc.wantTop {
				t.Fatalf("top URL = %q, want %q", got.URL, tc.wantTop)
			}
			if len(got.Alerts) != len(tc.wantAlert) {
				t.Fatalf("alerts len = %d, want %d", len(got.Alerts), len(tc.wantAlert))
			}
			for i, want := range tc.wantAlert {
				if got.Alerts[i].URL != want {
					t.Fatalf("alerts[%d].URL = %q, want %q", i, got.Alerts[i].URL, want)
				}
			}
		})
	}
}

func TestNotificationLinksDoesNotMutateCallerSlice(t *testing.T) {
	original := []Alert{{
		Status:      "firing",
		Fingerprint: "fp-1",
		StartsAt:    time.Date(2026, 7, 17, 1, 2, 3, 0, time.UTC),
	}}
	in := Notification{
		Project: "alpha",
		Rule:    "DiskFull",
		Status:  "firing",
		Alerts:  original,
	}

	out := NotificationLinks("https://observe.example.com", in)
	if out.URL == "" || out.Alerts[0].URL == "" {
		t.Fatalf("expected derived URLs, got %+v", out)
	}
	if in.URL != "" || original[0].URL != "" {
		t.Fatalf("NotificationLinks mutated caller notification/slice: in=%+v original=%+v", in, original)
	}
}
