package alert

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestRepresentativeAlertOrdering(t *testing.T) {
	t0 := time.Date(2026, 7, 17, 0, 0, 0, 0, time.UTC)
	mk := func(key, value, fp string, startsAt time.Time) Alert {
		return Alert{Labels: map[string]string{key: value}, StartsAt: startsAt, Fingerprint: fp}
	}
	cases := []struct {
		name   string
		alerts []Alert
		want   string
	}{
		{
			name: "severity beats duration and fingerprint",
			alerts: []Alert{
				mk("severity", "info", "a", t0),
				mk("severity", "warning", "b", t0),
				mk("severity", "critical", "z", t0.Add(time.Hour)),
			},
			want: "z",
		},
		{
			name: "fatal level ranks with critical above error",
			alerts: []Alert{
				mk("level", "error", "a", t0),
				mk("level", "fatal", "b", t0.Add(time.Minute)),
			},
			want: "b",
		},
		{
			name: "severity label wins over level label",
			alerts: []Alert{
				{Labels: map[string]string{"severity": "info", "level": "fatal"}, StartsAt: t0, Fingerprint: "a"},
				mk("severity", "warning", "b", t0.Add(time.Minute)),
			},
			want: "b",
		},
		{
			name: "same severity prefers earliest StartsAt",
			alerts: []Alert{
				mk("severity", "warning", "a", t0.Add(time.Minute)),
				mk("severity", "warning", "b", t0),
			},
			want: "b",
		},
		{
			name: "full tie falls back to ascending fingerprint",
			alerts: []Alert{
				mk("severity", "warning", "c", t0),
				mk("severity", "warning", "a", t0),
				mk("severity", "warning", "b", t0),
			},
			want: "a",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			original := append([]Alert(nil), tc.alerts...)
			for shift := range tc.alerts {
				rotated := append(append([]Alert(nil), tc.alerts[shift:]...), tc.alerts[:shift]...)
				got, ok := representativeAlert(rotated)
				if !ok || got.Fingerprint != tc.want {
					t.Fatalf("rotation %d picked %q, want %q", shift, got.Fingerprint, tc.want)
				}
			}
			for i := range original {
				if tc.alerts[i].Fingerprint != original[i].Fingerprint {
					t.Fatal("representativeAlert reordered the caller's slice")
				}
			}
		})
	}
	if _, ok := representativeAlert(nil); ok {
		t.Fatal("empty alerts must have no representative")
	}
}

type feishuCard struct {
	Card struct {
		Header struct {
			Template string `json:"template"`
			Title    struct {
				Content string `json:"content"`
			} `json:"title"`
		} `json:"header"`
		Elements []struct {
			Tag  string `json:"tag"`
			Text struct {
				Tag     string `json:"tag"`
				Content string `json:"content"`
			} `json:"text"`
		} `json:"elements"`
	} `json:"card"`
}

func decodeFeishu(t *testing.T, n Notification) feishuCard {
	t.Helper()
	raw, err := ChannelPayload("feishu", n)
	if err != nil {
		t.Fatal(err)
	}
	var card feishuCard
	if err := json.Unmarshal(raw, &card); err != nil {
		t.Fatal(err)
	}
	return card
}

