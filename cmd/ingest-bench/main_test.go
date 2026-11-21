package main

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	logspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	metricspb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	tracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	"google.golang.org/protobuf/proto"
)

func TestApplyGateThresholds(t *testing.T) {
	baseline := result{
		SamplesPerSecond: 100,
		LogsPerSecond:    50,
		SpansPerSecond:   40,
		P99LatencyMS:     10,
		MaxRSSBytes:      1000,
	}
	data, _ := json.Marshal(baseline)
	path := filepath.Join(t.TempDir(), "baseline.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	passing := result{
		SamplesPerSecond: 60,
		LogsPerSecond:    30,
		SpansPerSecond:   24,
		P99LatencyMS:     30,
		MaxRSSBytes:      1500,
		Readback:         readbackResult{Verified: true},
	}
	if err := applyGate(&passing, path); err != nil || !passing.GatePassed {
		t.Fatalf("boundary should pass: result=%#v err=%v", passing, err)
	}
	failing := result{
		SamplesPerSecond: 59,
		LogsPerSecond:    29,
		SpansPerSecond:   23,
		P99LatencyMS:     30.1,
		MaxRSSBytes:      1501,
		RequestsFailed:   2,
		Readback:         readbackResult{Verified: false, Error: "metrics missing"},
	}
	if err := applyGate(&failing, path); err != nil || failing.GatePassed || len(failing.GateFailures) != 7 {
		t.Fatalf("regressions should fail (want 7 failures): result=%#v err=%v", failing, err)
	}
}

