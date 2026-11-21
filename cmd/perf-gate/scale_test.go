package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	logspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	metricspb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	"google.golang.org/protobuf/proto"
)

const scalePhaseTestDuration = 100 * time.Millisecond

const testIngestToken = "lo_project_ingest_secret"

var testReadAuth = readCredential{token: "lou_user_read_secret", project: "perf-project"}

// assertReadCredential checks that a read request carries the user API token and
// project selector, never the project ingest token.
func assertReadCredential(t *testing.T, r *http.Request) {
	t.Helper()
	if r.Method != http.MethodGet {
		t.Errorf("read %s used method %s", r.URL.Path, r.Method)
	}
	if r.Header.Get("Authorization") != "Bearer "+testReadAuth.token ||
		r.Header.Get(projectHeader) != testReadAuth.project ||
		r.Header.Get("Cache-Control") != "no-cache" {
		t.Errorf("read %s headers = %#v", r.URL.Path, r.Header)
	}
}

func TestDiscoverScaleTargetIdentitiesRequiresDistinctEqualResourceProcesses(t *testing.T) {
	server := func(instanceID string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != scaleInstanceIdentityPath {
				t.Errorf("path = %q", r.URL.Path)
			}
			assertReadCredential(t, r)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"instance_id":"` + instanceID + `","gomaxprocs":1}`))
		}))
	}
	first := server("instance-a")
	defer first.Close()
	second := server("instance-b")
	defer second.Close()

	identities, err := discoverScaleTargetIdentities(
		context.Background(),
		first.Client(),
		[]string{first.URL, second.URL},
		testReadAuth,
		1,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(identities) != 2 || identities[0].InstanceID != "instance-a" || identities[1].InstanceID != "instance-b" ||
		identities[0].GOMAXPROCS != 1 {
		t.Fatalf("identities = %v", identities)
	}

	duplicate := server("instance-a")
	defer duplicate.Close()
	_, err = discoverScaleTargetIdentities(
		context.Background(),
		first.Client(),
		[]string{first.URL, duplicate.URL},
		testReadAuth,
		1,
	)
	if err == nil || !strings.Contains(err.Error(), "same process instance") {
		t.Fatalf("duplicate identity error = %v", err)
	}
	if strings.Contains(err.Error(), first.URL) || strings.Contains(err.Error(), duplicate.URL) {
		t.Fatalf("identity error exposed a raw URL: %v", err)
	}
}

func TestScaleMeasurementAndReadbackTargetsMustBeDisjoint(t *testing.T) {
	measurement := []string{"http://a", "http://b"}
	if disjointScaleTargets(measurement, []string{"http://b", "http://a"}) {
		t.Fatal("reordered measurement targets were accepted as readback targets")
	}
	if disjointScaleTargets(measurement, []string{"http://c", "http://b"}) {
		t.Fatal("partially overlapping readback targets were accepted")
	}
	if !disjointScaleTargets(measurement, []string{"http://c", "http://d"}) {
		t.Fatal("disjoint readback targets were rejected")
	}
}

func TestDiscoverControlledSinkIdentityRequiresNonceHeader(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set(scaleSinkIdentityHeader, "losink_0123456789abcdef")
		_, _ = w.Write([]byte("OK"))
	}))
	defer server.Close()
	identity, err := discoverControlledSinkIdentity(context.Background(), server.Client(), server.URL)
	if err != nil || identity != "losink_0123456789abcdef" {
		t.Fatalf("identity=%q err=%v", identity, err)
	}
}

func TestDiscoverScaleTargetIdentitiesRejectsUnequalCPUEnvelope(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"instance_id":"instance-a","gomaxprocs":2}`))
	}))
	defer server.Close()
	_, err := discoverScaleTargetIdentities(
		context.Background(),
		server.Client(),
		[]string{server.URL},
		testReadAuth,
		1,
	)
	if err == nil || !strings.Contains(err.Error(), "fixed per-replica budget") {
		t.Fatalf("resource-envelope error = %v", err)
	}
}

