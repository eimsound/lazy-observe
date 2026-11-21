package selfmetrics

import (
	"bytes"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/common/expfmt"
	"github.com/prometheus/common/model"
)

func TestRegistryRendersValidPrometheusTextFormat(t *testing.T) {
	start := time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC)
	now := start.Add(42*time.Second + 500*time.Millisecond)
	reg := New(Config{
		Version:   "v0.2.0",
		Commit:    "abc1234",
		StartedAt: start,
		Now:       func() time.Time { return now },
	})

	reg.ObserveHTTP(SurfaceIngest, 204, 15*time.Millisecond)
	reg.ObserveHTTP(SurfaceIngest, 429, 2*time.Millisecond)
	reg.ObserveHTTP(SurfaceQuery, 200, 120*time.Millisecond)
	reg.ObserveHTTP(SurfaceAdmin, 500, 3*time.Second)
	reg.ObserveHTTP(SurfacePublic, 302, 1*time.Millisecond)
	reg.ObserveHTTP(SurfaceMetrics, 200, 4*time.Millisecond)

	reg.ObserveIngest(ProtocolHTTP, SignalTraces, OutcomeAccepted)
	reg.ObserveIngest(ProtocolHTTP, SignalTraces, OutcomeRateLimited)
	reg.ObserveIngest(ProtocolGRPC, SignalLogs, OutcomeRateLimited)
	reg.ObserveIngest(ProtocolGRPC, SignalMetrics, OutcomeUpstreamError)

	reg.IncGRPCInFlight()
	reg.IncGRPCInFlight()
	reg.DecGRPCInFlight()

	var buf bytes.Buffer
	if err := reg.WritePrometheus(&buf); err != nil {
		t.Fatalf("WritePrometheus: %v", err)
	}
	text := buf.String()

	parser := expfmt.NewTextParser(model.UTF8Validation)
	families, err := parser.TextToMetricFamilies(strings.NewReader(text))
	if err != nil {
		t.Fatalf("Prometheus TextParser rejected exposition output: %v\n%s", err, text)
	}

	expectedFamilies := []string{
		"go_gc_completed_cycles_total",
		"go_goroutines",
		"go_memstats_alloc_bytes",
		"go_memstats_heap_inuse_bytes",
		"go_memstats_sys_bytes",
		"lazy_observe_build_info",
		"lazy_observe_http_request_duration_seconds",
		"lazy_observe_http_requests_total",
		"lazy_observe_ingest_rate_limited_total",
		"lazy_observe_ingest_requests_total",
		"lazy_observe_otlp_grpc_in_flight_requests",
		"lazy_observe_process_uptime_seconds",
	}
	for _, name := range expectedFamilies {
		if _, ok := families[name]; !ok {
			t.Errorf("missing expected metric family %q", name)
		}
	}

	// Verify build_info labels and value.
	buildMetrics := families["lazy_observe_build_info"].GetMetric()
	if len(buildMetrics) != 1 || buildMetrics[0].GetGauge().GetValue() != 1 {
		t.Fatalf("unexpected build_info: %+v", buildMetrics)
	}
	buildLabels := map[string]string{}
	for _, lp := range buildMetrics[0].GetLabel() {
		buildLabels[lp.GetName()] = lp.GetValue()
	}
	if buildLabels["version"] != "v0.2.0" || buildLabels["commit"] != "abc1234" {
		t.Fatalf("build_info labels=%v", buildLabels)
	}

	// Verify uptime gauge.
	uptimeVal := families["lazy_observe_process_uptime_seconds"].GetMetric()[0].GetGauge().GetValue()
	if uptimeVal != 42.5 {
		t.Fatalf("uptime=%v want 42.5", uptimeVal)
	}

	// Verify gRPC in-flight gauge.
	inFlightVal := families["lazy_observe_otlp_grpc_in_flight_requests"].GetMetric()[0].GetGauge().GetValue()
	if inFlightVal != 1 {
		t.Fatalf("in_flight=%v want 1", inFlightVal)
	}

	// Verify rate_limited counter incremented for both HTTP traces and gRPC logs.
	var httpTracesRL, grpcLogsRL float64
	for _, m := range families["lazy_observe_ingest_rate_limited_total"].GetMetric() {
		lbls := map[string]string{}
		for _, lp := range m.GetLabel() {
			lbls[lp.GetName()] = lp.GetValue()
		}
		if lbls["protocol"] == "http" && lbls["signal"] == "traces" {
			httpTracesRL = m.GetCounter().GetValue()
		}
		if lbls["protocol"] == "grpc" && lbls["signal"] == "logs" {
			grpcLogsRL = m.GetCounter().GetValue()
		}
	}
	if httpTracesRL != 1 || grpcLogsRL != 1 {
		t.Fatalf("rate_limited http/traces=%v grpc/logs=%v want 1,1", httpTracesRL, grpcLogsRL)
	}

	// Verify deterministic output order across repeated scrapes (ignoring live runtime gauges).
	var buf2 bytes.Buffer
	if err := reg.WritePrometheus(&buf2); err != nil {
		t.Fatalf("second WritePrometheus: %v", err)
	}
	extractOrder := func(raw string) []string {
		var names []string
		for _, line := range strings.Split(raw, "\n") {
			if name, ok := strings.CutPrefix(line, "# TYPE "); ok {
				names = append(names, strings.Fields(name)[0])
			}
		}
		return names
	}
	order1 := extractOrder(text)
	order2 := extractOrder(buf2.String())
	if strings.Join(order1, ",") != strings.Join(expectedFamilies, ",") {
		t.Fatalf("metric family order=%v want=%v", order1, expectedFamilies)
	}
	if strings.Join(order1, ",") != strings.Join(order2, ",") {
		t.Fatalf("non-deterministic family order: %v vs %v", order1, order2)
	}
}