func TestApplyGateRejectsFailedRequestsReadbackAndTailLatencyRegression(t *testing.T) {
	baseline := result{
		SamplesPerSecond: 100,
		LogsPerSecond:    50,
		P99LatencyMS:     1.0,
		MaxRSSBytes:      1000,
		MaxSignalP99MS:   map[string]float64{"metrics": 25, "logs": 25, "traces": 30},
	}
	data, _ := json.Marshal(baseline)
	path := filepath.Join(t.TempDir(), "baseline.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}

	withFailures := result{
		SamplesPerSecond: 200,
		LogsPerSecond:    100,
		P99LatencyMS:     2.0,
		MaxRSSBytes:      500,
		RequestsFailed:   1,
		Readback:         readbackResult{Verified: true},
	}
	if err := applyGate(&withFailures, path); err != nil {
		t.Fatal(err)
	}
	if withFailures.GatePassed || len(withFailures.GateFailures) != 1 ||
		!strings.Contains(withFailures.GateFailures[0], "failed requests") {
		t.Fatalf("failed requests must fail gate: result=%#v", withFailures)
	}

	withHighP99 := result{
		SamplesPerSecond: 200,
		LogsPerSecond:    100,
		P99LatencyMS:     10.5,
		MaxRSSBytes:      500,
		Readback:         readbackResult{Verified: true},
	}
	if err := applyGate(&withHighP99, path); err != nil {
		t.Fatal(err)
	}
	if withHighP99.GatePassed || len(withHighP99.GateFailures) != 1 ||
		!strings.Contains(withHighP99.GateFailures[0], "p99 latency") {
		t.Fatalf("p99 latency above floor budget must fail gate: result=%#v", withHighP99)
	}

	withSignalP99Regression := result{
		SamplesPerSecond: 200,
		LogsPerSecond:    100,
		P99LatencyMS:     8.0,
		SignalLatency: map[string]latencyPercentiles{
			"metrics": {P99MS: 10},
			"logs":    {P99MS: 12},
			"traces":  {P99MS: 35},
		},
		MaxRSSBytes: 500,
		Readback:    readbackResult{Verified: true},
	}
	if err := applyGate(&withSignalP99Regression, path); err != nil {
		t.Fatal(err)
	}
	if withSignalP99Regression.GatePassed || len(withSignalP99Regression.GateFailures) != 1 ||
		!strings.Contains(withSignalP99Regression.GateFailures[0], "traces p99 latency") {
		t.Fatalf("per-signal p99 regression must fail gate: result=%#v", withSignalP99Regression)
	}

	withExplicitMaxP99 := result{
		SamplesPerSecond: 100,
		LogsPerSecond:    50,
		P99LatencyMS:     2.0,
		MaxP99LatencyMS:  50.0,
		MaxRSSBytes:      1000,
		MaxRSSLimitBytes: 2000,
	}
	explicitData, _ := json.Marshal(withExplicitMaxP99)
	explicitPath := filepath.Join(t.TempDir(), "explicit-baseline.json")
	if err := os.WriteFile(explicitPath, explicitData, 0o600); err != nil {
		t.Fatal(err)
	}
	inExplicitBudget := result{
		SamplesPerSecond: 100,
		LogsPerSecond:    50,
		P99LatencyMS:     45.0,
		MaxRSSBytes:      1900,
		Readback:         readbackResult{Verified: true},
	}
	if err := applyGate(&inExplicitBudget, explicitPath); err != nil || !inExplicitBudget.GatePassed {
		t.Fatalf("within explicit max_p99 and max_rss_limit should pass: %#v err=%v", inExplicitBudget, err)
	}
	overExplicitBudget := result{
		SamplesPerSecond: 100,
		LogsPerSecond:    50,
		P99LatencyMS:     50.1,
		MaxRSSBytes:      2001,
		Readback:         readbackResult{Verified: true},
	}
	if err := applyGate(&overExplicitBudget, explicitPath); err != nil || overExplicitBudget.GatePassed ||
		len(overExplicitBudget.GateFailures) != 2 {
		t.Fatalf("over explicit budgets should fail: %#v err=%v", overExplicitBudget, err)
	}
}

func TestPayloadPoolGeneratesVariedValidOTLPPayloads(t *testing.T) {
	const runID = "bench_0123456789abcdef"
	pool, err := newPayloadPool(runID, 42, 8, 8)
	if err != nil {
		t.Fatal(err)
	}

	now1 := uint64(1_700_000_000_000_000_000)
	now2 := now1 + 5_000_000

	m1Raw, svcIdx1 := pool.materializeMetric(nil, 0, 1, now1)
	m1 := append([]byte(nil), m1Raw...)
	m2Raw, svcIdx2 := pool.materializeMetric(nil, 1, 2, now2)
	m2 := append([]byte(nil), m2Raw...)
	if bytes.Equal(m1, m2) || svcIdx1 < 0 || svcIdx1 >= len(benchServices) || svcIdx2 < 0 ||
		svcIdx2 >= len(benchServices) {
		t.Fatalf("metric payloads must vary with valid svcIndex: idx1=%d idx2=%d", svcIdx1, svcIdx2)
	}
	var mReq1, mReq2 metricspb.ExportMetricsServiceRequest
	if err := proto.Unmarshal(m1, &mReq1); err != nil {
		t.Fatalf("unmarshal metric 1: %v", err)
	}
	if err := proto.Unmarshal(m2, &mReq2); err != nil {
		t.Fatalf("unmarshal metric 2: %v", err)
	}
	pts1 := mReq1.ResourceMetrics[0].ScopeMetrics[0].Metrics[0].GetGauge().DataPoints
	pts2 := mReq2.ResourceMetrics[0].ScopeMetrics[0].Metrics[0].GetGauge().DataPoints
	if len(pts1) != 8 || len(pts2) != 8 {
		t.Fatalf("unexpected datapoint counts: %d %d", len(pts1), len(pts2))
	}
	if pts1[0].TimeUnixNano != now1 || pts2[0].TimeUnixNano != now2 || pts1[0].GetAsDouble() == pts2[0].GetAsDouble() {
		t.Fatalf("metric timestamps or values did not update: p1=%v p2=%v", pts1[0], pts2[0])
	}

	l1 := append([]byte(nil), pool.materializeLog(nil, 0, 1, now1)...)
	l2 := append([]byte(nil), pool.materializeLog(nil, 1, 2, now2)...)
	if bytes.Equal(l1, l2) {
		t.Fatal("log payloads must vary across requests")
	}
	var lReq1, lReq2 logspb.ExportLogsServiceRequest
	if err := proto.Unmarshal(l1, &lReq1); err != nil {
		t.Fatalf("unmarshal log 1: %v", err)
	}
	if err := proto.Unmarshal(l2, &lReq2); err != nil {
		t.Fatalf("unmarshal log 2: %v", err)
	}
	r1 := lReq1.ResourceLogs[0].ScopeLogs[0].LogRecords[0]
	r2 := lReq2.ResourceLogs[0].ScopeLogs[0].LogRecords[0]
	if r1.TimeUnixNano != now1 || r2.TimeUnixNano != now2 {
		t.Fatalf("log timestamps mismatch: %d vs %d", r1.TimeUnixNano, r2.TimeUnixNano)
	}
	if len(r1.TraceId) != 16 || len(r1.SpanId) != 8 || bytes.Equal(r1.TraceId, r2.TraceId) ||
		bytes.Equal(r1.SpanId, r2.SpanId) {
		t.Fatalf("log trace/span IDs must be valid and distinct")
	}
	if !strings.Contains(r1.Body.GetStringValue(), runID) || r1.Body.GetStringValue() == r2.Body.GetStringValue() {
		t.Fatalf(
			"log bodies should include runID and vary: %q vs %q",
			r1.Body.GetStringValue(),
			r2.Body.GetStringValue(),
		)
	}

	t1, traceID1, svc1 := pool.materializeTrace(nil, 0, 1, now1)
	t1Copy := append([]byte(nil), t1...)
	t2, traceID2, _ := pool.materializeTrace(nil, 1, 2, now2)
	if bytes.Equal(t1Copy, t2) || traceID1 == traceID2 || len(traceID1) != 32 || svc1 == "" {
		t.Fatalf("trace payloads must vary with distinct 32-hex trace IDs: %q vs %q", traceID1, traceID2)
	}
	var tReq tracepb.ExportTraceServiceRequest
	if err := proto.Unmarshal(t1Copy, &tReq); err != nil {
		t.Fatalf("unmarshal trace: %v", err)
	}
	spans := tReq.ResourceSpans[0].ScopeSpans[0].Spans
	if len(spans) != 8 {
		t.Fatalf("spans count = %d, want 8", len(spans))
	}
	if hex.EncodeToString(spans[0].TraceId) != traceID1 || len(spans[0].ParentSpanId) != 0 {
		t.Fatalf("root span invalid: traceID=%x parent=%x", spans[0].TraceId, spans[0].ParentSpanId)
	}
	if hex.EncodeToString(spans[1].TraceId) != traceID1 || !bytes.Equal(spans[1].ParentSpanId, spans[0].SpanId) {
		t.Fatalf("child span does not link to root span")
	}
	if spans[0].StartTimeUnixNano != now1 || spans[0].EndTimeUnixNano <= spans[0].StartTimeUnixNano {
		t.Fatalf("span timestamps invalid: start=%d end=%d", spans[0].StartTimeUnixNano, spans[0].EndTimeUnixNano)
	}
}

func TestParseProcStatusAndAggregateProcessTreeRSS(t *testing.T) {
	statusSample := `Name:	victoria-metric
Umask:	0022
State:	S (sleeping)
Tgid:	200
Pid:	200
PPid:	100
VmPeak:	  900000 kB
VmRSS:	   65536 kB
Threads:	8
`
	name, ppid, rss, ok := parseProcStatus([]byte(statusSample))
	if !ok || name != "victoria-metric" || ppid != 100 || rss != 65536*1024 {
		t.Fatalf("parseProcStatus() = (%q, %d, %d, %v)", name, ppid, rss, ok)
	}
	if _, _, _, ok := parseProcStatus([]byte("Name:\tkthread\nPPid:\t2\n")); ok {
		t.Fatal("status without VmRSS should return ok=false")
	}

	psSample := `
  100     1 102400 /tmp/bin/lazy-observe
  201   100 120000 /tmp/bin/victoria-metrics-prod
  202   100  80000 /tmp/bin/victoria-logs-prod
  203   100  70000 /tmp/bin/victoria-traces-prod
  204   100  30000 /tmp/bin/vmagent-prod
  205   201   5000 grandchild-helper
  999     1 500000 unrelated-daemon
`
	entries := parsePSProcessTable([]byte(psSample))
	tree, err := aggregateProcessTreeRSS(100, entries)
	if err != nil {
		t.Fatal(err)
	}
	wantTotal := uint64(102400+120000+80000+70000+30000+5000) * 1024
	if tree.TotalRSSBytes != wantTotal {
		t.Fatalf("TotalRSSBytes = %d, want %d", tree.TotalRSSBytes, wantTotal)
	}
	if tree.RootRSSBytes != 102400*1024 {
		t.Fatalf("RootRSSBytes = %d, want %d", tree.RootRSSBytes, 102400*1024)
	}
	if tree.ComponentRSSBytes["victoria-metrics-prod"] != 120000*1024 ||
		tree.ComponentRSSBytes["victoria-logs-prod"] != 80000*1024 ||
		tree.ComponentRSSBytes["victoria-traces-prod"] != 70000*1024 ||
		tree.ComponentRSSBytes["vmagent-prod"] != 30000*1024 ||
		tree.ComponentRSSBytes["grandchild-helper"] != 5000*1024 {
		t.Fatalf("unexpected ComponentRSSBytes: %#v", tree.ComponentRSSBytes)
	}
	if _, leaked := tree.ComponentRSSBytes["unrelated-daemon"]; leaked {
		t.Fatalf("unrelated process must not be included: %#v", tree.ComponentRSSBytes)
	}
	if len(tree.Processes) != 6 {
		t.Fatalf("len(Processes) = %d, want 6", len(tree.Processes))
	}
	if _, err := aggregateProcessTreeRSS(777, entries); err == nil {
		t.Fatal("missing root pid must return error")
	}
}

func TestVerifyReadbackSucceedsAfterFlushAndFailsWhenMissing(t *testing.T) {
	const (
		runID   = "bench_deadbeefcafebabe"
		traceID = "0123456789abcdef0123456789abcdef"
		token   = "lou_test_read_token"
		project = "benchmark"
	)

	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+token || r.Header.Get(projectHeader) != project {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		attempt := attempts.Add(1)
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/query":
			if attempt == 1 {
				_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[]}}`))
				return
			}
			_, _ = w.Write(
				[]byte(`{"status":"success","data":{"resultType":"vector","result":[{"value":[1700000000,"200"]}]}}`),
			)
		case "/api/v1/logs/hits":
			_, _ = w.Write([]byte(`{"hits":[{"total":400}]}`))
		case "/api/v1/logs/query":
			w.Header().Set("Content-Type", "application/stream+json")
			_, _ = fmt.Fprintf(
				w,
				"{\"_msg\":\"checkout run_id=%s slot=0\",\"_time\":\"2026-11-21T00:00:00Z\",\"%s\":\"%s\"}\n"+
					"{\"_msg\":\"orders run_id=%s slot=1\",\"_time\":\"2026-11-21T00:00:01Z\",\"%s\":\"%s\"}\n",
				runID, benchRunIDAttribute, runID,
				runID, benchRunIDAttribute, runID,
			)
		case "/api/traces/" + traceID:
			_, _ = fmt.Fprintf(
				w,
				`{"data":[{"traceID":"%s","spans":[`+
					`{"traceID":"%s","spanID":"1111111111111111","operationName":"POST /api/v1/checkout","processID":"p1","tags":[{"key":"%s","value":"%s"}]},`+
					`{"traceID":"%s","spanID":"2222222222222222","operationName":"GET /api/v1/orders","processID":"p1","tags":[{"key":"%s","value":"%s"}]}`+
					`],"processes":{"p1":{"serviceName":"bench-checkout-api","tags":[{"key":"%s","value":"%s"}]}}}]}`,
				traceID,
				traceID, benchRunIDAttribute, runID,
				traceID, benchRunIDAttribute, runID,
				benchRunIDAttribute, runID,
			)
		case "/api/traces/search":
			if !strings.Contains(r.URL.Query().Get("tags"), runID) {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			_, _ = fmt.Fprintf(
				w,
				`{"data":[{"traceID":"%s","spans":[{"traceID":"%s","spanID":"1111111111111111","operationName":"POST /api/v1/checkout","processID":"p1","tags":[{"key":"%s","value":"%s"}]}],"processes":{"p1":{"serviceName":"bench-checkout-api","tags":[{"key":"%s","value":"%s"}]}}}]}`,
				traceID,
				traceID,
				benchRunIDAttribute,
				runID,
				benchRunIDAttribute,
				runID,
			)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	res := verifyReadback(context.Background(), srv.Client(), readbackConfig{
		baseURL:               srv.URL,
		readToken:             token,
		projectSlug:           project,
		runID:                 runID,
		expectedSeries:        200,
		expectedMinLogs:       200,
		expectedSpansPerTrace: 2,
		sampleTraceIDs:        []string{traceID},
		traceService:          "bench-checkout-api",
		windowStart:           time.Now().Add(-5 * time.Second),
		timeout:               2 * time.Second,
		pollInterval:          20 * time.Millisecond,
	})
	if !res.Verified || !res.MetricsVerified || !res.LogsVerified || !res.TracesVerified {
		t.Fatalf("expected readback to pass after retry: %#v", res)
	}
	if res.ObservedSeries != 200 || res.ObservedLogs != 400 || len(res.VerifiedTraceIDs) != 1 {
		t.Fatalf("unexpected readback counts: %#v", res)
	}

	emptySrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/query":
			_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[]}}`))
		case "/api/v1/logs/hits":
			_, _ = w.Write([]byte(`{"hits":[{"total":0}]}`))
		default:
			_, _ = w.Write([]byte(`{"data":[]}`))
		}
	}))
	defer emptySrv.Close()

	failed := verifyReadback(context.Background(), emptySrv.Client(), readbackConfig{
		baseURL:         emptySrv.URL,
		readToken:       token,
		projectSlug:     project,
		runID:           runID,
		expectedSeries:  200,
		expectedMinLogs: 200,
		sampleTraceIDs:  []string{traceID},
		traceService:    "bench-checkout-api",
		windowStart:     time.Now().Add(-5 * time.Second),
		timeout:         80 * time.Millisecond,
		pollInterval:    20 * time.Millisecond,
	})
	if failed.Verified || failed.Error == "" {
		t.Fatalf("empty storage must fail readback: %#v", failed)
	}
}

