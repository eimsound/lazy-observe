package alert

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestChannelPayloadGolden(t *testing.T) {
	n := Notification{
		Project: "alpha",
		Rule:    "DiskFull",
		Status:  "firing",
		Alerts: []Alert{
			{
				Status:      "firing",
				Labels:      map[string]string{"alertname": "DiskFull", "instance": "node-1", "project_id": "alpha"},
				Annotations: map[string]string{"summary": "disk full"},
				StartsAt:    time.Date(2026, 7, 17, 1, 2, 3, 0, time.UTC),
				Fingerprint: "abc",
			},
		},
	}
	for _, kind := range []string{"webhook", "feishu", "dingtalk", "wecom", "slack"} {
		t.Run(kind, func(t *testing.T) {
			got, err := ChannelPayload(kind, n)
			if err != nil {
				t.Fatal(err)
			}
			assertGolden(t, kind+".golden", got)
		})
	}
	assertGolden(t, "smtp.golden", EmailPayload("alerts@example.com", []string{"ops@example.com"}, n))
}

func TestStormPayloadIncludesSummary(t *testing.T) {
	n := Notification{
		Project: "alpha",
		Rule:    "alert_storm",
		Status:  "firing",
		Storm: &StormSummary{
			GroupCount: 12,
			Severities: []Count{{Name: "critical", Count: 8}},
			Rules:      []Count{{Name: "HostDown", Count: 5}},
		},
	}
	for _, kind := range []string{"feishu", "dingtalk", "wecom"} {
		payload, err := ChannelPayload(kind, n)
		if err != nil {
			t.Fatal(err)
		}
		for _, want := range [][]byte{[]byte("Active alert groups: 12"), []byte("critical=8"), []byte("HostDown=5")} {
			if !bytes.Contains(payload, want) {
				t.Errorf("%s payload missing %q: %s", kind, want, payload)
			}
		}
	}
}
func assertGolden(t *testing.T, name string, got []byte) {
	t.Helper()
	want, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	got = bytes.ReplaceAll(got, []byte("\r\n"), []byte("\n"))
	want = bytes.TrimSuffix(want, []byte("\n"))
	if !bytes.Equal(got, want) {
		t.Fatalf("%s mismatch\ngot:  %s\nwant: %s", name, got, want)
	}
}