func TestRegistryConcurrentUpdatesAndScrapes(t *testing.T) {
	reg := New(Config{Version: "test", Commit: "deadbeef"})
	var wg sync.WaitGroup
	for g := range 8 {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for i := range 100 {
				reg.ObserveHTTP(Surface(id%int(surfaceCount)), 200+((i%4)*100), time.Duration(i)*time.Millisecond)
				reg.ObserveIngest(Protocol(id%2), SignalTraces, Outcome(i%int(outcomeCount)))
				reg.IncGRPCInFlight()
				reg.DecGRPCInFlight()
				if i%25 == 0 {
					var buf bytes.Buffer
					if err := reg.WritePrometheus(&buf); err != nil {
						t.Errorf("WritePrometheus: %v", err)
					}
				}
			}
		}(g)
	}
	wg.Wait()

	var buf bytes.Buffer
	if err := reg.WritePrometheus(&buf); err != nil {
		t.Fatalf("final WritePrometheus: %v", err)
	}
	parser := expfmt.NewTextParser(model.UTF8Validation)
	if _, err := parser.TextToMetricFamilies(strings.NewReader(buf.String())); err != nil {
		t.Fatalf("final parse failed: %v", err)
	}
}

func TestRegistryBuildInfoLabelEscapingStrictPrometheus004(t *testing.T) {
	cases := []struct {
		name        string
		version     string
		commit      string
		wantVersion string
		wantCommit  string
	}{
		{
			name:        "quotes backslashes newlines carriage returns tabs nbsp",
			version:     "v1\t\"\\\n\r\u00a0end",
			commit:      "sha\\1\t\"2\n3\u00a0",
			wantVersion: "v1\t\"\\\n\r\u00a0end",
			wantCommit:  "sha\\1\t\"2\n3",
		},
		{
			name:        "invalid utf8 bytes",
			version:     "v1-\xff-\xfe-ok",
			commit:      "c-\x80-ok",
			wantVersion: "v1-\ufffd-\ufffd-ok",
			wantCommit:  "c-\ufffd-ok",
		},
		{
			name:        "label injection attempt",
			version:     "v1\",evil=\"1\nfake_metric 1",
			commit:      "abc\"}\nfake_other 99",
			wantVersion: "v1\",evil=\"1\nfake_metric 1",
			wantCommit:  "abc\"}\nfake_other 99",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reg := New(Config{Version: tc.version, Commit: tc.commit})
			var buf bytes.Buffer
			if err := reg.WritePrometheus(&buf); err != nil {
				t.Fatalf("WritePrometheus: %v", err)
			}
			parser := expfmt.NewTextParser(model.UTF8Validation)
			families, err := parser.TextToMetricFamilies(strings.NewReader(buf.String()))
			if err != nil {
				t.Fatalf("expfmt rejected output: %v\n%s", err, buf.String())
			}
			if _, injected := families["fake_metric"]; injected {
				t.Fatal("label injection created fake_metric family")
			}
			if _, injected := families["fake_other"]; injected {
				t.Fatal("label injection created fake_other family")
			}
			metrics := families["lazy_observe_build_info"].GetMetric()
			if len(metrics) != 1 {
				t.Fatalf("build_info metrics count=%d want 1", len(metrics))
			}
			labels := map[string]string{}
			for _, lp := range metrics[0].GetLabel() {
				labels[lp.GetName()] = lp.GetValue()
			}
			if len(labels) != 2 {
				t.Fatalf("unexpected label set: %v", labels)
			}
			if labels["version"] != tc.wantVersion || labels["commit"] != tc.wantCommit {
				t.Fatalf(
					"round-trip labels version=%q commit=%q want version=%q commit=%q",
					labels["version"],
					labels["commit"],
					tc.wantVersion,
					tc.wantCommit,
				)
			}
		})
	}
}