func TestDiscoverScaleTargetIdentitiesRejectsContractDrift(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"instance_id":"one","storage_url":"http://user:password@example.invalid"}`))
	}))
	defer server.Close()
	_, err := discoverScaleTargetIdentities(
		context.Background(),
		server.Client(),
		[]string{server.URL},
		testReadAuth,
		1,
	)
	if err == nil || !strings.Contains(err.Error(), "invalid JSON contract") {
		t.Fatalf("contract error = %v", err)
	}
	if strings.Contains(err.Error(), "password") || strings.Contains(err.Error(), server.URL) {
		t.Fatalf("contract error exposed response or URL: %v", err)
	}
}

func TestParseTargetsDoesNotEchoCredentials(t *testing.T) {
	for _, target := range []string{
		"http://user:top-secret@example.invalid,http://other.invalid",
		"http://user:top-secret%zz@example.invalid,http://other.invalid",
	} {
		_, err := parseTargets(target)
		if err == nil {
			t.Fatal("credential-bearing target must be rejected")
		}
		if strings.Contains(err.Error(), "top-secret") || strings.Contains(err.Error(), "example.invalid") {
			t.Fatalf("target error exposed a raw URL: %v", err)
		}
	}
}

func TestValidateOTLPExportResponseRejectsPartialSuccess(t *testing.T) {
	if err := validateOTLPExportResponse(nil, true); err != nil {
		t.Fatalf("empty metrics response: %v", err)
	}
	if err := validateOTLPExportResponse(nil, false); err != nil {
		t.Fatalf("empty logs response: %v", err)
	}
	metricBody, err := proto.Marshal(
		&metricspb.ExportMetricsServiceResponse{PartialSuccess: &metricspb.ExportMetricsPartialSuccess{
			RejectedDataPoints: 2, ErrorMessage: "sensitive backend detail",
		}},
	)
	if err != nil {
		t.Fatal(err)
	}
	err = validateOTLPExportResponse(metricBody, true)
	if err == nil || !strings.Contains(err.Error(), "rejected 2") || strings.Contains(err.Error(), "sensitive") {
		t.Fatalf("metrics partial success error = %v", err)
	}
	logBody, err := proto.Marshal(&logspb.ExportLogsServiceResponse{PartialSuccess: &logspb.ExportLogsPartialSuccess{
		ErrorMessage: "warning without an explicit rejection count",
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err := validateOTLPExportResponse(logBody, false); err == nil {
		t.Fatal("partial-success warning must fail the acceptance gate")
	}
	if err := validateOTLPExportResponse([]byte("not protobuf"), true); err == nil {
		t.Fatal("invalid protobuf response must fail")
	}
}

func TestExecuteScaleExportCountsPartialSuccessAsFailure(t *testing.T) {
	body, err := proto.Marshal(
		&logspb.ExportLogsServiceResponse{PartialSuccess: &logspb.ExportLogsPartialSuccess{RejectedLogRecords: 1}},
	)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Content-Type") != "application/x-protobuf" ||
			r.Header.Get("Authorization") != "Bearer secret" {
			t.Errorf("headers = %#v", r.Header)
		}
		w.Header().Set("Content-Type", "application/x-protobuf")
		w.Header().Set(scaleSinkIdentityHeader, "losink_testfixture")
		_, _ = w.Write(body)
	}))
	defer server.Close()
	status, identity, _, _, err := executeScaleExport(
		context.Background(),
		server.Client(),
		server.URL,
		"secret",
		"loreq_0123456789abcdef01234567_1", // gitleaks:allow 测试夹具，不是凭据
		[]byte("request"),
		false,
	)
	if status != http.StatusOK || err == nil || !strings.Contains(err.Error(), "rejected 1") {
		t.Fatalf("executeScaleExport() = %d, %q, %v", status, identity, err)
	}
}

func TestExecuteScaleExportOmitsEmptyTokenForDirectControl(t *testing.T) {
	const requestID = "loreq_0123456789abcdef01234567_1"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			t.Errorf("direct controlled upstream received Authorization")
		}
		writeScaleSinkSuccess(t, w, r, "losink_testfixture")
	}))
	defer server.Close()
	status, identity, receipt, echoed, err := executeScaleExport(
		context.Background(),
		server.Client(),
		server.URL,
		"",
		requestID,
		[]byte("request"),
		true,
	)
	if err != nil || status != http.StatusOK || identity != "losink_testfixture" || echoed != requestID ||
		receipt != fmt.Sprintf("%x", sha256.Sum256([]byte("request"))) {
		t.Fatalf("direct export=%d %q %q %q err=%v", status, identity, receipt, echoed, err)
	}
}

func TestNewScalePayloadCarriesUniqueReadbackMarkers(t *testing.T) {
	payload, err := newScalePayload(2)
	if err != nil {
		t.Fatal(err)
	}
	if len(payload.marker) != 24 {
		t.Fatalf("marker = %q", payload.marker)
	}
	var metricsRequest metricspb.ExportMetricsServiceRequest
	if err := proto.Unmarshal(payload.metricBody, &metricsRequest); err != nil {
		t.Fatal(err)
	}
	points := metricsRequest.ResourceMetrics[0].ScopeMetrics[0].Metrics[0].GetGauge().DataPoints
	if len(points) != 2 {
		t.Fatalf("metric points = %d", len(points))
	}
	for _, point := range points {
		found := false
		for _, attribute := range point.Attributes {
			found = found || attribute.Key == scaleMarkerAttribute && attribute.Value.GetStringValue() == payload.marker
		}
		if !found {
			t.Fatalf("point does not carry marker: %#v", point.Attributes)
		}
	}
	var logsRequest logspb.ExportLogsServiceRequest
	if err := proto.Unmarshal(payload.logBody, &logsRequest); err != nil {
		t.Fatal(err)
	}
	for _, record := range logsRequest.ResourceLogs[0].ScopeLogs[0].LogRecords {
		if record.Body.GetStringValue() != scaleLogMessagePrefix+payload.marker {
			t.Fatalf("log marker = %q", record.Body.GetStringValue())
		}
	}
}

func TestMeasureScalePhaseKeepsSingleAndScaledTotalConcurrencyEqual(t *testing.T) {
	const sinkIdentity = "losink_testfixture"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeScaleSinkSuccess(t, w, r, sinkIdentity)
	}))
	defer server.Close()
	const fixedTotalConcurrency = 4
	baseline, err := measureScalePhase(
		context.Background(),
		server.Client(),
		[]string{server.URL},
		"secret",
		sinkIdentity,
		nil,
		nil,
		1,
		fixedTotalConcurrency,
		scalePhaseTestDuration,
	)
	if err != nil {
		t.Fatal(err)
	}
	scaled, err := measureScalePhase(
		context.Background(),
		server.Client(),
		[]string{server.URL, server.URL},
		"secret",
		sinkIdentity,
		nil,
		nil,
		1,
		fixedTotalConcurrency/2,
		scalePhaseTestDuration,
	)
	if err != nil {
		t.Fatal(err)
	}
	if baseline.Concurrency != fixedTotalConcurrency || scaled.Concurrency != fixedTotalConcurrency {
		t.Fatalf("baseline/scaled concurrency = %d/%d", baseline.Concurrency, scaled.Concurrency)
	}
	if baseline.MetricSamples+baseline.LogRecords == 0 || scaled.MetricSamples+scaled.LogRecords == 0 {
		t.Fatalf("baseline/scaled did not execute requests: %#v / %#v", baseline, scaled)
	}
}

func TestMeasureScalePhaseClassifiesHTTPFailures(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()
	phase, err := measureScalePhase(
		context.Background(),
		server.Client(),
		[]string{server.URL},
		"secret",
		"losink_testfixture",
		nil,
		nil,
		1,
		2,
		scalePhaseTestDuration,
	)
	if err != nil {
		t.Fatal(err)
	}
	if phase.RequestsFailed == 0 || phase.FailureKinds["http_503"] != phase.RequestsFailed ||
		len(phase.FailureKindsPerTarget) != 1 || phase.FailureKindsPerTarget[0]["http_503"] != phase.RequestsFailed {
		t.Fatalf("failure diagnostics = %#v", phase)
	}
}

func TestMeasureScalePhaseRejectsUnexpectedUpstreamIdentity(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/x-protobuf")
		w.Header().Set(scaleSinkIdentityHeader, "losink_wrongfixture")
	}))
	defer server.Close()
	phase, err := measureScalePhase(
		context.Background(),
		server.Client(),
		[]string{server.URL},
		"secret",
		"losink_expectedfixture",
		nil,
		nil,
		1,
		2,
		scalePhaseTestDuration,
	)
	if err != nil {
		t.Fatal(err)
	}
	if phase.RequestsFailed == 0 || phase.FailureKinds["upstream_identity_mismatch"] != phase.RequestsFailed {
		t.Fatalf("identity failure diagnostics = %#v", phase)
	}
}

func TestMeasureScalePhaseRejectsInvalidUpstreamPayloadProof(t *testing.T) {
	tests := []struct {
		name        string
		failureKind string
		writeProof  func(http.ResponseWriter, *http.Request)
	}{
		{
			name:        "payload receipt mismatch",
			failureKind: "upstream_receipt_mismatch",
			writeProof: func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set(scaleSinkReceiptHeader, strings.Repeat("0", sha256.Size*2))
				w.Header().Set(scaleSinkRequestHeader, r.Header.Get(scaleSinkRequestHeader))
			},
		},
		{
			name:        "request proof mismatch",
			failureKind: "upstream_request_proof_mismatch",
			writeProof: func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set(scaleSinkReceiptHeader, fmt.Sprintf("%x", sha256.Sum256(nil)))
				w.Header().Set(scaleSinkRequestHeader, "loreq_0123456789abcdef01234567_1")
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/x-protobuf")
				w.Header().Set(scaleSinkIdentityHeader, "losink_testfixture")
				test.writeProof(w, r)
			}))
			defer server.Close()
			phase, err := measureScalePhase(
				context.Background(),
				server.Client(),
				[]string{server.URL},
				"secret",
				"losink_testfixture",
				nil,
				nil,
				1,
				2,
				scalePhaseTestDuration,
			)
			if err != nil {
				t.Fatal(err)
			}
			if phase.RequestsFailed == 0 || phase.FailureKinds[test.failureKind] != phase.RequestsFailed {
				t.Fatalf("proof failure diagnostics = %#v", phase)
			}
		})
	}
}

func TestScalePhaseOrderBalancesTemporalPositions(t *testing.T) {
	want := [][]int{{0, 1, 3, 2}, {1, 2, 0, 3}, {2, 3, 1, 0}, {3, 0, 2, 1}}
	for round := range want {
		got := balancedScalePhaseOrder(4, round)
		if !slices.Equal(got, want[round]) {
			t.Fatalf("round %d order = %v, want %v", round+1, got, want[round])
		}
	}
	if !validScaleRoundCount(4, 4) || validScaleRoundCount(3, 4) || validScaleRoundCount(4, 6) ||
		validScaleRoundCount(0, 4) {
		t.Fatal("scale round count accepted an unbalanced value")
	}
	if got := formatFailureKinds(
		map[string]uint64{"http_503": 2, "transport_or_io": 1},
	); got != "http_503=2,transport_or_io=1" {
		t.Fatalf("formatted failure kinds = %q", got)
	}
}

func TestVerifyScaleReadbackUsesRealMetricAndLogQueries(t *testing.T) {
	const marker = "0123456789abcdef"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assertReadCredential(t, r)
		switch r.URL.Path {
		case "/api/v1/query":
			query := r.URL.Query().Get("query")
			if !strings.Contains(query, scaleMetricName) || !strings.Contains(query, marker) ||
				r.URL.Query().Get("nocache") != "1" {
				t.Errorf("metric query = %q", query)
			}
			_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[{"value":[1,"2"]}]}}`))
		case "/api/v1/logs/query":
			if !strings.Contains(r.URL.Query().Get("query"), marker) || r.URL.Query().Get("limit") != "2" {
				t.Errorf("log query = %q", r.URL.RawQuery)
			}
			_, _ = w.Write(
				[]byte(
					`{"_msg":"` + scaleLogMessagePrefix + marker + `","_time":"2026-07-18T00:00:00Z","sequence":"0"}` + "\n" +
						`{"_msg":"` + scaleLogMessagePrefix + marker + `","_time":"2026-07-18T00:00:00Z","sequence":"1"}`,
				),
			)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	if err := verifyScaleReadback(
		server.Client(),
		[]string{server.URL},
		testReadAuth,
		marker,
		2,
		time.Second,
	); err != nil {
		t.Fatal(err)
	}
}