func TestRepositoryBaselineContract(t *testing.T) {
	path := filepath.Join("..", "..", "bench", "baseline.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var baseline result
	if err := json.Unmarshal(data, &baseline); err != nil {
		t.Fatal(err)
	}
	if !baseline.RSSIncludesChildren || baseline.MaxRSSBytes == 0 || resolveRSSLimit(baseline) < baseline.MaxRSSBytes {
		t.Fatalf("baseline RSS contract invalid: %#v", baseline)
	}
	if baseline.P99LatencyMS <= 0 || resolveP99Budget(baseline) <= 0 {
		t.Fatalf("baseline p99 latency contract invalid: %#v", baseline)
	}
	if baseline.SamplesPerSecond <= 0 || baseline.LogsPerSecond <= 0 || baseline.SpansPerSecond <= 0 {
		t.Fatalf("baseline throughput contract invalid: %#v", baseline)
	}
	for _, sig := range []string{"metrics", "logs", "traces"} {
		if baseline.MaxSignalP99MS[sig] <= 0 {
			t.Fatalf("baseline missing positive max_signal_p99_ms for %s: %#v", sig, baseline.MaxSignalP99MS)
		}
	}
}

func TestValidateOTLPResponseRejectsPartialSuccess(t *testing.T) {
	mustMarshal := func(msg proto.Message) []byte {
		t.Helper()
		raw, err := proto.Marshal(msg)
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}

	// Clean responses pass for all three signals.
	if err := validateOTLPResponse(mustMarshal(&metricspb.ExportMetricsServiceResponse{}), signalMetrics); err != nil {
		t.Fatalf("clean metrics response failed: %v", err)
	}
	if err := validateOTLPResponse(mustMarshal(&logspb.ExportLogsServiceResponse{}), signalLogs); err != nil {
		t.Fatalf("clean logs response failed: %v", err)
	}
	if err := validateOTLPResponse(mustMarshal(&tracepb.ExportTraceServiceResponse{}), signalTraces); err != nil {
		t.Fatalf("clean traces response failed: %v", err)
	}

	cases := []struct {
		name string
		sig  signalKind
		body []byte
	}{
		{
			name: "metrics rejected points",
			sig:  signalMetrics,
			body: mustMarshal(&metricspb.ExportMetricsServiceResponse{
				PartialSuccess: &metricspb.ExportMetricsPartialSuccess{RejectedDataPoints: 3},
			}),
		},
		{
			name: "metrics error message",
			sig:  signalMetrics,
			body: mustMarshal(&metricspb.ExportMetricsServiceResponse{
				PartialSuccess: &metricspb.ExportMetricsPartialSuccess{ErrorMessage: "dropped metric"},
			}),
		},
		{
			name: "logs rejected records",
			sig:  signalLogs,
			body: mustMarshal(&logspb.ExportLogsServiceResponse{
				PartialSuccess: &logspb.ExportLogsPartialSuccess{RejectedLogRecords: 2},
			}),
		},
		{
			name: "logs error message",
			sig:  signalLogs,
			body: mustMarshal(&logspb.ExportLogsServiceResponse{
				PartialSuccess: &logspb.ExportLogsPartialSuccess{ErrorMessage: "dropped log"},
			}),
		},
		{
			name: "traces rejected spans",
			sig:  signalTraces,
			body: mustMarshal(&tracepb.ExportTraceServiceResponse{
				PartialSuccess: &tracepb.ExportTracePartialSuccess{RejectedSpans: 5},
			}),
		},
		{
			name: "traces error message",
			sig:  signalTraces,
			body: mustMarshal(&tracepb.ExportTraceServiceResponse{
				PartialSuccess: &tracepb.ExportTracePartialSuccess{ErrorMessage: "dropped span"},
			}),
		},
		{
			name: "malformed protobuf",
			sig:  signalTraces,
			body: []byte{0xff, 0xff, 0xff},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := validateOTLPResponse(tc.body, tc.sig); err == nil {
				t.Fatalf("expected %s to be rejected", tc.name)
			}
		})
	}
}

