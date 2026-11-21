package main

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	metricspb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	"google.golang.org/protobuf/proto"
)

func TestRepositoryGateConfig(t *testing.T) {
	cfg, err := loadGateConfig(filepath.Join("..", "..", "bench", "performance-gates.json"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Release.MaxBinaryBytes != 150_000_000 || cfg.Release.MaxIdleRSSBytes != 500*1024*1024 ||
		cfg.Query.MaxP95MS["trace"] != 1000 ||
		cfg.Query.MaxP99MS["issues"] != 1500 ||
		cfg.Query.MaxP99MS["logs"] != 1500 ||
		cfg.Query.MaxP99MS["trace"] != 1500 ||
		cfg.Query.MediumFixture.Issues != 10_000 ||
		cfg.Query.MediumFixture.Logs != 100_000 ||
		cfg.Query.MediumFixture.Traces != 10_000 {
		t.Fatalf("unexpected release/query budgets: %#v", cfg)
	}
	invalidP99 := cfg
	invalidP99.Query.MaxP99MS = map[string]float64{"issues": 900, "logs": 1500, "trace": 1500}
	if err := invalidP99.validate(); err == nil || !strings.Contains(err.Error(), "max_p99_ms.issues") {
		t.Fatalf("max_p99_ms below max_p95_ms validation error = %v", err)
	}
	if cfg.HAScaling.MinReplicaEfficiency != 0.68 || cfg.HAScaling.MinPassingRoundRatio != 0.75 ||
		cfg.HAScaling.MinTargetBalanceRatio != 0.7 ||
		cfg.HAScaling.MinBaselineRatio != 0.8 ||
		cfg.HAScaling.GOMAXPROCSPerReplica != 1 ||
		cfg.HAScaling.BatchSize != 20 ||
		cfg.HAScaling.ScaledConcurrencyPerReplica != 64 ||
		cfg.HAScaling.MinUpstreamHeadroomRatio != 2 ||
		cfg.HAScaling.SettleSeconds != 10 ||
		cfg.HAScaling.RecoverySeconds != 2 {
		t.Fatalf("unexpected HA resource/symmetry budgets: %#v", cfg.HAScaling)
	}
	for _, ratio := range []float64{-0.1, 0, 1.1} {
		invalid := cfg
		invalid.HAScaling.MinPassingRoundRatio = ratio
		if err := invalid.validate(); err == nil || !strings.Contains(err.Error(), "min_passing_round_ratio") {
			t.Fatalf("min_passing_round_ratio=%v validation error = %v", ratio, err)
		}
	}
	for _, ratio := range []float64{-0.1, 0, 1.1} {
		invalid := cfg
		invalid.HAScaling.MinTargetBalanceRatio = ratio
		if err := invalid.validate(); err == nil || !strings.Contains(err.Error(), "min_target_balance_ratio") {
			t.Fatalf("min_target_balance_ratio=%v validation error = %v", ratio, err)
		}
	}
}

func TestSummarizeLatenciesUsesNearestRank(t *testing.T) {
	values := make([]time.Duration, 100)
	for index := range values {
		values[index] = time.Duration(index+1) * time.Millisecond
	}
	got := summarizeLatencies(values)
	if got.P50MS != 50 || got.P95MS != 95 || got.P99MS != 99 {
		t.Fatalf("summary = %#v", got)
	}
}

func TestVerifyEmbeddedStorage(t *testing.T) {
	assetDir := t.TempDir()
	var manifest, binary bytes.Buffer
	payloads := make([][]byte, 0, len(releasePayloadNames))
	for index, name := range releasePayloadNames {
		raw := make([]byte, 4096)
		for offset := range raw {
			raw[offset] = byte((offset*31 + index*17) % 251)
		}
		sum := sha256.Sum256(raw)
		fmt.Fprintf(&manifest, "%s\t%d\t%x\n", name, len(raw), sum)
		var compressed bytes.Buffer
		writer := gzip.NewWriter(&compressed)
		if _, err := writer.Write(raw); err != nil {
			t.Fatal(err)
		}
		if err := writer.Close(); err != nil {
			t.Fatal(err)
		}
		payload := compressed.Bytes()
		if len(payload) < 128 {
			t.Fatalf("test payload %s compressed to only %d bytes", name, len(payload))
		}
		if err := os.WriteFile(filepath.Join(assetDir, name+".gz"), payload, 0o600); err != nil {
			t.Fatal(err)
		}
		payloads = append(payloads, append([]byte(nil), payload...))
		binary.Write(payload)
	}
	if err := os.WriteFile(filepath.Join(assetDir, "manifest.tsv"), manifest.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	binary.Write(manifest.Bytes())
	binaryPath := filepath.Join(t.TempDir(), "lazy-observe")
	if err := os.WriteFile(binaryPath, binary.Bytes(), 0o700); err != nil {
		t.Fatal(err)
	}
	count, err := verifyEmbeddedStorage(binaryPath, assetDir)
	if err != nil || count != len(releasePayloadNames) {
		t.Fatalf("verifyEmbeddedStorage() = %d, %v", count, err)
	}
	data, err := os.ReadFile(binaryPath)
	if err != nil {
		t.Fatal(err)
	}
	data = bytes.Replace(data, payloads[2], bytes.Repeat([]byte{99}, len(payloads[2])), 1)
	if err := os.WriteFile(binaryPath, data, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := verifyEmbeddedStorage(
		binaryPath,
		assetDir,
	); err == nil ||
		!strings.Contains(err.Error(), releasePayloadNames[2]) {
		t.Fatalf("missing payload was not rejected: %v", err)
	}
}

func TestEvaluateCapacityScaling(t *testing.T) {
	nominal := scalePhaseResult{Concurrency: 4, SamplesPerSecond: 100, LogsPerSecond: 200}
	highControl := scalePhaseResult{Concurrency: 8, SamplesPerSecond: 105, LogsPerSecond: 210}
	reference := capacityReferenceForTest(nominal, highControl)
	scaled := scalePhaseResult{
		Concurrency:       8,
		SamplesPerSecond:  150,
		LogsPerSecond:     300,
		RequestsPerTarget: []uint64{10, 10},
	}
	passing := evaluateCapacityScaling([]string{"a", "b"}, nominal, highControl, reference, scaled, 4, 0.7)
	if !passing.GatePassed || passing.SampleReplicaEfficiency < 0.714 || passing.LogReplicaEfficiency < 0.714 {
		t.Fatalf("passing result = %#v", passing)
	}
	scaled.RequestsPerTarget[1] = 0
	failing := evaluateCapacityScaling([]string{"a", "b"}, nominal, highControl, reference, scaled, 4, 0.8)
	if failing.GatePassed || len(failing.GateFailures) != 1 {
		t.Fatalf("failing result = %#v", failing)
	}
}

func TestMetricsLoadPayloadBatchSize(t *testing.T) {
	data, err := metricsLoadPayload(3)
	if err != nil {
		t.Fatal(err)
	}
	var request metricspb.ExportMetricsServiceRequest
	if err := proto.Unmarshal(data, &request); err != nil {
		t.Fatal(err)
	}
	points := request.ResourceMetrics[0].ScopeMetrics[0].Metrics[0].GetGauge().DataPoints
	if len(points) != 3 {
		t.Fatalf("data points = %d", len(points))
	}
}

func TestParseTargetsRejectsNonScalingInputs(t *testing.T) {
	if _, err := parseTargets("http://127.0.0.1:3000"); err == nil {
		t.Fatal("single target must be rejected")
	}
	if _, err := parseTargets("http://a:3000,http://a:3000"); err == nil {
		t.Fatal("duplicate targets must be rejected")
	}
	got, err := parseTargets("http://a:3000/, https://b:3001")
	if err != nil || len(got) != 2 || got[0] != "http://a:3000" {
		t.Fatalf("parseTargets() = %v, %v", got, err)
	}
}