func TestVerifyScalePayloadReadbackMirrorsThroughRealGateway(t *testing.T) {
	payload, err := newScalePayload(2)
	if err != nil {
		t.Fatal(err)
	}
	posts := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/metrics", "/v1/logs":
			posts++
			if r.Method != http.MethodPost || r.Header.Get("Authorization") != "Bearer "+testIngestToken ||
				r.Header.Get(projectHeader) != "" {
				t.Errorf("ingest %s used read credential or project selector: %#v", r.URL.Path, r.Header)
			}
			w.Header().Set("Content-Type", "application/x-protobuf")
		case "/api/v1/query":
			assertReadCredential(t, r)
			_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[{"value":[1,"2"]}]}}`))
		case "/api/v1/logs/query":
			assertReadCredential(t, r)
			_, _ = w.Write(
				[]byte(
					`{"_msg":"` + scaleLogMessagePrefix + payload.marker + `","_time":"2026-07-18T00:00:00Z","sequence":"0"}` + "\n" +
						`{"_msg":"` + scaleLogMessagePrefix + payload.marker + `","_time":"2026-07-18T00:00:00Z","sequence":"1"}`,
				),
			)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	if err := verifyScalePayloadReadback(
		server.Client(),
		[]string{server.URL, server.URL},
		testIngestToken,
		testReadAuth,
		payload,
		2,
		true,
		time.Second,
	); err != nil {
		t.Fatal(err)
	}
	if posts != 2 {
		t.Fatalf("mirrored posts=%d, want one metrics and one logs payload", posts)
	}
}

func TestScaleReadbackRejectsFalsePositiveBodies(t *testing.T) {
	message := scaleLogMessagePrefix + "marker"
	metricBodies := []string{
		`{"status":"success","data":{"resultType":"vector","result":[{"value":[1,"0"]}]}}`,
		`{"status":"success","data":{"resultType":"vector","result":[{"value":[1,"1"]}]}}`,
		`{"status":"success","data":{"resultType":"vector","result":[]},"echo":"marker"}`,
		`{"status":"success","data":{"resultType":"vector","result":[{"value":[1,"1"]}]}} trailing`,
	}
	for _, body := range metricBodies {
		if validScaleMetricReadback([]byte(body), 2) {
			t.Fatalf("metric false positive accepted: %s", body)
		}
	}
	logBodies := []string{
		`{"other":"` + message + `","_time":"2026-07-18T00:00:00Z","sequence":"0"}`,
		`{"_msg":"other","_time":"2026-07-18T00:00:00Z","sequence":"0","echo":"` + message + `"}`,
		`{"_msg":"` + message + `","_time":"2026-07-18T00:00:00Z","sequence":"0"}` + "\nnot-json",
		`{"_msg":"` + message + `","_time":"2026-07-18T00:00:00Z","sequence":"0"}` + "\n" + `{"_msg":"` + message + `","_time":"2026-07-18T00:00:00Z","sequence":"0"}`,
	}
	for _, body := range logBodies {
		if validScaleLogReadback([]byte(body), message, 2) {
			t.Fatalf("log false positive accepted: %s", body)
		}
	}
}

func TestMedianScalePhaseUsesAllRounds(t *testing.T) {
	rounds := []scalePhaseResult{
		{
			Replicas:                  2,
			Concurrency:               8,
			RecordsPerSecond:          300,
			SamplesPerSecond:          100,
			RequestsPerTarget:         []uint64{10, 30},
			RecordsPerSecondPerTarget: []float64{100, 300},
		},
		{
			Replicas:                  2,
			Concurrency:               8,
			RecordsPerSecond:          100,
			SamplesPerSecond:          50,
			RequestsPerTarget:         []uint64{5, 10},
			RecordsPerSecondPerTarget: []float64{50, 100},
		},
		{
			Replicas:                  2,
			Concurrency:               8,
			RecordsPerSecond:          200,
			SamplesPerSecond:          75,
			RequestsPerTarget:         []uint64{8, 20},
			RecordsPerSecondPerTarget: []float64{80, 200},
		},
	}
	got := medianScalePhase(rounds)
	if got.RecordsPerSecond != 200 || got.SamplesPerSecond != 75 || got.RequestsPerTarget[0] != 8 ||
		got.RequestsPerTarget[1] != 20 ||
		got.RecordsPerSecondPerTarget[0] != 80 {
		t.Fatalf("median phase = %#v", got)
	}
}

func TestRepresentativeBaselineAveragesEveryReplica(t *testing.T) {
	baselines := []scaleTargetBaseline{
		{
			Median: scalePhaseResult{
				Concurrency:       4,
				SamplesPerSecond:  100,
				LogsPerSecond:     200,
				RecordsPerSecond:  300,
				RequestsPerTarget: []uint64{30},
			},
		},
		{
			Median: scalePhaseResult{
				Concurrency:       4,
				SamplesPerSecond:  200,
				LogsPerSecond:     400,
				RecordsPerSecond:  600,
				RequestsPerTarget: []uint64{60},
			},
		},
	}
	got := representativeBaseline(baselines)
	if got.Concurrency != 4 || got.SamplesPerSecond != 150 || got.LogsPerSecond != 300 || got.RecordsPerSecond != 450 ||
		got.RequestsPerTarget[0] != 45 {
		t.Fatalf("representative baseline = %#v", got)
	}
}

func TestBaselineThroughputRatiosExposeAsymmetricReplicas(t *testing.T) {
	baselines := []scaleTargetBaseline{
		{Median: scalePhaseResult{SamplesPerSecond: 80, LogsPerSecond: 90}},
		{Median: scalePhaseResult{SamplesPerSecond: 100, LogsPerSecond: 120}},
	}
	samples, logs := baselineThroughputRatios(baselines)
	if samples != 0.8 || logs != 0.75 {
		t.Fatalf("baseline ratios = %.3f/%.3f", samples, logs)
	}
	baselines[0].Median.LogsPerSecond = 0
	if samples, logs = baselineThroughputRatios(baselines); samples != 0 || logs != 0 {
		t.Fatalf("zero-throughput ratios = %.3f/%.3f", samples, logs)
	}
}

func TestCapacityReferenceUsesFastestIndividualReplicaPerSignal(t *testing.T) {
	reference := scaleCapacityReference(
		[]scaleTargetBaseline{
			{Target: "target-1", Median: scalePhaseResult{SamplesPerSecond: 80, LogsPerSecond: 120}},
			{Target: "target-2", Median: scalePhaseResult{SamplesPerSecond: 100, LogsPerSecond: 90}},
		},
		[]scaleTargetBaseline{
			{Target: "target-1", Median: scalePhaseResult{SamplesPerSecond: 95, LogsPerSecond: 110}},
			{Target: "target-2", Median: scalePhaseResult{SamplesPerSecond: 90, LogsPerSecond: 130}},
		},
	)
	if reference.SamplesPerSecond != 100 || reference.SampleSource != "target-2_nominal" ||
		reference.LogsPerSecond != 130 || reference.LogSource != "target-2_high_control" || reference.RecordsPerSecond != 230 {
		t.Fatalf("capacity reference = %#v", reference)
	}
}

func TestEvaluateUpstreamHeadroomComputesMedianConservativeAndMajorityMetrics(t *testing.T) {
	evidence := evaluateUpstreamHeadroom(
		[]scalePhaseResult{
			{SamplesPerSecond: 310, LogsPerSecond: 400},
			{SamplesPerSecond: 100, LogsPerSecond: 200},
			{SamplesPerSecond: 280, LogsPerSecond: 360},
		},
		[]scalePhaseResult{
			{SamplesPerSecond: 50, LogsPerSecond: 80},
			{SamplesPerSecond: 80, LogsPerSecond: 100},
			{SamplesPerSecond: 60, LogsPerSecond: 90},
			{SamplesPerSecond: 70, LogsPerSecond: 100},
		},
		2,
	)
	if evidence.MedianControlSamplesPerSecond != 280 || evidence.MedianScaledSamplesPerSecond != 65 ||
		evidence.SampleRatio != 280.0/65.0 ||
		evidence.MedianControlLogsPerSecond != 360 || evidence.MedianScaledLogsPerSecond != 95 ||
		evidence.LogRatio != 360.0/95.0 ||
		evidence.MinimumControlSamplesPerSecond != 100 || evidence.MaximumScaledSamplesPerSecond != 80 ||
		evidence.ConservativeSampleRatio != 1.25 ||
		evidence.MinimumControlLogsPerSecond != 200 || evidence.MaximumScaledLogsPerSecond != 100 ||
		evidence.ConservativeLogRatio != 2 ||
		evidence.PassingControlRounds != 2 || evidence.RequiredPassingControlRounds != 2 ||
		evidence.MinimumRequiredRatio != 2 || evidence.MinimumConservativeRatio != 1.5 {
		t.Fatalf("upstream headroom evidence = %#v", evidence)
	}
}

func TestCapacityReferencePreventsClientConcurrencyFalsePositive(t *testing.T) {
	nominal := scalePhaseResult{Concurrency: 4, SamplesPerSecond: 100, LogsPerSecond: 100}
	highControl := scalePhaseResult{Concurrency: 8, SamplesPerSecond: 150, LogsPerSecond: 150}
	reference := capacityReferenceForTest(nominal, highControl)
	scaled := scalePhaseResult{
		Concurrency: 8, SamplesPerSecond: 150, LogsPerSecond: 150,
		RequestsPerTarget: []uint64{10, 10},
	}
	result := evaluateCapacityScaling([]string{"target-1", "target-2"}, nominal, highControl, reference, scaled, 4, 0.7)
	result.MinPassingRoundRatio = 0.75
	applyScaleRoundEfficiencyGate(&result, []scalePhaseResult{scaled})
	if result.GatePassed || result.SampleReplicaEfficiency != 0.5 || result.LogReplicaEfficiency != 0.5 {
		t.Fatalf("client concurrency false-positive result = %#v", result)
	}
}

func TestScaleRoundEfficiencyGateRejectsAggregateMedianFalsePositive(t *testing.T) {
	reference := scaleThroughputReference{SamplesPerSecond: 100, LogsPerSecond: 100}
	rounds := []scalePhaseResult{
		{SamplesPerSecond: 144, LogsPerSecond: 145},
		{SamplesPerSecond: 134, LogsPerSecond: 135},
		{SamplesPerSecond: 140, LogsPerSecond: 141},
		{SamplesPerSecond: 132, LogsPerSecond: 131},
	}
	aggregate := medianScalePhase(rounds)
	if sampleEfficiency, logEfficiency := aggregate.SamplesPerSecond/200, aggregate.LogsPerSecond/200; sampleEfficiency < 0.68 ||
		logEfficiency < 0.68 {
		t.Fatalf("test aggregate efficiency = %.3f/%.3f, want both >= 0.680", sampleEfficiency, logEfficiency)
	}

	result := scaleGateResult{
		Targets:                 []string{"target-1", "target-2"},
		SingleCapacityReference: reference,
		MinReplicaEfficiency:    0.68,
		MinPassingRoundRatio:    0.75,
		SampleReplicaEfficiency: aggregate.SamplesPerSecond / 200,
		LogReplicaEfficiency:    aggregate.LogsPerSecond / 200,
		GatePassed:              true,
	}
	applyScaleRoundEfficiencyGate(&result, rounds)
	if result.GatePassed || result.PassingScaledRounds != 2 || result.RequiredPassingScaledRounds != 3 ||
		len(result.ScaledRoundEfficiencies) != 4 {
		t.Fatalf("round efficiency result = %#v", result)
	}
	if len(result.GateFailures) != 1 || !strings.Contains(result.GateFailures[0], "only 2 of 4") ||
		!strings.Contains(result.GateFailures[0], "2(metric=0.670,log=0.675)") ||
		!strings.Contains(result.GateFailures[0], "4(metric=0.660,log=0.655)") {
		t.Fatalf("round efficiency failures = %v", result.GateFailures)
	}
}

func TestScaleRoundEfficiencyGateRequiresMetricAndLogInSameRound(t *testing.T) {
	rounds := []scalePhaseResult{
		{SamplesPerSecond: 160, LogsPerSecond: 120},
		{SamplesPerSecond: 120, LogsPerSecond: 160},
		{SamplesPerSecond: 160, LogsPerSecond: 160},
		{SamplesPerSecond: 160, LogsPerSecond: 160},
	}
	evaluations, passing, required := evaluateScaleRoundEfficiencies(
		rounds,
		scaleThroughputReference{SamplesPerSecond: 100, LogsPerSecond: 100},
		2,
		0.68,
		0.75,
	)
	if passing != 2 || required != 3 || evaluations[0].PairedGatePassed || evaluations[1].PairedGatePassed ||
		!evaluations[2].PairedGatePassed || !evaluations[3].PairedGatePassed {
		t.Fatalf("paired round evaluations = %#v, passing=%d required=%d", evaluations, passing, required)
	}
}

func TestScaleRoundEfficiencyGatePassesCurrentEvidenceAndRoundsRequirementUp(t *testing.T) {
	rounds := []scalePhaseResult{
		{SamplesPerSecond: 144.64, LogsPerSecond: 144.78},
		{SamplesPerSecond: 139.46, LogsPerSecond: 139.50},
		{SamplesPerSecond: 141.64, LogsPerSecond: 141.86},
		{SamplesPerSecond: 130.36, LogsPerSecond: 130.62},
	}
	result := scaleGateResult{
		Targets:                 []string{"target-1", "target-2"},
		SingleCapacityReference: scaleThroughputReference{SamplesPerSecond: 100, LogsPerSecond: 100},
		MinReplicaEfficiency:    0.68,
		MinPassingRoundRatio:    0.75,
		GatePassed:              true,
	}
	applyScaleRoundEfficiencyGate(&result, rounds)
	if !result.GatePassed || result.PassingScaledRounds != 3 || result.RequiredPassingScaledRounds != 3 ||
		len(result.GateFailures) != 0 {
		t.Fatalf("current-evidence result = %#v", result)
	}

	_, _, required := evaluateScaleRoundEfficiencies(
		append(rounds, rounds[0]),
		result.SingleCapacityReference,
		len(result.Targets),
		result.MinReplicaEfficiency,
		result.MinPassingRoundRatio,
	)
	if required != 4 {
		t.Fatalf("required passing rounds = %d, want ceil(5*0.75)=4", required)
	}
}

func TestEvaluateCapacityScalingRequiresExactLoadShape(t *testing.T) {
	nominal := scalePhaseResult{Concurrency: 8, SamplesPerSecond: 100, LogsPerSecond: 100}
	highControl := scalePhaseResult{Concurrency: 4, SamplesPerSecond: 100, LogsPerSecond: 100}
	reference := capacityReferenceForTest(nominal, highControl)
	scaled := scalePhaseResult{
		Concurrency:       4,
		SamplesPerSecond:  200,
		LogsPerSecond:     200,
		RequestsPerTarget: []uint64{10, 10},
	}
	result := evaluateCapacityScaling([]string{"target-1", "target-2"}, nominal, highControl, reference, scaled, 4, 0.7)
	if result.GatePassed || len(result.GateFailures) != 3 {
		t.Fatalf("load-shape mismatch result = %#v", result)
	}
}

func capacityReferenceForTest(nominal, highControl scalePhaseResult) scaleThroughputReference {
	return scaleCapacityReference(
		[]scaleTargetBaseline{
			{Target: "target-1", Median: nominal},
			{Target: "target-2", Median: nominal},
		},
		[]scaleTargetBaseline{
			{Target: "target-1", Median: highControl},
			{Target: "target-2", Median: highControl},
		},
	)
}

func writeScaleSinkSuccess(t *testing.T, w http.ResponseWriter, r *http.Request, identity string) {
	t.Helper()
	body, err := io.ReadAll(r.Body)
	if err != nil {
		t.Errorf("read sink request: %v", err)
		http.Error(w, "read", http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "application/x-protobuf")
	w.Header().Set(scaleSinkIdentityHeader, identity)
	w.Header().Set(scaleSinkReceiptHeader, fmt.Sprintf("%x", sha256.Sum256(body)))
	w.Header().Set(scaleSinkRequestHeader, r.Header.Get(scaleSinkRequestHeader))
	w.WriteHeader(http.StatusOK)
}

func TestScaleTargetBalanceFailuresChecksRequestAndThroughputFloors(t *testing.T) {
	balanced := scalePhaseResult{
		Replicas:                  2,
		RequestsPerTarget:         []uint64{100, 100},
		MetricRequestsPerTarget:   []uint64{50, 50},
		LogRequestsPerTarget:      []uint64{50, 50},
		RecordsPerSecondPerTarget: []float64{1000, 1000},
	}
	if failures := scaleTargetBalanceFailures(balanced, 0.7); len(failures) != 0 {
		t.Fatalf("balanced failures = %v", failures)
	}
	unbalanced := scalePhaseResult{
		Replicas:                  2,
		RequestsPerTarget:         []uint64{190, 10},
		MetricRequestsPerTarget:   []uint64{95, 5},
		LogRequestsPerTarget:      []uint64{95, 5},
		RecordsPerSecondPerTarget: []float64{1900, 100},
	}
	failures := scaleTargetBalanceFailures(unbalanced, 0.7)
	if len(failures) != 4 {
		t.Fatalf("unbalanced failures = %v", failures)
	}
	for _, failure := range failures {
		if !strings.Contains(failure, "target 2") || !strings.Contains(failure, "lower bound") {
			t.Fatalf("unexpected balance failure = %q", failure)
		}
	}
}

func TestRunScaleGateRequiresSeparateReadCredential(t *testing.T) {
	baseArgs := []string{
		"--config", filepath.Join("..", "..", "bench", "performance-gates.json"),
		"--targets", "http://127.0.0.1:1,http://127.0.0.1:2",
		"--readback-targets", "http://127.0.0.1:3,http://127.0.0.1:4",
		"--controlled-upstream", "http://127.0.0.1:5",
	}
	t.Setenv("LO_PERF_TOKEN", testIngestToken)
	t.Setenv("LO_PERF_QUERY_TOKEN", "")
	err := runScaleGate(baseArgs)
	if err == nil || !strings.Contains(err.Error(), "LO_PERF_QUERY_TOKEN is empty") {
		t.Fatalf("missing query token error = %v", err)
	}
	t.Setenv("LO_PERF_QUERY_TOKEN", testIngestToken)
	err = runScaleGate(baseArgs)
	if err == nil || !strings.Contains(err.Error(), "user API token") {
		t.Fatalf("project token as read credential error = %v", err)
	}
	t.Setenv("LO_PERF_TOKEN", testReadAuth.token)
	t.Setenv("LO_PERF_QUERY_TOKEN", testReadAuth.token)
	err = runScaleGate(baseArgs)
	if err == nil || !strings.Contains(err.Error(), "project ingest token") {
		t.Fatalf("user token as ingest credential error = %v", err)
	}
	if strings.Contains(err.Error(), testReadAuth.token) {
		t.Fatalf("credential error leaked token: %v", err)
	}
}

func TestUpstreamHeadroomInterleavedControlSchedule(t *testing.T) {
	wantFour := []string{"", "after-round-2", "", ""}
	for round, want := range wantFour {
		got, ok := upstreamControlPositionAfterRound(round, 4)
		if ok != (want != "") || got != want {
			t.Fatalf("4-round schedule round %d = (%q, %v), want (%q, %v)", round, got, ok, want, want != "")
		}
	}
	wantEight := []string{"", "after-round-2", "", "after-round-4", "", "after-round-6", "", ""}
	for round, want := range wantEight {
		got, ok := upstreamControlPositionAfterRound(round, 8)
		if ok != (want != "") || got != want {
			t.Fatalf("8-round schedule round %d = (%q, %v), want (%q, %v)", round, got, ok, want, want != "")
		}
	}
}

func TestUpstreamHeadroomToleratesSingleRoundJitter(t *testing.T) {
	tests := []struct {
		name          string
		controlRounds []scalePhaseResult
		scaledRounds  []scalePhaseResult
	}{
		{
			name: "single control dip",
			controlRounds: []scalePhaseResult{
				{SamplesPerSecond: 290000, LogsPerSecond: 290000},
				{SamplesPerSecond: 220000, LogsPerSecond: 220000},
				{SamplesPerSecond: 295000, LogsPerSecond: 295000},
			},
			scaledRounds: []scalePhaseResult{
				{SamplesPerSecond: 125000, LogsPerSecond: 125000},
				{SamplesPerSecond: 128000, LogsPerSecond: 128000},
				{SamplesPerSecond: 127000, LogsPerSecond: 127000},
				{SamplesPerSecond: 135000, LogsPerSecond: 135000},
			},
		},
		{
			name: "issue 302 combined dip and fast scaled round",
			controlRounds: []scalePhaseResult{
				{SamplesPerSecond: 258000, LogsPerSecond: 258000},
				{SamplesPerSecond: 285000, LogsPerSecond: 285000},
				{SamplesPerSecond: 288000, LogsPerSecond: 288000},
			},
			scaledRounds: []scalePhaseResult{
				{SamplesPerSecond: 126000, LogsPerSecond: 126000},
				{SamplesPerSecond: 129000, LogsPerSecond: 129000},
				{SamplesPerSecond: 128000, LogsPerSecond: 128000},
				{SamplesPerSecond: 135000, LogsPerSecond: 135000},
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			evidence := evaluateUpstreamHeadroom(tc.controlRounds, tc.scaledRounds, 2.0)
			if evidence.ConservativeSampleRatio >= 2.0 || evidence.ConservativeLogRatio >= 2.0 {
				t.Fatalf("expected legacy conservative min/max ratio < 2.0 to exercise jitter, got %#v", evidence)
			}
			if evidence.SampleRatio < 2.0 || evidence.LogRatio < 2.0 ||
				evidence.PassingControlRounds < evidence.RequiredPassingControlRounds {
				t.Fatalf("expected median and majority metrics to pass, got %#v", evidence)
			}
			result := scaleGateResult{
				UpstreamControlRounds: tc.controlRounds,
				ScaledRounds:          tc.scaledRounds,
				UpstreamHeadroom:      evidence,
				GatePassed:            true,
			}
			applyUpstreamHeadroomGate(&result)
			if !result.GatePassed || len(result.GateFailures) != 0 {
				t.Fatalf("expected gate pass under single-round jitter, got failures=%v", result.GateFailures)
			}
		})
	}
}

func TestUpstreamHeadroomFailsClosedOnBottleneckAndDegradation(t *testing.T) {
	tests := []struct {
		name          string
		controlRounds []scalePhaseResult
		scaledRounds  []scalePhaseResult
		wantSubstr    string
	}{
		{
			name: "real upstream bottleneck",
			controlRounds: []scalePhaseResult{
				{SamplesPerSecond: 230000, LogsPerSecond: 230000},
				{SamplesPerSecond: 235000, LogsPerSecond: 235000},
				{SamplesPerSecond: 228000, LogsPerSecond: 228000},
			},
			scaledRounds: []scalePhaseResult{
				{SamplesPerSecond: 128000, LogsPerSecond: 128000},
				{SamplesPerSecond: 130000, LogsPerSecond: 130000},
				{SamplesPerSecond: 129000, LogsPerSecond: 129000},
				{SamplesPerSecond: 131000, LogsPerSecond: 131000},
			},
			wantSubstr: "median ratio",
		},
		{
			name: "sustained degradation in majority of rounds",
			controlRounds: []scalePhaseResult{
				{SamplesPerSecond: 290000, LogsPerSecond: 290000},
				{SamplesPerSecond: 225000, LogsPerSecond: 225000},
				{SamplesPerSecond: 228000, LogsPerSecond: 228000},
			},
			scaledRounds: []scalePhaseResult{
				{SamplesPerSecond: 125000, LogsPerSecond: 125000},
				{SamplesPerSecond: 126000, LogsPerSecond: 126000},
				{SamplesPerSecond: 127000, LogsPerSecond: 127000},
				{SamplesPerSecond: 128000, LogsPerSecond: 128000},
			},
			wantSubstr: "only 1 of 3 upstream control rounds",
		},
		{
			name: "half of even rounds anomalous fails strict majority",
			controlRounds: []scalePhaseResult{
				{SamplesPerSecond: 340000, LogsPerSecond: 340000},
				{SamplesPerSecond: 340000, LogsPerSecond: 340000},
				{SamplesPerSecond: 200000, LogsPerSecond: 200000},
				{SamplesPerSecond: 200000, LogsPerSecond: 200000},
			},
			scaledRounds: []scalePhaseResult{
				{SamplesPerSecond: 120000, LogsPerSecond: 120000},
				{SamplesPerSecond: 120000, LogsPerSecond: 120000},
				{SamplesPerSecond: 120000, LogsPerSecond: 120000},
				{SamplesPerSecond: 120000, LogsPerSecond: 120000},
			},
			wantSubstr: "only 2 of 4 upstream control rounds",
		},
		{
			name: "misaligned metric and log headroom across rounds",
			controlRounds: []scalePhaseResult{
				{SamplesPerSecond: 260000, LogsPerSecond: 190000},
				{SamplesPerSecond: 190000, LogsPerSecond: 260000},
				{SamplesPerSecond: 260000, LogsPerSecond: 260000},
			},
			scaledRounds: []scalePhaseResult{
				{SamplesPerSecond: 100000, LogsPerSecond: 100000},
				{SamplesPerSecond: 100000, LogsPerSecond: 100000},
				{SamplesPerSecond: 100000, LogsPerSecond: 100000},
				{SamplesPerSecond: 100000, LogsPerSecond: 100000},
			},
			wantSubstr: "only 1 of 3 upstream control rounds",
		},
		{
			name: "severe single round collapse below conservative floor",
			controlRounds: []scalePhaseResult{
				{SamplesPerSecond: 300000, LogsPerSecond: 300000},
				{SamplesPerSecond: 140000, LogsPerSecond: 140000},
				{SamplesPerSecond: 305000, LogsPerSecond: 305000},
			},
			scaledRounds: []scalePhaseResult{
				{SamplesPerSecond: 125000, LogsPerSecond: 125000},
				{SamplesPerSecond: 128000, LogsPerSecond: 128000},
				{SamplesPerSecond: 127000, LogsPerSecond: 127000},
				{SamplesPerSecond: 130000, LogsPerSecond: 130000},
			},
			wantSubstr: "conservative min/max ratio",
		},
		{
			name:          "empty control rounds",
			controlRounds: nil,
			scaledRounds: []scalePhaseResult{
				{SamplesPerSecond: 100000, LogsPerSecond: 100000},
			},
			wantSubstr: "median ratio",
		},
		{
			name: "zero scaled throughput",
			controlRounds: []scalePhaseResult{
				{SamplesPerSecond: 200000, LogsPerSecond: 200000},
			},
			scaledRounds: []scalePhaseResult{
				{SamplesPerSecond: 0, LogsPerSecond: 0},
			},
			wantSubstr: "median ratio",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			evidence := evaluateUpstreamHeadroom(tc.controlRounds, tc.scaledRounds, 2.0)
			result := scaleGateResult{
				UpstreamControlRounds: tc.controlRounds,
				ScaledRounds:          tc.scaledRounds,
				UpstreamHeadroom:      evidence,
				GatePassed:            true,
			}
			applyUpstreamHeadroomGate(&result)
			if result.GatePassed || len(result.GateFailures) == 0 {
				t.Fatalf("expected fail-closed failure, got result=%#v", result)
			}
			joined := strings.Join(result.GateFailures, "; ")
			if !strings.Contains(joined, tc.wantSubstr) {
				t.Fatalf("failures = %q, want substring %q", joined, tc.wantSubstr)
			}
		})
	}
}

func TestFormatScaleFailureDiagnosticsIncludesPerRoundDetails(t *testing.T) {
	controlRounds := []scalePhaseResult{
		{SamplesPerSecond: 250000, LogsPerSecond: 255000},
		{SamplesPerSecond: 180000, LogsPerSecond: 185000},
		{SamplesPerSecond: 245000, LogsPerSecond: 248000},
	}
	scaledRounds := []scalePhaseResult{
		{SamplesPerSecond: 130000, LogsPerSecond: 131000},
		{SamplesPerSecond: 132000, LogsPerSecond: 133000},
		{SamplesPerSecond: 129000, LogsPerSecond: 130000},
		{SamplesPerSecond: 135000, LogsPerSecond: 136000},
	}
	result := scaleGateResult{
		Targets: []string{"target-1", "target-2"},
		SingleCapacityReference: scaleThroughputReference{
			SamplesPerSecond: 80000,
			SampleSource:     "target-1_nominal",
			LogsPerSecond:    80000,
			LogSource:        "target-2_high_control",
		},
		Scaled: scalePhaseResult{
			SamplesPerSecond: 131000,
			LogsPerSecond:    132000,
		},
		SampleReplicaEfficiency:     0.819,
		LogReplicaEfficiency:        0.825,
		MinReplicaEfficiency:        0.68,
		PassingScaledRounds:         4,
		RequiredPassingScaledRounds: 3,
		ScaledRounds:                scaledRounds,
		ScaledRoundEfficiencies: []scaleRoundEfficiency{
			{Round: 1, SampleReplicaEfficiency: 0.812, LogReplicaEfficiency: 0.819, PairedGatePassed: true},
			{Round: 2, SampleReplicaEfficiency: 0.825, LogReplicaEfficiency: 0.831, PairedGatePassed: true},
			{Round: 3, SampleReplicaEfficiency: 0.806, LogReplicaEfficiency: 0.813, PairedGatePassed: true},
			{Round: 4, SampleReplicaEfficiency: 0.844, LogReplicaEfficiency: 0.850, PairedGatePassed: true},
		},
		UpstreamControlRounds: controlRounds,
		UpstreamControlLabels: []string{"leading", "after-round-2", "trailing"},
		UpstreamHeadroom:      evaluateUpstreamHeadroom(controlRounds, scaledRounds, 2.0),
	}
	applyUpstreamHeadroomGate(&result)
	if result.GatePassed {
		t.Fatal("expected gate failure in diagnostic test")
	}
	diag := formatScaleFailureDiagnostics(result)
	for _, want := range []string{
		"median ratio",
		"upstream_headroom:",
		"median_ratio=samples:",
		"conservative_min_max_ratio=samples:",
		"passing_control_rounds=0/3 (required>=2)",
		"upstream_control_rounds:",
		"1(leading: samples=250000.0/s, logs=255000.0/s",
		"2(after-round-2: samples=180000.0/s, logs=185000.0/s",
		"3(trailing: samples=245000.0/s, logs=248000.0/s",
		"scaled_rounds:",
		"1(samples=130000.0/s, logs=131000.0/s, eff=0.812/0.819)",
		"4(samples=135000.0/s, logs=136000.0/s, eff=0.844/0.850)",
		"scaling_summary:",
	} {
		if !strings.Contains(diag, want) {
			t.Fatalf("diagnostics missing %q:\n%s", want, diag)
		}
	}
}

func TestRunScaleGateEmitsThreeControlRoundsAndDiagnosticsOnBottleneck(t *testing.T) {
	const sinkIdentity = "losink_0123456789abcdef"
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "gates.json")
	outputPath := filepath.Join(tmpDir, "scale.json")
	configJSON := `{
  "version": 1,
  "release": { "max_binary_bytes": 150000000, "max_idle_rss_bytes": 524288000 },
  "query": {
    "warmup_requests": 0,
    "measured_requests": 1,
    "medium_fixture": { "issues": 1000, "logs": 1000, "traces": 1000 },
    "max_p95_ms": { "issues": 1000, "logs": 1000, "trace": 1000 },
    "max_p99_ms": { "issues": 1500, "logs": 1500, "trace": 1500 }
  },
  "ha_scaling": {
    "settle_seconds": 0,
    "warmup_seconds": 0.01,
    "recovery_seconds": 0,
    "duration_seconds": 0.06,
    "scaled_concurrency_per_replica": 2,
    "gomaxprocs_per_replica": 1,
    "batch_size": 2,
    "min_replica_efficiency": 0.68,
    "min_passing_round_ratio": 0.75,
    "min_target_balance_ratio": 0.7,
    "min_baseline_ratio": 0.8,
    "min_upstream_headroom_ratio": 2.0,
    "max_failed_requests": 0
  }
}`
	if err := os.WriteFile(configPath, []byte(configJSON), 0o600); err != nil {
		t.Fatal(err)
	}

	controlledSink := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/health" {
			w.Header().Set(scaleSinkIdentityHeader, sinkIdentity)
			_, _ = w.Write([]byte("OK\n"))
			return
		}
		time.Sleep(10 * time.Millisecond)
		writeScaleSinkSuccess(t, w, r, sinkIdentity)
	}))
	defer controlledSink.Close()

	newMeasureTarget := func(instanceID string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == scaleInstanceIdentityPath {
				assertReadCredential(t, r)
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"instance_id":"` + instanceID + `","gomaxprocs":1}`))
				return
			}
			writeScaleSinkSuccess(t, w, r, sinkIdentity)
		}))
	}
	measureA := newMeasureTarget("measure-a")
	defer measureA.Close()
	measureB := newMeasureTarget("measure-b")
	defer measureB.Close()

	newReadbackTarget := func(instanceID string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case scaleInstanceIdentityPath:
				assertReadCredential(t, r)
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"instance_id":"` + instanceID + `","gomaxprocs":1}`))
			case "/v1/metrics", "/v1/logs":
				if r.Header.Get("Authorization") != "Bearer "+testIngestToken {
					t.Errorf("readback ingest auth = %q", r.Header.Get("Authorization"))
				}
				w.Header().Set("Content-Type", "application/x-protobuf")
				w.WriteHeader(http.StatusOK)
			case "/api/v1/query":
				assertReadCredential(t, r)
				_, _ = w.Write(
					[]byte(`{"status":"success","data":{"resultType":"vector","result":[{"value":[1,"2"]}]}}`),
				)
			case "/api/v1/logs/query":
				assertReadCredential(t, r)
				q := r.URL.Query().Get("query")
				msg := strings.TrimSuffix(strings.TrimPrefix(q, `_msg:="`), `"`)
				_, _ = w.Write(
					[]byte(
						`{"_msg":"` + msg + `","_time":"2026-07-18T00:00:00Z","sequence":"0"}` + "\n" +
							`{"_msg":"` + msg + `","_time":"2026-07-18T00:00:00Z","sequence":"1"}` + "\n",
					),
				)
			default:
				http.NotFound(w, r)
			}
		}))
	}
	readbackA := newReadbackTarget("readback-a")
	defer readbackA.Close()
	readbackB := newReadbackTarget("readback-b")
	defer readbackB.Close()

	t.Setenv("LO_PERF_TOKEN", testIngestToken)
	t.Setenv("LO_PERF_QUERY_TOKEN", testReadAuth.token)

	err := runScaleGate([]string{
		"--config", configPath,
		"--project", testReadAuth.project,
		"--targets", measureA.URL + "," + measureB.URL,
		"--readback-targets", readbackA.URL + "," + readbackB.URL,
		"--controlled-upstream", controlledSink.URL,
		"--output", outputPath,
	})
	if err == nil {
		t.Fatal("expected runScaleGate to fail closed when controlled upstream is bottlenecked")
	}
	errMsg := err.Error()
	for _, want := range []string{
		"gate failed:",
		"upstream_headroom:",
		"upstream_control_rounds:",
		"1(leading:",
		"2(after-round-2:",
		"3(trailing:",
		"scaled_rounds:",
	} {
		if !strings.Contains(errMsg, want) {
			t.Fatalf("error missing %q:\n%s", want, errMsg)
		}
	}

	raw, readErr := os.ReadFile(outputPath)
	if readErr != nil {
		t.Fatal(readErr)
	}
	var saved scaleGateResult
	if unmarshalErr := json.Unmarshal(raw, &saved); unmarshalErr != nil {
		t.Fatal(unmarshalErr)
	}
	if saved.Version != 7 || saved.GatePassed || saved.GeneratedAt == "" {
		t.Fatalf("saved scale.json metadata/gate = %#v", saved)
	}
	if len(saved.UpstreamControlRounds) != 3 {
		t.Fatalf("upstream_control_rounds len = %d, want 3", len(saved.UpstreamControlRounds))
	}
	if !slices.Equal(saved.UpstreamControlLabels, []string{"leading", "after-round-2", "trailing"}) {
		t.Fatalf("upstream_control_labels = %v", saved.UpstreamControlLabels)
	}
	if saved.UpstreamHeadroom.SampleRatio >= 2.0 || saved.UpstreamHeadroom.LogRatio >= 2.0 {
		t.Fatalf("expected headroom ratios < 2.0 under throttled sink, got %#v", saved.UpstreamHeadroom)
	}
}