func markdownBody(t *testing.T, kind string, n Notification) string {
	t.Helper()
	raw, err := ChannelPayload(kind, n)
	if err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		Text   string `json:"text"`
		Blocks []struct {
			Text struct {
				Text string `json:"text"`
			} `json:"text"`
		} `json:"blocks"`
		Markdown struct {
			Text    string `json:"text"`
			Content string `json:"content"`
		} `json:"markdown"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	switch kind {
	case "dingtalk":
		return decoded.Markdown.Text
	case "wecom":
		return decoded.Markdown.Content
	case "slack":
		return decoded.Blocks[0].Text.Text
	}
	t.Fatalf("unsupported kind %q", kind)
	return ""
}

func TestChannelPayloadFallsBackToRuleNameAndConciseLabels(t *testing.T) {
	t0 := time.Date(2026, 7, 17, 0, 0, 0, 0, time.UTC)
	labels := func(instance string) map[string]string {
		return map[string]string{
			"alertname": "DiskFull", "project_id": "prod", "project": "prod", "source": "rule", "kind": "x",
			"severity": "warning", "instance": instance, "device": "sda",
		}
	}
	n := Notification{Project: "prod", Rule: "DiskFull", Status: "firing", Alerts: []Alert{
		{Labels: labels("node-2"), StartsAt: t0.Add(time.Minute), Fingerprint: "a"},
		{Labels: labels("node-1"), StartsAt: t0, Fingerprint: "b"},
	}}
	const title = "[FIRING][WARNING] prod / DiskFull (+1 more)"
	const line = "device=sda instance=node-1"
	for kind, want := range map[string]string{
		"dingtalk": title + "\n" + line,
		"wecom":    title + "\n" + line,
		"slack":    "*" + title + "*\n" + line,
	} {
		if got := markdownBody(t, kind, n); got != want {
			t.Fatalf("%s body = %q, want %q", kind, got, want)
		}
	}
	card := decodeFeishu(t, n)
	if card.Card.Header.Title.Content != title || card.Card.Header.Template != "orange" {
		t.Fatalf("feishu header = %+v", card.Card.Header)
	}
	if len(card.Card.Elements) != 1 || card.Card.Elements[0].Text.Content != line {
		t.Fatalf("feishu elements = %+v, want single labels line without duplicate title", card.Card.Elements)
	}
	for _, kind := range []string{"dingtalk", "wecom", "slack"} {
		if body := markdownBody(t, kind, n); strings.Contains(body, "{") || strings.Contains(body, "node-2") {
			t.Fatalf("%s body leaked raw labels JSON or non-representative alerts: %q", kind, body)
		}
	}
}

func TestChannelPayloadResolvedTitleAndFeishuColors(t *testing.T) {
	alert := func(severity, summary string) Alert {
		return Alert{
			Labels:      map[string]string{"alertname": "DiskFull", "severity": severity},
			Annotations: map[string]string{"summary": summary},
			Fingerprint: "a",
		}
	}
	cases := []struct {
		name      string
		n         Notification
		wantTitle string
		wantColor string
	}{
		{
			name: "resolved drops severity tag and is green",
			n: Notification{
				Project: "prod",
				Rule:    "DiskFull",
				Status:  "resolved",
				Alerts:  []Alert{alert("critical", "node-1 磁盘使用率 95%")},
			},
			wantTitle: "[RESOLVED] prod / node-1 磁盘使用率 95%",
			wantColor: "green",
		},
		{
			name: "critical is red",
			n: Notification{
				Project: "prod",
				Rule:    "DiskFull",
				Status:  "firing",
				Alerts:  []Alert{alert("critical", "disk")},
			},
			wantTitle: "[FIRING][CRITICAL] prod / disk",
			wantColor: "red",
		},
		{
			name: "info is blue",
			n: Notification{
				Project: "prod",
				Rule:    "DiskFull",
				Status:  "firing",
				Alerts:  []Alert{alert("info", "disk")},
			},
			wantTitle: "[FIRING][INFO] prod / disk",
			wantColor: "blue",
		},
		{
			name: "storm is red",
			n: Notification{Project: "prod", Rule: "alert_storm", Status: "firing", Storm: &StormSummary{
				GroupCount: 3,
			}},
			wantTitle: "[FIRING] prod / alert_storm",
			wantColor: "red",
		},
		{
			name: "issue regression keeps its kind in the title",
			n: Notification{Project: "prod", Rule: "regression", Status: "firing", Alerts: []Alert{{
				Labels:      map[string]string{"source": "platform", "kind": "regression", "level": "error"},
				Annotations: map[string]string{"summary": "CheckoutError:\n  boom"},
			}}},
			wantTitle: "[FIRING][ERROR] prod / regression: CheckoutError: boom",
			wantColor: "red",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			card := decodeFeishu(t, tc.n)
			if card.Card.Header.Title.Content != tc.wantTitle || card.Card.Header.Template != tc.wantColor {
				t.Fatalf("feishu header = %+v, want title %q color %q", card.Card.Header, tc.wantTitle, tc.wantColor)
			}
			if len(card.Card.Elements) == 0 {
				t.Fatal("feishu card must never carry an empty elements array")
			}
			for _, el := range card.Card.Elements {
				if el.Text.Content == tc.wantTitle {
					t.Fatalf("feishu body repeats the header title: %+v", card.Card.Elements)
				}
			}
			if got := markdownBody(t, "dingtalk", tc.n); !strings.HasPrefix(got, tc.wantTitle) {
				t.Fatalf("dingtalk body = %q, want title first line %q", got, tc.wantTitle)
			}
		})
	}
}

func TestChannelPayloadFeishuEmptyBodyFallsBackToRuleName(t *testing.T) {
	n := Notification{Project: "prod", Rule: "DiskFull", Status: "firing", Alerts: []Alert{{
		Labels:      map[string]string{"alertname": "DiskFull", "severity": "warning"},
		Annotations: map[string]string{"summary": "node-1 disk full"},
	}}}
	card := decodeFeishu(t, n)
	if len(card.Card.Elements) != 1 || card.Card.Elements[0].Tag != "div" ||
		card.Card.Elements[0].Text.Content != "DiskFull" {
		t.Fatalf("feishu elements = %+v, want rule-name div fallback", card.Card.Elements)
	}
}

func TestChannelPayloadSlackEscapesRenderedText(t *testing.T) {
	n := Notification{Project: "prod", Rule: "DiskFull", Status: "firing", Alerts: []Alert{{
		Labels:      map[string]string{"alertname": "DiskFull"},
		Annotations: map[string]string{"summary": "<!channel> a&b"},
	}}}
	raw, err := ChannelPayload("slack", n)
	if err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		Text   string `json:"text"`
		Blocks []struct {
			Text struct {
				Text string `json:"text"`
			} `json:"text"`
		} `json:"blocks"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	const want = "[FIRING] prod / &lt;!channel&gt; a&amp;b"
	if decoded.Text != want || decoded.Blocks[0].Text.Text != "*"+want+"*" {
		t.Fatalf("slack text=%q block=%q, want escaped %q", decoded.Text, decoded.Blocks[0].Text.Text, want)
	}
}