func TestApplyStandaloneGateRejectsFailedReadback(t *testing.T) {
	passing := result{
		MaxRSSBytes: 1024,
		Readback:    readbackResult{Verified: true},
	}
	applyStandaloneGate(&passing)
	if !passing.GatePassed || len(passing.GateFailures) != 0 {
		t.Fatalf("healthy standalone result should pass: %#v", passing)
	}

	failedReadback := result{
		MaxRSSBytes: 1024,
		Readback:    readbackResult{Verified: false, Error: "logs missing"},
	}
	applyStandaloneGate(&failedReadback)
	if failedReadback.GatePassed || len(failedReadback.GateFailures) != 1 ||
		!strings.Contains(failedReadback.GateFailures[0], "readback verification failed") {
		t.Fatalf("failed readback must fail standalone gate: %#v", failedReadback)
	}

	missingAll := result{
		RequestsFailed: 3,
		MaxRSSBytes:    0,
		Readback:       readbackResult{Verified: false},
	}
	applyStandaloneGate(&missingAll)
	if missingAll.GatePassed || len(missingAll.GateFailures) != 3 {
		t.Fatalf("expected 3 standalone failures (requests, readback, rss): %#v", missingAll)
	}
}

func TestValidateTraceReadbackRejectsMissingRunIDOrMissingSpans(t *testing.T) {
	const (
		traceID = "0123456789abcdef0123456789abcdef"
		runID   = "bench_feedface12345678"
		service = "bench-checkout-api"
	)

	validDetail := []byte(fmt.Sprintf(
		`{"data":[{"traceID":"%s","spans":[`+
			`{"traceID":"%s","spanID":"aaaa1111aaaa1111","operationName":"op1","processID":"p1","tags":[{"key":"%s","value":"%s"}]},`+
			`{"traceID":"%s","spanID":"bbbb2222bbbb2222","operationName":"op2","processID":"p1"}`+
			`],"processes":{"p1":{"serviceName":"%s"}}}]}`,
		traceID, traceID, benchRunIDAttribute, runID, traceID, service,
	))
	if err := validateTraceDetailBody(validDetail, traceID, runID, 2); err != nil {
		t.Fatalf("valid trace detail rejected: %v", err)
	}

	// Duplicate span IDs must not satisfy minSpans=2.
	duplicateSpans := []byte(fmt.Sprintf(
		`{"data":[{"traceID":"%s","spans":[`+
			`{"traceID":"%s","spanID":"aaaa1111aaaa1111","operationName":"op1","tags":[{"key":"%s","value":"%s"}]},`+
			`{"traceID":"%s","spanID":"aaaa1111aaaa1111","operationName":"op1","tags":[{"key":"%s","value":"%s"}]}`+
			`]}]}`,
		traceID, traceID, benchRunIDAttribute, runID, traceID, benchRunIDAttribute, runID,
	))
	if err := validateTraceDetailBody(duplicateSpans, traceID, runID, 2); err == nil {
		t.Fatal("duplicate span IDs should not pass minSpans=2")
	}

	// Missing bench_run_id must be rejected even with enough unique spans.
	missingRunDetail := []byte(fmt.Sprintf(
		`{"data":[{"traceID":"%s","spans":[`+
			`{"traceID":"%s","spanID":"aaaa1111aaaa1111","operationName":"op1"},`+
			`{"traceID":"%s","spanID":"bbbb2222bbbb2222","operationName":"op2"}`+
			`]}]}`,
		traceID, traceID, traceID,
	))
	if err := validateTraceDetailBody(missingRunDetail, traceID, runID, 2); err == nil {
		t.Fatal("trace detail without bench_run_id must be rejected")
	}

	// Search must require non-empty spans, matching serviceName, and bench_run_id.
	if err := validateTraceSearchBody(validDetail, service, runID); err != nil {
		t.Fatalf("valid trace search rejected: %v", err)
	}
	wrongService := []byte(fmt.Sprintf(
		`{"data":[{"traceID":"%s","spans":[{"traceID":"%s","spanID":"aaaa1111aaaa1111","operationName":"op1","tags":[{"key":"%s","value":"%s"}]}],"processes":{"p1":{"serviceName":"other-service"}}}]}`,
		traceID,
		traceID,
		benchRunIDAttribute,
		runID,
	))
	if err := validateTraceSearchBody(wrongService, service, runID); err == nil {
		t.Fatal("trace search with wrong serviceName must be rejected")
	}
	missingRunSearch := []byte(fmt.Sprintf(
		`{"data":[{"traceID":"%s","spans":[{"traceID":"%s","spanID":"aaaa1111aaaa1111","operationName":"op1"}],"processes":{"p1":{"serviceName":"%s"}}}]}`,
		traceID,
		traceID,
		service,
	))
	if err := validateTraceSearchBody(missingRunSearch, service, runID); err == nil {
		t.Fatal("trace search without bench_run_id must be rejected")
	}
	emptySpansSearch := []byte(fmt.Sprintf(
		`{"data":[{"traceID":"%s","spans":[],"processes":{"p1":{"serviceName":"%s","tags":[{"key":"%s","value":"%s"}]}}}]}`,
		traceID,
		service,
		benchRunIDAttribute,
		runID,
	))
	if err := validateTraceSearchBody(emptySpansSearch, service, runID); err == nil {
		t.Fatal("trace search with empty spans must be rejected")
	}
}
